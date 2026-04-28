package tmux_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

// testSocket is the named tmux socket all tests share. tmux's server-per-socket
// model means this isolates the test world from the user's real tmux server —
// running these tests will not touch any of your real workspaces.
const testSocket = "canopy-test"

// TestMain initializes clog so package-level loggers don't shout JSON to
// stderr during the test run.
func TestMain(m *testing.M) {
	teardown, err := clog.Init(false)
	if err != nil {
		// Tests don't depend on logging working; carry on with stdlib default.
		_ = err
	}
	defer teardown()
	m.Run()
}

// requireTmux skips the test if tmux isn't on PATH. Most CI Linux runners
// have it; macOS dev machines without it should not fail the suite.
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
}

// newClient returns a Client scoped to the test socket and registers a
// cleanup hook that kills the entire tmux server on the test socket. That
// way no test leaves stale sessions around to confuse later tests.
func newClient(t *testing.T) *tmux.Client {
	t.Helper()
	c := tmux.WithSocket(testSocket)
	t.Cleanup(func() {
		_ = c.KillServer(context.Background())
	})
	return c
}

// TestSession_HappyPath is the load-bearing round-trip test: create a
// session, verify it's alive, kill it, verify it's gone. If this passes
// against a real tmux server, the basic shell-out shape works.
func TestSession_HappyPath(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "happy-path"
	cwd := t.TempDir()

	// Pre-condition: server doesn't exist yet, so HasSession returns false
	// (without erroring) thanks to our "exit code 1 = no session" mapping.
	if has, err := c.HasSession(ctx, name); err != nil || has {
		t.Fatalf("pre-create HasSession: got (%v, %v); want (false, nil)", has, err)
	}

	if err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if has, err := c.HasSession(ctx, name); err != nil || !has {
		t.Fatalf("post-create HasSession: got (%v, %v); want (true, nil)", has, err)
	}

	if err := c.Kill(ctx, name); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	if has, err := c.HasSession(ctx, name); err != nil || has {
		t.Fatalf("post-kill HasSession: got (%v, %v); want (false, nil)", has, err)
	}
}

// TestCreate_AlreadyExists covers the "Create twice" idempotency case. The
// workspace lifecycle relies on this to fast-fail when the user runs
// `canopy new` against a name that's already alive.
func TestCreate_AlreadyExists(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "already-exists"
	cwd := t.TempDir()

	if err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	err := c.Create(ctx, name, cwd, "")
	if !errors.Is(err, tmux.ErrSessionExists) {
		t.Fatalf("second Create: got %v; want errors.Is(... ErrSessionExists)", err)
	}
}

// TestSplitPane covers the four-pane build the workspace orchestrator
// performs. Start a session, split three times (each split targets the
// active pane, so we never reference indices). End with 4 panes.
func TestSplitPane(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "split-test"
	cwd := t.TempDir()

	if err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal); err != nil {
		t.Fatalf("SplitPane #1: %v", err)
	}
	if err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical); err != nil {
		t.Fatalf("SplitPane #2: %v", err)
	}
	if err := c.SplitPane(ctx, name, cwd, "", tmux.SplitVertical); err != nil {
		t.Fatalf("SplitPane #3: %v", err)
	}

	// Verify pane count via tmux list-panes.
	out, err := exec.Command("tmux", "-L", "canopy-test", "list-panes", "-t", name).Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 4 {
		t.Errorf("pane count = %d; want 4 (got: %s)", len(lines), out)
	}
}

// TestSelectLayout confirms the layout call returns nil for a known-good
// preset name.
func TestSelectLayout(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "layout-test"
	cwd := t.TempDir()

	if err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if err := c.SelectLayout(ctx, name, "tiled"); err != nil {
		t.Errorf("SelectLayout: %v", err)
	}
}

// TestKill_NotFound covers the "kill what isn't there" case. Reconciliation
// will hit this on every startup if a session was killed externally.
func TestKill_NotFound(t *testing.T) {
	requireTmux(t)
	c := newClient(t)

	err := c.Kill(context.Background(), "definitely-not-here")
	if !errors.Is(err, tmux.ErrSessionNotFound) {
		t.Fatalf("Kill(missing): got %v; want errors.Is(... ErrSessionNotFound)", err)
	}
}
