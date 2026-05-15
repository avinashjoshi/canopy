package main

import (
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestTruncateForWidth(t *testing.T) {
	cases := []struct {
		name string
		s    string
		cols int
		want string
	}{
		{"fits exact", "hello", 5, "hello"},
		{"fits with room", "hi", 10, "hi"},
		{"truncates with ellipsis", "hello-world", 8, "hello-w…"},
		{"empty input passes through", "", 5, ""},
		{"zero cols returns empty", "anything", 0, ""},
		{"negative cols returns empty", "anything", -3, ""},
		{"cols=1 returns just the ellipsis", "anything", 1, "…"},
		{"cols=2 fits one rune + ellipsis", "abc", 2, "a…"},
		// Wide-character cases: each Hiragana char is 2 cols wide.
		{"wide chars fit", "あい", 4, "あい"},
		{"wide chars truncate", "あいうえお", 5, "あい…"},
		{"wide chars truncate with budget rounding", "あいう", 4, "あ…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateForWidth(tc.s, tc.cols)
			if got != tc.want {
				t.Errorf("truncateForWidth(%q, %d) = %q; want %q (got width %d, want width %d)",
					tc.s, tc.cols, got, tc.want,
					runewidth.StringWidth(got), runewidth.StringWidth(tc.want))
			}
		})
	}
}

func TestInitialsForBranch(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"clear-workspace-identity", "cwi"},
		{"feat/oauth", "fo"},
		{"feat/oauth-flow", "fof"},
		{"main", "m"},
		{"", ""},
		{"-leading-hyphen", "lh"},                     // leading separator → first segment starts after it
		{"trailing-hyphen-", "th"},                    // trailing separator → no extra letter
		{"snake_case", "s"},                           // underscore NOT a separator
		{"v1.2.3", "v"},                               // dot NOT a separator (single token)
		{"feat/multi-segment-branch-name", "fmsbn"},   // mix of - and /
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := initialsForBranch(tc.in)
			if got != tc.want {
				t.Errorf("initialsForBranch(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRenderWorkspaceSegment covers the proportional-truncation behavior
// added by /plan-design-review C2-revised: when wsName and branch differ,
// they share the truncation budget so neither piece vanishes until the
// overall budget drops below dropThreshold. When wsName == branch (or
// either is empty), this helper delegates to renderBranchSegment so the
// single-identifier tier behavior stays unchanged.
func TestRenderWorkspaceSegment(t *testing.T) {
	cases := []struct {
		name      string
		wsName    string
		branch    string
		availCols int
		want      string
	}{
		// Both empty → empty segment.
		{"both_empty", "", "", 80, ""},

		// Single-name cases delegate to renderBranchSegment.
		{"branch_only", "", "fix-bug", 60, " / fix-bug"},
		{"wsname_only", "robust-otter", "", 60, " / robust-otter"},
		{"wsname_eq_branch", "fix-bug", "fix-bug", 60, " / fix-bug"},

		// Full render when both pieces comfortably fit.
		{"both_distinct_fit", "robust-otter", "tmux-stat", 80,
			" / robust-otter / tmux-stat"},
		{"both_distinct_fit_exact", "rust", "go", 60, " / rust / go"},

		// Proportional truncation in the wide tier when full doesn't fit.
		// Budget 50 cols, sep*2 = 6 → nameBudget = 44. wsName=12, branch=36,
		// totalW=48. wsShare = 44*12/48 = 11; brShare = 44-11 = 33. After
		// truncateForWidth (budget-1 then "…"), the segment fits the full
		// 50-col budget exactly: 11 + 6 + 33 = 50.
		{"proportional_at_50", "robust-otter", "tmux-statusline-remote-local-context", 50,
			" / robust-ott… / tmux-statusline-remote-local-con…"},

		// Tight enough to need initials tier — both sides collapse.
		{"initials_at_35", "robust-otter", "tmux-statusline-remote-local-context", 35,
			" / ro / tsrlc"},
		{"initials_at_30", "robust-otter", "fix-bug", 30,
			" / ro / fb"},

		// Under dropThreshold: drop everything.
		{"drop_at_29", "robust-otter", "tmux-statusline-remote-local-context", 29, ""},
		{"drop_at_0", "robust-otter", "fix-bug", 0, ""},
		{"drop_at_negative", "robust-otter", "fix-bug", -10, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderWorkspaceSegment(tc.wsName, tc.branch, tc.availCols)
			if got != tc.want {
				t.Errorf("renderWorkspaceSegment(%q, %q, %d) = %q; want %q",
					tc.wsName, tc.branch, tc.availCols, got, tc.want)
			}
		})
	}
}

func TestRenderBranchSegment(t *testing.T) {
	cases := []struct {
		name      string
		branch    string
		availCols int
		want      string
	}{
		// Full render — branch fits comfortably.
		{"full render", "fix-bug", 60, " / fix-bug"},
		{"full render long branch big budget", "clear-workspace-identity", 80, " / clear-workspace-identity"},

		// 40-59: right-ellipsis truncation when the branch doesn't fit.
		// Tier behavior is "budget, not forced format" — short branches
		// that fit render in full even in middle tiers.
		{"branch fits at 50, no truncation", "clear-workspace-identity", 50, " / clear-workspace-identity"},
		// 5-char branch in 50 cols easily fits — the 47-budget after sep
		// matches the 5-char branch, no ellipsis.
		{"short branch large budget", "fix-x", 50, " / fix-x"},
		// Long branch forced to truncate. availCols 50 - sep(3) = 47 budget.
		// truncateForWidth budget = 47-1 = 46 runes, then "…". 49-char input
		// truncates to 46 + ellipsis.
		{"long branch truncates at 50",
			"really-long-branch-name-that-overflows-the-budget", 50,
			" / really-long-branch-name-that-overflows-the-bud…"},
		// availCols 40 - sep(3) = 37 budget; 36 runes + ellipsis on 38-char input.
		{"long branch truncates at 40",
			"really-long-branch-name-that-overflows", 40,
			" / really-long-branch-name-that-overflo…"},

		// 30-39: initials.
		{"initials at 35", "clear-workspace-identity", 35, " / cwi"},
		{"initials at 30", "clear-workspace-identity", 30, " / cwi"},
		{"initials feat slash", "feat/oauth-flow", 35, " / fof"},

		// <30: drop entirely.
		{"drop at 29", "clear-workspace-identity", 29, ""},
		{"drop at 0", "clear-workspace-identity", 0, ""},
		{"drop at negative", "clear-workspace-identity", -5, ""},

		// Empty branch: never render anything regardless of budget.
		{"empty branch huge budget", "", 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderBranchSegment(tc.branch, tc.availCols)
			if got != tc.want {
				t.Errorf("renderBranchSegment(%q, %d) = %q; want %q", tc.branch, tc.availCols, got, tc.want)
			}
		})
	}
}
