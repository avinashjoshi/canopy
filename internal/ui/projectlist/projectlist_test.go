package projectlist

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/state"
)

// sampleRows builds a small deterministic []state.GlobalRow for tests
// that don't care about the specific contents.
func sampleRows() []state.GlobalRow {
	return []state.GlobalRow{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "bold-falcon", Branch: "feat/x", Status: state.StatusReady, Port: 3000, TmuxSession: "cravd-bold-falcon", Alive: true},
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "soft-fox", Branch: "feat/y", Status: state.StatusStopped, Port: 3010, TmuxSession: "cravd-soft-fox", Alive: false},
		{Project: "canopy", ProjectRoot: "/b/canopy", Name: "ancient-hornet", Branch: "main", Status: state.StatusReady, Port: 4000, TmuxSession: "canopy-ancient-hornet", Alive: true},
	}
}

func key(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// TestNew_EmptyRendersWithoutPanic: a freshly-constructed Model with no
// rows must render — empty state copy, no panic.
func TestNew_EmptyRendersWithoutPanic(t *testing.T) {
	m := New(Options{})
	out := m.View()
	if out == "" {
		t.Errorf("View() empty string")
	}
	if !strings.Contains(out, "No canopy projects") {
		t.Errorf("expected empty-state hint; got %q", out)
	}
}

// TestSetRows_ClampsCursor: rows shrink to fewer than the cursor; cursor
// should clamp.
func TestSetRows_ClampsCursor(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows()) // 3 rows
	// Move cursor to last row.
	m, _ = m.Update(key("end"))
	if m.cursor != 2 {
		t.Fatalf("cursor after end = %d, want 2", m.cursor)
	}
	// Shrink rows to 1.
	m.SetRows(sampleRows()[:1])
	if m.cursor != 0 {
		t.Errorf("cursor after shrink = %d, want clamped to 0", m.cursor)
	}
}

// TestNav_JKAndArrows: ↑/↓/j/k all move cursor, both within bounds.
func TestNav_JKAndArrows(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())

	// Down twice via j → cursor=2.
	m, _ = m.Update(key("j"))
	m, _ = m.Update(key("j"))
	if m.cursor != 2 {
		t.Errorf("cursor after two j = %d, want 2", m.cursor)
	}
	// Past-end stays at last row.
	m, _ = m.Update(key("j"))
	if m.cursor != 2 {
		t.Errorf("cursor past-end = %d, want 2 (clamped)", m.cursor)
	}
	// Up via k → cursor=1.
	m, _ = m.Update(key("k"))
	if m.cursor != 1 {
		t.Errorf("cursor after k = %d, want 1", m.cursor)
	}
	// Above-zero stays at 0.
	m, _ = m.Update(key("k"))
	m, _ = m.Update(key("k"))
	if m.cursor != 0 {
		t.Errorf("cursor above-zero = %d, want 0", m.cursor)
	}
	// Arrow keys also work.
	m, _ = m.Update(key("down"))
	if m.cursor != 1 {
		t.Errorf("cursor after down arrow = %d, want 1", m.cursor)
	}
	m, _ = m.Update(key("up"))
	if m.cursor != 0 {
		t.Errorf("cursor after up arrow = %d, want 0", m.cursor)
	}
}

// TestNav_GAndHomeEnd: g/G/home/end jump to first/last.
func TestNav_GAndHomeEnd(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())

	m, _ = m.Update(key("end"))
	if m.cursor != 2 {
		t.Errorf("end → cursor %d, want 2", m.cursor)
	}
	m, _ = m.Update(key("home"))
	if m.cursor != 0 {
		t.Errorf("home → cursor %d, want 0", m.cursor)
	}
	m, _ = m.Update(key("G"))
	if m.cursor != 2 {
		t.Errorf("G → cursor %d, want 2", m.cursor)
	}
	m, _ = m.Update(key("g"))
	if m.cursor != 0 {
		t.Errorf("g → cursor %d, want 0", m.cursor)
	}
}

// TestEnter_InvokesOnActivate: enter calls OnActivate with the cursor's
// row and returns the resulting tea.Cmd.
func TestEnter_InvokesOnActivate(t *testing.T) {
	var captured state.GlobalRow
	called := false
	cmd := func() tea.Msg { return "activated" }
	m := New(Options{
		OnActivate: func(r state.GlobalRow) tea.Cmd {
			captured = r
			called = true
			return cmd
		},
	})
	m.SetRows(sampleRows())

	// Move to row 1 (cravd / soft-fox), then enter.
	m, _ = m.Update(key("j"))
	_, gotCmd := m.Update(key("enter"))

	if !called {
		t.Fatalf("OnActivate not called")
	}
	if captured.Name != "soft-fox" {
		t.Errorf("OnActivate received row %q, want soft-fox", captured.Name)
	}
	if gotCmd == nil {
		t.Errorf("enter returned nil cmd, want forwarded OnActivate cmd")
	}
}

// TestEnter_NoCallbackIsNoOp: enter without OnActivate set is a no-op,
// no panic.
func TestEnter_NoCallbackIsNoOp(t *testing.T) {
	m := New(Options{}) // no OnActivate
	m.SetRows(sampleRows())

	_, cmd := m.Update(key("enter"))
	if cmd != nil {
		t.Errorf("enter without OnActivate returned non-nil cmd")
	}
}

