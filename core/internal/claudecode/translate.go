package claudecode

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/relocode/relo/internal/runtime"
)

// The CLI speaks newline-delimited JSON in both directions (`--input-format stream-json --output-format
// stream-json`). Relo writes user messages and control requests (initialize, interrupt) and answers the CLI's
// permission requests; the CLI writes its init, status, Anthropic stream events (`--include-partial-messages`),
// whole assistant and tool-result messages, and one result per turn.

func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func controlRequest(id string, request map[string]any) map[string]any {
	return map[string]any{"type": "control_request", "request_id": id, "request": request}
}

func userMessage(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}, "parent_tool_use_id": nil, "session_id": ""}
}

// lineReader reads lines of any length: a tool result can carry a whole file.
type lineReader struct{ r *bufio.Reader }

func newLineReader(r io.Reader) *lineReader { return &lineReader{bufio.NewReaderSize(r, 64*1024)} }

func (l *lineReader) next() ([]byte, error) {
	for {
		line, err := l.r.ReadBytes('\n')
		if trimmed := strings.TrimSpace(string(line)); trimmed != "" {
			return []byte(trimmed), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// apiUsage is Anthropic's usage block. output_tokens includes the thinking, which Relo counts as reasoning.
type apiUsage struct {
	Input      int `json:"input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	Output     int `json:"output_tokens"`
	Details    struct {
		Thinking int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (u apiUsage) usage() runtime.Usage {
	thinking := min(u.Details.Thinking, u.Output)
	return runtime.Usage{Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output - thinking, Reasoning: thinking}
}

// turnEnd is what a result line says about the turn it closes.
type turnEnd struct {
	text    string
	err     string
	aborted bool // the CLI stopped the turn because it was interrupted
	usage   runtime.Usage
}

// translator follows one session's output and turns it into Relo events. It keeps only what a line needs from the
// lines before it: the open message's blocks and the latest call's prompt size.
type translator struct {
	blocks  map[int]block
	stepIn  apiUsage
	lastCtx int
	inputAt map[int]time.Time // last progress event sent for a tool call's streaming arguments
	now     func() time.Time
}

type block struct{ kind, id, tool string }

// inputEvery spaces the progress events sent while a tool call's arguments stream in: a large file write streams
// for a long time, and any event tells Relo the agent is alive.
const inputEvery = 500 * time.Millisecond

func (t *translator) startTurn() { t.lastCtx = 0 }

// feed turns one output line into events. A result line also returns the turn's end.
func (t *translator) feed(line []byte) ([]runtime.Event, *turnEnd) {
	if t.now == nil {
		t.now = time.Now
	}
	var msg struct {
		Type    string          `json:"type"`
		Subtype string          `json:"subtype"`
		Status  string          `json:"status"`
		Event   json.RawMessage `json:"event"`
		Message json.RawMessage `json:"message"`
		ToolID  string          `json:"tool_use_id"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return nil, nil
	}
	switch msg.Type {
	case "stream_event":
		return t.stream(msg.Event), nil
	case "assistant":
		return t.assistant(msg.Message), nil
	case "user":
		return toolResults(msg.Message), nil
	case "result":
		return nil, result(line, t.lastCtx)
	case "system":
		switch msg.Subtype {
		case "status":
			switch msg.Status {
			case "requesting":
				return []runtime.Event{{Type: "session.status", Phase: "model"}}, nil
			case "compacting":
				return []runtime.Event{{Type: "session.compaction", Phase: "compacting", Compaction: "started"}}, nil
			}
		case "compact_boundary":
			return []runtime.Event{{Type: "session.compaction", Compaction: "ended"}}, nil
		case "thinking_tokens":
			return []runtime.Event{{Type: "session.reasoning", Phase: "reasoning"}}, nil
		case "task_started", "task_progress", "task_notification":
			// A command the CLI runs as a task (every Bash call). It is progress on the open tool call.
			return []runtime.Event{{Type: "session.tool.progress", Phase: "tool", ToolID: msg.ToolID}}, nil
		}
	}
	return nil, nil
}

func (t *translator) stream(raw json.RawMessage) []runtime.Event {
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			Usage apiUsage `json:"usage"`
		} `json:"message"`
		Block struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage *apiUsage `json:"usage"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "message_start":
		t.blocks, t.inputAt, t.stepIn = map[int]block{}, map[int]time.Time{}, ev.Message.Usage
		return []runtime.Event{{Type: "session.step.started", Phase: "model"}}
	case "content_block_start":
		b := block{kind: ev.Block.Type, id: ev.Block.ID, tool: reloTool(ev.Block.Name)}
		if t.blocks == nil {
			t.blocks = map[int]block{}
		}
		t.blocks[ev.Index] = b
		switch b.kind {
		case "thinking", "redacted_thinking":
			return []runtime.Event{{Type: "session.reasoning", Phase: "reasoning"}}
		case "text":
			return []runtime.Event{{Type: "session.text", Phase: "writing"}}
		case "tool_use", "server_tool_use":
			return []runtime.Event{{Type: "session.tool", Phase: "tool", Tool: b.tool, ToolID: b.id}}
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			return []runtime.Event{{Type: "session.delta", Phase: "writing", Delta: ev.Delta.Text}}
		case "thinking_delta":
			return []runtime.Event{{Type: "session.reasoning", Phase: "reasoning"}}
		case "input_json_delta":
			now := t.now()
			if t.inputAt == nil {
				t.inputAt = map[int]time.Time{}
			}
			if now.Sub(t.inputAt[ev.Index]) < inputEvery {
				return nil
			}
			t.inputAt[ev.Index] = now
			b := t.blocks[ev.Index]
			return []runtime.Event{{Type: "session.tool.input", Phase: "tool", ToolID: b.id}}
		}
	case "message_delta":
		u := t.stepIn
		if ev.Usage != nil {
			u = *ev.Usage
			// message_delta repeats the call's input usage; an older CLI sent only the output count.
			if u.Input+u.CacheRead+u.CacheWrite == 0 {
				u.Input, u.CacheRead, u.CacheWrite = t.stepIn.Input, t.stepIn.CacheRead, t.stepIn.CacheWrite
			}
		}
		usage := u.usage()
		t.lastCtx = usage.PromptTokens()
		usage.Context = t.lastCtx
		return []runtime.Event{{Type: "session.step.ended", ContextTokens: t.lastCtx, MoreSteps: ev.Delta.StopReason == "tool_use", Usage: &usage}}
	}
	return nil
}

// assistant reads a whole message for what streaming did not give: a tool call's complete arguments.
func (t *translator) assistant(raw json.RawMessage) []runtime.Event {
	var m struct {
		Content []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var out []runtime.Event
	for _, c := range m.Content {
		if c.Type == "tool_use" || c.Type == "server_tool_use" {
			out = append(out, runtime.Event{Type: "session.tool", Phase: "tool", Tool: reloTool(c.Name), ToolID: c.ID, ToolInput: describe(c.Name, c.Input)})
		}
	}
	return out
}

func toolResults(raw json.RawMessage) []runtime.Event {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var parts []struct {
		Type   string `json:"type"`
		ToolID string `json:"tool_use_id"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return nil // a plain string: Relo's own message echoed back
	}
	var out []runtime.Event
	for _, p := range parts {
		if p.Type == "tool_result" {
			out = append(out, runtime.Event{Type: "session.tool.done", Phase: "tool", ToolID: p.ToolID, ToolDone: true})
		}
	}
	return out
}

