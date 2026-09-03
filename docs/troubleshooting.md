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

## "canopy refuses to run inside a tmux session"

Canopy intentionally refuses to run from inside an existing tmux session — including (and especially) from inside a canopy workspace's tmux session. Reasons: nesting canopy means nesting tmux's attach/detach machinery (running `tmux attach` inside an attached session breaks in confusing ways), and accidentally creating workspaces from inside another workspace is the failure mode this guard exists to prevent.

The fix: detach the current tmux session first (`prefix-d` on whatever tmux prefix you've configured) and run canopy from the outer terminal.

`canopy version` is the one subcommand that always works regardless of context — it's the canonical "is canopy installed?" probe.

If you genuinely need to bypass the guard (testing, status-line scripting, CI), set `CANOPY_ALLOW_NESTED=1` for that invocation:

```bash
CANOPY_ALLOW_NESTED=1 canopy ls
```

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

The port plan reserves `<base>` for `canopy main` and assigns `<base>+10/+20/...` to workspaces. With defaults (`base: 40000`, `workspace_stride: 10`), the first project's main is 40000 and its first workspace is 40010. If something else on your machine is bound to the assigned port, `bin/dev` will collide.

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

## Remote host (v0.17) troubleshooting

### `canopy host add` probe fails with "permission denied"

Key auth isn't set up. Two fixes:

```bash
canopy host add tower cassy@tower --interactive   # offers ssh-copy-id automatically
# or, by hand:
ssh-copy-id cassy@tower.tail.ts.net
canopy host add tower cassy@tower.tail.ts.net
```

If the host is already registered and lost key auth, press `a` on its row in the TUI's **Remote hosts** tab to re-offer ssh-copy-id.

### TUI shows `(unknown)` version for a remote

That host is on canopy < v0.17.1.0 — older versions had a bug where `canopy_version` was always `"(unknown)"` over the wire (`cmd/canopy/ls.go` declared the var but never assigned it). Upgrade the host: select the row, press `U`. If `U` is hidden because the host is on a DEV binary, press `S` first (`canopy use release`).

### `canopy new --on tower --prompt "..."` leaks a temp file on success

Pre-v0.17.0.1 bug: the remote script used `exec canopy …`, which replaced bash before the cleanup trap could fire. Fixed in v0.17.0.1 (the `exec` was removed). Upgrade the host via `U`.

### Remote workspace shows `·` (no agent pane) but claude is running

Older remotes that didn't ship the v0.17.0 `agent.ClassifyOneShot` change can't classify their own agent panes — the laptop falls back to `·`. Upgrade the host. Or, if you've already done that and you still see `·`, the pane really did crash to a shell — `enter` to attach and inspect.

### Refresh hangs the TUI on launch when a host is unreachable

Pre-v0.17.0.1 bug. Refresher used to do flock I/O on the UI thread before returning the `tea.Cmd`, which froze the render against an unreachable host. Fixed: all I/O now lives inside the returned closure with a 3s deadline. Upgrade your laptop canopy.

### `canopy switch --on tower` falls back to ssh instead of mosh

As of v0.22.x this is expected, automatic behavior when mosh isn't installed
locally — canopy attaches over an ssh reconnect-loop instead of refusing to
attach. It re-dials with backoff on a dropped connection, so it's not just a
bare one-shot `ssh -t`. If you'd rather have mosh's UDP resilience, install
mosh on both ends:

```bash
# Arch / Omarchy
sudo pacman -S mosh

# Debian / Ubuntu
sudo apt-get install mosh

# macOS
brew install mosh
```

You can also opt into the ssh path explicitly, even when mosh IS installed,
with `--no-mosh` — useful when mosh's UDP port range (60000–61000 by
default) is blocked by a firewall or VPN but ssh still works:

```bash
canopy switch --on tower fix-the-bug --no-mosh
```

### `canopy new --on tower` from `$HOME` says "needs a project but you're not inside any"

The CLI doesn't auto-resolve the project from `$HOME` the way the TUI does. Either `cd` into a project dir first, or pass `--remote-cwd /path/on/remote` explicitly:

```bash
canopy new --on tower --remote-cwd /home/cassy/Work/cravd
```

(The TUI's `n` on a remote row does the right thing — it passes `--remote-cwd` resolved from the host registry.)

### `canopy host project add` is gone

Renamed to top-level `canopy project add` in v0.17.0. Old syntax is removed (no aliasing — clean v0.17 cut). Drop `host`:

```bash
canopy project add cravd /home/cassy/Work/cravd --on tower   # new
# was: canopy host project add cravd /home/cassy/Work/cravd --on tower
```

### Remote `(main)` rows show status "main" or branch "↗ —"

Pre-v0.17.0 wire format. Upgrade the host (`U`). v0.17.0+ runs `IsMain=true` and `fillMainBranches` on the remote side of `canopy ls --json` so the laptop gets real values.

### `canopy upgrade` (or in-TUI `U`) fails with "permission denied" on a host

From v0.21.1.0, the upgrade path detects this case explicitly. Either the source clone at `~/.canopy/src` or the install target at `$(BIN_DIR)/canopy.bin` (default `~/.local/bin/canopy.bin`) isn't writable by the user the SSH session runs as — almost always because a previous install was run via `sudo` and left root-owned files behind.

The error names the right directory and the recovery is:

```bash
ssh <host>
sudo chown -R $(whoami) ~/.canopy/src ~/.local/bin
canopy upgrade
```

Pre-v0.21.1.0 misclassified this as `there are local commits in the source clone` and steered users toward `git reset`, which never helped. If you see the old wording, your host is still on the prior version — `S` (canopy use release) then `U` again should now produce the clearer error. `make install` also pre-flights `$(BIN_DIR)` writability and `$(BIN_REAL)` ownership before `go build`, so the diagnosis surfaces immediately rather than mid-build.

### `canopy upgrade` (or in-TUI `U`) fails on a host with `make: go: No such file or directory`

`git pull` succeeded, then `make install` couldn't find `go`. Almost always means the host's Go toolchain lives behind `mise` / `asdf` activation that's wired through `~/.bashrc`, and the non-interactive `bash -l` we run over SSH skips `~/.bashrc` (Arch/Omarchy and Ubuntu defaults both bail out of bashrc when `$-` doesn't contain `i`). v0.21.4.0 introduced `bash -l`; v0.21.7.0 fixed it properly by prepending a shell snippet to every remote canopy command that adds `~/.local/bin`, `/usr/local/go/bin`, and `~/go/bin` to PATH first, then runs `eval "$(mise activate bash)"` if mise is present, then sources `~/.asdf/asdf.sh` if asdf is present. Upgrade your laptop canopy to v0.21.7.0+ — the next `U` against the same host should succeed.

If you're still stuck after upgrading the laptop, the Go binary is either missing entirely on the remote (install Go), or installed somewhere outside the four locations above (symlink it into `~/.local/bin` or add to mise/asdf).

### Hosts tab renders `· (never refreshed)` for every host on first load

Pre-v0.21.1.0 behavior. The Hosts tab now shows a Braille spinner (`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`, ~120 ms cadence) per host while the initial SSH fan-out is in flight; hosts with a cached snapshot keep their previous status, only never-refreshed hosts spin. Upgrade your laptop canopy.

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
