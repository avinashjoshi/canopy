package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/config"
)

// withFakeCanopyHome redirects HOME so settings writes land in t.TempDir()
// instead of clobbering the user's real ~/.canopy/config.json.
func withFakeCanopyHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Unsetenv("CANOPY_SOURCE_ROOT")
	canopyHome := filepath.Join(home, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return canopyHome
}

// TestOpenSettingsForm seeds the input with the current effective
// source-root so users see what they're replacing.
func TestOpenSettingsForm(t *testing.T) {
	canopyHome := withFakeCanopyHome(t)
	// Pre-seed config with a known value so we can assert the seed.
	store, err := config.NewUserStore(canopyHome)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	if err := store.Save(&config.UserConfig{SourceRoot: "/home/avi/Work"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	m, _ := newAddProjectTestModel(t)
	cmd := m.openSettingsForm()
	if m.mode != settingsFormMode {
		t.Errorf("mode = %v, want settingsFormMode", m.mode)
	}
	if m.addProjectInput.Value() != "/home/avi/Work" {
		t.Errorf("input seed = %q, want /home/avi/Work", m.addProjectInput.Value())
	}
	if !m.addProjectInput.Focused() {
		t.Error("input not focused")
	}
	if cmd == nil {
		t.Error("openSettingsForm returned nil cmd; want blink")
	}
}

// TestSettingsForm_SaveOnEnter writes the new value to config.json.
func TestSettingsForm_SaveOnEnter(t *testing.T) {
	canopyHome := withFakeCanopyHome(t)
	m, _ := newAddProjectTestModel(t)
	m.openSettingsForm()
	m.addProjectInput.SetValue("/new/source/root")
	model, _ := m.handleSettingsFormKey(tea.KeyMsg{Type: tea.KeyEnter})
	sm := model.(*Model)
	if sm.mode != listMode {
		t.Errorf("after save: mode = %v, want listMode", sm.mode)
	}

	// Verify config.json now contains the new value.
	store, err := config.NewUserStore(canopyHome)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	c, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SourceRoot != "/new/source/root" {
		t.Errorf("SourceRoot = %q, want /new/source/root", c.SourceRoot)
	}
}

// TestSettingsForm_EmptyValueClears: blank input acts as "unset" —
// SourceRoot becomes empty so the next get falls back to default/env.
func TestSettingsForm_EmptyValueClears(t *testing.T) {
	canopyHome := withFakeCanopyHome(t)
	// Pre-seed so we have something to clear.
	store, _ := config.NewUserStore(canopyHome)
	store.Save(&config.UserConfig{SourceRoot: "/old/value"})

	m, _ := newAddProjectTestModel(t)
	m.openSettingsForm()
	m.addProjectInput.SetValue("")
	m.handleSettingsFormKey(tea.KeyMsg{Type: tea.KeyEnter})

	store2, _ := config.NewUserStore(canopyHome)
	c, _ := store2.Load()
	if c.SourceRoot != "" {
		t.Errorf("SourceRoot = %q, want empty after clear", c.SourceRoot)
	}
}

// TestSettingsForm_Esc dismisses without saving.
func TestSettingsForm_Esc(t *testing.T) {
	canopyHome := withFakeCanopyHome(t)
	store, _ := config.NewUserStore(canopyHome)
	store.Save(&config.UserConfig{SourceRoot: "/original"})

	m, _ := newAddProjectTestModel(t)
	m.openSettingsForm()
	m.addProjectInput.SetValue("/should-not-stick")
	model, _ := m.handleSettingsFormKey(tea.KeyMsg{Type: tea.KeyEsc})
	sm := model.(*Model)
	if sm.mode != listMode {
		t.Errorf("after esc: mode = %v, want listMode", sm.mode)
	}

	store2, _ := config.NewUserStore(canopyHome)
	c, _ := store2.Load()
	if c.SourceRoot != "/original" {
		t.Errorf("SourceRoot = %q, want /original (esc must not save)", c.SourceRoot)
	}
}

// TestSettingsForm_ViewRenders checks the settings modal title +
// help legend are present.
func TestSettingsForm_ViewRenders(t *testing.T) {
	withFakeCanopyHome(t)
	m, _ := newAddProjectTestModel(t)
	m.openSettingsForm()
	out := m.renderSettingsForm()
	if !strings.Contains(out, "Settings") {
		t.Error("view missing 'Settings' title")
	}
	if !strings.Contains(out, "Source root") {
		t.Error("view missing 'Source root' label")
	}
	if !strings.Contains(out, "enter: save") {
		t.Error("view missing legend")
	}
}
