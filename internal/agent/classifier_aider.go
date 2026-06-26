package agent

import "regexp"

// aiderClassifier is a deliberate Unknown-returning stub. aider's TUI
// markers aren't dogfooded yet AND aider's interaction model is
// different enough (--yes-always vs. interactive permission flow per
// git-remote-presence) that the awaiting-markers regex set probably
// needs a different shape than claude/codex.
//
// Per the original codex-support design's "Sequencing" note: aider
// ships LAST, after we've used codex + opencode classifiers long
// enough to be sure the Classifier interface signature actually
// suits aider's "permission as flag, not as dialog" world. If it
// doesn't, the interface gets extended before aider patterns land.
//
// Until then: badge column renders `·` for aider workspaces (honest
// over a wrong-positive 💤).
//
// Tracking: TODOS.md "OPEN — v0.16.x — Extend --prompt / background
// workspaces to codex + opencode" — aider is wave 3.
type aiderClassifier struct{}

func (aiderClassifier) IdleMarkers() []*regexp.Regexp     { return nil }
func (aiderClassifier) AwaitingMarkers() []*regexp.Regexp { return nil }
func (aiderClassifier) IsRendering(content string) bool   { return false }
func (aiderClassifier) IsTrustDialog(content string) bool { return false }
