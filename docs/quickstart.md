# Relo quickstart (Linux)

## 1. Install

Needs Go (version in `core/go.mod`), Node.js 20+, VS Code, and `opencode` 2.x on `PATH`.

```sh
make dev        # builds core, packages .vsix, installs into VS Code
```

Without the VS Code CLI, `make vsix` only builds `extension/relo-code-<ver>.vsix`. Install it manually with
`code --install-extension extension/relo-code-0.1.0.vsix --force`.

## 2. Check the environment

```sh
make preflight  # or: sh scripts/doctor.sh   (read-only, never restarts OpenCode)
```

Exit code is non-zero only on hard failures (Go, Node, OpenCode, extension missing).

## 3. Open a folder and run Relo

1. `code /path/to/project`, then reload the window (Ctrl+Shift+P, "Developer: Reload Window").
2. Ctrl+Shift+P, then pick a command starting with **Relo:**, e.g. `Relo: Open`, `Relo: New Session`,
   `Relo: Run Task`, `Relo: Plan Task`, `Relo: Review Changes`, `Relo: Open Logs`.
3. Ctrl+Shift+. (or Cmd+Shift+. on macOS) opens the Relo view.

## 4. Configure models

Settings (Ctrl+,, search `relo.`):

- `relo.primaryModel`, `relo.tier1Model`, `relo.tier2Model` (format `provider/model`). Tier 2 is the cheap one: trivial and easy tasks and probes go there, medium and hard tasks to Tier 1, expert tasks to the Primary model.
- `relo.routing.byDifficulty`, `relo.routing.escalate`
- Token saving: `relo.efficiency.preset` (Saving level: `off|auto|max`), `relo.efficiency.voice` (Reply brevity), `relo.efficiency.craft` (Minimal code)
- `relo.runtime` (`opencode` or `mock`), `relo.openCode.path`

Pick models with **Relo: Select Primary Model** / **Select Tier 1 Model** / **Select Tier 2 Model**, or in Settings, Models.

Relo runs its own private OpenCode, separate from any OpenCode you have installed. It does not read your
OpenCode config, sessions or logins, and OpenCode's built-in providers are switched off. The only models you see
are the ones you add under Settings, Providers (an OpenAI- or Anthropic-compatible endpoint plus an API key).
Relo keeps OpenCode's state in `~/.config/relo-code/opencode`. Your own OpenCode is never touched.

## 5. Zero-cost smoke test

Add a provider that costs nothing (a local server such as Ollama or LM Studio works), pick its model for
Primary, Tier 1 and Tier 2, then run **Relo: New Session** and send a small task.

To test against OpenCode's own free models instead, set the environment variable `RELO_SHARE_OPENCODE=1` before
starting VS Code. That turns isolation off: Relo then uses your normal OpenCode setup and its full model catalog.
This is for development only.

## 6. UI-only testing (mock runtime)

No OpenCode, no model calls:

- Settings: `relo.runtime` = `mock`, or
- Launch config **Run Extension (mock runtime)** (F5), or
- `RELO_RUNTIME=mock code .`, or core flag `-runtime mock`.

## 7. Logs

- VS Code: **Relo: Open Logs** (Output channel named "Relo"). Core logs go to stderr, shown there.
- Core run standalone: `extension/bin/relo-core-linux-amd64 -workspace . -log-level debug`.
- Workspace state: `<workspace>/.relo/relo.db`.

## 8. Develop

- F5 **Run Extension** builds core, extension and webview first (`make build`).
- `make test`, `make lint`, `sh scripts/check.sh` before committing.

## 9. Report issues

Open an issue with: `sh scripts/doctor.sh` output, the "Relo" output channel log, OS, and steps to reproduce.
Remove API keys before posting.
