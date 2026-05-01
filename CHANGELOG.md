# Changelog

All notable changes to canopy are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and canopy adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.13.3.0] - 2026-04-30 — Workspace hints: ⚠ conflict (Lane A of 3)

First slice of the workspace-hints expansion designed in
`docs/design/v0.14-workspace-hints.md`. The TUI's hint column now
warns you BEFORE you try to ship that your branch would conflict
with main, so you fix it on your own clock instead of discovering
it at the worst possible moment.

### Added

- **`⚠ conflict` hint badge.** Every workspace gets a new
  `mergeability` detector that simulates `git merge origin/<default>`
  via `git merge-tree --write-tree` (no working tree side effects)
  and emits an orange-bold badge if the merge would conflict.
  Singular form is `⚠ conflict`; multi-conflict shows the count
  (`⚠ 3 conflicts`). Sits between the rename and git_stats badges
  so the warning reads next to the divergence counts that explain
  why.
- **Action hint for the agent.** The hint carries a suggested
  command (`git fetch origin && git rebase origin/<default>`) so
  the AGENT.md briefing and any future hover/popover have a
  ready-made fix to point at.

### Changed

- **`detect.RunFast` now spans 5 detectors instead of 4.** The new
  detector runs in the same parallel goroutine pool as the others;
  wall-clock for the full hint pass stays bounded. In steady state,
  most workspaces hit a `merge-base --is-ancestor` short-circuit
  (~10ms) and never call `merge-tree` at all. Diverged workspaces
  add ~30-80ms but don't extend the parallel pool's wall time.

This is Lane A of a three-lane series. Lanes B (`stuck_state`:
mid-rebase / mid-merge / detached HEAD) and C (`push_state`:
unpushed-to-origin separated from ahead-of-main) ship from their
own canopy workspaces and arrive in subsequent versions.

## [0.13.2.0] - 2026-05-01 — Upgrade UX polish: confirm gate, force-refresh, hints

Round of polish on the v0.13 upgrade flow surfaced by first-day
dogfooding.

### Added

- **`canopy upgrade` confirmation gate.** The CLI used to barrel
  through and run shell immediately after printing the changelog,
  making it impossible to actually read what was about to change.
  Now it prompts `Apply this upgrade? [Y/n]` after the changelog
  preview. Pass `--yes` (or `-y`) to skip; non-interactive stdin
  (CI, pipes, redirects) auto-confirms; `--force` continues to
  imply yes since forcing IS the confirmation.
- **`r` in the TUI also force-refreshes the auto-check cache.**
  The 6h TTL means that if you ship a release outside canopy
  (`make install` from `~/Work/canopy`), the pill won't notice
  until the next TTL window. Pressing `r` now busts the upgrade
  cache too — same intent as the existing "I want truth right
  now" semantic for workspace state.
- **`canopy upgrade --status` includes manual escape hatches.**
  The status output now ends with three hint lines documenting
  `canopy upgrade --check` (force refresh), `canopy upgrade
  --dismiss` (silence pill), and the in-TUI `r` / `D` keys. Makes
  `--status` the single answer to "why isn't the pill doing what
  I expect?"

### Infrastructure

- `RunUnifiedOptions` gains `RefreshOnInit bool`. The auto-check
  closure is now wired unconditionally (when not DEV) so the `r`
  key can use it; `RefreshOnInit` separately gates whether Init()
  fires it on launch. Caller (route.go) sets it from the same
  `needsRefresh` flag that previously gated wiring at all.

## [0.13.1.0] - 2026-05-01 — `canopy upgrade --status` diagnostic flag

Adds a debugging surface for the v0.13 auto-check feature. Useful for
answering "why isn't the pill showing?" or "did my dismissal take?"
without grepping JSON.

### Added

- **`canopy upgrade --status`** prints the auto-check cache contents:
  cache file path, running version, build mode (DEV vs release),
  cached `latest_version`, `dismissed_version`, time since last
  successful fetch, TTL remaining (or "expired"), and the resulting
  pill state with the suppression reason when relevant. Works on DEV
  builds (DEV doesn't auto-check, but inspecting the cache file is
  still useful diagnostically). Pure read — no network, no shell.

## [0.13.0.0] - 2026-04-30 — Upgrade UX overhaul

Canopy now tells you when there's a newer version, and lets you upgrade
without leaving the TUI.

### Added

- **Auto-check pill in the top bar.** When a newer canopy is available,
  the version pill mutates from `v0.12.3` to `v0.12.3 ⇑ v0.13.0` (yellow
  arrow). Synchronous read on TUI startup keeps launch zero-latency on
  cache hits; stale cache triggers an async refresh that lands as the
  TUI is already on screen, so the network never blocks rendering.
