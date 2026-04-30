# Changelog

All notable changes to canopy are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and canopy adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.9.0] - 2026-04-29 — TUI polish pass

A round of usability fixes and visual polish on top of the v0.8 unified TUI.
The popup and CLI verbs both correctly resolve project context when invoked
from inside a workspace dir, branch renames flow through to the TUI on the
next refresh, and pressing enter on a dead main row brings it up instead of
erroring. The list itself reads cleaner: branch icon, gray-styled main
branch, status column carries alive-state directly (no separate dot), and
selected rows wear a `❯` caret.

### Added
- **Branch icon (`⎇`)** prefixes every Branch column entry — single Unicode cell, no Nerd Font dependency.
- **Default branch shown on (main) rows.** `state.GlobalRow.Branch` for main rows now carries the project's actual default branch (origin/main or origin/master) instead of the `—` placeholder. Rendered in gray to signal "informational, not actionable."
- **Main row status reflects alive state.** Instead of always reading `main`, the (main) row's status column now shows `running` (green) when the main session is up and `not started` (gray) when it isn't. The standalone alive-dot column is gone — status carries that info.
- **Tab auto-focuses from Global view.** When canopy is launched outside any project (currentProject is empty) and the user presses Tab to switch to Local, the cursor row's project becomes the new Local context — Tab acts as "enter the project I'm looking at."
- **`p` keybind opens the workspace's PR.** Runs `gh pr view --web` from the workspace dir. Hidden when no `pr_status` hint exists, so the help line stays clean.
- **PR check status in the hint badge.** OPEN PRs now also surface CI status — `✓ checks`, `✗ checks`, or `… checks` next to the existing PR-state badge. Sourced from `gh pr view --json statusCheckRollup`.
- **Auto-detach other clients on attach.** Switching to a workspace from a second terminal now kicks the first terminal off the session by default, so two clients don't mirror keystrokes on the same tmux session. `CANOPY_NO_DETACH=1` opts back into tmux's default multi-client behavior.
- **`Manager.EnsureMainSession`.** Idempotently brings up the project's main tmux session. Used by both `canopy main` (CLI) and the TUI's enter-on-main-row path.
- **`git.CurrentBranch`** helper for reading a worktree's current branch.

### Changed
- **Selected-row caret `>` → `❯`.** Reads more cleanly at terminal weights.
- **Project headers + "add a project" flush-left.** Was indented at column 2; now at column 0 to outdent visually from the workspace rows below (column 2). Blank line between the "add a project" hint and the help line so they don't blur together.
- **Friendlier "no canopy.json" error.** Running `canopy main` or `canopy new` outside a project now reads `this directory isn't a canopy project yet — run` `canopy init` `here (or cd into one of your existing canopy projects)` instead of leaking the internal walk-up error verbatim.
- **Cobra usage dump suppressed on RunE errors.** `SilenceUsage: true` on the root command so failures land as a one-line error instead of a flag-table wall after the message.
- **Smarter delete safety.** `safetyChecks` now reads PR state once up front and skips the "unpushed commits" warning when the PR is `MERGED` — those commits are squashed/rebased into main, not lost work. Open-PR warning only fires for `OPEN` state.
- **Reconcile refreshes branch.** `Manager.Reconcile` now also re-reads `git branch --show-current` per workspace and persists the result, so a `git branch -m` rename surfaces in the TUI on the next refresh instead of staying frozen at workspace-create time.

### Fixed
- **Popup from workspace dir resolved to worktree path, not source repo.** `routeRoot` did `config.DiscoverAndLoad(cwd)` first, which found the worktree's checked-in `canopy.json` and set `cfg.ProjectRoot` to the worktree path. Every state.GlobalRow keys by the canonical source-repo ProjectRoot, so the Local tab matched zero rows. Now `workspace.ResolveCurrentProject` wins (workspace-path-prefix match first), with `DiscoverAndLoad` only as the fallback for unregistered cwd. Extracted into `resolveProjectContext` for testability.
- **Same bug in every CLI verb.** `loadManager` had the same `DiscoverAndLoad`-first shape, so `canopy reconcile`, `canopy switch`, `canopy rm`, etc. silently no-op'd when run from inside a workspace dir (Reconcile filtered every row out by ProjectRoot mismatch). Fixed via `resolveCfgForCwd` that mirrors the route-level resolver.
- **Enter on a dead main row errored instead of starting the session.** Used to print "main session not running — run `canopy main` in a terminal to start it", which a popup user can't even reach without first closing the popup. Now calls `Manager.EnsureMainSession` to build the session, then attaches/switch-clients in the same flow. `cmd/canopy main` was refactored to share the implementation.

