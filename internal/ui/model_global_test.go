package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
	gm := NewGlobal(store, tmux.New())
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
	gm := NewGlobal(store, tmux.New())

	out := gm.View()
	if !strings.Contains(out, "canopy") {
		t.Errorf("View missing title: %q", out)
	}
}

// TestGlobalModel_HelpToggle: ? shows help; any next key dismisses.
func TestGlobalModel_HelpToggle(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

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
	gm := NewGlobal(store, tmux.New())

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
	gm := NewGlobal(store, tmux.New())

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
	if !strings.Contains(gotErr.err.Error(), "/a/cravd") {
		t.Errorf("hint missing project path: %v", gotErr.err)
	}
}

// TestGlobalModel_ActivateOnReadyAttempts: pressing enter on a ready row
// returns a non-nil cmd (tea.ExecProcess). We don't actually run the
// attach in tests — that would launch tmux — but the cmd shape is the
// signal that the dispatch happened.
func TestGlobalModel_ActivateOnReadyAttempts(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	// Use a test tmux client so we don't touch the user's tmux server.
	gm := NewGlobal(store, tmux.WithSocket("canopy-test"))

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
