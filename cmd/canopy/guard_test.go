package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestEnforceNoNestedTmux drives the four corners of the guard:
// no TMUX -> allow; TMUX set -> refuse; TMUX + escape-hatch env ->
// allow; TMUX + per-cmd annotation -> allow.
func TestEnforceNoNestedTmux(t *testing.T) {
	t.Run("no TMUX env -> allow", func(t *testing.T) {
		t.Setenv("TMUX", "")
		t.Setenv(envAllowNested, "")
		t.Setenv("CANOPY_WORKSPACE_PATH", "")
		if err := enforceNoNestedTmux(&cobra.Command{Use: "ls"}); err != nil {
			t.Errorf("expected nil; got %v", err)
		}
	})

	t.Run("TMUX set -> refuse", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
		t.Setenv(envAllowNested, "")
		t.Setenv("CANOPY_WORKSPACE_PATH", "")
		err := enforceNoNestedTmux(&cobra.Command{Use: "new"})
		if err == nil {
			t.Fatal("expected error; got nil")
		}
		if !strings.Contains(err.Error(), "tmux") {
			t.Errorf("error should mention tmux: %v", err)
		}
		// Plain-tmux phrasing — the leading "refuses to run inside a
		// tmux session" line, not the workspace-specific variant.
		if !strings.Contains(err.Error(), "refuses to run inside a tmux session") {
			t.Errorf("expected ambient-tmux phrasing in opening line: %v", err)
		}
	})

	t.Run("TMUX + CANOPY_WORKSPACE_PATH -> refuse with workspace phrasing", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
		t.Setenv(envAllowNested, "")
		t.Setenv("CANOPY_WORKSPACE_PATH", "/home/x/.canopy/workspaces/foo/bar")
		err := enforceNoNestedTmux(&cobra.Command{Use: "new"})
		if err == nil {
			t.Fatal("expected error; got nil")
		}
		if !strings.Contains(err.Error(), "refuses to run inside a canopy workspace") {
			t.Errorf("expected canopy-workspace phrasing in opening line; got %v", err)
		}
	})

	t.Run("escape hatch env -> allow", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
		t.Setenv(envAllowNested, "1")
		if err := enforceNoNestedTmux(&cobra.Command{Use: "new"}); err != nil {
			t.Errorf("escape hatch should bypass guard; got %v", err)
		}
	})

	t.Run("per-command annotation -> allow", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
		t.Setenv(envAllowNested, "")
		cmd := &cobra.Command{
			Use:         "version",
			Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		}
		if err := enforceNoNestedTmux(cmd); err != nil {
			t.Errorf("annotation should bypass guard; got %v", err)
		}
	})

	t.Run("nil cmd -> still safe", func(t *testing.T) {
		t.Setenv("TMUX", "")
		if err := enforceNoNestedTmux(nil); err != nil {
			t.Errorf("nil cmd outside tmux should not error; got %v", err)
		}
		t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
		t.Setenv(envAllowNested, "")
		if err := enforceNoNestedTmux(nil); err == nil {
			t.Error("nil cmd inside tmux should error (no annotation to opt out)")
		}
	})
}
