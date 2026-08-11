# RFC: the `lane` abstraction

Status: draft · Branch: `rfc/lanes` · Foundation for PORTING.md §G (orchestration
layer). Lock this before G2.

## Problem

`spawn`, writeback, `status`, and `land` are facets of one missing noun: the
**lane** — a tracked unit of delegated work (PORTING.md:97-103). Today bowt can
*create* a lane (interactive `spawn`, `cmdSpawn` main.go:658) and *verify* one
(`gate` → `.bowt/gate.json`), but nothing *tracks* one: no record ties a brief +
its provenance to a worktree, a gate verdict, a review's findings, and a status
the orchestrator can query cold (after compaction). Mission-control does this
with `agents/<id>.md` + `-writeback.md` files on disk; this RFC makes that record
first-class and queryable so §G can build `land` (G1), headless `spawn` (G2),
writeback/comms (G3), and `status`/`lanes` (G4) on one shared shape.

The Notion pattern's load-bearing principle: **state is files on disk, so it
survives compaction** — the sub-agent and a cold orchestrator both read the same
files (the `subagent/PROMPT.md` → `writeback/*.md` handshake). Any lane store must
honor this, not replace it.

## Storage decision — table indexes files (both, split by role) · **DIRECT FIT**

Two existing on-disk mechanisms already carry lane state, and I build on both
rather than inventing a third:

- **Registry table** — `state.Store` over SQLite at `~/.bowt/state.db`
  (state.go:47-53), `worktrees` schema state.go:59-69, opened via `state.Open`
  by every command (main.go:122, :147, :167, :186). Migrations use the idempotent
  `CREATE TABLE IF NOT EXISTS` (state.go:103) + swallow-`duplicate column name`
  `ALTER` pattern proven by the `mode` column (state.go:77, :108-111, :117-119).
- **Per-worktree artifact files** — `gate` writes `<worktree>/.bowt/gate.json`
  atomically (gate.go:34-38, :238-272); `review` writes `<worktree>/.bowt-review/`
  (review.go:17); `spawn` reads `subagent/PROMPT.md` and the agent writes
  `subagent/writeback/*.md` (spawn.go:135, :141-164; prompts/plan.md, impl.md).

**Decision:** the lane **record** (the queryable index — status, provenance,
pointers, derived scalars) lives in a new **`lanes` table** in `~/.bowt/state.db`;
the lane's **content** (brief, PLAN/STATUS/VALIDATION, gate verdict, review
synthesis) stays as the **files that already exist** on disk. The table stores
*paths + hashes + derived scalars*, never the prose.

Why this split, not one or the other:

- **The two roles read through different channels.** The sub-agent is a coding
  agent — it reads/writes *files* (`PROMPT.md` in, `writeback/*.md` out); it does
  not query SQLite. The orchestrator's *cockpit* (G4) needs a cross-worktree
  query. Files serve the sub-agent handshake and compaction-proofness exactly as
  the Notion pattern requires; the table is the orchestrator's projection over
  them. Each reader keeps the channel it already uses.
- **It mirrors how `gate` already works.** `gate` writes a file (`gate.json`) and
  a *reader* projects it; the lane record is that reader, made durable. G4 is
  explicitly "the thin-projection a UI later just renders (never a second DB)"
  (PORTING.md:121). Storing prose in SQLite would *be* that second DB.
- **A cold orchestrator loses nothing.** `~/.bowt/state.db` is always reachable;
  the writeback files are always on disk. Rebuilding the table from files is
  possible in principle (the files are truth), so the DB is a cache with a
  rebuild path, never the sole copy.

**Rejected — a per-worktree `.bowt/lane.json` as the record.** It would duplicate
what `gate.json` + `writeback/*.md` already hold, creating a third copy to keep in
sync, and it cannot answer a cross-worktree cockpit query without a scan. The one
ergonomic it buys (`cat` the lane while standing in the worktree) is better served
by `bowt lane show <id>` emitting a read-only projection on demand. Not adopted.

So: **files-as-truth = DIRECT FIT** (gate.json/writeback convention, unchanged);
**lanes table = DIRECT FIT** (the `state.Store` seam + the `mode` idempotent-ALTER
migration, extended with lane methods).

