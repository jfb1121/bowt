// Package shell generates the shell integration — a `bowt` wrapper function that
// lets `bowt cd` change the caller's directory. Changing the parent shell's cwd
// is the one thing a child process cannot do, so it's the only reason the shim
// exists; every other command runs the binary directly. Agents don't need this
// (they use `bowt path` / `bowt exec`), which is why the binary is complete
// without it.
package shell

import "fmt"

// Init returns the snippet to eval, e.g. eval "$(bowt shell-init zsh)".
// bash and zsh share one POSIX-compatible function.
func Init(shellName string) (string, error) {
	switch shellName {
	case "bash", "zsh":
		return shim, nil
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
