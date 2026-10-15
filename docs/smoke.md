# Live smoke test

Drives the real Relo Core (in-process `app.New`, `Runtime: "opencode"`) against the real OpenCode backend using the free model `opencode/cat-jam-free` for all roles. The smoke test runs with `RELO_SHARE_OPENCODE=1` because that model is an OpenCode built-in, which Relo normally hides.

Costs real model turns. Gated by `RELO_LIVE_SMOKE=1`; skipped otherwise.

```sh
RELO_LIVE_SMOKE=1 sh scripts/smoke.sh auto
RELO_LIVE_SMOKE=1 sh scripts/smoke.sh off
```

Env:
- `RELO_SMOKE_MAX_TURNS` (default 4): hard cap on live `SendTask` calls. Calls past the cap are refused before reaching OpenCode.

Flow: initialize (models set to the free model, Saving level) -> `prompt.submit` with one objective -> collect events until no agent is running, or 4 min -> `Close` within 5s -> goroutine count check.

Artifacts in `/tmp/kilo/smoke/<UTC-ts>-<preset>/`:
- `events.jsonl`: every emitted event, timestamped.
- `summary.md`: event counts, last `cache.updated`, warn/error log lines.

Test source: `core/internal/app/smoke_live_test.go`.
