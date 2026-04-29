# Changelog

All notable changes to canopy are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and canopy adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- MIT license + README license badge.
- GitHub Actions CI: unit + e2e test runs on push/PR.
- `docs/landscape.md` — public-facing positioning ("where canopy fits").
- ASCII header in README for terminal-native vibes.
- Status badges now carry a 1-rune shape glyph (`…` setting_up, `⏸` stopped, `✗` broken, `!` orphaned) so the workspace state reads under protanopia and on monochrome terminals, not just by color. Healthy and main rows stay clean. The live `●`/`○` badge still conveys aliveness.
- **Persistent TUI (v0.7).** Three new ways to reach canopy without leaving your work:
  - `canopy popup` — launches the global TUI in a tmux floating popup. Bound to `<prefix>g` by `canopy install tmux`. Picking a workspace switches your client to it and closes the popup, no detach round-trip.
  - `canopy statusline --format=current` — single-line tmux status-bar widget showing the current workspace's name, status glyph, and port. Panic-safe (defer-recover protects your status bar) and tmux-injection-safe (escapes `#` to `##`).
  - `canopy install tmux` — idempotent `~/.tmux.conf` writer with `--force`, `--dry-run`, marker-block detection, timestamped backup, and symlink-aware atomic write (preserves stow/chezmoi setups). Embeds the running binary's path so dogfood vs system install both work.
- **Local / Global tabs in popup.** Tab switches between current-project workspaces and all-projects view. Local pre-selected when launched from inside a project, Global otherwise. Resolution is workspace-path-aware so `<prefix>g` from inside a worktree finds its parent project correctly.
- **Fuzzy search in popup.** `/` enters search mode; query matches name, project, OR branch via subsequence. `Esc` clears, `Enter` exits search keeping the filter.
- `canopy run` — execs `scripts.run` from the nearest `canopy.json` via `syscall.Exec`. Inherits `CANOPY_PORT` and friends from the workspace tmux session. Bind it manually if you want a one-keystroke server start; default block stays out of conflict-prone keys.
- `canopy retry --force` — allow retry on `ready` / `stopped` workspaces (re-runs `scripts.setup` on a healthy workspace; useful for refreshing DB schema or agent briefings). Always refuses `setting_up` (concurrent setup hazard) and `orphaned` (no on-disk dir).

### Changed
- README install section reorganized: explicit Linux / macOS / Windows (WSL2) one-liners; nvim called out as a hard requirement.
- Design doc keymap (`docs/design/v0-canopy.md`) synced to the actual bindings: `g`/`G`/home/end for first/last, `R` for retry-broken, `b`/esc for back-to-global, `ctrl+c` quit. Added a one-liner about modal pickers in the new-workspace flow.
- `tmux attach` calls inside the project TUI now auto-route to `tmux switch-client` when running inside an existing tmux session (popup mode, or any nested invocation). Single helper (`attachVerbForCurrentEnv`) at the wrapper layer; every attach site benefits.
- Pressing `o` in popup-mode global TUI now opens the project TUI inside the popup (was previously unsupported — failed with "exit status 1" because the spawned canopy hit its own nested-tmux guard). The spawned canopy bypasses the guard via `CANOPY_ALLOW_NESTED=1`. Successful attach from within sends a popup-close signal (exit code 7) so the popup goes away cleanly without an extra `q`.

### Deprecated

### Removed

### Fixed
- Popup `o` (open project) no longer prints "exit status 1" — the spawned canopy now bypasses the nested-tmux guard cleanly.
- Resurrecting a stopped workspace from inside the popup actually works now — the `tmux attach` from project TUI auto-routes to `switch-client` because the popup pty is itself inside a tmux client.
- Popup launcher now passes the calling pane's cwd to `tmux display-popup -d`, so the Local tab finds the right project even when launched from inside a workspace dir (was inheriting tmux server's cwd).
- Popup statusline runs with `allow-in-tmux` annotation — was previously refused by the nested-tmux guard every time tmux re-ran `#(canopy statusline)`.

### Security
- Statusline output escapes `#` → `##` so a hostile branch name like `feat#[bg=red]gotcha` cannot inject style sequences into tmux's `status-right`. Cosmetic-to-misleading impact mitigated; not RCE.

## [0.1.0] — TBD (first public release)

The first canopy release with a public home. Highlights of what's in
this release (which has been accumulating across the pre-v0.1 dogfood
cycle):

### Workspace lifecycle
- `canopy init`, `canopy new`, `canopy ls`, `canopy switch`, `canopy rm`,
  `canopy reconcile`, `canopy main`, `canopy retry`, `canopy version`.
- Per-workspace git worktree under `~/.canopy/workspaces/<project>/<name>/`
  (centralized storage; source repo stays clean).
- Three lifecycle scripts in `canopy.json`: `setup` (once on create), `run`
  (reserved for on-demand `canopy run`), `archive` (on remove).
- Workspace state machine: `setting_up` → `ready` / `broken`; `ready` ↔
  `stopped` (resurrectable); `orphaned` for vanished dirs.

### TUI
- Bubbletea TUI launches when `canopy` is invoked with no subcommand.
- Three-pane workspace layout: nvim (top-left), claude (top-right), shell
  (full-width bottom).
- Global TUI when run from outside any project: every project + every
  workspace + every alive `<project>-main` session canopy knows about.
- Init splash when `canopy` is run in a fresh git repo with no `canopy.json`.

### Recovery + diagnosis
- `canopy retry <name>` re-runs `scripts.setup` against an existing broken
  workspace (preserves worktree, branch, port, claude conversation history).
- Auto-detect setup hints: known stderr signatures (Rails master key,
  database-already-exists, network blip, missing bundle, etc.) surface as
  one-line user-facing diagnoses.

### Safety
- Refuse to run from inside an existing tmux session by default. Carve-outs
  for `canopy version` and `CANOPY_ALLOW_NESTED=1`.
- Per-script env: `CANOPY_WORKSPACE_PATH`, `CANOPY_ROOT_PATH`, `CANOPY_PORT`.
  `CONDUCTOR_*` aliases for zero-friction Conductor migration.

### Docs
- `docs/getting-started.md`, `docs/landscape.md`, `docs/canopy-json.md`,
  `docs/migrate-from-conductor.md`, `docs/troubleshooting.md`,
  `docs/architecture.md`, `docs/design/v0-canopy.md`.

[Unreleased]: https://github.com/oncactus/canopy/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/oncactus/canopy/releases/tag/v0.1.0
