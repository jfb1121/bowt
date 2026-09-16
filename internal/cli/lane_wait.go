package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/state"
)

// newLaneWaitCmd registers `bowt lane wait <id>...`: block until the named lane(s)
// leave the in-flight phase, then print their settled rows. This is the
// orchestrator notify primitive (dispatch → wait → gate → land).
func newLaneWaitCmd() *cobra.Command {
	var timeout, interval time.Duration
	var asJSON bool
	c := &cobra.Command{
		Use:   "wait <id>... [--timeout <dur>] [--interval <dur>]",
		Short: "block until the named lane(s) settle, then print their rows",
		Long: `Block until every named lane leaves the in-flight phase (planning/impl) — i.e.
until its detached supervisor has written a settled status (plan-review, review,
paused, done, or failed) — then print the settled rows.

Each poll reconciles the lane at read time exactly as 'bowt lanes' does: an
in-flight lane whose worktree lock is free (its supervisor was SIGKILLed before
writing the terminal status) is repaired from its writeback files and self-healed,
so wait can never hang on a dead supervisor. Progress goes to stderr; the JSON
result goes to stdout, so a script can capture the rows and be woken on exit.

--timeout (default 0 = wait indefinitely) elapsing with any lane still in-flight
prints the rows and returns a non-zero error naming the timed-out lane(s). A
human table is printed at a terminal; JSON is emitted otherwise (agent-first) or
with --json.`,
		Example: `  bowt lane wait my-lane-ab12
  bowt lane wait lane-a lane-b --timeout 30m
  bowt spawn --headless … ; bowt lane wait my-lane-ab12 --json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ls, err := state.OpenLanes()
			if err != nil {
				return err
			}
			deps := laneWaitDeps{ls: ls, probe: lock.Probe, now: time.Now, sleep: time.Sleep}
			return cmdLaneWait(deps, args, timeout, interval, asJSON)
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long, non-zero exit (0 = wait indefinitely)")
	c.Flags().DurationVar(&interval, "interval", 2*time.Second, "poll interval")
	c.Flags().BoolVar(&asJSON, "json", false, "force JSON output")
	return c
}

// laneWaitDeps bundles the injectable seams for runLaneWait so the poll loop is
// deterministic and instant under test: the lane store, the lock probe, and a
// clock/sleep pair. The cobra RunE wires the real state.OpenLanes(), lock.Probe,
// time.Now, and time.Sleep; tests fake the store + probe and step a virtual clock.
type laneWaitDeps struct {
	ls    state.LaneStore
	probe func(string) (bool, error)
	now   func() time.Time
	sleep func(time.Duration)
}

// runLaneWait is the testable core of `bowt lane wait`. It resolves each id up
// front (an unknown id is a hard error BEFORE any waiting), then polls every
// interval: each poll re-reads each still-pending lane and reconciles it via the
// shared reconcileForRead (persisting a correction — self-heal — exactly as the
// cockpit does), so an in-flight lane whose supervisor died settles from files and
// the wait can't hang. A lane drops out once isInFlight is false. It returns the
// latest reconciled row for every requested lane (in input order) plus, if a
// non-zero timeout elapsed with lanes still in-flight, an error naming them.
func runLaneWait(deps laneWaitDeps, ids []string, timeout, interval time.Duration) ([]laneView, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("interval must be positive, got %s", interval)
	}
	// Resolve every id first; a genuinely unknown id fails before we ever block.
	pending := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue // dedup repeats so we don't double-report a lane
		}
		if _, ok, err := deps.ls.GetLane(id); err != nil {
			return nil, err
		} else if !ok {
			return nil, fmt.Errorf("unknown lane %q", id)
		}
		seen[id] = true
		pending = append(pending, id)
	}
	order := append([]string(nil), pending...)

	deadline := deps.now().Add(timeout) // only consulted when timeout > 0
	views := make(map[string]laneView, len(order))
	for {
		stillPending := pending[:0]
		for _, id := range pending {
			lane, ok, err := deps.ls.GetLane(id)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("unknown lane %q", id)
			}
			fixed, changed, held, err := reconcileForRead(lane, deps.probe)
			if err != nil {
				return nil, err
			}
			if changed {
				if err := deps.ls.UpdateLane(fixed); err != nil {
					return nil, err
				}
			}
			views[id] = toLaneView(fixed, changed, held)
			if isInFlight(fixed.Status) {
				stillPending = append(stillPending, id)
			}
		}
		pending = stillPending
		if len(pending) == 0 {
			break // all settled
		}
		if timeout > 0 && !deps.now().Before(deadline) {
			break // timed out with lanes still in-flight
		}
		output.Errf("waiting on %d lane(s)…", len(pending))
		deps.sleep(interval)
	}

	rows := make([]laneView, 0, len(order))
	for _, id := range order {
		rows = append(rows, views[id])
	}
	if len(pending) > 0 {
		return rows, fmt.Errorf("timed out after %s waiting on lane(s): %s",
			timeout, strings.Join(pending, ", "))
	}
	return rows, nil
}

// cmdLaneWait runs the wait core, then prints the resulting rows (mirroring
// `bowt lanes`) whether it settled or timed out, and finally returns the wait
// error so a timeout exits non-zero for a script to react to.
func cmdLaneWait(deps laneWaitDeps, ids []string, timeout, interval time.Duration, asJSON bool) error {
	rows, waitErr := runLaneWait(deps, ids, timeout, interval)
	if rows != nil {
		if err := emitLaneViews(rows, asJSON); err != nil {
			return err
		}
	}
	return waitErr
}
