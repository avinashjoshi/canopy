package agent

import (
	"regexp"
	"strings"
)

// codexClassifier is the Classifier implementation for the codex
// launcher (the `codex` CLI from OpenAI's Codex project, NOT the
// /codex gstack skill or any other "codex" naming collision).
//
// Patterns below were dogfooded 2026-06-17 against codex-cli 0.140.0
// running model gpt-5.5. The literal pane captures used as fixtures
// live in internal/agent/testdata/codex_*.txt; classifier_codex_test.go
// asserts each State classification against those captures.
//
// Codex's UI shape vs. claude's, as of 2026-06:
//
//  - Idle marker: boxed banner `╭─...─╮ │ >_ OpenAI Codex (v... │ ╰─...─╯`.
//    The banner stays visible at the top of the pane between turns,
//    so matching it anywhere on screen is reliable. The footer
//    `gpt-5.5 default · <cwd>` ALSO marks idle but the model name is
//    going to change — banner is the more durable signal.
//
//  - Awaiting marker: numbered-option approval dialog ending in
//    `Press enter to confirm or esc to cancel`. Only fires when codex
//    is spawned with --ask-for-approval on-request|untrusted (default
//    mode auto-applies edits with no UI). canopy spawns codex with
//    on-request — see launchers.go's codex entry.
//
//  - Trust dialog: first-launch `Do you trust the contents of this
//    directory?` prompt with the same 1./2. numbered selector shape
//    as the awaiting dialog. The "Do you trust" prefix disambiguates.
//
//  - Spinner line: `• Working (Ns • esc to interrupt)`. The timer
//    increments every second; normalize() in state.go strips this line
//    so a stuck-mid-tool-call pane doesn't flip-flop the activity hash.
//    See spinnerLine regex (state.go) for the stripping.
//
// Same "last verified" convention as launchers.go's codex comment:
// when codex-cli ships a UI overhaul, re-capture fixtures, update the
// regexes, and bump the date.
type codexClassifier struct{}

// codexIdleMarkers are codex-only UI elements that prove the pane is
// rendering codex (not the keepAlive shell, not a stale scrollback).
// The boxed banner is the load-bearing match; the footer is a softer
// backup. Both stay visible between turns.
//
// Last verified: 2026-06-17 against codex-cli 0.140.0.
var codexIdleMarkers = []*regexp.Regexp{
	regexp.MustCompile(`>_ OpenAI Codex \(v`),       // banner header
	regexp.MustCompile(`/model to change`),           // banner row
	regexp.MustCompile(`(?m)^\s+gpt-[\d.]+\s+\w+\s+·\s+`), // footer (model · cwd)
}

// codexAwaitingPatterns are codex-TUI markers that mean a user action
// is required RIGHT NOW (approve/deny a proposed action). Only fires
// in --ask-for-approval on-request|untrusted modes; canopy uses
// on-request by default (see launchers.go).
//
// The numbered-Yes-option pattern (`› 1. Yes,`) is intentionally NOT
// here even though it appears in awaiting dialogs — codex's trust
// dialog uses the same numbered selector with `› 1. Yes, continue`,
// and we don't want to flag the trust state as "awaiting input"
// (canopy auto-dismisses trust dialogs; awaiting needs user action).
// The footer + edit-prefix patterns below are specific to the
// approval-required-action dialog and don't collide with trust.
//
// Last verified: 2026-06-17 against codex-cli 0.140.0.
var codexAwaitingPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Press enter to confirm or esc to cancel`),     // approval dialog footer
	regexp.MustCompile(`Would you like to make the following edits\?`), // file-edit approval prefix
}

func (codexClassifier) IdleMarkers() []*regexp.Regexp     { return codexIdleMarkers }
func (codexClassifier) AwaitingMarkers() []*regexp.Regexp { return codexAwaitingPatterns }

// IsRendering is the Phase-3 settle gate for codex panes. Codex's
// idle markers live in the TOP portion of the visible pane (boxed
// banner at rows 1-6, footer at row ~15) — unlike claude, whose input
// chevron + auto-mode footer sit at the bottom. So we can't use the
// bottomLines helper claude uses; we match against the full visible
// content, after trimming trailing blank rows.
//
// Why trim before matching: tmux `capture-pane -p` returns the
// VISIBLE pane verbatim, including blank rows that codex hasn't drawn
// into. A typical 50-row pane with codex content in rows 1-15 has 35
// trailing blanks. Without the trim, bottomLines(content, 12) would
// return 12 blank lines and the markers would never match — which is
// exactly the bug ultrareview caught 2026-06-26 (bug_008). The unit
// test masked it because `readFixture` strips trailing blanks before
// classification, so the test saw the trimmed shape while production
// saw the raw shape.
//
// Why no scrollback-false-positive concern: CapturePane uses `-p`
// without `-S`, so we only see the VISIBLE pane. A crashed-to-shell
// codex doesn't show its banner anywhere in the visible area — the
// banner is gone the moment the codex process exits, replaced by
// whatever the shell renders.
func (codexClassifier) IsRendering(content string) bool {
	// Strip trailing blank rows the visible pane preserves.
	trimmed := strings.TrimRight(content, "\n\t ")
	for _, p := range codexIdleMarkers {
		if p.MatchString(trimmed) {
			return true
		}
	}
	return false
}

// codexTrustDialogPattern matches codex's first-launch consent prompt
// for the working directory. The "Do you trust" prefix is highly
// specific; it can't collide with codex's other numbered-selector
// dialogs (file-edit approval, /model picker, etc.).
//
// Last verified: 2026-06-17 against codex-cli 0.140.0.
var codexTrustDialogPattern = regexp.MustCompile(`Do you trust the contents of this directory\?`)

func (codexClassifier) IsTrustDialog(content string) bool {
	return codexTrustDialogPattern.MatchString(content)
}
