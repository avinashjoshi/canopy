package main

import (
	"strings"
	"testing"
)

// TestValidateNewFlags covers the mutual-exclusion + dependency checks
// for canopy new's source-variant flags. Pure flag-state validation;
// no gh shellouts or git ops involved.
func TestValidateNewFlags(t *testing.T) {
	cases := []struct {
		name    string
		setup   func()
		wantErr string // substring; empty means expect no error
	}{
		{
			name:    "no flags",
			setup:   func() { resetNewFlags() },
			wantErr: "",
		},
		{
			name: "pr alone",
			setup: func() {
				resetNewFlags()
				newWorkspaceFlags.pr = 42
			},
			wantErr: "",
		},
		{
			name: "issue alone",
			setup: func() {
				resetNewFlags()
				newWorkspaceFlags.issue = 17
			},
			wantErr: "",
		},
		{
			name: "branch alone",
			setup: func() {
				resetNewFlags()
				newWorkspaceFlags.branch = "feat/x"
			},
			wantErr: "",
		},
		{
			name: "branch + allow-local",
			setup: func() {
				resetNewFlags()
				newWorkspaceFlags.branch = "feat/x"
				newWorkspaceFlags.allowLoc = true
			},
			wantErr: "",
		},
		{
			name: "pr + issue rejected",
			setup: func() {
				resetNewFlags()
				newWorkspaceFlags.pr = 42
				newWorkspaceFlags.issue = 17
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "pr + branch rejected",
			setup: func() {
				resetNewFlags()
				newWorkspaceFlags.pr = 42
				newWorkspaceFlags.branch = "feat/x"
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "all three rejected",
			setup: func() {
				resetNewFlags()
				newWorkspaceFlags.pr = 42
				newWorkspaceFlags.issue = 17
				newWorkspaceFlags.branch = "feat/x"
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "allow-local without branch rejected",
			setup: func() {
				resetNewFlags()
				newWorkspaceFlags.allowLoc = true
			},
			wantErr: "--allow-local only makes sense with --branch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			err := validateNewFlags()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected nil error; got %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q; got nil", tc.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestDefaultBranchName covers the workspace-name fallback when
// --branch is supplied without --name. Branch basename wins; user-
// supplied name overrides.
func TestDefaultBranchName(t *testing.T) {
	cases := []struct {
		branch, userName, want string
	}{
		{"feature/oauth", "", "oauth"},
		{"main", "", "main"},
		{"feature/nested/deep", "", "deep"},
		{"feat", "", "feat"},
		{"feature/oauth", "my-name", "my-name"}, // user override wins
		{"", "", ""},
	}
	for _, tc := range cases {
		got := defaultBranchName(tc.branch, tc.userName)
		if got != tc.want {
			t.Errorf("defaultBranchName(%q, %q) = %q; want %q",
				tc.branch, tc.userName, got, tc.want)
		}
	}
}

// resetNewFlags zeroes the package-level newWorkspaceFlags so tests
// can compose their own combinations without leakage from earlier
// test runs (cobra's flag parsing would mutate the same vars).
func resetNewFlags() {
	newWorkspaceFlags.name = ""
	newWorkspaceFlags.noAttach = false
	newWorkspaceFlags.pr = 0
	newWorkspaceFlags.issue = 0
	newWorkspaceFlags.branch = ""
	newWorkspaceFlags.allowLoc = false
}
