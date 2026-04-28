// Package namegen produces random workspace names of the form
// "<adjective>-<noun>" (e.g., bold-falcon, silent-otter, swift-comet).
//
// The wordlists are short on purpose: 60 × 60 = 3600 combinations is
// plenty for one developer's worth of in-flight workspaces. The names are
// pleasant to read, easy to type, and never collide with real branch
// names (they don't look like "feat/" or "fix-" patterns).
//
// Names are NOT cryptographically random. They use math/rand/v2 with the
// runtime's default source, which is fine for "give me a memorable label"
// and inappropriate for anything else.
package namegen

import (
	"math/rand/v2"
	"strings"
)

// adjectives is curated for readability and tone. Avoids negative-sounding
// words (no "broken", "failing", "anxious") because workspace names are
// what you stare at all day.
var adjectives = []string{
	"amber", "ancient", "azure", "bold", "brave", "bright", "calm",
	"clever", "cosmic", "crimson", "crisp", "curious", "dapper", "deft",
	"distant", "eager", "early", "fair", "fierce", "free", "gentle",
	"glad", "golden", "graceful", "happy", "hardy", "honest", "humble",
	"jolly", "keen", "kind", "lively", "lucky", "merry", "mighty",
	"misty", "noble", "nimble", "patient", "peaceful", "polite", "proud",
	"quick", "quiet", "rapid", "ready", "regal", "robust", "royal",
	"rustic", "sharp", "silent", "silver", "smart", "smooth", "solar",
	"steady", "still", "stout", "subtle", "sunny", "sweet", "swift",
	"tame", "tidy", "tranquil", "trusty", "vibrant", "warm", "wild",
	"wise", "young", "zealous",
}

// nouns lean toward animals and nature for the same readability reason —
// short words, distinctive sounds, easy to remember which workspace is
// which when you've got five open at once.
var nouns = []string{
	"alder", "ash", "aspen", "badger", "bear", "beaver", "birch",
	"buffalo", "cedar", "cheetah", "comet", "condor", "coyote", "crane",
	"cypress", "deer", "dolphin", "dove", "eagle", "elk", "ember",
	"falcon", "fawn", "fern", "finch", "firefly", "fox", "frost",
	"glacier", "hare", "hawk", "heron", "hickory", "horizon", "hornet",
	"ivy", "jay", "lark", "lichen", "lion", "lynx", "magnolia",
	"maple", "marlin", "marsh", "meadow", "moose", "moth", "mountain",
	"oak", "ocelot", "orca", "osprey", "otter", "owl", "panda",
	"pine", "puma", "raven", "river", "robin", "salmon", "sequoia",
	"sparrow", "spruce", "stag", "stork", "swan", "tern", "thrush",
	"tiger", "trout", "tundra", "vale", "valley", "willow", "wolf",
	"wren",
}

// Generate returns a fresh "adjective-noun" name using the package-level
// random source. Two calls with the same source state will produce the
// same name; callers that care about uniqueness across an existing set
// should use Unique instead.
func Generate() string {
	a := adjectives[rand.IntN(len(adjectives))]
	n := nouns[rand.IntN(len(nouns))]
	return a + "-" + n
}

// Unique returns a name that is not present in the used slice. It tries
// up to maxAttempts times before giving up; with a 3600-name space and
// realistic workspace counts (<100), collisions are vanishingly rare and
// maxAttempts=100 is more than sufficient.
//
// Returns the name plus a boolean: true on success, false if maxAttempts
// were exhausted (which means the wordlist is effectively full — call
// site should error rather than fall back to a numeric suffix).
func Unique(used []string) (string, bool) {
	const maxAttempts = 100

	taken := make(map[string]struct{}, len(used))
	for _, u := range used {
		taken[u] = struct{}{}
	}

	for i := 0; i < maxAttempts; i++ {
		candidate := Generate()
		if _, conflict := taken[candidate]; !conflict {
			return candidate, true
		}
	}
	return "", false
}

// Space returns the total number of distinct names this package can
// produce (|adjectives| × |nouns|). Useful for tests and for "ran out
// of names" diagnostics.
func Space() int {
	return len(adjectives) * len(nouns)
}

// All returns every possible name. Memory cost is small (~3600 strings)
// and only used by tests; production code should never call this.
func All() []string {
	out := make([]string, 0, Space())
	for _, a := range adjectives {
		for _, n := range nouns {
			out = append(out, a+"-"+n)
		}
	}
	return out
}

// IsValid reports whether s could plausibly be a generated name (i.e.,
// matches the "adj-noun" shape using the embedded wordlists). Used by
// reconciliation to recognize canopy-generated names vs user-supplied
// --name overrides — the distinction matters for some future v0.5 features.
func IsValid(s string) bool {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return false
	}
	adjSet := setOf(adjectives)
	nounSet := setOf(nouns)
	_, aok := adjSet[parts[0]]
	_, nok := nounSet[parts[1]]
	return aok && nok
}

// setOf is a tiny helper that turns a slice into a set for O(1) lookups.
// Kept private; the lists are small enough that constructing this on
// every IsValid call is fine.
func setOf(words []string) map[string]struct{} {
	out := make(map[string]struct{}, len(words))
	for _, w := range words {
		out[w] = struct{}{}
	}
	return out
}
