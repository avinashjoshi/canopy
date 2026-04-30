package state

import (
	"encoding/json"
	"testing"
)

// TestMigrateLegacyProject_BasicV1ToV2 verifies the central migration: a
// v1 file with a basename-keyed Projects entry and legacy Workspace rows
// becomes v2-shaped after one MigrateLegacyProject call.
func TestMigrateLegacyProject_BasicV1ToV2(t *testing.T) {
	s := &State{
		SchemaVersion: 1,
		Projects: map[string]ProjectMeta{
			"cravd": {PortBase: 3000}, // v1: basename key, no Root
		},
		Workspaces: []Workspace{
			{Project: "cravd", Name: "bold-falcon", Branch: "feature/x"},
			{Project: "cravd", Name: "soft-fox", Branch: "feature/y"},
			{Project: "other", Name: "unrelated", Branch: "main"}, // different project, untouched
		},
	}

	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	if _, basenameStillExists := s.Projects["cravd"]; basenameStillExists {
		t.Fatalf("basename key 'cravd' should be gone after migration")
	}
	meta, ok := s.Projects["/home/avi/Work/cravd"]
	if !ok {
		t.Fatalf("Projects[/home/avi/Work/cravd] missing after migration")
	}
	if meta.PortBase != 3000 {
		t.Fatalf("PortBase = %d, want 3000 (must survive migration)", meta.PortBase)
	}
	if meta.Root != "/home/avi/Work/cravd" {
		t.Fatalf("meta.Root = %q, want %q", meta.Root, "/home/avi/Work/cravd")
	}

	// Workspaces of the migrated project get ProjectRoot.
	for _, w := range s.Workspaces[:2] {
		if w.ProjectRoot != "/home/avi/Work/cravd" {
			t.Errorf("Workspace %q: ProjectRoot = %q, want %q", w.Name, w.ProjectRoot, "/home/avi/Work/cravd")
		}
		if w.Project != "cravd" {
			t.Errorf("Workspace %q: legacy Project = %q, want preserved as 'cravd'", w.Name, w.Project)
		}
	}
	// The unrelated project's row is untouched.
	if s.Workspaces[2].ProjectRoot != "" {
		t.Errorf("unrelated workspace got ProjectRoot = %q, should still be empty", s.Workspaces[2].ProjectRoot)
	}

	if s.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", s.SchemaVersion)
	}
}

// TestMigrateLegacyProject_Idempotent: calling migration twice on the same
// (basename, root) pair must not double-mutate.
func TestMigrateLegacyProject_Idempotent(t *testing.T) {
	s := &State{
		SchemaVersion: 1,
		Projects: map[string]ProjectMeta{
			"cravd": {PortBase: 3000},
		},
		Workspaces: []Workspace{
			{Project: "cravd", Name: "bold-falcon"},
		},
	}

	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	// Snapshot post-first-migration state.
	beforeSecond, _ := json.Marshal(s)

	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	afterSecond, _ := json.Marshal(s)

	if string(beforeSecond) != string(afterSecond) {
		t.Errorf("second migration changed state\nbefore: %s\nafter:  %s", beforeSecond, afterSecond)
	}
}

// TestMigrateLegacyProject_PartialOnly: when state has two projects but
// migration is called for only one, the other one is left untouched.
func TestMigrateLegacyProject_PartialOnly(t *testing.T) {
	s := &State{
		SchemaVersion: 1,
		Projects: map[string]ProjectMeta{
			"cravd":  {PortBase: 3000},
			"canopy": {PortBase: 4000}, // not migrating this one yet
		},
		Workspaces: []Workspace{
			{Project: "cravd", Name: "bold-falcon"},
			{Project: "canopy", Name: "soft-fox"},
		},
	}

	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	if _, ok := s.Projects["/home/avi/Work/cravd"]; !ok {
		t.Errorf("cravd not migrated to root key")
	}
	if _, ok := s.Projects["canopy"]; !ok {
		t.Errorf("canopy basename key should still exist (its migration hasn't run yet)")
	}
	if s.Workspaces[1].ProjectRoot != "" {
		t.Errorf("canopy workspace got ProjectRoot prematurely")
	}
}

// TestMigrateLegacyProject_AlreadyAtRootKey: if a v2-shaped entry already
// sits at the canonical root key, migration should not clobber it (e.g.
// the user manually fixed state.json before canopy got around to it).
func TestMigrateLegacyProject_AlreadyAtRootKey(t *testing.T) {
	s := &State{
		SchemaVersion: 2,
		Projects: map[string]ProjectMeta{
			"/home/avi/Work/cravd": {Root: "/home/avi/Work/cravd", PortBase: 3000},
		},
		Workspaces: []Workspace{
			{Project: "cravd", ProjectRoot: "/home/avi/Work/cravd", Name: "bold-falcon"},
		},
	}

	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	if len(s.Projects) != 1 {
		t.Errorf("expected 1 project entry, got %d", len(s.Projects))
	}
	if s.Projects["/home/avi/Work/cravd"].PortBase != 3000 {
		t.Errorf("PortBase clobbered: %d", s.Projects["/home/avi/Work/cravd"].PortBase)
	}
}

