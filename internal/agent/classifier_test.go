package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifierFor_RegistryLookup walks every known + every stub +
// the empty-string + a hand-crafted unknown launcher, and asserts the
// returned concrete type via Go's type switch. The point isn't the
// behavior (other tests cover that); it's that the dispatch table in
// ClassifierFor stays in sync as launchers are added.
func TestClassifierFor_RegistryLookup(t *testing.T) {
	cases := []struct {
		launcher  string
		typeCheck func(c Classifier) bool
		typeName  string
	}{
		{"claude", func(c Classifier) bool { _, ok := c.(claudeClassifier); return ok }, "claudeClassifier"},
		{"codex", func(c Classifier) bool { _, ok := c.(codexClassifier); return ok }, "codexClassifier"},
		{"opencode", func(c Classifier) bool { _, ok := c.(opencodeClassifier); return ok }, "opencodeClassifier"},
		{"aider", func(c Classifier) bool { _, ok := c.(aiderClassifier); return ok }, "aiderClassifier"},
		{"", func(c Classifier) bool { _, ok := c.(unknownClassifier); return ok }, "unknownClassifier"},
		{"future-gpt-pilot", func(c Classifier) bool { _, ok := c.(unknownClassifier); return ok }, "unknownClassifier"},
	}
	for _, tc := range cases {
		t.Run(tc.launcher, func(t *testing.T) {
			c := ClassifierFor(tc.launcher)
			if !tc.typeCheck(c) {
				t.Errorf("ClassifierFor(%q) returned %T; want %s", tc.launcher, c, tc.typeName)
			}
		})
	}
}

// TestClassifierFor_StubsReturnNilSlicesAndFalse pins the contract for
// stub + unknown launchers. The nil-slice convention is what lets the
// Detector range-iterate without a nil guard at every callsite — if a
// stub ever started returning a non-nil empty slice (which would be
// equivalent semantically but waste an allocation), that's a code-smell
// worth catching here.
func TestClassifierFor_StubsReturnNilSlicesAndFalse(t *testing.T) {
	for _, launcher := range []string{"", "opencode", "aider", "future-gpt-pilot"} {
		t.Run(launcher, func(t *testing.T) {
			c := ClassifierFor(launcher)
			if c.IdleMarkers() != nil {
				t.Errorf("IdleMarkers() = %v; want nil for stub %q", c.IdleMarkers(), launcher)
			}
			if c.AwaitingMarkers() != nil {
				t.Errorf("AwaitingMarkers() = %v; want nil for stub %q", c.AwaitingMarkers(), launcher)
			}
			if c.IsRendering("any content") {
				t.Errorf("IsRendering = true; want false for stub %q", launcher)
			}
			if c.IsTrustDialog("Do you trust the contents of this directory?") {
				t.Errorf("IsTrustDialog = true; want false for stub %q (even on text that LOOKS like a trust prompt)", launcher)
			}
		})
	}
}

// TestClassifierFor_NilSafeRangeIteration is the load-bearing safety
// guarantee: ranging over a nil regex slice yields zero iterations and
// does NOT panic. Without this, the Detector's loops in state.go would
// need an `if markers != nil` guard at every callsite. Pin it in a test
// so a refactor that switches to returning sentinel empty slices (which
// would also work) is a deliberate choice, not an accidental one.
func TestClassifierFor_NilSafeRangeIteration(t *testing.T) {
	c := ClassifierFor("opencode") // any stub
	count := 0
	for range c.IdleMarkers() {
		count++
	}
	for range c.AwaitingMarkers() {
		count++
	}
	if count != 0 {
		t.Errorf("ranged %d times over stub markers; want 0", count)
	}
}

// TestClassifierFor_ClaudeWrapsExistingPatterns proves the refactor is
// pure: claudeClassifier returns the SAME slice pointers (not just
// equal contents) as the package-level claudeIdleMarkers /
// claudeAwaitingPatterns vars. If a future refactor accidentally
// allocates new slices, the test catches it — that'd silently break
// any code that holds a long-lived reference to the package-level vars
// (none today, but the contract matters for the deprecated shims).
func TestClassifierFor_ClaudeWrapsExistingPatterns(t *testing.T) {
	c := ClassifierFor("claude")
	if &c.IdleMarkers()[0] != &claudeIdleMarkers[0] {
		t.Errorf("claudeClassifier.IdleMarkers() didn't return the package-level slice")
	}
	if &c.AwaitingMarkers()[0] != &claudeAwaitingPatterns[0] {
		t.Errorf("claudeClassifier.AwaitingMarkers() didn't return the package-level slice")
	}
}

