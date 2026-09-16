# Changelog

All notable changes to bowt are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and bowt aims to adhere
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0]

First public release.

### Added
- **Isolated worktrees** — `bowt new <branch>` creates a git worktree with an
  allocated port/offset (and, via hooks, a per-worktree DB and `.env`), and
  `bowt rm` tears it down. A SQLite registry (`~/.bowt/state.db`) is the single
  state owner; an exclusive per-worktree lock guards concurrent writers.
- **`bowt init` / `bowt setup`** — scaffold a committable `.bowt/` (config,
  setup/teardown hooks, and an agent guide) and re-run the setup hooks.
- **Agent-first output** — every command emits JSON when stdout is not a
  terminal; human tables only at an interactive terminal or with `--json`/`--human`.
- **Orchestration layer** — `spawn` (headless coding agents into tracked lanes,
  versioned prompts), `lanes`/`status` (the cockpit), `gate` (verification hook →
  machine-readable verdict), `review` (perspective review fan-out → synthesis),
  `research` (headless research fan-out), and `land` (gate + fast-forward + cleanup).
- **Provider adapters** — data-driven agent descriptors (`claude`, `codex`, plus
  user drop-ins under `~/.bowt/agents/*.json`); headless runs gated to built-ins.
- **Per-repo extensions** — `bowt <cmd>` runs `.bowt/extensions/<cmd>.sh`.
- **Distribution** — a single static binary; `go install` and a Homebrew cask.
- **MIT licensed.**

[Unreleased]: https://github.com/jfb1121/bowt/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/jfb1121/bowt/releases/tag/v0.1.0