// TestMigrateLegacyProject_BothKeysCollapse: a state.json with BOTH the
// legacy basename key AND the v2 root-path key for the same project (the
// "stale stub left by an older canopy" scenario). MigrateLegacyProject
// must drop the stale basename entry; FindBasenameCollision must then
// not fire (it was firing pre-fix and refusing Manager construction
// outright). PortBase from the legacy stub is salvaged when the v2
// entry doesn't have one.
func TestMigrateLegacyProject_BothKeysCollapse(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"cravd":                {PortBase: 3000}, // legacy v1 stub
			"/home/avi/Work/cravd": {Root: "/home/avi/Work/cravd"}, // v2 entry, no PortBase yet
		},
	}
	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	if _, stillThere := s.Projects["cravd"]; stillThere {
		t.Errorf("legacy basename entry should be removed: %v", s.Projects)
	}
	if got := s.Projects["/home/avi/Work/cravd"].PortBase; got != 3000 {
		t.Errorf("PortBase not salvaged from v1 stub: got %d, want 3000", got)
	}
	// FindBasenameCollision must NOT fire after the migration — both
	// keys collapsed into one.
	if other := s.FindBasenameCollision("/home/avi/Work/cravd"); other != "" {
		t.Errorf("FindBasenameCollision returned %q after collapse; want empty", other)
	}
}

// TestMigrateLegacyProject_BothKeysPreservesV2PortBase: when both keys
// exist AND the v2 entry already has a PortBase, the migration must
// preserve it (don't overwrite from the v1 stub).
func TestMigrateLegacyProject_BothKeysPreservesV2PortBase(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"cravd":                {PortBase: 3000}, // legacy v1 stub
			"/home/avi/Work/cravd": {Root: "/home/avi/Work/cravd", PortBase: 4000}, // v2 wins
		},
	}
	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	if got := s.Projects["/home/avi/Work/cravd"].PortBase; got != 4000 {
		t.Errorf("v2 PortBase clobbered by v1 stub: got %d, want 4000", got)
	}
}

// TestMigrateLegacyProject_SelfHealsEmptyRoot: a Projects entry already at
// the canonical root key but with meta.Root == "" should get its Root
// backfilled. Covers a partial-migration recovery.
func TestMigrateLegacyProject_SelfHealsEmptyRoot(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/home/avi/Work/cravd": {PortBase: 3000}, // Root field empty
		},
	}
	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	if s.Projects["/home/avi/Work/cravd"].Root != "/home/avi/Work/cravd" {
		t.Errorf("Root not self-healed")
	}
}

// TestMigrateLegacyProject_NoOpOnFreshState: calling migration on an empty
// State adds nothing, doesn't touch SchemaVersion.
func TestMigrateLegacyProject_NoOpOnFreshState(t *testing.T) {
	s := &State{}
	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	// Projects map gets initialized but stays empty.
	if len(s.Projects) != 0 {
		t.Errorf("expected empty Projects, got %d entries", len(s.Projects))
	}
	if s.SchemaVersion != 0 {
		t.Errorf("SchemaVersion bumped on a no-op migration: %d", s.SchemaVersion)
	}
}

// TestLoadV1FileRoundTrip: simulate loading a real v1-shaped JSON document
// (no Root, no ProjectRoot fields) and verify it parses cleanly into the
// v2 struct shape — legacy fields populate, new fields stay zero. Then
// run migration and assert v2 shape.
//
// This is the IRON-RULE regression test: prove a v1 state.json on disk
// doesn't crash v0.5.
func TestLoadV1FileRoundTrip(t *testing.T) {
	v1JSON := []byte(`{
		"schema_version": 1,
		"projects": {
			"cravd": {"port_base": 3000}
		},
		"workspaces": [
			{
				"project": "cravd",
				"name": "bold-falcon",
				"branch": "feature/x",
				"path": "/home/avi/.canopy/workspaces/cravd/bold-falcon",
				"tmux_session": "cravd-bold-falcon",
				"port": 3000,
				"status": "ready",
				"created_at": "2026-01-01T00:00:00Z"
			}
		]
	}`)

	var s State
	if err := json.Unmarshal(v1JSON, &s); err != nil {
		t.Fatalf("parse v1 file: %v", err)
	}

	// Pre-migration: legacy fields populate, new fields stay zero.
	if s.Workspaces[0].Project != "cravd" {
		t.Errorf("legacy Project not parsed: %q", s.Workspaces[0].Project)
	}
	if s.Workspaces[0].ProjectRoot != "" {
		t.Errorf("ProjectRoot pre-migration should be empty, got %q", s.Workspaces[0].ProjectRoot)
	}
	meta, ok := s.Projects["cravd"]
	if !ok {
		t.Fatalf("v1 Projects entry by basename missing")
	}
	if meta.Root != "" {
		t.Errorf("meta.Root pre-migration should be empty, got %q", meta.Root)
	}

	// Run migration.
	s.MigrateLegacyProject("cravd", "/home/avi/Work/cravd")

	// Post-migration: v2 shape.
	if s.Workspaces[0].ProjectRoot != "/home/avi/Work/cravd" {
		t.Errorf("ProjectRoot not backfilled: %q", s.Workspaces[0].ProjectRoot)
	}
	migrated, ok := s.Projects["/home/avi/Work/cravd"]
	if !ok {
		t.Errorf("Projects entry not moved to root key")
	}
	if migrated.PortBase != 3000 {
		t.Errorf("PortBase = %d, want 3000", migrated.PortBase)
	}
}

