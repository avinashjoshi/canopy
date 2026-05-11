package tmux

import "testing"

// Pure unit test for the pane-ID validator. No tmux required.
// Regression coverage for the v0.16 adversarial-review finding that
// Create/SplitPane only checked for non-empty, not format — so a tmux
// hook printing to stdout (e.g. session-created display-message) could
// pollute the captured `#{pane_id}` and silently flow through.
func TestValidPaneID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"%0", true},
		{"%15", true},
		{"%999999", true},
		// invalid
		{"", false},
		{"%", false},
		{"15", false},
		{"%abc", false},
		{"%15abc", false},
		{"%15\n%16", false}, // multi-line stdout from a buggy hook
		{"%15 extra", false},
		{" %15", false},
		{"@15", false},
	}
	for _, tc := range cases {
		if got := validPaneID(tc.in); got != tc.want {
			t.Errorf("validPaneID(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}
