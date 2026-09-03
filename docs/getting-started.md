# Getting started

A 5-minute tour. Assumes Linux (Arch / Omarchy ideal), `git >= 2.30`, `tmux >= 3.2`, and Go 1.22+ already installed. (macOS works too — the build is platform-agnostic Go — but the dogfood loop runs on Arch.)

## Install

One-liner:

```bash
curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh
canopy version
```

That clones canopy to `~/.canopy/src`, runs `make install`, and prints a PATH hint if `~/.local/bin` isn't on your shell's PATH. Idempotent — re-running on an already-installed machine prints "run canopy upgrade instead" and exits 0.

Output of `canopy version` looks like:

```
canopy v0.17.1.0+abc1234
  binary:    /home/you/.local/bin/canopy -> canopy.bin
  commit:    abc1234
  built:     2026-05-13T12:34:56Z
  mode:      release
```

## Install tmux keybinds (optional but recommended)

```bash
canopy install tmux
tmux source-file ~/.tmux.conf
```

That writes a managed block to `~/.tmux.conf` (with a timestamped backup) wiring three things:

- `<prefix>g` and `Ctrl+Alt+c` (prefix-less) — summon the canopy TUI in a tmux popup
- `<prefix>r` — run `scripts.run` (your dev server) in a popup
- `status-right` — show the current workspace name in the tmux status bar

The block is idempotent (re-running refuses unless you pass `--force`), so it's safe to commit `~/.tmux.conf` and re-install on a new machine.

## Onboard a project

Four shapes, all equivalent ways of "make this a canopy project":

```bash
# Inside the repo (the classic shape):
cd ~/code/myproject
canopy init                       # writes canopy.json (no scripts)
canopy init --with-scripts        # also drops stub bin/canopy-{setup,run,archive}

# From anywhere on your laptop (v0.20+):
canopy init ~/code/myproject      # init a folder without cd-ing in
canopy init https://github.com/foo/bar.git    # clone + init in one shot
canopy init https://github.com/foo/bar.git ~/code/bar    # explicit dest

# On a remote canopy host (v0.20+, requires `canopy host add` first):
canopy init https://github.com/foo/bar.git --on tower
```

For the URL form, canopy clones into a configurable **source-root** (default `~/.canopy/sources`; change with `canopy config set source-root ~/Work`). Press `,` from any TUI tab to edit the source-root visually.

If the project already has a Conductor `conductor.json`, canopy detects it and copies the script paths verbatim — see [`migrate-from-conductor.md`](migrate-from-conductor.md).

Edit the scripts to do whatever per-workspace setup your project needs (database, dependency install, secrets symlink), then commit them. Schema details: [`canopy-json.md`](canopy-json.md).

You can also drive Add Project from the TUI: launch `canopy` in a fresh repo (or press `a` on the Global tab in any existing TUI session) and paste a path or URL. Press `Tab` to cycle between local and registered hosts; the violet pill on the Target line tells you whether the dispatch is going to a remote machine.

## Create a workspace

```bash
canopy new
```

This:

1. Generates a random adjective-noun name (`bold-falcon`, `silent-otter`, …) — or use `--name fix-bug` to pick.
2. Allocates a unique TCP port (40010 for the first workspace in this project, 40020 for the second, etc.).
3. Creates a git worktree at `~/.canopy/workspaces/<project>/<name>` based on the latest `origin/<default-branch>`.
4. Runs `scripts.setup` if your `canopy.json` has one.
5. Builds a 3-pane tmux session: `nvim` top-left, `claude` top-right, shell full-width on the bottom.
6. Attaches you to it.

Other ways to spawn:

```bash
canopy new --pr 1214                                 # check out a GitHub PR
canopy new --issue 42                                # fresh branch; briefing seeded from issue body
canopy new --branch existing-feature                 # check out a remote branch
canopy new --prompt "fix the timezone bug"           # send claude an opening message
canopy new --prompt-file ./briefing.md               # multi-line prompt (max 32 KB)
canopy new --prompt "..." --no-attach                # fire-and-forget; check the TUI for state
```

Inside the session, `prefix-d` (typically `Ctrl-b d`) detaches you back to your shell. The tmux session keeps running.

## Rename the branch — canopy follows

Workspace identity follows the live git branch. Right after creation:

```bash
git branch -m fix-timezone-edge-case
```

Within 15 seconds, the tmux session name, statusline, terminal-tab title, and TUI rows all update to match. If you want an immediate refresh:

```bash
canopy rename
```

Power users who rebase often (or check out multiple feature branches in one worktree) can pin the label:

```bash
canopy rename --pin       # freeze the current branch name
canopy rename --unpin     # resume auto-tracking
```

## List, switch, remove

```bash
canopy ls                # workspaces in this project
canopy ls --all          # everything across every project + remote host
canopy switch <name>     # attach (resurrects after a reboot)
canopy rm <name>         # tear down (asks first; -y skips)
```

