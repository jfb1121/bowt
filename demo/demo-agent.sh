#!/usr/bin/env bash
# Stub "agent" for the bowt demo. Prints a realistic run instantly — no model,
# no network — and drops a PLAN.md into the worktree writeback like a real plan
# pass would. bowt's adapter invokes this exactly as it would `claude`; only the
# agent is fake. Used only by demo/seed.sh; never shipped as a real provider.
set -euo pipefail

say() { printf '  %s\n' "$1"; sleep 0.35; }

echo "demo-agent ▸ reading brief"
say "understanding the task: implement checkout"
say "surveying the repo: 3 files in scope"
say "drafting plan"

mkdir -p subagent/writeback 2>/dev/null || true
cat > subagent/writeback/PLAN.md <<'PLAN'
# Plan — checkout
1. add POST /checkout route
2. create the payment intent + persist the order
3. tests: happy path + declined card
PLAN

echo "demo-agent ✓ plan written → subagent/writeback/PLAN.md"
