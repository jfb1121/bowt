#!/usr/bin/env bash
# bowt's own gate hook — dogfooding. `bowt gate` / `bowt land` run this in a
# worktree to decide pass/fail. It mirrors `make check` (the single source of
# truth for what CI enforces), running each phase and reporting a per-check
# BOWT_CHECK line on stdout that gate collects:
#
#   BOWT_CHECK <name> <pass|fail> <exit_code>
#
# Progress goes to stderr (gate streams it live); only BOWT_CHECK lines go to
# stdout. The hook exits non-zero if any phase failed, so gate's overall verdict
# is 'fail' even though it also folds in each check.
set -u

overall=0

# check <name> <cmd...> — run a phase, emit its BOWT_CHECK line, remember failure.
check() {
	local name="$1"
	shift
	echo ">>> ${name}: $*" >&2
	if "$@" >&2; then
		echo "BOWT_CHECK ${name} pass 0"
	else
		local code=$?
		echo "BOWT_CHECK ${name} fail ${code}"
		overall=1
	fi
}

# Phases mirror `make check` (fmt-check + vet + lint + race), plus a build.
check build go build ./...
check fmt make fmt-check
check vet go vet ./...
check lint golangci-lint run
check test go test -race ./...

exit "${overall}"
