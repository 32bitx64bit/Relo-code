# Token saving

Goal: same verified result, fewer tokens. Input tokens first, output second.
Everything here is measured by the cache ledger before it is trusted; vendor numbers are hypotheses.

## Where tokens go

| Bucket | Cached? | Who controls it | Lever |
|---|---|---|---|
| OpenCode fixed overhead (system prompt + tool defs) | yes, per provider TTL | OpenCode | measure only; avoid spawning agents for trivial work |
| L0 role prompt + style fragments | yes, shared per (role, profile) | Relo | keep fragments tiny, few variants |
| L1 session-stable block | yes | Relo | scope per role |
| L2 generation body | append-only | Relo | context profile per role x difficulty; send deltas only |
| L3 volatile tail | no | Relo | keep minimal |
| Tool results in session history | re-read every turn | worker behavior | read less: targeted reads, bounded output |
| Worker output (summary, narration) | n/a (output price) | Reply brevity + schema caps | terse replies, capped report fields |
| Worker report into Primary context | no | Relo | clamp + dedupe |
| Spawn tax (fixed overhead x agents) | partly | scheduler/Primary | batch trivial tasks, reuse sessions |
| What the context pack lists | no | Relo | one line of counts for excluded items, never their text; memory recall capped (4 entries, 600 characters); an item the pool holds is not added again |

Ranking of expected impact (verify with the harness): 1) context scoping and read-less, 2) not spawning
agents that do not pay for their fixed overhead, 3) report clamping, 4) Reply brevity and Minimal code fragments.
Reply brevity is the smallest lever: vendor data shows ~3% over "be concise" at the median for Terse, more for Telegraphic,
and agent sessions are mostly code and tool calls that brevity does not touch.

## Token saving profiles

Settings keep their `relo.efficiency.*` keys and stored values. Display names, once:

| Key | Name | Values (stored = shown) |
|---|---|---|
| `preset` | Saving level | `off` = Off, `auto` = Balanced, `max` = Aggressive |
| `voice` | Reply brevity | `off` = Normal, `lite` = Short, `full` = Terse, `ultra` = Telegraphic, `auto` = Auto |
| `craft` | Minimal code | `off` = Off, `lite` = Light, `full` = Strict, `ultra` = Strictest, `auto` = Auto |
| `planLean` | Fewer agents | boolean |
| `roles` | Per-role overrides | table |

Two independent axes (Reply brevity, Minimal code), each with four levels:

- **Reply brevity**: terse prose. Applies to report fields, narration between tool calls, findings.
  Never to code, paths, commands, error strings, JSON keys, or the user-facing `userMessage`.
- **Minimal code**: minimum-code discipline for roles that write code. It shapes the change, never the file: no markers or comments are asked for in the user's code, and shortcuts are named in the report's risks. Ladder (need it? stdlib? native?
  installed dep? one line? minimal), no unrequested abstractions, validation/security/accessibility never cut,
  one tiny test for non-trivial logic. Reviewer gets the **review lens** variant instead (what breaks, what to cut).

Primary never gets Minimal code: it does not write code and its output is a JSON turn.
Optional Fewer agents (off by default): one line telling Primary to merge or drop tasks that do not need an agent.

### Default matrix (Balanced, base for `medium` difficulty)

| Role | Reply brevity | Minimal code | context budget |
|---|---|---|---|
| primary | Normal | Off | 24k |
| head | Short | Off | 12k |
| probe | Telegraphic | Off | 6k, read-only, no memory, direct-dependency reports only |
| worker / debug / integration | Terse | Light | 16k; task, code, report; 2k max per item; dependency reports only |
| architect | Terse | Light | 16k |
| test | Telegraphic | Off | 8k |
| reviewer | Terse (review format) | review lens | 12k; task, finding, code (diff + acceptance + prior findings) |

Modifiers, applied in order:
1. difficulty `trivial|easy`: Reply brevity +1 notch (cap Telegraphic), budget x0.5 (Primary exempt; primary keeps its 24k).
2. difficulty `hard`: no change. `expert`: Reply brevity capped at Short, Minimal code capped at Light (do not squeeze reasoning).
3. attempt >= 2 (escalation or retry): Reply brevity -1 notch. Terseness may have hurt; model escalates too.
Aggressive also shrinks budgets x0.75 except Primary (primary keeps 24k). Primary is exempt from all budget scaling; caps still apply.

### Settings (`relo.efficiency`, shown as Token saving)

```json
{
  "preset": "off | auto | max",
  "voice": "auto | off | lite | full | ultra",
  "craft": "auto | off | lite | full | ultra",
  "roles": { "probe": { "voice": "ultra", "craft": "off" } },
  "planLean": false,
  "reportMaxChars": 1200
}
```

Off = nothing. Balanced = matrix above. Aggressive = matrix with Reply brevity +1 notch and budgets x0.75 (Primary exempt).
Explicit `voice`/`craft` (Reply brevity / Minimal code) override the matrix globally; `roles` (Per-role overrides) overrides per role. Unknown Saving level uses Balanced; unknown level uses Auto. Primary Minimal code remains Off. `baseBudget` caps the selected budget when > 0; 0 means the caller keeps its default. Off (or empty Saving level) returns empty style and the supplied base budget without changing behavior (Budget 0 means the caller keeps its default). Safety clause appears once in any non-empty fragment: kept in Reply brevity fragments and appended once when Reply brevity is Normal but Minimal code or review is on.
Recommended rollout: ship Off, flip to Balanced once the A/B harness shows verification pass-rate parity.

