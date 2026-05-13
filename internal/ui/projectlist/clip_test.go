package projectlist

import (
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/state"
)

// TestView_CropsToHeight covers the height-aware viewport added so the
// top chrome (brand pill, tab bar) stays on-screen when the table is
// taller than the parent's envelope. The crop centers the cursor in the
// window and replaces the first/last visible line with a dim "↑N more"
// / "↓N more" indicator when rows are hidden in that direction.
func TestView_CropsToHeight(t *testing.T) {
	rows := []state.GlobalRow{}
	for i := 0; i < 20; i++ {
		rows = append(rows, state.GlobalRow{
			Project: "p1", ProjectRoot: "/p1",
			Name: "ws-" + string(rune('a'+i)), Branch: "b", Status: state.StatusReady,
		})
	}
	m := New(Options{})
	m.SetRows(rows)
	m.cursor = 10 // somewhere in the middle

	t.Run("no crop when height is zero (parent didn't size us)", func(t *testing.T) {
		m.SetSize(80, 0)
		out := stripStyle(m.View())
		// All 20 row names should appear when height=0 (the explicit
		// fallback for "don't know height yet").
		for i := 0; i < 20; i++ {
			name := "ws-" + string(rune('a'+i))
			if !strings.Contains(out, name) {
				t.Errorf("height=0: missing %q in output:\n%s", name, out)
			}
		}
	})

	t.Run("crops to height when table is taller", func(t *testing.T) {
		m.SetSize(80, 8)
		out := stripStyle(m.View())
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) > 8 {
			t.Errorf("height=8 should crop to <=8 lines; got %d:\n%s", len(lines), out)
		}
	})

	t.Run("cursor row visible in cropped view", func(t *testing.T) {
		m.cursor = 10
		m.SetSize(80, 8)
		out := stripStyle(m.View())
		cursorName := "ws-" + string(rune('a'+10))
		if !strings.Contains(out, cursorName) {
			t.Errorf("cursor row %q missing from cropped view:\n%s", cursorName, out)
		}
	})

	t.Run("shows ↑N more when rows hidden above", func(t *testing.T) {
		m.cursor = 15 // near the bottom
		m.SetSize(80, 6)
		out := stripStyle(m.View())
		if !strings.Contains(out, "more") {
			t.Errorf("expected '↑N more' indicator when rows hidden above; got:\n%s", out)
		}
	})

	t.Run("shows ↓N more when rows hidden below", func(t *testing.T) {
		m.cursor = 0 // top
		m.SetSize(80, 6)
		out := stripStyle(m.View())
		if !strings.Contains(out, "more") {
			t.Errorf("expected '↓N more' indicator when rows hidden below; got:\n%s", out)
		}
	})
}
