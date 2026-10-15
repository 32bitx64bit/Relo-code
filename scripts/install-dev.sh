#!/bin/sh
# Build Relo for this machine, package a .vsix, and install it into VS Code.
# Idempotent. No sudo. Set RELO_SKIP_INSTALL=1 to stop after building the .vsix.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

die() { echo "error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 not found on PATH$2"; }

need go " (install Go, version from core/go.mod)"
need node " (install Node.js 20+)"
need npm " (ships with Node.js)"

NODE_MAJOR=$(node -p 'process.versions.node.split(".")[0]')
[ "$NODE_MAJOR" -ge 20 ] || die "Node.js $NODE_MAJOR found; need 20+"

echo "==> core (host platform)"
make core

echo "==> extension dependencies"
if [ ! -d extension/node_modules ]; then
	(cd extension && npm ci)
else
	echo "node_modules present, skipping npm ci"
fi

echo "==> compile extension and copy webview"
make extension webview

echo "==> package vsix"
[ -x extension/node_modules/.bin/vsce ] || (cd extension && npm install --no-audit --no-fund)
(cd extension && ./node_modules/.bin/vsce package --no-dependencies --allow-missing-repository --skip-license)
VSIX_NAME=$(node -p 'const p=require("./extension/package.json");p.name+"-"+p.version+".vsix"')
VSIX="$ROOT/extension/$VSIX_NAME"
[ -f "$VSIX" ] || die "vsix not produced at $VSIX"
echo "vsix: $VSIX ($(du -h "$VSIX" | cut -f1))"

if [ "${RELO_SKIP_INSTALL:-0}" = "1" ]; then
	echo "RELO_SKIP_INSTALL=1: not installing"
	exit 0
fi

CODE=""
for c in code codium code-insiders; do
	if command -v "$c" >/dev/null 2>&1; then CODE=$c; break; fi
done
if [ -z "$CODE" ]; then
	echo "VS Code CLI not found. Install it from VS Code: Ctrl+Shift+P -> 'Shell Command: Install code command in PATH'." >&2
	echo "Then run: code --install-extension \"$VSIX\" --force" >&2
	exit 1
fi

echo "==> install with $CODE"
"$CODE" --install-extension "$VSIX" --force
echo "Installed. Reload VS Code window (Ctrl+Shift+P -> Developer: Reload Window)."
