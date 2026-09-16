#!/usr/bin/env bash
# bowt setup hook — runs after `bowt new` and on `bowt setup`.
# Positional args: $1=path  $2=branch  $3=offset  $4=port
# Env: BOWT_PORT BOWT_OFFSET BOWT_BRANCH BOWT_MAIN_REPO BOWT_REPO_NAME
#      BOWT_CODE_ONLY (+ GWT_* aliases), plus any vars from .bowt/config.
set -euo pipefail

path="$1"; branch="$2"; offset="$3"; port="$4"

echo "bowt setup: $branch → $path (port $port)"

# Provision this worktree's isolated environment. Examples:
#   cp "$BOWT_MAIN_REPO/.env.example" "$path/.env"
#   printf 'PORT=%s\n' "$port" >> "$path/.env"
#   createdb "${BOWT_DB_PREFIX:-app}_${offset}"