- **In-TUI upgrade flow (press `U`).** Four states: loading the
  changelog from GitHub, preview with the changelog rendered in a
  scrollable viewport (`j`/`k`/PgUp/PgDn/g/G), running with live-tailed
  `git pull` + `make install` output, done with success or stderr-tailed
  failure. Esc cancels pre-run; Ctrl-C cancels the running subprocess
  but stays in the flow so you can read what happened.
- **`canopy ls` hint line.** When an upgrade is available, plain `canopy
  ls` output ends with one dim line: `canopy v0.13.0 available — run
  canopy upgrade`. Pure cache read, never blocks.
- **Per-version dismissal.** Press `D` in the TUI or run `canopy upgrade
  --dismiss` to silence the pill until a newer version ships. Dismissal
  is automatic on the next release because the cache compares
  per-version, not by snooze timer.
- **Post-success restart hint.** The doneOK screen now reads "This
  canopy session is still running the old binary. Press q to quit, then
  re-run canopy to use v0.13.0." Linux/Mac keep the running inode alive
  after `make install` replaces the file; new invocations get the new
  binary, but the active TUI does not.
- **Global Ctrl-C in the upgrade flow.** Loading and preview states
  used to silently absorb Ctrl-C, breaking the convention that Ctrl-C
  always quits canopy. Now Ctrl-C quits from any pre-run state and
  cancels the subprocess from the running state.

### Changed

- `canopy upgrade --check` now writes the fetched VERSION into the
  auto-check cache so the next non-check invocation sees fresh data
  without re-fetching. Existing dismissed_version is preserved across
  --check.
- Successful `canopy upgrade` rewrites the cache to mark the running
  version as latest, with dismissal cleared. Pill disappears
  immediately instead of waiting for the next 6h refresh.

### Infrastructure

- New `~/.canopy/upgrade-check.json` cache (3 fields: `checked_at`,
  `latest_version`, `dismissed_version`), 6h TTL, atomic tempfile+rename
  writes. Mirrors the `state.json` pattern.
- New public types in `internal/ui` for the closure boundary between
  `cmd/canopy` and the TUI: `UpgradeRefreshFn`, `UpgradeChangelogFn`,
  `UpgradeShellFn`, `UpgradeDismissFn`. Keeps unexported types
  (`safeBuffer`) inside the package and lets `cmd/canopy` supply
  network/shell layers without import cycles.
- `RunUnified` signature gained 5 params for the upgrade closures
  (`initialUpgrade`, `refreshFn`, `changelogFn`, `shellFn`, `dismissFn`).

Design doc committed at `docs/design/v0.13-upgrade-ux.md`.

## [0.12.4] - 2026-04-30 — Revert org move: `oncactus/canopy` → `avinashjoshi/canopy`

Reverses v0.6's transfer of canopy to the `oncactus` org. Repo is back at
`github.com/avinashjoshi/canopy` for now while the brand strategy settles.
Mechanical change only — no behavior diff. Module path, every Go import,
README install one-liner + badges, install.sh, CHANGELOG compare links,
issue templates, and CLAUDE.md all repointed. Existing oncactus URLs keep
working via GitHub's transfer-redirect.

The historical migration plan at `docs/design/v0.6-org-move.md` and the
TODOS.md decision context are intentionally preserved verbatim — they
describe a past decision, not a current one. Contributor email
`avinash@oncactus.com` is also untouched.

### Changed
- Module path: `github.com/oncactus/canopy` → `github.com/avinashjoshi/canopy`.
  All Go imports, internal package references, and user-facing strings
  in `cmd/canopy/use.go` updated to match.
- README.md install one-liner, Go Reference badge, Go Report Card badge,
  Tests workflow badge, and `go install` examples now point at the
  avinashjoshi address.
- `install.sh` clones from `https://github.com/avinashjoshi/canopy`.
  `canopy upgrade` reads `VERSION` from
  `raw.githubusercontent.com/avinashjoshi/canopy/main/VERSION`.
- CHANGELOG.md compare links and release-tag links rewritten so the
  "see diff since last release" link in each entry now resolves.
- `.github/ISSUE_TEMPLATE/config.yml` + `feature.yml`, `CONTRIBUTING.md`,
  `docs/getting-started.md`, `docs/troubleshooting.md`, and `CLAUDE.md`
  all repointed.
- Local `git remote set-url origin` flipped on the source clone, which
  applies to every canopy worktree automatically.

## [0.12.3] - 2026-04-30 — Surface version in `canopy use` and `canopy --help`

Two small UX gaps closed: "which canopy am I running" should never
take an extra step. The release version now surfaces in both the
`canopy use` listing (per-target column) and at the top of `canopy
--help`, so the answer is always one command away — no more chasing
it through `canopy version`.

