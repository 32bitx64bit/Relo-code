## overview
You are making pictures with an image model the user pays for, a few cents to a few tens of cents a picture. The user sets the limits for the session (a budget, a number of jobs; either can be off) and may be asked to approve each job, so every job is a decision: say what you expect to learn from it before you start it.

Order of work:
1. Read the project for the specification (sizes, view angle, style, naming) and keep it in a short brief file the next task can start from. Ask the user only for what the project cannot tell you.
2. Put the pictures you want matched or changed into the reference store (action `refs`).
3. Write the prompt and call `generate`. It returns at once. Call `wait` (up to 45 seconds a time) until the job is `ready`, or has failed or been cancelled. A picture can take a minute or two, and the user may have to approve the spend first: keep waiting, or finish other work and come back.
4. Look at the contact sheet that `wait` returns. Judge the pictures against the brief, not against each other.
5. Take the best picture through `relo_sprite` (matte, fit, check).
6. Stop when the sprite passes the brief's checks. Report what was made and what it cost.

Pictures are made by a third party from your prompt and references. Do not put anything private in either.

Topics: `prompts`, `refs`, `sprites`, `views`, `iterate`, `cost`.

## prompts
Say what the object is, then its material, then its view and the style, in plain sentences: "A single iron ingot, hammered dark metal with a worn edge, three-quarter top-down view, centred, whole object in frame, painted game-icon style." The model follows detailed instructions well, so be specific rather than adding adjectives.

- One object per picture, centred, with room around it. A cropped object cannot be fixed afterwards.
- Describe the style the same way every time. Keep the description in the brief and reuse it word for word, so a set of items matches.
- Say what to leave out in the same sentence ("no shadow, no floor, no text, no border"), not in a separate list.
- Do not ask for pixel sizes in the prompt. Pick `size` instead, ask for the look at large size, and let `relo_sprite` shrink it.
- Change one thing at a time between jobs, so you know what caused a difference.

## refs
Add a picture with `refs` (a project path, or a picture from an earlier job), then list its id in `generate`'s `refs`. With any reference the job is an edit: the model starts from your pictures, and the prompt says what to do with them. The references are numbered in the order you list them, and the prompt may call them "image 1", "image 2" and so on.

Ways to use them:
- Match the project's existing art: attach two or three of its best existing pictures and write "Match the style, palette, line weight and shading of images 1 to 3. Draw: ..." This is the most reliable way to stay consistent with a game's look.
- Another view or state of something you already made: attach the finished picture and write what changes ("the same object seen from the side", "the same object, damaged and rusted"), and what must not change ("keep the colours and proportions").
- Refine a candidate: attach it and describe the single change.

Fewer references work better than many, and a clean reference on a plain background beats a clever prompt. Each reference adds to the price of the job. If the provider rejects the references, say so in your report: it may not support edits.

## sprites
What makes a generated picture usable as a sprite:
- `background`: ask for `transparent` first. The model then writes a real alpha channel, and the result needs no keying. Not every model or provider accepts it: if the call fails over the background, generate with `opaque` and ask in the prompt for a flat solid background in a colour that is not in the object (magenta or green work for most items: "on a flat solid #ff00ff background, no shadow, no floor, no gradient"), then key it out with `relo_sprite` matte.
- Always check what you got: run `relo_sprite` inspect on the picture and look at the alpha, because a model can answer `transparent` with a faint background or a soft halo.
- Soft shadows, glow and reflections spoil both transparency and keying: leave them out of the prompt.
- Pixel art or a very small final size has to survive the shrink: prefer bold shapes and few colours, and judge the result on a contact sheet at the real size, not at 1024.
- The model draws at its own size. Use `size` for the shape (1024x1024 for most items, a wide or tall size only if the object is) and `relo_sprite` fit and resize for the final pixels.

## views
When the brief calls for several views of one item (front, side, back, damaged):
1. Make the base view first and get it right.
2. Store it as a reference and ask for each other view from it: "The same object as image 1, seen from the left side. Keep the exact colours, materials, proportions and style."
3. Compare the views side by side on one contact sheet (`relo_sprite` sheet) before you accept them.
`relo_sprite` mirror and rotate can derive a view for free when the style allows it; use them before you spend on another picture.

## iterate
Budget the rounds before you start: two or three jobs per sprite is usually enough.
- Explore at `quality` low and `count` 2 to 4, to find a direction cheaply. Finish at medium or high with `count` 1 or 2.
- Do not regenerate a picture that is nearly right: fix the last bit with `relo_sprite` (trim, fit, outline) or one edit job with the picture as a reference and a single change in the prompt.
- When two rounds in a row do not move toward the brief, stop. Report what you tried and what the pictures lack, instead of spending the rest of the budget.
- If the user has asked for a set, finish the first item completely before you start the next, so the others can use it as the style reference.

## cost
Pictures are billed by the provider, to the user's account, by the amount of image data the model reads and writes. The price depends on `quality`, `size`, `count` and the number of references. Before a job runs, Relo works out a deliberately high estimate and refuses the job if it does not fit what is left of the session's budget (when the user has set one); once the job finishes, the real cost, from the usage the provider reports, replaces the estimate. `status` and every job show both.

- The numbers in `status` are the user's limits: the budget per session, the jobs per session, the defaults for quality, size and count. Stay inside them. `budgetUnlimited` or `jobsUnlimited` true means the user turned that limit off, because a large set of pictures needs room: it does not mean the pictures are free. Spend the same way you would with a limit, and keep to the rounds the brief allows. Raising `quality` or `count` for one job is allowed, but only when the picture is worth it.
- Low quality costs a few cents or less, medium several times that, high several times more again. A reference adds a little to each job.
- A failed job costs nothing when the provider refused it. One that was cut off may still be billed, and counts at its estimate.
- When a call says a limit is reached, do not look for a way around it. Stop, use what you have, and tell the user what is missing.