// TestExistingPortBaseSurvivesMigration: explicit IRON-RULE test that port
// allocations don't shuffle during migration. If this regresses, every
// v1 user's running services break on first canopy v0.5 invocation.
func TestExistingPortBaseSurvivesMigration(t *testing.T) {
	cases := []struct {
		basename string
		root     string
		port     int
	}{
		{"cravd", "/home/avi/Work/cravd", 3000},
		{"canopy", "/home/avi/Code/canopy", 4000},
		{"brain", "/home/avi/Other/brain", 5000},
	}
	for _, c := range cases {
		t.Run(c.basename, func(t *testing.T) {
			s := &State{
				Projects: map[string]ProjectMeta{
					c.basename: {PortBase: c.port},
				},
			}
			s.MigrateLegacyProject(c.basename, c.root)
			if got := s.Projects[c.root].PortBase; got != c.port {
				t.Errorf("PortBase = %d, want %d", got, c.port)
			}
		})
	}
}

// TestFindBasenameCollision_Empty: empty state has no collision.
func TestFindBasenameCollision_Empty(t *testing.T) {
	s := &State{}
	if got := s.FindBasenameCollision("/home/avi/Work/cravd"); got != "" {
		t.Errorf("FindBasenameCollision on empty state = %q, want empty", got)
	}
}

// TestFindBasenameCollision_SelfNotCollision: querying for a project that
// is already registered at this exact root path should NOT report itself
// as a collision.
func TestFindBasenameCollision_SelfNotCollision(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/home/avi/Work/cravd": {Root: "/home/avi/Work/cravd", PortBase: 3000},
		},
	}
	if got := s.FindBasenameCollision("/home/avi/Work/cravd"); got != "" {
		t.Errorf("self-query should not collide; got %q", got)
	}
}

// TestFindBasenameCollision_DifferentRootsSameBasename: the collision case.
func TestFindBasenameCollision_DifferentRootsSameBasename(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/home/avi/Work/cravd": {Root: "/home/avi/Work/cravd", PortBase: 3000},
		},
	}
	got := s.FindBasenameCollision("/home/avi/Other/cravd")
	if got != "/home/avi/Work/cravd" {
		t.Errorf("FindBasenameCollision = %q, want %q", got, "/home/avi/Work/cravd")
	}
}

// TestFindBasenameCollision_AllUniqueBasenames: three projects, all unique
// basenames, no collisions for any of them.
func TestFindBasenameCollision_AllUniqueBasenames(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/a/cravd":  {Root: "/a/cravd"},
			"/b/canopy": {Root: "/b/canopy"},
			"/c/brain":  {Root: "/c/brain"},
		},
	}
	for _, q := range []string{"/x/cravd-fork", "/y/something-else"} {
		if got := s.FindBasenameCollision(q); got != "" {
			t.Errorf("FindBasenameCollision(%q) = %q, want empty", q, got)
		}
	}
	// Sanity: actual collision still detected.
	if got := s.FindBasenameCollision("/x/cravd"); got != "/a/cravd" {
		t.Errorf("FindBasenameCollision(/x/cravd) = %q, want /a/cravd", got)
	}
}

// TestFindBasenameCollision_LegacyBasenameKey: a state with a v1 entry
// (basename as map key) should still detect collisions correctly.
// filepath.Base("cravd") == "cravd", so the comparison works either way.
func TestFindBasenameCollision_LegacyBasenameKey(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"cravd": {PortBase: 3000}, // pre-migration, basename key
		},
	}
	got := s.FindBasenameCollision("/home/avi/Work/cravd")
	if got != "cravd" {
		t.Errorf("FindBasenameCollision = %q, want %q (the legacy basename key)", got, "cravd")
	}
}
