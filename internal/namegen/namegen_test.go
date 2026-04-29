package namegen_test

import (
	"strings"
	"testing"

	"github.com/oncactus/canopy/internal/namegen"
)

// TestGenerate covers the basic shape: returns "adj-noun", both halves
// from the embedded wordlists.
func TestGenerate(t *testing.T) {
	t.Parallel()
	for i := 0; i < 100; i++ {
		got := namegen.Generate()
		if !namegen.IsValid(got) {
			t.Errorf("Generate produced %q, which IsValid rejected", got)
		}
		if !strings.Contains(got, "-") {
			t.Errorf("Generate produced %q, missing hyphen", got)
		}
	}
}

// TestUnique_RegeneratesOnCollision confirms that Unique skips names
// already in `used`. We seed `used` with the first half of all names;
// Unique must return one of the available second half within maxAttempts.
//
// Why first-half rather than "all but one": with maxAttempts=100 random
// draws from 3600 names, the probability of hitting a single specific
// open slot is ~3% — flaky test. Half-and-half makes success effectively
// certain (1 - 0.5^100), and still proves "Unique avoids the used set."
func TestUnique_RegeneratesOnCollision(t *testing.T) {
	t.Parallel()
	all := namegen.All()
	half := len(all) / 2
	used := all[:half]

	got, ok := namegen.Unique(used)
	if !ok {
		t.Fatalf("Unique returned ok=false; %d available slots", len(all)-half)
	}
	if contains(used, got) {
		t.Errorf("Unique returned %q which IS in used set", got)
	}
	if !namegen.IsValid(got) {
		t.Errorf("Unique returned %q which IsValid rejected", got)
	}
}

// TestUnique_ExhaustionReturnsFalse confirms the "wordlist full" failure
// path: when every possible name is in `used`, Unique returns ok=false.
func TestUnique_ExhaustionReturnsFalse(t *testing.T) {
	t.Parallel()
	used := namegen.All()
	got, ok := namegen.Unique(used)
	if ok {
		t.Errorf("Unique with full wordlist: got (%q, true); want (\"\", false)", got)
	}
	if got != "" {
		t.Errorf("Unique with full wordlist: name = %q; want empty string", got)
	}
}

// TestSpace verifies the documented invariant: Space() == |All()| ==
// |adjectives| * |nouns|.
func TestSpace(t *testing.T) {
	t.Parallel()
	if got, want := namegen.Space(), len(namegen.All()); got != want {
		t.Errorf("Space() = %d; len(All()) = %d", got, want)
	}
	if namegen.Space() < 1000 {
		t.Errorf("Space() = %d; want at least 1000 for realistic uniqueness", namegen.Space())
	}
}

// TestIsValid covers the shape-checker. Generated names pass; arbitrary
// strings (including hyphenated ones from outside the wordlists) fail.
func TestIsValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"generated name", "bold-falcon", true},
		{"another generated", "silent-otter", true},
		{"branch-shaped but not generated", "feature-oauth", false},
		{"no hyphen", "boldfalcon", false},
		{"empty", "", false},
		{"only hyphen", "-", false},
		{"adj only", "bold-", false},
		{"noun only", "-falcon", false},
		{"unknown adj", "fake-falcon", false},
		{"unknown noun", "bold-thing", false},
		{"three parts collapses to first hyphen", "bold-falcon-extra", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := namegen.IsValid(tc.in); got != tc.want {
				t.Errorf("IsValid(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
