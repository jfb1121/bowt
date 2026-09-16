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
		Long: `Create a .bowt/ directory with a generic config and setup/teardown hooks, and
add .bowt/ to the repo's .git/info/exclude.

Edit .bowt/config and .bowt/setup.sh for your stack; then 'bowt new <branch>'
runs them for each worktree (and 'bowt setup' re-runs them).`,
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
			return output.Emit(res)
		},
	}
}
