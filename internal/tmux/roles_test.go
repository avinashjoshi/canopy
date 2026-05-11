package tmux_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/tmux"
)

// TestRoles_RoundTrip is THE keystone test: create a session, tag 3
// panes by role, discard the in-process pane IDs (simulating a fresh
// canopy process attaching), then look each one back up by role. Proves
// the role contract works end-to-end with no in-process state needed.
//
// If this fails, no other roles test result is meaningful — the
// fundamental contract is broken.
func TestRoles_RoundTrip(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-roundtrip"
	cwd := t.TempDir()

	idePane, err := c.Create(ctx, name, cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.SetRole(ctx, idePane, "ide"); err != nil {
		t.Fatalf("SetRole(ide): %v", err)
	}

	shellPane, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 15)
	if err != nil {
		t.Fatalf("SplitPane shell: %v", err)
	}
	if err := c.SetRole(ctx, shellPane, "terminal:shell"); err != nil {
		t.Fatalf("SetRole(terminal:shell): %v", err)
	}

	agentPane, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal, 30)
	if err != nil {
		t.Fatalf("SplitPane agent: %v", err)
	}
	if err := c.SetRole(ctx, agentPane, "agent:claude"); err != nil {
		t.Fatalf("SetRole(agent:claude): %v", err)
	}

	// Discard the captured pane IDs by shadowing — recovery must not
	// depend on in-process state.
	idePane, shellPane, agentPane = "", "", ""
	_ = idePane // suppress "declared but not used" — they're discarded intentionally

	// Recover each by role. This is the proof.
	got, err := c.LookupPane(ctx, name, "ide")
	if err != nil {
		t.Fatalf("LookupPane(ide): %v", err)
	}
	if !strings.HasPrefix(got, "%") {
		t.Fatalf("LookupPane(ide) returned %q; expected a tmux pane ID like %%N", got)
	}

	if _, err := c.LookupPane(ctx, name, "terminal:shell"); err != nil {
		t.Fatalf("LookupPane(terminal:shell): %v", err)
	}

	// Glob lookup
	if _, err := c.LookupPane(ctx, name, "agent:*"); err != nil {
		t.Fatalf("LookupPane(agent:*): %v", err)
	}

	// Exact lookup of the agent role
	if _, err := c.LookupPane(ctx, name, "agent:claude"); err != nil {
		t.Fatalf("LookupPane(agent:claude): %v", err)
	}
}

// TestRoles_NotFound: looking up a role that doesn't exist returns
// ErrPaneNotFound (errors.Is unwrapping works).
func TestRoles_NotFound(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-notfound"
	cwd := t.TempDir()

	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := c.LookupPane(ctx, name, "agent:claude")
	if !errors.Is(err, tmux.ErrPaneNotFound) {
		t.Fatalf("LookupPane on empty session: got err %v; want ErrPaneNotFound", err)
	}
}

