package tmux_test

import (
	"context"
	"testing"
)

// TestAttachedSessions_NoServer_ReturnsEmpty: before any session has
// been created on the test socket, tmux's `list-sessions` exits 1
// with "no server running". AttachedSessions must map this to (empty
// map, nil) so callers can do `if attached["foo"]` without checking
// for errors first.
func TestAttachedSessions_NoServer_ReturnsEmpty(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	got, err := c.AttachedSessions(context.Background())
	if err != nil {
		t.Fatalf("AttachedSessions on empty server returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("AttachedSessions on empty server = %v; want empty", got)
	}
}

// TestAttachedSessions_DetachedSession_NotInResult: a session that's
// running but has no client attached (the typical "background"
// state) must NOT appear in the result map. This is the visible
// distinction between Alive and Attached in the TUI.
func TestAttachedSessions_DetachedSession_NotInResult(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	if err := c.Create(ctx, "detached-test", cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := c.AttachedSessions(ctx)
	if err != nil {
		t.Fatalf("AttachedSessions: %v", err)
	}
	if got["detached-test"] {
		t.Errorf("detached session reported as attached: %v", got)
	}
}

// TestSessionAttached_Detached: SessionAttached on a known-detached
// session returns (false, nil). Attached requires a client; we don't
// spawn one in unit tests.
func TestSessionAttached_Detached(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	if err := c.Create(ctx, "single-detached", cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := c.SessionAttached(ctx, "single-detached")
	if err != nil {
		t.Fatalf("SessionAttached: %v", err)
	}
	if got {
		t.Errorf("SessionAttached on detached = true; want false")
	}
}

// TestSessionAttached_DeadSession: SessionAttached on a session that
// doesn't exist returns (false, nil) — same shape as HasSession's
// "doesn't exist" mapping. Callers don't need a sentinel check just
// for "is anyone attached to a thing that's not there."
func TestSessionAttached_DeadSession(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	got, err := c.SessionAttached(context.Background(), "never-existed")
	if err != nil {
		t.Errorf("SessionAttached on dead session returned error: %v", err)
	}
	if got {
		t.Error("SessionAttached on dead session = true; want false")
	}
}