Sample output of `canopy ls --all`:

```
TMUX  PROJECT   NAME           BRANCH                    STATUS  PORT   SESSION
●     canopy    (main)         —                         main    40000  canopy/main
●     canopy    polite-vale    update-readme-and-docs    ready   40010  canopy/update-readme-and-docs
●     cravd     (main)         —                         main    41000  cravd/main
●     cravd     pr-1214        pd/follow-up-strategies   ready   41020  cravd/pd-follow-up-strategies
○     hey-cli   (main)         —                         main    42000  hey-cli/main
```

`●` = live tmux session, `○` = dead. The `○` flips to `●` automatically on `canopy switch`, which resurrects the session by re-running `scripts.run` and rebuilding the pane layout. Claude's per-cwd conversation history is preserved.

## The TUI

`canopy` with no arguments launches a Bubbletea TUI. Same data as `canopy ls`, plus interactive verbs:

| Key | Action |
|---|---|
| `enter` | attach to selected workspace (resurrects if stopped) |
| `n` | create a new workspace (picker: Fresh / Prompt / PR / Issue / Branch) |
| `d` | delete workspace (confirm gate) |
| `K` | kill tmux session (workspace survives on disk) |
| `R` | re-run `scripts.setup` (recovery for `broken` rows) |
| `i` | inspect — drawer with process tree, logs, env, tmux state |
| `b` | bare shell attach (skip setup/scripts) — inside the drawer |
| `P` | open the workspace's PR in your browser (via `gh pr view --web`) |
| `B` | open the running app at `http://localhost:<port>` (xdg-open) |
| `U` | upgrade canopy (or upgrade a remote host on the Hosts tab) |
| `D` | dismiss the current available-upgrade pill |
| `r` | refresh all (state, PR status, upgrade cache) |
| `/` | fuzzy search by name / branch |
| `Tab` / `Shift+Tab` / `←` / `→` / `h` / `l` | cycle between tabs |
| `?` | help legend (glyphs, badges, keybinds) |

Tabs: **<project>** (the current project's workspaces — only when you launched inside a project), **Global** (every workspace across every project), **Remote hosts** (registered SSH boxes).

### Agent-state badges

Each row shows what the agent pane is doing, polled every 2 seconds:

| Badge | Meaning |
|---|---|
| `⚡` (cyan) | claude is thinking |
| `💤` (gray) | claude is idle, ready for your next message |
| `✋` (yellow) | claude is awaiting input (y/N or tool-permission popup blocking) |
| `·` (subtle) | no agent pane / pane crashed to shell |

Pair these with `--prompt --no-attach` to run multiple claudes in parallel and triage by badge.

### Health badges

The HINTS column surfaces problems before they bite. Inferred from git plumbing on every refresh:

- `⚠ conflict` — merge conflict against `origin/<default>`
- `⚠ rebasing` / `merging` / `pick` / `detached` — git is mid-operation
- `↑N ↓N *N` — N commits ahead of `origin/<default>`, N behind, N dirty files
- `⇡N` / `⇅` — N commits unpushed to upstream, or upstream has diverged
- PR status — open / approved / merged / closed

## The main session

```bash
canopy main
```

Opens (or attaches to) a tmux session anchored at the project root, with `CANOPY_PORT` set to the project's base port (40000 for the first project, 41000 for the second, etc.). Useful when you want to work on the main branch without making a worktree. `bin/dev` in this session's shell pane binds to that base port — won't collide with workspaces.

## Clean up

```bash
canopy rm <name> -y      # one workspace
canopy reconcile         # if state.json has drifted from reality
```

`canopy reconcile` is the answer to "I killed a tmux session manually and now `canopy ls` looks wrong." Walks the state, updates statuses to match disk + tmux truth.

## Remote workspaces (optional)

For heavy work, register an SSH-reachable host once:

```bash
canopy host add tower cassy@tower.tail.ts.net    # or --interactive for a guided form
canopy project add your-project /home/cassy/Work/your-project --on tower
```

Then dispatch from anywhere:

```bash
canopy new --on tower                              # workspace lives on tower
canopy new --on tower --prompt "fix the bug" --no-attach
canopy switch --on tower fix-the-bug               # attach via mosh+tmux
canopy switch --on tower fix-the-bug --no-mosh     # ...or an ssh reconnect-loop instead of mosh
```

Full guide with hosts.json schema, ssh-copy-id flow, and TUI-side keys: [`remote-workspaces.md`](remote-workspaces.md).

## Next steps

- [`canopy-json.md`](canopy-json.md) — full schema reference
- [`remote-workspaces.md`](remote-workspaces.md) — v0.17 remote dispatch
- [`migrate-from-conductor.md`](migrate-from-conductor.md) — if you already have `conductor.json`
- [`troubleshooting.md`](troubleshooting.md) — common problems and fixes
- [`architecture.md`](architecture.md) — how the code is laid out (for contributors)
