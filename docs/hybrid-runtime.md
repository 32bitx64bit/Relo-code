# Hybrid runtime

Status: design agreed. The decisions at the end are settled: official Go SDKs for the model clients, hybrid as the only mode (no OpenCode-only setting), OpenCode stays a required dependency.

Built so far: `runtime.Mux` (`core/internal/runtime/mux.go`) with one routed backend, Claude Code ([claude-code.md](claude-code.md)), behind `relo.claudeCode.enabled`. It differs from the plan below in three ways. It routes by the model's provider (`SessionConfig.Provider`), not by a turn's capabilities. OpenCode's session ids keep their form and only the routed backend's carry a prefix (`claude_…`). And `ReplyPermission` is now a named interface (`runtime.PermissionReplier`), which the OpenCode adapter implements too. The native runtime is not built.

Relo keeps OpenCode where it earns its place and adds a native runtime where OpenCode is in the way. Both sit behind the `AgentRuntime` interface that already exists, behind a small multiplexer, so the orchestrator, context engine, integration engine and UI do not learn that there are two. `relo.runtime` keeps its two values: `opencode` means the real runtime stack (which from now on is the hybrid), `mock` is the in-process stand-in.

## What decides who does what

| Need | OpenCode | Native | Owner now → target |
|---|---|---|---|
| A turn with no tools that must return structured JSON (Primary) | Has no per-prompt `system`, `tools` or `format`; adds about 6.7 KB of its own prompt to an empty request; schema is requested in prose | Owns the request body, can use provider JSON modes, byte-stable prefix, cache markers | OpenCode → **native** |
| Read-only investigation: `read`, `list`, `glob`, `grep`, peer messaging | Works, but reads the real filesystem under a session directory, so an overlay-backed agent needs a directory first | Reads through `WorkspaceView` or a plain folder, no directory | OpenCode → **native** |
| `edit`, `write` | Mature matching, tuned for model mistakes | Must be built and measured; through `WorkspaceView` it needs no directory | **OpenCode** → native only if it measures as well |
| Shell (`bash`) | Process control, timeouts, PTY, per-OS behavior | Large to build well, and needs a real directory in any runtime | **OpenCode**, indefinitely |
| LSP diagnostics fed back after edits | Yes | No | **OpenCode** |
| Web fetch and search | Yes | Not planned | **OpenCode** |
| Summarizing history in place | Yes (`Compactor`) | Not at first; a turn that overflows ends in a context refresh | OpenCode; native gets its own before it edits (Phase 4) |
| Provider access | Whatever Relo configures | The same | Both |
| Permission prompts | Events to Relo's policy | The same events and the same policy | Both |

