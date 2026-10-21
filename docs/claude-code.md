# Claude Code backend

Relo can run agents on the Claude Code CLI (`claude`) with the CLI's own sign-in. Its models show up as
`claude-code/<model>` next to the models of your providers, and any role can use them: the Primary, the tiers, a
difficulty level. A run can mix them, for example the Primary on `claude-code/claude-opus-5-5` and Tier 2 on a
cheap provider model.

The reason to use it for Claude: the CLI keeps Claude's signed thinking between tool steps and caches the prompt.
Claude reached through an OpenAI-format route loses that thinking on every tool step and works its plan out again
(40-60k output tokens and 4-7 minutes per step at xhigh, see [quickstart.md](quickstart.md#4-configure-models)).

## Turning it on

1. Install Claude Code and sign in once in a terminal (`claude`, then `/login`, or `claude auth login`).
2. Set `relo.claudeCode.enabled` to on (Settings, Claude Code). The backend restarts once agents are idle.
3. Pick `claude-code/...` models for the roles you want.

Relo finds the CLI at `relo.claudeCode.path`, or else on `PATH`, at `~/.local/bin/claude` or
`~/.claude/local/claude`, or in the Claude Code editor extension (`~/.vscode*/extensions/anthropic.claude-code-*`,
also Cursor and Windsurf; the newest version wins). Both settings are user settings only, so a repository's
`.vscode/settings.json` cannot turn the backend on or point it at another binary.

When the CLI is missing or signed out, Claude Code's models are not offered, the rest of Relo starts as usual, and
VS Code shows the reason once. `backend.status` carries it under `claudeCode` (`enabled`, `ready`, `error`,
`version`, `plan`). The plan is the kind of sign-in (`Claude Max`, `API key`), never the account.

Usage counts against the signed-in account's plan limits, and what that account may be used for is set by its
terms with Anthropic. Relo never reads the CLI's credentials and does not log the account e-mail the CLI reports.

## What Relo runs

One CLI process per agent session, in the agent's workspace, in stream mode:

```text
claude -p --input-format stream-json --output-format stream-json --verbose --include-partial-messages
  --model <model> --effort <level> --system-prompt-file <Relo's prompt>
  --tools <built-in tools for the role> --allowedTools <...> --disallowedTools <...>
  --permission-prompt-tool stdio --safe-mode --strict-mcp-config --no-session-persistence
  [--mcp-config <file with Relo's hub>]
```

- **Relo's prompt replaces Claude Code's.** No CLAUDE.md, settings, hooks, plugins, skills or other MCP servers
  (`--safe-mode`, `--strict-mcp-config`).
- **Only the role's tools.** Relo's tool policy maps to Claude Code's tools: `read`, `grep`, `glob`, `edit`,
  `write`, `bash`, `webfetch`, `websearch` become `Read`, `Grep`, `Glob`, `Edit`, `Write`, `Bash`, `WebFetch`,
  `WebSearch`. A read-only session gets no `Edit`, `Write` or `Bash`. The Primary gets no built-in tool at all.
- **Relo's hub over MCP.** `relo_agents`, `relo_image` and `relo_canvas` reach the agent as `mcp__relo__agents`,
  `mcp__relo__image` and `mcp__relo__canvas`, enabled per role as with OpenCode. The hub's secret is written to a
  temporary file only the user can read, never put on the command line, and removed when the session closes.
- **Shell commands go through Relo's permissions.** Claude Code runs commands it judges read-only (`ls`, `cat`,
  `git status` and the like) without asking. Any other `Bash` call is a permission request, which Relo answers by
  `relo.permissions.*`, asking you when the policy says ask. File edits are pre-approved, as on OpenCode.
- **The session lasts as long as the agent's generation.** Every task of the session goes to the same process, so
  the conversation and its cache carry over. A new generation (a context refresh, a model change) starts a new
  process. Changing the model, effort, prompt or tools of a running session applies from its next session.
- **The environment** is Relo's own minus the variables that tie a Claude Code process to a parent session
  (`CLAUDECODE`, `CLAUDE_CODE_SESSION_ID` and the like), so a Relo started from inside Claude Code does not report
  into it.

The CLI's events map to Relo's: each model call starts and ends a step (with its prompt size and whether tool
calls follow), text streams into the reply, tool calls show with their command or path, and the turn ends with the
reply text and its usage (input, cache read, cache write, output and thinking, from the CLI's turn total).
**Cancel** sends the CLI an interrupt: the turn ends as interrupted and the session keeps its conversation.
**A timeout** interrupts, then kills the process if the CLI does not stop within 10 seconds. **A peer message**
reaches the running turn at its next step; with no turn running it waits for the agent's next task, as it does on
OpenCode.

## Differences from OpenCode

- **No compaction on Relo's request.** Claude Code compacts its own conversation when it nears the model's window,
  and Relo shows that as compaction. `relo.context.compactAt` does not apply to these sessions.
- **The window is not known.** The CLI does not report context windows, so the context chip shows no window and
  only `relo.models.contextLimit` caps the context Relo plans for.
- **A restart of the backend ends the sessions.** Nothing is persisted on the CLI side
  (`--no-session-persistence`); agents that were running come back as interrupted, as on OpenCode.
- **One process per agent.** Each is a CLI process (218 MB resident when idle, measured on 2.1.304).
  `relo.maxConcurrentAgents` bounds how many run at once.
- **Plan limits are the CLI's.** When the plan's usage window is used up, the turn fails with the CLI's message.

## Measured

One fix-and-extend task on a small Go repository (find the cause of a failing test, fix it, add a method and its
test), Sonnet 5.5 at xhigh on a Max sign-in. Weighted cost is in input-token equivalents: uncached + 1.25 × cache write
(2 × for the CLI's 1-hour writes) + 0.1 × cache read + 5 × output.

| Run | Tools offered | Model calls | Input (cache read) | Output (thinking) | Weighted | Time | Result |
|---|---|---|---|---|---|---|---|
| Claude Code with Relo's prompt and tools | 7 | 5 | 52,008 (39,050) | 3,275 (218) | 46,186 | 0.5 min | all checks pass |
| Claude Code with its own defaults | 28 | 4 | 110,536 (79,496) | 4,211 (463) | 91,077 | 0.6 min | all checks pass |

The first run had Relo's worker prompt and the worker's six tools; it asked for its report through a JSON schema,
which added the CLI's `StructuredOutput` tool (the backend does not: Relo reads the report from the reply). The
same task through 9router's OpenAI format was not measured in this run; per step it showed the 40-60k thinking
tokens described above.

The live tests (below) run four turns on Haiku 5.5 at low effort for 8 uncached input tokens, 15.8k cache reads,
6.1k cache writes and 245 output tokens, and a whole Relo run (Primary plans, a worker writes a file, the Primary
checks it) on Haiku in 17 seconds.

## Tests

- `core/internal/claudecode`: the test binary doubles as a scripted CLI that speaks the stream protocol as recorded
  from claude 2.1.304 (`fake_test.go`). The tests cover the command line and prompt file, tool lists per role,
  usage and step events, permission allow and deny, cancel and the next turn, timeout, a crash with the CLI's
  error, peer messages mid-turn, the hub secret's file, the model catalog, the signed-out case and finding the
  CLI. `TestTranslatorOnRecordedLines` runs the event mapping on lines recorded from the real CLI.
- `core/internal/runtime/mux_test.go`: routing by provider and by session prefix.
- Live, with an installed and signed-in CLI: `RELO_E2E_CLAUDE=1 go test ./internal/claudecode -run Live`
  (reply, permission, cancel, memory across turns) and `RELO_E2E_CLAUDE=1 go test ./internal/app -run
  TestLiveClaudeCodeRun` (a whole run; needs `opencode` too). `RELO_CLAUDE_PATH` picks the CLI.

## Code

- `core/internal/claudecode/`: finding and probing the CLI (`look.go`), the session process (`session.go`), the
  event mapping (`translate.go`), the runtime (`claudecode.go`).
- `core/internal/runtime/mux.go`: puts Claude Code behind the one runtime the orchestrator holds. A session is
  started on the backend of its model's provider, and its id (`claude_…`) routes every later call.
- `core/internal/app/claude.go`: starts the runtime with the backend when the setting is on.