### Added
- `canopy use` listing gains a VERSION column. The release row execs
  `canopy.bin version` once and parses the version label; dev workspace
  rows show "DEV" without forking (make build is dev-by-convention).
  Missing binaries show "—". Capped at 2s per release exec so a wedged
  binary can't hang the listing.
- `canopy --help` leads with a one-line version banner — `canopy
  v0.12.2+abc1234` for releases, `canopy DEV (workspace-name)` for
  dev builds inside a known worktree, plain `canopy DEV` otherwise.
  Reuses the existing `versionDetails()` plumbing; one extra resolve
  at process start.

## [0.12.2] - 2026-04-30 — `make build` DEV banner restored + TODOs queued

The v0.12.1 squash merge dropped two commits that were pushed to the
PR after the squash. v0.12.2 lands them: the dev-sentinel preservation
fix (so `make build` binaries register as DEV again) plus the TODO
entries for the next round of UX work.

### Fixed
- `make build` binaries now correctly register as DEV. The v0.12.0
  versionDetails() BuildInfo fallback was overwriting the literal
  "dev" sentinel with Go's pseudo-version (e.g.
  "v0.0.0-20260430201846-6f65463") for in-repo builds, which tripped
  IsDev to false. That killed the cyan DEV pill in the TUI, killed
  the [DEV:branch] suffix in the tmux statusline, and made `canopy
  upgrade` cheerfully offer to upgrade dev binaries instead of
  refusing. Fixed by capturing rawVersion before the fallback runs;
  IsDev now reads from rawVersion while d.Version can still surface
  the pseudo for forensic display.

### Documentation
- TODOS.md gains two new entries: a P2 "surface version in canopy
  use and canopy --help" quick-win pair, and a P3 "upgrade UX
  overhaul" with sub-items for auto-check, TUI flow, and integration.
  Design questions are noted on the P3 sub-items because the right
  shape isn't obvious yet (cadence, storage, surface, dismissal).

## [0.12.1] - 2026-04-30 — `canopy upgrade` falls back to git for private repos

`canopy upgrade` was hitting `HTTP 404` on the VERSION fetch because
canopy's repo is private and raw.githubusercontent.com 404s anonymous
requests for private content. Fixed by adding a git-based fallback:
when HTTP fails, run `git fetch origin main && git show origin/main:VERSION`
in the local `~/.canopy/src` clone. The clone has working auth (that's
how install.sh succeeded), so the git path always works regardless of
repo visibility.

### Fixed
- `canopy upgrade` now works for private repos. The HTTP path stays as
  the fast-path for public repos; git is the fallback for private. When
  the repo eventually goes public, the HTTP path takes over automatically
  with no code changes needed — strictly faster, no git fetch overhead.
- Same fallback applies to the optional CHANGELOG preview before the
  pull, so users on private repos still see "What's new" diffs.

