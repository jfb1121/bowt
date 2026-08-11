# bowt — full port ledger (twig bash → Go)

Tracks everything that must exist for bowt to fully replace the bash `twig`
(core `twig.sh`/`twig.zsh`, the env/config substrate, agent integration, the
extension system) **plus** the five planned RFC items. Status keys:
`[x]` done · `[~]` partial · `[ ]` pending · `[?]` reconsider/obsolete.

> Snapshot: **core complete + self-hosting.** Landed: worktree CRUD, SQLite
> registry, flock lock, config/env/hooks, `--code-only`, cobra + dynamic
> completion, `spawn` (versioned prompts + provenance), `gate`, agent adapters
> (provider-neutral), `review` (with the validation postconditions), and the
> per-repo extension loader. bowt manages its own worktrees; twig is out of the
> loop.
>
> **Deployed (2026-08-11): gen2-be runs on bowt.** 117 worktrees migrated into
> the bowt registry; `~/Graswald/gen2-be/.bowt` → `twig-configs/gen2-be/bowt/`
> (13 extensions migrated to invokable scripts + `gate.sh` + Django perspectives);
> `bowt manage --version` verified end-to-end; agents told to prefer bowt
> (`~/.claude/CLAUDE.md` + SessionStart hook). cftunnel migration in flight.
>
> **Remaining is the tail:** `init`/`setup`/`sync`/`refresh`/`status`,
> statusLine + docs-injection, `bowt new-extension`, and the polish follow-ups
> noted inline (lock `status/release/--force`, `doctor` completeness, review
> `--model`/target-worktrees/layer-routing, `.git/info/exclude`, `.claude` copy).

## A. Core commands (twig.sh dispatch)

