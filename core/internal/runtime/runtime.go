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
// Vision is "yes", "no" or empty for unknown, and VisionSource says where that came from (see internal/vision).
type Model struct {
	ID           string   `json:"id"`
	Provider     string   `json:"provider"`
	Name         string   `json:"name"`
	Context      int      `json:"context,omitempty"`
	Output       int      `json:"output,omitempty"`
	Efforts      []string `json:"efforts,omitempty"`
	Vision       string   `json:"vision,omitempty"`
	VisionSource string   `json:"visionSource,omitempty"`
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
	// Context is the latest provider call's prompt tokens: input + cacheRead + cacheWrite, excluding output.
	// The other fields add up over every call of a turn, so only this one says how full the window is.
	// 0 means the runtime did not report it.
	Context int `json:"context,omitempty"`
}

// PromptTokens counts one provider request's input, including cache hits and writes, never generated output.
// Do not use turn/session totals to measure the context window.
func (u Usage) PromptTokens() int {
	return u.Input + u.CacheRead + u.CacheWrite
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
	// ContextTokens is set on the event that ends one model step: that provider call's prompt tokens (input and cached
	// input, excluding output). Usage on the same event is that one call too, but surrounding usage events add up
	// over the turn. 0 when the event says nothing about it.
	ContextTokens int `json:"contextTokens,omitempty"`
	// MoreSteps is true on that same event when the step ended in tool calls, so the turn goes on with another
	// model call. A step that ends in plain text is the end of the turn.
	MoreSteps bool `json:"moreSteps,omitempty"`
	// Compaction follows a history summary the runtime is running: "started", "ended" or "failed".
	Compaction string `json:"compaction,omitempty"`
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

// Compactor is implemented by runtimes that can summarize a session's history while a turn is running. The summary
// replaces the older messages and the turn carries on from it, with the same session and the same edits. The request
// is admitted at once and runs at the next step boundary; progress arrives as events with Compaction set.
type Compactor interface {
	Compact(ctx context.Context, session SessionID) error
}

// MessageTools describes the Relo-owned MCP endpoint a runtime should register so agents can reach the coordination hub.
type MessageTools struct {
	URL    string
	Secret string
}

// MessageRuntime is optional. Runtimes that can host peer messaging implement it; callers must type-assert.
// ConfigureMessages registers the MCP endpoint for a directory. DeliverMessage injects one peer message into a
// session without starting or resetting a turn.
type MessageRuntime interface {
	ConfigureMessages(ctx context.Context, directory string, tools MessageTools) error
	DeliverMessage(ctx context.Context, session SessionID, id, text string) error
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
			// Enabled after the wildcard; setTools emits it as an explicit allow so it survives the "*" deny.
			// The canvas tool joins it through WithCanvas, on the same terms: the Primary keeps a read-only
			// canvas path, and what it may do with that tool is decided where the caller's role is known.
			ReloAgentsTool: true,
		}
	}
	tools := DefaultTools(mode, mutating)
	switch role {
	case "architect", "debug":
		// These may need to look something up.
	default:
		tools["webfetch"], tools["websearch"] = false, false
	}
	tools[ReloAgentsTool] = true
	return tools
}

// ReloAgentsTool is the MCP tool, served by Relo's coordination hub under the "relo" server, that lets an agent
// list and message its peers. OpenCode exposes it as "<server>_<tool>".
const ReloAgentsTool = "relo_agents"

// ReloImageTool is the MCP tool that shows an agent image files from its workspace. It is offered only to agents whose
// model is not known to be text only; see WithImages.
const ReloImageTool = "relo_image"

// WithImages adds the relo_image allow rule to a tool policy when enabled. The Primary never gets it: it plans from
// curated context and reads no files, and the core refuses its calls as well.
func WithImages(tools map[string]bool, role string, enabled bool) map[string]bool {
	if !enabled || role == RolePrimary {
		return tools
	}
	tools[ReloImageTool] = true
	return tools
}

// ReloCanvasTool is the optional MCP tool that drives Relo Canvas artifacts through the extension. It is enabled
// only when the connected extension advertises Canvas support.
const ReloCanvasTool = "relo_canvas"

// WithCanvas adds the relo_canvas allow rule to a tool policy, scoped to the canvas actions the turn may use.
// Without it the policy is returned unchanged, so an extension that does not support Canvas never sees the tool.
//
// The rule names the tool, not the actions: the MCP catalog is served by one loopback endpoint that authenticates
// with a single server-wide secret (internal/agentmsg/agentmsg.go), so the action list cannot be advertised per role
// there. The scope passed in is what the turn advertises to the model, and it is the scope the core enforces (see
// internal/agent/canvas_policy.go and internal/orchestrator/canvas.go): a builder role authors artifacts with every
// action, a role that may not author keeps the read-only path (list, open, inspect) to check a worker's result, and
// the core refuses the mutating actions before the extension is asked. A turn with no canvas action at all gets no
// canvas tool.
//
// The Primary's policy denies every tool with "*" and re-allows the ones it may use. The explicit allow this
// function adds must survive that deny, which is why opencode.setTools sends the allows of a wildcard-denied policy
// after the deny instead of in map order. The re-enable path hands back the tool, never an action: the scope of a
// role that may not author holds no create or update, and the core refuses those calls before the extension is
// asked.
func WithCanvas(tools map[string]bool, enabled bool, actions []string) map[string]bool {
	if !enabled || len(actions) == 0 {
		return tools
	}
	tools[ReloCanvasTool] = true
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