"Both" for providers is deliberate. Relo's providers are user-defined OpenAI-compatible and Anthropic-compatible endpoints (`provider.KindOpenAI`, `provider.KindAnthropic`, a base URL and a key), and OpenCode's own catalog is switched off by `enabled_providers`. A native runtime that speaks those two protocols loses no model Relo can reach today. (`RELO_SHARE_OPENCODE=1` is a development switch that drops isolation, so OpenCode sees the user's own setup and its full catalog again; native does not cover that.)

## Facts in the code that shape the design

- **Most turns are already read-only.** `StartSession` denies `edit`, `write`, `bash` and `shell` for any session with `ReadOnly` set, and the orchestrator sets it for every non-mutating role (Primary, Head, Probe, Reviewer) and for every role outside build mode. So the effective tools of those turns are `read`, `grep`, `glob`, `list` and the two Relo MCP tools. Setting `bash: true` in `DefaultTools` for a non-mutating role has no effect on them.
- **The Primary has every tool off** (`ToolsFor`), reasons over a curated context, and returns JSON. It is the most expensive model in a run and needs the least from a runtime.
- **There is one runtime.** `Coord.Runtime` is set once (`SetRuntime`) and `runAgent` uses it for everything. Optional abilities are found by type assertion: `Compactor`, `MessageRuntime` and `PermissionReplier` (in `askPerm`). Sessions are cached per agent in `runtimeSessions` and replaced when the agent's context generation or model changes.
- **Agents already restart their runtime session at a generation boundary.** Relo's context engine rebuilds the prompt per generation, and on refresh it closes the runtime session and starts a new one. A backend that is fixed for a generation therefore needs no history to move between backends.
- **A runtime session does not have to survive a restart.** `restoreSession` marks agents that were running, starting or waiting as `Interrupted`. A native session can live in memory.
- **Only `Compactor` runtimes summarize in place.** `watchContext` compacts mid-turn only when the runtime implements `Compactor`. Any runtime that reports a context-limit error (`isContextLimit`) gets the other path: the turn is aborted with that error and `MaybeRefresh` starts a new generation. That is the whole story for a runtime without `Compactor`, which matters below.
- **The event contract is already runtime-neutral.** `runtime.Event` carries phase, tool start and finish, usage, `ContextTokens`, `MoreSteps`, interruption, compaction progress and permission requests. The OpenCode adapter normalizes into it; the mock already implements it in-process.

## Architecture

```text
                    Coord (orchestrator)            one runtime.AgentRuntime
                           |
                      runtime.Mux ── picks a backend per agent generation
                      /          \
   opencode adapter (existing)    native runtime (new)
     opencode serve child           in-process
     own file tools, bash, LSP      llm clients ── openai-go / anthropic-sdk-go
     session dir = real folder      tools over an FS interface
                                     │
                              ┌──────┴────────┐
                       dirFS(folder)    WorkspaceView (worktree or virtual)
```

### The multiplexer

`runtime.Mux` implements `AgentRuntime` and forwards the optional interfaces to whichever backend owns the session.

- **Session ids** are prefixed by backend (`oc:…`, `nv:…`). Every later call (`SendTask`, `Events`, `Cancel`, `CloseSession`, `Compact`, `DeliverMessage`, `ReplyPermission`) is routed by the prefix. `Compact` on a backend that lacks it reports that, and the context watchdog already skips a runtime that is not a `Compactor`.
- **`StartSession` chooses.** `SessionConfig` has only `Title`, `Directory`, `Parent` and `ReadOnly` today; `Role` and `Tools` live on `AgentTask`, which arrives after the session exists. `SessionConfig` gains both. `Tools` is the map `runtime.ToolsFor(role, mode, mutating)` produces, which depends only on role, mode and mutability, so the orchestrator can compute it before the session starts.
- **`Models` and `Providers`** are the union of both backends, de-duplicated by `provider/model`, each entry tagged with the backend that can serve it. Native lists the models of Relo's configured providers with their declared context, output and reasoning efforts; the existing `reconcile` keeps working.
- **`Health`** reports each backend. `App.startRuntime` is where the Mux is assembled, around the OpenCode process it already starts. OpenCode stays required, so a missing or failing OpenCode keeps today's behavior: the mock is swapped in for everything and the status says why.

### Selecting a backend

A turn needs a set of capabilities: the tools that are effectively enabled (`tools[name]` and not denied by `ReadOnly`), structured output, and whether it must reach a workspace through a directory. The native backend declares what it can serve, in code, and that declaration is what grows phase by phase. The selector takes native when it covers the whole set and the model's provider is one Relo configured (an OpenAI-compatible or Anthropic endpoint), and OpenCode otherwise. If native cannot start the session (a provider kind it does not speak, a model it cannot list), the session starts on OpenCode instead. That fallback happens at `StartSession` only, never in the middle of a turn.

There is no user-facing setting that turns native off. Until Phase 1 ships the native backend declares nothing, so every turn goes to OpenCode and the default behaves as today. A native capability is declared only after its phase gate (below) has passed. For the A/B runs and for bisecting a regression, an environment variable (`RELO_NATIVE=off`) forces every turn to OpenCode; it is a diagnostic switch, not a documented setting.

Two rules keep this simple. The backend is **fixed for an agent's generation**: it is chosen when the session starts and changes only when the generation changes. And an agent's workspace is **settled before it changes hands**: if an OpenCode generation ran in a materialized directory, the directory is imported and removed (`Settle`) before a native generation reads or writes the agent's view.

Routing once Phases 1 and 2 have shipped, from the facts above:

| Role / mode | Effective tools | Backend |
|---|---|---|
| Primary | none, JSON out | native |
| Head, Probe, Reviewer | `read`, `list`, `glob`, `grep`, `relo_agents` | native |
| Any role in plan or review mode | read-only set | native |
| Worker, Test, Debug, Architect, Integration in build mode | `edit`, `write`, `bash`, plus web for Architect and Debug | OpenCode |

## The native runtime

Packages: `internal/native` (sessions and the loop), `internal/native/llm` (model clients), `internal/native/tools`.

### Model clients

One small interface, `Stream(ctx, Request) → chunks`, with two implementations built on the providers' official Go SDKs: `github.com/openai/openai-go/v3` for OpenAI-compatible chat completions and `github.com/anthropics/anthropic-sdk-go` for Anthropic Messages. Both resolve on the module proxy today. They would be the first direct dependencies in `core/go.mod`, which now holds only SQLite and its helpers; both are pure Go, so the Linux, macOS and Windows builds stay as they are, and the exact versions are pinned when Phase 1 starts.

The SDKs carry streaming, request building and typed responses; Relo's code carries what they do not: tool-call assembly across chunks, cancellation, retry with backoff (surfaced as `Phase: "retry"` and `Attempt`), per-request timeout, reasoning effort mapped from the model's advertised efforts, and usage mapped into `runtime.Usage` including `Context = input + cacheRead + cacheWrite`. API keys come from Relo's provider settings, are never logged, and are scrubbed from errors the way `provider.Discover` already does.

The catch is that "OpenAI-compatible" is a family, and the OpenAI SDK is written for OpenAI. A third-party endpoint can send a field the types do not know (a reasoning field, say), a partial tool argument, or an odd finish reason. The client uses the SDK's base-URL option and reads unknown fields from the raw JSON it keeps, and the `Stream` interface hides the SDK from the loop, so a quirk is fixed in one place or the client replaced without touching anything else. Each quirk gets a recorded-fixture test.

Owning the request body brings a few things OpenCode cannot offer: the system prompt is `AgentTask.System` verbatim, the history is the generation's messages, cache markers go on the stable prefix that `prompt.Parts.Static` and `Dynamic` already separate, and the usage Relo records is what the provider reported for that call.

### The loop

A turn is a sequence of steps. A step is one model call; if it ends in tool calls the tools run (independent calls concurrently, each with a timeout and a capped result) and another step follows. `Event.ContextTokens` and `MoreSteps` are set on the event that ends each step, exactly as the OpenCode adapter does, so `watchContext` and the context chip need no change. A turn ends when a step ends in text, on cancel, on error, or at a step limit. The turn's text is parsed for a JSON object the same way the OpenCode adapter does it (that helper moves to the `runtime` package so both share it).

History lives in memory for the session. There is no in-place summarizer at first. Without `Compactor`, a turn that outgrows the window fails with a context-limit error and the agent restarts in a new generation (see the facts above); it does not summarize and carry on. That is acceptable for the short, read-only turns of Phases 1 and 2, and the first thing to re-measure if a read-only turn is ever long. It is not acceptable for long edit turns, so a native `Compactor` is a precondition of Phase 4, not a later option. The loop keeps the last step's prompt size in `ContextTokens`, so `watchContext` already sees how full the window is.

### Tools and where they read

Tools use the names `ToolsFor` already produces (`read`, `list`, `glob`, `grep`, later `write` and `edit`) so the orchestrator's tool policy is unchanged. Descriptions are short; the prompt overhead per call is a design target, measured against OpenCode in the same fake-endpoint test.

They read through one interface, `FS` (`Read`, `Stat`, `List`, `Walk`). Two implementations:

- `dirFS(root)` for a plain folder: the user's workspace for probes and reviewers that stay on the main workspace, or any directory. It applies the same rules as the workspace layer: canonical paths, `.git` and `.relo` protected, no symlink followed out of the root.
- A `WorkspaceView` adapter for an isolated agent, whether its files are a worktree or a virtual workspace. It needs no directory.

`relo_agents` and `relo_canvas` run in-process. They call the handlers the loopback MCP hub already serves, so there is no HTTP hop or per-server secret, the tool names stay the same, and a peer message is delivered by appending to the session at the next step boundary (`DeliverMessage`). The agent's private key flow in `bindPeer` is unchanged.

### Permissions

A tool that needs approval emits the same permission `Event` the OpenCode adapter emits and waits for `ReplyPermission`. `askPerm` and `permission.Policy` run as before. Native read-only tools need no approval inside the workspace; anything outside it asks.

## How this connects to virtual workspaces

[Virtual workspaces](virtual-workspaces.md) stop at one wall with OpenCode: its tools cannot be routed through a view, so `relo.workspace.virtualReads` is gated off. A native session can honor it. The workspace manager's routed flag becomes per agent (set when the session's backend is native and every enabled tool is native), so:

