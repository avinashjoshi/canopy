package tmux_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/tmux"
)

// TestSetSessionEnv_HappyPath verifies a set/read round-trip: create a
// session, set CANOPY_REMOTE_HOST on it, read it back via
// `tmux show-environment -t <session>`, and confirm the stored value.
//
// This is the integration end the statusline relies on: when the remote
// canopy switch sets CANOPY_REMOTE_HOST per session, statusline
// subprocesses spawned by the tmux server for that session must inherit
// the value.
func TestSetSessionEnv_HappyPath(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "setenv-happy"
	cwd := t.TempDir()

	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := c.SetSessionEnv(ctx, name, "CANOPY_REMOTE_HOST", "tower"); err != nil {
		t.Fatalf("SetSessionEnv: %v", err)
	}

	got, err := exec.Command("tmux", "-L", testSocket, "show-environment", "-t", name, "CANOPY_REMOTE_HOST").Output()
	if err != nil {
		t.Fatalf("show-environment: %v", err)
	}
	want := "CANOPY_REMOTE_HOST=tower"
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("show-environment: got %q; want %q", strings.TrimSpace(string(got)), want)
	}
}

// TestSetSessionEnv_MissingSession verifies the "session doesn't exist"
// path returns nil, not an error. The caller's intent is conditional
// tagging — missing session is a no-op, not a failure mode worth
// propagating.
func TestSetSessionEnv_MissingSession(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()

	// Don't create any session. SetSessionEnv against a name that
	// doesn't exist should swallow tmux's exit-1 and return nil.
	err := c.SetSessionEnv(ctx, "does-not-exist", "CANOPY_REMOTE_HOST", "tower")
	if err != nil {
		t.Errorf("SetSessionEnv on missing session: got err=%v; want nil", err)
	}
}

// TestSetSessionEnv_EmptyInputs guards the input-validation branches: empty
// session or empty key returns a clear error. Empty value is a legitimate
// "clear the var" call to tmux and stays allowed.
func TestSetSessionEnv_EmptyInputs(t *testing.T) {
	c := tmux.WithSocket(testSocket)
	ctx := context.Background()
	if err := c.SetSessionEnv(ctx, "", "KEY", "value"); err == nil {
		t.Errorf("empty session: want error; got nil")
	}
	if err := c.SetSessionEnv(ctx, "session", "", "value"); err == nil {
		t.Errorf("empty key: want error; got nil")
	}
}

// TestUnsetSessionEnv_RoundTrip is the stale-tag fix in one assertion:
// set CANOPY_REMOTE_HOST on a session (simulating a prior remote attach),
// then unset it (simulating a later local attach), and confirm
// `show-environment` reports the key as cleared (tmux's `-NAME` form).
// Without this path, statusline subprocesses would inherit the stale
// value forever, falsely rendering a remote pill on a local session.
func TestUnsetSessionEnv_RoundTrip(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	name := "unset-roundtrip"
	cwd := t.TempDir()

	if _, err := c.Create(ctx, name, cwd, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.SetSessionEnv(ctx, name, "CANOPY_REMOTE_HOST", "tower"); err != nil {
		t.Fatalf("SetSessionEnv: %v", err)
	}
	// Sanity: it's set.
	got, _ := exec.Command("tmux", "-L", testSocket, "show-environment", "-t", name, "CANOPY_REMOTE_HOST").Output()
	if strings.TrimSpace(string(got)) != "CANOPY_REMOTE_HOST=tower" {
		t.Fatalf("pre-unset: got %q; want %q", strings.TrimSpace(string(got)), "CANOPY_REMOTE_HOST=tower")
	}

	if err := c.UnsetSessionEnv(ctx, name, "CANOPY_REMOTE_HOST"); err != nil {
		t.Fatalf("UnsetSessionEnv: %v", err)
	}

	// After unset, querying the specific key exits non-zero with
	// "unknown variable: CANOPY_REMOTE_HOST" on stderr. That's tmux's
	// signal that the key is gone from the session env entirely.
	cmd := exec.Command("tmux", "-L", testSocket, "show-environment", "-t", name, "CANOPY_REMOTE_HOST")
	combined, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("post-unset: show-environment should fail but returned %q", string(combined))
	}
	if !strings.Contains(string(combined), "unknown variable") {
		t.Errorf("post-unset: stderr should say 'unknown variable'; got %q", string(combined))
	}
}

// TestUnsetSessionEnv_MissingSession mirrors SetSessionEnv's missing-session
// contract: tmux exit 1 → swallow, return nil. Caller's intent is
// "best-effort cleanup", not a hard requirement.
func TestUnsetSessionEnv_MissingSession(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	if err := c.UnsetSessionEnv(ctx, "does-not-exist", "CANOPY_REMOTE_HOST"); err != nil {
		t.Errorf("UnsetSessionEnv on missing session: got err=%v; want nil", err)
	}
}

// TestUnsetSessionEnv_EmptyInputs is the input-validation guard. Same
// shape as TestSetSessionEnv_EmptyInputs.
func TestUnsetSessionEnv_EmptyInputs(t *testing.T) {
	c := tmux.WithSocket(testSocket)
	ctx := context.Background()
	if err := c.UnsetSessionEnv(ctx, "", "KEY"); err == nil {
		t.Errorf("empty session: want error; got nil")
	}
	if err := c.UnsetSessionEnv(ctx, "session", ""); err == nil {
		t.Errorf("empty key: want error; got nil")
	}
}
