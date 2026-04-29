package projectlist

import (
	"testing"

	"github.com/oncactus/canopy/internal/state"
)

// statusGlyphFor mirrors render.statusGlyph in the parent ui package.
// Both must return identical glyphs so the project TUI and global TUI
// (which uses this package) read the same under protanopia / monochrome.
func TestStatusGlyphFor_DistinctPerStatus(t *testing.T) {
	cases := []struct {
		status state.Status
		want   string
	}{
		{state.StatusReady, " "},
		{state.StatusSettingUp, "…"},
		{state.StatusStopped, "⏸"},
		{state.StatusBroken, "✗"},
		{state.StatusOrphaned, "!"},
		{"main", " "},
		{"unknown_future_status", " "},
	}
	seen := map[string]state.Status{}
	for _, tc := range cases {
		got := statusGlyphFor(tc.status)
		if got != tc.want {
			t.Errorf("statusGlyphFor(%q) = %q, want %q", tc.status, got, tc.want)
		}
		if got != " " {
			if prev, ok := seen[got]; ok {
				t.Errorf("glyph %q used by both %q and %q — not distinct under monochrome", got, prev, tc.status)
			}
			seen[got] = tc.status
		}
	}
}
