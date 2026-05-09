# TODOs

Items deferred during /office-hours and /plan-eng-review on 2026-04-27.
Each entry is self-contained for someone (you, future-Claude, or another AI agent) picking it up months later.

---

## ✅ DONE 2026-04-30 — Fullscreen+current delete strand (v0.11.3)

The fullscreen+current case now reuses the v0.11.2 popup detach pattern
(spawn detached `canopy rm`, `tmux detach-client`, `tea.Quit`) instead
of reordering `Manager.Remove`. Resolution lives in the UI layer
(`detachAndRemoveCmd` in `internal/ui/update.go`) — `Manager.Remove`'s
step ordering is unchanged. Aligns with the explicit memory guidance to
prefer detach + detached-subprocess over a workspace-layer reorder.

Residual hazard: a bare-shell `canopy rm $current` typed from inside
the doomed session's pane still strands the cleanup process when tmux
Kill fires (the CLI has no UI layer to spawn a detached subprocess
from). Niche — accept and document if it bites someone in practice.

---

## P2 — Deferred review findings from v0.10 ship (2026-04-30)

The `tmux-health-and-resurrect` ship-review surfaced several smaller findings
that were not blocking but worth following up. Each is self-contained:

### load cache: thundering herd on first refresh

**Where:** `internal/state/mem.go:GetLoad` (and `Get`).

**What:** The cache pattern is read-mutex / drop / probe (slow) / re-mutex.
Multiple concurrent goroutines for the same session each see "no fresh entry,"
each spawn a `ps -A` probe, all write back. On a TUI refresh after `r` (which
`InvalidateAll`s), every workspace probe runs N times if the goroutines for
the same session happen to overlap. Probably fine in steady state, but a
measurable cost on cold-cache refreshes.

**Fix:** singleflight pattern (`golang.org/x/sync/singleflight`) keyed by
session, OR hold the mutex across the probe call (simpler, blocks unrelated
sessions for the duration of one probe).

### git_stats: no cache + N×4 goroutines per refresh

**Where:** `internal/lifecycle/git_stats.go:detectGitStats`.

**What:** RunFast spawns 4 detector goroutines per workspace; the TUI calls
RunFast for every workspace per refresh. With N workspaces in the global TUI,
that's 4N concurrent goroutines each running 3+ git subprocesses. On a
50-workspace global view, ~600 git processes per refresh. The 10-min cache
helps `pr_status` but `git_stats` has no cache and runs every refresh.

**Fix:** cache stats results with a short TTL (e.g., 30s) keyed by
`(workspace, HEAD-sha)`, OR debounce RunFast calls per workspace, OR limit
goroutine concurrency with a semaphore.

### Kill: PID-reuse race in cwd-walk reap

**Where:** `internal/tmux/session.go:Kill` reap loop.

**What:** `pidsToReap` is captured before `kill-session`. After kill-session,
the loop SIGKILLs every PID from the snapshot. If any PID exited and was
reused before SIGKILL fires, we kill an unrelated process. Default Linux
`pid_max` (4M) makes this rare, but on container configs with low pid_max
(32K) or busy systems it's plausible within the ms window.

**Fix:** before each SIGKILL, re-stat `/proc/<pid>/stat` or `/proc/<pid>/cwd`
and only kill if the process still matches the recorded cwd / comm. The
image-name gate added in v0.10 mitigates the wrong-target case, but doesn't
fully close the race.

### tailFile: scans full file (up to 10MB) per drawer open

**Where:** `internal/ui/drawer.go:tailFile`.

**What:** Reads the file from the start with `bufio.Scanner`, ring-buffering
the last N lines. For lumberjack-rotated logs (10MB max, 3 backups) that's
up to ~10MB scanned every time the user opens the drawer or presses `r`
inside it.

**Fix:** seek to a tail window first (e.g., `Seek(size - 64KB, ...)`) and
scan from there — same pattern as `readBoundedFile` already uses for setup
logs. Trim partial first line after seek.

### Setup logs: no isSafeWorkspaceName guard

**Where:** `internal/workspace/setup_log.go:openSetupLog`,
`removeSetupLog`.

