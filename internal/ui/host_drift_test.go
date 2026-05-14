package ui

import (
	"strings"
	"testing"
)

// TestHostReferenceVersion exercises the priority ladder used to pick
// the bare semver every remote host's canopy_version is compared
// against on the Hosts tab. Three branches:
//
//   - release laptop → use the laptop's own versionLabel
//   - dev laptop with cached upstream-latest → fall back to that
//   - dev laptop with no cache → return "" (suppresses all badges)
//
// The first branch is the dogfood case ("upgrade my laptop, then visit
// the Hosts tab to see which hosts to U-key"); the second is the
// contributor case (dev binary reaching a release fleet); the third
// is the offline first-run guard against bad badges.
func TestHostReferenceVersion(t *testing.T) {
	cases := []struct {
		name             string
		versionLabel     string
		devWorkspace     string
		upgradeAvailable string
		want             string
	}{
		{
			name:         "release laptop wins regardless of upgrade cache",
			versionLabel: "v0.17.4.0+abc1234",
			// Even if there's a newer release cached, the local-version
			// match is the actionable comparison ("does this host
			// match the version I'm running?").
			upgradeAvailable: "0.17.5.0",
			want:             "v0.17.4.0+abc1234",
		},
		{
			name:         "release laptop with no cached upgrade",
			versionLabel: "v0.17.4.0",
			want:         "v0.17.4.0",
		},
		{
			name:             "dev laptop falls back to cached upstream-latest",
			devWorkspace:     "feature-A",
			upgradeAvailable: "0.17.4.0",
			want:             "0.17.4.0",
		},
		{
			name:         "dev laptop with no cache returns empty (suppresses badges)",
			devWorkspace: "feature-A",
			want:         "",
		},
		{
			name:         "literal 'dev' versionLabel is treated as dev",
			versionLabel: "dev",
			// No cache available → empty reference, suppress badge.
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{
				versionLabel:     tc.versionLabel,
				devWorkspace:     tc.devWorkspace,
				upgradeAvailable: tc.upgradeAvailable,
			}
			if got := m.hostReferenceVersion(); got != tc.want {
				t.Errorf("hostReferenceVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDriftAnnotation_HostDetail covers the four detail-drawer
// branches: same-or-unknown (no annotation), behind (⇑ upgrade
// available + press U hint), ahead (⇓ host is ahead).
//
// The behind case is the most user-facing: it's the line that turns
// "huh, this host is at v0.17.3" into "press U." Missing this hint
// would defeat the whole point of the upgrade indicator.
func TestDriftAnnotation_HostDetail(t *testing.T) {
	cases := []struct {
		name      string
		remote    string
		reference string
		wantSubs  []string // every substring must appear
		wantEmpty bool
	}{
		{
			name:      "same versions no annotation",
			remote:    "0.17.4.0",
			reference: "v0.17.4.0",
			wantEmpty: true,
		},
		{
			name:      "unknown reference no annotation",
			remote:    "0.17.4.0",
			reference: "",
			wantEmpty: true,
		},
		{
			name:      "dev remote no annotation",
			remote:    "dev",
			reference: "v0.17.4.0",
			wantEmpty: true,
		},
		{
			name:      "behind reference surfaces upgrade hint",
			remote:    "0.17.3.0",
			reference: "v0.17.4.0",
			wantSubs:  []string{"⇑", "upgrade available", "v0.17.4.0", "press U"},
		},
		{
			name:      "ahead reference surfaces inverted mismatch",
			remote:    "0.18.0.0",
			reference: "v0.17.4.0",
			wantSubs:  []string{"⇓", "ahead", "v0.17.4.0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := stripANSIDrift(driftAnnotation(tc.remote, tc.reference))
			if tc.wantEmpty {
				if out != "" {
					t.Errorf("expected empty annotation; got %q", out)
				}
				return
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(out, sub) {
					t.Errorf("annotation %q missing %q", out, sub)
				}
			}
		})
	}
}

// stripANSIDrift removes lipgloss color escapes so substring checks
// in this test file don't have to mirror the color codes. Same logic
// as the hosts package test helper — duplicated here because that
// helper is unexported.
func stripANSIDrift(s string) string {
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
