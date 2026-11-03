# How the Primary stays grounded without reading code

The Primary architects and delegates. It has no repository tools and never reads code: code in its context costs cache
writes every turn and pulls it into the workers' job. Its blindness is real, though (it once told a user a file was
"still in your working tree" for hours after it was gone), so facts reach it three other ways, each bounded.

1. **Measured by Relo.** `workspaceFacts` (git state, checks, whether a named file exists and where a copy survives)
   and the *Repository map* (`orchestrator/repomap.go`): areas with file counts, no code, in the cached stable block.
2. **Asked.** `relo_agents` with `action: "ask"` and the question in `text` (Primary only, 4 fresh asks a turn). A
   reader agent (a probe on the user's folder, read-only) answers in at most 1400 characters with `path:line`
   citations. Relo opens every cited file and tells the Primary which citations it found and which it did not
   (`orchestrator/ask.go`). Repeats are cached by question and tree; a slow reader is collected by asking again; the
   latest answers stay in the Primary's context so a rebuild does not repeat them.
3. **Checked by the worker.** Whatever a task builds on that the Primary did not measure goes in the task's `assumes`.
   The worker's brief says to verify each before building, and to report any that are wrong in `deviations` (what is
   true, with file:line). A wrong fact reaches the Primary at once as a `deviation` message, which wakes it. A reader
   that was wrong is caught here, before anything is built on it.

## Cost

In a Pixels at War run (123 Primary generations) cache writes were about 77% of the Primary's cost: nearly every turn
wrote its whole ~26k-token context again, and 100 of the 123 turns were woken by worker messages (progress dumps,
"Acknowledged", "is a build running?").

- Messages carry a `kind` (`ask`, `blocked`, `deviation`, `fyi`). Only a message that asks something wakes its
  recipient; a question mark counts as an ask when no kind is given. A finished worker is no longer woken (a model turn)
  to be told something. Progress, results and acknowledgements belong in the report.
- A builder's clean finish reaches the Primary as a status line (320 characters), not up to 1200. Failures, risks,
  wrong plan facts, questions and no-change finishes keep the whole summary. Probes and reviewers are never collapsed.
- Each Primary turn emits `primary.cost` (input, output, cacheRead, cacheWrite, `units` weighing a cache write 1.25,
  a read 0.1, an output token 5). Compare against the session's earlier turns before judging any change.

## One conversation, not one per turn

The Primary used to start a new conversation every turn: its session was closed afterwards and the whole curated
context was sent again as one message, so the provider wrote all of it to cache each time (`orchestrator/convo.go`,
`runAgentWith` in `worker.go`).

Now one conversation continues. The first message carries the whole context (stable items, then the dynamic body); each
later one carries only the context blocks that changed since the last, a list of the items that are gone, the workspace
facts and constraints (always whole, and stated to outrank anything said earlier), the ask and any peer messages. The
system prompt is the role prompt and output contract only, so it never changes under a running process. Blocks are the
rendered items, split at markdown headings outside code fences; a block with the same heading and text is not sent again.

The conversation is started over, with the whole context, when continuing costs more or cannot work:

- the role prompt, output contract or model changed (the process keeps the prompt it started with);
- it ran 60 turns, or grew past twice a fresh context plus 6000 tokens;
- its cache went cold (over 4 minutes idle) and it is bigger than a fresh context, since a cold cache is rewritten whole
  either way;
- the Primary asked for `refreshSelf`, a turn failed or was cancelled, or a send fails on a resumed session (the idle
  process died: the turn is retried once with a fresh conversation, so nothing is lost);
- a send fails on a fresh one (the session is dropped so the next turn gets a working one).

An idle process is closed after 10 minutes. Every other agent keeps its one-generation session.

Unverified: the saving has not been measured in a real run. It rests on the Claude CLI reusing its cache across user
messages in one stream-json process, and on a roughly five-minute cache lifetime (`primaryWarm`). Compare `primary.cost`
(cacheWrite against cacheRead) before and after.

## Not done

- The reader uses the probe route for `medium` tasks. If that is a weak model, its answers are only as good as its
  citations; the workers' check is the backstop.
