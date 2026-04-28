# Troubleshooting

Things that go wrong, in roughly the order users hit them.

## "canopy.json already exists" on `canopy init`

```
canopy.json already exists. This project is already initialized.

  - Run `canopy new` to create a workspace.
  - Run `canopy init --force` to regenerate canopy.json.
  - Run `canopy init --with-scripts --force` to also write bin/canopy-* stubs.
```

That's the friendly message, not an error — `canopy init` is idempotent. You're done. If you really want to overwrite, `--force`.

## "no canopy.json found in cwd or any parent directory"

You're not inside a canopy-initialized project. Three options:

1. `cd` into a project that has `canopy.json`.
2. Run `canopy init` here to onboard the current dir.
3. Run `canopy ls --all` if you just wanted to see workspaces across all projects (this works from anywhere).

## `canopy new` fails with "already exists"

Run `canopy ls`. If the workspace is in state.json with `status: ready` or `stopped`, use `canopy switch` to attach. If it's `broken`, you have two paths:

- **`canopy retry <name>`** — re-runs only `scripts.setup` against the existing worktree. Same dir, same branch, same port, same claude history; the workspace flips back to `ready` if setup succeeds. This is the right verb when the failure was a fixable knob (missing config, network blip, dep conflict) and you don't want to lose state.
- **`canopy rm <name>` then `canopy new`** — full teardown + fresh build. Use when the worktree itself is wrong (bad branch, corrupted checkout) or when you want a clean slate.

In the TUI the same choice is `R` (retry) vs `d` (delete) on the selected broken row.

When canopy recognizes the failure signature in `scripts.setup`'s stderr, it surfaces a one-line `hint:` under the table (and on `canopy new` / `canopy retry` failure output) telling you what to fix. Today's registry covers missing Rails master keys, "database already exists" from a partial setup, missing `bundle`, network/DNS errors, permission-denied on the script, and a generic `command not found` catch-all. The hint is heuristic — a wrong hint is rare but possible, in which case `~/.canopy/log/canopy.log` has the full stderr.

If there's no row but git says the branch exists, the branch lingered from a previous workspace whose state.json got nuked. Clean up the branch manually:

```bash
git branch -D <branch-name>
canopy new --name <branch-name>
```

## `canopy switch` says "session not found" but `canopy ls` shows status `ready`

That used to be a bug; now `canopy switch` runs a lazy reconcile that detects the mismatch and resurrects automatically. If you still see this, your binary is older than v0.0.20260428... — `git pull && make install`.

For a manual fix:

```bash
canopy reconcile
```

Walks state.json, syncs every workspace's status to disk + tmux reality. Never deletes rows; orphans get marked but stay until you `canopy rm` them.

## `canopy ls` shows `●` next to a workspace, but I killed its tmux session

Your binary is older than the live-tmux-badge feature. `git pull && make install`. The `●`/`○` indicator queries tmux at print time so it always reflects the current state.

## Pane closes immediately after I /quit nvim or claude

Shouldn't happen — every command pane is wrapped in `; exec "$SHELL"` so quitting drops you to a shell. If it's still happening:

- Confirm you're on a recent canopy: `canopy version` should show a commit from after April 2026.
- Check your `$SHELL` env var: `echo $SHELL` should print `/bin/bash`, `/usr/bin/zsh`, or similar.
- If `$SHELL` is unset (rare in interactive use, sometimes in CI-spawned shells), set it: `export SHELL=$(which bash)`.

## `bin/dev` in the shell pane says "port already in use"

The port plan reserves `<base>` for `canopy main` and assigns `<base>+10/+20/...` to workspaces. If something else on your machine is bound to the assigned port, `bin/dev` will collide.

Check what's using it:

```bash
ss -tlnp | grep $CANOPY_PORT
```

If it's an old workspace's dev server you forgot to stop, restart canopy via `canopy switch`-ing into it and Ctrl-C'ing.

If it's something completely unrelated, override the port plan in `~/.canopy/config.json`:

```json
{
  "ports": {
    "base": 50000
  }
}
```

Then `canopy reconcile` and `canopy rm` + `canopy new` to get a fresh port.

## "claude: no conversation found to continue" on resurrection

Used to surface during `canopy switch` resurrection when a workspace had never had a claude conversation in it. Now handled with a `claude --continue || claude` shell fallback — fresh claude starts when there's nothing to continue. Update if you're seeing it: `git pull && make install`.

## `tmux` says "server exited unexpectedly"

Almost always means the tmux server died in the middle of a multi-step canopy command. Causes:

- Out-of-memory (rare, but tmux is sensitive to OOM-killer).
- A pane's command exited and the auto-removed pane was the only one in the only session.
- Manual `tmux kill-server` while canopy was working.

Re-run the canopy command. If it persists, check `~/.canopy/log/canopy.log` for the tmux stderr canopy captured.

## State.json got corrupted / canopy refuses to load it

Corruption usually means hand-editing went wrong. The file is JSON; check it parses:

```bash
jq . ~/.canopy/state.json
```

If `jq` errors, your state is bad. Easiest fix:

```bash
mv ~/.canopy/state.json ~/.canopy/state.json.broken-$(date +%s)
canopy ls --all      # rebuilds an empty state on first call
```

You'll lose the workspace registry but the worktrees on disk and the tmux sessions still exist — `canopy reconcile` once you've got a few workspaces back, or just nuke the worktrees and start fresh.

## Logs

Everything canopy does at INFO level lands in `~/.canopy/log/canopy.log`. JSON, append-only, rotated automatically (10 MB max per file, 3 backups, 28 days retention, gzip-compressed).

For verbose output:

```bash
canopy --debug new --name foo
tail -f ~/.canopy/log/canopy.log | jq .
```

Logs include the project, command, sub-step, and elapsed ms per step — useful for "where did it slow down."

## Still stuck?

File an issue at https://github.com/avinashjoshi/canopy/issues with:

- `canopy version` output
- `cat ~/.canopy/state.json` (redact paths if sensitive)
- The relevant slice of `~/.canopy/log/canopy.log`
- What you ran and what you expected vs got