- With `virtualReads` on and a native backend, a read-only or edit-capable agent on a virtual workspace never gets a directory. With it off, native tools read whatever directory the agent has, as OpenCode would.
- When a later phase adds native `write` and `edit`, they go through `WorkspaceView.WriteFile`, guarded by the workspace revision the model last read, so two writers cannot silently overwrite each other. An agent that also needs a shell stays on OpenCode and gets a directory, with the settle-before-handoff rule above.

The Git-only option from that discussion (create a worktree lazily, commit the agent's branch and remove the worktree when it finishes, read proposals from the commit) is independent of this and works with either backend.

## Settings and rollout

- No new setting. `relo.runtime` stays `opencode` (the hybrid stack) or `mock`; `mock` has no native backend.
- Hybrid is the default from Phase 0 because, until native declares a capability, it sends everything to OpenCode. What changes behavior is a phase's capability being declared, and that happens only when the phase's gate has passed.
- With no opt-out for users, the gates carry the safety: a phase that fails its A/B does not ship its capability. The way back from a native regression is `RELO_NATIVE=off` while diagnosing and a release that stops declaring the capability.
- Nothing about stored sessions, changes or workspaces depends on which backend ran a turn, so switching a capability on or off between runs is safe.

## Testing and evidence

- **Conformance suite.** One set of scenarios, run against the mock, the OpenCode adapter (its tests already put an `httptest` server in place of OpenCode's API) and the native runtime against a fake model server: a plain turn, a tool turn, a multi-step turn (`MoreSteps`, `ContextTokens`), cancel, timeout, retry, JSON extraction from fenced and bare objects, permission ask and reply, usage accounting, peer-message delivery. A new backend is correct when the same scenarios produce the same event shapes.
- **Request-size comparison.** `TestAgentRequestsCarryOnlyTheToolsTheyUse` already runs the real OpenCode against a fake model endpoint and checks what goes out (it needs the `opencode` binary and `RELO_E2E_OPENCODE=1`, and `RELO_E2E_DUMP` writes each role's request body). The native runtime gets the same test against the same fake endpoint. Native must be smaller for the toolless and read-only cases or the phase is not worth shipping.
- **A/B on real models**, gated by an environment variable like the other live tests: a fixed set of tasks per phase run once with `RELO_NATIVE=off` and once with the phase's capability declared, comparing tokens, cost, time and whether the result is acceptable. Relo's own counters undercount, so cost is read from the provider-side numbers the native runtime records and from the fake-endpoint log, not from the old counters.
- **Workspace tests** for the native FS adapters reuse the path-safety and symlink cases from the workspace and `vws` suites.

## Phases

Each phase is shippable on its own. Its exit criteria are the gate for declaring its capability in the native backend; until they pass, the capability is built but not declared and the turns keep going to OpenCode.

| Phase | Adds | Exit criteria |
|---|---|---|
| 0. Seam | `SessionConfig.Role` and `Tools`, `runtime.Mux` in `App.startRuntime`, the `RELO_NATIVE=off` switch, shared JSON helper, conformance suite for mock and OpenCode. Native declares nothing. | No behavior change; the whole Go suite and the conformance suite pass. |
| 1. Toolless | Model clients on the official SDKs, the loop, event and usage parity, structured output. Native declares "no tools, JSON out": the Primary moves over. | Conformance passes on native; request size for the Primary turn is smaller than OpenCode's; A/B on a fixed plan-and-answer task set shows no loss. |
| 2. Read-only | `FS` interface and adapters, `read`, `list`, `glob`, `grep`, in-process `relo_agents` and `relo_canvas`, permissions. Head, Probe, Reviewer on native. | A/B on investigation tasks: findings accepted at the same rate, fewer tokens. A read-only turn that outgrows the window ends in a refresh, not a hang (test with a fake endpoint that returns a context-limit error). |
| 3. Routed reads | Per-agent routed flag; `virtualReads` honored for native sessions. | Read-only agents on virtual workspaces run with no directory; settle-before-handoff test. |
| 4. Native edits | A native `Compactor`, then `write` and `edit` through the view with revision guards, for roles that do not need a shell. | Edit success rate on a fixed task set matches OpenCode's, and a long edit turn compacts in place. If either fails, stop here; OpenCode keeps all edits. |
| 5. Optional | Native `bash`, web tools. | Only if a measured need remains. |

Phases 0 to 2 are the commitment. Phase 4 is where the real risk is, and the plan does not depend on it.

## Risks

- **Tool-calling quirks across OpenAI-compatible endpoints.** Some return partial tool arguments, wrong finish reasons or non-standard reasoning fields, and the OpenAI SDK is written for OpenAI. Mitigation: a recorded-fixtures test per quirk, a strict assembler that fails a step loudly instead of guessing, the SDK hidden behind `Stream`, and the start-of-session fallback to OpenCode for providers native cannot serve.
- **No user-facing opt-out.** Removing the OpenCode-only setting means a bad native capability reaches everyone at once. That is why each capability is declared only after an A/B on real models, and why `RELO_NATIVE=off` exists for diagnosis.
- **Edit reliability.** The reason Phase 4 is gated. A strict unique-match edit with a clear error is the starting point; whether it is good enough is a measurement, not an assumption.
- **Two paths to maintain.** The conformance suite is what keeps them equal; a divergence is a failing test.
- **Cross-platform.** The core builds for Linux, macOS and Windows. Everything in Phases 0 to 4 is file and network work; shell stays with OpenCode, which is where the OS-specific code is.
- **Secrets.** Keys live in Relo's settings, as they do now, and the native runtime holds them in memory for requests only. Requests and tool results are logged without credentials, and the transcript store is opt-in.

## Decisions

1. **Model clients use the official Go SDKs** (`openai-go/v3`, `anthropic-sdk-go`), behind the `Stream` interface.
2. **Hybrid is the default and the only mode.** The OpenCode-only setting is not built, and neither is a native-only one. The default behaves as today until a phase's gate passes and its capability is declared. `RELO_NATIVE=off` is a diagnostic switch for A/B runs, not a setting.
3. **OpenCode stays a required dependency.** The native runtime replaces the toolless and read-only turns and, if Phase 4 passes, plain edits. Shell, LSP feedback and web tools stay with OpenCode, and Phase 5 is optional. The installer, quickstart and the mock fallback do not change.
4. **Native transcripts** go to the existing event log with request bodies off by default and a development switch to turn them on.
