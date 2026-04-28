package state

import (
	"context"
	"errors"
	"testing"
)

// fakeProbe is a deterministic LivenessProbe for tests. Maps session name
// to (alive, err). Unknown names return (false, nil) — i.e. dead.
type fakeProbe struct {
	alive map[string]bool
	err   error // if non-nil, returned for ALL queries (simulates daemon down)
}

func (p *fakeProbe) HasSession(ctx context.Context, name string) (bool, error) {
	if p.err != nil {
		return false, p.err
	}
	return p.alive[name], nil
}

// TestBuildGlobalRows_Empty: zero state → empty slice, no panic.
func TestBuildGlobalRows_Empty(t *testing.T) {
	s := &State{}
	rows := s.BuildGlobalRows(context.Background(), &fakeProbe{})
	if len(rows) != 0 {
		t.Errorf("empty state → %d rows, want 0", len(rows))
	}
}

// TestBuildGlobalRows_WorkspacesOnly: state with workspaces but no main
// sessions alive should emit only workspace rows.
func TestBuildGlobalRows_WorkspacesOnly(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/a/cravd": {Root: "/a/cravd", PortBase: 3000},
		},
		Workspaces: []Workspace{
			{ProjectRoot: "/a/cravd", Project: "cravd", Name: "soft-fox", TmuxSession: "cravd-soft-fox", Status: StatusReady, Port: 3000, Branch: "feat/y"},
			{ProjectRoot: "/a/cravd", Project: "cravd", Name: "bold-falcon", TmuxSession: "cravd-bold-falcon", Status: StatusReady, Port: 3001, Branch: "feat/x"},
		},
	}
	probe := &fakeProbe{alive: map[string]bool{
		"cravd-soft-fox":    true,
		"cravd-bold-falcon": false, // dead
	}}

	rows := s.BuildGlobalRows(context.Background(), probe)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Sorted by name within project: bold-falcon comes before soft-fox.
	if rows[0].Name != "bold-falcon" || rows[1].Name != "soft-fox" {
		t.Errorf("unexpected order: %q, %q", rows[0].Name, rows[1].Name)
	}
	// Liveness reflected.
	if rows[0].Alive {
		t.Errorf("bold-falcon should be Alive=false")
	}
	if !rows[1].Alive {
		t.Errorf("soft-fox should be Alive=true")
	}
	// Project basename derived from root path.
	if rows[0].Project != "cravd" || rows[0].ProjectRoot != "/a/cravd" {
		t.Errorf("project fields: got Project=%q, ProjectRoot=%q", rows[0].Project, rows[0].ProjectRoot)
	}
}

// TestBuildGlobalRows_MainOnly: a project with no workspaces but an alive
// main session should emit only the synthetic main row.
func TestBuildGlobalRows_MainOnly(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/a/cravd": {Root: "/a/cravd", PortBase: 3000},
		},
	}
	probe := &fakeProbe{alive: map[string]bool{"cravd-main": true}}

	rows := s.BuildGlobalRows(context.Background(), probe)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if !rows[0].IsMain {
		t.Errorf("expected IsMain=true")
	}
	if rows[0].Name != "(main)" || rows[0].Status != "main" {
		t.Errorf("main row fields: name=%q status=%q", rows[0].Name, rows[0].Status)
	}
	if rows[0].Port != 3000 {
		t.Errorf("main row Port = %d, want 3000 (project's PortBase)", rows[0].Port)
	}
}

// TestBuildGlobalRows_Mixed: main + workspaces in two projects, sort order.
func TestBuildGlobalRows_Mixed(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/a/cravd":  {Root: "/a/cravd", PortBase: 3000},
			"/b/canopy": {Root: "/b/canopy", PortBase: 4000},
		},
		Workspaces: []Workspace{
			{ProjectRoot: "/b/canopy", Project: "canopy", Name: "ancient-hornet", TmuxSession: "canopy-ancient-hornet", Status: StatusReady},
			{ProjectRoot: "/a/cravd", Project: "cravd", Name: "bold-falcon", TmuxSession: "cravd-bold-falcon", Status: StatusReady},
		},
	}
	probe := &fakeProbe{alive: map[string]bool{
		"cravd-main":            true,
		"cravd-bold-falcon":     true,
		"canopy-ancient-hornet": false,
	}}

	rows := s.BuildGlobalRows(context.Background(), probe)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// Project sort: /a/cravd before /b/canopy. Within /a/cravd: main row first, then workspaces.
	want := []struct {
		project string
		name    string
		isMain  bool
	}{
		{"cravd", "(main)", true},
		{"cravd", "bold-falcon", false},
		{"canopy", "ancient-hornet", false},
	}
	for i, w := range want {
		if rows[i].Project != w.project || rows[i].Name != w.name || rows[i].IsMain != w.isMain {
			t.Errorf("row %d: got (%s, %s, isMain=%v); want (%s, %s, isMain=%v)",
				i, rows[i].Project, rows[i].Name, rows[i].IsMain, w.project, w.name, w.isMain)
		}
	}
}

// TestBuildGlobalRows_ProbeError: if HasSession returns errors for every
// call, every row should still render with Alive=false, no panic.
func TestBuildGlobalRows_ProbeError(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{"/a/cravd": {Root: "/a/cravd"}},
		Workspaces: []Workspace{
			{ProjectRoot: "/a/cravd", Project: "cravd", Name: "soft-fox", TmuxSession: "cravd-soft-fox"},
		},
	}
	probe := &fakeProbe{err: errors.New("tmux daemon down")}

	rows := s.BuildGlobalRows(context.Background(), probe)
	// Workspace row only (main session probe also errored, treated as dead).
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Alive {
		t.Errorf("Alive should be false when probe errors")
	}
}

// TestBuildGlobalRows_LegacyV1Workspace: a Workspace with only Project set
// (no ProjectRoot) should still render. The basename functions as the
// fallback key. Migration would have run already in production, but the
// listing must not crash if it sees a stale row.
func TestBuildGlobalRows_LegacyV1Workspace(t *testing.T) {
	s := &State{
		Workspaces: []Workspace{
			{Project: "cravd", Name: "bold-falcon", TmuxSession: "cravd-bold-falcon", Status: StatusReady},
		},
	}
	probe := &fakeProbe{}
	rows := s.BuildGlobalRows(context.Background(), probe)
	if len(rows) != 1 {
		t.Fatalf("legacy row got %d rows, want 1", len(rows))
	}
	if rows[0].Project != "cravd" {
		t.Errorf("legacy row Project: got %q", rows[0].Project)
	}
}

// TestSafeMainSessionName covers the basename → tmux session name
// transformation, mostly to lock in that we don't accidentally diverge
// from internal/tmux.SafeName for typical inputs.
func TestSafeMainSessionName(t *testing.T) {
	cases := []struct {
		basename, want string
	}{
		{"cravd", "cravd-main"},
		{"hey-cli", "hey-cli-main"},
		{"my.project", "my-project-main"},
		{"weird name!", "weird-name-main"},
		{"trailing-dash-", "trailing-dash-main"},
	}
	for _, c := range cases {
		if got := safeMainSessionName(c.basename); got != c.want {
			t.Errorf("safeMainSessionName(%q) = %q, want %q", c.basename, got, c.want)
		}
	}
}
