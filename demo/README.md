# Demo

`docs/demo.gif` — a ~16s recording of running work lanes with bowt.

## Regenerate

```bash
demo/record.sh     # build bowt → seed an isolated demo → record → docs/demo.gif
```

Requires `asciinema` + `agg` (`brew install asciinema agg`). Nothing here touches
your real `~/.bowt` — everything runs under a throwaway `HOME` at `/tmp/bowt-demo`.

## What's real vs staged

- **Real:** the worktrees (`bowt new`) with their isolated ports/offsets, and the
  `bowt spawn` call — it runs bowt's actual agent adapter against a **stub agent**
  (`demo-agent.sh`: a fake provider that returns instantly, no model, no network).
- **Staged:** the lane rows in `bowt lanes` / `bowt status` are seeded
  (`seed.sh`) in terminal states, because real headless spawns are slow, cost
  money, and are non-deterministic — unsuitable for a repeatable GIF.

## Files

| File | Role |
|------|------|
| `record.sh` | orchestrates build → seed → record → render |
| `seed.sh` | isolated demo state: real worktrees + seeded lanes + the stub agent |
| `run.sh` | the scripted session asciinema records |
| `demo-agent.sh` | the instant stub "agent" bowt's adapter invokes |
| `lanes.tape` | a [vhs](https://github.com/charmbracelet/vhs) alternative (needs a working vhs render backend) |
