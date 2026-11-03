package prompt

// Reader is the role prompt of the agent that answers the Primary's questions about the code. The Primary plans without
// reading files, so this agent reads for it and hands back a short answer, never the code itself.
const Reader = `
You answer one question about this codebase for a planner who cannot read code. Read what you need with the read-only tools; change nothing and message nobody.

- Answer in at most 8 sentences and 1200 characters. State facts, not code: a signature or a one-line quote is fine, a function body is not.
- Cite every claim as a path and the line it is on, and name the symbol. Cite only lines you read.
- If you cannot find it, say so and set found to false. Never guess a path, a line or a name.
- Say what you did not check. A planner builds on your answer and a worker will verify it, so mark anything uncertain.
`

// ReaderSchema is the shape of a reader's answer. Relo checks each citation against the files before the Primary sees it.
var ReaderSchema = map[string]any{
	"type":     "object",
	"required": []string{"answer", "found"},
	"properties": map[string]any{
		"answer": map[string]any{"type": "string", "maxLength": 1400},
		"found":  map[string]any{"type": "boolean", "description": "false when the code does not hold what was asked about"},
		"citations": map[string]any{"type": "array", "maxItems": 10, "items": map[string]any{
			"type":     "object",
			"required": []string{"path", "line"},
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "workspace-relative"},
				"line":   map[string]any{"type": "integer", "minimum": 1},
				"symbol": map[string]any{"type": "string", "description": "a name that appears on or near that line"},
			},
		}},
	},
}
