# Vision

Relo knows which models can see images. Agents on those models can open image files with `relo_image`, so they can check a UI against a reference or a game screenshot instead of trusting the code. Tasks that need eyes are routed to a model that has them.

Before this, Relo had no notion of image input. OpenCode assumes every custom model takes images, so a text-only model that read a PNG either failed the turn or got pixels it could not use. Agents were told to judge for themselves whether their model could see images.

## Which models see

Each model in the catalog carries `vision` (`yes`, `no`, or absent for unknown) and `visionSource`, which says where the answer came from. Sources are checked in this order:

| Source | Where it comes from |
|---|---|
| `configured` | `vision: true/false` on the model in `relo.providers`. You set it in the provider form (Images: Auto, Sees images, Text only), or discovery fills it when the endpoint reports it. |
| `catalog` | OpenCode's built-in catalog (models.dev data), for built-in providers and for a custom model with the same id as a built-in one. OpenCode's answer for a custom model is ignored, because OpenCode makes up `["text","image"]` for any custom model that declares nothing. |
| `name` | A guess from the model id (`vision.Guess`). Claude 3 and later, GPT-4o, GPT-4.1, GPT-4 Turbo, GPT-5 and later, o1, o3, o4, Gemini, GLM-*V, Pixtral, LLaVA and InternVL are guessed to see. So are ids with a `vl`, `vision` or `omni` segment. DeepSeek (except `-vl` and `-ocr`), gpt-oss, GPT-3.5, Codestral, Qwen coder models, Claude 2, Claude Instant, o1-mini and o3-mini are guessed text-only. Everything else stays unknown, including MiniMax, MiMo, Kimi and plain GLM. |

Discovery reads whatever the endpoint reports:

- Anthropic: `capabilities.image_input.supported`.
- OpenRouter: `architecture.input_modalities` or `architecture.modality`.
- Other endpoints: `input_modalities`, `modalities.input`, `supports_vision`, `supports_image_input`, or a `capabilities` field that names `vision` or `image_input`.

Most OpenAI-compatible proxies report none of these, so the name guess and the override do most of the work. A guess can be wrong, especially for new model families and for proxies that strip images on the way through. The settings page marks guesses ("guessed from the name") so you know which ones to confirm.

### What OpenCode is told

`provider.BuildConfig` writes `modalities` for every custom model whose answer is configured or guessed:

- A model that sees gets `{"input":["text","image"],"output":["text"]}`. This is what OpenCode assumes anyway.
- A text-only model gets `{"input":["text"],"output":["text"]}`. OpenCode then replaces any image in that model's context, whether from `read` or from an MCP tool, with `ERROR: Cannot read image (this model does not support image input). Inform the user.` The provider never receives `image_url` parts it would reject.

Unknown models get no `modalities`, so OpenCode's default (images allowed) still applies to them.

Changing `vision` in the provider form changes the config fingerprint, so the backend restarts like it does for any provider edit.

The model pickers show `images` or `text only` next to each model, and the line under a picked model says where the answer came from.

## relo_image

`relo_image` is a tool on Relo's loopback MCP hub, next to `relo_agents` and `relo_canvas`. It is general purpose: any PNG, JPEG, GIF, WebP or BMP file in the agent's workspace, not just Canvas renders.

```json
{"key": "<agent key>", "paths": ["shots/market.png", "ref/market-reference.png"], "region": {"x": 0, "y": 0, "width": 800, "height": 600}, "maxEdge": 1568}
```

| Field | Meaning |
|---|---|
| `paths` | 1 to 4 files, shown in the given order. To compare a result with a reference, pass both in one call. Relative paths resolve against the agent's workspace, then the session workspace. Absolute paths must be inside one of those. |
| `region` | Optional crop in source pixels, applied to every path. Use it to zoom into detail the downscale hides. |
| `maxEdge` | The longest edge sent, from 256 to 2576. The default is 1568, which every vision provider accepts without resizing on its side. |

