#!/bin/sh
# Live end-to-end smoke of Relo Core with the real OpenCode backend.
# Usage: RELO_LIVE_SMOKE=1 sh scripts/smoke.sh [off|auto]
# Costs real model turns. Default cap: RELO_SMOKE_MAX_TURNS=4 (override as needed).
set -eu

# The free model is an OpenCode built-in; Relo hides built-ins unless this is set.
export RELO_SHARE_OPENCODE=1
if [ "${RELO_LIVE_SMOKE:-}" != "1" ]; then
	echo "skip: set RELO_LIVE_SMOKE=1 to run the live smoke test" >&2
	exit 0
fi

preset="${1:-auto}"
case "$preset" in
	off|auto) ;;
	*) echo "usage: sh scripts/smoke.sh [off|auto]" >&2; exit 2 ;;
esac

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root/core"

RELO_SMOKE_PRESET="$preset" \
RELO_SMOKE_MAX_TURNS="${RELO_SMOKE_MAX_TURNS:-4}" \
	go test ./internal/app/ -run '^TestLiveSmoke$' -count=1 -v -timeout 6m

echo "artifacts: /tmp/kilo/smoke/<timestamp>-$preset/{events.jsonl,summary.md}"
