package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// The tests run this test binary as the CLI. With RELO_FAKE_CLAUDE set it speaks Claude Code's stream protocol, as
// recorded from claude 2.1.304: it answers initialize, plays a scripted turn for each user message, asks permission
// for its Bash call, and honors interrupts. It logs its arguments and every input line to RELO_FAKE_CLAUDE_LOG.
func TestMain(m *testing.M) {
	if mode := os.Getenv("RELO_FAKE_CLAUDE"); mode != "" {
		os.Exit(fakeClaude(mode))
	}
	os.Exit(m.Run())
}

type fakeLog struct {
	Args       []string `json:"args,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	System     string   `json:"system,omitempty"`
	MCP        string   `json:"mcp,omitempty"`
	MCPMode    string   `json:"mcpMode,omitempty"`
	Input      string   `json:"input,omitempty"`
	Permission string   `json:"permission,omitempty"`
	Bridge     string   `json:"bridge,omitempty"`
}

func fakeClaude(mode string) int {
	args := os.Args[1:]
	logf := func(entry fakeLog) {
		path := os.Getenv("RELO_FAKE_CLAUDE_LOG")
		if path == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		b, _ := json.Marshal(entry)
		f.Write(append(b, '\n'))
	}
	if len(args) > 0 && args[0] == "auth" {
		fmt.Printf(`{"loggedIn": %t, "authMethod": "claude.ai", "subscriptionType": "max", "email": "someone@example.invalid"}`+"\n", mode != "signed-out")
		return 0
	}
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("2.1.304 (Claude Code)")
		return 0
	}
	dir, _ := os.Getwd()
	entry := fakeLog{Args: args, Dir: dir}
	for i, a := range args {
		if i+1 >= len(args) {
			break
		}
		switch a {
		case "--system-prompt-file":
			b, _ := os.ReadFile(args[i+1])
			entry.System = string(b)
		case "--mcp-config":
			b, _ := os.ReadFile(args[i+1])
			entry.MCP = string(b)
			if st, err := os.Stat(args[i+1]); err == nil {
				entry.MCPMode = st.Mode().Perm().String()
			}
		}
	}
	logf(entry)

	out := bufio.NewWriter(os.Stdout)
	emit := func(format string, a ...any) {
		fmt.Fprintf(out, format+"\n", a...)
		out.Flush()
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<24)
	next := func() (map[string]any, bool) {
		for in.Scan() {
			line := in.Text()
			logf(fakeLog{Input: line})
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil {
				return m, true
			}
		}
		return nil, false
	}
	userText := func(m map[string]any) string {
		msg, _ := m["message"].(map[string]any)
		s, _ := msg["content"].(string)
		return s
	}
	step := func(id string, in, read, write int) {
		emit(`{"type":"system","subtype":"status","status":"requesting","session_id":"s"}`)
		emit(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"%s","usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":1}}}}`, id, in, write, read)
	}
	endStep := func(stop string, in, read, write, output, thinking int) {
		emit(`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"%s"},"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d,"output_tokens_details":{"thinking_tokens":%d}}}}`, stop, in, write, read, output, thinking)
		emit(`{"type":"stream_event","event":{"type":"message_stop"}}`)
	}
	text := func(index int, parts ...string) {
		emit(`{"type":"stream_event","event":{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}}`, index)
		for _, p := range parts {
			b, _ := json.Marshal(p)
			emit(`{"type":"stream_event","event":{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}}`, index, b)
		}
		emit(`{"type":"stream_event","event":{"type":"content_block_stop","index":%d}}`, index)
	}
	result := func(text string, in, read, write, output, thinking int) {
		b, _ := json.Marshal(text)
		emit(`{"type":"result","subtype":"success","is_error":false,"result":%s,"stop_reason":"end_turn","terminal_reason":"completed","usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d,"output_tokens_details":{"thinking_tokens":%d}}}`, b, in, write, read, output, thinking)
	}
	aborted := func() {
		emit(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`)
		emit(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":null,"terminal_reason":"aborted_streaming","errors":["[ede_diagnostic] result_type=user"],"usage":{"input_tokens":2,"output_tokens":0}}`)
	}
	// waitFor reads input until a line of the given type arrives; other lines are kept for the caller in order.
	var pending []map[string]any
	waitFor := func(want string) map[string]any {
		for {
			m, ok := next()
			if !ok {
				return nil
			}
			if m["type"] == want {
				return m
			}
			if req, _ := m["request"].(map[string]any); m["type"] == "control_request" && req["subtype"] == "interrupt" && want == "interrupt" {
				return m
			}
			pending = append(pending, m)
		}
	}
	// sdkNames are the servers the initialize request names as SDK-hosted.
	var sdkNames []string
	for {
		var m map[string]any
		if len(pending) > 0 {
			m, pending = pending[0], pending[1:]
		} else {
			var ok bool
			if m, ok = next(); !ok {
				return 0
			}
		}
		switch m["type"] {
		case "control_request":
			req, _ := m["request"].(map[string]any)
			if req["subtype"] == "initialize" {
				names, _ := req["sdkMcpServers"].([]any)
				for _, n := range names {
					if name, ok := n.(string); ok {
						sdkNames = append(sdkNames, name)
					}
				}
				emit(`{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"claude_code_version":"2.1.304","account":{"email":"someone@example.invalid"},"models":[`+
					`{"value":"default","resolvedModel":"claude-sonnet-5-5","displayName":"Default (recommended)","supportedEffortLevels":["low","medium","high","xhigh","max"]},`+
					`{"value":"opus","resolvedModel":"claude-opus-5-5","displayName":"Opus 5.5","supportedEffortLevels":["low","medium","high","xhigh","max"]},`+
					`{"value":"sonnet","resolvedModel":"claude-sonnet-5-5","displayName":"Sonnet 5.5","supportedEffortLevels":["low","medium","high","xhigh","max"]},`+
					`{"value":"haiku","resolvedModel":"claude-haiku-5-5","displayName":"Haiku 5.5","supportedEffortLevels":["low","medium","high"]}]}}}`, m["request_id"])
			} else {
				emit(`{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}`, m["request_id"])
			}
		case "user":
			prompt := userText(m)
			switch {
			case strings.HasPrefix(prompt, "plain"):
				emit(`{"type":"system","subtype":"init","model":"claude-sonnet-5-5","tools":[]}`)
				step("m1", 3, 1000, 200)
				emit(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":40}`)
				text(0, `{"status":"completed",`, `"summary":"ok"}`)
				emit(`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"ok\"}"}]}}`)
				endStep("end_turn", 3, 1000, 200, 50, 20)
				result(`{"status":"completed","summary":"ok"}`, 3, 1000, 200, 50, 20)
			case strings.HasPrefix(prompt, "tool"):
				step("m1", 2, 0, 5000)
				emit(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Bash","input":{}}}}`)
				emit(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}}`)
				emit(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...","description":"Run the tests"}}]}}`)
				emit(`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`)
				endStep("tool_use", 2, 0, 5000, 80, 30)
				emit(`{"type":"control_request","request_id":"perm-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"go test ./...","description":"Run the tests"},"tool_use_id":"t1"}}`)
				reply := waitFor("control_response")
				resp, _ := reply["response"].(map[string]any)
				answer, _ := resp["response"].(map[string]any)
				behavior, _ := answer["behavior"].(string)
				logf(fakeLog{Permission: behavior})
				if behavior == "allow" {
					emit(`{"type":"system","subtype":"task_started","tool_use_id":"t1","task_type":"local_bash"}`)
					emit(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"ok","is_error":false}]}}`)
				} else {
					emit(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"denied","is_error":true}]}}`)
				}
				step("m2", 2, 5000, 120)
				text(0, "done after "+behavior)
				endStep("end_turn", 2, 5000, 120, 10, 0)
				result("done after "+behavior, 4, 5000, 5120, 90, 30)
			case strings.HasPrefix(prompt, "hub"):
				// An SDK-hosted MCP server's traffic: the CLI sends each JSON-RPC message as an mcp_message control
				// request and waits for the host's mcp_response. The last message is a tools/call that the hub fails.
				for i, message := range []string{
					`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
					`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
					`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"canvas"}}`,
				} {
					emit(`{"type":"control_request","request_id":"mcp-%d","request":{"subtype":"mcp_message","server_name":"relo","message":%s}}`, i, message)
					reply := waitFor("control_response")
					resp, _ := reply["response"].(map[string]any)
					answer, _ := resp["response"].(map[string]any)
					b, _ := json.Marshal(answer["mcp_response"])
					logf(fakeLog{Bridge: string(b)})
				}
				step("m1", 2, 0, 100)
				text(0, "hub done")
				endStep("end_turn", 2, 0, 100, 5, 0)
				result("hub done", 2, 0, 100, 5, 0)
			case strings.HasPrefix(prompt, "callable"):
				// The tools the model can call, worked out as claude 2.1.306 does (probed against the real CLI):
				// --tools names the built-ins; --safe-mode keeps only SDK-hosted MCP servers, that is an "sdk" entry
				// of --mcp-config or a name in initialize.sdkMcpServers, and drops an "http" entry whole; each kept
				// server is asked for its tools/list through mcp_message; --disallowedTools removes a tool.
				list := func(flag string) []string {
					var out []string
					for i, a := range args {
						if a == flag && i+1 < len(args) {
							out = append(out, strings.Split(args[i+1], ",")...)
						}
					}
					return out
				}
				var cfg struct {
					Servers map[string]struct {
						Type string `json:"type"`
					} `json:"mcpServers"`
				}
				_ = json.Unmarshal([]byte(entry.MCP), &cfg)
				hosted := map[string]bool{}
				for name, server := range cfg.Servers {
					if server.Type == "sdk" {
						hosted[name] = true
					}
				}
				for _, name := range sdkNames {
					hosted[name] = true
				}
				denied := map[string]bool{}
				for _, tool := range list("--disallowedTools") {
					denied[tool] = true
				}
				callable := list("--tools")
				ask := func(server, message string) map[string]any {
					emit(`{"type":"control_request","request_id":"mcp-%s","request":{"subtype":"mcp_message","server_name":"%s","message":%s}}`, server, server, message)
					reply := waitFor("control_response")
					resp, _ := reply["response"].(map[string]any)
					answer, _ := resp["response"].(map[string]any)
					out, _ := answer["mcp_response"].(map[string]any)
					b, _ := json.Marshal(out)
					logf(fakeLog{Bridge: string(b)})
					return out
				}
				for server := range hosted {
					ask(server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
					reply := ask(server, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
					result, _ := reply["result"].(map[string]any)
					tools, _ := result["tools"].([]any)
					for _, tool := range tools {
						name, _ := tool.(map[string]any)["name"].(string)
						if full := "mcp__" + server + "__" + name; !denied[full] {
							callable = append(callable, full)
						}
					}
					if prompt == "callable canvas" {
						ask(server, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"canvas","arguments":{"key":"k","action":"list"}}}`)
					}
				}
				sort.Strings(callable)
				names, _ := json.Marshal(callable)
				emit(`{"type":"system","subtype":"init","model":"claude-sonnet-5-5","tools":%s}`, names)
				step("m1", 2, 0, 100)
				text(0, string(names))
				endStep("end_turn", 2, 0, 100, 5, 0)
				result(string(names), 2, 0, 100, 5, 0)
			case strings.HasPrefix(prompt, "steer"):
				step("m1", 2, 0, 100)
				emit(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/w/a.go"}}]}}`)
				endStep("tool_use", 2, 0, 100, 5, 0)
				peer := waitFor("user")
				emit(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"package a","is_error":false}]}}`)
				step("m2", 2, 100, 40)
				text(0, "got: "+userText(peer))
				endStep("end_turn", 2, 100, 40, 5, 0)
				result("got: "+userText(peer), 4, 100, 140, 10, 0)
			case strings.HasPrefix(prompt, "hang"), strings.HasPrefix(prompt, "slow"):
				step("m1", 2, 0, 100)
				if waitFor("interrupt") == nil {
					return 0
				}
				aborted()
			case strings.HasPrefix(prompt, "crash"):
				fmt.Fprintln(os.Stderr, "boom: the fake CLI fell over")
				return 3
			default:
				result("?", 1, 0, 0, 1, 0)
			}
		}
	}
}
