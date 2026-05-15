package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fixtureRows builds a three-row test set: release (built, active),
// one workspace with a built binary, one missing-binary workspace.
// Most picker tests start from this fixture; tests that need different
// shapes build their own.
func fixtureRows() []UseRow {
	return []UseRow{
		{Target: "release", Branch: "—", Version: "v9.9.9", Built: "built 2h ago",
			BinaryPath: "/bin/canopy.bin", IsRelease: true, HasBinary: true, Active: true},
		{Target: "feature-foo", Branch: "—", Version: "DEV", Built: "built 5m ago",
			BinaryPath: "/ws/foo/canopy", IsRelease: false, HasBinary: true, Active: false},
		{Target: "wip-bar", Branch: "wip-bar", Version: "DEV", Built: "(not built)",
			BinaryPath: "/ws/bar/canopy", IsRelease: false, HasBinary: false, Active: false},
	}
}

// keyMsg builds a tea.KeyMsg for the named key — saves typing tea.KeyMsg{...}
// across every test. Uses Runes for letters and Type for navigation keys.
func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func keyType(t tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: t}
}

// expectQuit asserts cmd is a tea.Quit (produces a QuitMsg when called).
// Fail-loud — without this most "did the picker exit?" tests devolve to
// vague pointer comparisons.
func expectQuit(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd, got nil")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("cmd produced %T, want tea.QuitMsg", cmd())
	}
}

func TestNewUsePicker_CursorStartsOnActive(t *testing.T) {
	rows := fixtureRows()
	// Move active off row 0 to verify NewUsePicker tracks it.
	rows[0].Active = false
	rows[1].Active = true
	m := NewUsePicker(rows, "Active: x -> y")
	if m.cursor != 1 {
		t.Errorf("cursor=%d, want 1 (active row index)", m.cursor)
	}
	if m.activeIdx != 1 {
		t.Errorf("activeIdx=%d, want 1", m.activeIdx)
	}
}

func TestNewUsePicker_NoActive_CursorAtZero(t *testing.T) {
	rows := fixtureRows()
	for i := range rows {
		rows[i].Active = false
	}
	m := NewUsePicker(rows, "")
	if m.cursor != 0 {
		t.Errorf("cursor=%d, want 0", m.cursor)
	}
	if m.activeIdx != -1 {
		t.Errorf("activeIdx=%d, want -1", m.activeIdx)
	}
}

func TestUsePicker_CursorDownWraps(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 2 // last row
	out, _ := m.Update(keyType(tea.KeyDown))
	if out.(*UsePickerModel).cursor != 0 {
		t.Errorf("cursor=%d after down-wrap, want 0", out.(*UsePickerModel).cursor)
	}
}

func TestUsePicker_CursorUpWraps(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 0
	out, _ := m.Update(keyType(tea.KeyUp))
	if out.(*UsePickerModel).cursor != 2 {
		t.Errorf("cursor=%d after up-wrap, want 2", out.(*UsePickerModel).cursor)
	}
}

func TestUsePicker_VimKeys(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 0
	out, _ := m.Update(keyRune('j'))
	if out.(*UsePickerModel).cursor != 1 {
		t.Errorf("j: cursor=%d, want 1", out.(*UsePickerModel).cursor)
	}
	out, _ = out.(*UsePickerModel).Update(keyRune('k'))
	if out.(*UsePickerModel).cursor != 0 {
		t.Errorf("k: cursor=%d, want 0", out.(*UsePickerModel).cursor)
	}
}

func TestUsePicker_HomeAndEnd(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 1
	out, _ := m.Update(keyRune('g'))
	if out.(*UsePickerModel).cursor != 0 {
		t.Errorf("g: cursor=%d, want 0", out.(*UsePickerModel).cursor)
	}
	out, _ = out.(*UsePickerModel).Update(keyRune('G'))
	if out.(*UsePickerModel).cursor != 2 {
		t.Errorf("G: cursor=%d, want 2", out.(*UsePickerModel).cursor)
	}
}

