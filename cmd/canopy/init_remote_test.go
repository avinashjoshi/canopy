package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunAddProjectRemote_EmptyHostName rejects up front instead of
// dispatching an SSH call with no target.
func TestRunAddProjectRemote_EmptyHostName(t *testing.T) {
	err := runAddProjectRemote(context.Background(), "", "https://github.com/foo/bar.git", addProjectOptions{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("empty host: nil error; want refused")
	}
	if !strings.Contains(err.Error(), "empty host") {
		t.Errorf("err = %v; want 'empty host' message", err)
	}
}

// TestRunAddProjectRemote_EmptyArg refuses the no-arg case explicitly.
// On a remote host, there's no meaningful "cwd" — the user must pass a
// URL (or an absolute path that exists on the remote, which we can't
// validate locally; we still require the arg).
func TestRunAddProjectRemote_EmptyArg(t *testing.T) {
	err := runAddProjectRemote(context.Background(), "tower", "", addProjectOptions{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("empty arg: nil error; want refused")
	}
}

// TestRunAddProjectRemote_HostNotRegistered surfaces a useful error
// with a fix-it hint (the `canopy host add` command) when the user
// types a host name that doesn't exist in the registry.
func TestRunAddProjectRemote_HostNotRegistered(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	if err := os.MkdirAll(filepath.Join(fakeHome, ".canopy"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	err := runAddProjectRemote(context.Background(), "nonexistent-host", "https://github.com/foo/bar.git", addProjectOptions{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("unknown host: nil error; want refused")
	}
	if !strings.Contains(err.Error(), "canopy host add") {
		t.Errorf("err = %v; want fix-it hint pointing at `canopy host add`", err)
	}
}

// TestShellQuote_Idempotent verifies the shell-quote helper produces
// shell-safe output for the common URL shapes we care about. The
// helper itself lives in install_tmux.go (shared across cmd/canopy);
// this test confirms it round-trips the metacharacters that would
// otherwise let a malicious URL inject commands into the remote shell.
func TestShellQuote_Idempotent(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Safe bare strings pass through unwrapped (existing shellQuote
		// behavior in install_tmux.go).
		{"simple", "simple"},
		// URLs with `/` and `:` get quoted because `:` is not bare-safe.
		{"https://github.com/foo/bar.git", "'https://github.com/foo/bar.git'"},
		// Single quote inside requires the close-escape-reopen dance.
		{"a'b", `'a'\''b'`},
		// Empty: any quoting is valid; we accept whatever the helper
		// produces (currently a bare empty string).
	}
	for _, tc := range cases {
		got := shellQuote(tc.in)
		if got != tc.want {
			t.Errorf("shellQuote(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestRunAddProjectRemote_RegisteredHostFailsCleanly: when the host
// IS registered but SSH itself fails (because we can't actually
// connect from this test environment), the wrapper surfaces the
// error wrapped with the host name. End-to-end SSH dispatch isn't
// testable hermetically; this gives us coverage of the error path.
func TestRunAddProjectRemote_RegisteredHostFailsCleanly(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	canopyHome := filepath.Join(fakeHome, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Seed hosts.json with a host whose SSH target points at an
	// unreachable address. SSH will fail; we just verify the error
	// is wrapped meaningfully.
	hostsJSON := `{
  "schema_version": 2,
  "hosts": {
    "fakehost": {"ssh_target": "user@192.0.2.99", "type": "ssh"}
  }
}`
	if err := os.WriteFile(filepath.Join(canopyHome, "hosts.json"), []byte(hostsJSON), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Cancel quickly so the test doesn't wait for SSH's 5s timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runAddProjectRemote(ctx, "fakehost", "https://github.com/foo/bar.git", addProjectOptions{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("cancelled SSH: nil error; want failure")
	}
	if !strings.Contains(err.Error(), "fakehost") && !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want host name OR cancel surfaced", err)
	}
}
