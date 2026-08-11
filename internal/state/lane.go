package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Status is a lane's lifecycle state. Typed constant per CONTRIBUTING (the same
// house rule that gives gate.Status / spawn.Mode / state.Mode their typed
// values); it serializes to the same JSON as a bare string.
type Status string

const (
	// StatusPlanning is a lane whose agent is running a plan pass.
	StatusPlanning Status = "planning"
	// StatusPlanReview means PLAN.md was written and the orchestrator must triage.
	StatusPlanReview Status = "plan-review"
	// StatusImpl is a lane whose agent is running an implementation pass.
	StatusImpl Status = "impl"
	// StatusReview means the review fan-out is running / findings are queued.
	StatusReview Status = "review"
	// StatusPaused is a lane blocked on a human ("PAUSED ON <owner>").
	StatusPaused Status = "paused"
	// StatusDone is a landed lane (ff-merged + cleaned up).
	StatusDone Status = "done"
	// StatusFailed is a gate/impl failure — not landable.
	StatusFailed Status = "failed"
)

// Lane is one tracked unit of delegated work. The json tags define the
// agent-facing wire shape (as Worktree does); the DB columns are internal. The
// table stores paths + hashes + derived scalars — never the prose, which stays
// in the writeback/gate/review files on disk (rfc/lanes.md "table indexes files").
type Lane struct {
	ID     string `json:"id"`     // stable lane id (ticket-slug or ULID); PK
	Ticket string `json:"ticket"` // human ticket ref, optional

	// Placement — (repo,branch) matches the worktrees PK for the G4 join.
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"` // absolute path; hook cwd + where files land

	Status Status `json:"status"`

	// Provenance — parsed from spawn at create, NOT re-parsed from the file.
	Agent         string `json:"agent"`          // caps.Name
	Model         string `json:"model"`          // resolved model
	PromptMode    string `json:"prompt_mode"`    // "plan" | "impl"
	PromptVersion string `json:"prompt_version"` // prompts/VERSION
	PromptHash    string `json:"prompt_hash"`    // gitBlobHash of the wrapper
	Attempt       int    `json:"attempt"`        // bumped by each followup re-spawn

	// Brief — the handshake input.
	BriefPath string `json:"brief_path"` // e.g. subagent/PROMPT.md
	BriefHash string `json:"brief_hash"` // gitBlobHash(brief content)

	// Waves / deps — dispatch ordering; recorded, not scheduled.
	Wave int      `json:"wave"`
	Deps []string `json:"deps"` // lane ids that must be done first; JSON TEXT column

	// Writeback pointers — dir + captured log (content stays on disk).
	WritebackDir string `json:"writeback_dir"` // subagent/writeback
	LogPath      string `json:"log_path"`      // captured headless stdout (G2)

	// Gate — link to .bowt/gate.json plus the indexed verdict scalar.
	GateVerdict string `json:"gate_verdict"` // "" | "pass" | "fail"
	GateCommit  string `json:"gate_commit"`  // Result.Commit the verdict is for
	GatePath    string `json:"gate_path"`    // <worktree>/.bowt/gate.json

	// Review — the C/S/N triple.
	ReviewBlockers int `json:"review_blockers"`
	ReviewMajors   int `json:"review_majors"`
	ReviewMinors   int `json:"review_minors"`

	// Comms — escalation/pause surfaced by bowt, not grepped by the orchestrator.
	Escalated      bool   `json:"escalated"`       // an ESCALATE marker seen in PLAN.md
	EscalationNote string `json:"escalation_note"` // first line of the block
	PausedOn       string `json:"paused_on"`       // owner from "PAUSED ON <owner>"

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// LaneStore is the lane persistence seam — a consumer-defined interface backed
// by the same *sqlStore as Store, kept SEPARATE so existing state.Store fakes
// need not grow lane methods (CONTRIBUTING: small interfaces, one impl + a fake).
type LaneStore interface {
	AddLane(l Lane) error
	GetLane(id string) (Lane, bool, error)
	ListLanes(repo string) ([]Lane, error) // G4 cockpit query
	UpdateLane(l Lane) error               // status transitions, followup bumps
}

// ErrLaneNotFound is returned by UpdateLane when the id has no row.
var ErrLaneNotFound = errors.New("lane not found")

const laneSchema = `
CREATE TABLE IF NOT EXISTS lanes (
	id              TEXT PRIMARY KEY,
	ticket          TEXT NOT NULL DEFAULT '',
	repo            TEXT NOT NULL,
	branch          TEXT NOT NULL,
	worktree        TEXT NOT NULL,
	status          TEXT NOT NULL DEFAULT 'planning',
	agent           TEXT NOT NULL DEFAULT '',
	model           TEXT NOT NULL DEFAULT '',
	prompt_mode     TEXT NOT NULL DEFAULT '',
	prompt_version  TEXT NOT NULL DEFAULT '',
	prompt_hash     TEXT NOT NULL DEFAULT '',
	attempt         INTEGER NOT NULL DEFAULT 0,
	brief_path      TEXT NOT NULL DEFAULT '',
	brief_hash      TEXT NOT NULL DEFAULT '',
	wave            INTEGER NOT NULL DEFAULT 0,
	deps            TEXT NOT NULL DEFAULT '',
	writeback_dir   TEXT NOT NULL DEFAULT '',
	log_path        TEXT NOT NULL DEFAULT '',
	gate_verdict    TEXT NOT NULL DEFAULT '',
	gate_commit     TEXT NOT NULL DEFAULT '',
	gate_path       TEXT NOT NULL DEFAULT '',
	review_blockers INTEGER NOT NULL DEFAULT 0,
	review_majors   INTEGER NOT NULL DEFAULT 0,
	review_minors   INTEGER NOT NULL DEFAULT 0,
	escalated       INTEGER NOT NULL DEFAULT 0,
	escalation_note TEXT NOT NULL DEFAULT '',
	paused_on       TEXT NOT NULL DEFAULT '',
	created         TEXT NOT NULL,
	updated         TEXT NOT NULL
);`

// laneColumns is the canonical column order shared by INSERT/UPDATE/SELECT so
// the three stay in lockstep.
const laneColumns = `id, ticket, repo, branch, worktree, status, agent, model, ` +
	`prompt_mode, prompt_version, prompt_hash, attempt, brief_path, brief_hash, ` +
	`wave, deps, writeback_dir, log_path, gate_verdict, gate_commit, gate_path, ` +
	`review_blockers, review_majors, review_minors, escalated, escalation_note, ` +
	`paused_on, created, updated`

// OpenLanes returns the LaneStore view of the default store at ~/.bowt/state.db.
func OpenLanes() (LaneStore, error) {
	path, err := defaultPath()
	if err != nil {
		return nil, err
	}
	return OpenLanesAt(path)
}

// OpenLanesAt opens the LaneStore view at an explicit path — used by tests with
// a temp file. It shares open() with OpenAt, so the same file backs both views.
func OpenLanesAt(path string) (LaneStore, error) {
	return open(path)
}

func (s *sqlStore) AddLane(l Lane) error {
	now := time.Now().UTC()
	if l.Created.IsZero() {
		l.Created = now
	}
	if l.Updated.IsZero() {
		l.Updated = now
	}
	_, err := s.db.Exec(
		`INSERT INTO lanes (`+laneColumns+`) VALUES `+
			`(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.ID, l.Ticket, l.Repo, l.Branch, l.Worktree, string(l.Status), l.Agent, l.Model,
		l.PromptMode, l.PromptVersion, l.PromptHash, l.Attempt, l.BriefPath, l.BriefHash,
		l.Wave, marshalDeps(l.Deps), l.WritebackDir, l.LogPath, l.GateVerdict, l.GateCommit, l.GatePath,
		l.ReviewBlockers, l.ReviewMajors, l.ReviewMinors, boolToInt(l.Escalated), l.EscalationNote,
		l.PausedOn, l.Created.Format(time.RFC3339Nano), l.Updated.Format(time.RFC3339Nano))
	return err
}

func (s *sqlStore) GetLane(id string) (Lane, bool, error) {
	row := s.db.QueryRow(`SELECT `+laneColumns+` FROM lanes WHERE id=?`, id)
	l, err := scanLane(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Lane{}, false, nil
	}
	if err != nil {
		return Lane{}, false, err
	}
	return l, true, nil
}

func (s *sqlStore) ListLanes(repo string) ([]Lane, error) {
	rows, err := s.db.Query(`SELECT `+laneColumns+` FROM lanes WHERE repo=? ORDER BY created`, repo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Lane
	for rows.Next() {
		l, err := scanLane(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateLane rewrites every mutable column for an existing lane (the whole
// record is the unit of update — each command reads a Lane, mutates fields,
// writes it back). It bumps Updated and refuses an unknown id with
// ErrLaneNotFound rather than silently inserting.
func (s *sqlStore) UpdateLane(l Lane) error {
	l.Updated = time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE lanes SET ticket=?, repo=?, branch=?, worktree=?, status=?, agent=?, model=?, `+
			`prompt_mode=?, prompt_version=?, prompt_hash=?, attempt=?, brief_path=?, brief_hash=?, `+
			`wave=?, deps=?, writeback_dir=?, log_path=?, gate_verdict=?, gate_commit=?, gate_path=?, `+
			`review_blockers=?, review_majors=?, review_minors=?, escalated=?, escalation_note=?, `+
			`paused_on=?, updated=? WHERE id=?`,
		l.Ticket, l.Repo, l.Branch, l.Worktree, string(l.Status), l.Agent, l.Model,
		l.PromptMode, l.PromptVersion, l.PromptHash, l.Attempt, l.BriefPath, l.BriefHash,
		l.Wave, marshalDeps(l.Deps), l.WritebackDir, l.LogPath, l.GateVerdict, l.GateCommit, l.GatePath,
		l.ReviewBlockers, l.ReviewMajors, l.ReviewMinors, boolToInt(l.Escalated), l.EscalationNote,
		l.PausedOn, l.Updated.Format(time.RFC3339Nano), l.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("update lane %q: %w", l.ID, ErrLaneNotFound)
	}
	return nil
}

func scanLane(sc scanner) (Lane, error) {
	var l Lane
	var status, deps, created, updated string
	var escalated int
	if err := sc.Scan(
		&l.ID, &l.Ticket, &l.Repo, &l.Branch, &l.Worktree, &status, &l.Agent, &l.Model,
		&l.PromptMode, &l.PromptVersion, &l.PromptHash, &l.Attempt, &l.BriefPath, &l.BriefHash,
		&l.Wave, &deps, &l.WritebackDir, &l.LogPath, &l.GateVerdict, &l.GateCommit, &l.GatePath,
		&l.ReviewBlockers, &l.ReviewMajors, &l.ReviewMinors, &escalated, &l.EscalationNote,
		&l.PausedOn, &created, &updated,
	); err != nil {
		return Lane{}, err
	}
	l.Status = Status(status)
	depsList, err := unmarshalDeps(deps)
	if err != nil {
		return Lane{}, err
	}
	l.Deps = depsList
	l.Escalated = escalated != 0
	l.Created, _ = time.Parse(time.RFC3339Nano, created)
	l.Updated, _ = time.Parse(time.RFC3339Nano, updated)
	return l, nil
}

// marshalDeps stores a dep list as a JSON array, with the empty list persisted
// as "" (the column DEFAULT) so an unset value round-trips back to nil.
func marshalDeps(deps []string) string {
	if len(deps) == 0 {
		return ""
	}
	b, _ := json.Marshal(deps) // []string always marshals
	return string(b)
}

// unmarshalDeps inverts marshalDeps: "" is nil, otherwise a JSON array.
func unmarshalDeps(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("decode lane deps %q: %w", s, err)
	}
	return out, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
