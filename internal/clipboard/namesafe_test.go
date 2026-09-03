package clipboard

import (
	"regexp"
	"strings"
	"testing"
)

// hashSuffixRe matches the "-" + 8 lowercase hex chars suffix
// SanitizeArtifactName appends only when it had to substitute a
// character.
var hashSuffixRe = regexp.MustCompile(`-[0-9a-f]{8}$`)

// TestSanitizeArtifactName_CleanInputPassesThroughUnchanged covers the
// common case — a registered host name, or the bare ~/.ssh/config-alias
// shape most raw --remote targets take (e.g. "tower") — where nothing
// needs substituting. These must pass through byte-for-byte: existing
// on-disk artifact filenames for already-bridged hosts must never
// shift under them on a canopy upgrade.
func TestSanitizeArtifactName_CleanInputPassesThroughUnchanged(t *testing.T) {
	for _, in := range []string{"tower", "tower.lan", "my-host_01"} {
		if got := SanitizeArtifactName(in); got != in {
			t.Errorf("SanitizeArtifactName(%q) = %q; want unchanged passthrough", in, got)
		}
	}
}

// TestSanitizeArtifactName_EmptyString is the one hardcoded special
// case: no character to hash usefully, so the fixed "host" fallback
// applies (unsuffixed — there's nothing to disambiguate against).
func TestSanitizeArtifactName_EmptyString(t *testing.T) {
	if got := SanitizeArtifactName(""); got != "host" {
		t.Errorf("SanitizeArtifactName(\"\") = %q; want %q", got, "host")
	}
}

// TestSanitizeArtifactName_MessyInputGetsHashSuffix covers the raw
// --remote/--on target shapes that motivated this function
// (user@host, host:port, path-traversal-shaped input): any character
// outside [A-Za-z0-9._-] becomes '-', and the result carries an
// 8-hex-char suffix so lossy substitution can't collide two distinct
// specs onto the same artifact filename. Also asserts "/" never
// survives (the path-traversal-safety property) and that sanitization
// is deterministic.
func TestSanitizeArtifactName_MessyInputGetsHashSuffix(t *testing.T) {
	tests := []struct {
		name, in, wantPrefix string
	}{
		{"user_at_host", "avi@tower.example.com", "avi-tower.example.com-"},
		{"host_colon_port", "tower.lan:2222", "tower.lan-2222-"},
		{"path_traversal_shaped", "../../etc/passwd", "..-..-etc-passwd-"},
		{"whitespace", "my host", "my-host-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeArtifactName(tc.in)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("SanitizeArtifactName(%q) = %q; want prefix %q", tc.in, got, tc.wantPrefix)
			}
			if !hashSuffixRe.MatchString(got) {
				t.Errorf("SanitizeArtifactName(%q) = %q; want an 8-hex-char disambiguating suffix", tc.in, got)
			}
			if strings.Contains(got, "/") {
				t.Errorf("SanitizeArtifactName(%q) = %q; must never contain '/'", tc.in, got)
			}
			// Deterministic: same input, same output, every time.
			if again := SanitizeArtifactName(tc.in); again != got {
				t.Errorf("SanitizeArtifactName(%q) not deterministic: %q then %q", tc.in, got, again)
			}
		})
	}
}

// TestSanitizeArtifactName_DistinctTargetsNoLongerCollide is the
// regression test for the collision the hash suffix fixes: two
// genuinely different raw --remote targets that both mangle down to
// the same substituted string ("tower:1" and "tower-1" both become
// "tower-1" without disambiguation) must now sanitize to DIFFERENT
// artifact names, so their SSH snippets / systemd tunnel units can't
// silently clobber each other across sessions.
func TestSanitizeArtifactName_DistinctTargetsNoLongerCollide(t *testing.T) {
	a := SanitizeArtifactName("tower:1")
	b := SanitizeArtifactName("tower-1")
	if a == b {
		t.Errorf("SanitizeArtifactName(%q) and SanitizeArtifactName(%q) collide on %q — distinct hosts would clobber each other's clipboard-bridge artifacts", "tower:1", "tower-1", a)
	}
	// "tower-1" itself is clean (no substitution) so it must be the
	// untouched passthrough case, distinguishing it from "tower:1"'s
	// hash-suffixed form.
	if b != "tower-1" {
		t.Errorf("SanitizeArtifactName(%q) = %q; want unchanged passthrough %q", "tower-1", b, "tower-1")
	}
}
