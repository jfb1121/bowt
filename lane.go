package main

import (
	"github.com/spf13/cobra"
)

// newLaneCmd is the parent group for lane-scoped verbs. A headless spawn records
// a lane row; these operate on that row. (Only `followup` ships in this slice —
// the read-only `lane show`/`lanes` cockpit is G4.)
func newLaneCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lane",
		Short: "operate on a headless spawn's lane (the tracked unit of delegated work)",
		Long: `A headless spawn (bowt spawn --headless) records a lane: a tracked unit of
delegated work with a status, provenance, and derived comms. lane subcommands
act on that row by id.`,
	}
	c.AddCommand(newLaneFollowupCmd())
	c.AddCommand(newLaneWaitCmd())
	return c
}
