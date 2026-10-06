# canopy

Git worktree workspaces with paired terminal sessions, per-project setup scripts, stable ports,
and agent-aware badges, for running several AI coding agents in parallel. One TUI sees every
workspace.

`canopy new` and a few seconds later you are attached to a tmux session with your editor, an
agent (`claude` by default) and a shell, on a fresh worktree with its own port block. A sidebar
on the left lists every project and workspace with agent badges, and the current workspace's
tmux windows as tabs. Reboot, `canopy switch <name>`, and you are back where you left off, agent
conversation included.

```
┌──────────┬────────────────────────┬────────────┐  tabs = tmux windows (top status bar)
│ ◆ canopy │ nvim                   │ claude     │
│ ▾ myapp  │                        │            │
│  ●⚡fix-tz│                        │            │
│   ● 0 work                        │            │
│   ○ 1 run:web                     │            │
│   + tab  │                        │            │
│  ●💤 bold │                        │            │
│ ▸ cravd 3├────────────────────────┴────────────┤
│          │ $                                   │
└──────────┴─────────────────────────────────────┘
```

Each workspace row carries a dim detail line with its port, PR number and CI state, and
ahead/behind counts. Each project also lists `main`: the repo root checkout as its own session (editor, agent, shell
at the root, port = the project's base). `⏎` on it attaches, creating the session if needed;
`K` stops it. The sidebar is modal while focused: `prefix+b` enters it, `q` or `prefix+b` leaves
it, and tmux pane navigation neither enters nor leaves it. `⏎` selects a workspace, main or tab
and keeps you in the sidebar. Sidebar keys: `⏎` select, `space` collapse a project, `n` new workspace (fresh,
PR, issue or branch, `←`/`→` to switch source, type to filter; opens as a full-screen tmux
popup), `a` add a project (path or git URL, also a popup), `t` new tab, `d` delete (or forget an empty project), `K` kill session, `q` collapse. Collapsed, the
sidebar is a 3-column strip showing the mark and the ✋/⚡ counts. Open/closed and the folds are
one shared state across every session (`~/.canopy/sidebar.json`), so switching workspaces never
changes what you see; it stays as you left it until you change it. `[sidebar] auto_collapse =
true` switches to focus-following instead (collapse on leave, expand on enter). `prefix+b`
cycles expand / focus / collapse; `prefix+g` opens the full
dashboard as a popup. Keybinds are only added where your tmux has nothing bound; tab styling
only where you kept tmux defaults; everything else is set per canopy session.

This is the Rust rebuild of [canopy v0 (Go)](https://github.com/avinashjoshi/canopy-archive) on a
server runtime: a persistent server owns state and sessions, thin clients (TUI and CLI) talk to
it over a newline-JSON Unix socket API with an event stream. See
`docs/design/v1-architecture.md`.

Status: **Phase 1 + remote thin client** on the tmux backend. The native PTY backend (Phase 3)
is not here yet.

## Install

One line, no root, no package manager:

```
curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh
```

It picks the build for your machine (Linux x86_64 / arm64, macOS Intel / Apple silicon),
verifies the checksum, and puts a single `canopy` binary in `~/.local/bin`. Then:

```
canopy doctor                     # tmux, git, agents, server, hooks: what is missing, if anything
```

canopy needs **tmux ≥ 3.2** and **git**. `gh` (GitHub CLI) is optional but powers the PR/issue
pickers and badges; an agent CLI (`claude`, `codex`, `opencode`, `gemini`, `aider`) is what
opens in the agent pane.

```
sudo pacman -S tmux git github-cli        # Arch / Omarchy
brew install tmux git gh                  # macOS
sudo apt install tmux git gh              # Debian / Ubuntu
```

The first `canopy` command starts the server, which configures tmux at runtime (sidebar toggle
on `prefix+b`, tabs at the top, dashboard popup on `prefix+g`, statusline) without touching
`~/.tmux.conf`, and wires Claude Code's hooks so the agent reports its own state (backing up
`~/.claude/settings.json` first; `[integrations] auto_install = false` opts out). Existing v0
workspaces in `~/.canopy/state.json` are imported on first run.

Other ways in:

- **From source**: `cargo install --git https://github.com/avinashjoshi/canopy canopy`, or clone
  and `cargo build --release && ln -sf $PWD/target/release/canopy ~/.local/bin/canopy`.
- **A specific version**: `CANOPY_VERSION=v1.2.3 sh install.sh`; another directory:
  `CANOPY_BIN_DIR=/usr/local/bin`.
- **Remote hosts**: `canopy --remote <ssh-target> --install` copies your local binary over; nothing
  to install there by hand.

### Upgrade

```
canopy upgrade                    # fetch the latest release, swap the binary, restart the server
canopy upgrade --check            # just say whether there is one
```

Re-running the install line does the same. Upgrades never touch your workspaces or tmux
sessions: the server restarts, sidebars respawn, and a server left running from an older
version is restarted automatically by the next command.

### Uninstall

```
canopy server stop
canopy integration uninstall claude      # removes the hooks from ~/.claude/settings.json
rm ~/.local/bin/canopy
rm -rf ~/.canopy                         # state, logs AND your worktrees: check first
```

## Use

```
cd ~/code/myapp && canopy init --with-scripts   # writes canopy.json + stub scripts
canopy init --from .othertool/settings.toml     # seed the scripts table from an existing file
canopy new                        # random name, attach
canopy new --name fix-tz --prompt "fix the timezone bug" --no-attach
canopy new --pr 1214              # check out a PR; briefing seeded from its body
canopy new --issue 42             # fresh branch; briefing seeded from the issue
canopy new --branch feature/x     # existing branch
canopy init https://github.com/you/repo.git   # clone into the source root + onboard
canopy                            # dashboard: projects as headers, main + workspaces under each
canopy ls [--all] [--json]
canopy switch <name>              # attach (resurrects a stopped workspace)
canopy run [script]               # launch scripts.run in a new tab (tmux window) of the session
canopy sidebar toggle             # show/hide the sidebar pane of the current session
canopy rm <name>                  # archive script, kill session, remove worktree + branch
canopy retry <name>               # re-run scripts.setup on a broken workspace (output streams live)
canopy log [name] [-f] [-n 60]    # setup log; -f follows while setup runs
canopy rename [<name>] [--pin]    # label follows the live branch (automatic every 15 s)
canopy main                       # session at the repo root (port = project base)
canopy server status|stop         # the daemon is auto-started by the first client
canopy api schema | call <method> [json]
```

### Remote hosts, zero setup

```
canopy --remote tower                 # dashboard for the canopy on `tower`; ⏎ attaches over mosh/ssh
canopy --remote tower --install       # copy this binary over first if canopy is missing there
canopy --remote user@host new --project myapp --prompt "fix the bug"
canopy --remote tower ls | switch <name> | rm <name> | stop <name> | reconcile
```

`--remote` takes any ssh target, nothing to register. Each API call rides `ssh -T host canopy
bridge` over a shared ControlMaster connection; attach is a real `mosh`/`ssh -t` into the remote
tmux session (`--ssh` forces ssh). The far side starts its server on demand.

Clipboard, no setup, both ways: while attached, the local client stays resident and mirrors
clipboards. Local → remote: text and PNG images, so `Ctrl+V` in a remote agent pastes a local
screenshot. Remote → local: anything copied in tmux (tmux's `copy-command` is pointed at the
remote machine's clipboard at runtime) or by an agent comes back to your local clipboard within
about a second, independent of whether your terminal supports OSC 52.

### canopy.json

```jsonc
{
  "scripts": {
    "setup": "bin/canopy-setup",            // once at creation; failure => broken; `retry` re-runs
    "run": "bin/dev",                        // or { "web": {"command": "bin/dev", "default": true}, ... }
    "archive": "bin/canopy-archive",         // at rm, before the worktree goes
    "timeouts": { "setup": 600, "archive": 120 }
  },
  "agent": { "type": "claude", "briefing": "...", "briefing_file": "docs/brief.md" },
  "agents": ["claude", "codex"]              // allowlist for --agent
}
```

Scripts get `CANOPY_WORKSPACE_PATH`, `CANOPY_ROOT_PATH`, `CANOPY_PORT` (… `CANOPY_PORT_END`),
`CANOPY_WORKSPACE_NAME`, `CANOPY_BRANCH`, `CANOPY_PROJECT`. For backward compatibility the core
four are also exported under the variable names of the workspace tool canopy replaced, built in,
so existing setup scripts run unchanged with no per-project configuration. Extra prefixes can be
added with `[env] prefixes` in `config.toml` or `env_prefixes` in `canopy.json`. `scripts.run`
commands are shell strings. The same variables are set in every pane of the session.

User-level defaults live in `~/.canopy/config.toml` (`canopy default-config` prints them): ports,
editor, default agent, sidebar, `[env] prefixes`, `[init] adopt_from` (files to seed scripts
from when onboarding a repo), `[ui] glyph` (the one-character mark, default `ᛉ`), named layouts.

### Ports

Projects get bases 40000, 41000, 42000 … (first come, persisted). Workspaces step by 10 inside
their project: 40010, 40020 …; the base itself is `canopy main`. Each workspace owns ten ports.
Tune in `~/.canopy/config.toml` (`canopy default-config` prints the defaults). The same file
sets the sidebar width or disables it (`[sidebar] enabled = false`), the editor command, the
default agent and named pane layouts.

## Development

canopy is developed inside canopy: `canopy new` here gives you a worktree with a warm build and
`bin/dev <args>` runs that checkout against a private sandbox (own state, socket and tmux
server), so dogfooding never touches your real workspaces.

Releases: bump `version` in `Cargo.toml`, tag `vX.Y.Z`, push the tag; CI builds every platform
and publishes the GitHub Release that `install.sh` and `canopy upgrade` read. See
`docs/releasing.md`.


```
cargo test --workspace
cargo clippy --workspace --all-targets
```

See `CLAUDE.md` for conventions and how to dogfood against an isolated home and tmux socket.
