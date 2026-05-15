package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// newAddProjectTestModel builds a minimal *Model wired up for the
// Add Project form. RunInitFunc is captured into a per-test channel
// so assertions can verify it was (or wasn't) called.
func newAddProjectTestModel(t *testing.T) (*Model, *testInitRecorder) {
	t.Helper()
	rec := &testInitRecorder{}
	m := &Model{
		nameInput:       textinput.New(),
		listInput:       textinput.New(),
		targetInput:     textinput.New(),
		addProjectInput: textinput.New(),
		mode:            listMode,
		tab:             tabGlobal,
		RunInitFunc:     rec.runInit,
	}
	return m, rec
}

// testInitRecorder captures RunInitFunc invocations so tests can
// assert "did the form actually call runInit on the expected path?"
// without needing real disk state.
type testInitRecorder struct {
	calls []testInitCall
}

type testInitCall struct {
	absPath     string
	withScripts bool
	force       bool
}

func (r *testInitRecorder) runInit(absPath string, withScripts, force bool) error {
	r.calls = append(r.calls, testInitCall{absPath: absPath, withScripts: withScripts, force: force})
	return nil
}

// TestOpenAddProjectForm_NilCallback: when RunInitFunc isn't wired
// (e.g. an old call site that hasn't been updated), opening the
// form surfaces an error rather than entering an unrecoverable state.
func TestOpenAddProjectForm_NilCallback(t *testing.T) {
	m := &Model{
		nameInput:       textinput.New(),
		listInput:       textinput.New(),
		targetInput:     textinput.New(),
		addProjectInput: textinput.New(),
		mode:            listMode,
	}
	cmd := m.openAddProjectForm()
	if cmd != nil {
		t.Errorf("nil RunInitFunc: openAddProjectForm returned a Cmd; want nil")
	}
	if m.mode == addProjectFormMode {
		t.Errorf("nil RunInitFunc: mode advanced to form; want unchanged")
	}
	if m.err == nil {
		t.Error("nil RunInitFunc: expected m.err to be set")
	}
}

// TestOpenAddProjectForm_HappyPath: with a wired callback, the form
// opens cleanly: mode advances, input is reset and focused, the
// blink Cmd is returned.
func TestOpenAddProjectForm_HappyPath(t *testing.T) {
	m, _ := newAddProjectTestModel(t)
	cmd := m.openAddProjectForm()
	if m.mode != addProjectFormMode {
		t.Errorf("mode = %v, want addProjectFormMode", m.mode)
	}
	if m.addProjectInput.Value() != "" {
		t.Errorf("input not reset: %q", m.addProjectInput.Value())
	}
	if !m.addProjectInput.Focused() {
		t.Error("input not focused")
	}
	if cmd == nil {
		t.Error("openAddProjectForm returned nil cmd; want blink")
	}
}

// TestHandleAddProjectFormKey_Esc closes the form back to listMode.
func TestHandleAddProjectFormKey_Esc(t *testing.T) {
	m, _ := newAddProjectTestModel(t)
	m.openAddProjectForm()
	model, _ := m.handleAddProjectFormKey(tea.KeyMsg{Type: tea.KeyEsc})
	sm := model.(*Model)
	if sm.mode != listMode {
		t.Errorf("after esc: mode = %v, want listMode", sm.mode)
	}
}

// TestSubmitAddProject_EmptyInput surfaces an inline error
// (decision #11: Global tab disables empty-Enter).
func TestSubmitAddProject_EmptyInput(t *testing.T) {
	m, rec := newAddProjectTestModel(t)
	m.openAddProjectForm()
	m.submitAddProject()
	if m.addProjectError == "" {
		t.Error("empty Enter: no error rendered")
	}
	if !strings.Contains(m.addProjectError, "Type a path or URL") {
		t.Errorf("error = %q; want 'Type a path or URL'", m.addProjectError)
	}
	if len(rec.calls) != 0 {
		t.Errorf("empty Enter called RunInitFunc %d times; want 0", len(rec.calls))
	}
}

// TestSubmitAddProject_LocalPath calls RunInitFunc with the abs path.
// Uses t.TempDir() so the path actually exists and Stat succeeds.
func TestSubmitAddProject_LocalPath(t *testing.T) {
	m, rec := newAddProjectTestModel(t)
	m.openAddProjectForm()
	dir := t.TempDir()
	m.addProjectInput.SetValue(dir)
	m.submitAddProject()
	if len(rec.calls) != 1 {
		t.Fatalf("RunInitFunc called %d times; want 1", len(rec.calls))
	}
	if rec.calls[0].absPath != dir {
		t.Errorf("absPath = %q; want %q", rec.calls[0].absPath, dir)
	}
}

// TestSubmitAddProject_PathMissing surfaces the Stat error inline
// without calling RunInitFunc.
func TestSubmitAddProject_PathMissing(t *testing.T) {
	m, rec := newAddProjectTestModel(t)
	m.openAddProjectForm()
	m.addProjectInput.SetValue("/tmp/this-path-should-not-exist-canopy-test-XYZ")
	m.submitAddProject()
	if m.addProjectError == "" {
		t.Error("missing path: no error rendered")
	}
	if len(rec.calls) != 0 {
		t.Errorf("missing path called RunInitFunc %d times; want 0", len(rec.calls))
	}
}

// TestErrorClearsOnNextKeystroke: after an error renders, the next
// non-Enter key should clear it (the user is fixing their input).
func TestErrorClearsOnNextKeystroke(t *testing.T) {
	m, _ := newAddProjectTestModel(t)
	m.openAddProjectForm()
	// Trigger an error first.
	m.submitAddProject()
	if m.addProjectError == "" {
		t.Fatal("setup: no error to clear")
	}
	// Now press a regular key.
	m.handleAddProjectFormKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	if m.addProjectError != "" {
		t.Errorf("error not cleared after keystroke: %q", m.addProjectError)
	}
}

// TestActionAddProject_Available gates the keybind on RunInitFunc
// being wired AND not on the Hosts tab.
func TestActionAddProject_Available(t *testing.T) {
	m, _ := newAddProjectTestModel(t)
	if !availableAddProject(m) {
		t.Error("availableAddProject: should be true with wired RunInitFunc on Global tab")
	}
	m.tab = tabHosts
	if availableAddProject(m) {
		t.Error("availableAddProject: should be false on Hosts tab")
	}
	m.tab = tabGlobal
	m.RunInitFunc = nil
	if availableAddProject(m) {
		t.Error("availableAddProject: should be false when RunInitFunc is nil")
	}
}

// TestAddProjectFormView_RendersStates checks each footer state:
// default legend, error, toast. Tied to the design doc's decisions
// #14, #15, #19.
func TestAddProjectFormView_RendersStates(t *testing.T) {
	m, _ := newAddProjectTestModel(t)
	m.width = 80
	m.openAddProjectForm()

	// Default state: legend with "ctrl+s: change source".
	if !strings.Contains(m.renderAddProjectForm(), "ctrl+s") {
		t.Error("default state: missing ctrl+s in footer")
	}
	// Error state: errorStyle line with ✗.
	m.addProjectError = "✗ test error"
	if !strings.Contains(m.renderAddProjectForm(), "✗ test error") {
		t.Error("error state: error message not rendered")
	}
	// Toast state: readyStyle line with ✓.
	m.addProjectError = ""
	m.addProjectToast = "✓ Added bar at /home/avi/Work/bar"
	if !strings.Contains(m.renderAddProjectForm(), "✓ Added bar") {
		t.Error("toast state: toast not rendered")
	}
}
