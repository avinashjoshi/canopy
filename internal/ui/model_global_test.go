package ui

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/settings"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

// TestNewGlobal_Constructs: NewGlobal returns a non-nil GlobalModel
// whose Init produces a refresh cmd. Smoke test that the wiring is sane.
func TestNewGlobal_Constructs(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	gm := NewGlobal(store, tmux.New(), settings.Default())
	if gm == nil {
		t.Fatal("NewGlobal returned nil")
	}
	if cmd := gm.Init(); cmd == nil {
		t.Errorf("Init returned nil cmd; expected refresh dispatch")
	}
}

// TestGlobalModel_RendersWithoutPanic: View on a fresh GlobalModel (no
// rows loaded yet) renders an empty-state without crashing.
func TestGlobalModel_RendersWithoutPanic(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New(), settings.Default())

	out := gm.View()
	if !strings.Contains(out, "canopy") {
		t.Errorf("View missing title: %q", out)
	}
}

// TestGlobalModel_HelpToggle: ? shows help; any next key dismisses.
func TestGlobalModel_HelpToggle(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New(), settings.Default())

	model, _ := gm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	gm = model.(*GlobalModel)
	if !gm.showHelp {
		t.Errorf("? should set showHelp=true")
	}
	if !strings.Contains(gm.View(), "keybindings") {
		t.Errorf("help overlay missing 'keybindings' header: %q", gm.View())
	}
	// Any key dismisses.
	model, _ = gm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	gm = model.(*GlobalModel)
	if gm.showHelp {
		t.Errorf("any key should dismiss help")
	}
}

// TestGlobalModel_QuitKey: q returns tea.Quit.
func TestGlobalModel_QuitKey(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New(), settings.Default())

	_, cmd := gm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q produced nil cmd; expected tea.Quit")
	}
	// Calling the cmd produces a tea.QuitMsg.
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("q cmd produced %T, want tea.QuitMsg", msg)
	}
}

// TestGlobalModel_ActivateOnStoppedSurfacesHint: pressing enter on a
// stopped row pipes a non-fatal error to the list's banner — no quit,
// no attach attempt. Verifies the v0.5 "global mode is read-only for
// stopped/broken/orphaned" boundary.
func TestGlobalModel_ActivateOnStoppedSurfacesHint(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New(), settings.Default())

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "soft-fox",
		Status:      state.StatusStopped,
		TmuxSession: "cravd-soft-fox",
	}
	cmd := gm.activate(row)
	if cmd == nil {
		t.Fatal("activate returned nil cmd")
	}
	// Run the cmd to extract the message.
	msg := cmd()
	gotErr, ok := msg.(globalErrMsg)
	if !ok {
		t.Fatalf("activate(stopped) produced %T, want globalErrMsg", msg)
	}
	if !strings.Contains(gotErr.err.Error(), "stopped") {
		t.Errorf("hint missing 'stopped': %v", gotErr.err)
	}
	// Hint now points at the 'o' keybind (open the project) instead of
	// telling the user to cd manually.
	if !strings.Contains(gotErr.err.Error(), "`o`") {
		t.Errorf("hint missing 'o' keybind reference: %v", gotErr.err)
	}
}

// TestGlobalModel_GoToProject_NonEmptyRoot: row with a populated
// ProjectRoot produces a non-nil cmd (tea.ExecProcess against the canopy
// binary). Smoke check that the dispatch happens.
func TestGlobalModel_GoToProject_NonEmptyRoot(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New(), settings.Default())

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/home/avi/Work/cravd",
		Name:        "soft-fox",
		Status:      state.StatusReady,
	}
	cmd := gm.goToProject(row)
	if cmd == nil {
		t.Errorf("goToProject(populated row) returned nil cmd")
	}
}

