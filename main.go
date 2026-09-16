// Command bowt manages git worktrees with isolated dev environments.
//
// Agent-first: commands emit structured JSON by default whenever stdout is not
// a terminal, so agents parse output instead of scraping human tables.
//
// The CLI is built on cobra so every command gets real --help, usage, and
// shell completion (see `bowt completion`) — but the command logic still lives
// in the cmd* functions below, called from each command's RunE.
package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/output"
)

// version is set at build time via -ldflags "-X main.version=…" (see Makefile).
var version = "dev"

func main() {
	// Discover user drop-in providers (~/.bowt/agents/*.json) once at startup,
	// AFTER package init has registered the built-ins, so a colliding drop-in is
	// skipped (built-ins win) and provider selection sees the full set. Mirrors
	// how the extension loader is wired from main; a missing dir is a no-op.
	agent.LoadDropins(output.Errf)

	root := newRootCmd()
	// Git-style dispatch: if the first word is not a built-in command but the
	// repo ships a matching extension, run it and exit with its code. Built-ins
	// always win; an unknown word with no extension falls through to cobra's
	// normal unknown-command error below.
	if code, handled := tryExtension(root, os.Args[1:]); handled {
		os.Exit(code)
	}
	if err := root.Execute(); err != nil {
		// Preserve bowt's error contract: "bowt: <err>" on stderr, exit 1.
		// Usage/errors are silenced on the commands so cobra doesn't also print
		// its own "Error:" line or dump usage on a runtime failure.
		output.Errf("%v", err)
		os.Exit(1)
	}
}

// newRootCmd builds the full command tree. It's a constructor (not a package
// var) so tests can build a fresh, isolated tree per case.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bowt",
		Short: "git worktrees with isolated environments",
		Long: `bowt — git worktrees with isolated environments.

Every branch gets its own worktree with an allocated port/offset. Commands emit
JSON by default whenever stdout is not a terminal, so agents parse structured
output instead of scraping tables.`,
		Version: version,
		// Own our error/usage output (see main): print "bowt: <err>" ourselves.
		SilenceUsage:  true,
		SilenceErrors: true,
		// No bash-completion command noise for a private tool; we ship our own.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	// `bowt --version` prints just the version string (matches the old behavior),
	// not cobra's default "bowt version X.Y".
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(
		newNewCmd(),
		newLsCmd(),
		newPathCmd(),
		newRmCmd(),
		newExecCmd(),
		newSpawnCmd(),
		newLaneCmd(),
		newLanesCmd(),
		newStatusCmd(),
		newLaneRunCmd(),
		newGateCmd(),
		newLandCmd(),
		newReviewCmd(),
		newResearchCmd(),
		newDoctorCmd(),
		newRootPathCmd(),
		newCdCmd(),
		newShellInitCmd(),
		newExtensionsCmd(),
		newLibCmd(),
		newVersionCmd(),
		newCompletionCmd(),
	)
	return root
}
