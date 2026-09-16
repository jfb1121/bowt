# bowt

[![CI](https://github.com/jfb1121/bowt/actions/workflows/ci.yml/badge.svg)](https://github.com/jfb1121/bowt/actions/workflows/ci.yml)

**b**unch **o**f **w**ork**t**rees — git worktrees with fully isolated environments, for humans and agents alike.

A single static Go binary. Every git branch gets its own worktree with an
allocated port/offset (and, as it grows, a per-worktree DB and `.env`), plus an
orchestration layer (`spawn`, `gate`, `review`, `land`) for driving headless
coding agents in parallel. Agent-first: every command speaks JSON when piped, so
an agent drives bowt exactly the way you do.

![bowt running work lanes](docs/demo.gif)

> **Pre-1.0.** The worktree core and the agent/orchestration layer (`spawn`,
> `gate`, `review`, `research`, `land`, lanes) are in and tested. A few
> conveniences (the Claude statusline, docs injection) aren't in yet — see
> [`PORTING.md`](PORTING.md).

## Design

- **Agent-first.** Commands emit JSON by default whenever stdout is not a
  terminal, so agents parse structured output instead of scraping tables. Human
  tables appear only at an interactive terminal (or with `--json`/`--human`).
- **The binary does everything.** A shell shim (`bowt shell-init`) is a thin,
  human-only convenience for `cd`; agents use `bowt path` / `bowt exec`.
- **One state owner.** bowt owns the registry (SQLite at `~/.bowt/state.db`). No
  second stateful system, no event bus writing to a DB.

## Install

```bash
# Homebrew
brew install jfb1121/bowt/bowt

# or with Go (1.25+)
go install github.com/jfb1121/bowt@latest
```

## Usage

```bash
bowt init                    # scaffold .bowt/ for this repo (config + hooks + agent guide)
bowt new feature/login       # create a worktree, allocate port/offset, run setup hooks
bowt ls                      # table at a terminal…
bowt ls | cat                # …JSON when piped (agent-first)
bowt setup feature/login     # re-run the setup hooks for a worktree
bowt path feature/login      # print its path (agents / the shim cd into it)
bowt rm feature/login        # teardown + deregister
```

`.bowt/` is meant to be committed, so your whole team shares one worktree setup;
edit `.bowt/setup.sh` for your stack, or point a coding agent at the scaffolded
`.bowt/AGENTS.md` and let it wire the hooks by reading the repo.

### Orchestrating lanes

Dispatch headless agents into isolated worktrees, track them as **lanes**, gate
and review the work, then land it:

```bash
bowt spawn --impl brief.md   # a headless agent works in its own worktree → a tracked lane
bowt lanes                   # the fleet: every lane and its status
bowt status                  # per-worktree cockpit: branch × lane × gate
bowt gate                    # run the repo's verification hook → machine-readable verdict
bowt review --all            # perspective code review of the diff
bowt land feature/login      # gate + fast-forward onto base + clean up
```

Agent fan-out:

```bash
bowt spawn --impl brief.md               # one headless coding agent against a brief
bowt review --all                        # perspective code review of the diff
bowt research brief.md --n 3             # fan out 3 headless research agents (web research)
bowt research --queries topics.txt       # one research agent per query line
bowt research brief.md --n 5 --synthesize --concurrency 2
```

`bowt research` is spawn for *research* rather than code: it fans headless agents
(the same subscription-powered child-process path as `spawn`) over web-research
tasks, each writing a cited findings file under `--out` (default `research-out/`)
— no gate, no commit, no PR. At most `--concurrency` agents run at once (default
2; the host OOMs past a handful), and `--synthesize` adds a final agent that
merges the findings into `SYNTHESIS.md`. `--dry-run` prints the plan without
launching anything.

## Development

```bash
make check    # fmt-check + vet + lint + test -race  (what CI runs)
make build    # version-stamped binary
make help     # list targets
```

CI runs the same `make check` on every push. See [`CONTRIBUTING.md`](CONTRIBUTING.md)
for conventions.

## Layout

```
*.go (package main) one file per command group (new · spawn · lanes · land ·
                    gate · review · research · cockpit · worktree · meta) + dispatch
internal/           compiler-private core — importable only within this module
  state/            worktree + lane registry (SQLite, behind a Store interface)
  repo/             git operations via os/exec
  lock/             exclusive per-worktree lock via flock(2)
  output/           JSON vs human render (agent-first)
  run/              process-execution seam (Runner) — config + hooks + tests
  config/           resolve .bowt + bash-source the config file
  env/              BOWT_* env contract
  hook/             pre-setup / setup / teardown lifecycle scripts
  worktree/         new · rm · path · list · setup — ties state + repo together
  scaffold/         `bowt init` templates (.bowt/ config + agent guide)
  agent/            provider adapters (claude, codex, drop-ins) — data-driven
  spawn/            versioned prompts + headless lane dispatch
  gate/ review/     verification verdict · perspective review fan-out
  research/         headless research fan-out
  extension/        per-repo `bowt <cmd>` extension loader
```

## Roadmap

Tracked in [`PORTING.md`](PORTING.md). Next: the Claude statusline + docs
injection, more stack examples, and **remote orchestration over SSH** — an agent
already drives `ssh host bowt …`, so this is cockpit/provisioning ergonomics on top.

## License

[MIT](LICENSE) © Jayesh Bafna
