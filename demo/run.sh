#!/usr/bin/env bash
# The scripted bowt lanes session that asciinema records. Paced with sleeps so
# it reads like a live demo. Run indirectly via demo/record.sh (which seeds the
# isolated demo, sets HOME/PATH, and records this).
set -uo pipefail

PROMPT=$'\033[38;5;213m$\033[0m '            # pink prompt
DIM=$'\033[38;5;245m'; RST=$'\033[0m'

run() {  # echo prompt+command, then run it, then pause
	printf '%s%s\n' "$PROMPT" "$1"; sleep 0.5
	eval "$1"; sleep "${2:-2}"
}
note() { printf '%s%s%s\n' "$DIM" "$1" "$RST"; sleep 0.7; }

clear
sleep 0.7
note "# three tasks, three isolated worktrees — each its own port, DB, env"
run "bowt ls" 2.2
note "# dispatch an agent to plan the checkout work"
run "bowt spawn --agent demo" 2.6
note "# the fleet: every lane and where it stands"
run "bowt lanes" 2.6
note "# per-worktree cockpit — branch x lane x gate"
run "bowt status" 3.2
