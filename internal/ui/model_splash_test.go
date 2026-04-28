package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestNewInitSplash_Constructs: NewInitSplash returns a non-nil Model
// with the cwd set, didInit false.
func TestNewInitSplash_Constructs(t *testing.T) {
	m := NewInitSplash("/tmp/foo")
	if m == nil {
		t.Fatal("NewInitSplash returned nil")
	}
	if m.cwd != "/tmp/foo" {
		t.Errorf("cwd = %q, want /tmp/foo", m.cwd)
	}
	if m.didInit {
		t.Errorf("didInit should start false")
	}
}

// TestInitSplash_View_ContainsCwd: cwd is shown so the user can verify
// they're about to init the right directory.
func TestInitSplash_View_ContainsCwd(t *testing.T) {
	m := NewInitSplash("/home/avi/Work/myrepo")
	out := m.View()
	if !strings.Contains(out, "/home/avi/Work/myrepo") {
		t.Errorf("View missing cwd: %q", out)
	}
	if !strings.Contains(out, "canopy init") {
		t.Errorf("View missing 'canopy init' prompt: %q", out)
	}
}

// TestInitSplash_IKey_SetsDidInitAndQuits: pressing 'i' sets didInit
// and returns tea.Quit so the caller can run init synchronously after.
func TestInitSplash_IKey_SetsDidInitAndQuits(t *testing.T) {
	m := NewInitSplash("/tmp/foo")
	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	sm := model.(*InitSplashModel)
	if !sm.didInit {
		t.Errorf("'i' should set didInit=true")
	}
	if cmd == nil {
		t.Fatal("'i' should return tea.Quit cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("'i' cmd produced %T, want tea.QuitMsg", cmd())
	}
}

// TestInitSplash_QKey_QuitsWithoutInit: 'q' quits but leaves didInit false.
func TestInitSplash_QKey_QuitsWithoutInit(t *testing.T) {
	m := NewInitSplash("/tmp/foo")
	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	sm := model.(*InitSplashModel)
	if sm.didInit {
		t.Errorf("'q' should leave didInit=false")
	}
	if cmd == nil {
		t.Fatal("'q' should return tea.Quit cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("'q' cmd produced %T, want tea.QuitMsg", cmd())
	}
}

// TestInitSplash_StrayKey_NoOp: pressing a random key (not in the keymap)
// is a no-op — splash is intentionally explicit, no accidental dismissal.
func TestInitSplash_StrayKey_NoOp(t *testing.T) {
	m := NewInitSplash("/tmp/foo")
	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	sm := model.(*InitSplashModel)
	if sm.didInit {
		t.Errorf("stray key set didInit; should be no-op")
	}
	if cmd != nil {
		t.Errorf("stray key produced cmd; should be nil")
	}
}

// TestInitSplash_CapitalI: 'I' (capital) also opts into init. Cheap
// usability win — users mash shift sometimes.
func TestInitSplash_CapitalI(t *testing.T) {
	m := NewInitSplash("/tmp/foo")
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("I")})
	if !model.(*InitSplashModel).didInit {
		t.Errorf("'I' (capital) should set didInit=true")
	}
}
