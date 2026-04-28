# Canopy

TUI for managing git worktrees with paired tmux sessions and per-project setup hooks.

> Status: pre-v0.1, in active development. Not yet usable end-to-end. Watch this space.

The pitch in one sentence: `canopy new` and ten seconds later you're attached to a tmux session with `nvim`, `claude`, a shell, and a running dev server, all on a fresh git worktree against an isolated database. Reboot your laptop, `canopy switch <name>`, and you're back exactly where you left off, claude conversation included.

## Build from source

Requires Go 1.22+ and a Linux box with `git >= 2.30` and `tmux >= 3.0` installed.

Clone and build:

```bash
git clone https://github.com/avinashjoshi/canopy.git
cd canopy
go build -o canopy ./cmd/canopy
./canopy --help
```

Or install straight to your `$GOBIN` (typically `~/go/bin`, must be on your `$PATH`):

```bash
go install github.com/avinashjoshi/canopy/cmd/canopy@latest
canopy --help
```

## What works today

- `canopy version` prints version + commit + build date
- `canopy --help` lists available commands
- `canopy --debug <cmd>` writes DEBUG-level JSON logs to `~/.canopy/log/canopy.log` (rotated automatically: 10 MB max per file, 3 backups, 28 days retention, gzip-compressed)

The wedge feature (`canopy new` → 10-second workspace setup, attached to a tmux session) lands in commit 4 per the build plan.

## Project structure

```
cmd/canopy/                Cobra root + subcommands
internal/clog/             Structured logging (slog + lumberjack)
internal/config/           canopy.json walk-up discovery + load    (step 3)
internal/git/              git worktree wrappers                   (step 2)
internal/tmux/             tmux session wrappers                   (step 2)
internal/hooks/            script execution with CANOPY_* env      (step 3)
internal/state/            workspace registry + flock              (step 3)
internal/workspace/        orchestration: Create/Remove/Reconcile  (step 4)
internal/namegen/          random adjective-noun workspace names   (step 4)
internal/ui/               Bubbletea Model/Update/View             (step 6)
internal/ai/               AI-tool defaults (multi-AI ready)       (step 6b)
docs/design/               Design doc (the source of truth)
docs/reviews/              Test plan + review artifacts
```

## Configuration (planned)

Each project that wants to use canopy drops a `canopy.json` at its root:

```json
{
  "scripts": {
    "setup": "bin/canopy-setup",
    "run": "bin/dev",
    "archive": "bin/canopy-archive"
  }
}
```

Three script paths, three env vars passed in (`CANOPY_WORKSPACE_PATH`, `CANOPY_ROOT_PATH`, `CANOPY_PORT`). The schema mirrors [Conductor.build's](https://conductor.build) on purpose; migrations from `conductor.json` are a five-minute `sed`.

## Documentation

- `docs/design/v0-canopy.md` — full design: architecture, premises, state machine, idempotency, error conventions
- `docs/reviews/v0-test-plan.md` — test coverage plan and critical concurrency tests
- `TODOS.md` — work deferred to v0.5 / v1
- `CLAUDE.md` — project context for Claude Code

## License

TBD before public release.
