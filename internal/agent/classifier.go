package agent

import "regexp"

// Classifier owns the launcher-specific surfaces that drive the agent
// pane state machine:
//
//   - IdleMarkers / AwaitingMarkers — regex slices the Detector matches
//     against RAW captured content (not normalize'd) to recognize
//     launcher-specific UI states. Pattern matching uses raw because
//     normalize() strips footer/spinner/input-prompt lines that the
//     idle/awaiting markers usually live IN.
//
//   - IsRendering — "is the pane at the agent's own UI right now (vs.
//     a fallback shell, vs. mid-spawn)?" Used as the Phase-3 settle
//     gate in initprompt.go's trust-dialog state machine. Each launcher
//     has its own marker characters (claude ❯, codex › + boxed banner,
//     aider >, opencode whatever) so a single regex doesn't work.
//
//   - IsTrustDialog — "is the pane currently showing the first-launch
//     trust/onboarding prompt that needs a key dismissal?" Returns
//     false for launchers without a trust-dialog concept; the
//     initprompt.go state machine treats a constant-false result as
//     "no trust phase, skip Phase-1's dismissal branch."
//
// One Classifier per registered launcher type. ClassifierFor returns
// an unknownClassifier for unregistered or empty-string launchers; the
// nil-slice convention below keeps that safe.
//
// Why an interface and not pure data (pattern tables keyed by launcher
// string): codex's settle-state isn't a single regex match, it's "is
// the boxed banner present AND no spinner line." Behavior that's more
// than a regex needs Go code; the data lives in unexported pattern
// slices each implementer carries. Hybrid is more code than pure data
// but less than a hand-rolled per-launcher branch in state.go.
type Classifier interface {
	// IdleMarkers and AwaitingMarkers are matched against RAW pane
	// content (the captured tmux pane string). A nil or empty slice
	// is the canonical "no markers; nothing to match" return — the
	// Detector ranges over the slice, and Go's range yields zero
	// iterations on nil. Callers MUST NOT assume non-nil.
	IdleMarkers() []*regexp.Regexp
	AwaitingMarkers() []*regexp.Regexp

	// IsRendering reports whether the pane currently shows the
	// launcher's own UI (vs. a fallback shell or a transient init
	// state). Used as the --prompt Phase-3 settle gate AND by the
	// agent_state badge column to distinguish "agent rendered, just
	// no spinner" idle from "no agent at all (StateUnknown)" idle.
	IsRendering(content string) bool

	// IsTrustDialog reports whether the pane currently shows the
	// launcher's first-launch trust/consent prompt. Returns false for
	// launchers without a trust-dialog concept (opencode, aider). The
	// Phase-1 wait loop in initprompt.go uses this to decide whether
	// to send-keys an Enter to dismiss.
	IsTrustDialog(content string) bool
}

// ClassifierFor returns the Classifier for the given launcher type.
//
// Empty launcher → unknownClassifier (defensive; the Detector's caller
// passes the value from LauncherFromRole(roleTag), which is "" when the
// role tag is malformed — we don't want a malformed role to crash the
// state machine).
//
// Unregistered launcher → unknownClassifier (same default; we don't
// fail loudly here because the caller may legitimately ask about a
// launcher whose patterns haven't been dogfooded yet — opencode and
// aider land as Unknown-returning stubs deliberately).
//
// Lookup is a Go map access plus a one-line switch; no allocation. The
// returned Classifier is stateless and safe to share across goroutines.
func ClassifierFor(launcher string) Classifier {
	switch launcher {
	case "claude":
		return claudeClassifier{}
	case "codex":
		return codexClassifier{}
	case "opencode":
		return opencodeClassifier{}
	case "aider":
		return aiderClassifier{}
	}
	return unknownClassifier{}
}

// unknownClassifier is the "no markers registered, no idle / awaiting /
// trust signals" fallback. Returns nil slices and constant false. Used
// for unrecognized launchers AND as the placeholder for opencode/aider
// during the transitional period before their patterns ship.
//
// Two important invariants this enforces:
//
//  1. range over a nil regex slice yields zero iterations (Go spec) —
//     so the Detector's loop bodies are safe without a nil guard.
//
//  2. IsRendering/IsTrustDialog both return false — so unknown
//     launchers neither trip the Phase-3 settle gate (canopy refuses
//     to send-keys until SOMETHING claims the pane is the agent's UI)
//     NOR get treated as showing a trust dialog (canopy doesn't blindly
//     hammer Enter into an unknown pane). Defensive default for both.
type unknownClassifier struct{}

func (unknownClassifier) IdleMarkers() []*regexp.Regexp     { return nil }
func (unknownClassifier) AwaitingMarkers() []*regexp.Regexp { return nil }
func (unknownClassifier) IsRendering(content string) bool   { return false }
func (unknownClassifier) IsTrustDialog(content string) bool { return false }
