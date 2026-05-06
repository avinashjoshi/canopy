// Statusline collapse algorithm: how to render
// "canopy / <branch> <glyph> :<port> [DEV:<x>]" when there isn't enough
// horizontal room in tmux's status-right for the full string.
//
// Pure functions only — no globals, no I/O. The width-tier table and
// the truncation/initials/drop fallbacks were locked in by
// /plan-design-review D2 and live here so they can be unit-tested
// without spinning up a real tmux session.
//
// Width tiers (cols available after tmux's other status-right segments):
//
//	>=60 cols : canopy / clear-workspace-identity ● :40010
//	40-59 cols: canopy / clear-workspace-… ● :40010
//	30-39 cols: canopy / cwi ● :40010              (initials)
//	<30 cols  : canopy ● :40010                    (drop branch)
//
// Cut order under width pressure (DEV suffix and project name handled
// at the call site; this file owns the branch-rendering decisions):
//  1. DEV-suffix-when-redundant drops first
//  2. Right-ellipsis truncation of the branch
//  3. Initials fallback (`clear-workspace-identity` -> `cwi`)
//  4. Branch drops entirely
//
// All width measurement uses runewidth so East Asian wide chars and
// emoji don't push past the budget.
package main

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

const (
	// fullThreshold is the minimum cols at which the branch renders in
	// full (untruncated). Below this we right-ellipsis.
	fullThreshold = 60

	// initialsThreshold is the minimum cols at which the branch renders
	// as truncated text. Below this we collapse to first-letter-of-each-
	// segment initials so the branch identity stays meaningful even when
	// the budget is brutal.
	initialsThreshold = 40

	// dropThreshold is the minimum cols at which any branch information
	// renders. Below this we drop the branch entirely so project name +
	// glyph + port still survive.
	dropThreshold = 30
)

// truncateForWidth returns s right-truncated with `…` to fit within
// maxCols display columns. Uses runewidth so East-Asian wide chars
// (CJK) and emoji are sized correctly — `len(s)` and
// `utf8.RuneCountInString(s)` both lie about visible width.
//
// If s already fits, returns s unchanged.
// If maxCols <= 0, returns "".
// If maxCols is 1 (only room for the ellipsis itself), returns "…".
func truncateForWidth(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= maxCols {
		return s
	}
	if maxCols == 1 {
		return "…"
	}

	// Build up runes until the next one would push us past maxCols-1
	// (we reserve 1 column for the trailing ellipsis).
	budget := maxCols - 1
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := runewidth.RuneWidth(r)
		if used+w > budget {
			break
		}
		b.WriteRune(r)
		used += w
	}
	b.WriteRune('…')
	return b.String()
}

// initialsForBranch collapses a branch like "clear-workspace-identity"
// to "cwi" — first letter of each hyphen- or slash-separated segment.
// Used when the column budget is too tight for even a truncated branch
// to be meaningful.
//
// Empty input returns "". Single-segment branches like "main" return
// the first letter ("m") — better than nothing when even 3 cols matter.
//
// Both `-` and `/` are treated as separators because real branch names
// use both: `feat/oauth-flow` -> `fof`. Underscores are NOT separators
// (snake_case is one logical name).
func initialsForBranch(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	expectInitial := true
	for _, r := range s {
		if r == '-' || r == '/' {
			expectInitial = true
			continue
		}
		if expectInitial {
			b.WriteRune(r)
			expectInitial = false
		}
	}
	return b.String()
}

// renderBranchSegment returns the formatted "/ <branch>" segment for
// the available cols, or "" when even initials don't fit.
//
// availCols is the total budget the caller wants this segment to
// occupy, INCLUDING the leading " / " separator (3 cols). Caller
// computes availCols by subtracting fixed parts (project, glyph,
// port, optional DEV suffix) from the full budget.
//
// Tiers map directly to the constants at the top of this file:
//
//	availCols >= fullThreshold     -> " / <full branch>"
//	initialsThreshold..fullThreshold -> " / <truncated…>"
//	dropThreshold..initialsThreshold -> " / <initials>"
//	< dropThreshold                -> "" (drop the segment entirely)
func renderBranchSegment(branch string, availCols int) string {
	if branch == "" || availCols < dropThreshold {
		return ""
	}
	const sep = " / "
	sepCols := runewidth.StringWidth(sep)

	if availCols >= fullThreshold && runewidth.StringWidth(branch)+sepCols <= availCols {
		return sep + branch
	}
	if availCols >= initialsThreshold {
		return sep + truncateForWidth(branch, availCols-sepCols)
	}
	// initialsThreshold..dropThreshold: collapse to initials. If the
	// initials are still too wide (CJK punctuation, emoji-only branches),
	// truncate them too.
	ini := initialsForBranch(branch)
	if runewidth.StringWidth(ini)+sepCols > availCols {
		return sep + truncateForWidth(ini, availCols-sepCols)
	}
	return sep + ini
}
