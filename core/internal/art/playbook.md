## overview
You are making pictures with an image model the user pays for, through the user's own provider account, a few cents to a few tens of cents a picture. The user sets the limits for the session (a budget, a number of jobs; either can be off) and may be asked to approve each job, so every job is a decision: say what you expect to learn from it before you start it.

Order of work:
1. Read the project for the specification (sizes, view angle, style, naming) and keep it in a short brief file the next task can start from. Ask the user only for what the project cannot tell you.
2. Put the pictures you want matched or changed into the reference store (action `refs`).
3. Write the prompt and call `generate`. It returns at once, or fails at once with the reason when the model cannot do what was asked. Call `wait` (up to 45 seconds a time) until the job is `ready`, or has failed or been cancelled. A picture can take a minute or two, and the user may have to approve the spend first: keep waiting, or finish other work and come back.
4. Look at the contact sheet that `wait` returns: a light checker, so transparent parts of a picture show as the checker, not as the picture's own colour. Judge the pictures against the brief, not against each other.
5. Take the best picture through `relo_sprite` (matte, fit, check).
6. Stop when the sprite passes the brief's checks. Report what was made and what it cost.

Pictures are made by a third party from your prompt and references. Do not put anything private in either.

Topics: `prompts`, `refs`, `sprites`, `views`, `game-dev uses`, `iterate`, `cost`, `jobs`.

A picture can be made one of two ways:
- A text-to-image job: `generate` with no `refs`. The model draws only from the prompt.
- A reference-guided job: `generate` with one or more stored references in `refs`. The model reads those pictures as well as the prompt, and they guide it; they are not pasted into the result. Relo fails the job rather than dropping the references. If a job with references fails, stop and report it to the user: do not retry it as text-only, because that makes a different picture (see `refs`).

Relo checks a model's published capabilities before it spends, where the provider publishes them (which parameters it accepts, and whether it takes references), and refuses the job before sending a picture request when the model cannot do what was asked. Where the provider publishes no capabilities for the model, nothing is checked in advance; an unsupported request fails at the provider, and the job fails loudly.

