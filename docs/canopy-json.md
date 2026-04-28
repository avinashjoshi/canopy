# canopy.json reference

`canopy.json` lives at the root of every project canopy manages. It's small on purpose — three optional script paths and that's it.

```json
{
  "scripts": {
    "setup": "bin/canopy-setup",
    "run": "bin/dev",
    "archive": "bin/canopy-archive"
  }
}
```

Run `canopy init` to create one with sensible defaults; `canopy init --with-scripts` also drops stub scripts at the paths above.

## Fields

All three script fields are optional. A `canopy.json` of `{}` is valid and means "create the worktree + tmux session, no hooks." Useful for projects that don't need DB setup or per-workspace dependency installs.

| Field | When it runs | If empty |
|---|---|---|
| `scripts.setup` | Once at workspace creation, after the worktree is checked out. Failure -> workspace marked `broken`. Re-runnable via `canopy retry <name>` (or `R` in the TUI) without losing the worktree, branch, port, or claude history. | Skipped silently. |
| `scripts.run` | Reserved for future on-demand invocation (`canopy run` is a v0.5 TODO). v0 does NOT auto-launch this. | No effect today. |
| `scripts.archive` | At workspace removal, before the worktree is deleted. Failure logged but doesn't block removal. | Skipped silently. |

Write `scripts.setup` to be safely re-runnable. If the first invocation crashes halfway through, the recovery path is `canopy retry <name>` — same script, same env, same workspace dir. A setup that hard-fails on `bin/rails db:create` because the DB already exists from the first attempt forces the user to fall back to `canopy rm` + `canopy new`, which throws away the worktree and claude history. Use `db:prepare` over `db:create`, `bundle install` over `bundle install --deployment`, idempotent symlinks (`ln -sf`), and existence checks before destructive ops.

Script paths are relative to the project root (the directory containing `canopy.json`). They must be executable files (have a shebang and the executable bit set). canopy invokes them via `exec.CommandContext` directly — there's no `sh -c` wrapper, so multi-arg shell expressions like `"rm -rf .sock && bin/dev"` won't work as a script path. Put that logic inside a script instead.

## Environment variables canopy passes to scripts

Every script invocation gets these on top of your shell's existing environment:

| Variable | Meaning |
|---|---|
| `CANOPY_WORKSPACE_PATH` | Absolute path to the workspace dir (the new git worktree) |
| `CANOPY_ROOT_PATH` | Absolute path to the original repo root |
| `CANOPY_PORT` | Allocated TCP port for this workspace |

Scripts run with `cwd = CANOPY_WORKSPACE_PATH`. If you need the original repo (e.g. to symlink shared secrets), use `CANOPY_ROOT_PATH`.

The same env vars are also set at the **tmux session** level via `tmux -e`, so any pane you create later (including the bottom shell pane and any `prefix-c` windows) can read them. Run `echo $CANOPY_PORT` in the shell pane to confirm.

## Example: cravd-style Rails project

```json
{
  "scripts": {
    "setup": "bin/canopy-setup",
    "run": "bin/dev",
    "archive": "bin/canopy-archive"
  }
}
```

`bin/canopy-setup`:

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$CANOPY_WORKSPACE_PATH"

# Symlink shared secrets from the source repo
ln -sf "$CANOPY_ROOT_PATH/config/master.key" config/master.key
ln -sf "$CANOPY_ROOT_PATH/.env" .env

bundle install
bin/rails db:create RAILS_ENV=development   # uses CANOPY_PORT in database.yml
```

`bin/canopy-archive`:

```bash
#!/usr/bin/env bash
set -euo pipefail
DISABLE_DATABASE_ENVIRONMENT_CHECK=1 bin/rails db:drop
```

`config/database.yml` reads `CANOPY_PORT` to derive a per-workspace database name (e.g. `dev_db_<port>`).

## Per-machine settings (`~/.canopy/config.json`)

A separate, optional file for canopy-wide tweaks that aren't per-project. Today there's just one section:

```json
{
  "ports": {
    "base": 40000,
    "project_stride": 1000,
    "workspace_stride": 10
  }
}
```

These are the defaults, so the file is only useful if you want to override them. Partial overrides are fine — fields you skip stay at the default.

- `base`: first project's base port. Default 40000 (clean of webapp / k8s / IANA-assigned ranges).
- `project_stride`: distance between consecutive project bases. Project 1 = base, project 2 = base + stride, ...
- `workspace_stride`: distance between workspaces within a project. Project's main = base, ws#1 = base + stride, ws#2 = base + 2*stride, ...

The base of each project is reserved for `canopy main`; workspaces from `canopy new` start one stride above.

## See also

- `docs/getting-started.md` — install + first workspace
- `docs/migrate-from-conductor.md` — if you have an existing `conductor.json`
- `docs/troubleshooting.md` — common problems
