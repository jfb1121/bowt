# Contributing to bowt

## The gate

Before every commit:

```bash
make check    # fmt-check + go vet + golangci-lint + go test -race
```

CI runs the identical command on push. If `make check` is green locally, CI is green.

## Conventions

These are the house rules the linter can't all enforce:

- **Errors are values.** Return them; wrap with context using `%w`
  (`fmt.Errorf("git %s: %w", args, err)`); use sentinels (`errors.Is`) for
  expected conditions. Never `panic` in `internal/*`. If you ignore an error on
  purpose, write `_ = x()` with a comment.
- **stdout = data, stderr = diagnostics.** Command results go through
  `output.Emit` (JSON); errors go to stderr via `output.Errf`. This is what makes
  bowt agent-parseable — don't mix them.
- **Small interfaces, defined at the consumer.** See `state.Store`. Don't add an
  interface until there's a second implementation or a test fake that needs it.
- **Package names describe function** (`state`, `repo`, `lock`) — never `util`,
  `common`, `core`, `helpers`. No stutter (`state.Store`, not `state.StateStore`).
- **Typed constants for fixed value sets.** For enum-like fields (gate status,
  lock mode, capabilities) define `type GateStatus string` + `const` values —
  not bare strings. They still serialize to the same JSON.
- **Structs, not `map[string]string`, for JSON output.** An agent-facing shape
  should be a declared type, even an anonymous one.
- **Tests:** table-driven where it fits; use `t.TempDir`/`t.Setenv`/`t.Cleanup`;
  fake the edges (`Runner`, `Agent`, `Clock`) rather than calling real
  git/pytest/claude for logic tests; always runnable under `-race`.

## Adding a dependency

Prefer the standard library. If you must add one, justify it, run `make tidy`,
and commit `go.sum`.
