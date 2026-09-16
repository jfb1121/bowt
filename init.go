package main

import (
	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/scaffold"
)

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "scaffold a .bowt/ config for this repo",
		Long: `Create a .bowt/ directory with a generic config, setup/teardown hooks, and an
agent guide (AGENTS.md). Commit .bowt/ so your whole team shares one worktree
setup; bowt's runtime artifacts are ignored via a scaffolded .bowt/.gitignore.

Edit .bowt/config and .bowt/setup.sh for your stack — or point a coding agent at
.bowt/AGENTS.md and let it wire the hooks by reading this repo. Then
'bowt new <branch>' runs them for each worktree ('bowt setup' re-runs them).`,
		Example: "  bowt init",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			main, err := repo.MainRepo()
			if err != nil {
				return err
			}
			res, err := scaffold.Init(main)
			if err != nil {
				return err
			}
			// Human hint on stderr (stdout stays reserved for the JSON result).
			output.Errf("next: edit .bowt/setup.sh, or point an agent at .bowt/AGENTS.md to wire it up")
			return output.Emit(res)
		},
	}
}
