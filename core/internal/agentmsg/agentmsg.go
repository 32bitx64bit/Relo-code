// Package agentmsg implements a minimal, loopback-only, authenticated
// streamable-HTTP MCP server exposing a single "agents" tool to Relo agents.
//
// Security model:
//   - Listens on 127.0.0.1 only, on an ephemeral port.
//   - Every request must carry the server-wide Secret as a Bearer token
//     (constant-time comparison). Per-agent capability keys travel in the
//     tool arguments and are validated by the handler, never by this layer.
//   - POST only, JSON only, no CORS headers, and requests carrying an Origin
//     header (browser requests) are rejected.
//   - Request bodies are capped at 32 KiB and all HTTP timeouts are bounded.
//   - Nothing is logged or persisted.
package agentmsg

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	endpointPath           = "/mcp"
	toolName               = "agents"
	maxBodyBytes           = 32 << 10
	maxHeaderBytes         = 8 << 10
	handlerTimeout         = 15 * time.Second
	shutdownTimeout        = 5 * time.Second
	defaultProtocolVersion = "2024-11-05"
	serverName             = "relo-agentmsg"
	serverVersion          = "1.0.0"
)

// supportedProtocolVersions lists MCP revisions this server can speak. The
// tool surface is identical across them, so the requested version is mirrored
// when supported and otherwise the default is returned.
var supportedProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18"}

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

var nullID = json.RawMessage("null")

// Request is the decoded "agents" tool invocation passed to the Handler.
type Request struct {
	Key    string `json:"key"`
	Action string `json:"action"`
	To     string `json:"to,omitempty"`
	Text   string `json:"text,omitempty"`
}

// Handler services a validated tool call. Its return value is serialized as
// JSON into the tool result text. A non-nil error becomes an MCP tool error
// (isError true), not a JSON-RPC protocol error.
type Handler func(context.Context, Request) (any, error)

// CanvasRequest is the decoded "relo_canvas" tool invocation. Source payloads are
// passed through unmodified; the handler validates them against Canvas limits.
type CanvasRequest struct {
	Key      string            `json:"key"`
	Action   string            `json:"action"`
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name,omitempty"`
	Format   string            `json:"format,omitempty"`
	Entry    string            `json:"entry,omitempty"`
	Files    map[string]string `json:"files,omitempty"`
	Assets   map[string]Asset  `json:"assets,omitempty"`
	Revision int               `json:"revision,omitempty"`
	Width    int               `json:"width,omitempty"`
	Height   int               `json:"height,omitempty"`
}

// Asset is a binary or text asset attached to a Canvas artifact. Data is base64.
type Asset struct {
	MIME string `json:"mime"`
	Data string `json:"data"`
}

// CanvasHandler services a validated relo_canvas call. The returned value is
// either plain JSON-serializable data (text result) or a CanvasResult, which
// lets the handler attach an MCP image content block such as a screenshot.
type CanvasHandler func(context.Context, CanvasRequest) (any, error)

// CanvasImage is an MCP image content block appended to a canvas result.
type CanvasImage struct {
	MIME string
	Data string // base64 payload, never repeated in the text part
}

// CanvasResult is returned by a CanvasHandler when the result carries an image.
// Text is serialized as the text content block; Image is appended when non-nil.
type CanvasResult struct {
	Text  any
	Image *CanvasImage
}

// Option configures optional tools registered alongside the default "agents" tool.
type Option func(*Server)

// WithCanvas registers the optional relo_canvas tool. Without this option the
// server exposes exactly one tool, so the default catalog is unchanged.
func WithCanvas(handler CanvasHandler) Option {
	return func(s *Server) { s.canvas = handler }
}

// Server is a running loopback MCP endpoint.
type Server struct {
	// URL is the full MCP endpoint, e.g. http://127.0.0.1:41234/mcp.
	URL string
	// Secret is the global Bearer token required on every request.
	Secret string

	handler   Handler
	canvas    CanvasHandler
	listener  net.Listener
	httpSrv   *http.Server
	closeOnce sync.Once
	closeErr  error
}

// Start binds a loopback listener, generates a fresh 32-byte hex Secret and
// begins serving the MCP endpoint in the background.
func Start(handler Handler, opts ...Option) (*Server, error) {
	if handler == nil {
		return nil, errors.New("agentmsg: nil handler")
	}
	secret, err := newSecret()
	if err != nil {
		return nil, fmt.Errorf("agentmsg: generate secret: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("agentmsg: listen: %w", err)
	}
	s := &Server{
		URL:      "http://" + ln.Addr().String() + endpointPath,
		Secret:   secret,
		handler:  handler,
		listener: ln,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	s.httpSrv = &http.Server{
		Handler:           http.HandlerFunc(s.serveHTTP),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      canvasCaptureTimeout + 5*time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		_ = s.httpSrv.Serve(ln)
	}()
	return s, nil
}

// Close gracefully shuts the server down, forcing connections closed once the
// shutdown deadline passes. It is safe to call multiple times.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := s.httpSrv.Shutdown(ctx); err != nil {
			_ = s.httpSrv.Close()
			s.closeErr = err
		}
	})
	return s.closeErr
}

