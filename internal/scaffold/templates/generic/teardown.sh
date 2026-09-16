#!/usr/bin/env bash
# bowt teardown hook — runs on `bowt rm`, before the worktree is removed.
# Positional args: $1=path  $2=branch  $3=offset  $4=port
set -euo pipefail

path="$1"; branch="$2"; offset="$3"; port="$4"

echo "bowt teardown: $branch"

# Undo what setup.sh created. Examples:
#   dropdb "${BOWT_DB_PREFIX:-app}_${offset}" 2>/dev/null || true
