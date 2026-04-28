# TODOs

Items deferred during /office-hours and /plan-eng-review on 2026-04-27.
Each entry is self-contained for someone (you, future-Claude, or another AI agent) picking it up months later.

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

## v0.6 — Agent lifecycle wrapper + detectors

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

## v0.5 — Per-project + per-workspace logs

**What:** Replace the single global `~/.canopy/log/canopy.log` with a project-aware tree:

```
~/.canopy/log/canopy.log              # global events: state-store, project discovery
~/.canopy/log/<project>/canopy.log    # everything tied to a project
~/.canopy/log/<project>/<workspace>/setup.log    # raw stdout/stderr of scripts.setup, per attempt
~/.canopy/log/<project>/<workspace>/archive.log  # raw stdout/stderr of scripts.archive
```

**Why:** Today every project's lifecycle events interleave in one file, so debugging a cravd setup failure requires grepping through brain's setup chatter. Per-workspace setup logs also let the auto-detect hint flow (entry below) link directly to a small file ("see full output: `~/.canopy/log/cravd/bold-falcon/setup.log`") instead of pointing at the firehose.

**Pros:** Greppability. Cleaner `tail -f` for the workspace you care about. Pairs with retry — each retry attempt rotates the setup log (`setup.log.1`, `setup.log.2`, ...) so you can compare what changed. The auto-detect hint becomes a one-line teaser that links to the full file. Easy to tar up a single project's logs to share when reporting a bug.

**Cons:** Slog handler swap is straightforward; the per-workspace setup-log capture is a tee on the `stdout`/`stderr` writers passed into `runSetupHooksOnly`. Migration: existing single log file stays in place, new logs go to the new tree — no destructive rename. Disk usage: ~40 KB per setup attempt × workspaces × retries; cap at 5 rotated files per workspace.

**Context:** clog package today takes a single path; refactor `clog.Init` to accept a default project hint (cwd-walk-up) so a Manager constructed via `loadManager()` automatically routes its logs into the right subdir. Per-workspace setup logs live in `internal/workspace/lifecycle.go` — wrap the stdout/stderr writers passed into `runSetupHooksOnly` with `io.MultiWriter(originalWriter, fileWriter)` where the file is opened at `<project>/<workspace>/setup.log` with O_TRUNC each time so retries always see a fresh file (rotated copy of the previous attempt kept as `setup.log.1`).

**Depends on / blocked by:** v0. No blockers.

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
