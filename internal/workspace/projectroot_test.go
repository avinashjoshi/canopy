package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// TestMain lives in lifecycle_test.go for this package.

// TestResolveCurrentProject_workspaceMatch: cwd inside a workspace's on-disk
// Path returns that workspace's canonical ProjectRoot, NOT the workspace dir.
// This is the user-reported "no local project" bug: pressing <prefix>g from
// inside silent-falcon used to walk up to the workspace's own canopy.json
// and return the workspace dir as project root, which matched no rows.
//
// Ported from cmd/canopy/popup_inner_resolve_test.go on the move from
// cmd/canopy/popup_inner.go to internal/workspace/projectroot.go.
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
			got := workspace.ResolveCurrentProject(tc.cwd, st)
			if got != tc.want {
				t.Errorf("ResolveCurrentProject(%q) = %q; want %q", tc.cwd, got, tc.want)
			}
		})
	}
}

// TestResolveCurrentProject_nilState: nil state yields empty string, not panic.
func TestResolveCurrentProject_nilState(t *testing.T) {
	if got := workspace.ResolveCurrentProject("/some/path", nil); got != "" {
		t.Errorf("nil state: got %q; want empty", got)
	}
}

// TestResolveCurrentProject_workspacePathPrefix_falsePositive: a cwd that
// *starts* with a workspace path string but isn't actually inside it (e.g.,
// sibling dir with a similar prefix) must NOT match. The prefix check is
// path-segment-aware (trailing slash).
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
	cwd := "/home/avi/.canopy/workspaces/canopy/silent-falcon"
	if got := workspace.ResolveCurrentProject(cwd, st); got != "" {
		t.Errorf("prefix false-positive: cwd=%q matched ws.Path=%q; got %q",
			cwd, st.Workspaces[0].Path, got)
	}
}

// TestResolveCurrentProject_symlinkedCwd is the D8 fix: when cwd is a symlink
// to a registered workspace, EvalSymlinks resolves it before the prefix
// match. Without this, the user gets an empty Local tab from inside what's
// clearly a known workspace.
//
// Real filesystem test (not table-driven against fake paths) because the
// behavior under test is filepath.EvalSymlinks, which can't be stubbed.
func TestResolveCurrentProject_symlinkedCwd(t *testing.T) {
	tmp := t.TempDir()
	// Real workspace dir, then a symlink pointing at it.
	realWs := filepath.Join(tmp, "real-workspace")
	if err := os.MkdirAll(realWs, 0o755); err != nil {
		t.Fatalf("mkdir realWs: %v", err)
	}
	link := filepath.Join(tmp, "linked-workspace")
	if err := os.Symlink(realWs, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Resolve the parent tmpdir's symlinks too (macOS /var → /private/var).
	// Otherwise the state's ws.Path won't match the resolved cwd.
	resolvedTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("eval tmp: %v", err)
	}

	st := &state.State{
		Workspaces: []state.Workspace{{
			Name:        "ws",
			Path:        filepath.Join(resolvedTmp, "real-workspace"),
			ProjectRoot: "/canonical/project/root",
		}},
	}

	got := workspace.ResolveCurrentProject(link, st)
	want := "/canonical/project/root"
	if got != want {
		t.Errorf("symlinked cwd: got %q; want %q (EvalSymlinks should resolve %q → %q)",
			got, want, link, realWs)
	}
}

// TestResolveCurrentWorkspace covers the workspace-name resolver used by
// the unified TUI to pre-select the row whose dir hosts the popup. Hits
// match by workspace Path prefix; misses (cwd outside any workspace,
// nil state) yield empty results.
func TestResolveCurrentWorkspace(t *testing.T) {
	st := &state.State{
		Workspaces: []state.Workspace{
			{
				Name:        "regal-pine",
				Path:        "/home/avi/.canopy/workspaces/canopy/regal-pine",
				ProjectRoot: "/home/avi/Work/canopy",
			},
			{
				Name:        "fierce-salmon",
				Path:        "/home/avi/.canopy/workspaces/cravd/fierce-salmon",
				ProjectRoot: "/home/avi/Work/cravd",
			},
		},
	}

	cases := []struct {
		name        string
		cwd         string
		wantRoot    string
		wantWsName  string
	}{
		{
			name:       "exact workspace path",
			cwd:        "/home/avi/.canopy/workspaces/canopy/regal-pine",
			wantRoot:   "/home/avi/Work/canopy",
			wantWsName: "regal-pine",
		},
		{
			name:       "subdir of workspace",
			cwd:        "/home/avi/.canopy/workspaces/canopy/regal-pine/internal/ui",
			wantRoot:   "/home/avi/Work/canopy",
			wantWsName: "regal-pine",
		},
		{
			name:       "different project workspace",
			cwd:        "/home/avi/.canopy/workspaces/cravd/fierce-salmon",
			wantRoot:   "/home/avi/Work/cravd",
			wantWsName: "fierce-salmon",
		},
		{
			name:       "outside any workspace",
			cwd:        "/home/avi/Work/canopy",
			wantRoot:   "",
			wantWsName: "",
		},
		{
			name:       "prefix collision (sibling dir)",
			cwd:        "/home/avi/.canopy/workspaces/canopy/regal-pine-sibling",
			wantRoot:   "",
			wantWsName: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRoot, gotName := workspace.ResolveCurrentWorkspace(tc.cwd, st)
			if gotRoot != tc.wantRoot || gotName != tc.wantWsName {
				t.Errorf("ResolveCurrentWorkspace(%q) = (%q, %q); want (%q, %q)",
					tc.cwd, gotRoot, gotName, tc.wantRoot, tc.wantWsName)
			}
		})
	}
}

// TestResolveCurrentWorkspace_nilState: nil state yields ("", "")
// without panicking.
func TestResolveCurrentWorkspace_nilState(t *testing.T) {
	root, name := workspace.ResolveCurrentWorkspace("/anywhere", nil)
	if root != "" || name != "" {
		t.Errorf("nil state: got (%q, %q); want (\"\", \"\")", root, name)
	}
}