### Added
- `upgradeGitFetchFile` package-level var as the git path's testability
  seam (mirrors `upgradeFetchVersion`'s shape). Tests for both happy-
  path-via-git and dual-failure scenarios.

## [0.12.0] - 2026-04-30 — Source-based install, `canopy use` switcher, and visible release/DEV indicators

End-user install + dev-loop ergonomics. One curl line installs canopy from
source. `canopy upgrade` keeps it current. `canopy use` flips the active
binary between any feature build and the released one in milliseconds.
Three visual indicators (TUI top bar, tmux statusline suffix, `canopy
version`) make it impossible to forget which canopy is active.

### Added
- `install.sh` at the repo root, hosted via raw.githubusercontent.com.
  Detects OS via `uname`, checks for git/tmux 3.2+/Go 1.22+/make and
  prints the exact install command if any is missing, clones to
  `~/.canopy/src`, and runs `make install`. Idempotent: re-running on
  an already-installed machine prints "run canopy upgrade" and exits 0.
  One-liner: `curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh`.
- `canopy upgrade` — fetches `~/.canopy/src` to latest main and runs
  `make install`. Reads VERSION from raw.githubusercontent.com and
  string-compares with the running binary, prints CHANGELOG diff before
  the pull. `--check` compares without upgrading; `--force` runs even
  when versions match. Refuses cleanly on dev binaries (suggests
  `canopy use release` first) and on missing/corrupt source clones.
- `canopy use [target]` — single switcher subcommand for the active
  binary. With no args, lists current target + every workspace canopy
  knows about with built-or-not status (sorted alphabetically). With a
  target name, retargets `~/.local/bin/canopy` symlink atomically:
  `release` (alias: `main`) points at `canopy.bin`; a workspace name
  points at that worktree's `./canopy`. `--build` flag rebuilds the
  workspace binary first.
- `make dev` Makefile target — thin wrapper that builds this worktree's
  `./canopy` (no ldflags, so version stays "dev") and flips
  `~/.local/bin/canopy` at it. Works from any worktree.
- `make release` Makefile target — flips `~/.local/bin/canopy` back at
  `canopy.bin` from any worktree, with no rebuild. Refuses cleanly if
  `canopy.bin` is missing.
- VERSION file at the repo root holding the human-curated semver
  (matches gstack/gbrain convention). `canopy upgrade` and the
  Makefile's ldflags both read from it.
- TUI top-bar version pill — muted gray for release builds (`v0.12.0+abc1234`),
  cyan for dev builds (`DEV: <workspace>`). Suppressed when no version
  info is available so tests + bare invocations stay clean.
- `canopy statusline --format=current` appends `[DEV:<workspace>]` to
  the workspace segment when the running canopy is a dev build, regardless
  of which workspace's tmux session the user is in. The "I forgot I
  switched and this isn't the released canopy" reminder.
- `canopy version` now prints a structured multi-line block: version,
  commit, build date, resolved binary path (with symlink target), mode
  (release/DEV), and workspace name when running a dev build inside a
  known worktree.

### Changed
- **`canopy install tmux` now invokes bare `canopy` (PATH-resolved)
  instead of the absolute path of the binary that ran the install.**
  With `canopy use` swapping the symlink between release and dev
  binaries, bare `canopy` follows the symlink automatically — no
  `canopy install tmux --force` re-run on every binary swap.
- `make install` now writes to `~/.local/bin/canopy.bin` and symlinks
  `~/.local/bin/canopy` at it. `canopy use` flips the symlink. The
  symlink is the single source of truth for the active canopy on PATH;
  every other tool (tmux, shell aliases, `canopy upgrade`) follows it.
- `make install` injects ldflags from VERSION + git short-sha so
  `canopy version` produces a real version string for installed binaries
  without needing goreleaser. `make build` and `make dev` deliberately
  skip the ldflags so dev binaries surface as `version == "dev"` and
  the DEV banner fires.

### Documentation
- README: rewritten Install section (curl one-liner first), new Update
  section (`canopy upgrade`), new Dev workflow section (`canopy use`,
  `make dev`, `make release`, visual indicators), new Uninstall section.
- CLAUDE.md: new "Multi-workspace dogfooding" section instructing future
  Claudes to use `./canopy` or `/tmp/canopy-XX` per-workspace and never
  bare `canopy`, with rules for routing "make this active" requests to
  `canopy use` or `make dev`.
- Design doc: `docs/design/v0-install-and-dev-workflow.md` formalizes
  the source-based distribution decision (Approaches A/B/C compared),
  filesystem shape, switching state machine, and failure modes.

### Why source-based instead of goreleaser + binary releases
Canopy's audience is devs comfortable with shells; they have or can
install Go in one line. Source-based distribution matches gstack/gbrain
(the user's existing tooling), avoids `.goreleaser.yaml` + release
workflow + Gatekeeper notarization + cross-platform build matrix, and
already matches the user's "make install on main" dogfood habit. If
non-Go users ever materialize, goreleaser layers on top of this; doesn't
replace it.

## [0.11.3] - 2026-04-30 — Detach-and-remove now covers fullscreen + instant state-row drop

### Fixed
- Deleting the workspace you launched fullscreen `canopy` from no longer
  strands on-disk + state cleanup mid-Remove. The v0.11.2 detach +
  detached-subprocess shortcut was scoped to popup mode; in fullscreen,
  `escapeIfDeletingCurrent` would fall through to `busyMode + removeCmd`
  with canopy still hosted in a pane of the doomed session — when tmux
  Kill fired, canopy died and the rest of `Manager.Remove` (git worktree
  remove, branch delete, state-row drop) never ran. The path now flips
  on a (root, name) match against `currentWorkspace` regardless of mode,
  spawns the same detached `canopy rm --yes --force <name>` subprocess,
  runs `tmux detach-client`, and `tea.Quit`s. The user lands at the
  shell that started tmux; cleanup finishes in the background. When
  canopy was launched from a non-tmux shell, detach-client errors
  harmlessly and the rest of the sequence still works.
- `Manager.Remove` drops the state row up front (under the lock,
  immediately after snapshotting the row) instead of last. Reopening
  canopy in the gap between popup-close and the detached subprocess
  finishing no longer shows the just-deleted workspace as a stale row,
  and pressing enter on it no longer surfaces "tmux session not found."
  Steps 2-6 (archive, tmux kill, git remove, branch delete, log
  cleanup) work entirely off the in-memory snapshot so they don't need
  the state row to still exist. Failures after the drop leave residue
  on disk that `canopy reconcile` discovers — strictly better UX than
  a zombie row that lies for several seconds.

### Changed
- Renamed `deletingCurrentInPopup` → `deletingCurrentSession` and
  `popupDetachAndRemoveCmd` → `detachAndRemoveCmd` to reflect that the
  shortcut is no longer popup-specific.

## [0.11.2] - 2026-04-30 — Popup delete: detach instead of auto-switch

### Fixed
- Deleting the workspace you opened the popup from no longer auto-builds and
  switches your tmux client to the project main session — a flow the user
  perceived as "tmux loaded a random session" because EnsureMainSession spun
  up a fresh nvim+claude pane behind the popup. The popup now closes
  immediately, the tmux client detaches back to whatever shell started tmux,
  and the workspace cleanup runs in a detached `canopy rm --yes --force`
  subprocess (Setsid + Process.Release) so it survives the popup's exit.
  Other delete paths (deleting a different workspace from the popup, or
  deleting from fullscreen) keep using the existing busyMode + escape flow.
- `removeDoneMsg` now auto-dismisses busyMode on success instead of leaving
  the popup sitting at "Removed." waiting for a keypress. Mirrors
  `createDoneMsg`'s "drop the busy view as soon as the work is done"
  pattern. Failure path is unchanged — busyMode stays so the captured
  archive output and error are visible for diagnosis.

### Added
- `tmux.Client.DetachClient(ctx)` — wrapper around `tmux detach-client` for
  the popup-delete escape path.

## [0.11.1] - 2026-04-30 — TUI UX fixes round

### Fixed
- `o` (focus project) now works on the already-selected project — re-focusing
  is a harmless tab switch, no reason to disable the keybind. Removed the
  same-project guard. Also short-circuits the canopy.json reload in that
  case so a transiently unreadable config doesn't surface a spurious error.
- Deleting the workspace whose dir hosts the running canopy popup no longer
  strands the tmux client. Before kill-session runs, canopy ensures the
  project's main session is up and `switch-client`s the user there. Match is
  on (project root, name) so cross-project name collisions (A/foo and B/foo)
  don't trip the wrong escape target.
- Popup picker now pre-selects the workspace whose directory hosted the
  popup invocation, instead of always landing on row 0. The latch fires on
  the first non-empty refresh — early empty probes don't burn the preselect
  opportunity, and a missed match (target filtered out by tab) still latches
  so later refreshes don't yank the cursor.
- New workspaces (and resurrections) now land active focus on the Claude
  pane instead of the nvim pane — the agent is what you typed `n` to talk
  to, the cursor should be there waiting.
- `n` from the Global picker no longer panics with a nil pointer dereference.
  Three `attachCmd(m.mgr, ...)` callsites that crashed when `m.mgr` was nil
  (canopy launched outside any project) now route through the existing
  `attachOrSwitch` helper, which uses the always-set `m.tc` directly.

### Added
- "← here" indicator on the workspace row whose directory hosts the running
  canopy invocation. Cyan + bold so it stays visible even when you navigate
  the cursor away from the auto-preselected row.

## [0.11.0] - 2026-04-30 — Cross-project new workspace + picker chrome polish

`n` (new workspace) now works from the Global tab. Press `n` on any row and
canopy creates a workspace in that row's project, mirroring how `d`/`R`/`K`
already follow the cursor cross-project. The two-step `o` then `n` ritual is
gone; one keystroke does what one keystroke should.

To make cross-project intent unmissable, every screen in the new-workspace
flow (variant picker, fresh / PR / issue / branch sub-modals, busy view)
now leads with a "creating in `<project>`" banner — brand-violet chip,
repo path beside it. The chip carries through to the success line:
"Workspace created in cravd."

The new flow's chrome also got pulled into the rest of the TUI's visual
vocabulary. Variant picker letters render as the same key pill the help
line uses, dropping the red-error-coded brackets they used to wear.
PR/issue/branch sub-modals lost their bespoke filter input in favor of
the main TUI's `🔍 FILTER` pill with the same `▏` caret. Cursor caret
unified across the variant picker and all three sub-modals to match the
main workspace list's `❯` marker — one indicator per cursor position,
TUI-wide.

### Added

- **Cross-project `n` from the Global tab.** `availableNewWorkspace` now
  fires when the cursor row has a non-empty `ProjectRoot`. The action
  resolves the target project via `managerForRow(cursor)` (the same
  primitive `d`/`R`/`K` already use), surfaces config-load errors via
  `m.err` rather than panicking, and stores the resolved Manager + name
  + root in three new model fields (`newTargetMgr`, `newTargetRoot`,
  `newTargetName`) used by every submit + loader in the flow.
- **Project banner on every new-workspace screen.** A new
  `renderTargetBanner` helper emits "creating in" + brand-violet
  rounded chip + dim project root. Injected at the top of
  `renderNewPicker`, `renderNewFresh`, `renderNewPR`, `renderNewIssue`,
  `renderNewBranch`, and the busy view's create branch.
- **Project name in the create success line.** "Workspace created in
  `<project>`." closes the loop the banner opened. Empty project name
  falls back to the legacy generic line.

### Changed

- **Variant picker shortcut letters** render as `keyPillStyle` (the
  inverted-bg pill the help line uses), no longer wrapped in `[ ]` and
  no longer borrowing `brokenStyle`'s error red. Pill chrome implies
  "press this" — same convention the rest of the TUI already used.
- **PR / issue / branch sub-modal filters** replaced their bespoke
  "PR number or filter:" / "Filter:" labels with the main TUI's
  `🔍 FILTER` pill (`searchLabelStyle` + `searchInputStyle`).
  Per-modal guidance moves to a dim hint to the right of the pill,
  visible only when the value is empty. Caret renders as `▏` matching
  the main TUI's search line, since `bubbles/textinput.View()`'s block
  cursor fights canopy's vocabulary; we read `Value()` and compose the
  caret manually.
- **Cursor caret unified to `❯`** across the variant picker (was `>`)
  and all three sub-modals (was `●`), matching the main workspace
  list's selected-row glyph.
- **`branchInWorkspace`** scope check now follows the in-flight
  new-flow target when set, so the "(in workspace X)" tag in the
  PR/branch picker reflects the target project — not the launch
  context.

## [0.10.1] - 2026-04-30 — Fix: `r` now refreshes PR status

Pressing `r` (refresh) busted the in-memory RSS/CPU cache but silently
ignored the 10-minute pr_status cache, so users who just merged a PR
or pushed a review change kept seeing the stale "PR #142 awaiting
review" hint until the TTL expired. `r` now busts both — same intent,
both freshness gates.

Background ticks and reconcile keep the 10-minute TTL to stay inside
the GitHub API budget. Only deliberate user action (`r`) invalidates.

### Fixed

- **`r` now invalidates the pr_status cache.** `actionRefresh` calls
  the new exported `lifecycle.ResetPRStatusCache` alongside the
  existing `memCache.InvalidateAll`. Regression test in
  `internal/ui/effective_status_test.go` seeds the cache via
  `RunFast`, presses `r`, and asserts the cache is empty.

## [0.10.0] - 2026-04-30 — Workspace health, diagnostics, and machine-load visibility

A complete pass on workspace health and observability. The TUI list page
now shows tmux liveness, attached-client state, memory and CPU usage per
workspace, and per-row git stats (commits ahead/behind, dirty file count)
at a glance. Pressing `i` opens a read-only diagnostic drawer with the
process tree, recent log entries, and last setup-script output for the
selected workspace. `K` kills a workspace's tmux session in place.
Pressing Enter on a row whose tmux session was killed externally now
resurrects automatically instead of erroring.

The drawer is a drawer, not a dashboard. Read-only. No live tailing,
no editing. The scope cap is the load-bearing constraint.

### Added

- **List-page row glyphs** distinguish three session states at a glance:
  `⊙` (green) — tmux alive AND a client is attached, `○` (subtle grey)
  — tmux alive but no client attached, blank — no tmux session (status
  column says why).
- **Mem + CPU column** in the TUI list. Format `320M 12%` combining
  RSS summed across the process tree in every pane with summed CPU%
  (sum of `pcpu` per process; can exceed 100% on multi-core boxes).
  Heat-colored: amber > 500 MB or > 50% CPU, red > 2 GB or > 200% CPU.
  Auto-loads on first render via Bubbletea's async tea.Cmd — no `r`
  keypress required. Cached per-session for 5 s; invalidated on `K`
  (kill) so a just-killed row's column flips to `—` immediately rather
  than lagging the actual state by up to TTL seconds. CPU shown
  consistently (including `0%`) so the cell format never varies — the
  alternative "omit if < 1%" rule conflated "no data" with "idle."
- **`K` keybind** kills the highlighted workspace's tmux session with
  y/N confirm. State.json row, worktree dir, branch all survive —
  re-pressing Enter resurrects via `Manager.Resurrect`. Works on main
  rows too: kills the project's main session; Enter rebuilds via
  `EnsureMainSession`. Refused only on rows with no live session.
- **`i` keybind** opens a diagnostic detail drawer for the highlighted
  row. Shows: process tree with RSS/CPU per pane, last 20 entries
  from the per-workspace log, last `scripts.setup` output, env (port,
  paths, branch), tmux-alive + attached state. Read-only. `Esc`/`q`
  closes, `r` reloads, `b` opens a bare attach.
- **`b` keybind in the drawer** opens a one-pane shell at the row's
  directory with `CANOPY_*` env vars set, no auto-running claude/nvim,
  no `scripts.setup` rerun. For workspace rows: drops into the worktree
  path. For main rows: drops into the project root. Subsumes the v0.5
  `canopy debug` TODO. Hidden from the drawer footer except for
  broken workspaces and main rows (where it adds value over plain
  Enter); for running/stopped workspaces it's redundant clutter.
- **`Manager.BareAttach` and `Manager.BareAttachMain`** create a
  `<session>-debug` tmux session at the row's directory.
- **Per-workspace logs** at `~/.canopy/log/canopy-<workspace>.log` via
  a new `clog.fanoutHandler`. slog records carrying a `name` attribute
  are tee'd to both the global `canopy.log` and the workspace's own
  log. The drawer reads the per-workspace file directly. The handler
  shares a `*sinkRegistry` across all derived (`WithAttrs`/`WithGroup`)
  clones so there's exactly ONE lumberjack writer per workspace name
  regardless of how many `slog.With(...)` chains touch it.
- **Per-workspace setup logs** at `~/.canopy/log/setup-<workspace>.log`.
  `scripts.setup` output is captured to disk via `io.MultiWriter`
  alongside the existing live stream. The drawer surfaces it so a
  `broken` workspace tells you why on `i`.
- **Git stats badge** on every workspace row when non-zero: `↑3 ↓1 *5`
  for commits ahead of main / commits behind main / files with
  uncommitted changes. Shows alongside PR badges, not under them —
  they're complementary signals ("is this done?" vs "what's in flight
  now?"). Zero counts hidden so clean rows stay quiet.
- **Help legend (`?`)** documents every glyph and badge in one place:
  presence indicators, status column, Mem/CPU column, right-side
  badges. Reading the help once should be enough to scan rows without
  guessing.
- `tmux.PaneInfos`, `tmux.SessionLoad`, `tmux.SessionAttached`, and
  `tmux.AttachedSessions` for process-tree probing and tmux-attach
  detection.
- `state.MemCache` (caches both RSS and CPU per session, TTL +
  invalidate), `state.LoadProbe`, `state.GlobalRow` extended with
  `MemRSS`, `CPU`, and `Attached` fields.

### Fixed

- **`nvim --embed` orphan leak on every kill.** When `Tmux.Kill(session)`
  ran, the tmux session died but `nvim --embed` children — which
  interactive `nvim .` forks with deliberate session-detachment so
  they can outlive their launcher — got reparented to PID 1 and lived
  on forever, idle. Over a 4-day workstation uptime running canopy
  tests, this accumulated ~50 zombie processes eating ~1.5GB of RAM.
  `Tmux.Kill` now snapshots the pane process tree AND scans
  `/proc/*/cwd` for processes whose cwd matches a pane's cwd AND whose
  `comm` matches a known target list (currently just `nvim`), then
  SIGKILLs the union after `kill-session`. The image-name gate is
  load-bearing: a bare cwd match would SIGKILL anything happening to
  live at the workspace path, including canopy itself when launched
  from a workspace pane via the popup keybind. Regression test in
  `internal/tmux/reap_test.go`. Same reap added to
  `Tmux.KillServerAndReap` for test cleanup.
- **Stale-ready Enter bug.** Pressing Enter on a workspace row whose
  tmux session was killed externally (e.g., `tmux kill-session -t ...`
  from another terminal) used to fail with `tmux.AttachCmd(...): tmux:
  session not found`. The Enter handler now re-checks `row.Alive`
  freshly probed by `BuildGlobalRows` and routes stale-ready rows
  through `Resurrect` automatically. Regression test in
  `internal/ui/effective_status_test.go`.
- **`var log = clog.Pkg(...)` package-init pattern broke the per-workspace
  fan-out.** Go's package-init order runs `var log = clog.Pkg("name")`
  at file scope BEFORE `main()` runs `clog.Init()`. The previous
  implementation snapshotted `slog.Default()` at `Pkg()` call-time,
  freezing the binding to the pre-Init stderr handler — so no
  package's `log.Info(...)` ever reached the fan-out wired up later
  in `Init()`, and no `canopy-<ws>.log` files were ever created in
  production. `Pkg` now returns a forwarding handler that resolves
  `slog.Default()` on every record. Regression test verifies fan-out
  reachability when `Pkg` is called before `Init`.
- **`fanoutHandler.WithAttrs`/`WithGroup` writer duplication.** Each
  derived handler had its own `writers` map; two `slog.With(...)`
  chains for the same workspace would open two `lumberjack.Logger`
  instances at the same file path, racing on rotation and leaking file
  descriptors at teardown. Hoisted writers into a shared
  `*sinkRegistry` so all derived handlers see exactly one lumberjack
  per workspace name.
- **`shipped` detector false positive on fresh forks behind main.**
  When a workspace branch had zero unique commits and main had
  fast-forwarded past the branch's start, the merge-commit-style
  ancestor check returned true and the badge fired "✓ shipped (local)"
  on actively-in-progress branches. Detector now uses squash-merge
  detection only (via `git cherry`); the merge-commit case is
  ambiguous from current git state alone with the fresh-fork case, so
  we don't claim shipped without recording the branch base commit
  (future work). Squash-merge workflows (the default `gh PR merge`
  path) keep working. Regression test:
  `TestDetectShipped_FreshForkBehindMain`.
- **Row position shifted right on selection.** Selected rows had a
  `❯ ` caret + presence-glyph (4 chars before name) while non-selected
  rows had only the presence glyph (2 chars), so moving the cursor
  onto a row visually shifted everything 2 columns right. Both branches
  now share the same 4-char prefix; columns stay rock-still as the
  cursor moves. Regression test:
  `TestRender_NameColumnAlignmentDoesNotShiftOnSelection`.
- **`killDoneMsg` cache invalidation lied.** Comment claimed the load
  cache was invalidated for the killed session; code didn't actually
  call `Invalidate`. After K, a later resurrect on the same row
  served up to TTL seconds of stale RSS/CPU from the dead session.
  Now plumbs the session name through `killDoneMsg` and invalidates
  on receipt.
- **Drawer cross-project name collision.** The drawer's stale-load
  guard checked `forName` only, not `forRoot`. In the global TUI,
  opening drawers in quick succession on `feature-a` (project-A) →
  close → `feature-a` (project-B) could land project-A's data in
  project-B's drawer if the load arrived after the switch. Guard now
  matches on `(forName, forRoot)`.

### Changed

- **Status column vocabulary unified across workspace and main rows.**
  Workspace `ready` rows display as `running` (matching what main rows
  already said). Stale-ready rows (recorded `ready` but freshly-probed
  `Alive=false`) display as `stopped` instead of lying with `ready`.
  `setting_up` renders with a space (`setting up`). Pure display-layer
  changes — `state.Status` enum values are unchanged.
- **Status glyph helper now matches the displayed status.** Previously
  the row glyph was driven by raw `state.Status`, so stale-ready rows
  rendered as `(blank) stopped` (ready glyph + stopped text) — visually
  inconsistent. The new `displayGlyph(row)` mirrors `displayStatus(row)`
  so glyph and text always agree.
- **`shipped` badge label.** Renamed `✓ shipped (local)` → `✓ merged`
  to reflect what we actually detect (commits in the default branch
  via squash-merge), not "production deployment." The "(local)"
  qualifier was an implementation detail of the detector that didn't
  add user-facing value.
- **Main rows are first-class for lifecycle ops.** Reified the
  principle that `(main)` rows *are* workspaces for session-lifecycle
  operations (kill tmux, inspect, bare attach, attach) and *aren't*
  for identity operations (delete, retry-setup, new). `K`, `i`, and
  `b` now work on main rows. `d` (delete) and `R` (retry) stay refused
  on main — those operate on workspace identity, which main doesn't
  have.
- **`isErrSessionNotFound` uses `errors.Is`** instead of substring
  matching. The "import cycle risk" the comment alluded to never
  existed: the ui package already imports tmux for `*tmux.Client`.
  Substring matching would have silently broken if tmux ever rephrased
  the error or internationalized it.

### Cleanup

- `canopy rm <ws>` now removes the per-workspace log file
  (`canopy-<ws>.log`) and setup log (`setup-<ws>.log`) alongside the
  state row and worktree.

## [0.9.1] - 2026-04-29 — Force screen no longer fires after PR auto-merge

When GitHub auto-deletes the remote branch on PR merge (the default
"Automatically delete head branches" flow), `canopy rm` no longer shows
the red force-delete screen warning about "unpushed commits." The
local commits past `origin/main` are work that already landed in
squashed form before the branch was retired — flagging them as
data-loss risk was the bug. The new check also covers the case where
`gh` is unavailable, unauthenticated, or rate-limited, so a missing
gh signal alone can't trigger a false force-screen.

### Fixed
- **`canopy rm` no longer false-positives on auto-deleted branches.** The safety preflight now treats "upstream was tracked, remote-tracking ref is now gone" as a "merged" signal, alongside the existing `gh pr view → MERGED` check. Either signal is sufficient. Reads `branch.<name>.{remote,merge}` directly from `.git/config` rather than `@{u}`, since `@{u}` itself fails once the remote ref is pruned — which was hiding exactly the state we needed to detect.

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

[Unreleased]: https://github.com/avinashjoshi/canopy/compare/v0.12.4...HEAD
[0.12.4]: https://github.com/avinashjoshi/canopy/releases/tag/v0.12.4
[0.8.0]: https://github.com/avinashjoshi/canopy/releases/tag/v0.8.0
[0.1.0]: https://github.com/avinashjoshi/canopy/releases/tag/v0.1.0