### Removed
- **Alive-dot column.** Was redundant for `ready` (always `●`), `stopped` / `broken` / `orphaned` (always `○`), and only added information for the `setting_up` and `main` rows. Status column carries that signal directly now.

## [0.8.0] - 2026-04-29 — TUI unification

One TUI for every canopy invocation. The popup, the global view, and the
project view collapse into one model with two tabs (Local + Global). What
this means in practice: pressing `<prefix>g` from any tmux pane opens the
same screen `canopy` shows from a terminal. No more "different screen"
jolt when navigating to a project — every entry point lands on the same
unified view.

### Added
- **One unified TUI surface.** Three Bubbletea models (project Model, GlobalModel, GlobalModel.AsPopup) and two cobra subcommands (popup, popup-inner) collapsed into one Model + one entry point. Same screen everywhere: `canopy` from a terminal, `canopy` from inside a workspace, `<prefix>g` popup. Tab key switches between Local (current project) and Global (all projects).
- **Cross-project destructive verbs.** `d` (delete) and `R` (retry) now work on any row in the Global tab, not just the current project. The confirm modal shows the project name as a safety net. Mirrors `canopy d --project=foo workspace-bar` on the CLI.
- **Retry confirmation modal.** Pressing `R` on a healthy (non-broken) workspace now opens a y/N gate instead of silently erroring. Mirrors the CLI's `canopy retry --force` friction so a muscle-memory R-press doesn't clobber state.
- **Project focus from Global tab.** Press `o` on any Global-tab row to switch the unified TUI's focus to that project — Local tab pre-populates, `n` (new workspace) becomes available. No spawn, no nested canopy. The unified TUI carries the focus context, not the parent shell.
- **Popup launches from inside workspace panes.** When you press `<prefix>g` from a workspace's tmux pane, the resolver maps the worktree path back to the canonical source-repo ProjectRoot and Local tab pre-populates with that project's workspaces. Includes a fix for symlinked cwds (`filepath.EvalSymlinks` before path-prefix matching).
- **Lazyworktree-flavored chrome.** Brand pill `◆ canopy` + scope pill on the top bar, rounded-cap tab pills via powerline glyphs (Nerd Font required), keybind cheatsheet at the bottom with each binding as `[key] desc`. Selected row highlights as a single bold-white-on-grey band with a `>` caret prefix. Tab bar always renders; empty tabs dim and show context-specific onboarding text. Search has its own `🔍 SEARCH` pill in active mode, `🔍 query  (esc to clear)` in persistent-filter mode.
- **Onboarding hint on Global tab.** Subtle reminder above the keybind cheatsheet: "add a project: cd to its repo and run `canopy init`". Hidden on Local where the project is already known.

### Changed
- **`canopy install tmux` writes the new bind shape.** `bind g display-popup -E -w 80% -h 80% -d "#{pane_current_path}" "CANOPY_IN_POPUP=1 <bin>"` — replaces the old `bind g run-shell "<bin> popup"`. The `-d "#{pane_current_path}"` is load-bearing for the Local-tab resolver. `CANOPY_IN_POPUP=1` is the env-var flag that flips the unified TUI into popup-mode rendering (single-line tab bar + switch-client attach).
- **Bare `canopy` runs inside tmux without `CANOPY_ALLOW_NESTED=1`.** The unified TUI uses `tmux switch-client` for selecting workspaces (no nested attach), so it's safe to launch from any tmux pane. `canopy new`, `canopy rm`, `canopy retry` still hit the nested-tmux guard — those genuinely need a non-tmux shell.
- **Routing simplified.** `cmd/canopy/route.go` now picks the unified TUI for almost every invocation; init splash only fires for fresh git repos with zero registered projects. When `workspace.New` fails (e.g. v1/v2 state collision), routing falls back to global mode with a warning instead of refusing to launch — destructive verbs on that project are unavailable until the underlying state is reconciled, but the user can still see and act on other projects.
- **Lazy-loaded hint badges persist across tab/search mutations.** PR-status, shipped, and rename badges no longer disappear when you press tab or type in the search box. Hints merge into the unfiltered row store; tab/search filters re-derive the rendered set from there.
- **Canopy migration auto-resolves stale v1/v2 state collisions.** When state.json contains both a legacy basename key (`cravd`) and a v2 root-path key (`/home/avi/Work/cravd`) for the same project, the basename entry is now dropped during `MigrateLegacyProject` (PortBase salvaged to v2). Previously the basename-collision guard refused to construct a Manager, blocking the unified TUI from launching from inside that project.

