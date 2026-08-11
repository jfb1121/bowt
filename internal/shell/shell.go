// Package shell generates the shell integration — a `bowt` wrapper function that
// lets `bowt cd` change the caller's directory, plus bowt's shell completion.
// Changing the parent shell's cwd is the one thing a child process cannot do, so
// it's the only reason the shim exists; every other command runs the binary
// directly. Agents don't need this (they use `bowt path` / `bowt exec`), which is
// why the binary is complete without it.
package shell

import "fmt"

// Init returns the snippet to eval, e.g. eval "$(bowt shell-init zsh)".
// bash and zsh share one POSIX-compatible cd shim; completion is loaded with the
// shell-specific loader so `bowt cd <TAB>` completes registered worktree names.
func Init(shellName string) (string, error) {
	switch shellName {
	case "bash":
		return shim + "\n" + bashCompletion, nil
	case "zsh":
		return shim + "\n" + zshCompletion, nil
	default:
		return "", fmt.Errorf("unsupported shell %q (want bash or zsh)", shellName)
	}
}

// The wrapper intercepts `cd` (resolving the path via the binary and doing the
// actual `builtin cd` itself) and passes everything else straight through.
// `command bowt` bypasses this function to reach the real binary — no recursion.
const shim = `bowt() {
	case "$1" in
	cd)
		shift
		local _dest
		if [ "$1" = "main" ] || [ "$1" = "root" ] || [ -z "$1" ]; then
			_dest="$(command bowt root)" || return
		else
			_dest="$(command bowt path "$1")" || return
		fi
		builtin cd "$_dest"
		;;
	*)
		command bowt "$@"
		;;
	esac
}
`

// The completion loaders coexist with the shim above: shell completion is keyed
// on the word `bowt`, so it fires even though `bowt` is now a function. The
// cobra completion script calls `bowt __complete …`, which the shim's `*)` case
// passes straight through to `command bowt` — no recursion, no special-casing.
// Sourcing is tolerant: a completion failure (e.g. an old binary) must never
// break the interactive shell, so errors are swallowed.
const bashCompletion = `if command -v bowt >/dev/null 2>&1; then
	source <(command bowt completion bash) 2>/dev/null || true
fi
`

// zsh needs the completion system initialized before compdef exists; init it if
// it hasn't been, then load bowt's completion.
const zshCompletion = `if ! command -v compdef >/dev/null 2>&1; then
	autoload -Uz compinit && compinit -C 2>/dev/null || true
fi
source <(command bowt completion zsh) 2>/dev/null || true
`
