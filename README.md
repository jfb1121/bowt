# bowt

[![CI](https://github.com/jfb1121/bowt/actions/workflows/ci.yml/badge.svg)](https://github.com/jfb1121/bowt/actions/workflows/ci.yml)

**b**unch **o**f **w**ork**t**rees — isolated per-worktree environments and a correctness-enforcing harness for running coding agents in parallel.

A single static Go binary. Every git branch gets its own worktree with an
allocated port/offset (and, as the port grows, a per-worktree DB and `.env`),
plus an orchestration layer (`spawn`, `review`, `gate`) for driving headless
coding agents. The successor to the `twig` shell tool.

> **Status: early.** Worktree CRUD + a SQLite registry + a per-worktree lock are
> in. The substrate, agent layer, and extension system are being ported slice by
> slice — see [`PORTING.md`](PORTING.md) for the full ledger.

## Design

- **Agent-first.** Commands emit JSON by default whenever stdout is not a
  terminal, so agents parse structured output instead of scraping tables. Human
  tables appear only at an interactive terminal (or with `--json`/`--human`).
- **The binary does everything.** A shell shim (`bowt init zsh`) is a thin,
  human-only convenience for `cd`; agents use `bowt path` / `bowt exec`.
- **One state owner.** bowt owns the registry (SQLite at `~/.bowt/state.db`). No
  second stateful system, no event bus writing to a DB.

## Install

Requires Go 1.25+.

```bash
go install github.com/jfb1121/bowt@latest
# or from a clone:
make build && ./bowt help
```

## Usage

```bash
bowt new feature/login       # create + register a worktree
bowt ls                      # table at a terminal…
bowt ls | cat                # …JSON when piped (agent-first)
bowt ls --json               # force JSON
bowt path feature/login      # print its path (agents / the shim cd into it)
bowt rm feature/login        # teardown + deregister
bowt version                 # build version
```

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
main.go              CLI entry + dispatch (single binary)
internal/            compiler-private core — importable only within this module
  state/             worktree registry (SQLite, behind a Store interface)
  repo/              git operations via os/exec
  lock/              exclusive per-worktree lock via flock(2)
  output/            JSON vs human render (agent-first)
  worktree/          new · rm · path · list — ties state + repo together
```

## Roadmap

Tracked in [`PORTING.md`](PORTING.md). Next slices: config/env substrate + shell
shim, `spawn` + versioned prompts, the extension system, `review`, `gate`.

## License

Private and unpublished — **all rights reserved** (see [`LICENSE`](LICENSE)). No
open-source license is granted yet; one may be chosen later.
