package agent

import "regexp"

// opencodeClassifier is a deliberate Unknown-returning stub. opencode's
// TUI markers haven't been dogfooded yet; until they are, the pane's
// badge column (⚡💤✋·) will render `·` for opencode workspaces — which
// is honest: canopy can't classify what it hasn't observed. Better than
// a wrong badge.
//
// To fill in: spawn opencode in a real workspace, capture the boot
// state / idle state / approval state (opencode's tool-call permission
// has a different shape than codex's per @docs), save raw captures as
// internal/agent/testdata/opencode_*.txt, replace the nil slices with
// real regex patterns, and ship a classifier_opencode_test.go pair.
//
// Tracking: TODOS.md "OPEN — v0.16.x — Extend --prompt / background
// workspaces to codex + opencode" — opencode is the wave-2 launcher
// after codex parity ships.
type opencodeClassifier struct{}

func (opencodeClassifier) IdleMarkers() []*regexp.Regexp     { return nil }
func (opencodeClassifier) AwaitingMarkers() []*regexp.Regexp { return nil }
func (opencodeClassifier) IsRendering(content string) bool   { return false }
func (opencodeClassifier) IsTrustDialog(content string) bool { return false }
