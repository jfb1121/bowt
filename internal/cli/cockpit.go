package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/gate"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/state"
)

// newStatusCmd registers `bowt status`: the per-worktree cockpit joining the
// registry × lanes × live lock × gate.json.
func newStatusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "per-worktree cockpit: registry × lanes × lock × gate",
		Long: `Show one row per registered worktree in the current repo — its HEAD commit,
dirty flag, whether its lock is currently held (a live spawn/gate/land), the
worktree's gate verdict (from .bowt/gate.json), and the lane(s) on it with each
lane's reconciled status, gate verdict, review C/S/N, and escalated/paused flags.

A worktree with no lane still appears (from the registry), just without lane
data. Lanes are reconciled at read time exactly as 'bowt lanes' does. This is the
one screen an orchestrator reads instead of grepping. JSON is emitted off a
terminal (agent-first) or with --json; a human table at a terminal.`,
		Example: `  bowt status
  bowt status --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			ls, err := state.OpenLanes()
			if err != nil {
				return err
			}
			name, err := repo.Name()
			if err != nil {
				return err
			}
			return cmdStatus(st, ls, name, lock.Probe, realWorktreeFacts, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "force JSON output")
	return c
}

// cmdStatus is the per-worktree cockpit: the registry joined with each worktree's
// live lock, its on-disk gate verdict, and the reconciled lane(s) on it. facts
// gathers the git/gate IO (injected so the join is testable without git); probe
// reads the live lock. Agent-first: JSON off a TTY or with --json.
func cmdStatus(st state.Store, ls state.LaneStore, repoName string, probe func(string) (bool, error), facts gatherFacts, asJSON bool) error {
	wts, err := st.List(repoName)
	if err != nil {
		return err
	}
	lanes, err := ls.ListLanes(repoName)
	if err != nil {
		return err
	}
	laneViews, err := reconcileLaneViews(ls, lanes, probe)
	if err != nil {
		return err
	}
	// Join lanes to worktrees on branch (worktrees' PK is (repo,branch); both
	// lists are already scoped to repoName).
	byBranch := make(map[string][]laneView, len(laneViews))
	for _, lv := range laneViews {
		byBranch[lv.Branch] = append(byBranch[lv.Branch], lv)
	}

	views := make([]worktreeView, 0, len(wts))
	for _, wt := range wts {
		f, err := facts(wt)
		if err != nil {
			return err
		}
		held, err := probe(wt.Path)
		if err != nil {
			return err
		}
		on := byBranch[wt.Branch]
		if on == nil {
			on = []laneView{} // a no-lane worktree still appears, with [] lanes
		}
		views = append(views, worktreeView{
			Repo: wt.Repo, Branch: wt.Branch, Path: wt.Path,
			Commit: f.Commit, Dirty: f.Dirty, LockHeld: held,
			GateVerdict: f.GateVerdict, GateCommit: f.GateCommit, Lanes: on,
		})
	}

	if asJSON || !output.IsTTY() {
		return output.Emit(views)
	}
	if len(views) == 0 {
		fmt.Println("no worktrees")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BRANCH\tCOMMIT\tDIRTY\tLOCK\tGATE\tLANES")
	for _, v := range views {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			v.Branch, orDash(v.Commit), yesNo(v.Dirty), lockCell(v.LockHeld),
			orDash(v.GateVerdict), laneSummary(v.Lanes))
	}
	return w.Flush()
}

// gatherFacts is the per-worktree IO the status cockpit needs beyond the
// registry: HEAD, dirty, and the on-disk gate verdict. Injected so the join +
// reconcile projection is tested without git or a real gate.json.
type gatherFacts func(wt state.Worktree) (wtFacts, error)

// wtFacts are the gathered facts for one worktree.
type wtFacts struct {
	Commit      string // abbreviated HEAD
	Dirty       bool
	GateVerdict string // .bowt/gate.json overall ("" when never gated)
	GateCommit  string // the commit that verdict is for (abbreviated)
}

// realWorktreeFacts is the production gatherFacts: git HEAD/dirty (best-effort —
// a worktree whose checkout is gone still lists, just without commit/dirty) plus
// the on-disk gate verdict. A corrupt gate.json is surfaced; a missing one is the
// normal "never gated" case.
func realWorktreeFacts(wt state.Worktree) (wtFacts, error) {
	var f wtFacts
	if _, short, err := repo.Head(wt.Path); err == nil {
		f.Commit = short
	}
	if dirty, err := repo.Dirty(wt.Path); err == nil {
		f.Dirty = dirty
	}
	res, ok, err := gate.ReadResult(wt.Path)
	if err != nil {
		return f, err
	}
	if ok {
		f.GateVerdict = string(res.Overall)
		f.GateCommit = res.CommitShort
	}
	return f, nil
}

func lockCell(held bool) string {
	if held {
		return "held"
	}
	return "free"
}

// reviewCell renders the C/S/N triple, or "-" when all zero.
func reviewCell(v laneView) string {
	if v.ReviewBlockers == 0 && v.ReviewMajors == 0 && v.ReviewMinors == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d/%d", v.ReviewBlockers, v.ReviewMajors, v.ReviewMinors)
}

// laneFlags renders the comms/reconcile flags for a lane: E=escalated,
// P(<owner>)=paused, R=reconciled at this read. "-" when none.
func laneFlags(v laneView) string {
	var flags []string
	if v.Escalated {
		flags = append(flags, "E")
	}
	if v.PausedOn != "" {
		flags = append(flags, "P("+v.PausedOn+")")
	}
	if v.Reconciled {
		flags = append(flags, "R")
	}
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, ",")
}

// laneSummary renders the lanes on a worktree as "id:status" tokens for the
// status table's LANES cell.
func laneSummary(lanes []laneView) string {
	if len(lanes) == 0 {
		return "-"
	}
	toks := make([]string, 0, len(lanes))
	for _, l := range lanes {
		toks = append(toks, l.ID+":"+string(l.Status))
	}
	return strings.Join(toks, " ")
}