// TestEnter_EmptyListIsNoOp: enter on an empty list is a no-op even
// when OnActivate is set (no panic, no callback fired).
func TestEnter_EmptyListIsNoOp(t *testing.T) {
	called := false
	m := New(Options{
		OnActivate: func(r state.GlobalRow) tea.Cmd {
			called = true
			return nil
		},
	})
	// no SetRows — list stays empty.

	_, _ = m.Update(key("enter"))
	if called {
		t.Errorf("OnActivate fired on empty list")
	}
}

// TestGoToProject_InvokesCallback: 'c' calls OnGoToProject with the row
// at the cursor and forwards its tea.Cmd.
func TestGoToProject_InvokesCallback(t *testing.T) {
	var captured state.GlobalRow
	called := false
	m := New(Options{
		OnGoToProject: func(r state.GlobalRow) tea.Cmd {
			captured = r
			called = true
			return func() tea.Msg { return "go-to" }
		},
	})
	m.SetRows(sampleRows())
	// Move to row 2 (canopy / ancient-hornet) and press c.
	m, _ = m.Update(key("end"))
	_, gotCmd := m.Update(key("o"))

	if !called {
		t.Fatalf("OnGoToProject not invoked")
	}
	if captured.Name != "ancient-hornet" {
		t.Errorf("captured row %q, want ancient-hornet", captured.Name)
	}
	if gotCmd == nil {
		t.Errorf("c returned nil cmd; expected forwarded callback cmd")
	}
}

// TestGoToProject_NoCallback: o without OnGoToProject is a no-op.
func TestGoToProject_NoCallback(t *testing.T) {
	m := New(Options{}) // no OnGoToProject
	m.SetRows(sampleRows())

	_, cmd := m.Update(key("o"))
	if cmd != nil {
		t.Errorf("o without OnGoToProject returned non-nil cmd")
	}
}

// TestRender_GroupsByProject: consecutive rows with the same Project share
// one header line; a project change emits a new header. Verifies the
// grouped layout doesn't repeat project names per row.
func TestRender_GroupsByProject(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	out := m.View()

	// Project headers appear exactly once per project even when there are
	// multiple rows. cravd has 2 rows; the literal "cravd" should appear
	// only on its header line in the rendered output (status cells, etc.,
	// don't echo the project basename).
	cravdCount := strings.Count(out, "cravd")
	if cravdCount != 1 {
		t.Errorf("rendered output mentions 'cravd' %d times, want exactly 1 (header only)\noutput:\n%s",
			cravdCount, out)
	}
	// canopy has 1 row; same expectation.
	canopyCount := strings.Count(out, "canopy")
	if canopyCount != 1 {
		t.Errorf("rendered output mentions 'canopy' %d times, want exactly 1\noutput:\n%s",
			canopyCount, out)
	}
}

// TestRefresh_InvokesOnRefresh: r calls OnRefresh and forwards its cmd.
func TestRefresh_InvokesOnRefresh(t *testing.T) {
	called := false
	m := New(Options{
		OnRefresh: func() tea.Cmd {
			called = true
			return func() tea.Msg { return "refreshed" }
		},
	})

	_, gotCmd := m.Update(key("r"))
	if !called {
		t.Errorf("OnRefresh not called")
	}
	if gotCmd == nil {
		t.Errorf("r returned nil cmd, want forwarded OnRefresh cmd")
	}
}

// TestSetError_RendersBanner: SetError above the table, cleared by passing nil.
func TestSetError_RendersBanner(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())

	m.SetError(errors.New("boom"))
	if !strings.Contains(m.View(), "boom") {
		t.Errorf("error banner not rendered")
	}

	m.SetError(nil)
	if strings.Contains(m.View(), "boom") {
		t.Errorf("error banner not cleared")
	}
}

// TestCursorRow_Empty: returns false when list is empty.
func TestCursorRow_Empty(t *testing.T) {
	m := New(Options{})
	_, ok := m.CursorRow()
	if ok {
		t.Errorf("CursorRow on empty list returned ok=true")
	}
}

// TestCursorRow_NonEmpty: returns the row at cursor.
func TestCursorRow_NonEmpty(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	m, _ = m.Update(key("end"))

	row, ok := m.CursorRow()
	if !ok {
		t.Fatalf("CursorRow ok=false, want true")
	}
	if row.Name != "ancient-hornet" {
		t.Errorf("CursorRow at end = %q, want ancient-hornet", row.Name)
	}
}

// TestRender_IncludesRowFields: the rendered output contains the row data
// (project, name, branch, status, port, badge). Smoke test only — exact
// formatting is left to lipgloss + visual review.
func TestRender_IncludesRowFields(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	out := m.View()

	for _, want := range []string{"cravd", "bold-falcon", "feat/x", "ready", "3000"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestSetSize_DoesNotPanic: SetSize is a setter; tested only for
// not-blowing-up on extreme values.
func TestSetSize_DoesNotPanic(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	m.SetSize(0, 0)
	m.SetSize(10000, 10000)
	_ = m.View()
}
