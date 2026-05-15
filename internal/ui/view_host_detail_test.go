package ui

import (
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
)

// TestRenderHostDetail_DriftAnnotationBehind verifies the drawer's
// "canopy: v…" line carries the upgrade-available annotation when the
// host is behind the laptop. This is the spelled-out twin of the
// Hosts-tab row badge: the drawer is where users land for "tell me
// more," so the line has to spell out the reference version, not just
// flash a glyph.
func TestRenderHostDetail_DriftAnnotationBehind(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {CanopyVersion: "0.17.3.0"},
	}
	m.versionLabel = "v0.17.4.0+abc1234"
	m.hostDetailTarget = "tower"

	out := stripANSIForView(m.renderHostDetail())
	if !strings.Contains(out, "v0.17.3.0") {
		t.Errorf("drawer missing remote version line:\n%s", out)
	}
	if !strings.Contains(out, "⇑ upgrade available") {
		t.Errorf("drawer missing ⇑ upgrade annotation:\n%s", out)
	}
	if !strings.Contains(out, "v0.17.4.0") {
		t.Errorf("drawer missing reference version in annotation:\n%s", out)
	}
}

// TestRenderHostDetail_DriftAnnotationAhead pins the inverse case:
// when the host is ahead of local, the drawer flips to "host is
// ahead of your local" so users can tell at a glance whether their
// laptop is the one that needs the upgrade.
func TestRenderHostDetail_DriftAnnotationAhead(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {CanopyVersion: "0.17.5.0"},
	}
	m.versionLabel = "v0.17.4.0+abc1234"
	m.hostDetailTarget = "tower"

	out := stripANSIForView(m.renderHostDetail())
	if !strings.Contains(out, "⇓ host is ahead") {
		t.Errorf("drawer missing ⇓ ahead annotation:\n%s", out)
	}
}

// TestRenderHostDetail_DriftAnnotationSilentOnMatch ensures the
// silence-is-the-signal rule extends to the drawer: when the host's
// version matches the laptop's, no annotation appears. Without this
// guard the drawer would carry noise on every match.
func TestRenderHostDetail_DriftAnnotationSilentOnMatch(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {CanopyVersion: "0.17.4.0"},
	}
	m.versionLabel = "v0.17.4.0+abc1234"
	m.hostDetailTarget = "tower"

	out := stripANSIForView(m.renderHostDetail())
	for _, g := range []string{"⇑", "⇓", "upgrade available", "host is ahead"} {
		if strings.Contains(out, g) {
			t.Errorf("matching versions must not carry annotation %q:\n%s", g, out)
		}
	}
}

// TestRenderHostDetail_DevLocalSuppressesAnnotation covers the dev-
// local-with-no-upstream-cache path: hostReferenceVersion returns ""
// and the annotation must collapse to nothing. Without this the
// drawer would mis-fire as "upgrade available" against an empty
// reference version, which would render as "your local: v".
func TestRenderHostDetail_DevLocalSuppressesAnnotation(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {CanopyVersion: "0.17.3.0"},
	}
	m.devWorkspace = "feature-x"
	m.versionLabel = ""
	m.upgradeAvailable = ""
	m.hostDetailTarget = "tower"

	out := stripANSIForView(m.renderHostDetail())
	for _, g := range []string{"⇑", "⇓", "upgrade available", "host is ahead"} {
		if strings.Contains(out, g) {
			t.Errorf("dev local with empty upstream cache must not annotate %q:\n%s", g, out)
		}
	}
}

// TestRenderHostDetail_DevLocalFallsBackToUpstreamCache pins the
// useful branch of the dev-local path: when contributors are
// dogfooding canopy itself, the upstream-latest cache is the only
// number that compares meaningfully — so the drawer should still flag
// hosts running older releases against that.
func TestRenderHostDetail_DevLocalFallsBackToUpstreamCache(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {CanopyVersion: "0.17.3.0"},
	}
	m.devWorkspace = "feature-x"
	m.versionLabel = ""
	m.upgradeAvailable = "0.17.4.0"
	m.hostDetailTarget = "tower"

	out := stripANSIForView(m.renderHostDetail())
	if !strings.Contains(out, "⇑ upgrade available") {
		t.Errorf("dev-local with upstream cache must still flag behind hosts:\n%s", out)
	}
}

// stripANSIForView is the test-local ANSI sequence stripper. Mirrors
// the one in internal/ui/hosts/hosts_test.go — kept separate (rather
// than exported) so the production code never grows a public ANSI
// helper just to satisfy tests.
func stripANSIForView(s string) string {
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
