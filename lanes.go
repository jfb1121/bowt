package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/state"
)

// newLanesCmd registers `bowt lanes`: the read-only lane list, one row per lane
// with its (reconciled) status + derived scalars.
func newLanesCmd() *cobra.Command {
	var asJSON bool
	var repoName string
	c := &cobra.Command{
		Use:   "lanes [--repo <r>]",
		Short: "list the tracked lanes and their reconciled status",
		Long: `List every headless-spawn lane for the repo — id, status, agent/model,
attempt, wave, gate verdict, review C/S/N, and the escalated/paused flags.

Each lane is reconciled at read time: a lane stuck in a running status
(planning/impl/review) whose worktree lock is free (its detached supervisor died
before writing the terminal status) is repaired from its writeback files before
it is shown, and the repair is persisted (self-heal). A human table is printed at
a terminal; JSON is emitted otherwise (agent-first) or with --json.`,
		Example: `  bowt lanes
  bowt lanes --json
  bowt lanes --repo other-repo`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ls, err := state.OpenLanes()
			if err != nil {
				return err
			}
			if repoName == "" {
				repoName, err = repo.Name()
				if err != nil {
					return err
				}
			}
			return cmdLanes(ls, repoName, lock.Probe, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "force JSON output")
	c.Flags().StringVar(&repoName, "repo", "", "repo to list lanes for (default: current repo)")
	return c
}

// laneView is the read-only cockpit projection of a lane row: a subset of
// state.Lane's scalars an orchestrator reads, plus Reconciled — set when the
// status was repaired from files at THIS read (a SIGKILL-orphaned row self-heal
// has now corrected). A declared shape, per house style, not a map.
type laneView struct {
	ID             string       `json:"id"`
	Ticket         string       `json:"ticket,omitempty"`
	Status         state.Status `json:"status"`
	Agent          string       `json:"agent,omitempty"`
	Model          string       `json:"model,omitempty"`
	Attempt        int          `json:"attempt"`
	Wave           int          `json:"wave"`
	GateVerdict    string       `json:"gate_verdict,omitempty"`
	ReviewBlockers int          `json:"review_blockers"`
	ReviewMajors   int          `json:"review_majors"`
	ReviewMinors   int          `json:"review_minors"`
	Escalated      bool         `json:"escalated"`
	PausedOn       string       `json:"paused_on,omitempty"`
	Branch         string       `json:"branch"`
	Worktree       string       `json:"worktree"`
	Reconciled     bool         `json:"reconciled,omitempty"`
	// LockHeld is the live signal, reported alongside the recorded phase so a
	// reader can catch the one inconsistency the phase alone cannot express:
	// a running status (planning/impl/review) with no lock held means the
	// supervisor is gone and the row has not been healed yet — which is what
	// makes a finished lane read as still working.
	LockHeld bool `json:"lock_held"`
}

func toLaneView(l state.Lane, reconciled bool, lockHeld bool) laneView {
	return laneView{
		ID: l.ID, Ticket: l.Ticket, Status: l.Status, Agent: l.Agent, Model: l.Model,
		Attempt: l.Attempt, Wave: l.Wave, GateVerdict: l.GateVerdict,
		ReviewBlockers: l.ReviewBlockers, ReviewMajors: l.ReviewMajors, ReviewMinors: l.ReviewMinors,
		Escalated: l.Escalated, PausedOn: l.PausedOn, Branch: l.Branch, Worktree: l.Worktree,
		Reconciled: reconciled, LockHeld: lockHeld,
	}
}

// worktreeView is the per-worktree cockpit row: the registry facts joined with
// the live lock state, the on-disk gate verdict, and the lane(s) on the worktree.
type worktreeView struct {
	Repo        string     `json:"repo"`
	Branch      string     `json:"branch"`
	Path        string     `json:"path"`
	Commit      string     `json:"commit,omitempty"`
	Dirty       bool       `json:"dirty"`
	LockHeld    bool       `json:"lock_held"`
	GateVerdict string     `json:"gate_verdict,omitempty"`
	GateCommit  string     `json:"gate_commit,omitempty"`
	Lanes       []laneView `json:"lanes"`
}

// reconcileLaneViews reconciles each lane at read time, SELF-HEALS a corrected
// row (RFC option b: a running-status lane whose lock is free is unambiguously
// stale, so persist the reconstructed terminal status to RETIRE the SIGKILL edge
// rather than re-derive it every read — the decision stays the pure reconcile()
// above; only this reader writes), and projects the result. Shared by `lanes`
// and `status` so both reconcile identically.
func reconcileLaneViews(ls state.LaneStore, lanes []state.Lane, probe func(string) (bool, error)) ([]laneView, error) {
	views := make([]laneView, 0, len(lanes))
	for _, lane := range lanes {
		fixed, changed, held, err := reconcileForRead(lane, probe)
		if err != nil {
			return nil, err
		}
		if changed {
			if err := ls.UpdateLane(fixed); err != nil {
				return nil, err
			}
		}
		views = append(views, toLaneView(fixed, changed, held))
	}
	return views, nil
}

// cmdLanes lists the repo's lanes as the read-only cockpit projection, each lane
// reconciled (and self-healed) first. Agent-first: JSON off a TTY or with --json.
func cmdLanes(ls state.LaneStore, repoName string, probe func(string) (bool, error), asJSON bool) error {
	lanes, err := ls.ListLanes(repoName)
	if err != nil {
		return err
	}
	views, err := reconcileLaneViews(ls, lanes, probe)
	if err != nil {
		return err
	}
	return emitLaneViews(views, asJSON)
}

// emitLaneViews renders a set of reconciled lane rows agent-first: JSON off a TTY
// or with --json, else the human cockpit table. Shared by `bowt lanes` and `bowt
// lane wait` so their row output stays identical.
func emitLaneViews(views []laneView, asJSON bool) error {
	// A nil slice marshals to JSON `null`; agents expect an array. Coerce to [].
	if views == nil {
		views = []laneView{}
	}
	if asJSON || !output.IsTTY() {
		return output.Emit(views)
	}
	if len(views) == 0 {
		fmt.Println("no lanes")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tAGENT\tMODEL\tATT\tWAVE\tGATE\tREVIEW\tFLAGS\tBRANCH")
	for _, v := range views {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			v.ID, v.Status, orDash(v.Agent), orDash(v.Model), v.Attempt, v.Wave,
			orDash(v.GateVerdict), reviewCell(v), laneFlags(v), v.Branch)
	}
	return w.Flush()
}
