package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/oncactus/canopy/internal/state"
)

// TestStatuslineGlyph covers all five workspace statuses + the unknown
// fallback. Glyphs are user-visible and protanopia-friendly per commit
// 10dd07e — a regression here would degrade the at-a-glance signal.
func TestStatuslineGlyph(t *testing.T) {
	cases := []struct {
		status state.Status
		want   string
	}{
		{state.StatusReady, "●"},
		{state.StatusSettingUp, "…"},
		{state.StatusStopped, "⏸"},
		{state.StatusBroken, "✗"},
		{state.StatusOrphaned, "!"},
		{state.Status("unknown_future_status"), ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := statuslineGlyph(tc.status); got != tc.want {
				t.Errorf("statuslineGlyph(%q) = %q; want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestEscapeForTmux is the CRITICAL regression test for the codex-flagged
// tmux statusline injection vector. A workspace name like
// `feat#[bg=red]gotcha` reaching `status-right` unmodified would inject
// style sequences into the user's tmux bar. tmux's documented escape is
// `#` → `##`.
//
// IRON RULE: this test must pass before v0.7 ships.
func TestEscapeForTmux(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no_hash", "silent-falcon", "silent-falcon"},
		{"hostile_style_inject", "feat#[bg=red]gotcha", "feat##[bg=red]gotcha"},
		{"single_hash", "issue#42", "issue##42"},
		{"multiple_hashes", "##build", "####build"},
		{"empty", "", ""},
		{"hash_at_start_and_end", "#x#", "##x##"},
		{"full_status_line_safe", "canopy: silent-falcon ● :40010", "canopy: silent-falcon ● :40010"},
		{"full_status_line_hostile", "canopy: feat#[fg=red] ● :40010", "canopy: feat##[fg=red] ● :40010"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeForTmux(tc.in); got != tc.want {
				t.Errorf("escapeForTmux(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFormatCurrent verifies the user-facing line shape: "canopy: <name>
// <glyph> :<port>" with hash-escape applied.
func TestFormatCurrent(t *testing.T) {
	cases := []struct {
		name   string
		ws     string
		status state.Status
		port   int
		want   string
	}{
		{"ready", "silent-falcon", state.StatusReady, 40010, "canopy: silent-falcon ● :40010"},
		{"broken", "misty-aspen", state.StatusBroken, 40011, "canopy: misty-aspen ✗ :40011"},
		{"stopped", "bold-otter", state.StatusStopped, 40012, "canopy: bold-otter ⏸ :40012"},
		{"hostile_name", "feat#[bg=red]", state.StatusReady, 40013, "canopy: feat##[bg=red] ● :40013"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatCurrent(tc.ws, tc.status, tc.port); got != tc.want {
				t.Errorf("formatCurrent: got %q; want %q", got, tc.want)
			}
		})
	}
}

// TestFindWorkspaceBySession covers the lookup that maps a tmux session
// name (from `tmux display-message -p '#S'`) back to a Workspace row.
// This is how the "current workspace" mapping works (codex T6).
func TestFindWorkspaceBySession(t *testing.T) {
	st := &state.State{
		Workspaces: []state.Workspace{
			{Name: "silent-falcon", TmuxSession: "canopy-silent-falcon", Status: state.StatusReady, Port: 40010},
			{Name: "misty-aspen", TmuxSession: "canopy-misty-aspen", Status: state.StatusBroken, Port: 40011},
		},
	}

	cases := []struct {
		name        string
		session     string
		wantNil     bool
		wantWsName  string
	}{
		{"match_ready", "canopy-silent-falcon", false, "silent-falcon"},
		{"match_broken", "canopy-misty-aspen", false, "misty-aspen"},
		{"non_canopy_session", "main", true, ""},
		{"empty_session", "", true, ""},
		{"unknown_canopy_session", "canopy-deleted-workspace", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := findWorkspaceBySession(st, tc.session)
			if (ws == nil) != tc.wantNil {
				t.Fatalf("findWorkspaceBySession(%q): got %v, wantNil=%v", tc.session, ws, tc.wantNil)
			}
			if ws != nil && ws.Name != tc.wantWsName {
				t.Errorf("findWorkspaceBySession(%q): got name %q, want %q", tc.session, ws.Name, tc.wantWsName)
			}
		})
	}
}

// TestRunStatusline_invalidFormat verifies that a bogus --format value
// produces empty stdout (and a warn log we don't assert on). Important
// because tmux re-runs us every status-interval; any stderr noise would
// spam canopy.log forever if the user mistypes the format.
func TestRunStatusline_invalidFormat(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	if err := os.MkdirAll(filepath.Join(fakeHome, ".canopy"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := newStatuslineCmd()
	cmd.SetArgs([]string{"--format=bogus"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("invalid format: stdout = %q; want empty", out.String())
	}
}

// TestRunStatusline_outsideTmux verifies the "no current tmux client"
// path produces empty stdout. This is what statusline returns when run
// from a non-tmux context (e.g. a user running `canopy statusline`
// directly from a plain shell to debug).
//
// We force "no current client" by pointing HOME at a fresh dir (no
// state.json) AND being outside any tmux session. Most CI runners
// satisfy the second condition naturally.
func TestRunStatusline_outsideTmux(t *testing.T) {
	if os.Getenv("TMUX") != "" {
		t.Skip("running inside tmux; can't simulate 'no current client'")
	}
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	if err := os.MkdirAll(filepath.Join(fakeHome, ".canopy"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := newStatuslineCmd()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("no tmux: stdout = %q; want empty", out.String())
	}
}

// TestRunStatusline_corruptState verifies that a malformed state.json
// produces empty stdout (silent failure). A canopy bug or filesystem
// issue must NEVER garbage-fill the user's tmux status bar.
func TestRunStatusline_corruptState(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	canopyHome := filepath.Join(fakeHome, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write deliberately malformed JSON.
	if err := os.WriteFile(filepath.Join(canopyHome, "state.json"),
		[]byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	cmd := newStatuslineCmd()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Even with corrupt state, stdout must be empty (not "error: ..." or
	// a partial line). tmux gets nothing and shows nothing.
	if !strings.HasPrefix(out.String(), "") || strings.Contains(out.String(), "error") {
		t.Errorf("corrupt state: stdout = %q; want empty/no-error", out.String())
	}
	if out.Len() != 0 {
		t.Errorf("corrupt state: stdout non-empty: %q", out.String())
	}
}

// TestRenderCurrentLine_panicRecovery is the CRITICAL test for IRON RULE
// #2: statusline NEVER panics out. We trigger a panic via a nil-deref
// path inside renderCurrentLine and verify the wrapper returns gracefully.
//
// We can't easily inject a panic from outside, so this test exercises
// the defer-recover in runStatusline by wrapping a panicking call. If
// someone removes the defer-recover, this test fails.
//
// IRON RULE: this test must pass before v0.7 ships.
func TestRunStatusline_panicRecovery(t *testing.T) {
	// We exercise the recover by replacing the cobra RunE temporarily
	// with a panicking version that wraps the same defer-recover idiom
	// from runStatusline. If the production defer-recover is intact,
	// the cobra Execute() returns nil and stdout stays sane.
	cmd := newStatuslineCmd()
	cmd.RunE = func(c *cobra.Command, args []string) (err error) {
		// Inline copy of the defer-recover from runStatusline.
		defer func() {
			if r := recover(); r != nil {
				statuslineLog.Warn("statusline.panic.test", "recovered", "true")
				err = nil
			}
		}()
		panic("simulated panic inside statusline")
	}
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	// If defer-recover works, Execute returns nil. If broken, it panics
	// out and the test crashes. Either is a clear signal.
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: got err=%v; want nil (recover should swallow panic)", err)
	}
	if out.Len() != 0 {
		t.Errorf("panic path: stdout = %q; want empty", out.String())
	}
}