// TestGlobalModel_GoToProject_NoFallbackAvailable: a v1-unmigrated row
// with no canonical ProjectRoot AND no worktree Path (e.g. a main-only
// row whose siblings also have no Path) surfaces a clear migration hint.
func TestGlobalModel_GoToProject_NoFallbackAvailable(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New(), settings.Default())

	// Unmigrated row: ProjectRoot is just a basename, no Path. No siblings.
	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "cravd", // basename, not absolute
		IsMain:      true,
		Name:        "(main)",
		Status:      "main",
	}
	cmd := gm.goToProject(row)
	if cmd == nil {
		t.Fatal("expected error cmd, got nil")
	}
	msg := cmd()
	gotErr, ok := msg.(globalErrMsg)
	if !ok {
		t.Fatalf("expected globalErrMsg, got %T", msg)
	}
	if !strings.Contains(gotErr.err.Error(), "no canonical root path on file") {
		t.Errorf("error message should explain the migration path: %v", gotErr.err)
	}
}

// TestGlobalModel_GoToProject_DerivesFromWorktreePath: when ProjectRoot is
// just a basename but the row has a worktree Path inside a real git repo,
// resolveProjectRoot derives the source repo via `git rev-parse
// --git-common-dir`. Verifies the v1-unmigrated escape hatch works without
// a registry.
func TestGlobalModel_GoToProject_DerivesFromWorktreePath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// Create a fake source repo + worktree under it.
	source := t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=main", source},
		{"-C", source, "config", "user.email", "t@e"},
		{"-C", source, "config", "user.name", "t"},
		{"-C", source, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	worktree := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", source, "worktree", "add", worktree).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}

	row := state.GlobalRow{
		Project:     filepath.Base(source),
		ProjectRoot: filepath.Base(source), // unmigrated basename
		Name:        "wt",
		Status:      state.StatusReady,
		Path:        worktree,
	}

	got, err := resolveProjectRoot(row, []state.GlobalRow{row})
	if err != nil {
		t.Fatalf("resolveProjectRoot: %v", err)
	}
	// EvalSymlinks may differ from the raw t.TempDir() on macOS (/var vs
	// /private/var); compare basenames as the cheap correctness signal.
	if filepath.Base(got) != filepath.Base(source) {
		t.Errorf("resolved root basename = %q, want %q", filepath.Base(got), filepath.Base(source))
	}
}

// TestGlobalModel_CloseOut_FlagOff_FallsBackToHint: with auto_close_shipped
// disabled (the default), pressing enter on a shipped row produces the
// existing hint flow — a globalErrMsg pointing at `canopy rm <name>`.
// Regression check that the opt-in flag really is opt-in.
func TestGlobalModel_CloseOut_FlagOff_FallsBackToHint(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New(), settings.Default())

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "soft-fox",
		Status:      state.StatusReady,
	}
	cmd := gm.closeOut(row)
	if cmd == nil {
		t.Fatal("closeOut returned nil cmd")
	}
	msg := cmd()
	got, ok := msg.(globalErrMsg)
	if !ok {
		t.Fatalf("closeOut(off) produced %T, want globalErrMsg", msg)
	}
	if !strings.Contains(got.err.Error(), "canopy rm soft-fox") {
		t.Errorf("hint missing canonical command: %v", got.err)
	}
	if gm.closeOutTarget != "" {
		t.Errorf("countdown should not start when feature off; target=%q", gm.closeOutTarget)
	}
}

// TestGlobalModel_CloseOut_FlagOn_StartsCountdown: with the flag on,
// closeOut populates countdown state and returns a tick cmd. The View
// should then render the countdown banner. We don't run the tick to
// completion (5 real-time seconds + a subprocess) — that's covered by
// the cancel test below.
func TestGlobalModel_CloseOut_FlagOn_StartsCountdown(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	s := settings.Default()
	s.Lifecycle.AutoCloseShipped = true
	gm := NewGlobal(store, tmux.New(), s)

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd", // absolute → resolveProjectRoot succeeds
		Name:        "soft-fox",
		Status:      state.StatusReady,
	}
	cmd := gm.closeOut(row)
	if cmd == nil {
		t.Fatal("closeOut returned nil cmd")
	}
	if gm.closeOutTarget != "soft-fox" {
		t.Errorf("closeOutTarget = %q; want soft-fox", gm.closeOutTarget)
	}
	if gm.closeOutRemaining != closeOutCountdownSeconds {
		t.Errorf("remaining = %d; want %d", gm.closeOutRemaining, closeOutCountdownSeconds)
	}
	if gm.closeOutClosing {
		t.Errorf("closing should be false at start; got true")
	}
	view := gm.View()
	if !strings.Contains(view, "Closing \"soft-fox\"") {
		t.Errorf("View missing countdown banner: %q", view)
	}
}