func TestUsePicker_EnterChoosesBuiltRow(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 1 // feature-foo, HasBinary=true
	out, cmd := m.Update(keyType(tea.KeyEnter))
	pm := out.(*UsePickerModel)
	if pm.chosenTarget != "feature-foo" {
		t.Errorf("chosenTarget=%q, want feature-foo", pm.chosenTarget)
	}
	if pm.chosenBuild {
		t.Errorf("chosenBuild=true; Enter shouldn't set it")
	}
	expectQuit(t, cmd)
}

func TestUsePicker_EnterOnMissingBinary_SetsHintNoQuit(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 2 // wip-bar, HasBinary=false
	out, cmd := m.Update(keyType(tea.KeyEnter))
	pm := out.(*UsePickerModel)
	if pm.chosenTarget != "" {
		t.Errorf("chosenTarget=%q; Enter on missing-binary should not pick", pm.chosenTarget)
	}
	if cmd != nil {
		t.Errorf("expected no tea.Quit on missing-binary Enter, got %T", cmd())
	}
	if !strings.Contains(pm.hint, "press b to build") {
		t.Errorf("hint=%q, want 'press b to build' nudge", pm.hint)
	}
}

func TestUsePicker_EnterOnMissingRelease_HintAboutInstall(t *testing.T) {
	rows := []UseRow{
		{Target: "release", IsRelease: true, HasBinary: false},
	}
	m := NewUsePicker(rows, "")
	out, cmd := m.Update(keyType(tea.KeyEnter))
	pm := out.(*UsePickerModel)
	if cmd != nil {
		t.Errorf("expected no quit on missing release, got %T", cmd())
	}
	if !strings.Contains(pm.hint, "make install") {
		t.Errorf("hint=%q, want 'make install' nudge", pm.hint)
	}
}

func TestUsePicker_BuildKeyOnWorkspace_SetsBuildAndQuits(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 1 // feature-foo
	out, cmd := m.Update(keyRune('b'))
	pm := out.(*UsePickerModel)
	if pm.chosenTarget != "feature-foo" {
		t.Errorf("chosenTarget=%q, want feature-foo", pm.chosenTarget)
	}
	if !pm.chosenBuild {
		t.Errorf("chosenBuild=false; 'b' should set it true")
	}
	expectQuit(t, cmd)
}

func TestUsePicker_BuildKeyOnMissingBinaryWorkspace_StillBuilds(t *testing.T) {
	// 'b' on a workspace with no binary is the canonical fix path —
	// "no dev binary built, press b to build". Must succeed regardless
	// of HasBinary so the hint nudge actually works.
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 2 // wip-bar, HasBinary=false
	out, cmd := m.Update(keyRune('b'))
	pm := out.(*UsePickerModel)
	if pm.chosenTarget != "wip-bar" {
		t.Errorf("chosenTarget=%q, want wip-bar", pm.chosenTarget)
	}
	if !pm.chosenBuild {
		t.Errorf("chosenBuild should be true for 'b' on missing binary")
	}
	expectQuit(t, cmd)
}

func TestUsePicker_BuildKeyOnRelease_HintNoQuit(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 0 // release row
	out, cmd := m.Update(keyRune('b'))
	pm := out.(*UsePickerModel)
	if pm.chosenTarget != "" {
		t.Errorf("chosenTarget=%q; 'b' on release should not pick", pm.chosenTarget)
	}
	if pm.chosenBuild {
		t.Errorf("chosenBuild=true; 'b' on release should not set it")
	}
	if cmd != nil {
		t.Errorf("expected no quit on 'b'-on-release, got %T", cmd())
	}
	if !strings.Contains(pm.hint, "make install") {
		t.Errorf("hint=%q, want 'make install' nudge", pm.hint)
	}
}

func TestUsePicker_EscCancels(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 1
	out, cmd := m.Update(keyType(tea.KeyEsc))
	pm := out.(*UsePickerModel)
	if pm.chosenTarget != "" {
		t.Errorf("Esc should not set chosenTarget, got %q", pm.chosenTarget)
	}
	expectQuit(t, cmd)
}

func TestUsePicker_QCancels(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	out, cmd := m.Update(keyRune('q'))
	pm := out.(*UsePickerModel)
	if pm.chosenTarget != "" {
		t.Errorf("q should not set chosenTarget, got %q", pm.chosenTarget)
	}
	expectQuit(t, cmd)
}