The result is one text block followed by one image block per file, in order. For each requested path, the text gives `index` (its position among the image blocks), `path`, `format`, `source` and `sent` sizes, `crop`, and `scale` (source pixels per sent pixel). With the scale, the agent can turn a point it sees into a crop of the source. It also gives `approxTokens`, a rough cost of width × height / 750 (Anthropic's formula). A path that could not be shown gets an `error` instead, and the other paths are still shown. The call fails only when none could be.

How a file is prepared:

- Opened through `os.Root`, so a symlink cannot lead outside the workspace. Files larger than 40 MiB or 36 megapixels are refused before decoding.
- Cropped, then scaled with Catmull-Rom so the longest edge fits `maxEdge`. Downscaled text stays readable instead of aliasing.
- A PNG or JPEG that needs no crop or scaling and is small enough is sent byte for byte. Anything else is encoded as PNG. When the PNG would exceed 3.5 MiB, it is flattened onto white and encoded as JPEG at quality 90, then 75. If it is still too large, it is shrunk by a fifth at a time until it fits. The metadata always gives the size that was actually sent.
- An animated GIF is shown as its first frame, and the metadata says so.
- SVG is refused with a pointer to Canvas `capture`: it is source code, not pixels.

Limits: 4 images and 12 MiB per call, 30 calls a minute per agent, 2 images being prepared at once across the session, and a 30 second deadline.

### Who gets it

| Agent's model | Tool | Told |
|---|---|---|
| Sees images | offered | View screenshots and references with `relo_image`, and compare the result with the reference before reporting a visual task done. |
| Unknown | offered | Same, plus: Relo does not know whether this model reads images, so if none arrives, say visual verification was unavailable. |
| Text only | not offered | This model cannot see images. Do not claim a visual match; report that visual verification was unavailable and what a person or vision model should check. |

The Primary never gets it. The Primary plans from curated context and does not read files. The handler checks the role and the model as well, so a text-only model that calls the tool anyway gets a tool error, not pixels.

### Getting a screenshot to look at

`relo_image` shows files. It does not take screenshots. Where the pixels come from depends on the target:

- Canvas artifacts and approved local web previews: `relo_canvas capture`.
- Native apps and games: whatever the project provides. For example, a `--screen <name>` flag that renders one screen to a PNG, or a headless test harness. The agent writes the file inside its workspace and then views it.
- References from the user: put the image in the workspace and name the path in the request.

## Routing tasks that need eyes

A Primary task can set `vision: true` when its agent must look at images: match a screenshot or reference design, or check rendered UI. Relo picks the model for the task's difficulty as usual. If that model is not known to see images, Relo walks the routing ladder: first up to the nearest model that sees, then down. If no model in the ladder sees, the task keeps the model its difficulty picked, and the agent is told it cannot see.

Escalation on retry follows the same rule: a vision task skips text-only models on the way up. If the current model is the only one that sees, the task stays on it rather than escalating to a stronger model that cannot see the reference.

The review task Relo adds after a vision task is a vision task too, so the reviewer can check the result against the reference.

"Sees" here means `vision: yes`, from any source. An unknown model does not count, so a model known to see is preferred over one that might.

Because the Primary model is part of the ladder, a vision task goes to the Primary's model when that is the only one that sees. This costs more, and it is the right call when the alternative is a model that cannot see the reference.

## Code

| Piece | Where |
|---|---|
| Name guess, image preparation, path resolution | `core/internal/vision` |
| Discovery fields, `vision` on `provider.Model`, OpenCode `modalities` | `core/internal/provider` |
| Catalog `capabilities.input` | `core/internal/opencode/client.go` (`Models`) |
| Config, catalog and name resolution | `core/internal/app/models.go` (`reconcile`) |
| MCP tool contract and validation | `core/internal/agentmsg/image.go` |
| Key check, role and model gate, limits | `core/internal/orchestrator/images.go` |
| Offering the tool and the prompt line | `core/internal/orchestrator/worker.go` |
| Ladder walk | `core/internal/routing/router.go` (`Seeing`, `EscalateSeeing`) |
| Badge and override | `webview/src/main.js`, `extension/src/providers.ts` |

Tests: `go test ./internal/vision ./internal/provider ./internal/agentmsg ./internal/orchestrator ./internal/routing` cover the pieces. `RELO_E2E_OPENCODE=1 go test ./internal/opencode -run 'TestReloImage|TestTextOnly|CanvasCapture'` checks the real OpenCode: pixels from `relo_image` and from a Canvas capture reach the model as an image part, and a model declared text-only gets OpenCode's note instead.

## Not done

- Nothing compares images automatically. The agent looks and judges. A pixel diff against a reference would give a number, but not whether the layout is right.
- No tool takes screenshots of native apps. Each project has to provide its own.
- The Primary cannot look at a reference image itself. Workers and debug agents can.
