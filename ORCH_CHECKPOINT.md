# bowt — orchestrator checkpoint

> **FIRST ACTION AFTER COMPACTION:** read this file + `PORTING.md`. You are the
> **orchestrator** building `bowt` (a Go rewrite of `twig`). You do NOT hand-write
> impl code — you write briefs, spawn lanes (via the Agent tool, since `bowt
> spawn` is interactive-only today), gate what comes back (independent `make
> check` + a functional run), then merge FF + `bowt rm` + delete branch + tick the
> ledger. Personal git identity, **no Co-Authored-By/Claude attribution**. Push
> only to the repos' own `jfb1121` (bowt) / `jfb1121` (twig-configs) origins.
> **Current next action: build Section G (orchestration layer) — start with G1
> `bowt land`, and lock the `lane` schema before G2.**

## What bowt is / where
- Repo: `~/jfb1121/bowt` → private `github.com/jfb1121/bowt` (module path too). On `PATH` as `bowt` (`~/.local/bin/bowt` → `go install` binary; `make install` to refresh).
- Config/driver repo: `~/qp/jt26/twig-configs` → private `jfb1121/twig-configs` (per-repo config for both twig AND bowt; QP git identity there).
- Ledger: `PORTING.md`. Tooling gate: `make check` (fmt-check + vet + golangci-lint + `go test -race`), enforced by CI (`.github/workflows/ci.yml`).

## State (done + merged to bowt main)
Full command surface: `new ls rm path cd exec root spawn gate review doctor completion shell-init version` + per-repo extensions. SQLite registry, flock lock, config/env/hooks, `--code-only`, cobra + dynamic completion, `spawn` (versioned prompts + provenance), `gate` (machine-readable `.bowt/gate.json`), agent adapters (provider-neutral: `--agent`, session+oneshot, `{{MEMORY_FILE}}`), `review` (validation postconditions). **bowt is self-hosting** (creates/removes its own lane worktrees).

## Deployed: gen2-be runs on bowt (2026-08-11)
- 117 worktrees migrated into `~/.bowt/state.db`.
- `~/Graswald/gen2-be/.bowt` → `twig-configs/gen2-be/bowt/` (symlink, locally excluded). 14 twig extensions unwrapped to invokable scripts (incl. cftunnel) + `gate.sh` + Django `review-perspectives` symlink. `bowt manage --version` verified end-to-end.
- Agents told to prefer bowt via `~/.claude/CLAUDE.md` + the SessionStart hook aliases `bowt`. twig kept as fallback.

## The pattern (Notion — "this is the pattern")
- Main: https://app.notion.com/p/3828033ab5c98149bfdcd0fd4841c918 (Orchestrator→Sub-Agent)
- Scale case study (16 tickets, waves, plan+review gates): https://app.notion.com/p/3838033ab5c981fb82b7d111dc27874f
- Autonomous protocol (standing orders, multi-angle review, LOCAL_VERIFY, stop-and-wait): https://app.notion.com/p/3988033ab5c9813d8d5be6b2b36e9f22
- Essence: two roles (orchestrator never codes; fresh sub-agent per ticket); the `subagent/` handshake is **files on disk** (brief → writeback) so it survives compaction; a checkpoint doc with RESUME BRIEF; a plan gate (STOP-AFTER-PLAN, validate before building) + a review gate (multi-angle, never auto-fix, orch triages); waves/deps (dispatch only unblocked, parallelize independent).

## Section G — orchestration layer (the plan; details in PORTING.md §G)
The realization: `spawn` / writeback / `status` / `land` are all facets of ONE missing abstraction — the **`lane`** (a tracked unit of delegated work). This encodes the Notion pattern into bowt.
- **G1 `bowt land <branch>`** — gate → ff-merge → cleanup; refuse if ungated/dirty/non-FF. *Quick win, independent, start first.* (Would have prevented the cftunnel half-merge.)
- **G2 `lane` object + headless `spawn`** — the keystone. Define the lane record; make `spawn` run the agent non-interactively, background it, capture writeback + record state. Gap #1.
- **G3 writeback/comms first-class** — structured protocol on the lane (status/escalate/result/followup); `bowt lane followup`; escalations surfaced not grepped. (The user's insight.)
- **G4 `bowt status`/`lanes`** — cockpit: per-worktree lock holder + last gate verdict + dirty + lane status, `--json`.
- Spine: **lane schema → G2 → {G3, G4}**; G1 independent.

### `lane` schema (draft — informed by the Notion pattern; lock before G2)
`{ id, ticket, brief_path + hash, prompt_version, agent + model, worktree, branch, status (planning|plan-review|impl|review|paused|done|failed), wave/deps, writeback_artifacts (VALIDATION/PLAN/STATUS paths), gate_verdict (from .bowt/gate.json), review_findings (C/S/N), escalations/questions, provenance }`. Store: a `lanes` table in `~/.bowt/state.db` (or per-worktree `.bowt/lane.json`). Mirrors mission-control's `agents/<id>.md` + `-writeback.md`, made queryable.

## Standing conventions
- Gate every lane independently (never trust its "green"); merge FF only when green.
- No Claude attribution in commits (bowt: personal `pjayeshbafna08@gmail.com`; twig-configs: QP `pjayeshbafna.qp@gmail.com`).
- Perspectives/config live in twig-configs, NOT ported into bowt.
- Follow-ups tracked inline in PORTING.md (lock `status/release/--force`; `doctor` full + non-zero-on-fail; review `--model`/target-worktrees/layer-routing; `init`/`setup`/`sync`/`refresh`/`status`; statusLine; `prune`).

## In-flight / next
- No lanes currently running. **Next:** start G1 (`bowt land`) lane + finalize the `lane` schema (RFC `rfc/lanes.md`), then G2.