## The schema

### Typed status enum — **DIRECT FIT** (house rule: typed constants)

CONTRIBUTING mandates typed constants for fixed value sets; the codebase already
does this three times — `gate.Status` (gate.go:45-51), `spawn.Mode`
(spawn.go:41-48), `state.Mode` (state.go:19-28). The lane status follows suit:

```go
// Status is a lane's lifecycle state. Typed constant per CONTRIBUTING; it
// serializes to the same JSON as a bare string.
type Status string

const (
    StatusPlanning   Status = "planning"    // agent running a plan pass
    StatusPlanReview Status = "plan-review" // PLAN.md written, awaiting orch triage
    StatusImpl       Status = "impl"        // agent running an impl pass
    StatusReview     Status = "review"      // review fan-out running / findings queued
    StatusPaused     Status = "paused"      // PAUSED ON <owner>: blocked on a human
    StatusDone       Status = "done"        // landed (ff-merged + cleaned up)
    StatusFailed     Status = "failed"      // gate/impl failed, not landable
)
```

### The `Lane` record

```go
// Lane is one tracked unit of delegated work. json tags define the agent-facing
// wire shape (as state.Worktree does, state.go:31-40); DB columns are internal.
type Lane struct {
    ID       string `json:"id"`       // stable lane id (ticket-slug or ULID); PK
    Ticket   string `json:"ticket"`   // human ticket ref, optional

    // Placement — (repo,branch) matches the worktrees PK (state.go:68).
    Repo     string `json:"repo"`
    Branch   string `json:"branch"`
    Worktree string `json:"worktree"` // absolute path; hook cwd + where files land

    Status   Status `json:"status"`

    // Provenance — parsed from spawn at create, NOT re-parsed from the file.
    Agent         string `json:"agent"`          // caps.Name (agent.go:36)
    Model         string `json:"model"`           // resolved model (spawn.go:208)
    PromptMode    string `json:"prompt_mode"`     // "plan" | "impl" (spawn.go:41-48)
    PromptVersion string `json:"prompt_version"`  // prompts/VERSION (spawn.go:76)
    PromptHash    string `json:"prompt_hash"`      // gitBlobHash of the wrapper (spawn.go:88,122)
    Attempt       int    `json:"attempt"`          // bumped by each followup re-spawn

    // Brief — the handshake input.
    BriefPath string `json:"brief_path"`  // e.g. subagent/PROMPT.md (spawn.go:135)
    BriefHash string `json:"brief_hash"`  // gitBlobHash(brief content) (spawn.go:88)

    // Waves / deps — dispatch ordering (PORTING.md:108).
    Wave int      `json:"wave"`
    Deps []string `json:"deps"` // lane ids that must be done first; JSON TEXT column

    // Writeback pointers — dir + which artifacts are present (content stays on disk).
    WritebackDir string `json:"writeback_dir"` // subagent/writeback
    LogPath      string `json:"log_path"`       // captured headless stdout (G2)

    // Gate — link to .bowt/gate.json, plus the indexed verdict scalar.
    GateVerdict string `json:"gate_verdict"` // "" | "pass" | "fail" (gate.Status, gate.go:45)
    GateCommit  string `json:"gate_commit"`  // Result.Commit the verdict is for (gate.go:78)
    GatePath    string `json:"gate_path"`     // <worktree>/.bowt/gate.json (gate.go:34-38)

    // Review — the C/S/N triple (review.Counts, validate.go:25-29).
    ReviewBlockers int `json:"review_blockers"`
    ReviewMajors   int `json:"review_majors"`
    ReviewMinors   int `json:"review_minors"`

    // Comms — escalation/pause surfaced by bowt, not grepped by the orchestrator.
    Escalated     bool   `json:"escalated"`      // an ESCALATE marker seen in PLAN.md
    EscalationNote string `json:"escalation_note"` // first line of the block
    PausedOn      string `json:"paused_on"`       // owner from "PAUSED ON <owner>"

    Created time.Time `json:"created"`
    Updated time.Time `json:"updated"`
}
```

### DDL + migration — **DIRECT FIT** (`mode`-column pattern)

