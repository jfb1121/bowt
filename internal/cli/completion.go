package cli

import (
	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/state"
	"github.com/jfb1121/bowt/internal/worktree"
)

// completeBranchArg is the ValidArgsFunction wired onto commands that take a
// worktree/branch as their first argument (cd, rm, path, exec). It completes the
// registered worktree names for the current repo, so `bowt cd <TAB>` offers real
// branches instead of falling back to filenames.
func completeBranchArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Only the first positional arg is a branch; past that, offer nothing
	// (and never fall back to file completion).
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	st, err := state.Open()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, err := branchNames(st)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// branchNames returns the registered worktree branch names for the current repo.
// Factored out of completeBranchArg so it can be tested directly with a seeded
// store and a fixture git repo (no shell round-trip required).
func branchNames(st state.Store) ([]string, error) {
	wts, err := worktree.List(st)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(wts))
	for _, wt := range wts {
		names = append(names, wt.Branch)
	}
	return names, nil
}
