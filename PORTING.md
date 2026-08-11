# bowt — full port ledger (twig bash → Go)

Tracks everything that must exist for bowt to fully replace the bash `twig`
(core `twig.sh`/`twig.zsh`, the env/config substrate, agent integration, the
extension system) **plus** the five planned RFC items. Status keys:
`[x]` done · `[~]` partial · `[ ]` pending · `[?]` reconsider/obsolete.

> Snapshot: landed so far — worktree CRUD + `exec`/`cd`/`root`, SQLite registry,
> per-worktree flock lock, config loading + `BOWT_*`/`GWT_*` env + lifecycle
> hooks, and a cobra dispatch with generated shell completion (incl. dynamic
> worktree-name completion). Next: `--code-only`, then `spawn` + versioned
> prompts, then `gate`. The agent layer, extension system, and `review` are ahead.

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
- [ ] `doctor [--agent X]` — validate deps / ports / registry / provider
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
- [ ] `--code-only` mode + registry `flags` column + `GWT_CODE_ONLY` export  *(BOWT_CODE_ONLY/GWT_CODE_ONLY exported as `0` for now; mode itself is a later slice)*
- [x] lifecycle hooks — invoke `pre-setup.sh` / `setup.sh` / `teardown.sh` with the `$path $branch $offset $port` contract via the Runner (`internal/hook`); pre-setup fatal, setup/teardown warn
- [ ] `.git/info/exclude` management (add `.twig/`|`.bowt/`)
- [ ] copy agent config dir (`.claude/`) into each new worktree  *(ties to D + adapters)*

## C. Agent / Claude integration

- [?] `_bowt_signal` — **DEFER (not now; maybe later).** HTTP POST transport whose only consumer was the abandoned dashboard's `/api/twig/signal` → unbounded `signals` table (a DB-heaviness culprit). No listener in terminal-only bowt, so not ported now. If revived later, emit JSON lines to stdout / a bounded local log — never POST-to-DB.
- [ ] statusLine — generate `.twig-status`, install into `~/.claude/settings.json`, port `statusline.sh` renderer (Claude-specific, adapter-gated)
- [ ] docs injection on `init` — `twig-docs.md` / `twig-extensions.md` → `.claude/` + `CLAUDE.md` `@`-refs
- [x] shell completion — cobra-generated `bowt completion bash|zsh|fish|powershell`, with **dynamic completion** of registered worktree names for `cd`/`rm`/`path`/`exec`; loaded (alongside the `cd` shim) via `shell-init`

## D. Extension system  *(port the MECHANISM, not the 18 scripts)*

- [ ] per-repo extension loader — resolve + invoke `.twig/extensions/<cmd>.sh` as a subprocess
- [ ] native extension tier — twig-shipped generic commands as **compiled subcommands** (RFC item 5)
- [ ] resolution + precedence — **per-repo shadows native**; `help`/`doctor` show which source ran
- [ ] extension manifest — header fields (`bowt-lock: none|shared|exclusive`, `bowt-scope: repo|native`, desc)
- [ ] helper lib — `bowt lib` prints a sourceable `bowt.lib.sh` (`bowt_err`/`bowt_signal`/`bowt_gate_record`/`bowt_status_add`) that shells back to the binary
- [ ] `bowt new-extension <name>` — scaffold a manifest+helper skeleton

**Existing extensions migrate to the contract (NOT bowt's to reimplement):**
gen2-be — `aws cftunnel gos grafana lint manage review shell test test-log tunnel vite web webhook worker`;
bowt-app — `run test tsc`. Only **`review`** splits: its generic harness goes native (see F), its data (perspectives/routing) stays per-repo.

## E. Planned RFC items (new — beyond current twig)

- [~] **1. Per-worktree lock** — flock(2) primitive built + wired into `new`/`rm`/`spawn` (held for the agent's lifetime as a child process). **Pending:** wire into `review`/`gate`; `bowt lock status|release`; `--force` (terminate holder); busy-holder identity in the message (`busy: pid … (review, 6m)`)
- [x] **2. Versioned spawn prompts** — `bowt spawn [--impl]`: `internal/spawn/prompts/{plan,impl}.md` + `VERSION` (go:embed), `{{BRIEF}}` substitution, provenance line (`prompt: <mode>.md @ vN (hash)`) printed + prepended + copied into writebacks, clause-survival test. `runAgent` seam hardcodes `claude` pending item 4.
- [ ] **3. `gate`** — compose lint+test (+ per-repo hook) → machine-readable `gate.json` (commit/worktree/scope), holds the lock
- [ ] **4. Agent adapters** — provider descriptors, `session` + `oneshot` modes, capability checks, `doctor --agent`, `{{MEMORY_FILE}}` (depends on item 2), agent in provenance
- [ ] **5. Native extension tier** — see D

## F. review pipeline (the crown jewel — its own track)

- [ ] diff scope + layer classification (route perspectives by changed paths)
- [ ] perspective fan-out with `max_parallel` throttle — via the `oneshot` adapter (E4)
- [ ] report validation postconditions — verdict/body count match, clean-report byte floor, markdown-decoration tolerance
- [ ] synthesis → decision queue (`SYNTHESIS.md`, PENDING→AGREE/REJECT), archive prior queue
- [ ] review-target worktree spawn/reuse/reset (code-only, pin to origin)
- [ ] SUMMARY/SYNTHESIS output contract + runner postcondition (fail if 0 reviewed)

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