Which way to use one:
- Use text-to-image for the first picture of a new item whose look the prompt can describe, and for anything the project's art gives no example of.
- Use a reference-guided job to match a look: the project's existing art for style, a finished picture for another view, state or a single change. References carry the style, subject, proportions and palette better than words; the prompt still says what to draw with them.
- A mixed set never matches: keep one way (usually reference-guided from the project's art) for a whole set of items.

## prompts
Say what the object is, then its material, then its view and the style, in plain sentences: "A single iron ingot, hammered dark metal with a worn edge, three-quarter top-down view, centred, whole object in frame, painted game-icon style." The model follows detailed instructions well, so be specific rather than adding adjectives.

- The prompt is plain text only. Parameter switches such as `--ar`, `--sref`, `--cref`, `--stylize`, `--chaos` and `--raw`, and values such as `stylize: 100` or `chaos 20`, are refused before anything is spent: they are text the model would read, not settings. Pass settings as `quality`, `size`, `background`, `count` and `params`, and references in `refs` with a role.
- `params` holds `aspect_ratio`, `resolution` and `seed`. All three are model-dependent. Relo asks the provider which parameters the chosen model publishes (`status` shows `supported_parameters`, or `unknown` when none are published), and sends what it was told to send:
  - model listed and the list names the parameter: it is sent;
  - model listed and the list does not name a parameter you set: the job is refused before anything is spent; set only parameters the list names, or change model if the user allows it;
  - model unlisted, or the list cannot be fetched: the parameter is sent anyway, and the provider decides; the job fails loudly if it rejects it.
  The same holds for `size`, `quality` and `background`; the defaults the user left in place are dropped when the list does not name them, instead of failing the job; a value equal to the user's default counts as left in place. A provider kind that has no such parameters refuses the job before it is created. The values are checked for shape only (an `N:N` ratio, a short resolution token, a nonnegative seed), not for what the model accepts: use values the model documents.
- One object per picture, centred, with room around it. A cropped object cannot be fixed afterwards.
- Describe the style the same way every time. Keep the description in the brief and reuse it word for word, so a set of items matches.
- Say what to leave out in the same sentence ("no shadow, no floor, no text, no border"), not in a separate list.
- Do not ask for pixel sizes in the prompt. Pick `size` instead, ask for the look at large size, and let `relo_sprite` shrink it.
- Change one thing at a time between jobs, so you know what caused a difference.
- Send the same prompt at most twice per session; a third identical prompt is refused. If two rounds do not move toward the brief, change the prompt in a way the brief explains or stop and report.

## refs
Add a picture with `refs` (a project path, or a picture from an earlier job); it returns an id. List that id in `generate`'s `refs`, either bare or as `{id, role, weight}`:
- `role` says what the reference is for: `style` copies the look, palette and line weight; `subject` is the thing to draw (at most one subject per job); `composition` copies the layout and framing. Leave role out and it is a plain reference.
- `weight`, 0 to 1, says how strongly the role is followed; leave it out for the provider's default. A weight of 0 means no weight is sent, not zero strength.
- Relo states each role in the prompt for you: references are numbered in the order you list them, and the prompt gains a line such as "References: image 1: style reference (weight 0.5); image 2: composition reference." You may still call them "image 1", "image 2" in your own sentences.

With any reference the model starts from your pictures, so the prompt says what to do with them. Ways to use them:
- Match the project's existing art: attach two or three of its best existing pictures with role `style` and write "Match the style, palette, line weight and shading of images 1 to 3. Draw: ..." This is the most reliable way to stay consistent with a game's look.
- Another view or state of something you already made: attach the finished picture with role `subject` and write what changes ("the same object seen from the side", "the same object, damaged and rusted"), and what must not change ("keep the colours and proportions").
- Refine a candidate: attach it and describe the single change.

Fewer references work better than many, and a clean reference on a plain background beats a clever prompt. Each reference adds to the price of the job. With a reference the provider edits or restyles the pictures rather than drawing from text alone.

How many references a model takes, and whether it takes any, varies by model: some take none, some a few. Where the provider publishes what the chosen model supports, Relo checks it before it spends, and a model that cannot be used with the references on the job is refused before anything is spent. Where it publishes nothing for that model, the request goes out and the job fails at the provider if the model cannot take the references. Lower the number of references, or pick another model, only when the user has told you a different model is allowed; otherwise stop and report. Do not assume a reference count that worked for one model works for the next.

If a job with references fails, the provider refused the references or could not send them, and the job fails loudly. Do not carry on: stop and report the failure to the user. Never run it again without the references, and never retry the same prompt as text-only to get past it. A text-only picture would not be what the brief asked for. Do not go looking for another way to send the references yourself.

## sprites
What makes a generated picture usable as a sprite:
- `background`: ask for `transparent` first. The model then writes a real alpha channel, and the result needs no keying. Not every model or provider accepts it: if the call fails over the background, generate with `opaque` and ask in the prompt for a flat solid `#ff00ff` background, then key it out with `relo_sprite` matte. `#ff00ff` is not a colour sprites use, so the key removes cleanly.
- One centred whole object, nothing else. The prompt states each of these plainly:
  - one object, centred, whole object inside the frame with a margin around it;
  - flat solid `#ff00ff` background, one flat colour with no gradient;
  - no text, no letters, no numbers, no watermark, no border;
  - no symbols, icons or UI overlaid on the picture;
  - no other objects, no props, no duplicates of the object;
  - no shadow, no floor, no reflection, no glow and no contact shadow.
  A model can add a stray object or a caption that no keying removes; a picture that breaks these is not worth matting.
- Always check what you got: run `relo_sprite` inspect on the picture and look at the alpha, because a model can answer `transparent` with a faint background or a soft halo.
- Soft shadows, glow and reflections spoil both transparency and keying: leave them out of the prompt.
- Pixel art or a very small final size has to survive the shrink: prefer bold shapes and few colours, and judge the result on a contact sheet at the real size, not at 1024.
- The model draws at its own size. Use `size` for the shape (1024x1024 for most items, a wide or tall size only if the object is) and `relo_sprite` fit and resize for the final pixels.

### from a generated sheet
A picture can hold several frames when a model draws a grid of poses or directions. It is not sprites until it is processed, and that is `relo_sprite`'s job, not another paid job's:
- One picture asked for with `count` comes back as that many separate pictures, not a sheet: take each one through the pipeline on its own.
- A picture the model drew as a grid of frames is one picture: `slice` cuts it, `mode: grid` with `cols` and `rows` for a regular grid; `mode: components` (the default) for frames spaced apart on a plain background, which keeps only pieces at least `minArea` pixels.
- `fit` then gives every frame the same `size` with `margin` and `anchor`, so each sprite sits at the same place in its frame; `common` keeps relative sizes across the slice, and `trim` fits the ink, not the empty border. A model rarely centres every frame the same way; fit is what makes frames line up.
- Check alpha: end the pipeline with `check` and `rules` of width, height and `alpha: true`, `strict: true` so the run writes nothing when a frame is the wrong size or still opaque. Put `matte` before it when the background is not already transparent.
- Judge the frames together: `sheet` shows them on the light checker at 1x and 3x; a faint background or a halo shows at once. Then compare with the brief's frame size and origin.
- A reference guides style, subject, proportions and palette; it is not a copy of the picture. A sheet is no exception: the frames are not a copy of a reference, so check the proportions of each frame against the brief rather than trusting the model to repeat them.

## views
When the brief calls for several views of one item (front, side, back, damaged):
1. Make the base view first and get it right.
2. Store it as a reference and ask for each other view from it: "The same object as image 1, seen from the left side. Keep the exact colours, materials, proportions and style." A finished sprite, not the raw keyed picture, makes the best reference.
3. Compare the views side by side on one contact sheet (`relo_sprite` sheet) before you accept them; the checker shows a transparent edge at once.
`relo_sprite` mirror and rotate can derive a view for free when the style allows it; use them before you spend on another picture.

## iterate
Budget the rounds before you start: two or three jobs per sprite is usually enough.
- Explore at `quality` low and `count` 2 to 4, to find a direction cheaply. Finish at medium or high with `count` 1 or 2.
- Do not regenerate a picture that is nearly right: fix the last bit with `relo_sprite` (trim, fit, outline) or one edit job with the picture as a reference and a single change in the prompt.
- The same prompt with the same references is accepted twice per session and refused the third time; use a different prompt rather than repeating it.
- When two rounds in a row do not move toward the brief, stop. Report what you tried and what the pictures lack, instead of spending the rest of the budget.
- If a job with references fails, stop and report it to the user. Do not retry it without the references.
- If the user has asked for a set, finish the first item completely before you start the next, so the others can use it as the style reference.

## game-dev uses
The same two ways cover most game art: text-to-image for the first picture of something the prompt can describe, reference-guided for anything that must match art the game already has or change a picture you made.
- Sprites and sheets: items, characters, enemies, effects; one object per picture, or a grid of frames the model draws, then `relo_sprite` slice, fit and check (see `sprites`).
- Tilesets (and tiles for terrain and maps): ask for a grid of tiles on a plain background (`count` gives separate pictures, a drawn grid is one picture), then `slice` with `mode: grid` and `cols`/`rows`. Tiles must line up: check every tile at the final pixel size on a `sheet`, and say in the prompt that tiles are seamless and share one edge design.
- Textures: tiling materials (stone, wood, grass, metal). Say "seamless tileable texture, even lighting, no shadow, no vignette, seen straight on"; a reference of an existing texture as `style` keeps a set consistent.
- Backgrounds, parallax layers and skyboxes: wide pictures with `aspect_ratio` or a wide `size`. Say what is foreground, what is midground and what is far, and keep the playable area clear of busy detail. A drawn panorama is not a seamless loop; check the left and right edges meet if the game tiles it.
- UI assets: panels, buttons, frames, bars. Ask for one clean shape, a flat background, no text and no numbers (the game draws text), and state the state where it matters ("a gold frame for a selected slot"). Consistency across states comes from one style reference and one prompt changed one way at a time.
- Edits and variations of existing art: attach the existing art or a screenshot of the running game in `refs`, with role `subject` for the exact thing to keep, `style` for its look, and change one thing in the prompt ("the same panel, damaged", "the same character, winter coat"). Ask for a small variant set with `count` when you want to choose.
- A screenshot is a reference like any other: tell the model what to take from it ("match the palette and UI weight of image 1, draw: ..."), not to redraw it. A reference guides style, subject, proportions and palette; it is not a copy of the picture, and text or exact layout in it will not be copied faithfully.

## jobs
Every job is one model, one prompt and the references listed for it, and it comes out in one of two ways:
- text-to-image: no references in `refs`. Only the prompt steers the picture, so it must carry the whole look.
- reference-guided: one or more references in `refs`, each with a role. The model reads your pictures and lets them steer what it draws; the prompt then says what to keep and what to change.
- Reference support and the limit vary by model: one may take no references, another one, another several, and the cost changes with the model and the references. Where the provider publishes a model's capabilities, Relo checks them before it spends and refuses an unsupported job before sending a picture request; where it publishes nothing for the model, the job goes out and fails at the provider. When the job is refused or fails, do not lower the number on your own judgement: stop and report which model, how many references and what the error said. Never drop the references to get a picture.
- `aspect_ratio`, `resolution` and `seed` are model-dependent (see `prompts`). `status` lists the chosen model's `supported_parameters`, or `unknown` when the provider publishes none. A listed model is refused before anything is spent when the list does not name a parameter you set; an unlisted model, or a list that could not be fetched, goes out with the parameter and the provider decides, so the job fails loudly if it rejects one. Parameters left at the user's defaults are dropped when the list does not name them.
- The picture format varies by model. PNG, JPEG and WebP are accepted, and every picture is stored as PNG; any other media type, such as an SVG, fails the job with the type named in the error. Do not ask for a file format in the prompt.
- A job is not retried out of hand. A rate limit or a timeout stops the job with that error: stop and report it to the user, with what the job said, and do not start another job for the same picture to get round it. An unsupported request fails at the provider where it publishes no capabilities for the model.
- An HTTP 400 from the provider means it refused this prompt or these references. Where the provider gave a reason, the error names it in fixed words (its safety filter refused the request, a reference picture could not be read, the prompt is too long) with the provider's error code. Do not reword the prompt and send it again, and do not start another job for the same picture: stop and report the error as it says it. The user decides what changes.
- A job's cost says whether it is real or estimated: the job reports that its cost is estimated when the provider did not say what the call was billed, so the figure is Relo's deliberately high estimate. Say so when you report the spend.
- A reference guides style, subject, proportions and palette; it is not a copy of the picture. Do not rely on it to copy text, exact layout, or a frame count the model cannot count. Describe those in the prompt, then check the result.
- If a job with references fails, stop and report it (see `refs`). Do not retry it text-only.

## cost
Pictures are billed by the provider, to the user's account, by the amount of image data the model reads and writes. The price depends on `quality`, `size`, `count` and the number of references. Before a job runs, Relo works out a deliberately high estimate and refuses the job if it does not fit what is left of the session's budget (when the user has set one); once the job finishes, the real cost, from the usage the provider reports, replaces the estimate. `status` and every job show both, and a job says when its cost is still the estimate (`costFromEstimate`).

- The numbers in `status` are the user's limits: the budget per session, the jobs per session, the defaults for quality, size and count. Stay inside them. `budgetUnlimited` or `jobsUnlimited` true means the user turned that limit off, because a large set of pictures needs room: it does not mean the pictures are free. Spend the same way you would with a limit, and keep to the rounds the brief allows. Raising `quality` or `count` for one job is allowed, but only when the picture is worth it.
- Low quality costs a few cents or less, medium several times that, high several times more again. A reference adds a little to each job.
- A failed job costs nothing when the provider refused it. One that was cut off or timed out may still be billed, and counts at its estimate.
- When a call says a limit is reached, do not look for a way around it. Stop, use what you have, and tell the user what is missing. A rate limit (the provider asks you to slow down) or a timeout is the same: stop and report it, do not retry the same picture.
