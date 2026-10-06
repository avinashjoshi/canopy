# Getting started

1. Install: `curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh`,
   then `canopy doctor` (from source instead: `cargo build --release && ln -sf $PWD/target/release/canopy ~/.local/bin/canopy`).
2. In a repo: `canopy init --with-scripts`, edit `bin/canopy-setup` (keep it idempotent), commit.
3. `canopy new`. The server starts itself, configures tmux at runtime (no dotfile edits) and
   installs Claude Code hooks (with a backup of `~/.claude/settings.json`).
4. `prefix+g` opens the dashboard as a popup from any pane; `prefix+b` opens the sidebar.
5. Done. You land in a session: sidebar on the left, editor top-left, agent top-right,
   shell below. Tabs (tmux windows) show in the status bar at the top; `prefix+c` opens one in the
   workspace directory, `canopy run` opens one per run script, and the sidebar lists them under the
   current workspace.
6. `q` in the sidebar hides it; `prefix+b` brings it back.

`canopy integration install|uninstall claude` and `[integrations] auto_install = false` are there
if you want manual control. `canopy upgrade` moves you to the latest release in place.

## Where things live

- `~/.canopy/state.json` — registry (schema 3; v0's schema 2 is imported).
- `~/.canopy/workspaces/<project>/<name>` — worktrees.
- `~/.canopy/config.toml` — ports, editor, default agent, layouts, backend.
- `~/.canopy/log/server.log` — server log (`CANOPY_LOG=debug` for more).
- `$XDG_RUNTIME_DIR/canopy/canopy.sock` — API socket (`CANOPY_SOCKET_PATH` overrides).

## Lifecycle

`setting_up → ready ⇄ stopped`, `broken` (setup failed; `retry`), `orphaned` (directory gone; `rm`).

Setup output is streamed as it happens: `canopy new` and `canopy retry` print it, the dashboard
shows it while a workspace is being created (press `L` on any row for its log, or `⏎` on a row
that is still setting up), and the sidebar shows the current line under the row. The file is
`~/.canopy/log/<project>-<workspace>.log`; `canopy log <name> -f` follows it from the shell.

`canopy switch` on a stopped workspace rebuilds the session without re-running setup; the agent
resumes (`claude --resume <id>` when the hook integration has reported a session id, else
`--continue`).
