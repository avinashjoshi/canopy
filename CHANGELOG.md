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

### Changed
- README install section reorganized: explicit Linux / macOS / Windows (WSL2) one-liners; nvim called out as a hard requirement.

### Deprecated

### Removed

### Fixed

### Security

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
