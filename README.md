# Canopy

TUI for managing git worktrees with paired tmux sessions and per-project setup hooks.

> Status: pre-v0.1, in active development. Not yet usable end-to-end. Watch this space.

The pitch in one sentence: `canopy new` and ten seconds later you're attached to a tmux session with `nvim`, `claude`, a shell, and a running dev server, all on a fresh git worktree against an isolated database. Reboot your laptop, `canopy switch <name>`, and you're back exactly where you left off, claude conversation included.

## Build from source

Requires Go 1.22+ and a Linux box with `git >= 2.30` and `tmux >= 3.0` installed.

Clone and install for daily use (binary lands at `~/.local/bin/canopy`):

```bash
git clone https://github.com/avinashjoshi/canopy.git
cd canopy
make install
canopy --help
```

`make install` is the dogfood loop. Run it after every `git pull` to keep the installed binary current. If `~/.local/bin` isn't on your `$PATH`, the Makefile prints a one-liner to add it.

Other Make targets:

```
make build       # build ./canopy in repo root, don't install
make test        # fast unit tests
make test-e2e    # full E2E suite (real tmux, scratch repo, slow)
make lint        # golangci-lint if installed
make uninstall   # remove ~/.local/bin/canopy
make clean       # remove ./canopy
```

If you prefer `go install` (puts the binary in `$GOBIN`, default `~/go/bin`):

```bash
go install github.com/avinashjoshi/canopy/cmd/canopy@latest
```

## What works today

The wedge feature is live. From inside any project that has a `canopy.json`:

```bash
canopy new                  # creates a workspace with a random name (e.g. bold-falcon)
canopy new --name fix-bug   # explicit name
canopy main                 # opens a tmux session in the project root (no worktree)
canopy ls                   # workspaces in the current project
canopy ls --all             # workspaces across every project (also implicit when run outside any project)
canopy switch <name>        # attach (resurrect first if stopped; auto-reconciles status)
canopy rm <name>            # tear down (archive script + tmux + git + branch)
canopy reconcile            # update workspace statuses to match disk + tmux reality
```

Each workspace gets a 3-pane tmux session: nvim top-left, claude top-right (with `--continue` on resurrect so prior conversation history resumes), shell full-width on the bottom. `scripts.run` from `canopy.json` is reserved for a future on-demand `canopy run` invocation; v0 doesn't auto-start it.

### Port allocation

Every workspace gets a unique TCP port via `CANOPY_PORT`, allocated through a Conductor-style block plan:

- Each project's first workspace lands on `base_port` (default 3000).
- Subsequent workspaces in the same project step up by `workspace_stride` (default 10): 3000, 3010, 3020, ...
- A new project's first workspace lands `project_stride` higher than the previous project (default 1000): cravd → 3000, brain → 4000, hey-cli → 5000.

Project-to-base assignments are first-come-first-served and persisted in `state.json`, so a workspace's port is stable across reboots.

Defaults are tweakable via `~/.canopy/config.json` (optional file):

```json
{
  "ports": {
    "base": 3000,
    "project_stride": 1000,
    "workspace_stride": 10
  }
}
```

Partial overrides are fine — any field you skip stays at the default.

Plus operational glue:

- `canopy init` — onboard a project (creates `canopy.json` + stub `bin/canopy-*` scripts; detects existing `conductor.json` and mirrors its schema)
- `canopy version` — version, commit, build date
- `canopy --debug` — DEBUG-level JSON logs to `~/.canopy/log/canopy.log` (auto-rotated: 10 MB / 3 backups / 28 days / gzip)

Workspaces live at `~/.canopy/workspaces/<project>/<name>` — canopy owns the storage so the source repo stays clean. Each workspace gets a 4-pane tmux session (nvim, claude, shell, your dev server) and a unique TCP port via `CANOPY_PORT`.

A Bubbletea TUI (`canopy` with no args) is on the roadmap for the next milestone.

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

## Onboarding a project

Run `canopy init` from your project root:

```bash
cd ~/Work/your-project
canopy init
```

That drops a `canopy.json` plus three stub scripts at `bin/canopy-{setup,run,archive}` for you to fill in. Edit the scripts, commit them, then run `canopy new`.

If the project already has a `conductor.json` (Conductor's config — same schema), `canopy init` detects it and copies the script paths verbatim. Your existing `bin/conductor-*` scripts keep working; just remember to switch any `CONDUCTOR_*` env-var references in your scripts and config files to the `CANOPY_*` equivalents.

### canopy.json schema

```json
{
  "scripts": {
    "setup": "bin/canopy-setup",
    "run": "bin/dev",
    "archive": "bin/canopy-archive"
  }
}
```

Three script paths. Each script gets the same env vars when canopy invokes it:

| Var | Meaning |
|---|---|
| `CANOPY_WORKSPACE_PATH` | absolute path to the workspace dir |
| `CANOPY_ROOT_PATH` | absolute path to the original repo root |
| `CANOPY_PORT` | allocated TCP port for this workspace (3000-3999) |

`setup` runs once at workspace creation. `run` is the long-running command for the server pane (re-launched on resurrection). `archive` runs at workspace removal.

## Documentation

User-facing guides:

- [`docs/getting-started.md`](docs/getting-started.md) — 5-minute tour: install, init, first workspace
- [`docs/canopy-json.md`](docs/canopy-json.md) — schema reference + `~/.canopy/config.json` settings
- [`docs/migrate-from-conductor.md`](docs/migrate-from-conductor.md) — step-by-step for projects with `conductor.json`
- [`docs/troubleshooting.md`](docs/troubleshooting.md) — common problems and fixes

For contributors / future-you:

- [`docs/architecture.md`](docs/architecture.md) — codebase layout, dependency direction, where to add things
- [`docs/design/v0-canopy.md`](docs/design/v0-canopy.md) — design doc with premises, state machine, error conventions
- [`docs/reviews/v0-test-plan.md`](docs/reviews/v0-test-plan.md) — test coverage plan and critical concurrency tests
- [`TODOS.md`](TODOS.md) — deferred work, organized by milestone
- [`CLAUDE.md`](CLAUDE.md) — project context for Claude Code

## License

TBD before public release.
