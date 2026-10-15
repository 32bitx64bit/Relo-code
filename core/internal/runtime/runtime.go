package runtime

import "context"

type SessionID string

type SessionConfig struct {
	Title     string
	Directory string
	Parent    string
	ReadOnly  bool
}

type AgentTask struct {
	AgentID    string
	Role       string
	Model      string
	Provider   string
	System     string
	Prompt     string
	Tools      map[string]bool
	Effort     string // reasoning effort (variant id); empty = model default
	Schema     map[string]any
	Directory  string
	TimeoutSec int
}

// Model is one selectable model. Context/Output are the model's own maximums in tokens (0 = unknown).
// Efforts are the reasoning effort levels the model accepts (OpenCode "variants"); empty = no effort control.
type Model struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Name     string   `json:"name"`
	Context  int      `json:"context,omitempty"`
	Output   int      `json:"output,omitempty"`
	Efforts  []string `json:"efforts,omitempty"`
}

// EffectiveWindow is the context window Relo plans against: the smaller of the global limit and the
// model maximum. A zero global limit means "no cap" and a zero model max means "unknown"; if both are
// zero the window is unknown (0).
func EffectiveWindow(global, modelMax int) int {
	switch {
	case global > 0 && modelMax > 0:
		return min(global, modelMax)
	case global > 0:
		return global
	default:
		return max(modelMax, 0)
	}
}

type Provider struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Models []Model `json:"models"`
}

type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	// Context is how much the latest provider call held: its input, cached input and output. Input and
	// the other fields add up over every call of a turn, so only this one says how full the window is.
	// 0 means the runtime did not report it.
	Context int `json:"context,omitempty"`
}

type Event struct {
	Type       string         `json:"type"`
	Session    string         `json:"session"`
	Text       string         `json:"text,omitempty"`
	Delta      string         `json:"delta,omitempty"`
	PartType   string         `json:"partType,omitempty"`
	Permission map[string]any `json:"permission,omitempty"`
	Error      string         `json:"error,omitempty"`
	Idle       bool           `json:"idle,omitempty"`
	Raw        map[string]any `json:"raw,omitempty"`
	Done       bool           `json:"done,omitempty"`
	JSON       string         `json:"json,omitempty"`
	Usage      *Usage         `json:"usage,omitempty"`
	// Phase is what the model is doing right now, normalised across OpenCode API versions: model (waiting for
	// the first token), reasoning, writing (reply text), tool, compacting, retry. Empty when the event says nothing
	// about it. Only "writing" and empty-phase deltas are reply text; everything else must never reach the body.
	Phase    string `json:"phase,omitempty"`
	Tool     string `json:"tool,omitempty"`     // tool name, set on the event that starts a call
	ToolID   string `json:"toolId,omitempty"`   // ties a call's events together
	ToolDone bool   `json:"toolDone,omitempty"` // a tool call finished (success or failure)
	// ToolInput is a one-line description of what a tool call was asked to do (the command, or the path), on the
	// event that carries the call's arguments.
	ToolInput string `json:"toolInput,omitempty"`
	Attempt   int    `json:"attempt,omitempty"` // retry attempt, with Phase "retry"
	// Interrupted is the runtime confirming that a Cancel stopped the turn. It is not a terminal event: the session
	// stays usable, and a new task can be sent to it.
	Interrupted bool `json:"interrupted,omitempty"`
}

type Health struct {
	Healthy bool   `json:"healthy"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
	Error   string `json:"error,omitempty"`
}

type AgentRuntime interface {
	StartSession(ctx context.Context, cfg SessionConfig) (SessionID, error)
	SendTask(ctx context.Context, session SessionID, task AgentTask) error
	Events(ctx context.Context, session SessionID) (<-chan Event, error)
	Cancel(ctx context.Context, session SessionID) error
	CloseSession(ctx context.Context, session SessionID) error
	Models(ctx context.Context) ([]Model, error)
	Providers(ctx context.Context) ([]Provider, error)
	Health(ctx context.Context) Health
	Close() error
}

// RolePrimary must match agent.Primary. Duplicated as a string because this package
// cannot import internal/agent without a cycle risk.
const RolePrimary = "primary"

// ToolsFor returns the tool policy for one agent turn. Primary gets every tool disabled:
// it reasons over curated context and returns structured JSON, and investigation is
// delegated to probes. Every other role gets DefaultTools, without the web tools unless the role researches.
func ToolsFor(role, mode string, mutating bool) map[string]bool {
	if role == RolePrimary {
		return map[string]bool{
			// "*" denies whatever else OpenCode offers (shell, webfetch, task, ...); the names are listed too so the
			// intent reads plainly and older servers that match by name still deny them.
			"*":    false,
			"read": false, "grep": false, "glob": false, "list": false, "ls": false,
			"write": false, "edit": false, "bash": false, "shell": false, "webfetch": false, "websearch": false,
		}
	}
	tools := DefaultTools(mode, mutating)
	switch role {
	case "architect", "debug":
		// These may need to look something up.
	default:
		tools["webfetch"], tools["websearch"] = false, false
	}
	return tools
}

func DefaultTools(mode string, mutating bool) map[string]bool {
	tools := map[string]bool{
		"read": true, "grep": true, "glob": true, "list": true, "ls": true,
		// OpenCode describes every tool it offers in every call of every agent, and its "Code Mode" repeats them all in
		// the system prompt. These do nothing Relo needs: it plans delegation and tracks progress itself, runs commands
		// through shell, and has no skills or questions to put to a person mid-task.
		"task": false, "subagent": false, "execute": false, "todowrite": false, "todoread": false, "skill": false, "question": false,
	}
	if mode == "plan" || mode == "review" || !mutating {
		tools["write"] = false
		tools["edit"] = false
		tools["bash"] = mode != "review"
		return tools
	}
	tools["write"] = true
	tools["edit"] = true
	tools["bash"] = true
	return tools
}