- [x] `new <branch> [-b base]` — create + register (twig's branch fallback)
- [x] `ls [--json]` — list (JSON-first)
- [x] `rm <branch>` — remove + deregister  *(teardown.sh hook still pending — see B)*
- [x] `path <branch>` — print worktree path (bowt-native; agent-first)
- [x] `exec <branch> -- <cmd>` — runs a command in the worktree dir with the per-worktree env (BOWT_*/GWT_* + config vars) injected (slice 3a + config slice)
- [x] `cd`/`main`/`root` + `shell-init [bash|zsh]` shim — interactive navigation (slice 3a)
- [ ] `init [template]` — scaffold `.twig`/`.bowt`, docs, statusLine
- [ ] `setup [branch]` — run pre-setup.sh + setup.sh
- [ ] `sync` — stash → fetch → rebase main → setup → pop
- [~] `doctor [--agent X]` — `--agent X` provider smoke-check landed (via adapters); **pending:** full deps/ports/registry checks, and exit non-zero when a check fails (currently exits 0)
- [x] `help` — cobra per-command help/usage/flags + "did you mean" (dispatch migrated to cobra)
- [ ] `status` (`show`/`add`/`rm`/`init`) — statusLine items (Claude-specific)
- [ ] `refresh` — regenerate status + docs across all worktrees
- [ ] `claude` → **`agent`** — launch an agent with a prompt (becomes adapter-driven; keep `claude` alias)
- [?] `update` — bash did `git pull` of twig itself; replace with `brew upgrade bowt` / self-update

## B. Environment & config substrate

- [x] registry store — **SQLite** behind the `Store` interface (slice 2; was JSON in slice 1, swap touched no caller)
- [x] offset allocation (monotonic-ish, `max+1`)
- [~] port allocation — computes `base+offset` with `base` now from config (`BOWT_PORT_BASE`/`GWT_PORT_BASE`); **still missing** the `lsof` in-use probe and `GWT_PORT_STRIDE`
- [x] `.twig/config` loading — bash-source via the `run.Runner` seam, env-diffed against a clean baseline (`internal/config`); resolves `.bowt/` then `.twig/`
- [x] env export contract — `BOWT_PORT/OFFSET/BRANCH/MAIN_REPO/REPO_NAME/CODE_ONLY` with `GWT_*` + `TWIG_REPO_NAME` + `AUTOENV_ASSUME_YES` back-compat aliases (`internal/env`), injected into hooks and `exec`
- [x] `--code-only` mode — `bowt new --code-only`; typed `Mode` enum in a `mode` registry column (idempotent `ALTER` migration; legacy rows → `full`); drives `BOWT_CODE_ONLY`/`GWT_CODE_ONLY` (0/1) into hooks+`exec`; `ls` shows MODE
- [x] lifecycle hooks — invoke `pre-setup.sh` / `setup.sh` / `teardown.sh` with the `$path $branch $offset $port` contract via the Runner (`internal/hook`); pre-setup fatal, setup/teardown warn
- [ ] `.git/info/exclude` management (add `.twig/`|`.bowt/`)
- [ ] copy agent config dir (`.claude/`) into each new worktree  *(ties to D + adapters)*

## C. Agent / Claude integration

- [?] `_bowt_signal` — **DEFER (not now; maybe later).** HTTP POST transport whose only consumer was the abandoned dashboard's `/api/twig/signal` → unbounded `signals` table (a DB-heaviness culprit). No listener in terminal-only bowt, so not ported now. If revived later, emit JSON lines to stdout / a bounded local log — never POST-to-DB.
- [ ] statusLine — generate `.twig-status`, install into `~/.claude/settings.json`, port `statusline.sh` renderer (Claude-specific, adapter-gated)
- [ ] docs injection on `init` — `twig-docs.md` / `twig-extensions.md` → `.claude/` + `CLAUDE.md` `@`-refs
- [x] shell completion — cobra-generated `bowt completion bash|zsh|fish|powershell`, with **dynamic completion** of registered worktree names for `cd`/`rm`/`path`/`exec`; loaded (alongside the `cd` shim) via `shell-init`

## D. Extension system  *(port the MECHANISM, not the 18 scripts)*

- [x] per-repo extension loader — resolve + invoke `<configDir>/extensions/<cmd>.sh` as a subprocess (inherited stdio, propagated exit code); built-ins always win
- [ ] native extension tier — twig-shipped generic commands as **compiled subcommands** (RFC item 5) — bowt's own commands already cover "native"; a discoverable third tier is deferred
- [x] resolution + precedence — per-repo extension runs only when the word is not a built-in; `bowt extensions` lists the repo's extensions (native tier deferred, so no cross-source precedence yet)
- [x] extension manifest — header fields (`bowt-lock: none|shared|exclusive` enforced around the run; `bowt-desc`); unknown keys (e.g. `bowt-scope`) ignored
- [x] helper lib — `bowt lib` prints a sourceable `bowt.lib.sh` (`bowt_log`/`bowt_err`); `$BOWT_LIB` points the script at it (richer `bowt_signal`/`bowt_gate_record`/`bowt_status_add` callbacks deferred)
- [ ] `bowt new-extension <name>` — scaffold a manifest+helper skeleton

**Existing extensions migrate to the contract (NOT bowt's to reimplement):**
gen2-be — `aws cftunnel gos grafana lint manage review shell test test-log tunnel vite web webhook worker`;
bowt-app — `run test tsc`. Only **`review`** splits: its generic harness goes native (see F), its data (perspectives/routing) stays per-repo.

## E. Planned RFC items (new — beyond current twig)

- [~] **1. Per-worktree lock** — flock(2), wired into `new`/`rm`/`spawn`/`gate`/`review` (held for the whole run/agent lifetime as a child process). **Pending:** `bowt lock status|release`; `--force` (terminate holder); busy-holder identity in the message (`busy: pid … (review, 6m)`)
- [x] **2. Versioned spawn prompts** — `bowt spawn [--impl]`: `internal/spawn/prompts/{plan,impl}.md` + `VERSION` (go:embed), `{{BRIEF}}` substitution, provenance line (`prompt: <mode>.md @ vN (hash)`) printed + prepended + copied into writebacks, clause-survival test. `runAgent` seam hardcodes `claude` pending item 4.
- [x] **3. `gate`** — `bowt gate [--scope]` runs the repo's `<configDir>/gate.sh` hook (per-check `BOWT_CHECK` lines) under the exclusive lock → atomic machine-readable `.bowt/gate.json` (overall/commit/worktree/dirty/checks); exit mirrors verdict. Django checks live in the repo's hook — core stays generic.
- [x] **4. Agent adapters** — `internal/agent`: `Agent` iface with `Session`+`Oneshot` modes, `claude` (default) + `codex` stub, `--agent`→`BOWT_AGENT`/`GWT_AGENT`→default selection, capability checks (require-oneshot errors; unknown knob warns+drops), `{{MEMORY_FILE}}` in single-source prompts, provenance `agent: <name> · prompt: <mode>.md @ v2 (hash)`, `doctor --agent`. Wired into `spawn` (default-claude byte-identical). *Oneshot's consumer is `review` (later). Follow-ups: read `BOWT_AGENT` from `.bowt/config` too (currently env only); `doctor --agent` should exit non-zero on a failed check (currently 0).*
- [~] **5. Native extension tier** — bowt's own commands are compiled (covering "native"); the per-repo shell-extension loader shipped (D). A discoverable bowt-shipped third tier + cross-source precedence deferred.

## F. review pipeline (the crown jewel — its own track)

- [~] diff scope + layer classification — in-place diff scope (`merge-base(base,HEAD)`→working tree, base `@{upstream}`→`origin/main`) + preflight facts done; **layer auto-routing deferred** (gen2-be-path-specific) — defaults to `--all`/`-p`
- [x] perspective fan-out with `max_parallel` throttle — via `agent.Oneshot` (default 3, `BOWT_REVIEW_PARALLEL`), under the exclusive lock
- [x] report validation postconditions — verdict/body count match, clean-report ≥300B floor, markdown-decoration tolerance (`internal/review/validate.go`, fully unit-tested via a fake Oneshot)
- [x] synthesis → decision queue (`SYNTHESIS.md`, `decision: PENDING`), prior queue archived to `SYNTHESIS.<epoch>.md`, same postcondition re-applied
- [ ] review-target worktree spawn/reuse/reset (code-only, pin to origin) — **deferred; in-place review only for now**
- [x] SUMMARY/SYNTHESIS output contract + runner postcondition (non-zero exit if 0 usable — "nothing was reviewed")

> Review follow-ups: `--model` is recorded but not applied (needs an `Opts` arg on `agent.Oneshot`, which touches the agent iface + providers); confirm the default base (`origin/main` vs twig's `origin/staging`).

## G. Orchestration layer (the cockpit — encodes the Notion orchestrator pattern)

`spawn` / writeback / `status` / `land` are facets of one missing abstraction: the
**`lane`** — a tracked unit of delegated work (mission-control's `agents/<id>.md` +
`-writeback.md`, made first-class). This is what turns bowt from "a toolkit I
operate by hand" into "a cockpit that runs the loop." Pattern refs:
[orchestrator→sub-agent](https://app.notion.com/p/3828033ab5c98149bfdcd0fd4841c918),
[scale case study](https://app.notion.com/p/3838033ab5c981fb82b7d111dc27874f),
[autonomous protocol](https://app.notion.com/p/3988033ab5c9813d8d5be6b2b36e9f22).

- [ ] **`lane` schema** (foundation — lock before G2): `{id, ticket, brief+hash,
  prompt_version, agent+model, worktree, branch, status
  (planning|plan-review|impl|review|paused|done|failed), wave/deps, writeback
  paths, gate_verdict, review C/S/N, escalations, provenance}` in a `lanes` table
  (or per-worktree `.bowt/lane.json`). Draft an `rfc/lanes.md` first.
- [ ] **G1 `bowt land <branch>`** *(quick win, independent — start first)* — gate →
  ff-merge → cleanup; refuse if ungated / dirty / non-FF. Encodes "never merge an
  ungated lane" as a verb. Would have prevented the cftunnel half-merge.
- [ ] **G2 `lane` object + headless `spawn`** *(keystone; gap #1)* — make `spawn`
  run the agent **non-interactively**, background it, capture the writeback, and
  record lane state + completion. Everything else hangs off this.
- [ ] **G3 writeback/comms first-class** *(depends on G2)* — structured protocol on
  the lane (status / escalate / result / followup), `bowt lane followup <id>`
  (writes FOLLOWUP + bumps provenance + re-spawns), escalations surfaced not
  grepped. Markdown artifacts stay human-readable; bowt indexes them. Plan gate
  (STOP-AFTER-PLAN) + review gate (multi-angle, never auto-fix) live here.
- [ ] **G4 `bowt status` / `bowt lanes`** *(depends on G2)* — cockpit `--json`:
  per-worktree lock holder + last gate verdict + commit + dirty + each lane's
  status. The thin-projection a UI later just renders (never a second DB).
- [ ] **later: autonomous protocol** — standing orders in the checkpoint,
  multi-angle review lenses, `LOCAL_VERIFY.md` deep functional pass, stop-and-wait
  (owner-only: contract/scope/merge/prod/money), `PAUSED ON <owner>` marker.

Spine: **schema → G2 → {G3, G4}**; G1 is independent and shippable now.

## Retired by the port (do NOT carry over)

- [?] dual `twig.sh` + `twig.zsh` maintenance → one binary (this is a root cause; zsh copy already drifted, missing `update`/`claude`/`status`/`refresh`/`main`)
- [?] `bash -n` as the only guardrail → real `go test` + `go vet`
- [?] manual `~/.twig/registry.tsv` TSV parsing → typed store

## Suggested slice order

2. SQLite store (B) — first dependency; the `Store` seam pays off.
3. config + env export + lifecycle hooks + `--code-only` (B) + the shell shim & `cd`/`exec` (A).
4. `spawn` + versioned prompts + provenance (A, E2) + lock wired into spawn (E1).
5. extension system: loader + manifest + helper lib + native tier (D, E5).
6. `review` native harness + adapters `oneshot` (F, E4) + lock wired in (E1).
7. `gate` (E3) + `doctor`/`status`/`refresh`/`sync`/`init` + signal transport (A, C).
