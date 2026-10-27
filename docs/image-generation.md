# Picture generation

Agents on art tasks can make pictures through an image API you pay for: write the prompt, attach reference art, wait for the pictures, look at them, change one thing and go again, and pass the one they pick to `relo_sprite` (see [sprites.md](sprites.md)) to turn it into a game-ready PNG. Nothing here is specific to one game. The agent works out the specification from the project and keeps it in a brief file.

Relo calls the API itself, from the core, with a provider and key you already gave it. There is no browser, no hand-off and no subscription to drive: you pay per picture, and Relo keeps the spend on a short lead.

## Which providers

| Provider kind | Pictures |
|---|---|
| `openai` | Yes: any endpoint that implements `POST {baseURL}/images/generations` and `/images/edits` and answers with base64 pictures (`data[].b64_json`). OpenAI's own does. A router or proxy works if it passes those paths through |
| `anthropic` | **No.** Anthropic's API only reads images. Its [vision documentation](https://platform.claude.com/docs/en/build-with-claude/vision) says Claude "cannot generate, produce, edit, manipulate, or create images". Choosing an Anthropic provider for pictures is an error that says so. Claude can still be the model that plans and judges the pictures; it just cannot draw them |

An endpoint that answers with links instead of picture data is refused (Relo does not fetch links). If your router does not expose the image paths, or turns your key into a subscription route, pictures will fail with a message that names the status and path.

## Turning it on

1. Add a provider with kind `openai`, its base URL (`https://api.openai.com/v1` for OpenAI) and your API key under **Relo: Providers**. This is the provider system the rest of Relo uses; the key is stored the same way and never written to disk by Relo or logged.
2. Turn on `relo.art.enabled` (Settings, Sprites). The Primary then marks image tasks `art`, those tasks go to a model that sees, and their agents get `relo_sprite` and `relo_art`.
3. If you have more than one OpenAI-style provider, set `relo.art.provider` to the one to use. Empty means the first.
4. Check the other settings below, most of all `relo.art.sessionBudgetUSD`.

Everything applies to a running backend; no restart.

## What it costs and how that is kept in check

