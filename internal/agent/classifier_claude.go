package agent

import "regexp"

// claudeClassifier is the Classifier implementation for the claude
// launcher. It's a thin adapter over the existing package-level
// claudeIdleMarkers / claudeAwaitingPatterns slices and the
// IsClaudeRendering / IsTrustDialog helper functions in state.go —
// which are kept as deprecated package-level shims so that callers
// outside this package (workspace/initprompt.go etc.) keep compiling
// while the Classifier interface is the going-forward dispatch.
//
// Behavior MUST be byte-identical to pre-classifier-refactor canopy:
// every method here either returns the existing pattern slice
// unchanged or calls the existing helper unchanged. The existing
// claude E2E and unit tests are the regression pin (see CLAUDE.md's
// test-discipline section — a regression in claude classification
// breaks the most-used path).
//
// Why the wrapper instead of moving the patterns into this file:
// the patterns are tightly coupled to normalize() (state.go) and
// the bottomLines helper (state.go); moving them risks churn in the
// /diff for the same observable behavior. The hybrid interface +
// data-table convention (see the D2 design call in the codex-support
// design doc) explicitly anticipates this — patterns are data, the
// interface is the seam.
type claudeClassifier struct{}

func (claudeClassifier) IdleMarkers() []*regexp.Regexp {
	return claudeIdleMarkers
}

func (claudeClassifier) AwaitingMarkers() []*regexp.Regexp {
	return claudeAwaitingPatterns
}

func (claudeClassifier) IsRendering(content string) bool {
	return IsClaudeRendering(content)
}

func (claudeClassifier) IsTrustDialog(content string) bool {
	return IsTrustDialog(content)
}
