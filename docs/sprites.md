# Sprites

Relo can turn a generated picture into a game-ready sprite without leaving the editor. Agents on art tasks get `relo_sprite`, a local pipeline that removes a flat background, trims, fits, outlines, slices and checks a picture, then writes the PNG into their workspace, where it shows up among the proposed changes.

This page is the local half: nothing in it talks to any service. The other half, where agents produce the pictures through an image API you pay for (`relo_art`, the budget and the approval), is in [image-generation.md](image-generation.md).

Nothing here is specific to one game. The agent works out the specification (sizes, view angle, naming, style) from the project itself and keeps it in a short brief file in the workspace, so the next task starts from it.

## Turning it on

`relo.art.enabled` (Settings, Sprites) is off by default. When it is on:

- The Primary's task schema gains an `art` field. The Primary sets it on tasks that make image assets. Without the setting the schema is unchanged and the field is not offered.
- An art task is a vision task: `art` implies `vision`, so it is routed to a model that can see (see [vision.md](vision.md)). The review task made for it inherits the `vision` flag, but not `art`: reviewers look at the result with `relo_image` and do not get `relo_sprite`.
- A worker on an art task gets `relo_sprite`, with a paragraph of guidance in its prompt.

The setting applies to a running backend. Changing it closes the loopback hub so its tool catalog is rebuilt on the next turn.

## relo_sprite

`relo_sprite` is a tool on Relo's loopback MCP hub, next to `relo_agents`, `relo_image` and `relo_canvas`. OpenCode agents see it as `relo_sprite`, Claude Code agents as `mcp__relo__sprite`.

```json
{
  "key": "<agent key>",
  "input": {"path": "art/raw/iron.png"},
  "ops": [
    {"op": "matte"},
    {"op": "fit", "size": [128, 128], "margin": 4, "anchor": "bottom"},
    {"op": "outline"},
    {"op": "check", "rules": {"width": 128, "height": 128, "alpha": true}, "strict": true}
  ],
  "output": "Textures/Iron.png",
  "preview": true
}
```

| Field | Meaning |
|---|---|
| `input.path` | The picture to start from: a generator's output, a reference, a sprite already made. Relative paths resolve against the agent's workspace, then the session workspace. Absolute paths must be inside one of those. |
| `ops` | Up to 24 steps, run in order. Empty means inspect: report the size, how much is solid, and the bounds. |
| `output` | PNG to write in the agent's workspace. Needs `{n}` when the pipeline ends with several pictures. |
| `overwrite` | Replace `output` if it exists. Off by default. |
| `preview` | Also return the result as a contact sheet picture, so the agent can look at it. |

The result is one text block (every step, every check, the size and bounds of each picture, and what was written with its SHA-256) followed by the preview picture if one was asked for or the pipeline ended in `sheet`.

### Steps

| Op | Does |
|---|---|
| `inspect` | Size, share of solid pixels, bounds. |
| `matte` | Background to transparency. `mode: key` (default) reads the background colour from the picture's border, or takes `color`, and flood-fills inward from the edge within `tolerance` (default 48). `holes` also removes enclosed regions such as the middle of a ring. `shrink` and `feather` pull the edge in and soften it, and the edge colour is repainted from the sprite's interior, so no trace of the background colour is left in the soft edge. `mode: mask` takes a picture that is white where the sprite is. |
| `trim` | Crop to the visible part, with optional `padding`. |
| `fit` | Scale the visible part into `size` with a `margin` and an `anchor`. Only shrinks unless `upscale`. `common` gives every picture the smallest scale any needs, so relative sizes survive a slice. |
| `resize` | By `size` (one side may be 0), `scale` or `maxEdge`, with `lanczos` (default), `catmullrom`, `area`, `bilinear` or `nearest`, and optional `sharpen`. |
| `outline` | A 1 to 6 px outline in a colour. It never grows the picture, so leave room with `margin` in `fit`. |
| `mirror`, `rotate` | Derive other facings. |
| `slice` | One picture into many, by grid (`cols`, `rows`) or by connected pieces (`gap`, `minArea`). |
| `sheet` | A contact sheet on a checker, colour or tiled picture, at several magnifications (default 1 and 3), with labels. This is how an agent judges how a sprite reads at game scale. |
| `check` | Tests rules and reports each one: exact or maximum size, `alpha`, `margin`, how much is solid, encoded size, a file name pattern, and leftover background colour (`maxKeyResidue`). With `strict`, a failure stops the run and nothing is written. |

Resampling works on premultiplied colour, so a transparent edge never leaves a dark or light halo. The PNG encoder is deterministic and writes no metadata: the same input and steps give the same bytes.

### Where it writes

Writes go into the agent's own workspace through `os.Root`, so a symlink cannot lead out of it. With a virtual workspace the file arrives as a normal proposed change, to be reviewed and applied like any other. The name must be a `.png` inside the workspace and may not touch `.git` or `.relo`. A set is validated as a whole first: a bad name, a missing `{n}` or an existing file refuses the whole call, so a half set is never left behind. Each file is written beside its destination and renamed into place.

### Who gets it

| Caller | Result |
|---|---|
| Builder role, art task, model that sees or is unknown, Build mode | offered |
| The Primary | never |
| A task that is not an art task | refused |
| A text-only model | refused: it could not check its own work |
| Reviewer, probe and other read-only roles | not offered |
| Plan mode | not offered |
| The feature switched off | not offered |

The tool policy, the prompt and the handler each apply these rules, so a model that calls the tool without being offered it gets a tool error.

Limits: 20 calls a minute per agent, 24 steps and 64 pictures per call, inputs up to 16 megapixels, sides up to 4096 px after any step, 8 MiB per written file, 2 pictures being decoded at once across the session, and a 30 second deadline. A 16 megapixel input can take several hundred MB while it is resampled.

## Pictures from an image API

An agent on an art task also gets `relo_art`, which makes the pictures through an OpenAI-compatible image API that you pay for per picture. A picture it makes is a file in `.relo/art/raw/<job>/<n>.png`; pass that path to `relo_sprite` as `input.path`. When the pipeline starts from such a picture and writes a sprite, `relo_sprite` also writes `art/provenance/<name>.json` beside it (job, prompt, model, references, hashes), which arrives as a proposed change with the sprite. See [image-generation.md](image-generation.md).

You can also use `relo_sprite` on any picture it can read, such as one you put in the workspace yourself.
