package main

import (
	"context"
	"os"
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

// TestResolveTargetWorkspace_NoArgsOutsideTmux: no positional + no tmux
// produces an actionable error message that names the fix.
//
// Skipped when the test process is itself inside tmux — the dogfood
// loop runs the full suite inside a workspace pane, where TMUX is set
// and CurrentSession would succeed against a real session.
func TestResolveTargetWorkspace_NoArgsOutsideTmux(t *testing.T) {
	if os.Getenv("TMUX") != "" {
		t.Skip("running inside tmux; can't simulate the outside-tmux path here")
	}

	_, err := resolveTargetWorkspace(context.Background(), nil, nil)
	if err == nil {
		t.Fatalf("want error for no-args-outside-tmux; got nil")
	}
	if !strings.Contains(err.Error(), "not inside a tmux session") {
		t.Errorf("error lost the friendly direction: %v", err)
	}
}
