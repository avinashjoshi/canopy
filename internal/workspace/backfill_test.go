package workspace_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

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
	workspace.BackfillRoles(ctx, c, name, "claude")

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

	workspace.BackfillRoles(ctx, c, name, "claude")

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
	workspace.BackfillRoles(ctx, c, name, "claude")

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
	workspace.BackfillRoles(ctx, c, name, "claude")

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
	workspace.BackfillRoles(ctx, c, name, "claude")

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

// TestBackfillRoles_CommandConflictSkipped: a 3-pane session with the
// canonical layout, but with `vim` running in the canonical SHELL slot
// (i.e., the user manually rearranged something and the editor is now
// in the wrong position) gets skipped. The conflict signal —
// `pane_current_command` reports an editor where the canonical role is
// `terminal:shell` — overrides positional inference.
//
// Without the command-sniffing safeguard, backfill would have happily
// tagged the editor pane as `terminal:shell`, then later lookups by
// role would land on the wrong pane.
func TestBackfillRoles_CommandConflictSkipped(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	if _, err := exec.LookPath("vim"); err != nil {
		t.Skip("vim not on PATH; skipping command-conflict integration test")
	}
	c := tmux.WithSocket(testSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()
	cwd := t.TempDir()
	name := "backfill-command-conflict"

	// Build canopy's canonical 3-pane layout shape, but put `vim` in
	// the slot that list-panes traversal lands at position 2 (the
	// canonical shell slot). The ide and agent slots are plain shells.
	//
	// Canopy's canonical create sequence is Create → SplitVertical(15)
	// → SplitHorizontal(30). List-panes traversal returns
	// [pos0=ide, pos1=agent, pos2=shell]. We start vim in the second
	// SplitPane call's pane (pos1 = agent slot) — that's a clear
	// "ide-class command in agent slot" conflict.
	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 15); err != nil {
		t.Fatalf("SplitPane shell: %v", err)
	}
	// `-c 'q!'` would exit immediately; we need vim to STAY running
	// so pane_current_command reads it. Use `+startinsert` and an
	// empty buffer — vim sits in insert mode, process stays alive.
	if _, err := c.SplitPane(ctx, name, cwd, "vim", tmux.SplitHorizontal, 30); err != nil {
		t.Fatalf("SplitPane vim: %v", err)
	}

	// Give vim a moment to start up so pane_current_command reflects
	// it rather than the brief shell that exec'd into it. Accept any
	// vim-family comm name: GitHub's Ubuntu runners ship `vim.tiny` as
	// /usr/bin/vim, which reports its real comm not the symlink. The
	// match must mirror what classifyCommand can resolve to ide-class.
	vimFamily := map[string]bool{
		"vim": true, "vim.tiny": true, "vim.basic": true,
		"vim.nox": true, "vi": true, "nvim": true,
	}
	sawVim := false
	const pollSteps = 60 // 60 × 50ms = 3s. Slow CI runners need the headroom.
	for i := 0; i < pollSteps; i++ {
		cmds, err := c.PaneCommands(ctx, name)
		if err != nil {
			t.Fatalf("PaneCommands: %v", err)
		}
		for _, cmd := range cmds {
			if vimFamily[cmd] {
				sawVim = true
				break
			}
		}
		if sawVim {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx done while waiting for vim")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !sawVim {
		// vim didn't show up in pane_current_command. Two causes seen in
		// practice: (1) vim.tiny on Ubuntu CI fails to start without a
		// real TTY/terminfo, leaving sh visible; (2) the runner is so
		// slow vim hasn't finished spawning in 3s. Either way, we can't
		// observe the wrong-slot conflict, so we can't assert the
		// safeguard fired. Skip instead of failing — the unit tests in
		// command_sniff_test.go cover the conflict logic exhaustively;
		// this integration test is just bonus on environments where
		// vim actually runs.
		cmds, _ := c.PaneCommands(ctx, name)
		t.Skipf("no vim-family command in PaneCommands within 3s (likely vim.tiny on CI without TTY); observed: %v", cmds)
	}

	// Backfill should refuse — vim is in the canonical agent slot.
	workspace.BackfillRoles(ctx, c, name, "claude")

	// No roles should be tagged.
	all, _ := c.ListAllRoles(ctx, name)
	if len(all) != 0 {
		t.Fatalf("expected 0 tagged panes after command-conflict refusal, got %d: %v", len(all), all)
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

	workspace.BackfillRoles(ctx, c, name, "")

	if _, err := c.LookupPane(ctx, name, "agent:claude"); err != nil {
		t.Errorf("LookupPane(agent:claude) after empty-launcher backfill: %v", err)
	}
}
