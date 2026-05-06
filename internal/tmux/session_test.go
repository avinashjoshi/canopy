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

// testSocket is this package's named tmux socket. tmux's server-per-socket
// model means this isolates the test world from the user's real tmux server.
//
// Per-PACKAGE socket name is load-bearing: `go test ./...` runs packages in
// parallel by default. If internal/tmux and internal/workspace shared one
// socket, their TestMain teardowns would kill each other's sessions mid-run
// ("server exited unexpectedly" failures, intermittent on busy machines).
// Each package uses its own socket so they can't trample each other; within
// a package tests run sequentially (no t.Parallel calls) so a single socket
// is fine.
const testSocket = "canopy-test-tmux"

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
// cleanup hook that kills the entire tmux server on the test socket
// AND reaps any pane-tree descendants left behind. KillServerAndReap
// (rather than the bare KillServer) is required because `nvim --embed`
// detaches from its tmux pane on launch so it can outlive the launcher —
// killing the tmux server alone leaves it parented to PID 1, accumulating
// across test runs. See KillServerAndReap docs.
func newClient(t *testing.T) *tmux.Client {
	t.Helper()
	c := tmux.WithSocket(testSocket)
	t.Cleanup(func() {
		_ = c.KillServerAndReap(context.Background())
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
	out, err := exec.Command("tmux", "-L", testSocket,"list-panes", "-t", name).Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 4 {
		t.Errorf("pane count = %d; want 4 (got: %s)", len(lines), out)
	}
}

// TestSelectPaneDirection confirms direction-relative pane selection
// works after a horizontal split — the active pane should move from
// pane 0 (left) to the new right-side pane.
func TestSelectPaneDirection(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "select-pane-dir-test"
	cwd := t.TempDir()

	if err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// -d keeps active on pane 0; the new pane is to the right.
	if err := c.SplitPane(ctx, name, cwd, "", tmux.SplitHorizontal); err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if err := c.SelectPaneDirection(ctx, name, "R"); err != nil {
		t.Fatalf("SelectPaneDirection R: %v", err)
	}
	out, err := exec.Command("tmux", "-L", testSocket,"display-message", "-p", "-t", name, "#{pane_left}").Output()
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	left := strings.TrimSpace(string(out))
	if left == "0" {
		t.Errorf("after select-pane R, active pane_left = 0 (still on left pane); want non-zero (right pane)")
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

// TestSafeName covers the "tmux session name sanitizer" — stricter than
// git.Sanitize because tmux can't tolerate dots or colons in names
// (target syntax uses them as separators). Real-world cases include
// project dirs like "avi.tools" or temp dirs from mktemp ("tmp.X").
func TestSafeName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"v1.2.3", "v1-2-3"},
		{"avi.tools", "avi-tools"},
		{"tmp.X-feat", "tmp-X-feat"},
		{"feature/oauth", "feature-oauth"},
		{"feature: bug", "feature-bug"},
		{"a..b", "a-b"}, // run of dots collapses
		{"-leading", "leading"},
		{"trailing-", "trailing"},
		{"", ""},
		{"underscore_kept", "underscore_kept"},
		{"JIRA-1234", "JIRA-1234"}, // case preserved
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := tmux.SafeName(tc.in); got != tc.want {
				t.Errorf("SafeName(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
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

func TestRename_HappyPath(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	if err := c.Create(ctx, "rename-old", cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.Rename(ctx, "rename-old", "rename-new", "new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	// Old name should be gone, new name alive.
	if has, _ := c.HasSession(ctx, "rename-old"); has {
		t.Errorf("old session still exists after rename")
	}
	if has, _ := c.HasSession(ctx, "rename-new"); !has {
		t.Errorf("new session missing after rename")
	}
}

func TestRename_NotFound(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	err := c.Rename(context.Background(), "does-not-exist", "whatever", "")
	if !errors.Is(err, tmux.ErrSessionNotFound) {
		t.Fatalf("Rename(missing): got %v; want ErrSessionNotFound", err)
	}
}

func TestRename_Collision(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	if err := c.Create(ctx, "rename-a", cwd, ""); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if err := c.Create(ctx, "rename-b", cwd, ""); err != nil {
		t.Fatalf("Create b: %v", err)
	}
	err := c.Rename(ctx, "rename-a", "rename-b", "")
	if !errors.Is(err, tmux.ErrSessionNameInUse) {
		t.Fatalf("Rename(collision): got %v; want ErrSessionNameInUse", err)
	}
}

func TestRename_Identity(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	if err := c.Create(ctx, "rename-id", cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.Rename(ctx, "rename-id", "rename-id", ""); err != nil {
		t.Fatalf("Rename(self->self): got %v; want nil (no-op success)", err)
	}
}
