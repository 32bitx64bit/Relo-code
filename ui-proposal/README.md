# Relo UI Proposal

- Build offline: `node ui-proposal/build.mjs` (uses installed extension TypeScript compiler).
- Open `ui-proposal/index.html` directly with browser; no server or network required.
- Dark-first VS Code shell keeps session thread central; inspector tabs expose orchestration state.
- Thread shows plans, tool calls, approvals, refresh, escalation, compressed reports, and proposed hunks.
- Agent hierarchy and DAG show role profiles, worktree isolation, and Haiku-to-Sonnet escalation.
- Context shows generations, provenance, promotion, dropped items, and L0-L3 layers.
- Prompt cache shows hit rates, estimated savings, and intentional refresh/escalation session misses.
- Changes shows isolated diffs, accept/reject, conflicts, and verification outcomes.
- Routing shows difficulty models, escalation ladder, attempt cap, and failure toggle.
- Efficiency shows Off/Auto/Max, role profiles, modifiers, prompt anatomy, token ledger, and parity badge.
- Mock: all tokens, savings, pass rates, cache, sessions, models, costs, and verification data.
- Real values come from Relo ledger/A-B harness; UI actions are local simulations.
- Standalone HTML/CSS/JS with strict TypeScript source; file:// works without backend or network.

## Native redesign
- VS Code theme tokens only (`--vscode-*`), no custom palette, gradients, or shadows.
- Density: 13px base, 4px grid, 1px panel-border dividers, collapsible sections.
- Rebuild: `node build.mjs` from ui-proposal directory.
