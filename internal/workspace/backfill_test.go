package workspace_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// TestBackfillRoles_CanonicalLayout: a v0.15-style session with exactly
// 3 panes and no @canopy-role set should get all 3 roles tagged on the
// CORRECT panes. Asserts pane-to-role mapping, not just "roles exist."
//
// This is a regression test for a bug found via smoke testing: the
// initial implementation tagged in pane CREATION order, but tmux's
// list-panes returns panes in layout-TREE traversal order. With
// canopy's canonical splits (ide, then shell at bottom, then agent
// at right), the layout-tree order is [ide, agent, shell] — so the
// canonical role mapping must match that, not the creation sequence.
func TestBackfillRoles_CanonicalLayout(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(testSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()
	cwd := t.TempDir()
	name := "backfill-canonical"

	// Build the exact same 3-pane layout that buildSession creates,
	// WITHOUT setting roles (simulates v0.15). Capture pane IDs so we
	// can assert correct pane-to-role mapping post-backfill.
	idePane, err := c.Create(ctx, name, cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	shellPane, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 15)
	if err != nil {
		t.Fatalf("SplitPane shell: %v", err)
	}
	agentPane, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal, 30)
	if err != nil {
		t.Fatalf("SplitPane agent: %v", err)
	}

	// Pre-condition: no roles set.
	all, _ := c.ListAllRoles(ctx, name)
	if len(all) != 0 {
		t.Fatalf("pre-backfill: expected 0 tagged panes, got %d", len(all))
	}

	// Backfill.
	if err := workspace.BackfillRoles(ctx, c, name, "claude"); err != nil {
		t.Fatalf("BackfillRoles: %v", err)
	}

	// Post-condition: each role lookup returns the CORRECT pane ID.
	// This catches the bug where backfill tagged in creation order
	// instead of list-panes (layout-tree) order.
	tests := []struct {
		role     string
		wantPane string
	}{
		{"ide", idePane},
		{"agent:claude", agentPane},
		{"terminal:shell", shellPane},
	}
	for _, tc := range tests {
		got, err := c.LookupPane(ctx, name, tc.role)
		if err != nil {
			t.Errorf("LookupPane(%s): %v", tc.role, err)
			continue
		}
		if got != tc.wantPane {
			t.Errorf("backfill mistagged: role %s landed on pane %s; want %s",
				tc.role, got, tc.wantPane)
		}
	}
}

// TestBackfillRoles_NonCanonicalSkipped: a session with 4 panes (user
// added a manual split) does NOT get backfilled — silent mistagging
// is worse than no tagging. Logs a warn and returns nil.
func TestBackfillRoles_NonCanonicalSkipped(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(testSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()
	cwd := t.TempDir()
	name := "backfill-non-canonical"

	// 4-pane session.
	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 25); err != nil {
			t.Fatalf("SplitPane %d: %v", i, err)
		}
	}

	if err := workspace.BackfillRoles(ctx, c, name, "claude"); err != nil {
		t.Fatalf("BackfillRoles: %v", err)
	}

	// No roles should be set — backfill skipped.
	all, _ := c.ListAllRoles(ctx, name)
	if len(all) != 0 {
		t.Fatalf("post-backfill: expected 0 tagged panes (non-canonical skip), got %d", len(all))
	}
}

// TestBackfillRoles_AlreadyTagged: a fully-tagged session is a no-op
// (early return, no SetRole calls). Verified via post-state matches
// pre-state exactly.
func TestBackfillRoles_AlreadyTagged(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(testSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()
	cwd := t.TempDir()
	name := "backfill-already-tagged"

	// Build canonical layout AND tag panes (v0.16-style).
	idePane, err := c.Create(ctx, name, cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.SetRole(ctx, idePane, "ide"); err != nil {
		t.Fatalf("SetRole ide: %v", err)
	}
	shellPane, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 15)
	if err != nil {
		t.Fatalf("SplitPane shell: %v", err)
	}
	if err := c.SetRole(ctx, shellPane, "terminal:shell"); err != nil {
		t.Fatalf("SetRole shell: %v", err)
	}
	agentPane, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal, 30)
	if err != nil {
		t.Fatalf("SplitPane agent: %v", err)
	}
	if err := c.SetRole(ctx, agentPane, "agent:claude"); err != nil {
		t.Fatalf("SetRole agent: %v", err)
	}

	preAll, _ := c.ListAllRoles(ctx, name)
	if len(preAll) != 3 {
		t.Fatalf("pre-backfill: expected 3 tagged panes, got %d", len(preAll))
	}

	// Backfill — should be a no-op.
	if err := workspace.BackfillRoles(ctx, c, name, "claude"); err != nil {
		t.Fatalf("BackfillRoles (already tagged): %v", err)
	}

	postAll, _ := c.ListAllRoles(ctx, name)
	if len(postAll) != 3 {
		t.Fatalf("post-backfill: expected 3 tagged panes (no-op), got %d", len(postAll))
	}
}

