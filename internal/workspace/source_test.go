package workspace

import (
	"strings"
	"testing"
)

// TestParseSourceSpec covers the human-typed source string parser
// used by the TUI's new-workspace modal. CLI flags use the typed
// SourceSpec fields directly so don't go through this path.
func TestParseSourceSpec(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    SourceSpec
		wantErr string // substring; empty = expect no error
	}{
		{"empty → zero spec", "", SourceSpec{}, ""},
		{"whitespace → zero spec", "   ", SourceSpec{}, ""},

		{"pr space form", "pr 1234", SourceSpec{PR: 1234}, ""},
		{"pr colon form", "pr:1234", SourceSpec{PR: 1234}, ""},
		{"pr uppercase tolerance", "PR 1234", SourceSpec{PR: 1234}, ""},

		{"issue space", "issue 42", SourceSpec{Issue: 42}, ""},
		{"issue colon", "issue:42", SourceSpec{Issue: 42}, ""},

		{"branch simple", "branch feat/x", SourceSpec{Branch: "feat/x"}, ""},
		{"branch colon", "branch:feat/x", SourceSpec{Branch: "feat/x"}, ""},
		{"branch local modifier", "branch feat/x local", SourceSpec{Branch: "feat/x", AllowLocal: true}, ""},
		{"branch --allow-local modifier", "branch feat/x --allow-local", SourceSpec{Branch: "feat/x", AllowLocal: true}, ""},

		{"pr with non-numeric", "pr abc", SourceSpec{}, "positive integer"},
		{"pr negative", "pr -5", SourceSpec{}, "positive integer"},
		{"pr alone (no number)", "pr", SourceSpec{}, "needs an argument"},
		{"unknown kind", "wat 5", SourceSpec{}, "unknown source kind"},
		{"unknown branch modifier", "branch feat/x weird", SourceSpec{}, "unknown branch modifier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSourceSpec(tc.input)
			if tc.wantErr != "" {
				if err == nil {
					t.Errorf("expected error containing %q; got nil", tc.wantErr)
					return
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tc.want {
				t.Errorf("parse(%q) = %+v; want %+v", tc.input, got, tc.want)
			}
		})
	}
}

// TestSourceSpec_Validate covers the mutual-exclusion + dependency
// rules. Same shape the CLI's old validateNewFlags enforced; now
// owned by SourceSpec so both surfaces share one truth.
func TestSourceSpec_Validate(t *testing.T) {
	cases := []struct {
		name    string
		spec    SourceSpec
		wantErr string
	}{
		{"zero", SourceSpec{}, ""},
		{"pr only", SourceSpec{PR: 1}, ""},
		{"issue only", SourceSpec{Issue: 1}, ""},
		{"branch only", SourceSpec{Branch: "x"}, ""},
		{"branch + allow-local", SourceSpec{Branch: "x", AllowLocal: true}, ""},
		{"pr + issue", SourceSpec{PR: 1, Issue: 1}, "mutually exclusive"},
		{"pr + branch", SourceSpec{PR: 1, Branch: "x"}, "mutually exclusive"},
		{"issue + branch", SourceSpec{Issue: 1, Branch: "x"}, "mutually exclusive"},
		{"all three", SourceSpec{PR: 1, Issue: 1, Branch: "x"}, "mutually exclusive"},
		{"allow-local without branch", SourceSpec{AllowLocal: true}, "only makes sense with --branch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected nil; got %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected %q; got nil", tc.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestSourceSpec_IsZero confirms the zero-value detector that gates
// the resolver's fast-path (no source flags → no work needed).
func TestSourceSpec_IsZero(t *testing.T) {
	cases := []struct {
		spec SourceSpec
		want bool
	}{
		{SourceSpec{}, true},
		{SourceSpec{AllowLocal: true}, true}, // only meaningful with branch; bare allow-local doesn't count
		{SourceSpec{PR: 1}, false},
		{SourceSpec{Issue: 1}, false},
		{SourceSpec{Branch: "x"}, false},
	}
	for _, tc := range cases {
		if got := tc.spec.IsZero(); got != tc.want {
			t.Errorf("IsZero(%+v) = %v; want %v", tc.spec, got, tc.want)
		}
	}
}
