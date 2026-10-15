#!/bin/sh
# Read-only environment checklist for Relo. Never kills or restarts OpenCode.
# Exit 1 only on hard failures (go/node/opencode missing or wrong version, extension missing).
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
FAIL=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; FAIL=1; }
warn() { printf 'WARN  %s\n' "$1"; }

# version_ge "1.25.0" "1.23" -> true if $1 >= $2
version_ge() {
	[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

# Go
GO_NEED=$(sed -n 's/^go \([0-9.]*\).*/\1/p' "$ROOT/core/go.mod" | head -n1)
if command -v go >/dev/null 2>&1; then
	GO_HAVE=$(go env GOVERSION 2>/dev/null | sed 's/^go//;s/[^0-9.].*//')
	if version_ge "$GO_HAVE" "$GO_NEED"; then pass "go $GO_HAVE (>= $GO_NEED from core/go.mod)"
	else fail "go $GO_HAVE too old; need >= $GO_NEED"; fi
else fail "go not found; install Go >= $GO_NEED"; fi

# Node
if command -v node >/dev/null 2>&1; then
	NODE_HAVE=$(node -p 'process.versions.node')
	if version_ge "$NODE_HAVE" "20.0.0"; then pass "node $NODE_HAVE (>= 20)"
	else fail "node $NODE_HAVE too old; need >= 20"; fi
else fail "node not found; install Node.js 20+"; fi

# OpenCode binary
if command -v opencode >/dev/null 2>&1; then
	OC_VER=$(opencode --version 2>/dev/null | head -n1 | sed 's/^opencode v//;s/^v//')
	case "$OC_VER" in
		2.*) pass "opencode $OC_VER" ;;
		*) fail "opencode '$OC_VER' found; Relo expects 2.x" ;;
	esac
else
	fail "opencode not on PATH; install from https://opencode.ai/docs"
fi

# OpenCode server reachability (read-only status query; does not start or stop anything)
if command -v opencode >/dev/null 2>&1; then
	SVC=$(timeout 10 opencode service status 2>/dev/null | head -n1 || true)
	case "$SVC" in
		http*) pass "opencode server reachable at $SVC" ;;
		*) warn "opencode server not running; Relo will start its own on demand (no action needed)" ;;
	esac

	# Model count (read-only; never prints credentials)
	MODELS=$(timeout 20 opencode models 2>/dev/null | grep -c '/' || true)
	if [ "${MODELS:-0}" -gt 0 ]; then pass "opencode models available: $MODELS"
	else warn "your own OpenCode lists no models (fine: Relo uses only providers added in Settings, Providers)"; fi
	AUTH=$(timeout 10 opencode auth list 2>/dev/null | head -n1 || true)
	case "$AUTH" in
		*"No authenticated"*) warn "your own OpenCode has no logins (fine: Relo uses its own private OpenCode and only the providers you add in Settings)" ;;
		*) [ -n "$AUTH" ] && pass "opencode auth: $AUTH" ;;
	esac
fi

# VS Code CLI and extension
CODE=""
for c in code codium code-insiders; do
	if command -v "$c" >/dev/null 2>&1; then CODE=$c; break; fi
done
if [ -n "$CODE" ]; then
	pass "VS Code CLI: $CODE"
	if "$CODE" --list-extensions 2>/dev/null | grep -qi '^relocode\.relo-code$'; then
		pass "extension relocode.relo-code installed"
	else
		fail "extension not installed; run: make dev"
	fi
else
	fail "VS Code CLI (code/codium) not found; in VS Code run 'Shell Command: Install code command in PATH'"
fi

# Bundled core binary for this host
OS=$(uname -s | tr '[:upper:]' '[:lower:]'); ARCH=$(uname -m)
case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; esac
if [ -x "$ROOT/extension/bin/relo-core-$OS-$ARCH" ]; then pass "core binary extension/bin/relo-core-$OS-$ARCH"
else warn "core binary missing; run: make core"; fi

echo
if [ "$FAIL" -eq 0 ]; then echo "doctor: all hard checks passed"; else echo "doctor: hard failures above"; fi
exit "$FAIL"
