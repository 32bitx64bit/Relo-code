# Context usage versus 9router

## Verified records (2026-10-16 UTC)

Databases were opened read-only:

- `/home/gavin/.9router/db/data.sqlite`, table `usageHistory`.
- `/home/gavin/Desktop/rimmodding/Project-RimFactory-Revived/.relo/relo.db`, tables `usage`, `events`, and `agents`.

### Claude tool loop

9router row **20438**, `2026-10-16T14:43:05.334Z`, `claude-haiku-5-5`:

| Field | Tokens |
| --- | ---: |
| `promptTokens` | 138286 |
| `completionTokens` | 8034 |
| `tokens.cached_tokens` | 66457 |
| `tokens.cache_creation_input_tokens` | 2685 |

Relo event **2607**, `agent.completed`, timestamp **1792161785** (same second),
agent `agent_61e72dd483dd67581665`: `contextTokens=79178`, `window=200000`.
Relo usage row **1804** records that agent's turn totals:
`input=452914`, `output=16570`, `cacheRead=0`, `cacheWrite=0`.
9router's nine matching Haiku requests, rows 20429–20438 (excluding non-Haiku row 20432), sum to output **16570**.
Their `promptTokens - cached_tokens - cache_creation_input_tokens` sum is **434914**.
Relo's input is exactly **434914 + 9 × 2000 = 452914**. This confirms the tool-loop match, not just time proximity.

### Codex cross-check

9router row **18012**, `2026-10-16T09:45:14.424Z`, `gpt-6-luna`:
`promptTokens=208615`, `completionTokens=14324`, `cached_tokens=208384`.
Relo event **1200**, timestamp **1792143914**, agent `agent_01a3419b80d7a252d1f1`:
`contextTokens=224939`, `window=272000`; usage row **827** has turn totals
`input=319640`, `output=79809`, `cacheRead=15736320`.
The meter equals **208615 + 2000 + 14324 = 224939**, not the turn's cumulative input or cache totals.

## Root cause

Installed 9router code under
`/home/gavin/.nvm/versions/node/v24.16.0/lib/node_modules/9router/app/.next-cli-build/server/chunks/` explains the large discrepancy:

1. `8499.js`, module `29675`, Claude translator `P`, creates inclusive `prompt_tokens` alongside exclusive `input_tokens` and cache fields.
2. `5330.js`, module `73483`, export `_6`, normalizes history by adding cache fields to `prompt_tokens` again.
3. The same module's `O9` adds **2000** input tokens to the client response.
4. Its `WL` OpenAI formatter drops native cache fields without translating them into `prompt_tokens_details`.
5. `8895.js` applies `WL(O9(C.usage), targetFormat)` at the terminal streaming chunk.

Executing those installed, read-only functions reproduces row 20438 exactly:

| Stage | Input/prompt | Cache read | Cache write | Output |
| --- | ---: | ---: | ---: | ---: |
| Native usage inferred through translator replay | 2 | 66457 | 2685 | 8034 |
| Translator inclusive prompt | 69144 | 66457 | 2685 | 8034 |
| `usageHistory` normalization | 138286 | 66457 | 2685 | 8034 |
| Client OpenAI wire | 71144 | absent | absent | 8034 |
| Old Relo context | 79178 | — | — | included |
| Corrected Relo context from available wire usage | 71144 | — | — | excluded |

Native input **2** is reconstructed, not retained raw upstream telemetry. The replay, nine-request totals, and independent Codex match agree.
The **59108** history-versus-meter gap is **69142 - 2000 - 8034**.
Removing output alone cannot reconcile 9router history with its client response.
Relo cannot recover discarded fields or identify arbitrary provider-added padding from normalized usage alone.

No request reaches 400000 `promptTokens` during the supplied Relo database's recorded run.
The later near-500k row **20558**, `15:54:30.365Z`, has `promptTokens=497788`, model `claude-sonnet-5-5`.
It cannot be matched to this run: its last usage row **2046** is at `14:53:31Z`.

## Relo correction and verification

- `runtime.Usage.PromptTokens()` uses **Input + CacheRead + CacheWrite**, never output or reasoning.
- OpenCode step events and terminal last-request context use that formula; billing remains cumulative.
- Terminal totals lacking last-request context preserve the latest measured tool step instead of replacing it with a turn sum.
- Compaction uses each step's prompt count, ignoring running usage totals.
- Existing `Settings.Window` is correct: min(global cap, configured model window), with unknown-window behavior unchanged.
- Tests cover cache reads/writes, output exclusion, multiple tool steps, terminal totals, and threshold boundaries for global/model windows.
- A real installed OpenCode process against a local fake provider verifies step and terminal prompt counts, without paid requests.

## Skipped work and risks

9router databases, installation, settings, and historical Relo records remain untouched.
Exact equality with 9router's history remains blocked by its double-counting, padding, and discarded response cache fields.
Fix 9router's normalization/formatter separately, then capture a new matched request before declaring end-to-end parity.
The original near-500k request needs its corresponding Relo database/session to verify attribution.