// TestCodexClassifier_AgainstRealFixtures runs the codex classifier
// against the captures saved during the 2026-06-17 dogfood spike. The
// fixtures are the load-bearing ground truth for codex pattern
// correctness — when codex-cli ships a UI change that drifts the
// regexes, the fixtures get re-captured (see testdata/README.md) and
// the patterns are bumped together.
//
// Each row asserts the SEMANTIC outcome (what ClassifyOneShot returns,
// plus IsTrustDialog), not the per-marker hit. Per-marker asserts are
// brittle: the awaiting fixture still contains the codex banner at
// the top (so IdleMarkers also match), but ClassifyOneShot returns
// AwaitingInput because awaiting beats idle in match order. Testing
// the state, not the individual matchers, captures the real contract.
func TestCodexClassifier_AgainstRealFixtures(t *testing.T) {
	cases := []struct {
		fixture     string
		wantState   State
		wantTrust   bool
		wantRender  bool
	}{
		{
			// Trust dialog hides the banner from the bottom of the pane
			// and matches neither idle nor (post-tightening) awaiting
			// patterns. IsTrustDialog is the only positive signal.
			fixture:    "codex_trust_dialog.txt",
			wantState:  StateUnknown,
			wantTrust:  true,
			wantRender: false,
		},
		{
			fixture:    "codex_idle.txt",
			wantState:  StateIdle,
			wantTrust:  false,
			wantRender: true,
		},
		{
			// ClassifyOneShot can't see motion — it can only do pattern
			// matching. The thinking fixture's banner + footer still
			// match idle markers, so static classification falls into
			// Idle. The motion-based StateThinking only comes from
			// ClassifyTwoShot / Detector.Observe (see spinner-stripped
			// test below for that path).
			fixture:    "codex_thinking_a.txt",
			wantState:  StateIdle,
			wantTrust:  false,
			wantRender: true,
		},
		{
			// Awaiting dialog: ClassifyOneShot returns AwaitingInput
			// even though the banner at the top of the pane also
			// matches an idle marker — awaiting beats idle in order.
			fixture:    "codex_awaiting_input.txt",
			wantState:  StateAwaitingInput,
			wantTrust:  false,
			wantRender: false, // approval dialog takes over bottom 12 lines
		},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			content := readFixture(t, tc.fixture)
			if got := ClassifyOneShot("codex", content); got != tc.wantState {
				t.Errorf("ClassifyOneShot(codex, %s) = %v; want %v",
					tc.fixture, got, tc.wantState)
			}
			c := ClassifierFor("codex")
			if got := c.IsTrustDialog(content); got != tc.wantTrust {
				t.Errorf("IsTrustDialog(%s) = %v; want %v",
					tc.fixture, got, tc.wantTrust)
			}
			if got := c.IsRendering(content); got != tc.wantRender {
				t.Errorf("IsRendering(%s) = %v; want %v",
					tc.fixture, got, tc.wantRender)
			}
		})
	}
}

// TestCodexClassifier_ThinkingSpinnerStripped verifies that normalize()
// strips codex's `• Working (Ns • esc to interrupt)` line. The two
// thinking fixtures differ ONLY in the timer (1s vs 3s); after
// normalization, the rest of the content is identical, so the
// hash-based stability check in Detector.Observe stays stable across
// the second-by-second timer flips. Pre-refactor, this exact bug would
// have flagged codex as Thinking forever any time it was rendering its
// own UI (because the timer in the spinner line always changes).
func TestCodexClassifier_ThinkingSpinnerStripped(t *testing.T) {
	a := readFixture(t, "codex_thinking_a.txt")
	b := readFixture(t, "codex_thinking_b.txt")
	if a == b {
		t.Fatal("fixtures identical pre-normalize — captures may have been re-taken without motion")
	}
	if na, nb := normalize(a), normalize(b); na != nb {
		t.Errorf("normalize() didn't strip the codex spinner timer:\nA=%q\nB=%q", na, nb)
	}
}

// readFixture loads internal/agent/testdata/<name>. Centralized so the
// path lookup only lives in one place.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return strings.TrimRight(string(data), "\n")
}

