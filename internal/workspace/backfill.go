// Backfill of @canopy-role tags for sessions created before v0.16.
//
// v0.15 and earlier sessions don't have @canopy-role set on their panes.
// When canopy v0.16+ attaches to one of those still-running sessions
// (via `canopy switch`, the TUI's attachOrSwitch, or EnsureMainSession's
// already-alive branch), it runs BackfillRoles to retroactively tag the
// canonical layout. Sessions that get killed and resurrected in v0.16
// always go through buildSession/Resurrect which tag at creation time —
// so backfill is only for the "never died, just unattached" case.
//
// Conservative heuristic per /office-hours + /plan-eng-review + /codex
// reviews: only tag when the session has EXACTLY 3 panes (the canonical
// canopy layout). Sessions with manual splits or extra panes get
// skipped with a warn — wrong tagging is worse than no tagging.

package workspace

import (
	"context"
	"strings"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

// BackfillRoles retroactively tags the panes of a still-alive session
// with @canopy-role values, for sessions created before v0.16.
//
// Best-effort and idempotent:
//   - Already-fully-tagged sessions: no-op.
//   - Canonical 3-pane layout, partially or fully untagged: tag in
//     list-panes ORDER (position 0 → ide, 1 → agent:<launcherType>,
//     2 → terminal:shell). Layout-tree depth-first traversal, NOT pane
//     creation order. NOT raw #{pane_index} — codex caught: users with
//     `pane-base-index 1` in tmux config would mistarget.
//   - Non-canonical pane count (≠3) or window count (≠1): skipped with
//     a warn line. The workspace continues to function via existing
//     index-based code paths until next resurrect (which fully tags
//     fresh).
//
// All errors are logged and swallowed — backfill is informational, not
// load-bearing. A backfill failure must NOT prevent the user from
// attaching to their workspace, so this function returns no error: the
// caller has nothing actionable to do with one.
//
// launcherType comes from the workspace's canopy.json Agent.Type. For
// canopy main sessions, pass "" — agent.RoleForType handles the empty
// case by defaulting to "agent:claude" (matches main_session.go's
// hardcoded `claude --continue || claude` literal).
//
// Known residual limitations (acknowledged, not yet fixed — captured in
// TODOS.md):
//   - Fully-but-wrong-tagged session: an already-tagged session that
//     was tagged in the WRONG order (e.g., from a stray manual SetRole
//     or pre-fix v0.16.0 behavior) is treated as a no-op. Never gets
//     repaired.
//   - launcherType vs observed-command divergence: in Global flows
//     where launcherType is empty (UI auto-attach across projects),
//     a pane actually running codex/opencode gets tagged as
//     agent:claude because the canonical role uses the launcherType
//     argument, not the sniffed command. Downstream polling sees the
//     wrong launcher.
//   - HasClient swallows ALL tmux errors as "no client" (see its
//     docstring) — if the tmux server fails non-trivially, the
//     concurrent-attach guard silently disables itself. Defense-in-
//     depth, not a full mitigation: the residual race window is
//     narrow but real.
//   - TOCTOU between ListAllRoles / PanesInOrder / PaneCommands: pane
//     state can shift between the three tmux calls. Net effect is
//     bounded by the incongruence + command-sniff checks, but a
//     pathological multi-process race could still partially mistag.
func BackfillRoles(ctx context.Context, tc *tmux.Client, session, launcherType string) {
	// Step 1: check if all required roles are already set. Skip if yes.
	// Codex caught: checking only `agent:*` would silently leave
	// partially-tagged sessions unfinished.
	allRoles, err := tc.ListAllRoles(ctx, session)
	if err != nil {
		log.Warn("workspace.backfill.list-roles-failed",
			"session", session, "err", err.Error())
		return
	}
	required := map[string]bool{"ide": false, "terminal:shell": false, "agent:": false}
	for _, ri := range allRoles {
		if ri.Role == "ide" {
			required["ide"] = true
		} else if ri.Role == "terminal:shell" {
			required["terminal:shell"] = true
		} else if strings.HasPrefix(ri.Role, "agent:") && len(ri.Role) > len("agent:") {
			required["agent:"] = true
		}
	}
	allPresent := true
	for _, ok := range required {
		if !ok {
			allPresent = false
			break
		}
	}
	if allPresent {
		return // fully tagged, nothing to do
	}

	// Concurrent-attach guard: if a client is already attached to this
	// session, skip backfill. The hazard is two simultaneous `canopy
	// switch <ws>` invocations against the same v0.15-style session
	// both seeing "untagged" and racing through SetRole. tmux's
	// set-option is idempotent so the writes don't corrupt, but the
	// read-modify-write window can mistag if pane state shifts mid-
	// flow. Bailing out on attached-clients narrows the window
	// drastically — the second invocation in the user's "open two
	// canopy switches in two terminals" workflow will see the first's
	// client and skip.
	//
	// This is best-effort, not a complete mitigation. A fully concurrent
	// pair of switches launched before either attaches would still both
	// see no clients. The v0.16 incongruence check is the load-bearing
	// safeguard against the actual overwrite bug; this just shrinks the
	// race surface for the rare two-process case.
	if attached, err := tc.HasClient(ctx, session); err != nil {
		log.Warn("workspace.backfill.has-client-failed",
			"session", session, "err", err.Error())
		// Fall through — best-effort.
	} else if attached {
		log.Info("workspace.backfill.skipped.client-attached",
			"session", session,
			"note", "another client is in this session; deferring backfill to avoid concurrent SetRole race")
		return
	}

	// Step 2: count windows + panes. Skip backfill if non-canonical.
	//
	// Canopy's canonical session shape is one window with exactly three
	// panes. Without the window-count gate, three panes spread across
	// multiple tmux windows (a user splitting off extra workspaces or
	// custom layouts) would still pass the `count == 3` check —
	// list-panes -s is session-scoped, NOT window-scoped — and the
	// positional inference would tag arbitrary panes from arbitrary
	// windows. The window gate is the cheapest defense.
	winCount, err := tc.WindowCount(ctx, session)
	if err != nil {
		log.Warn("workspace.backfill.window-count-failed",
			"session", session, "err", err.Error())
		return
	}
	if winCount != 1 {
		log.Warn("workspace.backfill.skipped",
			"session", session, "window_count", winCount, "expected", 1,
			"note", "multi-window layout; positional inference unsafe — will tag fresh on next resurrect")
		return
	}

	count, err := tc.PaneCount(ctx, session)
	if err != nil {
		log.Warn("workspace.backfill.pane-count-failed",
			"session", session, "err", err.Error())
		return
	}
	if count != 3 {
		log.Warn("workspace.backfill.skipped",
			"session", session, "pane_count", count, "expected", 3,
			"note", "non-canonical layout; will fully tag on next resurrect")
		return
	}

	// Step 3: tag panes in list-panes ORDER, not raw #{pane_index}.
	// Codex-corrected: pane-base-index 1 configs would mistarget if
	// we used the raw index field.
	panes, err := tc.PanesInOrder(ctx, session)
	if err != nil {
		log.Warn("workspace.backfill.list-panes-failed",
			"session", session, "err", err.Error())
		return
	}
	if len(panes) != 3 {
		// Race — pane count changed between PaneCount and PanesInOrder.
		// Bail out conservatively.
		log.Warn("workspace.backfill.race-detected",
			"session", session, "pane_count", len(panes))
		return
	}

	// canonical layout — order matches tmux's list-panes DEPTH-FIRST
	// traversal of the layout tree, NOT pane creation order. This was
	// verified empirically via the v0.16 smoke test against a fresh
	// `canopy new` session: list-panes returned [ide pane, agent pane,
	// shell pane] even though the splits ran ide → shell → agent.
	//
	// Layout tree shape (after canopy's standard splits):
	//   window
	//   ├── top 85% horizontal-split
	//   │   ├── ide (left ~70%)
	//   │   └── agent (right ~30%)   ← list-panes position 1
	//   └── shell (bottom 15%)        ← list-panes position 2
	//
	// IMPORTANT: if buildSession's split sequence ever changes (e.g.,
	// shell becomes the second split instead of the first), this
	// canonical mapping must update to match the new traversal order.
	canonicalRoles := []string{"ide", agent.RoleForType(launcherType), "terminal:shell"}

	// Build paneID → existing-role map for O(1) lookup.
	existing := make(map[string]string, len(allRoles))
	for _, ri := range allRoles {
		existing[ri.ID] = ri.Role
	}

	// Incongruence check: if ANY pane already has a role that doesn't
	// match the canonical positional expectation, skip backfill entirely.
	// This prevents overwriting user-set tags or tags from a non-canonical
	// layout (e.g., a custom session tagged in a different order).
	//
	// Caught by adversarial review: previous behavior would overwrite
	// "ide" with "agent:claude" if the ide-tagged pane happened to land
	// at list-panes position 1 (the canonical agent slot). Real bug.
	for i, paneID := range panes {
		if existing[paneID] != "" && existing[paneID] != canonicalRoles[i] {
			log.Warn("workspace.backfill.skipped.incongruent",
				"session", session, "pane", paneID,
				"existing_role", existing[paneID],
				"expected_role", canonicalRoles[i],
				"note", "existing tag inconsistent with canonical layout; not modifying any pane")
			return
		}
	}

	// Command-sniffing safeguard: cross-check `pane_current_command`
	// against the canonical positional expectation. Catches sessions
	// where the layout has 3 panes but they're NOT canopy's canonical
	// [ide, agent, shell] arrangement (e.g., user manually rearranged or
	// restarted things). Positional inference alone can't see this; the
	// command running in each pane reveals the mismatch.
	//
	// Conservative: only refuse when a pane's command CLEARLY belongs to
	// a different canonical slot. Empty / unknown commands (pane between
	// commands, custom editor, exotic shell) don't trip the check — we'd
	// rather tag a slightly-off pane than refuse on every non-mainstream
	// setup.
	cmds, err := tc.PaneCommands(ctx, session)
	if err != nil {
		log.Warn("workspace.backfill.pane-commands-failed",
			"session", session, "err", err.Error())
		// Fall through — command sniffing is defense-in-depth, not load-bearing.
	} else {
		for i, paneID := range panes {
			actual := cmds[paneID]
			expected := canonicalRoles[i]
			if commandConflictsWithRole(actual, expected) {
				log.Warn("workspace.backfill.skipped.command-mismatch",
					"session", session, "pane", paneID,
					"pane_command", actual,
					"expected_role", expected,
					"note", "pane is running a command that belongs to a different canonical role; not modifying any pane")
				return
			}
		}
	}

	// Safe to tag: only fill in panes that are currently untagged.
	// Never overwrite existing tags (we verified above they're congruent).
	tagged := 0
	for i, paneID := range panes {
		if existing[paneID] != "" {
			continue // already tagged correctly per the incongruence check
		}
		want := canonicalRoles[i]
		if err := tc.SetRole(ctx, paneID, want); err != nil {
			log.Warn("workspace.backfill.set-role-failed",
				"session", session, "pane", paneID, "role", want, "err", err.Error())
			// Continue with other panes even on partial failure.
			continue
		}
		tagged++
	}
	if tagged > 0 {
		log.Info("workspace.backfill.tagged",
			"session", session, "launcher", launcherType, "panes_tagged", tagged)
	}
}

// commandConflictsWithRole reports whether `cmd` (a value from
// `pane_current_command`) is recognizably the wrong canonical-role
// CLASS for the slot at that position. Used by backfill's command-
// sniffing safeguard to refuse tagging when the layout clearly doesn't
// match canopy's canonical [ide, agent, shell] arrangement.
//
// Matching is by role CLASS (ide / agent / shell), not exact role
// string — a pane running `codex` in a slot canonically assigned to
// `agent:claude` is NOT a conflict (user intentionally switched
// agents), but a pane running `nvim` in the shell slot IS.
//
// Shell-class commands (bash, zsh, fish, …) are treated as PERMISSIVE
// in any slot: the agent or editor may simply not be running yet
// (crash, not started, user dropped to shell). Refusing on shell-in-
// agent-slot would break the common "I quit claude to poke around"
// case. Only "ide" or "agent" classified commands landing in the wrong
// slot trigger refusal — those are unambiguous mistagging signals.
//
// Returns false (no conflict) for empty or unknown commands too.
func commandConflictsWithRole(cmd, role string) bool {
	cmdClass := classifyCommand(cmd)
	if cmdClass == "" || cmdClass == "shell" {
		return false // unknown or shell-class; permissive
	}
	roleClass := roleClassOf(role)
	if roleClass == "" {
		return false // unrecognized role; don't second-guess
	}
	return cmdClass != roleClass
}

// classifyCommand maps a `pane_current_command` value to its canonical
// role class ("ide", "agent", "shell"). Returns "" for unknown commands.
func classifyCommand(cmd string) string {
	switch cmd {
	case "":
		return ""
	case "nvim", "vim", "vim.tiny", "vim.basic", "vim.nox", "vi", "hx", "helix", "emacs":
		// vim.tiny / vim.basic / vim.nox are the Debian/Ubuntu vim
		// variants — GitHub Actions runners ship vim.tiny as /usr/bin/vim,
		// and pane_current_command reports the real comm not the symlink.
		return "ide"
	case "bash", "zsh", "fish", "sh", "dash", "ksh":
		return "shell"
	}
	for _, t := range agent.KnownAgents() {
		if cmd == t {
			return "agent"
		}
	}
	return ""
}

// roleClassOf maps a canopy role string ("ide", "terminal:shell",
// "agent:<type>") to its role class. Returns "" for unrecognized roles.
func roleClassOf(role string) string {
	switch role {
	case "ide":
		return "ide"
	case "terminal:shell":
		return "shell"
	}
	const prefix = "agent:"
	if strings.HasPrefix(role, prefix) && len(role) > len(prefix) {
		return "agent"
	}
	return ""
}
