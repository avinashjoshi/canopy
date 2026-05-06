package main

import (
	"context"
	"strings"
	"testing"
)

// TestResolveTargetWorkspace_ExplicitArg: with a positional arg, the
// resolver returns it directly and never consults tmux. Works equally
// from inside or outside a session — the simplest and most-used path.
func TestResolveTargetWorkspace_ExplicitArg(t *testing.T) {
	got, err := resolveTargetWorkspace(context.Background(), nil, []string{"my-ws"})
	if err != nil {
		t.Fatalf("explicit arg should not error: %v", err)
	}
	if got != "my-ws" {
		t.Errorf("got %q; want my-ws", got)
	}
}

// TestResolveTargetWorkspace_NoArgsOutsideTmux: no positional arg + nil
// manager produces an actionable error message that names the fix
// (`pass the workspace name explicitly`). The nil-manager path is also
// the first guard in resolveTargetWorkspace so this test doubles as a
// nil-safety regression check — it caught a panic in CI when the
// function refactor moved mgr.List above the tmux check.
func TestResolveTargetWorkspace_NoArgsOutsideTmux(t *testing.T) {
	_, err := resolveTargetWorkspace(context.Background(), nil, nil)
	if err == nil {
		t.Fatalf("want error for no-args-nil-manager; got nil")
	}
	if !strings.Contains(err.Error(), "pass the workspace name explicitly") {
		t.Errorf("error lost the friendly direction: %v", err)
	}
}