**What:** `name` is interpolated directly into the on-disk path without
validation. Today callers pass `state.Workspace.Name` which is sanitized at
creation, so this is theoretical. But it's a defense-in-depth gap relative
to `clog.RemoveWorkspaceLog` which DOES guard the same path family. Also:
a workspace named `v1.2` (allowed by `git.Sanitize`'s dot keep) round-trips
fine through setup_log.go but is rejected by `isSafeWorkspaceName`, so the
per-workspace clog stream silently disappears for such names.

**Fix:** share one validator. Move `isSafeWorkspaceName` to `internal/git`
next to `Sanitize`, or call it from both layers. Align the dot rule across
both layers (probably: reject dots in both, since they'd mess with file
extensions anyway).

### Drawer setup log: ANSI escapes break TUI layout

**Where:** `internal/ui/drawer.go:drawerLoadCmd` setup-log section.

**What:** Setup log content is displayed verbatim. If the user's setup
script writes raw ANSI escape sequences (color, cursor moves, terminal
resets), those escapes render in the drawer's lipgloss-styled view, breaking
layout (cursor jumps, color bleed into the rest of the TUI).

**Fix:** strip ANSI from `setupLog` before rendering, similar to projectlist's
`stripAnsi`. Move `stripAnsi` to a shared internal helper.

### BuildGlobalRows: N+1 tmux probe pattern

**Where:** `internal/state/listing.go:BuildGlobalRows`.

**What:** Per project: 1 HasSession call for main + 1 per workspace,
sequential. With many projects, this accumulates. Not a v0.10 regression but
exposed at scale by v0.10's auto-population of the load column.

**Fix:** batch via `tmux list-sessions -F '#{session_name}'` once, build
a set, look up locally. AttachedProbe already does this — extend the same
pattern to liveness probing.

### detectShipped: blank-line guard regression

**Where:** `internal/lifecycle/shipped.go:detectShipped`.

**What:** Old code explicitly skipped blank lines in cherry output; new
code dropped that guard. After `strings.TrimSpace(string(cherryOut))`, the
trimmed string can still contain a blank line in the middle (rare for
cherry, but possible if cherry's output gains commentary in some git
versions), which would fail the `strings.HasPrefix(line, "- ")` check and
short-circuit to nil.

**Fix:** restore the `if line == "" { continue }` skip in the cherry-output
loop.

### BareAttach: store BaseCommit on workspace creation

**Where:** `internal/state/state.go:Workspace`.

**What:** v0.10's `detectShipped` dropped merge-commit-style detection
because it was a false-positive vector (couldn't distinguish "branch was
merge-commit-merged" from "fresh fork that fell behind"). Storing the
branch's base commit at workspace creation would let us safely detect both
cases.

**Fix:** add `BaseCommit string` to `state.Workspace`. Set at creation in
`Manager.Create` via `git rev-parse HEAD`. Update `detectShipped` to use
`base..HEAD == 0` (no work) vs `base..HEAD > 0 + reachable from main`
(merged via merge-commit) vs cherry `-` lines (squash-merged).

---

---

## ✅ DONE 2026-04-29 — `canopy debug` (subsumed into the inspect drawer)

Shipped on the `tmux-health-and-resurrect` branch. The verb itself was
not added — instead, `Manager.BareAttach(ctx, name)` lives on the
workspace package and is invoked from the diagnostic detail drawer
(opened with `i` in the TUI) via `b`. From the user's perspective, this
is the same flow the original TODO described: drop into the workspace
dir without rerunning `scripts.setup`, with `CANOPY_*` env vars set, no
agent pane, no auto-runs — just a shell. Implementation in
`internal/workspace/lifecycle.go` (`BareAttach`) +
`internal/ui/drawer.go` (drawer + `b` keybind).

The drawer-based shape is better than a top-level verb because the
launch context (an `i` view of the broken workspace, with the setup
log right there) is exactly what the user has loaded when they decide
they want to drop in. A separate `canopy debug` verb would have
required typing the workspace name from the shell.

---

## v0.6 follow-up — Background liveness probe (deferred 2026-04-29)

**What:** Run a `tea.Tick` every N seconds (e.g., 10s) that re-probes
liveness for visible TUI rows and updates `Alive` in place. Today, the
list refreshes only on `r` keypress and TUI navigation events.

**Why:** When a workspace's tmux session dies out-of-band (laptop
sleep, manual `tmux kill-session`, OOM), the list shows the row as
`ready` until the user presses `r`. The Enter handler now handles this
gracefully (stale-ready → resurrect, see v0.9.x changelog), so the
case where it bites is purely cosmetic — the column lies for a few
seconds.

**Pros:** Makes the Mem column feel alive; turns the list into a
"current truth" view rather than a "snapshot from last keypress."

**Cons:** ~10x more `tmux has-session` calls (cheap but not free),
possible visual flicker mid-keystroke when the column re-renders, and
the bug-fix path already handles the stale-ready case. Worth doing
only if the staleness has actively bitten outside cosmetics.

**Context:** Decided in `/plan-ceo-review` as the only deferred item
when the diagnostic drawer + Mem column shipped. Reconsider after a
few weeks of dogfood with the explicit-refresh-only model.

**Depends on / blocked by:** none.

---

## v0.1.0 — Demo recording for the launch

**What:** A 30–45 second terminal recording showing the canopy happy path end-to-end: `canopy init` → `canopy new` (auto-attach into the 3-pane tmux session) → tiny edit → detach → `canopy ls` → `canopy switch <name>` (claude conversation resumes) → `canopy rm`. Output as `docs/demo.gif` referenced from the README hero and used as the launch-tweet asset.

**Why:** People decide whether a CLI tool is worth installing in 10 seconds. A short, clean recording does the job better than 200 words of pitch copy — especially load-bearing for a launch where strangers see the README cold.

**Pros:** Highest-leverage marketing artifact per minute of effort. Pairs with the v0.1.0 tag + Show HN / r/golang / Lobste.rs cross-post sequence. [vhs](https://github.com/charmbracelet/vhs) — Charm's recording tool, perfect aesthetic match for canopy's stack — makes the recording fully scripted and rerunnable as the UI evolves.

**Cons:** Real polish work; first take never looks right. Recording inside canopy's live tmux is fiddly because the no-nesting guard refuses to run inside tmux (use `CANOPY_ALLOW_NESTED=1` for the recording session). The .tape script needs maintenance whenever the TUI keymap/layout changes.

**Context:** Likely shape: `tape/canopy-demo.tape` (vhs convention) invoked via a `make demo` target. Aim for ~800×600, 30-second loop, pin font + dark theme to match the README aesthetic. Plan:

1. Set up a fresh scratch project (Rails or Node — pick whichever onboards in <5s).
2. Write `tape/canopy-demo.tape`. Comment each `Type` / `Sleep` / `Enter` block so future maintainers can iterate.
3. `vhs tape/canopy-demo.tape` renders `docs/demo.gif`.
4. Add `![demo](docs/demo.gif)` to README under the badge row.
5. Optional: still frame at `docs/demo.png` for places that don't autoplay GIFs.

Avi (2026-04-28): "I'll work on the demo later, just keep a todo on that." Deferred until post-merge of the parallel agent's work AND post-org-move, so the recording reflects final TUI + final URLs.

**Depends on / blocked by:** Org move complete; parallel agent's branch merged.

---

## v0.5 — Repo org move (`avinashjoshi/canopy` → org)

**What:** Move canopy out of Avi's personal GitHub into either **cravd** or **oncactus** org. Bulk-update `go install` URLs in README + docs, the module path in `go.mod`, every internal `import` line, and any `gh` URLs in CLAUDE.md.

**Why:** Canopy crossed the "this is real and useful" threshold with the v0.5 ancient-hornet ship. Personal repo was fine for early dogfood; org repo is the right home before any external users show up. Avi: "its time for sure!!"

**Pros:** Cleaner story for OSS release. Brand association with whichever org wins (likely oncactus per below). No more "wait, why is this a personal repo?" friction at install time.

**Cons:** Mass file edits across the repo + module path change requires every consumer to update. Not destructive but visible. Ancient-hornet had to land first because the diff was big enough that doing org-move and global-TUI in the same change would've been brutal to rebase.

**Context:**
- The structural analogy: cravd inc ≈ 37signals (legal entity, marketplace origin), cactus/oncactus ≈ basecamp (flagship that grows to eclipse the parent in mindshare).
- 37signals puts their dev tooling under `basecamp/*` (kamal, trix, etc.), not `37signals/*` — flagship-product brand has the public mindshare, not the corporate entity.
- **If canopy goes public OSS with brand association: `oncactus/canopy`** (mirrors `basecamp/kamal`).
- **If canopy stays internal-only: `cravd/canopy`** is fine.
- Current README signals (Mac/Windows install instructions, "License TBD before public release" line) tilt toward public release → recommendation is **oncactus**.
- Loose end: the `Co-Authored-By` Claude attribution lines in commit history reference avinashjoshi commits — those stay as historical record, no rewrite needed.

**Depends on / blocked by:** v0.5 ancient-hornet — landed 2026-04-28. Unblocked.

---

## v0.5 — multi-project support — PARTIAL (2026-04-28)

**Shipped on `ancient-hornet`:**
- Read-only global TUI: `canopy` from outside any project lists every workspace + every alive `<project>-main` session canopy knows about. Enter on `ready`/`main` rows attaches via tmux.
- Init splash: `canopy` in a fresh git repo (no canopy.json) shows an init prompt; pressing `i` runs `canopy init` synchronously after the splash exits.
- Project ID = canonical absolute root path. State.Projects map keyed by it. Workspace.ProjectRoot field is the v2 authoritative key. Lazy v1→v2 migration runs once at workspace.New time.
- Basename uniqueness invariant: `canopy init` and `workspace.New` refuse to register a project whose basename collides with another already-registered project. State on disk is left untouched on refusal.
- Reusable `internal/ui/projectlist/` Bubbletea sub-component: GlobalModel wraps it; the future v1 in-session overlay (below) embeds the same component instead of re-rendering the table.
- `state.BuildGlobalRows` is the single source of truth for the project-grouped row list; both `canopy ls --all` (tabwriter) and the TUI consume it.

**Still deferred to v0.6 (see entries below):** create/remove from global mode, project picker modal, `-p <project>` flag.

---

## v0.6 — global mode lifecycle (create / remove from anywhere)

**What:** Make `canopy` in global mode able to create + remove workspaces without cd'ing into the project. New project picker modal: 'n' in global mode → list of registered projects → name input → create. 'd' on a global-mode row → confirm → remove (without needing the project's canopy.json on hand).

**Why:** v0.5 ships read-only global mode. The friction is "see workspaces from anywhere, but to create one you still cd in." Once we've felt that friction in real use, we'll know whether the project picker is worth the UX complexity (two modals deep) or whether attaching is good enough.

**Pros:** Closes the "open canopy anywhere, do everything from there" loop. Project basenames are already unique in v0.5 (uniqueness invariant), so the picker can use basenames as display labels with no disambig logic.

**Cons:** Two modals deep (pick project, then name). The TUI's viewMode enum grows. Resurrecting a stopped workspace from global mode requires loading the source canopy.json — possible via `git -C <worktree-path> rev-parse --git-common-dir` to find the source repo without needing a registry, but adds a code path that can fail (deleted source repo, etc.).

**Context:** v0.5 wired the data layer correctly (state.Projects keyed by canonical root, basename uniqueness enforced). The picker would just iterate `state.Projects` and let the user pick a root. From there `canopy.json` lookup is `<root>/canopy.json` and the existing workspace.New flow runs unchanged. Resurrect-from-global is the only new code path that needs invented; everything else is UI plumbing.

**Depends on / blocked by:** v0.5 ancient-hornet branch landed.

---

## v0.6 — `canopy reconcile --remove-project <root>`

**What:** A CLI surface for removing a project entry from `state.Projects` (and any of its workspaces) without hand-editing state.json. Triggers the basename-collision-refusal escape hatch when the colliding project is gone from disk.

**Why:** v0.5 refuses to init a basename-colliding project, with the workaround being "hand-edit state.json." That's fine for power users but not for the "I deleted ~/Work/cravd and now ~/Code/cravd init refuses" case. We need a clean way to evict a stale project entry.

**Pros:** Closes the only remaining hand-edit-state.json case in v0.5. ~30 LOC.

**Cons:** Subcommand surface area. Needs careful UX (don't accidentally evict a real project's data because basenames matched).

**Context:** Implementation: extend `cmd/canopy/reconcile.go` with a `--remove-project <canonical-root>` flag. Under state.WithLock: assert `s.Projects[root]` exists, refuse if any Workspaces still reference that root (force flag to override), then `delete(s.Projects, root)`. Print a confirmation summary.

**Depends on / blocked by:** v0.5.

---

## v0.6 — drop legacy `Workspace.Project` field

**What:** v0.5 keeps Workspace.Project (basename) as a legacy-read-only field for back-compat with v1 state files. Once everyone has migrated (one release of v0.5+ in the wild), drop the field entirely.

**Why:** Single source of truth. As long as Project (basename) is alongside ProjectRoot (canonical path), there's a temptation to use the wrong one and we keep paying the back-compat cost on every state.json read.

**Pros:** Simpler schema. ~20 LOC of cleanup across state.go, lifecycle.go, listing.go.

**Cons:** Anyone running v0.5+ that hasn't yet run a project-scoped command (which would trigger migration) would have un-migrated v1 rows still in their state.json when they upgrade to v0.6. Mitigation: add a one-shot `state.MigrateLegacyAll()` in v0.6 that walks every entry and migrates them by basename → root lookup; for orphans (basename in state, no project found at any known root), error and require manual reconcile.

**Context:** Mechanism: stop emitting `omitempty` legacy field in v0.5+, then drop the field entirely in v0.6. Bump SchemaVersion to 3.

**Depends on / blocked by:** v0.5 has shipped + been in use for at least one cycle.

---

## v0.6 — global mode E2E tests

**What:** Three Go tests under `-tags=e2e`: canopy-from-home (global TUI launches with rows from prior project-scoped activity), canopy-from-fresh-repo (init splash launches and `i` produces a canopy.json), canopy-from-project (today's project TUI, regression).

**Why:** The three top-level routing flows currently have unit tests for the routing decision and TUI-layer smoke tests, but no end-to-end test that wires `canopy` through to the actual screen. Manual smoke checklist works for solo dev; E2E is the safety net once multi-agent CI starts running.

**Pros:** Real regression coverage for the entire user-facing flow.

**Cons:** ~2-3h of test plumbing — vt100/expect-style harness for Bubbletea programs, plus tmux socket isolation for the project flow. Tests would be slow (2-5s each) so probably gated behind `-tags=e2e` like the existing workspace tests.

**Context:** Bubbletea has [teatest](https://github.com/charmbracelet/x/tree/main/exp/teatest) for golden-file Model testing. Use that for the splash + global flows. Project flow already has E2E coverage via the existing `internal/workspace/lifecycle_test.go`.

**Depends on / blocked by:** v0.5 ancient-hornet shipped.

---

## v0.5 — `canopy doctor` subcommand

**What:** Validates project config, checks git/tmux versions, lists tmux sessions matching project prefix that lack a state.json row (orphan-tmux discovery), lists workspace dirs on disk that lack a row (orphan-disk discovery), offers to clean up.

**Why:** v0 doesn't have a way to surface inconsistencies. State.json + tmux + disk can drift if the user hand-edits anything. Without `doctor`, the only recourse is reading the JSON manually.

**Pros:** One subcommand makes canopy self-healing. Becomes the documented "first thing to run when something seems off."

**Cons:** Real new surface area. Discovery logic for orphan-tmux requires parsing `tmux ls -F` and matching a prefix pattern. Worth doing once `canopy.json` schema and state schema are stable.

**Context:** Acceptance criteria: `canopy doctor` exits 0 if everything healthy, 1 if anything wrong. Categories: (a) tmux sessions matching `<project>-*` not in state, (b) dirs in `<project>/worktrees/` not in state, (c) state rows where dir is missing AND tmux is missing (already surfaced as `orphaned`), (d) `canopy.json` schema validation errors, (e) git/tmux version below minimum.

**Depends on / blocked by:** v0.

---

## v0.5 — Hook timeout

**What:** Per-script timeout (configurable in `canopy.json` or hardcoded 5min default) so a hung `scripts.setup` doesn't freeze the TUI.

**Why:** Failure mode flagged in /plan-eng-review. Hooks have no timeout in v0. A network-stuck `bundle install` will look like a frozen TUI until the user manually SIGINT.

**Pros:** ~30 LOC (`exec.CommandContext` with deadline already in v0; the new code is the YAML-side schema + per-script default). Fixes a real frustrating UX moment.

**Cons:** Adds a config schema decision (per-script field? top-level default? both?). Most hooks in practice are fast or already have their own timeout machinery (`bundle install` retries, etc.). May never bite.

**Context:** v0 uses `exec.CommandContext(ctx, script)` already, but `ctx` is `context.Background()` for hooks. Fix is `ctx, cancel := context.WithTimeout(parent, timeout)` with timeout sourced from a `scripts.timeouts.setup` field in canopy.json (default 300s), or a top-level `timeout: 300` (simpler). On timeout: kill process group, log timeout, set status to `broken` with a clear `last_error: "script setup timed out after 300s"`. SIGINT escape hatch already exists in v0 so users have a manual recourse meanwhile.

**Depends on / blocked by:** v0 hook runner exists.

---

## v0.5 — PTY handoff for interactive hooks

**What:** Real PTY allocation when a script needs interactive stdin (e.g., `gem install` prompts, `git pull` with credentials, sudo).

**Why:** v0 declares hooks non-interactive. If a hook ever needs to prompt for input, today's plan freezes (process waits for stdin that's not connected). Real fix is allocating a PTY and proxying it through the TUI — non-trivial.

**Pros:** Removes a class of hook failures. Aligns with how mature workspace managers handle this.

**Cons:** Real engineering work. PTY libs (`creack/pty`, etc.) introduce a non-stdlib dep. UX for "show a prompt inside the running-setup view" is its own design problem.

**Context:** v0 best practice is "make hooks non-interactive" — pre-store secrets, use `--yes` flags, configure git creds via SSH keys. Document this in the README's "writing canopy.json" section.

**Depends on / blocked by:** v0. May want to skip this entirely and stay in the "make hooks non-interactive" world forever.

---

## v0.2 — darwin / macOS releases

**What:** Add `darwin/amd64` + `darwin/arm64` to `goreleaser` config. Code-signing if needed for Gatekeeper compliance.

**Why:** v0.1.0 ships linux-only. macOS is plausible secondary audience (lots of devs on Macs); cross-compile is free; sign+notarize is not.

**Pros:** Doubles potential audience. `go build` already cross-compiles cleanly. There's existing signal that Mac users want this category of tool.

**Cons:** Notarization requires an Apple Developer account (currently $99/year), code-signing certs, and a `goreleaser` Notary config. Several hours to set up.

**Context:** Skip until v0.1.0 has at least one Linux user other than Avi who reports it works. Then revisit. If not signing, users can run `xattr -d com.apple.quarantine` themselves — fine for early-adopter audience.

**Depends on / blocked by:** v0.1.0 ships and at least one external user reports it works on Linux.

---

## v0.6 — Agent lifecycle wrapper + detectors — PARTIAL (2026-04-28)

**Shipped on `agent-lifecycle` branch:**
- Agent launcher map (`internal/agent/launchers.go`) for claude/codex/opencode/aider, picked via `agent.type` in canopy.json. Backwards compat: empty agent block defaults to claude.
- Briefing assembly (`internal/agent/briefing.go`) with the hybrid fresh-vs-resume strategy. Full briefing on `AgentLaunchCount==0`; hints-only delta on resume; empty (no `--append-system-prompt` flag) on resume + no active hints. Briefing rebuilt fresh on every agent launch via in-memory assembly + temp file at `~/.canopy/tmp/`. SourceKind variants (fresh/pr/issue/branch) with prompt-injection delimiter framing.
- Detector framework (`internal/lifecycle/`) with three detectors: rename_suggested (cheap, runs every TUI tick), shipped (cheap, every tick), pr_status (10min cache, runs only on canopy reconcile + manual `r`). All run in parallel via tea.Cmd goroutines.
- Hint badges in the global TUI: `↻ rename` (amber), `✓ shipped` (green bold), `PR` / `✓ PR` (cyan). Pressing enter on a row with `shipped` hint routes through `OnCloseOut` instead of attaching.
- `canopy rm` smart safety pre-flight: refuses on uncommitted / unpushed / open-PR unless `--force`. Orphan workspaces (worktree dir gone) get a graceful pass-through, NOT a block.
- State schema additions: `Workspace.AgentLaunchCount` (incremented on Create + Resurrect), `Workspace.SourceKind` (immutable). Both omitempty.
- Config schema additions: `agent.{type, briefing, briefing_file}`. `scripts.agent` retained as power-user override.
- O1 (claude --continue + --append-system-prompt) verified empirically.

**Refined after first dogfood (2026-04-28):**
- Removed the OnCloseOut routing entirely. Enter on a shipped/PR-merged row attaches normally; deletion stays a manual `canopy rm` step. The `auto_close_shipped` config flag was reverted — destructive auto-rm was the wrong shape.
- `pr_status` moved into the cheap-tick set (RunFast). With the 10min cache, the API budget concern was overstated; running it on every refresh means PR state shows immediately rather than only on `r`.
- Badge precedence: PR state wins when present. The local "shipped" detector now renders as `✓ shipped (local)` and is hidden when a `pr_status` hint is also active for the same workspace. PR badges decode the message into open/approved/changes/merged/closed colored variants.
- `detectShipped` now falls back to local `<default>` when there's no remote, so purely-local repos surface a "shipped" signal without needing a GitHub remote.

**Shipped in agent-lifecycle follow-ups:**
- `canopy new --pr <num>` / `--issue <num>` / `--branch <name>` / `--allow-local` flags. PR flow handles same-repo PRs (checkout origin/<head>) and cross-repo / fork PRs (fetch via `refs/pull/<n>/head:canopy/pr-<n>`). Issue flow seeds the briefing with the issue body. --branch checks out an existing branch, requiring origin/<name> unless --allow-local. SourceContext (PR/issue body) flows through state.Workspace into the briefing wrapped in a `<<<CANOPY_SOURCE_DATA>>>` data fence.
- `auto_close_shipped` flag in `~/.canopy/config.json` for the auto-close-on-merge UX with 5s cancel window. v0.6 currently surfaces a hint (`canopy rm <name>`) instead of auto-running.

201 tests across 14 packages. Smoke verified: build clean, all tests green including `-tags=e2e`.

---

## v0.6 — Agent lifecycle wrapper + detectors (original entry, kept for context)

**What:** Wrap every agent session with canopy-assembled workspace context + active lifecycle detector hints so any coding agent (Claude, Codex, OpenCode, aider) boots knowing where it is in the feature lifecycle. Seven accepted scope items:

1. `agent.{type, briefing, briefing_file}` block in `canopy.json` — agent-agnostic config (default `type: claude`). `scripts.agent` stays as a power-user override.
2. Built-in launcher map in `internal/agent/launchers.go` keyed by `agent.type` (claude/codex/opencode/aider). Adding a new agent = one PR adding a map entry.
3. Briefing assembled in-memory and passed inline (no persistent `.canopy/AGENT.md` file). Hybrid strategy: full briefing on fresh launch (AgentLaunchCount==0), hints-only delta on resume; skipped entirely if no hints active on resume.
4. Detector framework at `internal/lifecycle/`: rename_suggested, shipped, pr_status. Run as parallel tea.Cmd goroutines. pr_status caches 10min in-memory; runs only on canopy reconcile + manual `r`. Hints recomputed on demand, NOT persisted in state.json.
5. `canopy new` extended with `--pr <num>` / `--issue <num>` / `--branch <name>` / `--allow-local` flags. Each gets a dedicated AGENT.md briefing variant. PR/issue body wrapped with delimiter framing as basic prompt-injection mitigation.
6. `canopy rm` smart pre-flight safety: refuses on uncommitted/unpushed/open PR; `--force` bypass; orphan workspaces (worktree dir gone) get a warning + proceed.
7. `auto_close_shipped` flag in `~/.canopy/config.json` (default false). When true: shipped detector firing + safety check passing → 5s cancel window in TUI / immediate run in headless reconcile.

State schema: `Workspace.AgentLaunchCount int` + `Workspace.SourceKind string` (one-time set: fresh/pr/issue/branch).

**Why:** Two user-stated outcomes: branch reflects feature intent within minutes of scoping; visible "are we done?" close-out moment at ship time. Today both are forgettable — workspaces accumulate as zombies, branch names stay random. Detectors surface state in the TUI; the briefing teaches the agent how to drive the lifecycle. Agent-agnostic so canopy doesn't lock to Claude as the agent ecosystem evolves.

**Pros:** Conductor's "card with status + archive" pattern, made agent-driven and terminal-native. Single agent-agnostic injection point. No persistent file in worktree (rebuilt fresh per launch). Detector framework extensible — adding stuck/conflict detectors later is one file each. Backwards compatible (empty `agent` block defaults to claude).

**Cons:** Surface area: 12 files touched across new packages (internal/agent, internal/lifecycle) + extensions to 6 existing files. ~700-900 LOC + tests. `agent.type` value coverage matters — adding a new agent requires a launcher map entry that must stay in sync with the agent CLI's evolving flags. Hint badge UX in TUI is real design work (recommend /plan-design-review before shipping).

**Context:** Full design at `docs/design/v0.6-agent-lifecycle.md` (rewritten 2026-04-28 to match CEO + Eng review decisions). CEO plan with full decision history at `~/.gstack/projects/canopy/ceo-plans/2026-04-28-agent-lifecycle-wrapper.md`. O1 (claude --continue + --append-system-prompt re-injection) verified empirically — works as designed. Implementation order locked; ready to cut a fresh branch off main.

**Depends on / blocked by:** v0.5 ancient-hornet (landed). v0.5 Multi-AI-tool entry below is superseded by this design. `canopy rename` verb still independently useful but not v0.6 critical path.

---

## v0.5 — `canopy rename <new-branch>` verb

**What:** A small subcommand to rename a workspace's git branch atomically. `canopy rename feat/oauth` renames the worktree's current branch (e.g. from the auto-generated `bold-falcon` to `feat/oauth`) and updates the state row's `Branch` field in the same lock window.

**Why:** Workspaces start with random adjective-noun names (the namegen pattern). Once the user knows what the feature is, the branch should reflect that — `bold-falcon` → `open-canopy-anywhere`. Today this requires `git branch -m old new` + manual hand-edit of state.json or a brittle re-discovery dance via `canopy reconcile`. One verb closes the loop.

Pairs with the v0.6 agent lifecycle wrapper (above): the AGENT.md briefing tells Claude/Codex to call `canopy rename` once the feature is scoped, so the rename happens automatically as the conversation crystallizes.

**Pros:** ~50 LOC + tests. The data model (Workspace.Name = dir = tmux session, Workspace.Branch = renameable) already supports this — Name and Branch are separate fields and Name is what tmux/dir use. Atomic via state.WithLock.

**Cons:** Tmux session name and worktree dir name stay frozen at the workspace's generated name (renaming those mid-flight breaks tmux attach + invalidates `CANOPY_WORKSPACE_PATH` for any cached env). A user who expected "rename = rename everything" might be surprised. Doc the boundary clearly: rename is git-branch-only.

**Context:** Implementation: `cmd/canopy/rename.go`. Calls `git -C cfg.ProjectRoot branch -m current new` (current discovered via `git -C ws.Path rev-parse --abbrev-ref HEAD` so we tolerate prior manual renames). Updates `state.Workspaces[i].Branch`. Validates new name with `git check-ref-format --branch`. Idempotent: rename to current name = no-op + friendly message. Pairs with the existing v0.5 entry "Branch-rename tolerance in `canopy rm`" — both should land together so rename + rm form a coherent loop.

**Depends on / blocked by:** none. Can ship before the agent lifecycle wrapper.

---

## v0.5 — Multi-AI-tool support (via layout-as-config)

**What:** Make the AI pane configurable in `canopy.json` so users can choose claude / codex / opencode / aider / etc. per project. Both the launch command AND the resume command go in config.

**Why:** v0 hardcodes `claude` and `claude --continue`. Other AI tools have different invocation + resume patterns. As more devs adopt different AI CLIs (codex, opencode, aider), canopy stays tool-agnostic — it just runs whatever command the project's `canopy.json` specifies.

**Pros:** Aligns canopy with the layout-as-config v0.5 milestone (one feature, two wins). Future-proofs for AI-tool churn (the AI CLI landscape is moving fast). Lets one repo use claude while another uses codex without canopy caring.

**Cons:** Surfaces a quality variance: AI tools that lack per-directory storage or a non-interactive resume flag will work in canopy but lose the resurrection magic. Documenting the "what works fully vs partially" matrix is a small README chore.

**See also:** `docs/design/v0.6-agent-lifecycle.md` — the Agent lifecycle wrapper (above) supersedes this entry's surface area with a more flexible shape (`scripts.agent` script + canopy-assembled briefing file). Implement the lifecycle wrapper instead of this entry; this stays as the historical record of the simpler "just make the launch command configurable" version.

**Context:** v0 design includes a `mode` parameter in pane-creation (`fresh` vs `resume`). For v0.5, replace the hardcoded `claude` / `claude --continue` strings with config-loaded `panes[i].cmd` / `panes[i].resume_cmd`. The architecture nudge in v0 is to put those two strings in a tiny `internal/ai/defaults.go` constants file, not inline in `internal/workspace/` or `internal/ui/` — makes the v0.5 swap trivial. Compatibility matrix to seed in README:

  - Claude Code: full magic. `claude` / `claude --continue`.
  - aider: full magic. `aider` / `aider --restore-chat-history`.
  - Codex CLI: degraded — `codex resume <id>` needs a thread ID, doesn't auto-pick the latest in cwd. Might need a wrapper.
  - opencode: TBD, needs verification before claiming compatibility.

**Depends on / blocked by:** v0.5 layout-as-config (they're the same milestone; ship together).

---

## v0.5 — `canopy run` subcommand for on-demand scripts.run

**What:** `canopy run` invokes the project's `scripts.run` with the
right `CANOPY_*` env vars in the user's current workspace. Replaces
the old "always-running server pane" model with a deliberate "start
the server when I need it" UX.

**Why:** v0 dropped the auto-running server pane (the design-doc 4-pane
layout). The new 3-pane layout is nvim + claude + shell — the user
runs `bin/dev` in the shell when they want it. `canopy run` is the
ergonomic shortcut: from inside any pane, type `canopy run` and the
right command fires with the right env, regardless of which workspace
you're in.

**Pros:** One canopical way to start the dev server. Removes the
"which directory am I in / what env do I need" friction. Avi's
mental model from Conductor was `cmd+r` to fire the run hook;
`canopy run` is that idea ported to the terminal.

**Cons:** Needs a way to know which workspace you're in. Easiest:
match `pwd` against state.json paths. If no match, error with
suggestion ("you're in the source repo; cd into a workspace or run
`canopy main`"). Implementation is ~30 LOC.

**Context:** `scripts.run` already exists in canopy.json schema and
is currently unused at create time. `canopy run` activates the field.
Could also pair with a tmux key-binding helper (see "tmux navigation
help" below) so `canopy main`'s sessions can offer a hotkey overlay.

**Depends on / blocked by:** v0.

---

## v0.5 — Tmux navigation help / overlay

**What:** A small in-session help cheatsheet that explains canopy's
pane layout + the most useful tmux navigation keys to a user who
isn't fluent in tmux yet. Could be:
  - A `?` keybinding in canopy-managed sessions that pops up a
    visible overlay (popup window, or `tmux display-message`)
  - A `canopy help-tmux` subcommand that prints a cheat sheet
  - A static "your panes" header at the top of each window with
    pane labels (file, claude, shell)

**Why:** Avi noted the experimental layout is fine for now but mentioned
"some sort of help for navigating tmux" as future polish. A tool that
opinionates about tmux layouts owes its users a way to learn the
keys without reading the tmux man page.

**Pros:** Lowers the floor for first-time tmux users. Makes canopy's
opinionated layout self-explaining. Conductor's GUI surfaces hotkeys
visually; this is the terminal equivalent.

**Cons:** Tmux popup support varies across versions (>=3.2 required
for popup-window). A static cheatsheet is easier but less discoverable.

**Context:** Useful keys to teach:
  - prefix-d: detach (return to canopy CLI)
  - prefix-arrow: switch panes
  - prefix-z: zoom active pane (toggle fullscreen)
  - prefix-c: new window
  - prefix-1/2/3: switch to window N
  - prefix-,: rename window
  - prefix-[: scroll mode (q to exit)

**Depends on / blocked by:** none. Easy add post-v0.

---

## v1 — Pluggable session backend (tmux → zellij/kitty/etc.)

**What:** Abstract the tmux dependency behind a `SessionBackend` interface so canopy can swap to other multiplexers (zellij, kitty's session protocol, Ghostty's eventual native persistence, etc.) without changing core logic.

**Why:** v0 hardcodes tmux. That's the right call now (tmux is universal, mature, solves the persistence problem better than anything else). But the multiplexer landscape is moving — zellij has momentum, Ghostty may ship session persistence, kitty has its own thing. Locking forever to tmux means canopy ages with tmux. Locking to an interface means canopy can move when the world does.

**Pros:** Future-proofs against multiplexer shifts. Lets users on niche setups (zellij-only, no tmux) use canopy. Forces a clean architectural boundary that already exists informally in `internal/tmux/`.

**Cons:** YAGNI risk — the abstraction may never get a second implementation. Designing an interface for one backend often gets the interface wrong (you need 2-3 real implementations to know what abstracts well). Wait until at least one user asks for non-tmux before doing this.

**Context:** Today every multiplexer call goes through `internal/tmux/`. Step 1 is renaming that package to `internal/session/` and giving it a `Backend` interface (`Create`, `Attach`, `Kill`, `HasSession`, `Resurrect`). Step 2 is implementing the interface for tmux (move existing code). Step 3 (later) is implementing for whatever second backend gets requested. Acceptance criteria for v1: at least two backends implemented and passing the same test suite.

**Depends on / blocked by:** v0 ships, at least one user asks for a non-tmux backend OR Ghostty/zellij/kitty meaningfully changes the persistence story.

---

## v1 — Multi-AI within one workspace (parallel-pane provider switching)

**What:** Hotkey to add a new pane running a different AI tool, side-by-side with the existing one. Compare outputs, hand off context, A/B different models on the same problem.

**Why:** "I started with claude, want to try codex on this" is a real workflow as the AI-CLI ecosystem matures. Different tools have different strengths; comparing them in-context is valuable.

**Pros:** A genuinely novel feature in TUI-land. Supports the "AI as collaborator, not replacement" pattern. Doesn't require shared context across AIs (that's a much harder problem) — just side-by-side.

**Cons:** UX surface area: how do you select which tool? Config UI for adding ad-hoc tools? Persistence of which extra panes were opened? Real product work.

**Context:** Likely shape: `+` key opens a tool-picker (list of tools from `canopy.json` or a global registry). Selected tool launches in a new pane in the same tmux window or window-group. Not a v0/v0.5 feature — wait until v0.5 layout-as-config has shipped and there's signal that users actually want this.

**Depends on / blocked by:** v0.5 multi-AI-tool support (above).

---

## v0.5 — Branch-rename tolerance in `canopy rm`

**What:** When the user has renamed a branch after canopy created it
(`git branch -m bold-falcon feat/oauth`), `canopy rm bold-falcon`
should still work cleanly — including deleting the renamed branch
rather than warning about the missing original.

**Why:** Branch renames are a normal part of the workspace lifecycle
(canopy creates a random name, the user develops on it, eventually
renames the branch to something descriptive before pushing). Today
canopy rm calls `git branch -D <original-name>`, fails silently
with a warning, and leaves the renamed branch on disk. Minor wart
but accumulates over time.

**Pros:** Cleaner removal flow. Less manual cleanup. Users feel free
to rename branches knowing canopy keeps up.

**Cons:** Need to discover the workspace's CURRENT branch (via `git
worktree list --porcelain`) before deleting. ~15 LOC + a test.

**Context:** v0 stores `branch` in state.json at creation time and
treats it as immutable. Fix: in `workspace.Remove`, before calling
`git.DeleteBranch`, look up the current branch by querying
`git worktree list --porcelain` for the workspace path and reading
the `branch refs/heads/<name>` line. Pass that to DeleteBranch
instead of the stored `wsCopy.Branch`. Update state.json's branch
field too on Reconcile so canopy ls shows the current branch name,
not the stale original.

**Depends on / blocked by:** none — can land any time post-v0.

---

## v0.5 — Worktree adopt

**What:** `canopy adopt <branch>` — register an existing git worktree (created via `git worktree add` outside canopy) into state.json without re-running `scripts.setup`.

**Why:** Migration path for users who already have manually-created worktrees. Especially useful for Avi during the cravd dogfood: existing worktrees can be "adopted" instead of recreated.

**Pros:** Smooth onboarding. Doesn't force users to nuke existing work to start using canopy.

**Cons:** New subcommand surface. Needs to scan for git worktrees on disk and present them.

**Context:** Likely command shape: `canopy adopt <branch>` with a `-p <port>` to specify which port the existing setup is using. Marks status as `ready` directly. Skips `scripts.setup`. Useful corollary: a `canopy adopt --all` that scans the project's worktree dir and adopts every git worktree found.

**Depends on / blocked by:** v0.

---

## ✅ PARTIAL 2026-04-29 — Per-workspace logs (workspace + setup)

Shipped on the `tmux-health-and-resurrect` branch. Two of the three
file types from the original entry now exist:

- `~/.canopy/log/canopy-<workspace>.log` — every slog record carrying
  a `name` attribute matching a workspace is tee'd here in addition to
  the global `canopy.log`. Implemented as a `clog.fanoutHandler` that
  wraps the existing JSON handler and adds per-workspace fan-out
  transparently — no call-site changes anywhere in canopy.
- `~/.canopy/log/setup-<workspace>.log` — `scripts.setup` output is
  captured to disk via `io.MultiWriter` in
  `Manager.runSetupHooksOnly`. Truncated each setup run (the previous
  attempt's output is rarely useful once a new run starts).

`canopy rm <ws>` removes both files. The diagnostic drawer (`i` in the
TUI) reads them directly to show recent activity + last setup output.

**What's still NOT done from the original entry:** the
`~/.canopy/log/<project>/canopy.log` per-project layer (events
interleaved across workspaces in the same project still go to the
global log), and `archive.log` capture for `scripts.archive`. Both are
straightforward extensions of the fan-out handler if/when they're
needed — the per-project layer would key on a `project` attribute the
way the per-workspace layer keys on `name`.

---

## v0.5 — Auto-detect fixable setup failures

**What:** When `scripts.setup` fails, scan its stderr for known signatures (`master.key not found`, `bundle: command not found`, network errors) and surface a one-line "what to fix" hint in the TUI before the user has to dig through `~/.canopy/log/canopy.log`.

**Why:** The `canopy retry` flow added 2026-04-27 covers "I fixed it, run it again." The remaining gap is "I don't know what to fix." Avi's first cravd attempt failed on a missing Active Record encryption credential — diagnosable in one line of stderr, but the user still had to read the log file.

**Pros:** ~50 LOC + a small registry of regex → hint pairs. Makes the broken state actionable instead of just informational. Pairs naturally with the existing retry verb.

**Cons:** Heuristic. Wrong hint is worse than no hint. Needs a curated registry that grows as we see real failures across users.

**Context:** Likely lives in `internal/workspace/lifecycle.go` next to `markBroken`. Signature: `func diagnoseSetup(stderr []byte) (hint string, ok bool)`. Registry as a `[]struct{ pattern *regexp.Regexp; hint string }`. The hint shows in the TUI's broken-row error banner and in `canopy ls --verbose`. Bonus: dump the matching log lines with the hint so the user can confirm without `tail -f`.

**Depends on / blocked by:** v0 retry verb exists.

---

## v1 — Edit-and-retry directly from the TUI

**What:** When a workspace is in `broken` status, offer an "edit + retry" affordance from the TUI: open `$EDITOR` on the failing script (or its log), let the user fix, then re-run with one keystroke.

**Why:** The `R`-to-retry flow in v0 assumes the user can fix the issue out-of-band (in another terminal). The whole point of the TUI is to keep the user inside one surface. Round-tripping through "open another tmux pane, fix the file, come back, press R" is the obvious next refinement.

**Pros:** Closes the recovery loop entirely inside canopy. Pairs with auto-detect above — the hint says what's wrong, the editor opens the right file, R re-runs.

**Cons:** Adds editor-handoff complexity (tea.ExecProcess on `$EDITOR`, then refresh). Picking *which* file to open is non-trivial — sometimes it's `scripts.setup`, sometimes a config file the script reads, sometimes a missing secret.

**Context:** Likely shape: a new modal that lists candidate files (the script itself, recently-modified files in the project, `~/.canopy/log/canopy.log`) and on selection opens `$EDITOR` via `tea.ExecProcess`. After the editor exits, the modal asks "Retry now? (y/N)". Could be gated behind the auto-detect hint registry: only files mentioned in the hint show up as candidates.

**Depends on / blocked by:** v0 retry verb. Best paired with auto-detect (above) so the candidate list isn't a dump of every file in the repo.

---

## v0.5 — Canopy onboarding

**What:** A guided first-run experience for users who've just installed canopy. Replaces the bare `canopy init` with an interactive walkthrough that explains the canopy.json schema, scaffolds `scripts.setup`/`scripts.run`/`scripts.archive` with project-aware defaults (Rails? Node? Go?), checks tmux/git versions, runs a smoke-test workspace creation, and points the user at `canopy ls` + the TUI.

**Why:** Today the path from `brew install canopy` to "first attached workspace" is `canopy init` → read `docs/canopy-json.md` → write three scripts by hand → `canopy new`. Every step is documented but none are guided. For a tool whose value is a high-friction-removed daily loop, the onboarding loop should be near-zero friction itself.

**Pros:** Removes the "I have to read three docs before I get value" cliff. Project-type detection (Gemfile present → Rails template, package.json → Node template) makes the scaffolded scripts immediately useful instead of TODO-stub. Becomes the demo path — "watch me onboard canopy in 90 seconds" is a real marketing artifact.

**Cons:** Real new surface area (a dedicated TUI flow, project-type detection, a registry of language-specific script templates). Risk of over-engineering — many users will outgrow the templates fast. Templates need to stay current as ecosystems shift (Rails 7 → 8, npm → pnpm/bun).

**Context:** Likely shape: `canopy init` becomes the entry point for the wizard when run interactively (TTY detected), keeps current behavior (just write canopy.json) when piped or `--non-interactive`. Wizard steps: (a) detect project type from cwd, (b) confirm/override, (c) scaffold scripts under `bin/canopy-*` with shebang + `set -euo pipefail` + project-aware defaults, (d) optionally run a smoke-test `canopy new --name onboarding-test` and tear it down, (e) print "you're ready: try `canopy new` or just `canopy`". Lives in `cmd/canopy/init.go` + a new `internal/onboarding/` package for templates. Pairs naturally with the future global splash screen (which would route first-launch users into this flow).

**Depends on / blocked by:** v0. No blockers.

---

## v1 — Auto-cleanup workspaces after PR merges

**What:** Detect when a workspace's branch has been merged + deleted upstream, and offer (or auto-execute) `canopy rm <workspace>` so the user doesn't have to remember the cleanup step. Pairs with the v0.6 agent lifecycle wrapper (which makes the rm step explicit) by closing the loop without an agent in the room.

**Why:** v0.6's agent-driven lifecycle requires Claude/Codex to remember to call `canopy rm` after `/ship` lands. That works while the agent is engaged. But shipped workspaces with no further agent attention will accumulate as zombies. An out-of-band watcher closes that gap.

**Pros:** Closes the "did the workspace get cleaned up?" question without manual bookkeeping. Pairs naturally with `canopy reconcile` — orphan detection there could surface "branch is gone upstream, want to rm?" prompts.

**Cons:** Detecting "merged" is fuzzy. Branch deleted on origin doesn't strictly mean merged (could be force-deleted, abandoned, renamed, ...). Need a real signal: `git for-each-ref` + `git log <branch>..origin/main` to confirm every commit is reachable from main. Even then, false positives possible. Auto-rm without confirmation is too aggressive — surface as a prompt in `canopy reconcile` and the TUI's broken-row remediation flow.

**Context:** Likely shape: extend `canopy reconcile` with a "stale-branch" detector. For each workspace, fetch origin (best-effort), check if the branch's HEAD is reachable from origin/<default-branch>, and if so AND origin no longer carries the branch, mark the row with a new `reachable_merged` hint. The TUI's broken/orphaned remediation flow shows the hint and offers `d` (delete) with a friendlier confirmation copy ("This branch appears merged + deleted upstream. Remove the workspace?"). Auto-execution gated behind `--auto-cleanup` flag on reconcile, never default.

Could also pair with the in-session overlay (below) — the overlay's status segment shows a small "✓ shipped, ready to remove" badge when the watcher fires.

**Depends on / blocked by:** v0.6 `canopy rename` (so the branch name in state.json matches the upstream branch) + v0.5 reconcile entry below. Auto-cleanup behind a flag is the safe v1 shape; default-on is a v2 question once we have telemetry on false-positive rate.

---

## v0.7 — `scripts.shipped` lifecycle hook

**What:** A new optional script in `canopy.json`'s `scripts` block that fires when the v0.6 `shipped` detector triggers for a workspace. User can wire post-merge automation (Slack message, Linear status update, deploy ping, etc.).

**Why:** Power-user wiring. Canopy already has setup/run/archive hooks; shipped is the natural fourth lifecycle moment. Cheap to add once the shipped detector exists. Pairs especially well with `auto_close_shipped: true` in config — the shipped script fires before the auto-rm.

**Pros:** ~30 LOC. Re-uses the existing scripts runner machinery (internal/hooks/runner.go). Same env vars as setup/archive. No schema migration.

**Cons:** Without it, users wire post-merge automation in their CI/PR workflows instead — which is arguably the right place for it. A scripts.shipped hook in canopy duplicates work most teams already do elsewhere.

**Context:** Implementation: extend canopy.json `Scripts` struct with `Shipped string` field. Run from `internal/lifecycle/shipped.go` after the detector confirms shipped state, before the auto-close-on-shipped flag triggers `canopy rm`. Env: standard CANOPY_* + a new CANOPY_SHIPPED_AT timestamp.

**Depends on / blocked by:** v0.6 shipped detector lands.

---

## v0.7 — Stuck workspace detector

**What:** Detector that flags workspaces with no commits and no agent activity in N days (default 7). Surfaces as an amber `stuck (Nd)` hint in the TUI; AGENT.md briefing on next attach starts with: "this workspace has been quiet for N days. Pick up where you left off, or close it?"

**Why:** Workspaces accumulate when an idea didn't pan out. Without explicit prompting, they stay in state.json forever. The shipped detector catches the "worked, ready to close" case; stuck catches the "didn't work, ready to abandon" case.

**Pros:** ~50 LOC. Same shape as rename_suggested / shipped — pure git read (last commit timestamp), no network. Cheap to add once the v0.6 detector framework exists.

**Cons:** Threshold is arbitrary; some users might be intentionally parking work. Configurable via `~/.canopy/config.json` `stuck_days_threshold` (default 7). False positives are mild — just a hint, not destructive.

**Context:** Implementation: `internal/lifecycle/stuck.go` reads `git log -1 --format=%ct` for the workspace's branch, compares to time.Now(). Hint kind: `stuck`. AGENT.md briefing variant on resume: prepended sentence acknowledging the gap.

**Depends on / blocked by:** v0.6 detector framework lands.

---

## v0.7 — Conflict detector

**What:** Detector that flags workspaces whose branch can't merge cleanly into the default branch. Surfaces as a red `conflict` hint; AGENT.md briefing on attach: "rebase onto origin/main first."

**Why:** Long-lived workspaces drift from main. By the time the user comes back, conflicts have accumulated. Surfacing this early saves the "tried to ship, hit conflicts, lost an hour" cycle.

**Pros:** ~60 LOC. Implementation: `git merge-tree` (3-way merge dry-run) returns conflicts as exit code or stderr signal. No external state needed.

**Cons:** Edge case for most users (branches usually merge cleanly). Adds noise if it fires for stale branches that the user is about to abandon anyway.

**Context:** `internal/lifecycle/conflict.go`. Run only on canopy reconcile (not every TUI refresh) — `git merge-tree` can be slow on large repos. Cache result for 10 min per workspace.

**Depends on / blocked by:** v0.6 detector framework lands.

---

## v1 — `canopy event` bus (REJECTED in v0.6, conditional revisit)

**What:** A `canopy event <kind> [args]` CLI verb the agent emits to communicate semantic lifecycle events to canopy (e.g., `canopy event scoped open-canopy-anywhere`, `canopy event committed "auth refactor done"`). Events persist in `~/.canopy/log/<project>/<workspace>/lifecycle.jsonl`. Detectors and downstream features (scripts.shipped hook, dashboards) can subscribe.

**Why considered:** During the v0.6 CEO review (2026-04-28), the agent-side equivalent of canopy detectors was proposed: instead of canopy reading git/gh state, the agent emits events and canopy stores them. Cross-tool integration becomes uniform (every consumer reads the event log).

**Why rejected:** User pushback during review: "agent has access to git/gh, do we really need events? Seems like overkill." Detectors reading git/gh state directly cover the use cases without a new contract.

**Conditional revisit trigger:** Add this only if a real use case shows up that detectors can't satisfy. Concrete example: the v0.7 `scripts.shipped` hook needs a precise "merged at <timestamp>" event that git can't tell us (only that the branch is reachable). If/when scripts.shipped grows that requirement, revisit the event bus.

**Pros (if revisited):** Agent-canopy contract is explicit. Decouples detector cadence (slow) from agent cadence (fast). Foundation for dashboards / Slack notifications / future automation.

**Cons (why we said no for now):** New API surface (verb + storage format + schema). Premature abstraction — solving a problem that doesn't yet exist. canopy stays focused on git+tmux orchestration, not message-bus-as-a-service.

**Depends on / blocked by:** A real, named use case. Not until then.

---

## v1 — In-session canopy overlay (TurboC++ style)

**What:** A persistent canopy presence inside an attached workspace tmux session — a status bar at the bottom (or a popup-on-hotkey) that shows the workspace name, port, status, and a row of F-key / single-letter shortcuts. One shortcut pops the full canopy splash/list as a `tmux display-popup` overlay so the user can switch workspaces without detaching the current session. Same overlay also shows quick-reference cheatsheet for canopy + tmux keybinds.

**Why:** Today switching between workspaces means: tmux prefix-d to detach → wait for canopy TUI to re-render → enter to attach to a different one. The detach round-trip is friction every time. A persistent overlay turns "switch workspace" into one keypress without leaving the current session's pane focus. Bonus: the cheatsheet solves the "I forgot what tmux prefix-d does" muscle-memory gap that bites every Conductor refugee.

**Why now (vs v2):** Lifts canopy from "a TUI you launch" to "ambient infrastructure", which is the Conductor north-star positioning. Avi already has the global splash screen in flight (parallel canopy session as of 2026-04-28), so the overlay's render target is being built independently — this entry is the "wire it into a tmux popup + status line" half.

**Pros:** Single-keystroke workspace switch. No more "wait, am I in cravd or canopy right now?" — the status bar always shows it. The cheatsheet eliminates the largest tmux-onboarding friction without editing the user's `.tmux.conf`. Pairs with global splash so the overlay can do "switch project AND workspace" in one popup.

**Cons:** Big design surface. Two distinct UI affordances bundled (status line + popup) and they probably ship in different orders. Keybinding choice is a minefield: every shortcut must NOT collide with shell readline keybinds (no `C-a`, `C-e`, `C-r`, `C-w`, `C-u`, `C-k`, etc.) AND must NOT collide with whatever tmux prefix the user has bound (Avi has changed his — needs to be checked at install). Status line takes vertical real estate in an already 3-pane layout. Implementation requires writing tmux config snippets canopy ships, plus an opt-in path for users who don't want their tmux line touched.

**Context:** Two layers:

1. **Status bar:** canopy ships a tmux config snippet (sourced via `source-file` from `~/.canopy/tmux.conf` written at `canopy init`) that adds a right-aligned status segment showing `<workspace> :<port> [<status>] [hints]`. The `[hints]` segment surfaces v0.6 detector hints inline (`✓ shipped`, `↻ rename`, `PR #N`) so the user sees lifecycle state from inside an attached session without detaching to the global TUI. Polled via `tmux refresh-client -S` every few seconds, populated from `canopy ls --tmux-status` (a new flag that emits a single line ready for tmux's `#()` interpolation). Opt-in only — `canopy init --with-status-bar` and prompted in onboarding.

2. **Popup launcher:** a tmux key binding (default `prefix-c` for "canopy"; user-overridable) runs `tmux display-popup -E -w 80% -h 80% canopy --popup`. The `--popup` flag tells canopy's TUI it's running in a transient popup, so quitting drops the popup instead of returning to a host shell. The popup's TUI is the existing list, but with a "switch and dismiss popup" verb (enter) and a "switch by detaching popup THEN attaching" path (handled via `tmux switch-client` from inside the popup process, which works because tmux popups inherit the client).

3. **Cheatsheet pane:** an alternate popup (default `prefix-?`) that shows a static panel of canopy + tmux keybinds. Lives in `docs/cheatsheet.md` rendered via lipgloss. Doesn't need the canopy state machine — pure render.

Keybind discipline: NEVER bind anything below tmux's prefix. All canopy keys go behind the user's existing tmux prefix (`<prefix>-c`, `<prefix>-?`, `<prefix>-s` for switch, etc.). Onboarding asks: "Your tmux prefix is `C-b`/`C-a`/other?" and writes the snippet accordingly. Never overwrite an existing user-bound key — detect via `tmux list-keys` parse and skip with a warning.

**Depends on / blocked by:** v0 (TUI exists, landed). v0.5 global splash + project listing (landed in ancient-hornet 2026-04-28). v0.6 detector framework (in flight) for the status segment's `[hints]` portion. The popup's first-launch render = the v0.5 splash; subsequent renders = the v0.5 global TUI's project list.

**v0.7 partial implementation:** the popup launcher and read-only status bar pieces of this v1 vision are scoped into v0.7 as `canopy popup` and `canopy statusline --format=current`. After codex review, scope was narrowed: cheatsheet pane, popup verb handoff, and the auto-installer are deferred to v0.8 (see CEO plan 2026-04-28-persistent-sidebar-tui.md and the new entries below). The v1 TODO stays as the long-term umbrella; v0.7 is the first ship of the pattern.

---

## v0.8 — `canopy session` (pinned-reach dedicated tmux session)

**What:** A subcommand that creates or attaches to a dedicated tmux session named with a reserved prefix (likely `canopy-hub-<project>`) running the existing global TUI in pane 0. Bound to `<prefix> G` in the user's tmux config, the chord flips the current tmux client to the canopy session. From canopy, picking a workspace fires `tmux switch-client -t <workspace-session>`. Picking nothing and re-pressing the chord flips back to the previous client (`tmux switch-client -l`).

**Why:** v0.7 ships the popup (ephemeral, tap-and-gone). The popup is universal but it covers your work for the duration of the pick. A dedicated session gives you "always-running, one chord away, from any tmux session" — the universal-tmux equivalent of Conductor's always-on window. After dogfooding the popup, this is the next layer if "I want canopy *running* somewhere I can flip to" turns out to be a real recurring need.

**Pros:** Cross-session reach (works from any tmux session). canopy state stays warm (no cold-start on each invocation). Pairs cleanly with the v0.7 popup — popup is the "quick switch", session is the "pinned dashboard". Universal across every OS canopy targets.

**Cons:** Reserved-prefix naming convention to design (avoid collision with existing `<project>-<workspace>` and `<project>-main` patterns). Lifecycle questions: when does the session die? Multi-client semantics. Adds a startup path the install command needs to handle.

**Context:** Triggered by the persistent-sidebar-tui CEO review on 2026-04-28. Scope was narrowed from "popup + session + statusline + 5 enhancements" to "popup + read-only statusline" after codex outside-voice review showed the session work alone has substantial design surface (T5: naming collision; T3: attach-vs-switch-client confusion). Ship after v0.7 popup has been used for ~2 weeks of dogfood — the popup-vs-session decision should be made on lived evidence, not hypotheticals.

**Depends on / blocked by:** v0.7 popup landed.

---

## v0.8 — `canopy install tmux` (idempotent ~/.tmux.conf writer)

**What:** A subcommand that writes canopy's tmux integration (popup keybind, statusline interpolation, future session keybind) into `~/.tmux.conf` between `# canopy:start` / `# canopy:end` marker comments. Idempotent re-runs replace the block in place. Backup at `~/.tmux.conf.canopy-backup-<timestamp>`. Supports `--uninstall` to remove the block. Refuses if tmux < 3.2 with a clear message. Detects pre-existing user binds for the same keys and asks before overriding.

**Why:** v0.7 ships the integration as a docs snippet the user pastes into their tmux.conf manually. That works for canopy's first-handful-of-users phase. As soon as multiple people are running canopy, "did you remember to source-file your tmux.conf" becomes the most common bug report. An installer fixes the discovery cliff.

**Pros:** Solves the discovery problem. Pairs with future `canopy install hypr-sidebar` and other per-WM installers — establishes the pattern. Makes onboarding a one-liner: `canopy init && canopy install tmux`.

**Cons:** Codex flagged this as the highest-risk feature in the original v0.7 plan. Doing it correctly means handling: TPM (`run -b 'plugins/...'`) and `source-file` includes, format/comment preservation, duplicate-block detection, conflicting bind detection, optional auto-`tmux source-file` reload. ~200+ LOC done robustly, not the ~50 LOC the original plan estimated. Easily a week of CC time on its own.

**Context:** Original v0.7 cherry-pick; deferred to v0.8 after codex outside-voice review. The v1 in-session-overlay TODO above references "canopy ships a tmux config snippet" — this is that snippet, productized.

**Depends on / blocked by:** v0.7 popup + statusline landed (so we know what to install).

---

## Future — Sidebar pane mode (`canopy --sidebar`)

**What:** A canopy mode that runs in a narrow vertical tmux pane (~25 cols), designed to live alongside the user's working pane(s) in the same tmux session. User splits the pane themselves (or via canopy install). Toggle key collapses the pane to 0 cols and back. Visible-alongside-your-work view — closest to the literal "Conductor sidebar" mental model.

**Why:** During the persistent-sidebar-tui CEO review, this was one of three candidate shapes (α). Avi chose to defer it after concluding "visible-alongside isn't a deal-breaker." If after v0.7 popup + v0.8 session dogfood there's still a recurring "I want canopy and my editor on screen at the same time" itch, this is the answer.

**Pros:** Literal sidebar feel. Works inside one tmux session without leaving it. Universal across every OS canopy targets.

**Cons:** Per-session — the sidebar pane lives in one tmux session at a time. Doesn't compose with `canopy session` (which is cross-session). Requires user-side tmux split-window choreography or a canopy-managed split. Discovery cliff unless paired with `canopy install tmux`.

**Context:** Deferred during the persistent-sidebar-tui review (2026-04-28) after Avi confirmed visible-alongside isn't a deal-breaker. Ship only if v0.7 + v0.8 dogfood reveals the felt-experience gap.

**Depends on / blocked by:** v0.7 popup landed; v0.8 dogfood signal.

---

## v0.8 — PR status in `canopy statusline`

**What:** Extend `canopy statusline --format=current` to surface PR state alongside workspace name/status/port. Concrete shape: `canopy: silent-falcon ●ready :40010 PR #42 ⚠conflict` (or `✓clean`, `…draft`, `⏳ci-running`, `✗ci-failed`, `✓merged`).

**Why:** Today the statusline tells you what workspace you're in and that it's healthy locally. It doesn't tell you whether you can ship — PR conflict, CI red, draft state, merged-and-forgotten. Surfacing PR state in the always-visible glance widget closes the "did I check that PR yet?" gap that bites every solo dev with multiple in-flight PRs.

**Pros:** Reuses the v0.6 detector framework (`pr_status` Hint kind already exists; the global TUI renders it). Composes with the existing statusline format machinery. Concrete, scoped.

**Cons:** Detector calls `gh pr view` which is a network round-trip — naively running it on every `status-interval` (15s) burns rate limit and adds 200-500ms latency to the statusline. Needs a cache (likely `~/.canopy/pr-status-cache.json` with TTL) and a background refresh strategy (probably the v0.6 lifecycle reconcile loop, which already pulls hints).

**Context:** Surfaced during v0.7 dogfood on 2026-04-29. User's request: "the statusline can be similar to the other one we have which also shows the PR status. we should have good PR statuses like conflict." Implementation needs: (1) wire statusline to the lifecycle detector framework's PR hints; (2) define a glyph mapping for PR states (conflict/clean/draft/ci-pass/ci-fail/merged); (3) cache results so tmux's 15s refresh doesn't hammer GitHub; (4) `--format=current` stays single-line — multi-line statusline is its own can of worms.

**Depends on / blocked by:** v0.7 popup + statusline landed; v0.6 detector framework already shipped. No blockers.

---

## v0.8 — Canopy actions from tmux key binds (Claude-driven)

**What:** Tmux keybinds (managed by `canopy install tmux`) that pipe canned prompts into the active workspace's claude pane via `tmux send-keys`. Concrete examples: `<prefix>M` = "merge this PR" (sends to claude), `<prefix>F` = "fix the failing CI", `<prefix>X` = "explain what broke." Each binding identifies the workspace (parse `tmux display-message -p '#S'`), looks up the claude pane (the `claude` pane in canopy's standard layout), sends the prompt + Enter.

**Why:** Avi's daily flow: see PR conflict in status bar → wants to ask claude to resolve. Today: switch to claude pane, type "merge this PR for me", enter. Two extra keystrokes minimum, plus context switch. With this: `<prefix>M`, claude starts working in the background, you stay where you are. The killer latency reduction for the agent-in-a-pane workflow.

**Pros:** Composes cleanly with v0.7 popup + statusline (same managed tmux block). Each binding is ~10 LOC of `tmux send-keys`. Configurable: ship a default set, let users add their own via `canopy.json` (similar to scripts.run).

**Cons:** Coupling to canopy's standard pane layout (claude is the second pane). Layout drift breaks the bindings. Send-keys to a wrong pane during workspace switches is a real risk — need to verify the workspace AND the pane name before sending. Prompt injection vector if canned prompts include workspace names or branch names without escaping.

**Context:** Surfaced during v0.7 dogfood on 2026-04-29 alongside the always-on keybind bar (which is the v1 umbrella). This is the smaller, action-oriented half: just the bindings, not the always-visible help bar. Ships independently.

**Depends on / blocked by:** v0.7 popup + canopy install tmux landed. v0.6 agent-pane stability (claude pane name is stable across canopy versions).

---

## v0.9 — Session-naming refactor: session = project, window = workspace

**What:** Restructure tmux session/window topology. Today: every workspace is its own tmux session named `<project>-<workspace>` (e.g., `cravd-misty-aspen`, `canopy-silent-falcon`). Proposed: one tmux session per project named `<project>`, each workspace is a window named `<workspace>` (or `<workspace>:<branch>` if they differ). `canopy main` becomes the project session's first window.

**Why:** As workspace count grows, `tmux ls` becomes a wall of `<project>-<workspace>` rows. Grouping windows under project sessions matches mental model ("I'm in the cravd project, switching between its workspaces") and lets users use tmux's window navigation (`<prefix>n/p`) for same-project workspace switching.

**Pros:** Cleaner `tmux ls`. Tighter mental model. tmux native window navigation works for intra-project switching. Status bar's `#W` (window name) becomes load-bearing identity, freeing `#S` (session name) to identify the project. Avi's request: "the session name can be the project name and the session tab can be the workspace name + branch name (if different)."

**Cons:** *Major* refactor:
  1. **`canopy popup` switch model breaks.** Today: `tmux switch-client -t <workspace-session>`. Tomorrow: `switch-client -t <project> + select-window -t :<workspace>`. Two-step dance, more places to fail.
  2. **Per-workspace env vars are session-level today** (CANOPY_PORT, CANOPY_WORKSPACE_PATH inherited by all panes). Window-level env is supported but has different inheritance semantics. Audit every CANOPY_* consumer.
  3. **`canopy main` overlaps awkwardly.** Today: separate session. Tomorrow: a window inside the project session — but is it window 0 (special) or just a regular window? What happens when the user has no workspaces yet?
  4. **Migration story.** Existing users' tmux state (live sessions named `<project>-<workspace>`) doesn't auto-migrate. Either a one-shot `canopy migrate-tmux-naming` verb, or accept that v0.9 invalidates resurrection until the user rebuilds.
  5. **`canopy.json` env-injection model needs review.** Hooks set CANOPY_* via session env; window-level requires either pane-level injection or window-env (newer tmux feature).
  6. **Statusline `current` lookup breaks** — today `tmux display-message -p '#S'` returns the workspace identifier; tomorrow `#S` returns the project name and `#W` returns the workspace.

**Context:** Surfaced during v0.7 dogfood on 2026-04-29. User's request was concrete: "the session name can be the project name and the session tab can be the workspace name + branch name (if different)." Has a "scrap it and do this instead" smell that warrants its own /plan-ceo-review and /plan-eng-review pass before any code changes — it's an architectural one-way door (every consumer of the naming convention has to migrate together). Recommendation: do it once, do it right, with a real migration story for existing users.

**Depends on / blocked by:** v0.7 + v0.8 dogfood signal that the cleaner topology is worth the migration cost. CEO+eng review of the trade-off table above.

---

## Future — Always-on keybind bar (TurboC++ style)

**What:** Extension of the existing v1 in-session-overlay TODO above. Specifically: a row in tmux's status-left or status-bottom showing the active canopy keybinds in real time, like TurboC++'s `F1 Help · F2 Save · F3 Open ...` bar. Updates contextually: in a workspace with PR conflict, the bar shows `M merge · F fix-ci · X explain`. Outside any canopy session, the bar is empty.

**Why:** Discoverability. Users forget `<prefix>g` opens the popup; they DEFINITELY forget `<prefix>M` asks claude to merge. An always-visible bar is the canonical fix. Pairs with the v0.8 Claude-driven actions (above) — actions are useless if users don't remember the bindings.

**Pros:** Solves discoverability for every keybind canopy ships. Users learn by glancing, not by reading docs. Composes with the existing v1 in-session-overlay TODO (same surface, this defines the content).

**Cons:** Vertical real estate is precious — the bar competes with the user's existing status-left/status-bottom content. Need an opt-in (`canopy install tmux --with-keybind-bar` or similar). Contextual keybinds require the statusline to know the current workspace's state (PR status, broken-ness, etc.) which doubles down on the v0.8 PR-status detector wiring.

**Context:** Avi's request on 2026-04-29: "thats why i was thinking of the turboc++ like statusbar that is always ready for keybindings... i'm sure theres a good easy way to do this." The "easy way" is a fair instinct — tmux's `status-format` is a genuinely powerful templating language and most of the work is content design, not implementation. But the design surface (which keybinds to surface? how to handle context-sensitivity? what about the user's existing status content?) is real.

**Depends on / blocked by:** v0.8 PR-status detector (so the bar can be contextual). v0.8 Claude-driven actions (so there's something worth surfacing).

---

## v0.8 — TUI unification: one model, three contexts (✅ SHIPPED v0.8.0, 2026-04-29)

**What:** Collapse canopy's three TUI flows into one. Today there are three separate Bubbletea models and three invocation paths:

```
                        Today                                   v0.8 unified
  ────────────────────────────────────────────────────────────────────────────
  canopy (in project)    → Model           (project TUI)        → unified TUI
  canopy (outside)       → GlobalModel     (global TUI)         → unified TUI
  canopy popup           → GlobalModel     (popup mode)         → unified TUI
                          via popup-inner   in display-popup        same code
```

The unified TUI is what the popup currently shows: Local + Global tabs, fuzzy search, status glyphs, n/d/R verbs. Attach behavior auto-adjusts to context (switch-client when inside tmux, attach when not — already abstracted via `attachVerbForCurrentEnv`). Popup-vs-not is just a presentation detail (display-popup hosts canopy directly; no separate `popup` subcommand).

**Why:** Avi's call on 2026-04-29 after dogfooding v0.7: "making things unify is the most important thing next... I don't think we will need a separate popup — I want to prioritize that. The sidebar is not very critical! Like the main TUI can be same as the popup."

The current three-model split was an artifact of incremental development:
- Model (project TUI) shipped first; scoped to one project; has destructive verbs.
- GlobalModel shipped in v0.5 for cross-project read-only view; deliberately read-only to keep the v0.5 scope tight.
- Popup mode bolted onto GlobalModel via AsPopup() during v0.7.

The v0.5 boundary ("global is read-only, project owns destructive") made sense before tabs existed. Now that the popup has Local/Global tabs and the popup body is the most-used canopy surface, that boundary is friction: users hit `o` to "open project" because they want destructive verbs, but the resulting project TUI is a different model with a different layout (different header, different keymap, no tabs), causing the screens-feel-different bug Avi reported.

**Pros:**

1. **One mental model.** Whatever invocation path the user takes, the screen is the same. Tab switches scope; everything else is identical.
2. **Eliminates the popup-mode coupling.** popup-inner can disappear; tmux's `bind g display-popup -E "canopy"` is enough. Less code, less surface area to test.
3. **`o` becomes redundant** in popup mode (no separate project view to open). Saves a keybind for something more useful.
4. **Destructive verbs (n/d/R) work everywhere.** Currently locked behind `o`; after unification they work directly on Local-tab rows. Force-retry-on-non-broken already shipped → confirmation modal in TUI for that becomes natural.
5. **Routing simplifies.** main.go's routeRoot becomes "always launch the unified TUI; pre-select Local tab if in a project, Global otherwise." No project-vs-global branch.
6. **The popup-attach exit-7 signal can be simplified** or removed — the unified TUI inside display-popup handles attach itself; no nested canopy spawn needed for `o`.

**Cons:**

1. **Real refactor.** The Model and GlobalModel today have different field sets, different state messages, different render paths. Unifying means picking one as the base and porting the other's features in. ~600-800 LOC of careful work + thorough testing.
2. **v0.5 read-only boundary disappears.** Currently the global TUI is intentionally safe — no destructive ops across projects. Unification means n/d/R work on Local tab rows from any invocation. Need to think about: what does `n` (new workspace) do when the cursor is on a different project's row vs current? Probably: only enabled on Local tab rows, since `n` requires canopy.json context.
3. **Popup keymap might grow.** Today the popup is intentionally minimal (arrow + enter + tab + / + q). Adding n/d/R to popup mode is fine but raises the visual noise. May want a "popup-mode" rendering toggle that hides destructive keys on cramped popup geometry.
4. **Tests need rework.** ~30 popup-mode tests (TestPopup* in model_global_test.go) will need rewriting since they're scoped to GlobalModel.
5. **Project TUI's specific layout may be missed.** The project TUI today has a project-name header, single-table layout. Some users might prefer that for their daily project work. The unified TUI's tab bar adds a row of chrome that's "useless" when you're 99%-of-the-time on Local tab. Mitigate: maybe the tab bar collapses to a single line when only one tab has rows, or hide tab bar entirely when invoked from project (`canopy --no-global` flag, or just default Local without showing the bar when only Local has data).

**Context:** Surfaced during 2026-04-29 dogfood after Avi noticed pressing `o` on a project loaded "a fully different screen" (different layout, no tab bar, different help line). The visual jolt confirmed the three-model split is leaking into UX. Avi's prioritization: "making things unify is the most important thing next" — this comes BEFORE PR-status statusline, Claude keybind actions, and session-naming refactor in the v0.8 backlog.

**Implementation sketch (rough — needs CEO+eng review before code):**

1. **Pick the merge target.** Likely `GlobalModel` becomes the base (it already has tabs, search, popup mode wiring). Migrate `Model`'s features onto it: project-scoped workspace creation flow (n), removal (d), retry (R), busy/streaming UI for long-running ops, hint badges, etc.
2. **Tab semantics:** Local tab pre-selected when invoked from inside a project (canopy.json walk-up succeeds); Global pre-selected otherwise. Single-tab fast path: if Local has rows AND Global is identical (no other projects), suppress the tab bar.
3. **Verb scoping:** n/d/R work on the cursor row regardless of tab. Validation: `n` needs the cursor row's project (Local context) — if cursor is on a Global-only row in a different project, prompt or refuse.
4. **Routing:** `routeRoot` always calls the unified TUI. Init splash for fresh repos stays separate.
5. **Popup → no separate command:** `canopy popup` and `canopy popup-inner` can be deleted. Tmux config snippet becomes `bind g display-popup -E "canopy"`. The unified TUI detects display-popup hosting via... probably `$TMUX_PANE` shape (tmux popup panes have a different ID format) or a CANOPY_IN_POPUP env we set via the install command's display-popup invocation.
6. **install tmux update:** rewrites the bind to use plain `canopy`, drops the popup-inner subcommand.
7. **canopy.session future:** ships AFTER unification because the dedicated session also hosts the same unified TUI.

**Estimated effort:** L (CC ~6-8 hours of design + implementation + testing). Worth a /plan-ceo-review and /plan-eng-review before implementation since this touches every TUI entry point.

**Depends on / blocked by:** v0.7 popup + statusline + install tmux landed (this branch). No external blockers. Should ship before any v0.8 features that build on the TUI surface (PR statusline is fine in parallel; Claude keybind actions can wait).

**CEO review status (2026-04-29):** /plan-ceo-review completed, 3-round spec review converged at 9/10. CEO plan: `~/.gstack/projects/canopy/ceo-plans/2026-04-29-tui-unification.md`. Approach: A (big-bang GlobalModel-as-base, single PR with 2-commit internal structure). Mode: SELECTIVE EXPANSION. Cherry-picks accepted: D3 (R confirm-on-non-broken modal), D5 (tab bar always-render with empty-state onboarding), D6 (cross-project d/R with project-name confirm), D7 (keymap.go extraction), D8 (filepath.EvalSymlinks fix in resolver port). Cherry-picks deferred: D4 (open-in-editor — see TODO below). **Run /plan-eng-review before implementation.**

---

## v0.8+ — Repurpose freed-up `o` keybind for "open worktree in editor" (deferred from TUI unification CEO review)

**What:** After v0.8 TUI unification, the `o` keybind is freed (its old purpose — open project TUI from popup — disappears). A natural reuse: launch the user's editor on the highlighted workspace dir.

**Why:** matches canopy's "switch fast" gestalt. You'd be inside tmux on the workspace one keystroke later anyway; `o` could short-circuit straight to `nvim /path/to/workspace` (or whatever the user has configured).

**Open design questions** (these are why this was deferred from the unification PR — they deserve their own design pass):
- Which editor binary? Read `$EDITOR`? Add an `editor` field to `canopy.json`? Both with config trumping env?
- Is it `o` (lowercase) or `O` (uppercase)? Does that matter? Lowercase fits the "single keystroke for the most common action" pattern; uppercase signals "this is more aggressive than just navigating."
- Where does the editor open?
  - New tmux pane in the current workspace's session? (closest to "switch fast" — but creates a new pane every time, clutter)
  - New tmux window in the current session? (cleaner, but same clutter problem)
  - Current pane (replacing the canopy TUI)? (zero clutter, but loses the canopy TUI entirely)
  - Detached tmux popup? (quick-edit a file feels great, but for a multi-file editor the popup geometry is too small)
- What's the behavior when invoked from outside tmux? Just `exec.Command(editor, path)`?
- What's the behavior when the workspace's tmux session isn't running? Auto-`canopy switch` first?
- Should this also exist as a CLI verb (`canopy edit <workspace>`)?
- Does it work on the Local tab only, or both Local and Global? (Probably both — opening another project's workspace in $EDITOR is a real use case.)

**Effort:** S (CC ~30-60 min once design is settled).
**Priority:** P2 (nice-to-have; muscle memory for `o` is valuable but small).
**Depends on / blocked by:** v0.8 TUI unification. Within unification PR, `o` is unbound (no-op).

---

## v0.9+ — "Recent workspaces" 3rd tab in unified TUI

**What:** After the unified TUI ships (v0.8), consider a 3rd tab beyond Local/Global: "Recent" — last N workspaces the user attached to, ordered by last-use timestamp.

**Why:** scales as canopy adoption grows. With 5 projects × 10 workspaces each, the Global tab gets noisy. Most-recently-used is a strong default for "what do I want next?"

**Pros:**
- Power-user feature; avoids fuzzy-search for the 5-most-recent case
- State already tracked (state.Workspaces have lastSwitchedAt or similar)

**Cons:**
- Adds a 3rd tab to the bar (always-show + dim policy still works)
- Requires defining "recent" precisely (last N? last 7 days? exclude main rows?)
- Premature if Global tab + fuzzy search covers 95% of cases

**Effort:** S (CC ~45 min).
**Priority:** P3.
**Depends on / blocked by:** v0.8 TUI unification.

---

## ✅ DONE 2026-04-30 — Surface version in `canopy use` and `canopy --help` (v0.12.3)

Both sub-items shipped on the `surface-version-in-use-and-help` branch.

`canopy use` listing has a VERSION column: release row execs
`canopy.bin version` once (2s timeout) and parses the first line; dev
workspace rows show plain "DEV" without forking; missing binaries
show "—". `canopy --help` leads with `canopy v0.12.2+<sha>` for
releases or `canopy DEV (workspace-name)` for dev builds, computed
once at process start via `versionDetails()`.

The "(stale: 12h)" suffix from the original sketch did not ship —
the BUILT column already conveys recency, and a separate stale
threshold needs design. Punt to a future round if anyone wants it.

---

## ✅ DONE 2026-04-30 — Upgrade UX overhaul (v0.13.0.0)

Shipped per design doc at `docs/design/v0.13-upgrade-ux.md`. All three
sub-items below landed in the same PR: auto-check pill + cache file,
in-TUI U-key flow with scrollable changelog viewport, and the
"press U to upgrade" integration. The deferred P3 entry's design
questions were resolved interactively during /plan-eng-review and
locked in the design doc.

Original P3 brief preserved below for historical context.

---

## P3 (HISTORICAL) — Upgrade UX overhaul (deferred from v0.12.0 ship 2026-04-30)

The v0.12.0 `canopy upgrade` is functional but spartan: explicit
invocation only, plain stdout, no proactive surface for available
updates. Three sub-items, design questions noted on each.

### Auto-check for available upgrades

**Where:** new helper, surfaced via `canopy ls` / TUI status / tmux statusline.

**What:** Avi originally said "no need for background" during v0.12.0
planning, then changed his mind after dogfooding. The right shape
isn't obvious — needs design.

**Design questions:**
- **Cadence:** check on every `canopy` invocation (cheap if cached)?
  Stat-on-launch with daily TTL? Background daemon (rejected by
  prior canopy design pattern — no daemon)?
- **Storage:** cache result in `~/.canopy/upgrade-check.json` with
  `{checked_at, latest_version, dismissed_until}`. TTL maybe 24h.
- **Surface:** where does "v0.13.0 available" show up?
  - Top-bar pill in the TUI (third pill alongside scope and version)?
  - One-line hint in `canopy ls` output?
  - Toast in the status bar that auto-dismisses?
  - Tmux statusline suffix ("[upgrade available]")?
- **Dismissal:** if user runs `canopy upgrade --check` and decides
  not to upgrade, suppress the hint until next version. Or for N days.
- **Network failures:** silent (just don't show the hint) — never
  block canopy on a failed check.

**Fix sketch:**
- New `cmd/canopy/upgrade_check.go` with `cachedRemoteVersion(ctx)`
  that reads/writes the cache file with TTL.
- Hook into the unified TUI `RunUnified` path: after `versionDetails`,
  call `cachedRemoteVersion` async; if result lands and remote >
  current, set a model field that the view renders.
- New `canopy upgrade --dismiss` flag to suppress the hint.

**Effort:** M (CC ~1-2h once design is settled).
**Priority:** P3 (nice-to-have, not blocking).

### TUI flow for `canopy upgrade`

**Where:** new `internal/ui/upgrade.go` (or popup mode of unified TUI).

**What:** Today `canopy upgrade` is plain stdout: prints versions, the
CHANGELOG diff, runs the shell, prints success. A TUI flow would
let users:
- See the CHANGELOG in a scrollable pane (long entries get truncated
  in plain stdout)
- Confirm before proceeding (currently auto-runs on `canopy upgrade`)
- See live progress during `git pull` + `make install` (not just
  silent block-and-wait)
- Surface specific failures (compile error, conflict) inline rather
  than dumping at the end

**Design questions:**
- Always TUI, or `--tui` flag, or auto-switch when stdout is a tty?
- Reuse the popup chrome (lipgloss-styled box) or fullscreen?
- How to render `git pull` / `make install` output streams — pipe to a
  scrolling log view?

**Fix sketch:**
- Default behavior unchanged when stdout is not a tty (CI, scripts).
- Tty + interactive: open a Bubbletea program that owns the upgrade
  flow end-to-end. Streams stdout/stderr from the shell commands into
  a viewport.
- Confirm gate before running shell. ESC cancels.

**Effort:** M-L (CC ~2-3h, depends on how rich the streaming gets).
**Priority:** P3.

### Auto-check + TUI together

If both ship, the auto-check hint becomes a "press U to upgrade now"
action that drops into the TUI flow. That's the satisfying user
experience: notification → action → done, without leaving the TUI.
Doable as a follow-up once both above are in.

---

## v0.15+ — Workspace actions menu (replace ambiguous `R retry`)

Captured 2026-04-30 from user feedback. Today the only re-run path is
`R` (retry scripts.setup), and the verb is opaque — "retry what?"
Meanwhile users have project-specific re-runnable operations (reseed
DB, run migrations, tail dev logs, restart server, refresh
fixtures…) with no first-class home in canopy. They live as ad-hoc
shell commands the user has to remember and type.

### Shape

Replace the single `R` keybind with an **actions menu** (`A` key, or
`R` repurposed). Opens a small picker over the workspace row listing
every action available for the current workspace. Built-in actions:

- **Re-run setup** (today's `R retry`, gated to broken+force as today)
- **Restart server** (kill scripts.run pane, relaunch)
- **Open in editor** (the freed-up keybind from the v0.8 unification —
  see deferred entry below)

User-defined actions come from a new `actions` block in canopy.json:

```json
{
  "scripts": { "setup": "...", "run": "...", "archive": "..." },
  "actions": {
    "reseed":   { "command": "bin/reseed",            "window": true,  "destructive": true,  "label": "Reseed DB" },
    "migrate":  { "command": "bin/rails db:migrate",  "window": true,                         "label": "Run migrations" },
    "logs":     { "command": "tail -f log/dev.log",   "window": true,                         "label": "Tail dev logs" }
  }
}
```

Field semantics:
- `command` — string, executed via `exec.CommandContext` (same env vars
  as scripts.setup: CANOPY_WORKSPACE_PATH, CANOPY_ROOT_PATH, CANOPY_PORT)
- `window: true` — spawn in a NEW tmux window inside the workspace
  session (visible to user, scrollable, non-blocking). Default false
  (= run inline, stream to drawer like setup does today).
- `destructive: true` — show confirm gate before running.
- `label` — display name in the picker; falls back to action key.

### Why `window: true` matters

This is the unlock the user is asking about. Today scripts.setup
output streams to the inspect drawer — fine for one-shot setup but
useless for `tail -f` or anything you want to watch alongside dev
work. Spawning in a new tmux window means:

- Dev pane keeps running undisturbed
- User can tab to the action window with the standard tmux next-window
  binding to watch progress
- Output stays around as long as the window stays open
- Long-running actions (rails console, bin/console, log streams) get a
  natural home

### Why this beats keeping `R retry`

1. **Discoverability.** The picker lists everything available; users
   don't have to remember which keys do what.
2. **Composability.** Every project has its own re-run rituals; shoving
   them into a single hardcoded verb doesn't scale.
3. **Naming.** The picker entry says "Re-run setup" not "retry," which
   answers "retry what?" without a glossary.

### Implementation sketch

- `internal/config/config.go` — add `Actions map[string]Action` field
  with validation (no key collisions with built-ins, command is
  non-empty).
- `internal/workspace/actions.go` — new package-level dispatcher.
  Built-ins map to existing lifecycle calls; user-defined actions
  shell out via `exec.CommandContext` with the same env contract.
- `internal/tmux/session.go` — add `NewWindow(session, name, cwd, cmd)`
  helper if not already present.
- `internal/ui/` — new picker mode (model state + view + update).
  Reuse the popup chrome from v0.11.0.
- Migration: keep `R` as a hidden alias for "Re-run setup" for one
  release so muscle memory survives.

### Deferred subquestions

- **Sequencing.** Should actions run serially (block) or fire-and-forget
  if `window: true`? Lean fire-and-forget when windowed, blocking when
  inline.
- **Status surfacing.** Action exit codes — where do they show up?
  Probably a transient toast in the TUI plus a line in canopy.log.
- **Keybind shortcuts.** Eventually each action could declare a
  one-key shortcut (e.g. `r` → reseed) for power users. Defer to v0.16+.

---

## v0.15+ — Onboarding wizard + global config

Captured 2026-04-30 from user feedback. Canopy has no global config
file today — every customizable surface is either hardcoded (pane
layout, editor=nvim, port range 3000-3999, default agent fallback) or
lives only per-project in canopy.json. There's no first-run flow,
no "change my defaults" verb, and new users discover canopy.json
schema by reading the README.

### Comprehensive settings inventory

This is what onboarding needs to surface. Some exist today, some are
new and would need to land alongside this feature.

#### Per-project settings (canopy.json, exists today)

| Setting | Today | Notes |
|---------|-------|-------|
| `scripts.setup` | ✅ required | runs once on workspace creation |
| `scripts.run` | ✅ required | server pane command, re-launched on resurrect |
| `scripts.archive` | ✅ required | runs on workspace removal |
| `scripts.agent` | ✅ optional | power-user override for agent launcher |
| `agent.type` | ✅ optional | claude / codex / opencode / aider, default claude |
| `agent.briefing` | ✅ optional | inline project notes appended to AGENT.md |
| `agent.briefing_file` | ✅ optional | path to a .md briefing (wins over inline) |
| `actions.<name>` | ❌ new | from the actions-menu feature above |
| `layout` | ❌ new | tmux pane layout override (see below) |
| `port_range` | ❌ new | currently hardcoded 3000-3999; e.g. Rails wants 3000s, Next wants 4000s |
| `default_base_branch` | ❌ new | currently inferred from origin/HEAD |

#### Global settings (~/.canopy/config.json, NEW — does not exist today)

| Setting | Notes |
|---------|-------|
| `default_agent` | machine-wide default when canopy.json doesn't set agent.type |
| `editor` | currently hardcoded to nvim in the editor pane; could be helix, code, zed, etc. |
| `multiplexer` | currently tmux-only; pluggable backend opens zellij/kitty (see v1 entry) |
| `pane_layout` | preset name: `4pane` (today's default), `tdl` (omarchy's terminal-dev-layout), `minimal` (just server + editor), `custom` |
| `pane_layout_custom` | full pane spec for the `custom` layout |
| `theme` | TUI palette: dark / light / high-contrast / protanopia (already implemented per #3 PR — surface as setting) |
| `upgrade_check.enabled` | bool; today implicit "on" |
| `upgrade_check.interval_hours` | today hardcoded 6h |
| `tmux.statusline_install` | bool; today implicit "on at first run" |
| `tmux.popup_install` | bool; today implicit "on at first run" |
| `shell_completion` | which shells canopy installed completion for (bash/zsh/fish) |
| `tdl_integration` | bool; if true, use omarchy's `tdl` instead of canopy's own pane builder |

The `tdl` integration is the user's specific ask — omarchy already has
a terminal-dev-layout helper that opens a known pane structure. If
canopy can defer to `tdl` when configured, users on omarchy get one
layout system instead of two competing ones.

#### State (~/.canopy/state.json, exists today)

NOT user-configurable — this is the workspace registry. Mentioned for
completeness so the onboarding flow doesn't accidentally claim it.

### Shape

`canopy onboard` — a TUI wizard, re-runnable any time. Three sections:

1. **Agent** — pick default AI tool; show each launcher's status (binary
   on PATH? authed?). Set `default_agent` in global config.
2. **Layout** — pick pane preset. Live preview by spawning a throwaway
   tmux session, screenshotting `tmux capture-pane`, killing it. (Or
   just ASCII-render the layout for v1.)
3. **Editor** — pick `nvim` / `helix` / `$EDITOR` / custom command.
4. **Advanced** — upgrade check, tmux integrations, theme, tdl mode.

For per-project: `canopy onboard --project` walks the canopy.json
schema, prompting for each field with the current value as default.
Useful for greenfield setup AND for migrating an existing canopy.json
to a new schema (e.g. adopting the actions-menu fields above).

### Auto-trigger on first run

If `~/.canopy/config.json` doesn't exist when canopy starts, prompt
once: "first time using canopy — want to run the 5-min onboarding
wizard? [Y/n/never]". `never` writes `{"onboarded": true}` so we
don't ask again. Skipping is fine — defaults stay as today.

### Implementation sketch

- `internal/config/global.go` — new file. Loads/writes
  `~/.canopy/config.json` with the schema above. All fields optional;
  zero values mean "use the canopy default."
- Every hardcoded constant today (`nvim`, `3000-3999`, `6h`, the
  4-pane builder) reads through a getter that consults global config
  with a code-default fallback. No flag-day rewrite — gradual.
- `cmd/canopy/onboard.go` — new subcommand. TUI flow built on the
  existing Bubbletea model patterns; reuse the picker chrome.
- Tests: a happy-path table-driven test for global config load/save +
  a unit test per getter that asserts both "no global config" and
  "global config overrides default" branches.

### Sequencing

Best landed AFTER the actions-menu feature (above), because actions
benefit from being introduced via the onboarding wizard ("here are
the actions your project supports — want to add one?"). Until both
ship, the global config is the more valuable half — it unblocks every
hardcoded-constant complaint a user has had so far.

---

## v0.15+ — Offboard / delete a project completely

Captured 2026-04-30 from user feedback. Today `canopy rm <workspace>`
removes ONE workspace. There's no verb for "I'm done with this
project — clean up everything canopy did for it." The user has to:

1. `canopy rm` every workspace one by one
2. Manually edit `~/.canopy/state.json` to remove the project entry
3. Manually `rm -rf ~/.canopy/workspaces/<project>/` if it persists
4. Maybe remove `canopy.json` from the repo (or not — depends on intent)
5. Maybe undo the tmux statusline / popup hooks (they're global, so
   probably not, but unclear)

That's manual, error-prone, and racy if any workspace tmux session is
still running.

### User intents (each shapes the verb differently)

1. **"I'm done with this project."** Drop all workspaces, drop project
   from state, leave the repo + canopy.json alone.
2. **"This project moved (renamed / migrated to org)."** Re-register
   under the new identity. Workspaces should ideally re-attach.
3. **"Wipe everything canopy did."** All of #1 plus removing canopy.json
   from the repo and any project-specific tmux artifacts.
4. **"I never want canopy globally."** Uninstall flow. Out of scope
   here — that's `make uninstall` + manual ~/.canopy purge.

### Proposed shape

`canopy offboard <project>` — interactive, default-safe.

```
canopy offboard cravd

The following will be removed:
  - 3 workspaces (still-salmon, dapper-hare, mute-otter)
    · 1 has open uncommitted changes (still-salmon, *4 dirty)
    · 1 has tmux session running (mute-otter)
  - project entry in ~/.canopy/state.json
  - workspace dir ~/.canopy/workspaces/cravd/

Will NOT remove:
  - canopy.json in the repo (use --remove-config to also delete)
  - ~/.canopy/log/canopy.log (shared across all projects)

Continue? [y/N]
```

### Safety design

- **Default refuses** if any workspace has uncommitted changes OR a
  running tmux session. `--force` overrides.
- **Per-workspace archive runs.** Each workspace's `scripts.archive`
  fires before its worktree is removed (DB drops, server kills, etc.)
  — same contract as `canopy rm`.
- **Atomic state update.** Acquire the state.json flock once, do all
  the removals, write the new state, release. No half-offboarded state
  if the user Ctrl-C's mid-flow.
- **Dry-run.** `--dry-run` prints the plan without doing anything.

### Flag set

- `--force` — proceed past dirty workspaces / running sessions.
- `--remove-config` — also delete canopy.json from the project root.
- `--dry-run` — print plan, change nothing.
- `--keep-workspaces` — drop project from state but leave worktrees on
  disk (escape hatch for users who want manual control).

### Implementation sketch

- `cmd/canopy/offboard.go` — new subcommand.
- `internal/workspace/offboard.go` — orchestration. Reuses
  `Manager.Remove` per workspace; new `dropProject` method on the
  state package for the registry surgery.
- `internal/state/state.go` — add `RemoveProject(root string) error`.
  Delete entries, garbage-collect empty Projects map keys.
- TUI surface: in the Global tab, a project-row action `O offboard`
  next to the workspace-row actions. Today the Global tab is
  workspace-rows only; this also implies a project-roll-up row above
  each project's workspaces. (Possibly nicer as a separate Projects
  tab — open question.)

### Deferred subquestions

- **Project rename intent (#2).** Should `canopy offboard --rename
  <new>` exist, or is rename a separate verb? Lean separate verb
  (`canopy reproject`?) — different consequences (offboard is
  destructive, rename is not).
- **Re-onboarding.** What happens if someone offboards then re-runs
  `canopy new` from inside the same repo? Should "just work" — re-add
  the project to state on first workspace creation, like a fresh setup.
  Confirm this in the test plan.

---

## v0.16+ — Kick-off-with-prompt + background workspaces (idea pool)

Captured 2026-04-30 from user feedback. Today `canopy new <branch>`
creates a workspace, runs scripts.setup, launches the agent with the
standard AGENT.md briefing, and drops you into the tmux session. The
agent sits at an empty prompt waiting for you to type the first
message. That's a missed beat — half the time the user already KNOWS
what they want the agent to do; they just want to fire and forget.

### The unlock

Pair two capabilities:

1. **Initial prompt** passed to the agent as the first user message.
2. **Detached/background kick-off** that creates the workspace, fires
   the prompt, and returns the user to the TUI without entering the
   session.

Together: "I have an idea. Open three workspaces, give each a
different angle on it, let them run while I'm at lunch." Real
parallel-AI workflow.

### CLI surface

```
canopy new add-oauth --prompt "Add OAuth login via GitHub. Read CONTRIBUTING.md first."
canopy new add-oauth --prompt-file ideas/oauth.md
canopy new add-oauth --prompt "..." --detach     # don't switch into session
canopy new add-oauth --prompt "..." --detach --notify   # notify on idle/done
```

### TUI surface

The `n` (new workspace) modal grows two optional fields:

- **Initial prompt** — multiline text input, optional. Empty = today's
  behavior.
- **Detach checkbox** — "Run in background, return to project list"

For longer prompts the modal could open `$EDITOR` (like git commit)
when the user presses a key — keeps the modal compact while supporting
real ideas, not just one-liners.

### Idea-bank workflow this unlocks

The user's framing: "kick off an idea in the background." This is the
seed of a richer flow:

- Maintain an `ideas/` dir in the project (gitignored or a separate
  branch). Each idea is one .md file.
- `canopy new --prompt-file ideas/X.md` per idea
- Run them in parallel detached
- TUI Global tab shows each one's progress (the existing PR/dirty/git
  hints already cover this — they just need to be informative for an
  in-flight agent run)
- A future `canopy idea <name>` shortcut wraps it: pick from the dir,
  spawn workspace, kick off agent, detach

### Implementation sketch

- **Launcher contract.** Each `internal/agent/launchers.go` entry
  accepts an optional `InitialMessage string`. Launchers convert it
  to whatever the underlying CLI wants:
  - `claude` — pass via positional arg (or stdin pipe; verify which is
    cleanest)
  - `codex` — `codex --prompt "..."` if supported, else stdin
  - `aider` — `aider --message "..."`
  - `opencode` — TBD; check the binary's flag
- **Plumbing.** New `--prompt` / `--prompt-file` flags on `canopy new`,
  passed through to `workspace.Manager.Create`, threaded into the
  launcher invocation. Mirror in the TUI new-workspace flow.
- **Detach.** Already partially in place — the v0.11.x detach pattern
  (spawn detached subprocess, `tmux detach-client`) generalizes. The
  new `--detach` flag means "skip the auto-switch-into-session step
  after creation"; tmux session stays alive in the background, scripts
  keep running.
- **State surfacing.** A workspace running an agent in the background
  needs to be distinguishable in the TUI. New status? Or an extra
  hint badge (`◷ running` next to the row)? Defer to design pass —
  could land as a follow-up after the basic primitive ships.

### Risks / open questions

- **Cost surprise.** Kicking off three Opus workspaces in parallel
  burns tokens fast. Maybe a confirm gate above N=2 backgrounded
  workspaces, or a per-project budget hint. Small, but worth thinking
  through before this becomes one-button-easy.
- **Idle detection.** Knowing when a backgrounded agent is "done" vs
  "stuck waiting on something" is a hard problem. v1 punt: treat all
  backgrounded sessions as in-progress until the user explicitly
  switches in to check. v2: pane-tail heuristics (no output for 30s
  with no in-flight tool call = idle).
- **Prompt-injection surface.** The initial prompt is just a user
  message — same trust boundary as anything the user types into
  claude themselves. No new exposure beyond what claude already has.

### Sequencing

Sits well AFTER:
- Actions menu (above) — gives a natural home for "send a follow-up
  prompt to a running agent" as another action.
- Global config (above) — `default_agent` lands first so the
  `--prompt` flag knows which launcher to talk to.

But the **`--prompt` half alone, no detach**, is a small ship by
itself: ~50 lines plumbing through the existing flow, gated to claude
launcher only at first. Worth doing as a precursor if the bigger
background flow is too far out.

---

## P3 — Defensive TestMain reap for crashed-runner stragglers (2026-05-01)

**Where:** `internal/tmux/session_test.go:TestMain` and
`internal/workspace/lifecycle_test.go:TestMain`.

**What:** The symmetric reap fix shipped today (extract `cwdScanForReap`
into a shared helper, wire it into both `Client.Kill` and
`KillServerAndReap`) closes the per-test leak path. It does NOT close the
**crashed-runner** leak path: when `go test` is itself SIGKILLed (Ctrl-C,
OOM, panic outside a recoverable goroutine), `t.Cleanup` never runs and
the test's tmux server is left orphaned to systemd-user.

Evidence (today): three orphan tmux servers from past runs were still
alive on the (since-removed) `canopy-test` socket — `TestSession_HappyPath`
(1d 12h old), `TestCreate_AlreadyExists` (1d 1h), `TestSelectPaneDirection`
(22h). They held tiny RAM individually but represent a real test-flakiness
hazard: tmux's "attach to existing server if socket present" semantics
mean the next `tmux.WithSocket("canopy-test-tmux")` call could inherit
state from a crashed predecessor.

**Fix:** in each tmux-using package's `TestMain`, before `m.Run()`:
1. Walk `/proc/*/cmdline` for processes whose comm is `tmux` and whose
   cmdline contains `-L canopy-test-tmux` (or `-L canopy-test-workspace`
   for the workspace package).
2. SIGKILL each. They have no live socket file by definition (old socket
   was either removed or replaced) so signaling is the only reach.
3. Also `rm -f /tmp/tmux-$UID/canopy-test-tmux` defensively, to handle
   the rare case where a test left the socket file on disk but the server
   inside it is dead.

~20-30 LOC each. Mirrors the pattern in `cwdScanForReap`. Test would be:
spawn a fake `tmux -L canopy-test-tmux` server in a sub-test, call the
helper, verify the server is dead — but this is overkill for hygiene
code; manual verification by introducing a deliberate `os.Exit(1)` mid-test
once during dev should be enough.

**Why deferred:** the symptom is rare (only manifests on crashed runners),
the impact is small (test flakiness, not user-facing), and bundling it
into the symmetric-reap PR widens the diff into a different kind of
change (test-harness hygiene vs. tmux primitive correctness). Right-sized
diff principle: ship the correctness fix with clean validation, defend
against the crash path separately.

**Depends on / blocked by:** nothing. Self-contained.

---

## P3 — Deferred from v0.15 (clear-workspace-identity ship)

### Lipgloss AdaptiveColor migration

**What:** canopy's internal/ui palette uses fixed ANSI 256 colors (e.g.
`lipgloss.Color("241")` for dim text). Light-terminal users see ~2.8:1
contrast on dim labels — fails WCAG AA.

**Why deferred:** canopy is dark-terminal-first; light-term support is a
broader migration touching every styler. Out of scope for the workspace-
identity feature.

**Where:** sweep all `lipgloss.NewStyle().Foreground(lipgloss.Color(...))`
calls and replace with `lipgloss.AdaptiveColor{Light: ..., Dark: ...}`.

**Depends on / blocked by:** nothing. Self-contained.

### DESIGN.md for canopy's terminal-UI conventions

**What:** color palette, glyph set, spacing rules, density philosophy. Today
these conventions live in scattered comments (`internal/ui/projectlist/
projectlist.go:849` for color 241 = "dim", `cmd/canopy/statusline.go:192`
for glyph set, etc.).

**Why deferred:** flagged in design review's Step 0B. Not blocking but useful
for future TUI changes — saves future-Claude (and future-Avi) from
rediscovering the precedents.

**Where:** new `DESIGN.md` in repo root. /design-consultation skill is the
right way to produce it.

**Depends on / blocked by:** nothing. Self-contained.

### Pre-existing TmuxSession field consumers

**What:** v0.15 dropped `tmux_session` from state.json. Any external script
that greps state.json for that field silently breaks. We have no known
consumers (only canopy's own code reads it), but worth documenting and
keeping an eye on.

**Why deferred:** speculative concern. If someone reports their gstack hook /
shellrc broke, point them at `TmuxSessionName()` (compute from project_root
+ branch via `safeNameForTmux`).

**Where:** documentation only.

**Depends on / blocked by:** community feedback.

---

## v0.16+ — Current-workspace context popup (the bigger arc)

Captured 2026-05-09 from user: *"there should be a way to open the
current workspace and do actions on the current workspace... this is
a bigger thing but we have a good start i think."*

The "good start" is the prefix-less `Ctrl+Alt+C` chord shipped in this
PR (`bind -n C-M-c display-popup -E "CANOPY_IN_POPUP=1 canopy"`). That
gives users a one-chord summon-canopy-from-anywhere experience. The
*next* layer is making the popup smart about *where you are*: when
fired from inside a canopy-managed workspace pane, the popup should
default to **acting on this workspace**, not the workspace switcher.

### Shape

The Ctrl+Alt+C popup has two contexts depending on `pane_current_path`:

1. **Inside a canopy workspace** → "Current workspace" mode.
   Header shows the workspace name, branch, status, agent state. Body
   is a fuzzy-searchable list of actions (rerun setup, restart server,
   open in editor, project-defined actions from canopy.json). Built-ins
   plus user-defined entries from the `actions` block — see the
   v0.15+ Workspace actions menu entry below for the action schema.

2. **Outside any workspace** → "Switch / create" mode (today's behavior).
   Workspace list across Local + Global tabs, `n` to create.

Both are the same popup; the mode flips based on whether
`workspace.ResolveCurrentProject` finds a managed workspace at the
firing pane's cwd.

### Why this is one design, not two

The user already has muscle memory for `<prefix>g` and `Ctrl+Alt+C`.
Splitting context-popup into a separate chord (e.g., `Ctrl+Alt+.`)
fragments that surface and forces another mnemonic. Letting the same
popup be context-sensitive is what tmux-menus, k9s, and lazygit all
do — the trigger is constant, the menu adapts.

### Inspirations driving this design (2026-05-08 ecosystem scan)

- **tmux-menus** (jaclu/tmux-menus) — single trigger key opens a
  navigable menu hierarchy. Standout: dim active-state items rather
  than hiding them. Direct inspiration for the popup hierarchy.
- **tmux-command-palette** (lost-melody/tmux-command-palette) —
  fzf-driven command palette over tmux actions. Shape the actions
  list as a fuzzy-searchable palette, not a fixed picker.
- **numentext** (numentech-co/numentext) — F1 searchable help in a
  Go TUI; same idea applied to keybindings + actions.
- **tmux-sessionx** (omerxx/tmux-sessionx) — session switcher with
  *live preview pane*. Worth borrowing for the switch-mode body —
  show actual tmux pane content next to the workspace list.
- **tmux-tab** (leohenon/tmux-tab) — Alt-tab between sessions with
  MRU ordering + previews. Suggests an MRU back-toggle hotkey
  canopy is missing.
- **tmux-agent-indicator + lazyclaude + opensessions** — three
  independent peers converging on per-pane AI-agent state
  (idle/thinking/awaiting-input) as the differentiator. Project
  memory `project_agent_state_unlocks_background.md` captures this
  as load-bearing for the v0.16+ background-workspaces TODO; the
  current-workspace context popup is the natural place to surface
  that state at the row level.
- **mynav** (GianlucaP106/mynav) — closest direct peer. Read their
  state model + workspace/session distinction before this design
  is locked in.

### Sequencing

1. ✅ Ship `Ctrl+Alt+C` no-prefix chord (this PR — foundation).
2. Land the Workspace actions menu (existing v0.15+ entry below) —
   builds the action dispatcher + canopy.json `actions` schema.
3. Wire context detection into the popup's entry point — same
   trigger, different default mode.
4. Add live preview pane to switch-mode (tmux-sessionx pattern).
5. Surface agent state on each row (the unlock for v0.16+
   background workspaces).

Steps 2–3 ship as v0.16; 4–5 may slip to v0.17 depending on appetite.

### Open questions

- Where does the "switch to a different workspace" entry live in
  current-workspace mode? Probably as an action: "Switch workspace…"
  pivots back to switch-mode.
- Should fuzzy-search be flat (one list, current actions + other
  workspaces interleaved) or modal (tabbed: Actions / Switch /
  Create)? Lean modal — flat lists with mixed entry types tend to
  feel chaotic past ~12 items.
- Does this subsume the Future "Sidebar pane mode" TODO, or do they
  coexist? Probably coexist — the popup is summon-on-demand, the
  sidebar is always-there. Same data model, two presentations.

### Depends on / blocked by

- v0.15+ Workspace actions menu (this entry references its action
  schema; can't ship context-popup without it).
- Agent-state indicator (nice-to-have for v1, not strictly required).

---

## References — tmux ecosystem inspirations (surveyed 2026-05-08)

Scanned awesome-tmux + numentech-co/numentext + jaclu/tmux-menus from
user prompt. Recorded here as the canonical pointer when reasoning about
canopy's switcher / palette / status surface evolution. Full taxonomy
+ "what to crib" notes live in memory `tools_tried.md` under "Tmux
ecosystem inspirations." Cross-referenced from the v0.16+ context-popup
entry above and the v0.15+ workspace actions menu entry below.

Highlights:

- **Agent-state visibility** (tmux-agent-indicator, lazyclaude,
  opensessions) — *the* highest-leverage cluster. Three independent
  peers converging on per-pane AI-state surfacing. Canopy is uniquely
  positioned because every workspace already has a known agent pane.
  Load-bearing for v0.16+ background workspaces (memory:
  `project_agent_state_unlocks_background.md`).
- **Live preview switcher** (tmux-sessionx, tmux-tab) — modern
  baseline canopy doesn't meet yet.
- **Command palette** (tmux-menus, tmux-command-palette, numentext
  F1) — `?`-triggered fuzzy-searchable action overlay.
- **Pinning UX benchmark** (tmux-grip) — read before iterating on
  the v0.15.1.0 `--pin/--unpin` work.
- **Direct peer** (mynav) — closest tmux-TUI worktree-flavored
  peer; study their state model.

---

## Wild idea — Cloud-hosted canopy workspaces (streamed terminals)

Captured 2026-05-08 from user. Pure idea-pool entry; no milestone, no
estimate. Worth a thought experiment + MVP probe, not a roadmap commit.

### The vision

A canopy workspace runs on a remote box (cloud VM, container, or
Fly Machine), not on your laptop. Every terminal in that workspace —
the agent terminal, the dev-server terminal, your `nvim` editing
session, the shell pane — streams from the cloud to your local tmux/
terminal. Your laptop is a thin client; the workspace state (worktree,
processes, agent context, file edits) lives in the cloud and survives
laptop sleep, network blips, and machine swaps.

The user pitch writes itself: "click `canopy new` from your phone,
walk to your desk, attach the same workspace from your laptop, keep
working." Or: "kick off four agent workspaces in parallel, each on its
own cloud box, none of them eating your laptop battery." It's
Conductor → tmuxinator → canopy → **canopy cloud**.

### What's actually new vs. what already exists

The "stream a remote terminal" half is solved technology — `mosh`,
`tmate`, `gotty`, `wetty`, plain `ssh + tmux attach` all do this. The
canopy-shaped half is the orchestration:

- Provisioning a workspace = "spin up a VM/container, clone the repo
  at the right commit, run scripts.setup, launch the agent, expose the
  tmux session over SSH/mosh/wss."
- Workspace state lives in the cloud's `~/.canopy/state.json`, mirrored
  back to the local TUI so the Global tab shows local + cloud rows
  side-by-side.
- The local TUI's "switch to workspace" action either runs the existing
  tmux attach (for local rows) or `ssh -t cloud-box tmux attach -t <ws>`
  (for cloud rows). Same keybind, two transports.
- nvim, claude, dev server — all run on the cloud. The local terminal
  is just a renderer. Latency budget: comfortable typing needs ≤80ms
  RTT, which means **region matters** (US-East user → US-East cloud).
- Editor experience: tmux + nvim over mosh is genuinely fine for most
  edits. Heavy LSP / treesitter is the test case. If it's bad, fallback
  is "edit locally via VS Code Remote SSH or `code-tunnel`, run agent
  remotely" — but that splits the model and probably feels worse than
  going all-in.

### The MVP

Smallest thing that proves the loop:

1. `canopy cloud-new <branch>` — same UX as `canopy new`, but instead
   of provisioning under `~/.canopy/workspaces/`, it `ssh`s to a
   pre-configured cloud box, runs a remote canopy binary that creates
   the workspace + tmux session over there, prints back a connection
   string.
2. `canopy cloud-switch <name>` (or extend Global tab with cloud rows)
   — runs `mosh cloud-box -- tmux attach -t canopy-<name>`. That's it.
   The user gets dropped into the same Bubbletea-launched tmux layout
   they'd get locally, just streamed from a remote machine.
3. State sync: cloud canopy posts its `state.json` to a known endpoint
   (or just `rsync ~/.canopy/state.json` periodically); local TUI
   merges remote rows into Global tab. Stale-tolerant.

The MVP can hardcode "the cloud box" to a single user-owned VM the
user pre-provisions (`canopy cloud-init` to bootstrap it: install
canopy, install tmux, clone the repo, set up scripts.setup deps).
**Multi-tenant cloud-as-a-service is not the MVP** — that's the
business; the MVP is "BYO box, canopy makes it usable."

### Open questions to chew on later

- **Ergonomics floor.** Is mosh + tmux + nvim actually pleasant for an
  8-hour session, or does it feel like working through a straw? Test by
  using it for one real day before over-engineering.
- **Repo state.** Cloud workspace clones from `origin`, but local
  uncommitted work is on the laptop. Either canopy `rsync`s the repo
  up before creating, or the workflow assumes "commit + push first."
  The latter is cleaner; matches how Conductor's cloud rumors work.
- **Cost shape.** Per-workspace VM is expensive idle. Container-per-
  workspace on a shared host is cheaper. Fly Machines auto-stop
  semantics are basically built for this. Worth a back-of-envelope
  before committing — but the MVP punts entirely (BYO box).
- **State sync direction.** Today `state.json` is local-canonical. With
  cloud rows, who's canonical? Probably each canopy is canonical for
  its own workspaces and the local TUI is a read-side aggregator.
  Avoids the distributed-systems tar pit.
- **Auth.** SSH keys for MVP. Anything fancier (OIDC, short-lived
  tokens) is product, not MVP.
- **Killer feature, not toy.** The fire-and-forget angle pairs hard
  with v0.16+ kick-off-with-prompt + background workspaces above. "Open
  three cloud workspaces with these three prompts, ping me when any
  agent is stuck." That's the unique thing — local-only canopy can't do
  it without burning your laptop.

### Why this is interesting (and why it's risky)

Interesting: it's the natural extension of canopy's "workspace as the
unit" — the unit doesn't have to live on your laptop. Risky: every
remote-terminal-as-IDE attempt has bumped against latency, editor
parity, and "I just want my files locally." The differentiator vs.
"just SSH" is the orchestration UX (one verb to provision + connect)
and the multi-workspace background-agent loop. Without that loop, this
is a glorified `tmate`.

### Posture

Park it. Re-visit after v0.1.0 ships and after the v0.16+ background-
agents idea matures (cloud is much more compelling once the local
background-agent UX exists, because the cloud version is "the same
thing, but it doesn't drain your laptop"). MVP probe = one weekend
hack on a personal VM to feel out the latency/UX floor before deciding
if it's a real product direction.

---

## Wild idea — Switch agent / model on a live workspace

Captured 2026-05-08 from user. Pure idea-pool entry; partially overlaps
with two earlier TODOs but pushes further:
- v0.5 "Configurable AI tool" (line ~466) — config-time picker.
- v1 "Multi-AI within one workspace" (line ~575) — parallel panes.

This entry is about the **third axis**: runtime switching of the agent
or model on an *existing* workspace. Cursor's composer shows a `GPT-5.5`
+ `Fast` chip the user can toggle per-turn (see screenshot 2026-05-08);
canopy today hardcodes claude for the lifetime of the workspace.

### What "switching" actually means (three nested levels)

The user's question — "what happens if we switch?" — needs disambiguation
because there are three different switches with three different blast
radii:

1. **Model within the same agent CLI.** `claude /model opus`,
   `codex --model gpt-5`. This is agent-internal; canopy has no role
   here. Just document it in the briefing or a help overlay — "press
   Ctrl-` to ask the agent to switch models." MVP: zero canopy code.
2. **Agent CLI for new turns on this workspace.** Workspace was launched
   with claude; user wants codex to take over from here. Canopy kills
   the claude pane, launches codex in the same pane, but the briefing
   + worktree + state.json all stay intact. Agent conversation context
   does NOT carry over (claude's `~/.claude/projects/<path>` is opaque
   to codex). New agent starts with a fresh briefing that includes a
   "you're picking up from claude — here's the diff so far, here's the
   active hint set."
3. **A/B parallel.** Two agents on the same workspace at the same time.
   Already covered by the v1 multi-AI parallel-pane entry. Cross-link.

This entry focuses on **(2)** — single-pane agent swap — because it's
the most interesting design question and the one that doesn't already
have a shape in TODOs.

### The thorny questions

- **What happens to the conversation?** Each agent CLI stores its own
  history under its own dotdir keyed by cwd. Switching agents = starting
  fresh on the new agent. Canopy can soften this by injecting a
  "previous agent was claude; here's a summary of the last N turns"
  block into the briefing — but extracting that summary from claude's
  JSONL is brittle. MVP: don't try; just brief the new agent with the
  workspace state (branch, hints, scope) and let the user re-state
  context if needed. Honest UX: "switching agents resets the
  conversation."
- **What happens to in-flight work?** If claude is mid-tool-call when
  the user hits switch, canopy needs to confirm ("claude is running a
  command — switch anyway?") and SIGTERM the agent process. Same shape
  as the existing kill-pane logic.
- **Does canopy track which agent ran which turn?** Probably not in v1
  — it's metadata users don't ask for until they do. But the workspace
  state could grow an `agent_history: [{agent, started_at, ended_at}]`
  list later. Cheap to add.
- **Briefing evolution.** Today the briefing has a Claude-shaped
  preamble ("You are the agent for this workspace…"). For agent swaps
  to work cleanly, the briefing needs to be agent-agnostic at the top
  and agent-specific only in the tool-call examples. The agent-
  lifecycle wrapper design (`docs/design/v0.6-agent-lifecycle.md`)
  already moves toward this — the launcher map abstracts the CLI; the
  briefing assembler abstracts the prompt. Agent-swap is the natural
  test case for whether that abstraction holds.
- **Re-resume semantics.** Today `canopy switch <name>` runs `claude
  --continue`. After an agent swap, the resume command for this
  workspace is now `codex` (no continue flag, codex doesn't have one,
  or whatever the right call is). Canopy needs to remember per-
  workspace which agent is active and use the right launcher map entry
  on resume. Field on `state.Workspace`: `Agent string`. Defaults to
  "claude" for back-compat.

### How the user triggers it

Picker UX options (think before committing):

- **Per-workspace command.** `canopy agent codex` from anywhere → finds
  the workspace by cwd, kills the agent pane, relaunches with codex.
  Easy, scriptable, no TUI work.
- **TUI keybind.** Inside the workspace TUI / popup, an `a` keybind
  opens an agent picker (list of agents from `canopy.json` `agents`
  block + a global default registry). Pick one → confirm → swap.
- **Cursor-style chip in the agent pane.** Not canopy's surface — that
  belongs to the agent CLI. Skip.

MVP probably picks the command form first (`canopy agent <name>`) because
it's testable without any TUI changes. TUI keybind comes after.

### MVP shape

1. Land the v0.5 "agent.type config + launcher map" first — that's the
   prerequisite. Without a launcher map there's nothing to swap to.
2. Add `Agent string` to `state.Workspace`, default "claude", populated
   on workspace creation from `canopy.json` config.
3. New verb: `canopy agent <agent-name>` (or `canopy switch-agent`).
   Resolves workspace by cwd, confirms (TTY) or forces (`--yes`),
   sends SIGTERM to the agent pane's process, updates
   `state.Workspace.Agent`, relaunches via the new launcher map entry
   with a fresh briefing that includes a "you're succeeding <prev-agent>
   on this workspace; full briefing follows" preamble.
4. `canopy switch <name>` reads `state.Workspace.Agent` and dispatches
   to the correct launcher's resume command.
5. Briefing assembler grows an optional `PrevAgent` field that, when
   set, prepends the handoff preamble.

~150-250 LOC + tests. Bigger than the rename verb; smaller than the
lifecycle wrapper itself.

### Why this is interesting (and why it's risky)

Interesting: the AI-CLI ecosystem is genuinely volatile (claude /
codex / aider / opencode / gemini-cli / amp). Canopy is in a privileged
position to be the *workspace shell* that doesn't care which agent you
use today vs. tomorrow. Agent-swap on a live workspace is the demo that
sells that pitch.

Risky: the conversation-loss UX is a real cliff. Users will instinctively
expect "switch from claude to codex" to mean "codex picks up where
claude left off," which is technically not possible without writing a
canopy-level conversation transcript layer (huge scope, brittle, not
worth it). Honest framing matters — "switching agents starts a fresh
conversation; the workspace state and worktree stay" — and the briefing-
preamble handoff softens but doesn't eliminate the cliff.

### Posture

Park behind two prereqs: (1) agent-lifecycle wrapper ships and the
launcher map exists, (2) at least one second agent (codex or aider) has
a real launcher entry tested in dogfood. Without both, this is paper.
After both, it's a one-PR feature with high marketing leverage.

**Depends on / blocked by:** v0.5 agent-lifecycle wrapper +
launcher map. See `docs/design/v0.6-agent-lifecycle.md`.
