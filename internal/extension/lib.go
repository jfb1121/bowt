package extension

// Lib is the sourceable bash helper an extension loads via `source "$BOWT_LIB"`,
// and the exact text `bowt lib` prints. The per-worktree BOWT_* + config
// environment is already exported into the script, so the lib is intentionally
// thin — just diagnostic helpers that respect the stdout=data / stderr=logs
// split. Richer callbacks (bowt_signal, bowt_gate_record, bowt_status_add) are
// deliberately deferred.
const Lib = `# bowt.lib.sh — sourceable helpers for bowt extensions.
#
# The per-worktree BOWT_* + config environment is already exported into
# your script, so this lib is intentionally thin. Source it at the top of an
# extension with:
#
#   source "$BOWT_LIB"
#
# Keep the stdout=data / stderr=diagnostics split: results go to stdout, logs
# and errors go to stderr via the helpers below. Richer callbacks (bowt_signal,
# bowt_gate_record, bowt_status_add) are not yet provided.

# bowt_log <msg...> — print an informational line to stderr.
bowt_log() { printf 'bowt: %s\n' "$*" >&2; }

# bowt_err <msg...> — print an error line to stderr.
bowt_err() { printf 'bowt: error: %s\n' "$*" >&2; }
`