func result(line []byte, lastCtx int) *turnEnd {
	var r struct {
		Subtype        string   `json:"subtype"`
		IsError        bool     `json:"is_error"`
		Result         *string  `json:"result"`
		Errors         []string `json:"errors"`
		TerminalReason string   `json:"terminal_reason"`
		Usage          apiUsage `json:"usage"`
	}
	if json.Unmarshal(line, &r) != nil {
		return &turnEnd{err: "Claude Code sent an unreadable result"}
	}
	end := &turnEnd{usage: r.Usage.usage(), aborted: strings.HasPrefix(r.TerminalReason, "aborted")}
	end.usage.Context = lastCtx
	if r.Result != nil {
		end.text = *r.Result
	}
	if r.Subtype != "success" || r.IsError {
		end.err = end.text
		for _, e := range r.Errors {
			if e != "" && !strings.HasPrefix(e, "[ede_diagnostic]") {
				end.err = e
				break
			}
		}
		if end.err == "" {
			end.err = "Claude Code ended the turn: " + strings.ReplaceAll(r.Subtype, "_", " ")
		}
	}
	return end
}

// Relo's tool names (runtime.ToolsFor, the OpenCode set) and Claude Code's.
var claudeTools = map[string]string{
	"read": "Read", "grep": "Grep", "glob": "Glob", "edit": "Edit", "write": "Write",
	"bash": "Bash", "shell": "Bash", "webfetch": "WebFetch", "websearch": "WebSearch",
}

var reloTools = map[string]string{
	"Read": "read", "Grep": "grep", "Glob": "glob", "Edit": "edit", "Write": "write", "NotebookEdit": "edit",
	"Bash": "shell", "WebFetch": "webfetch", "WebSearch": "websearch",
}

// mcpServer is the name Relo's coordination hub is registered under; Claude Code calls its tools mcp__relo__<tool>.
const mcpServer = "relo"

// reloMCPTools maps Relo's hub tools to the names Claude Code gives them.
var reloMCPTools = map[string]string{
	runtime.ReloAgentsTool: "mcp__" + mcpServer + "__agents",
	runtime.ReloImageTool:  "mcp__" + mcpServer + "__image",
	runtime.ReloCanvasTool: "mcp__" + mcpServer + "__canvas",
}

func reloTool(name string) string {
	if r, ok := reloTools[name]; ok {
		return r
	}
	if tool, ok := strings.CutPrefix(name, "mcp__"+mcpServer+"__"); ok {
		return "relo_" + tool
	}
	return strings.ToLower(name)
}

// describe is the one line shown for a tool call: the command, the path, the pattern.
func describe(name string, input map[string]any) string {
	str := func(key string) string { s, _ := input[key].(string); return s }
	var s string
	switch name {
	case "Bash":
		s = str("command")
	case "Read", "Edit", "Write", "NotebookEdit":
		s = str("file_path")
		if s == "" {
			s = str("notebook_path")
		}
	case "Grep", "Glob":
		s = str("pattern")
		if p := str("path"); p != "" {
			s += " in " + p
		}
	case "WebFetch":
		s = str("url")
	case "WebSearch":
		s = str("query")
	default:
		s = str("action")
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}
