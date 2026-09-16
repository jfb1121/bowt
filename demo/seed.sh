#!/usr/bin/env bash
# Seed an isolated bowt state for the lanes demo GIF — deterministic, no real
# agents or models. Everything lives under a throwaway HOME so it never touches
# your real ~/.bowt. Real bits: the worktrees (bowt new) and the stub-agent
# adapter wiring. Seeded bits: the lane rows (in reconcile-safe terminal states).
#
# Usage: BOWT_BIN=/path/to/bowt demo/seed.sh /tmp/bowt-demo
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
BOWT_BIN="${BOWT_BIN:-bowt}"
DEMO="${1:?usage: seed.sh <demo-home-dir>}"

rm -rf "$DEMO"; mkdir -p "$DEMO/bin"
export HOME="$DEMO"

# Put the demo binary on a PATH the tape can use.
cp "$(command -v "$BOWT_BIN")" "$DEMO/bin/bowt"
BOWT="$DEMO/bin/bowt"

# 1. A throwaway repo.
REPO="$DEMO/acme"
mkdir -p "$REPO"; cd "$REPO"
git init -q
git config user.email demo@bowt.sh
git config user.name "bowt demo"
git commit -q --allow-empty -m "init acme"

# 2. Real, isolated worktrees (real ports/offsets).
"$BOWT" new feat/checkout      >/dev/null
"$BOWT" new feat/search        >/dev/null
"$BOWT" new fix/webhook-retry  >/dev/null

# 3. A real drop-in stub agent (bowt's adapter runs it like any provider).
#    hooks/effort declared only to keep the demo output clean (no guardrail /
#    effort-drop warnings) — it's a fake provider used solely for the GIF.
mkdir -p "$DEMO/.bowt/agents"
cat > "$DEMO/.bowt/agents/demo.json" <<JSON
{
  "schemaVersion": 1,
  "name": "demo",
  "bin": "$HERE/demo-agent.sh",
  "hooks": true,
  "effort": { "low": [], "medium": [], "high": [] },
  "invocations": { "session": { "prefix": [], "prompt": "arg-after-dashdash" } }
}
JSON

# A brief in the checkout worktree so `bowt spawn` has something to plan.
CHECKOUT_WT="$("$BOWT" path feat/checkout)"
mkdir -p "$CHECKOUT_WT/subagent"
printf 'Implement the checkout flow: route, payment intent, tests.\n' > "$CHECKOUT_WT/subagent/PROMPT.md"

# 4. Seed lane rows in terminal (reconcile-safe) states so the cockpit is stable.
DB="$DEMO/.bowt/state.db"
seed_lane() { # id branch status blockers majors minors
  local wt; wt="$("$BOWT" path "$2" 2>/dev/null || echo "$REPO")"
  sqlite3 "$DB" "INSERT INTO lanes
    (id,repo,branch,worktree,status,agent,model,review_blockers,review_majors,review_minors,created,updated)
    VALUES ('$1','acme','$2','$wt','$3','claude','opus-4.8',$4,$5,$6,datetime('now'),datetime('now'));"
}
seed_lane checkout feat/checkout     plan-review 0 0 0
seed_lane search   feat/search       paused      0 0 0
seed_lane webhook  fix/webhook-retry done        0 1 3

echo "seeded demo at $DEMO" >&2