Added to `OpenAt` beside the existing `schema`/`addModeColumn` execs
(state.go:103-111). New table via `CREATE TABLE IF NOT EXISTS`; any later column
via the same `ALTER` + `isDuplicateColumn` swallow (state.go:117-119).

```sql
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
    deps            TEXT NOT NULL DEFAULT '',   -- JSON array of lane ids
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
);
```

`(repo, branch)` is not the PK here (a branch may host successive lanes over
time; the lane id is stable across `bowt rm`). It matches `worktrees(repo,branch)`
(state.go:68) for the G4 join while the lane outlives the worktree row.

**`deps` as a JSON TEXT column — PLANNED EXTENSION.** Waves/deps are specified by
the plan (PORTING.md:108, ORCH_CHECKPOINT.md:41) but have no existing
representation to adopt — the current schema is all flat scalars (state.go:59-69).
A JSON list in one column is the minimal extension. A normalized `lane_deps` join
table is rejected as over-engineered for a single-user tool with N < ~20 lanes;
if cross-lane queries ever need it, the migration path is additive.

### Store seam — **DIRECT FIT**

Lane persistence is exposed through a consumer-defined interface backed by the
same `*sqlStore`/`*sql.DB`, mirroring `state.Store` (state.go:47-53) and obeying
CONTRIBUTING's "small interfaces, one impl + a fake" rule. Kept *separate* from
`state.Store` so existing fakes needn't grow lane methods:

```go
type LaneStore interface {
    AddLane(l Lane) error
    GetLane(id string) (Lane, bool, error)
    ListLanes(repo string) ([]Lane, error) // G4 cockpit query
    UpdateLane(l Lane) error               // status transitions, followup bumps
}
```

## Writeback / comms protocol (G3)

The **file handshake is the source of truth**; the lane record **indexes** it.
bowt does the parse once at capture and stores scalars, so the orchestrator reads
a field instead of grepping prose ("escalations surfaced not grepped",
PORTING.md:119).

| On-disk file (truth) | Written by | Lane fields it derives (stored, not the prose) |
|---|---|---|
| `subagent/PROMPT.md` (+ appended `FOLLOWUP.md`) | orchestrator | `brief_path`, `brief_hash` (spawn.go:135, :141-164) |
| provenance line at prompt top | `spawn.Assemble` | `agent`, `prompt_mode`, `prompt_version`, `prompt_hash` (spawn.go:96-123) |
| `writeback/VALIDATION.md` | plan agent | presence → status past `planning` |
| `writeback/PLAN.md` | plan agent | presence → `plan-review`; `ESCALATE` marker → `escalated` + `escalation_note` (prompts/plan.md step 3) |
| `writeback/STATUS.md` | impl agent | presence → `review`/`done`; `PAUSED ON <owner>` → `paused_on` + `status=paused` (PORTING.md:126) |
| `.bowt/gate.json` | `gate.Run` | `gate_verdict`, `gate_commit`, `gate_path` (gate.go:71-85) |
| `.bowt-review/SYNTHESIS.md` | `review.Runner` | `review_blockers/majors/minors` via `review.Counts` (validate.go:25-29, :107-120) |

- **Escalations / questions.** The plan wrapper tells the agent to write the
  problem into `PLAN.md` and "mark it ESCALATE" (prompts/plan.md step 3). At
  capture bowt scans for that marker and sets `escalated=true` + `escalation_note`;
  the lane stays in its phase (the *orchestrator* triages) but the cockpit flags
  it. This is the only grep, done once, by bowt.
- **PAUSED.** A `PAUSED ON <owner>` marker (owner-only stop-and-wait,
  PORTING.md:126) sets `status=paused` + `paused_on`; nothing auto-resumes.
- **followup bumps provenance — DIRECT FIT.** `bowt lane followup <id>` writes
  `subagent/FOLLOWUP.md`, which `spawn.ResolveBrief` already auto-appends
  (spawn.go:157-162), then re-spawns. Re-running `spawn.Assemble` recomputes the
  provenance line (spawn.go:113-130); bowt updates `prompt_version`/`prompt_hash`
  and increments `attempt`, and resets `status` to `impl`/`planning`. The prior
  writeback files are the agent's context (spawn.go:161).

