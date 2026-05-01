package ui

import (
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/state"
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

// TestRenderVersionPill_upgradeAvailable: 4th branch added in v0.13.
// Release pill mutates to include "⇑ v<latest>" when upgradeAvailable
// is set. Pill is still rendered (not suppressed); the version label
// is still present.
func TestRenderVersionPill_upgradeAvailable(t *testing.T) {
	m := &Model{}
	m.SetVersionInfo("v0.12.3+abc1234", "")
	m.SetUpgradeAvailable("0.13.0")
	pill := m.renderVersionPill()
	if pill == "" {
		t.Fatal("upgrade-available pill must render")
	}
	if !strings.Contains(pill, "v0.12.3+abc1234") {
		t.Errorf("upgrade pill missing current version; got %q", pill)
	}
	if !strings.Contains(pill, "⇑ v0.13.0") {
		t.Errorf("upgrade pill missing arrow + new version; got %q", pill)
	}
	if strings.Contains(pill, "DEV") {
		t.Errorf("upgrade pill should not contain DEV; got %q", pill)
	}
}

// TestRenderVersionPill_upgradeNotAvailable: setting upgradeAvailable
// to "" reverts to the plain release pill (no arrow, no new version).
// Covers the "post-upgrade clears the pill" path where we want the
// arrow to disappear after the user upgrades.
func TestRenderVersionPill_upgradeNotAvailable(t *testing.T) {
	m := &Model{}
	m.SetVersionInfo("v0.13.0+abc", "")
	m.SetUpgradeAvailable("") // explicit empty
	pill := m.renderVersionPill()
	if !strings.Contains(pill, "v0.13.0") {
		t.Errorf("plain release pill missing version; got %q", pill)
	}
	if strings.Contains(pill, "⇑") {
		t.Errorf("upgrade arrow leaked into plain release pill; got %q", pill)
	}
}

// TestRenderVersionPill_devWinsOverUpgrade: DEV pill wins even when
// upgradeAvailable is set (which it shouldn't be for DEV anyway, but
// defensive). Belt-and-suspenders: route.go won't set
// upgradeAvailable on DEV builds, but if it ever did, the renderer
// suppresses the upgrade arrow to avoid confusion.
func TestRenderVersionPill_devWinsOverUpgrade(t *testing.T) {
	m := &Model{}
	m.SetVersionInfo("dev", "feature-A")
	m.SetUpgradeAvailable("0.13.0") // shouldn't happen, but if it does...
	pill := m.renderVersionPill()
	if !strings.Contains(pill, "DEV: feature-A") {
		t.Errorf("DEV pill must win; got %q", pill)
	}
	if strings.Contains(pill, "⇑") {
		t.Errorf("DEV pill must NOT show upgrade arrow; got %q", pill)
	}
}
