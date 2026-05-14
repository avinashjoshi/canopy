package hosts

import (
	"strings"
	"testing"
	"time"

	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
)

func TestClipboardPillPlain_RendersForKnownStates(t *testing.T) {
	cases := []struct {
		bridge string
		want   string
	}{
		{"bridged", "📋 bridged"},
		{"broken", "📋! broken"},
		{"off", ""},     // common state, no pill to keep row uncluttered
		{"", ""},        // unknown / pre-v0.18 remote, no signal
		{"garbage", ""}, // forward-compat: unknown future states render nothing
	}
	for _, c := range cases {
		got := clipboardPillPlain(c.bridge)
		if got != c.want {
			t.Errorf("clipboardPillPlain(%q) = %q, want %q", c.bridge, got, c.want)
		}
	}
}

func TestClipboardPill_StyledOutputContainsExpectedGlyph(t *testing.T) {
	// The styled variant adds ANSI color codes; we just check the
	// visible bytes are present. The unstyled variant covers the
	// stable-content assertion.
	for _, c := range []struct {
		bridge string
		needle string
	}{
		{"bridged", "📋 bridged"},
		{"broken", "📋! broken"},
	} {
		got := clipboardPill(c.bridge)
		if !strings.Contains(got, c.needle) {
			t.Errorf("clipboardPill(%q) = %q, expected to contain %q", c.bridge, got, c.needle)
		}
	}
	// off and "" produce no pill (returns "") in both styled and plain
	// variants; the renderer skips emitting it entirely.
	for _, bridge := range []string{"off", ""} {
		if clipboardPill(bridge) != "" {
			t.Errorf("clipboardPill(%q) should return empty, got %q", bridge, clipboardPill(bridge))
		}
	}
}

func TestBuildRows_CopiesClipboardBridgeFromSnapshot(t *testing.T) {
	hosts := []host.Host{
		{Name: "tower", SSHTarget: "avi@tower.lan", Type: "ssh"},
		{Name: "fly-iad", SSHTarget: "avi@fly-iad.lan", Type: "ssh"},
		{Name: "stale", SSHTarget: "avi@stale.lan", Type: "ssh"},
	}
	snaps := map[string]*state.RemoteHostSnapshot{
		"tower": {
			CanopyVersion:   "0.18.0",
			LastSeen:        time.Now(),
			ClipboardBridge: "bridged",
		},
		"fly-iad": {
			CanopyVersion:   "0.18.0",
			LastSeen:        time.Now(),
			ClipboardBridge: "broken",
		},
		// stale: no snapshot at all → ClipboardBridge stays empty.
	}
	rows := BuildRows(hosts, snaps)
	if len(rows) != 3 {
		t.Fatalf("BuildRows returned %d rows, want 3", len(rows))
	}
	byName := make(map[string]Row)
	for _, r := range rows {
		byName[r.Name] = r
	}
	if byName["tower"].ClipboardBridge != "bridged" {
		t.Errorf("tower.ClipboardBridge = %q, want %q", byName["tower"].ClipboardBridge, "bridged")
	}
	if byName["fly-iad"].ClipboardBridge != "broken" {
		t.Errorf("fly-iad.ClipboardBridge = %q, want %q", byName["fly-iad"].ClipboardBridge, "broken")
	}
	if byName["stale"].ClipboardBridge != "" {
		t.Errorf("stale.ClipboardBridge should be empty when no snapshot, got %q", byName["stale"].ClipboardBridge)
	}
}

func TestRenderRow_IncludesPillAtSufficientWidth(t *testing.T) {
	r := Row{
		Name:            "tower",
		Status:          StatusOnline,
		StatusDetail:    "3s ago",
		Version:         "0.18.0",
		ClipboardBridge: "bridged",
	}
	// At <80c the pill is dropped first (tiered drop, matches the v0.17
	// width strategy for version).
	narrow := renderRow(r, 79, false)
	if strings.Contains(narrow, "📋") {
		t.Errorf("pill rendered at width 79; expected drop until >=80\nrow: %q", narrow)
	}
	wide := renderRow(r, 120, false)
	if !strings.Contains(wide, "📋 bridged") {
		t.Errorf("pill missing at width 120; expected `📋 bridged`\nrow: %q", wide)
	}
}

func TestRenderRow_PillAlsoRendersOnSelectedRow(t *testing.T) {
	// Bug class: the selected-row path uses a separate parts-building
	// branch. Easy to forget the pill there. This test asserts both
	// paths emit it.
	r := Row{
		Name:            "tower",
		Status:          StatusOnline,
		StatusDetail:    "3s ago",
		ClipboardBridge: "bridged",
	}
	selected := renderRow(r, 120, true)
	if !strings.Contains(selected, "📋 bridged") {
		t.Errorf("selected-row path missing pill\nrow: %q", selected)
	}
}

func TestRenderRow_OffStateProducesNoPill(t *testing.T) {
	r := Row{
		Name:            "tower",
		Status:          StatusOnline,
		StatusDetail:    "3s ago",
		ClipboardBridge: "off",
	}
	for _, sel := range []bool{false, true} {
		got := renderRow(r, 120, sel)
		if strings.Contains(got, "📋") {
			t.Errorf("off state should NOT render a pill (selected=%v); got %q", sel, got)
		}
	}
}
