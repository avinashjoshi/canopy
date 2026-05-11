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

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

// BackfillRoles retroactively tags the panes of a still-alive session
// with @canopy-role values, for sessions created before v0.16.
//
// Best-effort and idempotent:
//   - Already-fully-tagged sessions: no-op.
//   - Canonical 3-pane layout, partially or fully untagged: tag in
//     list-panes ORDER (position 0 → ide, 1 → terminal:shell, 2 →
//     agent:<launcherType>). NOT raw #{pane_index} — codex caught:
//     users with `pane-base-index 1` in tmux config would mistarget.
//   - Non-canonical pane count (≠3): skipped with a warn line. The
//     workspace continues to function via existing index-based code
//     paths until next resurrect (which fully tags fresh).
//
// All errors are logged and swallowed — backfill is informational, not
// load-bearing. A backfill failure must NOT prevent the user from
// attaching to their workspace. Callers should not check the return
// value (kept as `error` for future flexibility but always nil today).
//
// launcherType comes from the workspace's canopy.json Agent.Type. For
// canopy main sessions, pass "" — agent.RoleForType handles the empty
// case by defaulting to "agent:claude" (matches main_session.go's
// hardcoded `claude --continue || claude` literal).
func BackfillRoles(ctx context.Context, tc *tmux.Client, session, launcherType string) error {
	// Step 1: check if all required roles are already set. Skip if yes.
	// Codex caught: checking only `agent:*` would silently leave
	// partially-tagged sessions unfinished.
	allRoles, err := tc.ListAllRoles(ctx, session)
	if err != nil {
		log.Warn("workspace.backfill.list-roles-failed",
			"session", session, "err", err.Error())
		return nil
	}
	required := map[string]bool{"ide": false, "terminal:shell": false, "agent:": false}
	for _, ri := range allRoles {
		if ri.Role == "ide" {
			required["ide"] = true
		} else if ri.Role == "terminal:shell" {
			required["terminal:shell"] = true
		} else if len(ri.Role) > len("agent:") && ri.Role[:len("agent:")] == "agent:" {
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
		return nil // fully tagged, nothing to do
	}

	// Step 2: count panes. Skip backfill if non-canonical.
	count, err := tc.PaneCount(ctx, session)
	if err != nil {
		log.Warn("workspace.backfill.pane-count-failed",
			"session", session, "err", err.Error())
		return nil
	}
	if count != 3 {
		log.Warn("workspace.backfill.skipped",
			"session", session, "pane_count", count, "expected", 3,
			"note", "non-canonical layout; will fully tag on next resurrect")
		return nil
	}

	// Step 3: tag panes in list-panes ORDER, not raw #{pane_index}.
	// Codex-corrected: pane-base-index 1 configs would mistarget if
	// we used the raw index field.
	panes, err := tc.PanesInOrder(ctx, session)
	if err != nil {
		log.Warn("workspace.backfill.list-panes-failed",
			"session", session, "err", err.Error())
		return nil
	}
	if len(panes) != 3 {
		// Race — pane count changed between PaneCount and PanesInOrder.
		// Bail out conservatively.
		log.Warn("workspace.backfill.race-detected",
			"session", session, "pane_count", len(panes))
		return nil
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
			return nil
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
	return nil
}