// TestGlobalModel_CloseOut_AnyKeyCancelsDuringCountdown: a keypress
// while the countdown is active aborts the auto-rm. Subprocess never
// runs; banner clears; the list's error banner gets a "cancelled" hint
// so the user sees their cancel landed.
func TestGlobalModel_CloseOut_AnyKeyCancelsDuringCountdown(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	s := settings.Default()
	s.Lifecycle.AutoCloseShipped = true
	gm := NewGlobal(store, tmux.New(), s)

	// Manually enter countdown state (closeOut would also work but
	// requires a row resolve; this is more direct).
	gm.closeOutTarget = "soft-fox"
	gm.closeOutProject = "/a/cravd"
	gm.closeOutRemaining = 3

	model, cmd := gm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	gm = model.(*GlobalModel)
	if cmd != nil {
		t.Errorf("cancel keypress should not produce a follow-up cmd; got %T", cmd)
	}
	if gm.closeOutTarget != "" {
		t.Errorf("target should be cleared on cancel; got %q", gm.closeOutTarget)
	}
	if gm.closeOutRemaining != 0 {
		t.Errorf("remaining should be cleared on cancel; got %d", gm.closeOutRemaining)
	}
}

// TestGlobalModel_CloseOut_TickDecrementsAndFires: each tick decrements
// the counter; a tick that hits zero flips to the closing state and
// emits the rm subprocess cmd. We assert the state machine transitions,
// not the actual subprocess (which would cd into a non-existent root
// and exec the canopy binary — too coupled for a unit test).
func TestGlobalModel_CloseOut_TickDecrementsAndFires(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	s := settings.Default()
	s.Lifecycle.AutoCloseShipped = true
	gm := NewGlobal(store, tmux.New(), s)

	gm.closeOutTarget = "soft-fox"
	gm.closeOutProject = "/a/cravd"
	gm.closeOutRemaining = 2

	// First tick: 2 → 1, returns another tick cmd.
	model, cmd := gm.Update(closeOutTickMsg{})
	gm = model.(*GlobalModel)
	if gm.closeOutRemaining != 1 {
		t.Errorf("after first tick: remaining = %d; want 1", gm.closeOutRemaining)
	}
	if cmd == nil {
		t.Errorf("expected next-tick cmd, got nil")
	}
	if gm.closeOutClosing {
		t.Errorf("should not be closing yet; got true")
	}

	// Second tick: 1 → 0, fires rm subprocess.
	model, cmd = gm.Update(closeOutTickMsg{})
	gm = model.(*GlobalModel)
	if gm.closeOutRemaining != 0 {
		t.Errorf("after second tick: remaining = %d; want 0", gm.closeOutRemaining)
	}
	if !gm.closeOutClosing {
		t.Errorf("should be closing after counter hits zero")
	}
	if cmd == nil {
		t.Errorf("expected rm subprocess cmd, got nil")
	}
}

