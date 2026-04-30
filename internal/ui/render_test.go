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

// renderVersionPill drives the top-bar release-vs-DEV indicator. Three
// branches: DEV pill (devWorkspace set), release pill (versionLabel set),
// suppressed (both empty). Any regression here changes the user's
// at-a-glance "which canopy am I running" cue, which is the whole point
// of the install-and-dev-workflow design.
func TestRenderVersionPill_dev(t *testing.T) {
	m := &Model{}
	m.SetVersionInfo("ignored-when-dev-set", "feature-A")
	pill := m.renderVersionPill()
	if pill == "" {
		t.Fatal("DEV pill must render when devWorkspace is set")
	}
	if !strings.Contains(pill, "DEV: feature-A") {
		t.Errorf("DEV pill missing 'DEV: feature-A' content; got %q", pill)
	}
	// versionLabel must be ignored when devWorkspace wins — the DEV
	// signal is more useful and we don't want to clutter the pill.
	if strings.Contains(pill, "ignored-when-dev-set") {
		t.Errorf("DEV pill leaked versionLabel content: %q", pill)
	}
}

func TestRenderVersionPill_release(t *testing.T) {
	m := &Model{}
	m.SetVersionInfo("v0.12.0+abc1234", "")
	pill := m.renderVersionPill()
	if pill == "" {
		t.Fatal("release pill must render when versionLabel is set")
	}
	if !strings.Contains(pill, "v0.12.0+abc1234") {
		t.Errorf("release pill missing version label; got %q", pill)
	}
	if strings.Contains(pill, "DEV") {
		t.Errorf("release pill must not contain 'DEV': %q", pill)
	}
}

func TestRenderVersionPill_suppressed(t *testing.T) {
	m := &Model{}
	// Default zero-value → no version info, no pill. This is the
	// "tests + bare-bones invocations don't gain unwanted chrome"
	// path described in the renderVersionPill docstring.
	if pill := m.renderVersionPill(); pill != "" {
		t.Errorf("empty version info should suppress pill; got %q", pill)
	}

	// Explicit empty pair via setter → also suppressed.
	m.SetVersionInfo("", "")
	if pill := m.renderVersionPill(); pill != "" {
		t.Errorf("explicit-empty SetVersionInfo should suppress pill; got %q", pill)
	}
}