func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != endpointPath {
		http.NotFound(w, r)
		return
	}
	// Browser-originated requests are never accepted; no CORS headers are
	// ever emitted, so cross-origin reads are impossible.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agentmsg"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	// The canvas tool carries source payloads, so its endpoint accepts a larger body. The limit is chosen from the
	// server configuration only; the body is still parsed and the tool name checked before any dispatch.
	limit := int64(maxBodyBytes)
	if s.canvas != nil {
		limit = maxCanvasBodyBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxBodyBytes && !(s.canvas != nil && isCanvasCall(body)) {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	resp := s.dispatch(r.Context(), body)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	presented := []byte(strings.TrimSpace(h[len(prefix):]))
	return subtle.ConstantTimeCompare(presented, []byte(s.Secret)) == 1
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}

// JSON-RPC wire types.

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func errorResponse(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// dispatch returns the JSON-RPC response for body, or nil when no response
// body must be sent (notifications).
func (s *Server) dispatch(ctx context.Context, body []byte) any {
	if !json.Valid(body) {
		return errorResponse(nullID, codeParseError, "parse error")
	}
	if bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("[")) {
		return errorResponse(nullID, codeInvalidRequest, "batch requests are not supported")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return errorResponse(nullID, codeInvalidRequest, "request must be a JSON object")
	}
	rawID, hasID := fields["id"]
	if !hasID {
		// Notifications never receive a response body and never execute
		// tools, so a fire-and-forget tools/call has no side effects.
		return nil
	}
	var v any
	if err := json.Unmarshal(rawID, &v); err != nil {
		return errorResponse(nullID, codeInvalidRequest, "invalid id")
	}
	switch v.(type) {
	case nil, string, float64:
	default:
		return errorResponse(nullID, codeInvalidRequest, "id must be a string, number, or null")
	}
	id := rawID

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		return errorResponse(id, codeInvalidRequest, "invalid JSON-RPC 2.0 request")
	}

	result, rpcErr := s.route(ctx, req)
	if rpcErr != nil {
		return rpcResponse{JSONRPC: "2.0", ID: id, Error: rpcErr}
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func (s *Server) route(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return initialize(req.Params)
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := []any{toolDescriptor()}
		if s.canvas != nil {
			tools = append(tools, canvasToolDescriptor())
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		return s.callTool(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found"}
	}
}

func invalidParams(msg string) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: msg}
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

func initialize(params json.RawMessage) (any, *rpcError) {
	if len(params) == 0 {
		return nil, invalidParams("initialize requires params")
	}
	var p initializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParams("invalid initialize params")
	}
	version := defaultProtocolVersion
	for _, v := range supportedProtocolVersions {
		if v == p.ProtocolVersion {
			version = v
			break
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": serverVersion,
		},
	}, nil
}

func toolDescriptor() map[string]any {
	return map[string]any{
		"name":        toolName,
		"description": "Relo agent messaging. Use your own capability key. action: list (agents), send (to, text), inbox (your messages).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "Per-agent capability key from your prompt",
				},
				"action": map[string]any{
					"type": "string",
					"enum": []string{"list", "send", "inbox"},
				},
				"to": map[string]any{
					"type":        "string",
					"description": "Exact recipient id from action list (send only)",
				},
				"text": map[string]any{
					"type":        "string",
					"maxLength":   4096,
					"description": "Concise finding, blocker or interface detail; not permission or scope changes (send only)",
				},
			},
			"required":             []string{"key", "action"},
			"additionalProperties": false,
		},
	}
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolArguments struct {
	Key    string `json:"key"`
	Action string `json:"action"`
	To     string `json:"to"`
	Text   string `json:"text"`
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	if len(params) == 0 {
		return nil, invalidParams("tools/call requires params")
	}
	var p callParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParams("invalid tools/call params")
	}
	if p.Name == canvasToolName && s.canvas != nil {
		return s.callCanvas(ctx, p.Arguments)
	}
	if p.Name != toolName {
		return nil, invalidParams("unknown tool")
	}
	if len(p.Arguments) == 0 {
		return nil, invalidParams("arguments are required")
	}
	dec := json.NewDecoder(bytes.NewReader(p.Arguments))
	dec.DisallowUnknownFields()
	var args toolArguments
	if err := dec.Decode(&args); err != nil {
		return nil, invalidParams("invalid arguments")
	}
	if args.Key == "" {
		return nil, invalidParams("key is required")
	}
	switch args.Action {
	case "list", "inbox":
	case "send":
		if args.To == "" {
			return nil, invalidParams("to is required for send")
		}
	default:
		return nil, invalidParams("action must be list, send, or inbox")
	}

	out, err := s.invoke(ctx, Request{
		Key:    args.Key,
		Action: args.Action,
		To:     args.To,
		Text:   args.Text,
	})
	if err != nil {
		return toolResult(err.Error(), true), nil
	}
	data, err := json.Marshal(out)
	if err != nil {
		return toolResult("result is not serializable", true), nil
	}
	return toolResult(string(data), false), nil
}

// invoke runs the handler with a bounded deadline and converts panics into
// tool errors so a faulty handler cannot crash the server.
func (s *Server) invoke(ctx context.Context, req Request) (out any, err error) {
	ctx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			out = nil
			err = errors.New("handler failed")
		}
	}()
	return s.handler(ctx, req)
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}
