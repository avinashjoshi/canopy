package main

import (
	"testing"

	"github.com/oncactus/canopy/internal/state"
)

// TestResolveCurrentProject_workspaceMatch: cwd inside a workspace's
// on-disk Path returns that workspace's canonical ProjectRoot, NOT the
// workspace dir. This is the user-reported "no local project" bug:
// pressing <prefix>g from inside silent-falcon used to walk up to
// the workspace's own canopy.json and return the workspace dir as
// project root, which matched no rows.
func TestResolveCurrentProject_workspaceMatch(t *testing.T) {
	st := &state.State{
		Projects: map[string]state.ProjectMeta{
			"/home/avi/Work/canopy": {Root: "/home/avi/Work/canopy"},
		},
		Workspaces: []state.Workspace{
			{
				Name:        "silent-falcon",
				Path:        "/home/avi/.canopy/workspaces/canopy/silent-falcon",
				ProjectRoot: "/home/avi/Work/canopy",
			},
			{
				Name:        "misty-aspen",
				Path:        "/home/avi/.canopy/workspaces/canopy/misty-aspen",
				ProjectRoot: "/home/avi/Work/canopy",
			},
		},
	}

	cases := []struct {
		name string
		cwd  string
		want string
	}{
		{
			name: "exact_workspace_path",
			cwd:  "/home/avi/.canopy/workspaces/canopy/silent-falcon",
			want: "/home/avi/Work/canopy",
		},
		{
			name: "subdir_of_workspace",
			cwd:  "/home/avi/.canopy/workspaces/canopy/silent-falcon/internal/ui",
			want: "/home/avi/Work/canopy",
		},
		{
			name: "different_workspace_same_project",
			cwd:  "/home/avi/.canopy/workspaces/canopy/misty-aspen/cmd",
			want: "/home/avi/Work/canopy",
		},
		{
			name: "outside_any_workspace_or_project",
			cwd:  "/tmp",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCurrentProject(tc.cwd, st)
			if got != tc.want {
				t.Errorf("resolveCurrentProject(%q) = %q; want %q", tc.cwd, got, tc.want)
			}
		})
	}
}

// TestResolveCurrentProject_nilState: nil state yields empty string,
// not panic.
func TestResolveCurrentProject_nilState(t *testing.T) {
	if got := resolveCurrentProject("/some/path", nil); got != "" {
		t.Errorf("nil state: got %q; want empty", got)
	}
}

// TestResolveCurrentProject_workspacePathPrefix_falsePositive: a cwd
// that *starts* with a workspace path string but isn't actually inside
// it (e.g., sibling dir with a similar prefix) must NOT match. The
// prefix check must be path-segment-aware (use trailing slash).
func TestResolveCurrentProject_workspacePathPrefix_falsePositive(t *testing.T) {
	st := &state.State{
		Workspaces: []state.Workspace{
			{
				Path:        "/home/avi/.canopy/workspaces/canopy/silent",
				ProjectRoot: "/home/avi/Work/canopy",
			},
		},
	}
	// "silent-falcon" SHARES the prefix "silent" but is a different dir.
	// Without slash-aware matching, this would be a false positive.
	cwd := "/home/avi/.canopy/workspaces/canopy/silent-falcon"
	if got := resolveCurrentProject(cwd, st); got != "" {
		t.Errorf("prefix false-positive: cwd=%q matched ws.Path=%q; got %q",
			cwd, st.Workspaces[0].Path, got)
	}
}
