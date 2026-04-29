# Getting Started

A 5-minute tour. Assumes Linux (Arch / Omarchy ideal), `git >= 2.30`, `tmux >= 3.0`, and Go 1.22+ already installed.

## Install

```bash
git clone https://github.com/oncactus/canopy.git
cd canopy
make install
```

Drops the binary at `~/.local/bin/canopy`. Make sure `~/.local/bin` is on your `$PATH`. The Makefile prints a one-liner if it isn't.

```bash
canopy version
```

Should print something like `canopy v0.0.0-... (commit abc1234, built 2026-...)`.

## Onboard a project

`cd` into any git repo, then:

```bash
canopy init
```

That writes a minimal `canopy.json` (no scripts) so canopy knows this directory is a project. If you want stub setup/run/archive scripts to fill in, use `canopy init --with-scripts`. If your project already has a Conductor `conductor.json`, canopy detects it and copies the script paths verbatim — see `migrate-from-conductor.md`.

## Create a workspace

```bash
canopy new
```

This:

1. Generates a random adjective-noun name (`bold-falcon`, `silent-otter`, …).
2. Allocates a unique TCP port (40010 for the first workspace in this project, 40020 for the second, ...).
3. Creates a git worktree at `~/.canopy/workspaces/<project>/<name>` based on the latest `origin/<default-branch>`.
4. Runs `scripts.setup` if your `canopy.json` has one.
5. Builds a 3-pane tmux session: nvim top-left, claude top-right, shell full-width on the bottom.
6. Attaches you to it.

`canopy new --name fix-bug` if you want to pick the name yourself. `canopy new --no-attach` if you want it built but not attached.

Inside the session, `prefix-d` (typically `Ctrl-b d`) detaches you back to your shell. The tmux session keeps running.

## List, switch, remove

```bash
canopy ls            # workspaces in this project
canopy ls --all      # everything across every project
canopy switch <name> # attach (resurrects after a reboot)
canopy rm <name>     # tear down (asks first; -y skips)
```

`canopy ls` shows a `TMUX` column with `●` (alive) or `○` (dead) at a glance.

## The main session

```bash
canopy main
```

Opens (or attaches to) a tmux session anchored at the project root, with `CANOPY_PORT` set to the project's base port (40000 for the first project, 41000 for the second, etc.). Useful when you want to work on the main branch without making a worktree. `bin/dev` in this session's shell pane binds to that base port — won't collide with workspaces.

## Clean up

```bash
canopy rm <name> -y    # one workspace
canopy reconcile       # if state.json has drifted from reality
```

`canopy reconcile` is the answer to "I killed a tmux session manually and now `canopy ls` looks wrong." Walks the state, updates statuses to match disk + tmux truth.

## Next steps

- `docs/canopy-json.md` — full config schema reference
- `docs/migrate-from-conductor.md` — for `cravd` and anything else that already has `conductor.json`
- `docs/troubleshooting.md` — common problems and fixes
- `docs/architecture.md` — how the code is laid out (for contributors)
