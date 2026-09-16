#!/usr/bin/env bash
# Regenerate docs/demo.gif: build bowt, seed an isolated demo, record with
# asciinema, render to GIF with agg (font-based, no browser).
# Requires: asciinema, agg  (brew install asciinema agg). Nothing touches ~/.bowt.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
DEMO=/tmp/bowt-demo
mkdir -p "$REPO/docs"

echo "▸ building demo binary"
go build -o /tmp/bowt-demo-bin "$REPO"

echo "▸ seeding isolated demo state"
BOWT_BIN=/tmp/bowt-demo-bin "$REPO/demo/seed.sh" "$DEMO"

echo "▸ recording session"
export HOME="$DEMO"
export PATH="$DEMO/bin:$PATH"
cd "$DEMO/acme-worktrees/feat-checkout"
asciinema rec --overwrite --window-size 110x30 -c "bash '$REPO/demo/run.sh'" /tmp/bowt-demo.cast

echo "▸ rendering GIF"
agg --font-size 24 --theme monokai --idle-time-limit 2 /tmp/bowt-demo.cast "$REPO/docs/demo.gif"

echo "✓ wrote $REPO/docs/demo.gif"