### Removed
- **`canopy popup` and `canopy popup-inner` subcommands.** tmux's `display-popup -E` now invokes `canopy` directly. ~600 LOC of subcommand + spawning + signal-channel code deleted.
- **Spawn-a-child-canopy popup architecture.** The `goToProject` re-exec, `CANOPY_FROM_GLOBAL` / `CANOPY_FROM_POPUP` env vars, the `os.Exit(7)` popup-attach signal, and the `attachedFromPopup` field are all gone. Single-process popup hosting via `display-popup -E` is simpler and faster.
- **Three Bubbletea models and their tests.** `internal/ui/model_global.go` (906 LOC), `internal/ui/model_global_test.go` (748 LOC), and the popup test trio (`popup_test.go`, `popup_inner_test.go`, `popup_inner_resolve_test.go`) all deleted. Net: -1452 LOC across the merge commit alone.

### Fixed
- **Cross-project delete now matches by (Project, Name) pair.** Pre-fix: two projects each with a workspace named "foo" could be ambiguous on the Global tab — a refresh between modal-open and confirm could re-order rows so the wrong "foo" got deleted. P0 data-loss bug caught by the adversarial review during /ship; regression test added.
- **Popup-from-workspace-pane finds the right project.** Previously `<prefix>g` from inside `~/.canopy/workspaces/canopy/foo` would walk up canopy.json into the worktree dir, mismatch every state.GlobalRow's ProjectRoot, and the Local tab would silently empty. Resolver now does a workspace-path-prefix lookup first and returns the canonical source-repo ProjectRoot.
- **Lazy hint loading no longer breaks across tab switches.** rowHintsMsg now mutates the unfiltered store before re-pushing the filtered set to projectlist, so hints survive every SetRows that tab-switch and search-mutation trigger.

### Added (foundations)
- `config.LoadFrom(root)` — direct read of `<root>/canopy.json` without the walk-up that `DiscoverAndLoad` does. Used by the unified TUI's transient Manager construction (cross-project d/R) where the caller already knows the canonical project root from `state.GlobalRow.ProjectRoot`.
- `internal/workspace/projectroot.go` — the popup-pane → project-root resolver, ported from `cmd/canopy/popup_inner.go` and exposed as `workspace.ResolveCurrentProject`. Two-step lookup (workspace-path prefix match → registered-project canopy.json walk-up), with `filepath.EvalSymlinks` applied first for symlink resilience. Structured DEBUG logs at every resolution branch so `~/.canopy/log/canopy.log` answers the "why is my Local tab empty?" question without rerunning under `--debug`.
- `internal/ui/keymap.go` — listMode bindings as a `[]Binding` data table wrapping `bubbles/key.Binding` with an `Action` func. Replaces the giant switch statement in `update.go`. Each binding has an optional `Available(*Model) bool` predicate that gates both help-line rendering and dispatch — `n` is hidden on Global tab via this single source of truth.

### Added
- MIT license + README license badge.
- GitHub Actions CI: unit + e2e test runs on push/PR.
- `docs/landscape.md` — public-facing positioning ("where canopy fits").
- ASCII header in README for terminal-native vibes.
- Status badges now carry a 1-rune shape glyph (`…` setting_up, `⏸` stopped, `✗` broken, `!` orphaned) so the workspace state reads under protanopia and on monochrome terminals, not just by color. Healthy and main rows stay clean. The live `●`/`○` badge still conveys aliveness.
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

[Unreleased]: https://github.com/oncactus/canopy/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/oncactus/canopy/releases/tag/v0.8.0
[0.1.0]: https://github.com/oncactus/canopy/releases/tag/v0.1.0