func TestUsePicker_CtrlCCancels(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	out, cmd := m.Update(keyType(tea.KeyCtrlC))
	pm := out.(*UsePickerModel)
	if pm.chosenTarget != "" {
		t.Errorf("ctrl+c should not set chosenTarget, got %q", pm.chosenTarget)
	}
	expectQuit(t, cmd)
}

func TestUsePicker_HintClearsOnNextKey(t *testing.T) {
	// One-frame hint contract: any subsequent keypress wipes the hint
	// so it doesn't carry stale advice across navigation.
	m := NewUsePicker(fixtureRows(), "")
	m.cursor = 2
	out, _ := m.Update(keyType(tea.KeyEnter)) // sets hint
	pm := out.(*UsePickerModel)
	if pm.hint == "" {
		t.Fatal("expected hint set; setup invariant broken")
	}
	out, _ = pm.Update(keyRune('j'))
	if out.(*UsePickerModel).hint != "" {
		t.Errorf("hint=%q after navigation; want cleared", out.(*UsePickerModel).hint)
	}
}

func TestUsePicker_NarrowTerminal_ShowsHint(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "Active: x -> y")
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 20})
	v := m.View()
	if !strings.Contains(v, "Terminal too narrow") {
		t.Errorf("View at width=30 missing narrow-terminal hint:\n%s", v)
	}
	if !strings.Contains(v, "--list") {
		t.Errorf("Narrow hint missing --list escape hatch:\n%s", v)
	}
}

func TestUsePicker_WideTerminal_RendersRows(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "Active: x -> y")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v := m.View()
	for _, want := range []string{"release", "feature-foo", "wip-bar"} {
		if !strings.Contains(v, want) {
			t.Errorf("View missing row %q:\n%s", want, v)
		}
	}
}

func TestUsePicker_View_ActiveMarker(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "") // row 0 (release) is active
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v := m.View()
	// "▶" should appear before "release"; without bullet marker it
	// would just be "  release" (two spaces).
	if !strings.Contains(v, "▶") {
		t.Errorf("View missing ▶ marker for active row:\n%s", v)
	}
	releaseIdx := strings.Index(v, "release")
	markerIdx := strings.Index(v, "▶")
	if markerIdx == -1 || markerIdx > releaseIdx {
		t.Errorf("▶ should appear before 'release'; marker@%d release@%d", markerIdx, releaseIdx)
	}
}

func TestUsePicker_View_HeaderText(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "Active: /home/u/.local/bin/canopy -> canopy.bin")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v := m.View()
	if !strings.Contains(v, "Active: /home/u/.local/bin/canopy -> canopy.bin") {
		t.Errorf("View missing activeSymlinkText header:\n%s", v)
	}
}

func TestUsePicker_View_FooterHelp(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v := m.View()
	for _, want := range []string{"select", "switch", "build", "cancel"} {
		if !strings.Contains(v, want) {
			t.Errorf("Footer missing %q:\n%s", want, v)
		}
	}
}

func TestUsePicker_View_NotBuiltRowVisible(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v := m.View()
	if !strings.Contains(v, "(not built)") {
		t.Errorf("View missing (not built) for wip-bar row:\n%s", v)
	}
}

func TestUsePicker_EmptyRows_NoCrash(t *testing.T) {
	// Defensive: no rows at all (state.json + release both missing).
	m := NewUsePicker(nil, "Active: missing")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	// Navigation should not panic.
	m.Update(keyType(tea.KeyDown))
	m.Update(keyType(tea.KeyUp))
	m.Update(keyType(tea.KeyEnter))
	m.Update(keyRune('b'))
	// And cancel should still work.
	_, cmd := m.Update(keyType(tea.KeyEsc))
	expectQuit(t, cmd)
}

func TestUsePicker_WindowSize_TracksDims(t *testing.T) {
	m := NewUsePicker(fixtureRows(), "")
	out, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	pm := out.(*UsePickerModel)
	if pm.width != 120 || pm.height != 40 {
		t.Errorf("window dims = (%d,%d), want (120,40)", pm.width, pm.height)
	}
}