// TestRoles_LookupAllPanes_EmptySlice: zero matches returns empty slice
// + nil error (collection-API convention), NOT ErrPaneNotFound. The
// caller may legitimately want to know "no agents in this session yet"
// without it being an error.
func TestRoles_LookupAllPanes_EmptySlice(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-empty-slice"
	cwd := t.TempDir()

	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	matches, err := c.LookupAllPanes(ctx, name, "agent:*")
	if err != nil {
		t.Fatalf("LookupAllPanes: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("LookupAllPanes on empty session: got %d matches; want 0", len(matches))
	}
}

// TestRoles_PrefixGlob: agent:* matches agent:claude, agent:codex,
// etc. NOT agent (no colon) and NOT terminal:shell.
func TestRoles_PrefixGlob(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-glob"
	cwd := t.TempDir()

	p1, err := c.Create(ctx, name, cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.SetRole(ctx, p1, "agent:claude"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	p2, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 50)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if err := c.SetRole(ctx, p2, "terminal:shell"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	matches, err := c.LookupAllPanes(ctx, name, "agent:*")
	if err != nil {
		t.Fatalf("LookupAllPanes(agent:*): %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("LookupAllPanes(agent:*): got %d matches; want 1", len(matches))
	}
	if matches[0].Role != "agent:claude" {
		t.Fatalf("LookupAllPanes(agent:*)[0].Role = %q; want agent:claude", matches[0].Role)
	}
}

// TestRoles_MultiMatch_WarnAndReturnFirst: when 2 panes share the same
// role (which shouldn't happen with canopy's own builders but CAN happen
// with custom layouts), LookupPane returns the first by tmux's natural
// list-panes order.
//
// We can't easily assert the warn was logged without intercepting slog,
// so this test focuses on the "returns first, doesn't error" behavior.
func TestRoles_MultiMatch_WarnAndReturnFirst(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-multi"
	cwd := t.TempDir()

	p1, err := c.Create(ctx, name, cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.SetRole(ctx, p1, "terminal:shell"); err != nil {
		t.Fatalf("SetRole p1: %v", err)
	}
	p2, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 50)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if err := c.SetRole(ctx, p2, "terminal:shell"); err != nil {
		t.Fatalf("SetRole p2: %v", err)
	}

	got, err := c.LookupPane(ctx, name, "terminal:shell")
	if err != nil {
		t.Fatalf("LookupPane: %v", err)
	}
	// First match by tmux list-panes ORDER, which is the session's natural
	// order (typically pane creation order within the window). Should be p1.
	if got != p1 {
		t.Fatalf("LookupPane multi-match: got %s; want first match %s (p1)", got, p1)
	}
}

// TestRoles_SetRole_EmptyPaneID: defensive check for the bug class
// where Create/SplitPane would have returned an empty pane ID. Should
// fail fast rather than silently no-op or target the wrong pane.
func TestRoles_SetRole_EmptyPaneID(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()

	if err := c.SetRole(ctx, "", "ide"); err == nil {
		t.Fatal("SetRole with empty paneID should error, got nil")
	}
}

// TestRoles_SetRole_EmptyRole: defensive check.
func TestRoles_SetRole_EmptyRole(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-empty-role"
	cwd := t.TempDir()

	pane, err := c.Create(ctx, name, cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := c.SetRole(ctx, pane, ""); err == nil {
		t.Fatal("SetRole with empty role should error, got nil")
	}
}

// TestRoles_SelectPane: focuses a specific pane by ID.
func TestRoles_SelectPane(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-select"
	cwd := t.TempDir()

	p1, err := c.Create(ctx, name, cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p2, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 50)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	// SplitPane uses `-d`, so active pane is still p1. Select p2.
	if err := c.SelectPane(ctx, p2); err != nil {
		t.Fatalf("SelectPane(p2): %v", err)
	}
	// Re-select p1 (no-op-shaped — we just verify it doesn't error).
	if err := c.SelectPane(ctx, p1); err != nil {
		t.Fatalf("SelectPane(p1): %v", err)
	}

	// Empty pane ID errors.
	if err := c.SelectPane(ctx, ""); err == nil {
		t.Fatal("SelectPane with empty paneID should error, got nil")
	}
}

// TestRoles_PaneCount: verifies session-scoped pane counting.
func TestRoles_PaneCount(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-count"
	cwd := t.TempDir()

	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, err := c.PaneCount(ctx, name); err != nil || got != 1 {
		t.Fatalf("PaneCount after Create: got (%d, %v); want (1, nil)", got, err)
	}
	if _, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 50); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if got, err := c.PaneCount(ctx, name); err != nil || got != 2 {
		t.Fatalf("PaneCount after SplitPane: got (%d, %v); want (2, nil)", got, err)
	}
	if _, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal, 30); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if got, err := c.PaneCount(ctx, name); err != nil || got != 3 {
		t.Fatalf("PaneCount canonical 3-pane: got (%d, %v); want (3, nil)", got, err)
	}
}

// TestRoles_PanesInOrder: returns pane IDs in tmux list-panes natural
// order — used by the backfill heuristic.
//
// The backfill canonical-role mapping (in workspace.BackfillRoles) is
// load-bearing on this order being:
//   - stable across calls for the same layout
//   - matching the layout-tree depth-first traversal, not pane creation
//     order — for canopy's canonical splits, that's [ide, agent, shell]
//     even though we Create→SplitVertical(shell)→SplitHorizontal(agent).
//
// If either assertion regresses, BackfillRoles will mistag panes.
func TestRoles_PanesInOrder(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-order"
	cwd := t.TempDir()

	// Build canopy's canonical 3-pane layout.
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

	got, err := c.PanesInOrder(ctx, name)
	if err != nil {
		t.Fatalf("PanesInOrder: %v", err)
	}

	// Assertion 1: layout-tree order. The canonical canopy layout traverses
	// to [ide, agent, shell] — pane CREATION order was [ide, shell, agent].
	// BackfillRoles' canonicalRoles slice depends on this exact ordering.
	want := []string{idePane, agentPane, shellPane}
	if len(got) != len(want) {
		t.Fatalf("PanesInOrder: got %d panes; want %d (%v)", len(got), len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("PanesInOrder[%d] = %s; want %s\nfull order: got %v, want %v",
				i, got[i], want[i], got, want)
		}
	}

	// Assertion 2: stability. Calling PanesInOrder twice for the same
	// session must return identical slices; otherwise BackfillRoles'
	// canonical mapping can land on different panes between read and write.
	got2, err := c.PanesInOrder(ctx, name)
	if err != nil {
		t.Fatalf("PanesInOrder (second call): %v", err)
	}
	if len(got2) != len(got) {
		t.Fatalf("PanesInOrder unstable: first=%d panes, second=%d panes", len(got), len(got2))
	}
	for i := range got {
		if got[i] != got2[i] {
			t.Errorf("PanesInOrder unstable at index %d: first=%s, second=%s", i, got[i], got2[i])
		}
	}
}

// TestRoles_ListAllRoles: returns every (paneID, role) pair for tagged
// panes in the session.
func TestRoles_ListAllRoles(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-list-all"
	cwd := t.TempDir()

	p1, err := c.Create(ctx, name, cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.SetRole(ctx, p1, "ide"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	p2, err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical, 50)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	// p2 untagged on purpose — should NOT appear in ListAllRoles.
	_ = p2

	all, err := c.ListAllRoles(ctx, name)
	if err != nil {
		t.Fatalf("ListAllRoles: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("ListAllRoles: got %d tagged panes; want 1 (untagged p2 excluded)", len(all))
	}
	if all[0].Role != "ide" {
		t.Fatalf("ListAllRoles[0].Role = %q; want ide", all[0].Role)
	}
}

// TestWindowCount: returns 1 for a fresh canopy-shaped session. Used
// by the backfill window-count gate to reject multi-window layouts.
func TestWindowCount(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()
	name := "win-count"

	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, err := c.WindowCount(ctx, name); err != nil || got != 1 {
		t.Fatalf("WindowCount after Create: got (%d, %v); want (1, nil)", got, err)
	}
}

// TestHasClient: returns true only when a client is actually attached
// to the named session, false on missing-session or detached-session.
// Used by the backfill concurrent-attach guard.
func TestHasClient(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	// Missing session — `tmux list-clients -t <missing>` errors out;
	// HasClient should swallow it and return false.
	if got, err := c.HasClient(ctx, "no-such-session"); err != nil || got {
		t.Fatalf("HasClient(missing): got (%v, %v); want (false, nil)", got, err)
	}

	// Existing session, no clients attached (Create makes a detached
	// session via `-d`).
	if _, err := c.Create(ctx, "no-client-session", cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, err := c.HasClient(ctx, "no-client-session"); err != nil || got {
		t.Fatalf("HasClient(detached): got (%v, %v); want (false, nil)", got, err)
	}
}

// TestRoles_InvalidGlob: globs with `*` in any position other than a
// single trailing char are rejected up front with ErrInvalidGlob.
// Previously these silently matched nothing — looked like "no such pane"
// when the real cause was a malformed pattern.
func TestRoles_InvalidGlob(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "roles-invalid-glob"
	cwd := t.TempDir()

	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cases := []string{
		"*:claude",        // leading *
		"agent:*claude",   // middle *
		"agent:**",        // double trailing *
		"*agent*",         // both ends
	}
	for _, pattern := range cases {
		_, err := c.LookupAllPanes(ctx, name, pattern)
		if !errors.Is(err, tmux.ErrInvalidGlob) {
			t.Errorf("LookupAllPanes(%q): got err %v; want ErrInvalidGlob", pattern, err)
		}
		_, err = c.LookupPane(ctx, name, pattern)
		if !errors.Is(err, tmux.ErrInvalidGlob) {
			t.Errorf("LookupPane(%q): got err %v; want ErrInvalidGlob", pattern, err)
		}
	}

	// Sanity: well-formed patterns still go through (return
	// ErrPaneNotFound rather than ErrInvalidGlob on an empty session).
	for _, pattern := range []string{"ide", "agent:claude", "agent:*", "*"} {
		_, err := c.LookupAllPanes(ctx, name, pattern)
		if errors.Is(err, tmux.ErrInvalidGlob) {
			t.Errorf("LookupAllPanes(%q): rejected as invalid; want acceptance", pattern)
		}
	}
}

// TestRoles_NoCrossSession: verifies the -s flag scopes lookups to ONE
// session (not server-wide via -a). Two sessions with the same role
// must not contaminate each other's lookups.
func TestRoles_NoCrossSession(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	// Session A with agent:claude
	pa, err := c.Create(ctx, "session-a", cwd, "")
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	if err := c.SetRole(ctx, pa, "agent:claude"); err != nil {
		t.Fatalf("SetRole A: %v", err)
	}

	// Session B with no roles
	if _, err := c.Create(ctx, "session-b", cwd, ""); err != nil {
		t.Fatalf("Create B: %v", err)
	}

	// Lookup in B should return ErrPaneNotFound (NOT match A's pane).
	_, err = c.LookupPane(ctx, "session-b", "agent:claude")
	if !errors.Is(err, tmux.ErrPaneNotFound) {
		t.Fatalf("Cross-session contamination: LookupPane in B found A's pane (err=%v)", err)
	}
}
