package orchestrator

import (
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/relocode/relo/internal/runtime"
)

const loopNudge = "Repeated identical tool calls detected. Stop repeating them; use a different approach or report the blocker."

// errLoopDetected ends a run whose agent repeated a call after being steered once.
var errLoopDetected = errors.New("loop_detected")

type loopCall struct {
	name    string
	counted bool
}

// loopDetector counts calls, not streaming updates. It never retains tool arguments.
type loopDetector struct {
	window [8][32]byte
	n      int
	hits   int
	calls  map[string]*loopCall
	order  []string
}

// observe returns 1 for the first loop and 2 for any subsequent loop.
func (d *loopDetector) observe(ev runtime.Event) int {
	if ev.ToolID == "" || (ev.Tool == "" && ev.PartType != "tool" && !ev.ToolDone) {
		return 0
	}
	if d.calls == nil {
		d.calls = make(map[string]*loopCall)
	}
	call := d.calls[ev.ToolID]
	if call == nil {
		call = &loopCall{}
		d.calls[ev.ToolID] = call
		d.order = append(d.order, ev.ToolID)
		if len(d.order) > 64 {
			delete(d.calls, d.order[0])
			d.order = d.order[1:]
		}
	}
	if ev.Tool != "" {
		call.name = ev.Tool
	}
	if call.counted || call.name == "" {
		return 0
	}
	args, available := toolArguments(ev)
	if !available && !ev.ToolDone {
		return 0
	}
	// Missing arguments must not make unrelated calls appear identical.
	if !available {
		args = map[string]any{"unavailable_call_id": ev.ToolID}
	}
	encoded, err := json.Marshal([]any{call.name, args})
	if err != nil {
		return 0
	}
	call.counted = true
	sig := sha256.Sum256(encoded)
	if d.n == len(d.window) {
		copy(d.window[:], d.window[1:])
		d.n--
	}
	d.window[d.n] = sig
	d.n++
	matches := 0
	for _, previous := range d.window[:d.n] {
		if previous == sig {
			matches++
		}
	}
	if matches < 3 {
		return 0
	}
	d.n = 0
	d.hits++
	if d.hits == 1 {
		return 1
	}
	return 2
}

// OpenCode sends complete arguments either on a called event or in a tool part's state. Claude Code sends them
// as the input of its tool_use message, at the top level of Raw.
func toolArguments(ev runtime.Event) (any, bool) {
	props := ev.Raw
	if nested, ok := props["properties"].(map[string]any); ok {
		props = nested
	}
	if args, ok := props["input"]; ok {
		return args, true
	}
	part, _ := props["part"].(map[string]any)
	state, _ := part["state"].(map[string]any)
	if state["status"] == "pending" {
		return nil, false
	}
	args, ok := state["input"]
	return args, ok
}
