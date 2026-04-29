package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestNewPopupInnerCmd_isHidden: popup-inner is a delegation target,
// not a user-facing command. It must not appear in `canopy --help`.
func TestNewPopupInnerCmd_isHidden(t *testing.T) {
	cmd := newPopupInnerCmd()
	if !cmd.Hidden {
		t.Error("popup-inner: Hidden=false; expected true (internal subcommand)")
	}
}

// TestPopupCommands_carryAllowInTmuxAnnotation: popup, popup-inner, and
// statusline all run from inside tmux. Without the allow-in-tmux annotation
// the nested-tmux guard refuses the invocation and the user-facing flow
// breaks (popup never opens, statusline shows error in tmux bar).
func TestPopupCommands_carryAllowInTmuxAnnotation(t *testing.T) {
	cases := []struct {
		name string
		cmd  func() *cobra.Command
	}{
		{"popup", func() *cobra.Command { return newPopupCmd() }},
		{"popup-inner", func() *cobra.Command { return newPopupInnerCmd() }},
		{"statusline", func() *cobra.Command { return newStatuslineCmd() }},
		{"install-tmux", func() *cobra.Command { return newInstallTmuxCmd() }},
		{"run", func() *cobra.Command { return newRunCmd() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cmd()
			got, ok := c.Annotations[allowInTmuxAnnotation]
			if !ok {
				t.Fatalf("%s: missing allow-in-tmux annotation; nested-tmux guard will refuse", tc.name)
			}
			if got != "true" {
				t.Errorf("%s: allow-in-tmux=%q; want \"true\" (guard.go:44 only honors \"true\")",
					tc.name, got)
			}
		})
	}
}