OpenAI bills the image models by tokens: images read and written, and the prompt. At the time of writing, `gpt-image-2.5-flare` and `gpt-image-2.5-sunburst` are $8 per million image input tokens, $30 per million image output tokens and $5 per million text input tokens ([OpenAI pricing](https://developers.openai.com/api/docs/pricing)). OpenAI publishes no per-picture price for these two; for the earlier `gpt-image-2` it lists, at 1024x1024, about $0.006 at low quality, $0.053 at medium and $0.211 at high. Check the pricing page for your model.

Relo handles money in two steps:

- **Before the call** it works out a deliberately high **estimate** (the `gpt-image-2` per-picture figures, plus a margin for each reference and the prompt). A job that does not fit what is left of the session's budget is refused before it is created, with the numbers. With the budget off (below) nothing is refused for its price, but the estimate is still shown in the question and the spend is still counted.
- **After the call** it replaces the estimate with the **real cost**, computed from the `usage` the provider reports. If the provider reports none, the estimate is kept and the job says so (`costFromEstimate`).

Rules around that:

- A call the provider **refuses** (bad key, unsupported option, content policy) is not billed, and counts as zero.
- A call that was **cut off** (the connection broke, or you cancelled it while it ran) may still have been billed, so it keeps counting at its estimate, and the message tells the agent and you to check the provider's usage page.
- A job you **decline**, or cancel before it starts, counts as zero.
- A **session cap** (`relo.art.sessionJobLimit`, default 25) bounds the jobs one session starts.
- **Approval:** with `relo.art.approval` = `session` (default) Relo asks before the first job of a session and you can answer **Always** for the rest of it; `each` asks every time. The question shows the prompt, the model, quality, size, number of pictures, the most the job can cost and what the session has spent. Yolo mode skips the question but **never** the budget or the job cap you set. If you set neither, Yolo with `Always` lets the agents spend with nothing in the way, so use that pair on purpose.
- The two limits are separate: turning one off leaves the other in force. With a limit off, agents are told so (`budgetUnlimited`, `jobsUnlimited` in `status`, with no figure for it) and the playbook still tells them to spend with care.
- The budget is per session. The project total is shown to agents too, so they can see how much has gone into the folder overall.

The numbers are not a billing system. Your provider's usage page is the record. The prices in the table inside Relo (`core/internal/art/cost.go`) are copied from OpenAI's pricing page and need re-checking when models change.

## Settings

| Setting | Default | Meaning |
|---|---|---|
| `relo.art.enabled` | off | Switches on `relo_sprite` and `relo_art` |
| `relo.art.provider` | empty | ID of the Relo provider to use; empty is the first `openai` one |
| `relo.art.model` | `gpt-image-2.5-flare` | Image model. `gpt-image-2.5-sunburst` is the one OpenAI recommends for editing precision, which matters when you give references |
| `relo.art.quality` | `medium` | `low`, `medium`, `high` or `auto`; the agent may override it for a job |
| `relo.art.size` | `1024x1024` | `1024x1024`, `1536x1024`, `1024x1536` or `auto` |
| `relo.art.candidates` | 2 | Pictures per job (1 to 4). Each one is billed |
| `relo.art.sessionBudgetUSD` | 10 | The most the agents may spend in a session, in US dollars. **0 means no limit.** Anything up to 100000 |
| `relo.art.sessionJobLimit` | 25 | Jobs one session may start. **0 means no limit.** Anything up to 100000 |
| `relo.art.approval` | `session` | `session` or `each` |

## What an agent does

`relo_art` is a hub tool next to `relo_sprite`. OpenCode agents see `relo_art`, Claude Code agents `mcp__relo__art`. It is offered under the same rules as `relo_sprite`: a builder role, on a task flagged `art`, on a model that can see, in build mode, never the Primary. A task with no `art` flag does not get it.

| Action | Does |
|---|---|
| `status` | Is it set up (and if not, why not), the model, the defaults, the budget left. It never shows the endpoint or key |
| `playbook` | The guide: `overview`, `prompts`, `refs`, `sprites`, `views`, `iterate`, `cost`. On demand, so it is not in every prompt |
| `refs` | Store reference pictures from workspace files or from earlier jobs' pictures; returns an id for each |
| `generate` | Start a job: `prompt`, optional `refs` (ids, in the order the prompt may call them image 1, image 2, ...), `quality`, `size`, `background` (`auto`, `transparent`, `opaque`) and `count`. Returns at once |
| `wait` | Block up to 45 seconds on a job. When it is ready it returns the pictures as one numbered sheet, with the real cost |
| `cancel`, `jobs` | Stop a job, list this session's jobs and the budget |

A job without references is a plain generation (`/images/generations`). With references it is an edit (`/images/edits`): the model starts from the pictures and the prompt says what to do with them. That is how a set of items is kept in one style, and how another view of a finished item is made.

Jobs are asynchronous because a picture can take a minute or two and the hub's calls time out after one. The agent calls `generate`, then `wait` until the job is `ready`, `failed` or `cancelled`. A job belongs to the session, not to the agent's key, so another turn can wait on it. Refining a picture is a new job with the old one as a reference.

Pictures land in `.relo/art/raw/<job>/<n>.png` in the project (git-excluded with the rest of `.relo`), not in the agent's workspace, so a handful of large pictures per job never bloat the repository. The agent passes the path of the one it picks to `relo_sprite`, which reads it, and writes the finished sprite into its own workspace, where it shows up as a proposed change. You can open the raw pictures in the editor at any time.

### Transparency

OpenAI's image models can return a real alpha channel (`background: "transparent"` with PNG output), so the agent asks for it first. Whether a given model or provider accepts it is not something Relo can promise (it was not confirmed for the 2.5 models when this was written): if the call is refused over the background, the playbook has the agent generate on a flat key colour and remove it with `relo_sprite` matte. Either way it checks the alpha it actually got.

### Provenance

When `relo_sprite` writes a sprite from a generated picture, it also writes `art/provenance/<name>.json` in the agent's workspace: the job, picture number, prompt, model, quality, size, references, the picture's hash and the sprite files made. It arrives as a proposed change with the sprite, one file per sprite so two agents never edit the same file. It is a convenience for you, for example if a store asks you to disclose AI-generated art. Whether and how to disclose is your decision.

## Your key

The key lives where all Relo provider keys live: in the editor's secret storage, sent to the core in memory with the provider list. The core reads it when a job runs and puts it in the `Authorization` header of the one request to the provider's own host (a redirect to another host is refused). It is not written to the job files, the event log or the audit rows, it is not in any tool result, and anything shaped like a key is scrubbed from the provider's error messages before an agent sees them.

Prompts and reference pictures go to your provider. Do not put anything private in them. Review your provider's data policy.

## Where things are

| Where | What |
|---|---|
| `<project>/.relo/art/jobs/<id>/job.json` | The record of each job (survives a restart) |
| `.relo/art/raw/<id>/` | The pictures of each job |
| `.relo/art/refs/` | Stored reference pictures (metadata stripped, size capped, stored once by hash) |

After a restart, a job that was waiting for your approval asks again. A job that was running is marked failed, because the call may or may not have been billed; it keeps counting at its estimate.

## Not verified

- **No call to the real API has been made.** The tests run against a local stand-in that follows OpenAI's documented request and response shapes. `RELO_E2E_IMAGES=1` runs one cheap real call (see below); until someone does, expect to adjust.
- Whether the 2.5 models accept `background: "transparent"`, how many references `/images/edits` takes (Relo allows 6), and the real per-picture cost of the 2.5 models (OpenAI publishes tokens, not pictures).
- Whether OpenCode's MCP call timeout is longer than the 45 seconds a `wait` may block (the default `wait` is 25).
- The model names and prices were read from OpenAI's documentation on 2026-10-26.

## Tests

- `go test ./internal/art ./internal/agentmsg ./internal/orchestrator ./internal/app`: the request and response shapes against a fake API (generation, edits with image parts, errors, redirects, cut-off connections), cost and estimate arithmetic, the manager's lifecycle (approval, budget, job cap, cancel, restart), the hub tool's validation and exposure rules, and that the key never reaches a job file, a status, an event or an error.
- `RELO_E2E_IMAGES=1 RELO_E2E_IMAGES_KEY=sk-... go test ./internal/art -run TestLiveImages -v` makes one 1024x1024 **low quality** picture and one edit of it against the real endpoint (`RELO_E2E_IMAGES_BASE_URL`, default `https://api.openai.com/v1`; `RELO_E2E_IMAGES_MODEL`) and prints the reported usage and cost, in all about a cent or two. It never runs by default.
