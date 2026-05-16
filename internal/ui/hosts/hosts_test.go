package hosts

import (
	"strings"
	"testing"
	"time"

	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
)

// ----------------------------------------------------------------------
// v0.20 clipboard-bridge pill tests
// ----------------------------------------------------------------------

func TestClipboardPillPlain_RendersForKnownStates(t *testing.T) {
	cases := []struct {
		bridge string
		want   string
	}{
		{"bridged", "📋 bridged"},
		{"broken", "📋! broken"},
		{"off", ""},     // common state, no pill to keep row uncluttered
		{"", ""},        // unknown / pre-v0.20 remote, no signal
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
			CanopyVersion:   "0.20.0",
			LastSeen:        time.Now(),
			ClipboardBridge: "bridged",
		},
		"fly-iad": {
			CanopyVersion:   "0.20.0",
			LastSeen:        time.Now(),
			ClipboardBridge: "broken",
		},
		// stale: no snapshot at all → ClipboardBridge stays empty.
	}
	rows := BuildRows(hosts, snaps, "")
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
		Version:         "0.20.0",
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

// ----------------------------------------------------------------------
// v0.17.5 drift indicator tests
// ----------------------------------------------------------------------

// TestExtractBareSemver covers the normalization seam: every wire form
// canopy emits ("v0.17.4.0+abc", "0.17.4.0", "dev", "(unknown)", "")
// must funnel into the bare dotted-number form or the empty string
// "suppress comparison" sentinel. ComputeDrift and the renderer both
// trust ExtractBareSemver — a bug here would surface as a yellow ⇑
// arrow against a string that isn't actually older.
func TestExtractBareSemver(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Both wire forms strip cleanly.
		{"v0.17.4.0", "0.17.4.0"},
		{"0.17.4.0", "0.17.4.0"},
		// goreleaser-style build suffix peeled off so cache vs binary
		// comparisons line up even when the binary has +sha appended.
		{"v0.17.4.0+abc1234", "0.17.4.0"},
		{"0.17.4.0+abc1234", "0.17.4.0"},
		// Dev / unknown / empty: callers must treat as "no comparison."
		{"dev", ""},
		{"(unknown)", ""},
		{"", ""},
		{"  ", ""},
		// Non-numeric first segment isn't a release we can compare —
		// don't pretend it is.
		{"main-abc1234", ""},
		{"feature-branch", ""},
		// Whitespace tolerance: ssh transport occasionally adds a CR.
		{" v0.17.4.0 ", "0.17.4.0"},
	}
	for _, tc := range cases {
		if got := ExtractBareSemver(tc.in); got != tc.want {
			t.Errorf("ExtractBareSemver(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestComputeDrift exercises every Drift branch. The DriftUnknown row
// is the load-bearing one — if comparison accidentally falls through
// to DriftSame we'd render a green-implication "host matches" when it
// in fact has no measurable version at all.
func TestComputeDrift(t *testing.T) {
	cases := []struct {
		name, remote, ref string
		want              Drift
	}{
		{"same", "0.17.4.0", "v0.17.4.0", DriftSame},
		{"same with build suffix on ref", "0.17.4.0", "v0.17.4.0+abc1234", DriftSame},
		{"remote behind reference", "0.17.3.0", "v0.17.4.0", DriftBehind},
		{"remote ahead of reference", "0.18.0.0", "v0.17.4.0", DriftAhead},
		// Tolerant of length mismatch: trailing zeros are implicit.
		{"length mismatch same", "0.17", "0.17.0.0", DriftSame},
		{"length mismatch behind", "0.17", "0.18.0", DriftBehind},
		// Either side missing/dev/unknown → unknown.
		{"remote dev", "dev", "v0.17.4.0", DriftUnknown},
		{"remote unknown sentinel", "(unknown)", "v0.17.4.0", DriftUnknown},
		{"remote empty", "", "v0.17.4.0", DriftUnknown},
		{"reference empty", "0.17.4.0", "", DriftUnknown},
		{"both empty", "", "", DriftUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeDrift(tc.remote, tc.ref); got != tc.want {
				t.Errorf("ComputeDrift(%q, %q) = %v, want %v",
					tc.remote, tc.ref, got, tc.want)
			}
		})
	}
}

// TestBuildRows_PopulatesDrift verifies the BuildRows ↔ Row.Drift wire:
// the renderer reads Row.Drift, so a regression here would silently
// drop the badge for every host. Three fixtures cover same / behind /
// no-snapshot (must not blow up + must default to DriftUnknown).
func TestBuildRows_PopulatesDrift(t *testing.T) {
	now := time.Now()
	hosts := []host.Host{
		{Name: "alpha", Type: "ssh"},
		{Name: "beta", Type: "ssh"},
		{Name: "gamma", Type: "ssh"}, // no snapshot
	}
	snaps := map[string]*state.RemoteHostSnapshot{
		"alpha": {CanopyVersion: "0.17.4.0", LastSeen: now},
		"beta":  {CanopyVersion: "0.17.3.0", LastSeen: now},
	}
	rows := BuildRows(hosts, snaps, "v0.17.4.0")
	want := map[string]Drift{
		"alpha": DriftSame,
		"beta":  DriftBehind,
		"gamma": DriftUnknown,
	}
	for _, r := range rows {
		if r.Drift != want[r.Name] {
			t.Errorf("Row %q: Drift = %v, want %v", r.Name, r.Drift, want[r.Name])
		}
	}
}

// TestBuildRows_EmptyReferenceSuppresses confirms that passing ""
// disables the badge globally — the dev-with-no-cache fallback path
// must NOT mis-fire as DriftBehind/Ahead. We assert via the row
// directly so this test also pins down the renderRow path's input.
func TestBuildRows_EmptyReferenceSuppresses(t *testing.T) {
	hosts := []host.Host{{Name: "alpha", Type: "ssh"}}
	snaps := map[string]*state.RemoteHostSnapshot{
		"alpha": {CanopyVersion: "0.17.3.0"},
	}
	rows := BuildRows(hosts, snaps, "")
	if rows[0].Drift != DriftUnknown {
		t.Errorf("empty reference: Drift = %v, want DriftUnknown", rows[0].Drift)
	}
}

// TestRenderRow_DriftGlyph asserts the rendered row carries the right
// codepoint for each Drift state, in both selected and non-selected
// paths. We strip ANSI escapes and check for the glyph substring — the
// goal is to catch "no badge when there should be one" regressions
// regardless of the exact style codes around them.
func TestRenderRow_DriftGlyph(t *testing.T) {
	cases := []struct {
		name      string
		drift     Drift
		wantGlyph string // "" means no badge
	}{
		{"same renders no badge", DriftSame, ""},
		{"unknown renders no badge", DriftUnknown, ""},
		{"behind renders ⇑", DriftBehind, "⇑"},
		{"ahead renders ⇓", DriftAhead, "⇓"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Row{
				Name:    "alpha",
				Status:  StatusOnline,
				Version: "0.17.3.0",
				Drift:   tc.drift,
			}
			out := stripANSI(renderRow(r, 120, false))
			if tc.wantGlyph == "" {
				for _, g := range []string{"⇑", "⇓"} {
					if strings.Contains(out, g) {
						t.Errorf("unexpected drift glyph %q in row: %q", g, out)
					}
				}
				return
			}
			if !strings.Contains(out, tc.wantGlyph) {
				t.Errorf("missing drift glyph %q in row: %q", tc.wantGlyph, out)
			}
		})
	}
}

// TestRenderRow_DriftGlyphSelectedRow pins the selected-row path
// (separate code branch from the non-selected one). The selection
// background eats inner foreground colors but the glyph itself must
// still appear in the codepoint stream.
func TestRenderRow_DriftGlyphSelectedRow(t *testing.T) {
	r := Row{
		Name:    "alpha",
		Status:  StatusOnline,
		Version: "0.17.3.0",
		Drift:   DriftBehind,
	}
	out := stripANSI(renderRow(r, 120, true))
	if !strings.Contains(out, "⇑") {
		t.Errorf("selected row missing drift glyph: %q", out)
	}
}

// TestRenderRow_NarrowWidthDropsVersion confirms the existing 80c
// threshold for the version column still applies — below 80 the
// version cell (and therefore the drift badge) is dropped entirely.
// Without this guard, narrow terminals would wrap awkwardly.
func TestRenderRow_NarrowWidthDropsVersion(t *testing.T) {
	r := Row{
		Name:    "alpha",
		Status:  StatusOnline,
		Version: "0.17.3.0",
		Drift:   DriftBehind,
	}
	out := stripANSI(renderRow(r, 60, false))
	if strings.Contains(out, "0.17.3.0") || strings.Contains(out, "⇑") {
		t.Errorf("narrow width should drop version+drift; got %q", out)
	}
}

// stripANSI removes terminal escape sequences so substring assertions
// don't have to know lipgloss's exact color codes. ANSI CSI sequences
// match `\x1b[...m`; that covers everything lipgloss emits here.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