## Cache integration

- Style fragments live in `Static` (L0), appended after the role prompt in fixed order: Reply brevity, Minimal code/review.
  Byte-stable per (role, Reply brevity, Minimal code); at most roles x profiles distinct L0 variants, so agents with the same
  role and profile share a cache entry.
- A profile is fixed for an agent session. Changing it (settings change, attempt downshift) starts a new
  OpenCode session at a generation boundary. It is an intentional L0 miss; it never happens mid-generation.
- Fragments never contain counters, timestamps, ids, or the profile name.
- Fragment size ceilings are asserted in tests (Reply brevity <= 140, Minimal code <= 160, review <= 170, whole <= 330 tokens by the engine counter).
  Vendor prompts cost ~1.1k tokens; do not paste them. Write compact originals.

## Context profiles (input side)

`style.Resolve` also returns a context profile for `context.BuildRequest`: budget, allowed classes, per-item token cap,
memory, dependency-report restriction, and read-only flag. Primary includes all classes + memory; head 12k; probe 6k,
read-only, no memory, direct-dependency reports; worker/debug/integration use the table above; architect 16k; test 8k.
Reviewer uses the table above. Aggressive scales budget by 0.75 (except Primary); trivial/easy scale by 0.5 (except Primary). The supplied base budget is a hard cap when > 0; 0 means the caller keeps its default.
Replaces the flat `Settings.Budget` and the probe/test special case in `workerBudget`.

Delta dispatch: first prompt of a generation sends full `Dynamic`; later turns send only new L2 items + L3.
Refresh opens a new session with the full `Dynamic` block.

## Realistic cache hit rate

Hit = `cacheRead / (input + cacheRead + cacheWrite)`. A live 2 to 3 step run measured about 78%. The cached prefix is
about 9.4k tokens (roughly 90% is OpenCode's own system prompt and tool definitions, which Relo does not control)
and every provider call adds about 2k tokens of uncached tail. Expect:

| Run shape | Hit rate |
|---|---|
| short (2 to 3 provider calls) | 80 to 85% |
| long (many calls on one generation) | about 90% |
| 95% | needs a prefix near 40k tokens and few tool steps; do not promise it |

Levers that actually move the number: Primary has no tools (one call per turn), decisions never change Static
mid-generation, the Primary session is reused while generation and model are unchanged, and extra tool steps are
avoided. A refresh, an escalation or a model change is an intentional miss. Measure with the ledger; the
`prompt usage` debug log records input, cacheRead and output per call.

## Enforce, do not persuade

Models ignore style instructions sometimes. Relo clamps deterministically:
- report JSON schema gets `maxLength`/`maxItems` (summary, findings, risks, next); `ParseReport` clips and logs overflow.
- Primary receives the clamped, deduped report, never the raw transcript.
- verification output keeps failing test names + head/tail (target <= 2 KiB), full log stays on disk.
- worker prompt line: bounded reads (line ranges, `| tail`), no whole-file dumps.

## Measure

- `metrics.CacheStats` rows gain `role` and `profile` (`v:full|c:full`) labels.
- Ledger per task: uncached input, cacheRead, cacheWrite, output, attempts, verification pass/fail.
- Weighted input = input + 1.25 x cacheWrite + 0.1 x cacheRead (Anthropic default, configurable weights).
- A/B harness (live, env-gated like the OpenCode smoke): same fixture task under Off / Balanced / Aggressive (`off | auto | max`); print the
  ledger; fail if pass-rate drops. Same job, fewer tokens is the acceptance test.
- Auto-downshift: failed or retried under an aggressive profile, next attempt uses a lighter one (see modifiers).

## Do not

- Depend on an external style CLI/proxy (telemetry by default, account for routing) or install a style skill globally in
  OpenCode: it would hit Primary and bypass role scoping. Relo injects per-agent keyed instruction entries.
- Rewrite the user's prompt. Vendor research shows this degrades answers.
- Apply Reply brevity to code, commands, paths, error strings, or JSON keys.
- Drop security warnings or irreversible-action confirmations into terse form (auto-clarity rule is in every Reply brevity fragment).

## Implementation plan (after phase 1 lands)

| Slice | Files | Notes |
|---|---|---|
| style package | `core/internal/style/**` (new), embedded fragments, matrix, `Resolve`, `Fragment`, `ID`, token-ceiling tests | no deps beyond agent/context |
| prompt + context | `prompt.ComposePartsWith(opts)`, report schema caps, context profile in `BuildRequest` | extends A2 APIs additively |
| orchestrator | spawn resolves profile; `nextAttempt` returns (model, profile); delta dispatch | B1 hook already exists |
| host + extension | Token saving settings parse/payload (`efficiency`), command "Relo: Set Token Saving" (planned, not yet in `extension/package.json`), profile chip on agents | A4a/A5 surfaces |
| metrics + bench | ledger labels, weighted input, live A/B harness | needs A1 usage plumbing |

## Credits

Reply brevity and Minimal code fragments are original, compact rewrites of ideas from caveman (JuliusBrussee, Apache-2.0/MIT) and ponytail (DietrichGebert, MIT). No code or full prompt text is vendored. Credit in `NOTICE`.
