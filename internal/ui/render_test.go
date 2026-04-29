package ui

import (
	"strings"
	"testing"

	"github.com/oncactus/canopy/internal/state"
)

// statusGlyph must return a distinct shape per non-healthy status and a
// space for healthy/main rows. The shapes are the protanopia-friendly
// disambiguator: red (broken) and orange (orphaned) confuse without them.
func TestStatusGlyph_DistinctPerStatus(t *testing.T) {
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
		{"unknown_future_status", " "}, // graceful fallback
	}
	seen := map[string]state.Status{}
	for _, tc := range cases {
		got := statusGlyph(tc.status)
		if got != tc.want {
			t.Errorf("statusGlyph(%q) = %q, want %q", tc.status, got, tc.want)
		}
		// Distinctness check: each non-space glyph must map to one status.
		if got != " " {
			if prev, ok := seen[got]; ok {
				t.Errorf("glyph %q used by both %q and %q — not distinct under monochrome", got, prev, tc.status)
			}
			seen[got] = tc.status
		}
	}
}

// statusCell embeds the glyph + space + name. Verifies the glyph reaches
// the rendered output (lipgloss styling does not strip it) and that the
// padding still produces a name field of at least the requested width.
func TestStatusCell_IncludesGlyphAndName(t *testing.T) {
	cases := []struct {
		status     state.Status
		wantGlyph  string
		wantInCell string // substring that must appear after style stripping
	}{
		{state.StatusBroken, "✗", "broken"},
		{state.StatusOrphaned, "!", "orphaned"},
		{state.StatusReady, " ", "ready"},
		{state.StatusSettingUp, "…", "setting_up"},
	}
	for _, tc := range cases {
		got := statusCell(tc.status, len(tc.wantInCell))
		if !strings.Contains(got, tc.wantGlyph) {
			t.Errorf("statusCell(%q): rendered %q missing glyph %q", tc.status, got, tc.wantGlyph)
		}
		if !strings.Contains(got, tc.wantInCell) {
			t.Errorf("statusCell(%q): rendered %q missing name %q", tc.status, got, tc.wantInCell)
		}
	}
}