// TestGlobalModel_CloseOut_DoneClearsState: when the rm subprocess
// returns, the model exits close-out mode. Success clears everything;
// failure preserves the error so the View can show it.
func TestGlobalModel_CloseOut_DoneClearsState(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New(), settings.Default())

	gm.closeOutTarget = "soft-fox"
	gm.closeOutClosing = true

	// Failure path: error preserved, target cleared, refresh kicked off.
	model, cmd := gm.Update(closeOutDoneMsg{
		target: "soft-fox",
		err:    fmt.Errorf("rm failed: exit 1"),
		output: "stderr line",
	})
	gm = model.(*GlobalModel)
	if gm.closeOutTarget != "" {
		t.Errorf("target should be cleared after done; got %q", gm.closeOutTarget)
	}
	if gm.closeOutErr == nil {
		t.Errorf("closeOutErr should be set on failure")
	}
	if !strings.Contains(gm.closeOutErr.Error(), "stderr line") {
		t.Errorf("closeOutErr missing captured output: %v", gm.closeOutErr)
	}
	if cmd == nil {
		t.Errorf("expected refresh cmd after done")
	}

	// Success path: error cleared too.
	gm.closeOutTarget = "soft-fox"
	gm.closeOutClosing = true
	gm.closeOutErr = nil
	model, _ = gm.Update(closeOutDoneMsg{target: "soft-fox", err: nil})
	gm = model.(*GlobalModel)
	if gm.closeOutErr != nil {
		t.Errorf("closeOutErr should be nil on success; got %v", gm.closeOutErr)
	}
}

// TestGlobalModel_CloseOut_StaleTickIgnored: a tick that arrives after
// the countdown was cancelled (or while a subprocess is in flight) must
// not double-fire the rm. Without this guard, a fast cancel-then-press
// could leak ticks across countdowns.
func TestGlobalModel_CloseOut_StaleTickIgnored(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New(), settings.Default())

	// No target set → tick should be a no-op (no remaining decrement,
	// no follow-up cmd).
	model, cmd := gm.Update(closeOutTickMsg{})
	gm = model.(*GlobalModel)
	if cmd != nil {
		t.Errorf("stale tick produced a cmd; got %T", cmd)
	}
	if gm.closeOutRemaining != 0 {
		t.Errorf("stale tick mutated remaining: %d", gm.closeOutRemaining)
	}

	// Already-closing tick: same — should not fire a second subprocess.
	gm.closeOutTarget = "soft-fox"
	gm.closeOutClosing = true
	model, cmd = gm.Update(closeOutTickMsg{})
	gm = model.(*GlobalModel)
	if cmd != nil {
		t.Errorf("tick during closing produced a cmd; got %T", cmd)
	}
}

// TestGlobalModel_CloseOut_FlagOn_UnresolvableRowSurfacesError: a
// shipped row whose ProjectRoot is unmigrated and has no Path falls
// back to a clear error instead of starting a countdown against a
// bogus working directory.
func TestGlobalModel_CloseOut_FlagOn_UnresolvableRowSurfacesError(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	s := settings.Default()
	s.Lifecycle.AutoCloseShipped = true
	gm := NewGlobal(store, tmux.New(), s)

	// Unmigrated row: ProjectRoot is just a basename, no Path.
	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "cravd",
		Name:        "soft-fox",
	}
	cmd := gm.closeOut(row)
	if cmd == nil {
		t.Fatal("expected error cmd, got nil")
	}
	msg := cmd()
	if _, ok := msg.(globalErrMsg); !ok {
		t.Fatalf("expected globalErrMsg, got %T", msg)
	}
	if gm.closeOutTarget != "" {
		t.Errorf("countdown should not start on unresolved row; target=%q", gm.closeOutTarget)
	}
}

// TestGlobalModel_ActivateOnReadyAttempts: pressing enter on a ready row
// returns a non-nil cmd (tea.ExecProcess). We don't actually run the
// attach in tests — that would launch tmux — but the cmd shape is the
// signal that the dispatch happened.
func TestGlobalModel_ActivateOnReadyAttempts(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	// Use a test tmux client so we don't touch the user's tmux server.
	gm := NewGlobal(store, tmux.WithSocket("canopy-test"), settings.Default())

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "soft-fox",
		Status:      state.StatusReady,
		TmuxSession: "cravd-soft-fox",
	}
	cmd := gm.activate(row)
	if cmd == nil {
		t.Errorf("activate(ready) returned nil cmd; expected tea.ExecProcess")
	}
}
