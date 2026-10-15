# OpenCode runtime

Relo speaks OpenCode v2 HTTP API. Calls use HTTP Basic auth (`opencode` plus the per-process `OPENCODE_SERVER_PASSWORD`). Relo generates a random password for each child server and never reads it from server output. HTTP 401 errors are surfaced as `opencode authentication failed (HTTP 401): ...`.

## Process

Relo Core starts `opencode serve --hostname 127.0.0.1 --port <ephemeral>`. Readiness uses authenticated `GET /api/info`; process startup retries ports if another process wins the reservation race. Ordinary HTTP requests have a timeout; SSE connections do not.

## Calls used

| Method | Path | Use |
|---|---|---|
| GET | `/api/info` | Health and version |
| GET | `/api/model`, `/api/provider` | Model and provider discovery |
| POST | `/api/session` | Create session at `{ location: { directory } }` |
| POST | `/api/session/:id/move` | Honor task worktree directory |
| POST | `/api/session/:id/model` | Set model only when provider/model changes |
| PUT | `/api/experimental/session/:id/instructions/entries/relo-system` | Stable system prompt; write only if SHA-256 changes |
| POST | `/api/session/:id/agent` | Select `build` agent for every turn |
| PATCH | `/api/session/:id` | Apply session permission rules for disabled tools |
| POST | `/api/session/:id/prompt` | Send task text |
| GET | `/api/event` | Authenticated SSE event stream |
| GET | `/api/session/:id/message` | Read assistant final text and token usage after idle |
| POST | `/api/session/:id/interrupt` | Cancel current work |
| DELETE | `/api/session/:id` | Close session |
| POST | `/api/session/:id/permission/:requestID/reply` | Reply with `once`, `always`, or `reject` |

OpenCode v2 prompt has no `system`, `tools`, or `format` field. Relo stores system text in stable `relo-system` instruction entry to preserve prompt prefixes; schema-constrained JSON is requested by the caller's system instruction, not API `format`. Disabled tools use session permission rules; this is not a strict per-prompt tool allowlist. Every turn selects `build`; OpenCode's `plan` agent is not used because its persona leaks "Plan mode" text into user chat. Read-only tasks deny the `edit` permission (covers write/edit tools) and `bash` via session permission rules. OpenCode `build` agent instructions still apply.

Each assistant message supplies `tokens` (`input`, `output`, `reasoning`, `cache.read`, `cache.write`). Relo emits per-message usage and cumulative usage with terminal `Done`. Terminal event contains complete assistant text; `JSON` is set only for a JSON object, including a fenced `json` object.

SSE v2 data is `{ id, type, data }`. Relo filters by session, reconnects with backoff and `Last-Event-ID`, and queues events so a full subscriber buffer cannot drop terminal Done/Error. A final unterminated SSE data frame is dispatched on disconnect. SSE lines over 8 MiB are discarded (logged once); `Last-Event-ID` advances from the discarded event's envelope id so reconnects do not replay it. Session-less events are not fanned out, except `backend.*` notices. Each turn emits exactly one terminal event: `session.error`/`session.execution.failed` and the timeout set Done with Error; `session.idle` and `session.execution.succeeded` dedupe to one Done. Completion message lookup is bounded to 5s and aborted on `Close`. Usage with null or all-zero tokens is ignored so it does not overwrite the last real usage. OpenCode's server event stream is volatile; replay depends on server support, so events produced during disconnection cannot be guaranteed recoverable. `CloseSession` cancels its subscribers and timeout timer; `Cancel` and `CloseSession` treat HTTP 404 as success (session already closed) and are safe to call repeatedly; the stream stops when no session has subscribers.

`AgentTask.TimeoutSec` optionally bounds one turn. At expiry, Relo calls `/interrupt` and emits `session.error` with timeout detail; zero keeps caller-managed timeout behavior. Each call to `SendTask` sends `Prompt` verbatim as the only prompt field and does not add a wrapper or extra parts. The model is set only when provider/model changes; the `relo-system` instruction entry is PUT only when its SHA-256 changes. Reuse sessions sequentially and consume one terminal event before the next `SendTask`; usage is read from assistant messages since the latest user message, not accumulated across the whole session.

On context refresh Relo aborts/closes the OpenCode session and starts a new one. The Relo agent id stays the same.

## Token overhead

The adapter always selects OpenCode's `build` agent and enforces read-only work through session permission deny rules (`write`, `edit`, `bash`). Session deny rules are appended after the agent's rules and OpenCode resolves permissions last-match-wins (inferred from the v2.0.27 binary, not live-verified), giving read-only enforcement without the `plan` persona. It does not add metadata, prompt wrappers, or tool definitions of its own. OpenCode contributes its own system prompt and a description of every tool it offers, in every call of every agent, so Relo denies the tools it never uses (`task`, `subagent`, `execute`, `todowrite`, `todoread`, `skill`, `question`, and `webfetch`/`websearch` except for debug and architect agents; `core/internal/runtime/runtime.go`). Measured against OpenCode v2.0.27 with a fake model endpoint (`RELO_E2E_OPENCODE=1 go test ./internal/opencode -run TestAgentRequestsCarryOnlyTheToolsTheyUse`): a probe's empty request fell from 35 KB to 6.7 KB and a worker's from 37.7 KB to 8.9 KB, mostly because OpenCode's "Code Mode" repeats the whole tool catalog in the system prompt, and its `subagent` tool alone was 2.7 KB. The provider may add fixed system/tool overhead and cache accounting; measure it per model/provider rather than infer it from prompt bytes. `client_live_test.go` probes `/api/session/{id}/context` and `/api/experimental/session/{id}/instructions/entries` and logs payload byte/token estimates where endpoints are exposed.

## Live smoke (2026-10-14)

Run: `RELO_LIVE_OPENCODE=1 go test -count=1 -v -run Live ./internal/opencode/` with `/usr/bin/opencode` v2.0.27.

| Check | Result |
|---|---|
| Enabled models | 142 |
| Default model / provider | No usable connected model returned by default lookup or integration connections |
| Session creation | Passed |
| Prompt/deltas/terminal usage | Skipped: no usable model; no inference credentials available to test |
| Fixed per-session input overhead | Not measured; first-prompt usage unavailable |
| Same-session/cross-session cache | Not measured; prompt portion skipped |
| Context/instruction probe | Live test logs probe response or exact endpoint error on next run with this server configuration |

No live token numbers are claimed from health/model discovery alone. The live harness runs at most three tiny prompts (same session twice, then fresh session once) when a default or connected model is available. It logs usage input/output/cacheRead/cacheWrite for each and reports first near-empty prompt `usage.input` as the fixed-overhead estimate; this includes the actual prompt and system entry, so treat as an upper bound, not isolated OpenCode overhead.

## Mock runtime

`relo.runtime = mock` (also used automatically if OpenCode is missing) implements same `AgentRuntime` interface in-process. No inference cost. The mock drives acceptance scenario and emits synthetic usage with increasing cache reads when calls reuse same system prompt.

## Permissions

OpenCode permission events are forwarded to VS Code. Relo policy (`relo.permissions.*`) may allow automatically; otherwise user must approve before Relo replies to OpenCode.