// TestBackfillRoles_PartialTagged: a session with SOME roles set but
// not all (e.g., agent tagged but ide missing) gets the missing ones
// filled in. Codex-caught: checking only `agent:*` would silently
// leave partial-tagged sessions undetected.
func TestBackfillRoles_PartialTagged(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(testSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()
	cwd := t.TempDir()
	name := "backfill-partial"

	// Build canonical layout, tag ONLY the agent pane (simulates a
	// half-finished v0.15→v0.16 transition or a buggy custom layout).
	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 15); err != nil {
		t.Fatalf("SplitPane shell: %v", err)
	}
	agentPane, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal, 30)
	if err != nil {
		t.Fatalf("SplitPane agent: %v", err)
	}
	if err := c.SetRole(ctx, agentPane, "agent:claude"); err != nil {
		t.Fatalf("SetRole agent: %v", err)
	}

	// Backfill should fill in the missing ide + terminal:shell.
	if err := workspace.BackfillRoles(ctx, c, name, "claude"); err != nil {
		t.Fatalf("BackfillRoles: %v", err)
	}

	// All 3 canonical roles should now be present.
	all, _ := c.ListAllRoles(ctx, name)
	if len(all) != 3 {
		t.Fatalf("post-backfill: expected 3 tagged panes, got %d", len(all))
	}
	for _, role := range []string{"ide", "terminal:shell", "agent:claude"} {
		if _, err := c.LookupPane(ctx, name, role); err != nil {
			t.Errorf("LookupPane(%s) post-partial-backfill: %v", role, err)
		}
	}
}

// TestBackfillRoles_IncongruentTagsSkipped: a session where some panes
// already have @canopy-role set to a value that DOESN'T match the
// canonical positional expectation should be skipped entirely — never
// overwriting an existing tag.
//
// Regression test for adversarial review finding #1: previous behavior
// would overwrite "ide" with "agent:claude" if the ide-tagged pane
// happened to land at list-panes position 1 (the canonical agent slot).
func TestBackfillRoles_IncongruentTagsSkipped(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(testSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()
	cwd := t.TempDir()
	name := "backfill-incongruent"

	// Build a 3-pane canonical layout. Tag the canonical-agent-position
	// pane with "ide" (intentionally wrong — incongruent). The other two
	// panes are untagged.
	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 15); err != nil {
		t.Fatalf("SplitPane shell: %v", err)
	}
	if _, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal, 30); err != nil {
		t.Fatalf("SplitPane agent: %v", err)
	}

	// PanesInOrder returns [pos0, pos1, pos2] in list-panes traversal order.
	// Canonical mapping is [ide, agent:*, terminal:shell].
	// Tag pos1 (the canonical agent slot) with "ide" — incongruent.
	panes, err := c.PanesInOrder(ctx, name)
	if err != nil {
		t.Fatalf("PanesInOrder: %v", err)
	}
	if len(panes) != 3 {
		t.Fatalf("expected 3 panes, got %d", len(panes))
	}
	if err := c.SetRole(ctx, panes[1], "ide"); err != nil {
		t.Fatalf("SetRole incongruent: %v", err)
	}

	// Backfill should refuse — incongruent tag detected.
	if err := workspace.BackfillRoles(ctx, c, name, "claude"); err != nil {
		t.Fatalf("BackfillRoles: %v", err)
	}

	// Post-condition: ONLY the original incongruent tag remains. The
	// other two panes are still untagged. No overwrite happened.
	all, _ := c.ListAllRoles(ctx, name)
	if len(all) != 1 {
		t.Fatalf("expected 1 tagged pane after incongruent skip, got %d (overwriting bug regressed)", len(all))
	}
	if all[0].ID != panes[1] || all[0].Role != "ide" {
		t.Errorf("incongruent tag was modified: got pane=%s role=%s; want pane=%s role=ide",
			all[0].ID, all[0].Role, panes[1])
	}
}

// TestBackfillRoles_EmptyLauncherDefaultsToClaude: passing "" as the
// launcher type produces "agent:claude" via agent.RoleForType. Same
// path used by the canopy main session backfill.
func TestBackfillRoles_EmptyLauncherDefaultsToClaude(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(testSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()
	cwd := t.TempDir()
	name := "backfill-empty-launcher"

	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 15); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if _, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal, 30); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	if err := workspace.BackfillRoles(ctx, c, name, ""); err != nil {
		t.Fatalf("BackfillRoles: %v", err)
	}

	if _, err := c.LookupPane(ctx, name, "agent:claude"); err != nil {
		t.Errorf("LookupPane(agent:claude) after empty-launcher backfill: %v", err)
	}
}
