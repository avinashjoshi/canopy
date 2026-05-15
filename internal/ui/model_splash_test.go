package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestNewInitSplash_Constructs: NewInitSplash returns a non-nil Model
// with the cwd seeded into the input, no result yet.
func TestNewInitSplash_Constructs(t *testing.T) {
	m := NewInitSplash("/tmp/foo")
	if m == nil {
		t.Fatal("NewInitSplash returned nil")
	}
	if m.cwd != "/tmp/foo" {
		t.Errorf("cwd = %q, want /tmp/foo", m.cwd)
	}
	if m.input.Value() != "/tmp/foo" {
		t.Errorf("input prefill = %q, want /tmp/foo (back-compat: Enter on default inits cwd)", m.input.Value())
	}
}

// TestInitSplash_View_ContainsForm: the splash renders the form
// elements — title, prompt, input value. Replaces the pre-v0.18
// "press i" view assertion.
func TestInitSplash_View_ContainsForm(t *testing.T) {
	m := NewInitSplash("/home/avi/Work/myrepo")
	out := m.View()
	if !strings.Contains(out, "Add Project") {
		t.Errorf("View missing 'Add Project' title: %q", out)
	}
	if !strings.Contains(out, "/home/avi/Work/myrepo") {
		t.Errorf("View missing prefilled cwd: %q", out)
	}
	if !strings.Contains(out, "Folder path or git URL") {
		t.Errorf("View missing input prompt: %q", out)
	}
}

// TestInitSplash_EnterOnPrefilledCwd_SubmitsCwd preserves the pre-v0.18
// muscle memory: Enter on the default value (no editing) inits cwd.
// Decision #11 in the v0.18 design doc.
func TestInitSplash_EnterOnPrefilledCwd_SubmitsCwd(t *testing.T) {
	m := NewInitSplash("/tmp/foo")
	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	sm := model.(*InitSplashModel)
	if sm.result.Action != SplashSubmit {
		t.Errorf("Enter on default: action = %v, want SplashSubmit", sm.result.Action)
	}
	if sm.result.Arg != "/tmp/foo" {
		t.Errorf("Enter on default: arg = %q, want /tmp/foo", sm.result.Arg)
	}
	if cmd == nil {
		t.Fatal("Enter should return tea.Quit cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("Enter cmd produced %T, want tea.QuitMsg", cmd())
	}
}

// TestInitSplash_EnterAfterEdit_SubmitsTypedValue: editing the input
// then pressing Enter submits the new value (URL or other path).
func TestInitSplash_EnterAfterEdit_SubmitsTypedValue(t *testing.T) {
	m := NewInitSplash("/tmp/foo")
	// Replace the default value with a git URL.
	m.input.SetValue("https://github.com/foo/bar.git")
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	sm := model.(*InitSplashModel)
	if sm.result.Action != SplashSubmit {
		t.Errorf("action = %v, want SplashSubmit", sm.result.Action)
	}
	if sm.result.Arg != "https://github.com/foo/bar.git" {
		t.Errorf("arg = %q, want the typed URL", sm.result.Arg)
	}
}

// TestInitSplash_Esc_DismissesWithoutInit: esc dismisses with no init.
func TestInitSplash_Esc_DismissesWithoutInit(t *testing.T) {
	m := NewInitSplash("/tmp/foo")
	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	sm := model.(*InitSplashModel)
	if sm.result.Action != SplashDismiss {
		t.Errorf("esc: action = %v, want SplashDismiss", sm.result.Action)
	}
	if sm.result.Arg != "" {
		t.Errorf("esc: arg = %q, want empty (no submit)", sm.result.Arg)
	}
	if cmd == nil {
		t.Fatal("esc should return tea.Quit cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("esc cmd produced %T, want tea.QuitMsg", cmd())
	}
}

// TestInitSplash_TypingForwardsToInput: regular character keys forward
// to the textinput, mutating the value. This is how the user replaces
// the prefilled cwd with a URL or different path.
func TestInitSplash_TypingForwardsToInput(t *testing.T) {
	m := NewInitSplash("")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if !strings.Contains(m.input.Value(), "h") || !strings.Contains(m.input.Value(), "i") {
		t.Errorf("typing didn't reach input; value = %q", m.input.Value())
	}
}