## Lifecycle

```
                 bowt spawn (headless, G2)
                 └─ INSERT lane {status: planning|impl, provenance, brief_hash, wave, deps}
planning ──PLAN.md──▶ plan-review ──followup --impl──▶ impl ──review.Run──▶ review
   │                      │                              │                    │
   └──ESCALATE flag───────┴────PAUSED ON <owner>─────────┴──────▶ paused      │
                                                                              ▼
                                        bowt land <branch> (G1): gate.json==pass
                                        & clean & FF ──▶ ff-merge + rm ──▶ done
                                        (gate fail / non-FF / dirty)   ──▶ failed
```

- **Create (G2).** Headless `spawn`: resolve brief (spawn.go:141), `Assemble`
  (spawn.go:109), acquire the EXCLUSIVE per-worktree lock (`lock.Acquire`,
  main.go:713; lock.go:25), run the agent non-interactively + backgrounded,
  capturing stdout to `log_path`, and `AddLane`. Today `Session` blocks with
  inherited stdio (main.go:722-729, agent.go:70-73); making it headless +
  backgrounded is a **PLANNED EXTENSION** of the `Opts` stream seam
  (agent.go:56-62 already wires Stdout/Stderr — redirect them to `log_path`) plus
  process supervision. That supervision is a G2 impl concern; **the schema is
  agnostic to how the agent runs**, so it is out of scope here beyond recording
  the create/complete transitions.
- **Transitions** are driven by the command that performs each step (spawn,
  followup, review, land); each does one `UpdateLane`. Presence of writeback
  artifacts + gate/review results are the evidence, never a second source.
- **Close (G1 `land`).** `bowt land <branch>` reads `.bowt/gate.json`
  (gate.go:34-38), refuses if `overall != pass` / dirty / non-FF ("never merge an
  ungated lane" — would have prevented the cftunnel half-merge, PORTING.md:112),
  ff-merges, then `bowt rm` + branch delete, then `UpdateLane status=done`. G1
  ships **before** the schema by reading `gate.json` directly; when the table
  exists it also closes the row.

## Per-phase read/write map (build order)

| Phase | Reads | Writes |
|---|---|---|
| **G1 `land`** | `.bowt/gate.json` (gate.go:34-38); `worktrees` (state.go:121); lock (lock.go:25) | git ff-merge + `bowt rm`; *optionally* `lanes.status=done` if the row exists. **Schema-independent — start now.** |
| **G2 headless spawn + lane object** | brief files (spawn.go:141); `worktrees`; lock | **INSERT** the `Lane` (id, ticket, repo/branch/worktree, agent/model, prompt_* + brief_hash, wave/deps, `status=planning\|impl`, log_path); on exit **UPDATE** status + writeback pointers. The keystone. |
| **G3 writeback/comms** | `writeback/*.md`, `.bowt/gate.json`, `.bowt-review/SYNTHESIS.md` (validate.go:107) | **UPDATE** status transitions, `escalated`/`escalation_note`, `paused_on`, `review_*`; `followup` writes `FOLLOWUP.md` + bumps `prompt_*`/`attempt`. |
| **G4 `status`/`lanes`** | `ListLanes` + `worktrees` join + live lock holder (lock.go) + `gate.json` | **nothing** — read-only `--json` projection. |

Spine confirmed: **schema → G2 → {G3, G4}**; G1 independent (PORTING.md:128).

## Non-goals

- **Not a second DB.** Writeback/gate/review prose stays in files; the table holds
  paths + scalars only (PORTING.md:121).
- **Not a scheduler.** Waves/deps are *recorded*; the orchestrator decides
  dispatch. bowt stores `wave`/`deps`, it does not run a queue.
- **Not the signal transport.** `_bowt_signal` stays deferred (PORTING.md:58);
  lane state is pull (read files/DB), not push.
- **Not replacing files as truth.** The DB is a rebuildable projection; the
  sub-agent handshake remains files on disk, compaction-proof.
- **Not G2's execution mechanism.** How the agent is backgrounded/supervised is a
  separate G2 impl RFC; this doc fixes only the record it reads and writes.
