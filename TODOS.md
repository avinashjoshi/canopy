# TODOs

Items deferred during /office-hours and /plan-eng-review on 2026-04-27.
Each entry is self-contained for someone (you, future-Claude, or another AI agent) picking it up months later.

---

## v0.5 — multi-project support

**What:** Replace single-project-via-cwd-walkup with cross-project workspace listing. Project picker modal in TUI. `-p <project>` flag on subcommands. Project sidebar UX.

**Why:** Avi has multiple projects in `~/Work/` (cravd, brain, hey-cli, dotfiles, tries). v0 only handles whichever repo you're cd'd into. Once cravd is dogfood-stable, switching across projects in one TUI session is the next big UX unlock.

**Pros:** Matches the original spec. Removes the "cd to canopy a different project" friction. Sets up a project-aggregator pattern that scales to many repos.

**Cons:** Adds project-picker modal (~3 hours Bubbletea), `-p` flag plumbing, and a registry of "known projects" (probably `~/.canopy/projects.json` — list of repo paths that have a `canopy.json`). Real scope.

**Context:** v0 design discovers project via cwd walk-up. State already keys workspaces by `(project, name)` tuple, so the data model supports multi-project from day 1 — only the UX and discovery layer need work. Good entry points: `internal/config/discover.go` (add `DiscoverAll() []Project`), `internal/ui/` (add project sidebar), `cmd/canopy/new.go` (add `-p` flag).

**Depends on / blocked by:** v0 must ship first. No technical blockers.

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

## v0.5 — Multi-AI-tool support (via layout-as-config)

**What:** Make the AI pane configurable in `canopy.json` so users can choose claude / codex / opencode / aider / etc. per project. Both the launch command AND the resume command go in config.

**Why:** v0 hardcodes `claude` and `claude --continue`. Other AI tools have different invocation + resume patterns. As more devs adopt different AI CLIs (codex, opencode, aider), canopy stays tool-agnostic — it just runs whatever command the project's `canopy.json` specifies.

**Pros:** Aligns canopy with the layout-as-config v0.5 milestone (one feature, two wins). Future-proofs for AI-tool churn (the AI CLI landscape is moving fast). Lets one repo use claude while another uses codex without canopy caring.

**Cons:** Surfaces a quality variance: AI tools that lack per-directory storage or a non-interactive resume flag will work in canopy but lose the resurrection magic. Documenting the "what works fully vs partially" matrix is a small README chore.

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
