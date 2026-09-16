package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/run"
	"github.com/jfb1121/bowt/internal/state"
	"github.com/jfb1121/bowt/internal/worktree"
)

func newSetupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "setup [branch]",
		Short: "re-run the setup hooks for a worktree",
		Long: `Re-run the repo's pre-setup.sh and setup.sh for a worktree — the same hooks
'bowt new' runs on creation. Useful after editing setup.sh or to re-provision.

With no argument, targets the worktree containing the current directory.`,
		Example: `  bowt setup
  bowt setup feature/login`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeBranchArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			branch, err := resolveSetupBranch(st, args)
			if err != nil {
				return err
			}
			// Take the per-worktree lock so setup can't race a concurrent writer.
			l, err := lock.Acquire(branch)
			if err != nil {
				return err
			}
			defer func() { _ = l.Release() }()

			r := run.Exec{Stderr: os.Stderr}
			if err := worktree.Setup(st, r, branch); err != nil {
				return err
			}
			return output.Emit(struct {
				SetUp string `json:"setup"`
			}{SetUp: branch})
		},
	}
	return c
}

// resolveSetupBranch returns the explicit branch argument, or infers the
// worktree whose path contains the current directory.
func resolveSetupBranch(st state.Store, args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	wts, err := worktree.List(st)
	if err != nil {
		return "", err
	}
	for _, wt := range wts {
		if cwd == wt.Path || strings.HasPrefix(cwd, wt.Path+string(filepath.Separator)) {
			return wt.Branch, nil
		}
	}
	return "", fmt.Errorf("not inside a registered worktree — pass a branch: bowt setup <branch>")
}
