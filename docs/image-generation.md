# Picture generation

Agents on art tasks can make pictures through an image API you pay for: write the prompt, attach reference art, wait for the pictures, look at them, change one thing and go again, and pass the one they pick to `relo_sprite` (see [sprites.md](sprites.md)) to turn it into a game-ready PNG. Nothing here is specific to one game. The agent works out the specification from the project and keeps it in a brief file.

Relo calls the API itself, from the core, with a provider and key you already gave it. There is no browser, no hand-off and no subscription to drive: you pay per picture, and Relo keeps the spend on a short lead.

## Which providers

| Provider kind | Pictures |
|---|---|
| `openai` | Yes: any endpoint that implements `POST {baseURL}/images/generations` and `/images/edits` and answers with base64 pictures (`data[].b64_json`). OpenAI's own does. A router or proxy works if it passes those paths through |
| `openrouter` | Yes: every job (text-only or with references) is one or more `POST {baseURL}/images` calls, with references as ordered `input_references` image_url data URLs and pictures read from `data[].b64_json`. The reply's `media_type` decides the picture: PNG, JPEG and WebP are accepted, JPEG and WebP are converted to PNG, and any other type fails the job with the type named. `GET {baseURL}/images/models` is checked first (cached for 10 minutes): a listed model that lacks `input_references`, or an explicit `size`/`quality`/`background`/`aspect_ratio`/`resolution`/`seed` it does not support, is refused before anything is spent; settings the user left at their defaults are dropped when unsupported. A failed model list or an unlisted model lets the call through. Each job's pictures cost what `usage.cost` reports in total, or the estimate when the provider reports none |
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
| `status` | Is it set up (and if not, why not), the model, the defaults, the budget left, and the model's supported parameters when the provider publishes them (`supported_parameters`; `unknown` otherwise). It never shows the endpoint or key |
| `playbook` | The guide: `overview`, `prompts`, `refs`, `sprites`, `views`, `game-dev uses`, `iterate`, `cost`, `jobs`. On demand, so it is not in every prompt |
| `refs` | Store reference pictures from workspace files or from earlier jobs' pictures; returns an id for each |
| `generate` | Start a job: `prompt`, optional `refs` (ids, in the order the prompt may call them image 1, image 2, ...), `quality`, `size`, `background` (`auto`, `transparent`, `opaque`), `count`, and, for OpenRouter, `params` with `aspect_ratio`, `resolution` and `seed`. Returns at once |
| `wait` | Block up to 45 seconds on a job. When it is ready it returns the pictures as one numbered sheet, with the real cost |
| `cancel`, `jobs` | Stop a job, list this session's jobs and the budget |

A job without references is a plain generation and one with references is an edit: on an OpenAI-style provider that is `/images/generations` and `/images/edits`; an OpenRouter provider uses its one image endpoint for both (see the provider table). With references the model starts from the pictures, and the prompt says what to do with them. That is how a set of items is kept in one style, and how another view of a finished item is made.

### Parameters that depend on the model

`aspect_ratio` (`N:N`), `resolution` (1 to 16 ASCII letters or digits) and `seed` (a nonnegative integer) are optional OpenRouter parameters. Relo validates only their shape, so the accepted values come from the model, not from Relo. Before a job it reads the provider's published `supported_parameters` for the chosen model (cached for 10 minutes). For a listed model it refuses the job before sending a picture request when the model's list does not name a parameter the agent set; `size`, `quality` and `background` that the user left at their defaults are dropped instead when they are not listed. When the model is unlisted, or the model list cannot be fetched, nothing is checked in advance: the parameter is sent and the provider decides, so the job fails loudly if it rejects it. A provider kind that does not accept these parameters refuses the job before it is created.

`status` reports these capabilities as `supported_parameters`: the chosen model's list when the provider publishes it, or `unknown`. Reference support and how many references a model takes vary the same way; Relo allows up to 6.

### Game-dev uses

The playbook's `game-dev uses` topic covers the common assets: sprites and sheets (one object per picture, or a drawn grid the agent slices with `relo_sprite`), tilesets (a grid sliced with `cols`/`rows`, checked at the final pixel size), seamless textures, backgrounds and skyboxes, and UI pieces such as panels, buttons and frames. Existing art, a finished sprite or a screenshot of the running game can be stored with `refs` and used with a role (`style`, `subject`, `composition`) to edit it or make variations of it; a reference guides style, subject, proportions and palette and is not a copy, so text and exact layouts still need checking.

### Picture formats

The answer's `media_type` decides what can be stored: PNG is used as is, JPEG and WebP are converted to PNG for the job and the sheet, and any other media type, an SVG for example, fails the job with the type named in the error. Relo asks for PNG output where the model lists that parameter. Like reference counts, the format a model actually returns varies by model.

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
- The values each OpenRouter model accepts for `aspect_ratio` and `resolution`, and whether it accepts `seed`, are known only from the provider's published `supported_parameters`; Relo checks shape, not the model's accepted range.
- Which media types a real model returns: the tests cover PNG, JPEG, WebP and the rejection of an SVG against local stand-ins, and an unexpected type fails the job rather than being converted.
- Whether OpenCode's MCP call timeout is longer than the 45 seconds a `wait` may block (the default `wait` is 25).
- The model names and prices were read from OpenAI's documentation on 2026-10-26.

## Tests

- `go test ./internal/art ./internal/agentmsg ./internal/orchestrator ./internal/app`: the request and response shapes against a fake API (generation, edits with image parts, errors, redirects, cut-off connections), cost and estimate arithmetic, the manager's lifecycle (approval, budget, job cap, cancel, restart), the hub tool's validation and exposure rules, and that the key never reaches a job file, a status, an event or an error.
- `RELO_E2E_IMAGES=1 RELO_E2E_IMAGES_KEY=sk-... go test ./internal/art -run TestLiveImages -v` makes one 1024x1024 **low quality** picture and one edit of it against the real endpoint (`RELO_E2E_IMAGES_BASE_URL`, default `https://api.openai.com/v1`; `RELO_E2E_IMAGES_MODEL`) and prints the reported usage and cost, in all about a cent or two. It never runs by default.
