# Configuring bowt for this repo (agent guide)

You are helping set up **bowt** for THIS repository. bowt gives every git branch
its own worktree with an isolated, conflict-free environment. Your job: fill in
`.bowt/setup.sh` (plus `teardown.sh` and `config`) so each worktree gets a
working environment for *this repo's* stack. Read the repo, then write the hooks.

## How bowt calls the hooks

`bowt new <branch>` (and `bowt setup`) run `.bowt/pre-setup.sh` then
`.bowt/setup.sh`; `bowt rm` runs `.bowt/teardown.sh`. Each is called with:

- **positional args:** `$1=path  $2=branch  $3=offset  $4=port`
- **environment:** `BOWT_PORT BOWT_OFFSET BOWT_BRANCH BOWT_MAIN_REPO
  BOWT_REPO_NAME BOWT_CODE_ONLY`, plus any variables you define in
  `.bowt/config`.

Every worktree gets a distinct `$offset` (0, 1, 2, …) and `$port`
(`BOWT_PORT_BASE + offset`, base 8000 by default). Key **everything** off these
so N worktrees run in parallel without collisions.

## What to do

1. **Learn the stack.** Inspect the repo — `package.json`, `pyproject.toml`,
   `go.mod`, `Dockerfile`, `docker-compose.yml`, any `.env.example` — to find how
   it runs, which services/ports it needs, and how it reaches a database.
2. **Set the port base** in `.bowt/config` (`BOWT_PORT_BASE=...`) if 8000 is wrong
   for this repo. Add a `BOWT_DB_PREFIX` or similar if useful.
3. **Write `setup.sh`** to provision ONE isolated environment per worktree —
   typically:
   - generate a per-worktree `.env` (write `$port`, derive extra ports as
     `$((port+1))`, `$((port+2))`, … for additional services)
   - create a per-worktree database, e.g. `${BOWT_DB_PREFIX:-app}_${offset}`
   - install dependencies and run migrations
4. **Write `teardown.sh`** to undo exactly what `setup.sh` created (drop the DB,
   remove generated files) so `bowt rm` leaves nothing behind.
5. **Add extensions** for repeated commands: `.bowt/extensions/<name>.sh` defining
   `_bowt_ext_<name>()`, invoked as `bowt <name>` (e.g. `bowt test`, `bowt run`).

## Rules

- Derive every port and resource name from `$offset`/`$port`. Never hardcode a
  single shared port or database name — that defeats the whole point.
- Keep secrets OUT of `.bowt/`. Read them from the environment or the main repo's
  git-ignored `.env`; do not commit them into the hooks.
- **Verify before you finish:** `bowt new test/bowt-check`, confirm the app runs
  on its assigned port, then `bowt rm test/bowt-check` and confirm clean teardown.
