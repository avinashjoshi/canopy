package workspace_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// TestNew_RunsMigrationOnV1State proves that workspace.New triggers the
// v1→v2 migration on first call. This is the entry point for every
// project-scoped command, so any v1 user's state file is migrated the
// first time they run any canopy command in v0.5+.
//
// Setup: pre-write a v1-shaped state.json in a fake $HOME, write a
// canopy.json + git init under another temp dir, point HOME at the fake
// home, call workspace.New, then read state.json back and assert v2 shape.
func TestNew_RunsMigrationOnV1State(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	canopyHome := filepath.Join(fakeHome, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir canopyHome: %v", err)
	}

	// Build a project root with canopy.json. Use a basename that matches
	// the v1 state entry so migration kicks in.
	projectRoot := t.TempDir()
	projectBasename := filepath.Base(projectRoot)
	cfgJSON := `{"scripts": {}}`
	if err := os.WriteFile(filepath.Join(projectRoot, "canopy.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}

	// Pre-write a v1 state.json. The PortBase must survive migration.
	v1JSON := fmt.Sprintf(`{
		"schema_version": 1,
		"projects": {
			"%s": {"port_base": 7000}
		},
		"workspaces": [
			{
				"project": "%s",
				"name": "legacy-foo",
				"branch": "feature/x",
				"path": "/some/old/path",
				"tmux_session": "%s-legacy-foo",
				"port": 7000,
				"status": "ready",
				"created_at": "2026-01-01T00:00:00Z"
			}
		]
	}`, projectBasename, projectBasename, projectBasename)
	if err := os.WriteFile(filepath.Join(canopyHome, "state.json"), []byte(v1JSON), 0o644); err != nil {
		t.Fatalf("write v1 state: %v", err)
	}

	cfg, err := config.DiscoverAndLoad(projectRoot)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}

	// Construct Manager — this runs migrateAndGuard.
	if _, err := workspace.New(cfg); err != nil {
		t.Fatalf("workspace.New: %v", err)
	}

	// Read state.json back and assert v2 shape.
	store, err := state.NewStore(canopyHome)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if st.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", st.SchemaVersion)
	}
	if _, basenameStillExists := st.Projects[projectBasename]; basenameStillExists {
		t.Errorf("basename key %q should be gone after migration", projectBasename)
	}
	meta, ok := st.Projects[cfg.ProjectRoot]
	if !ok {
		t.Fatalf("Projects[%q] missing after migration", cfg.ProjectRoot)
	}
	if meta.PortBase != 7000 {
		t.Errorf("PortBase = %d, want 7000 (must survive migration)", meta.PortBase)
	}
	if meta.Root != cfg.ProjectRoot {
		t.Errorf("meta.Root = %q, want %q", meta.Root, cfg.ProjectRoot)
	}
	if len(st.Workspaces) != 1 {
		t.Fatalf("workspaces count = %d, want 1", len(st.Workspaces))
	}
	if st.Workspaces[0].ProjectRoot != cfg.ProjectRoot {
		t.Errorf("legacy workspace ProjectRoot not backfilled: got %q", st.Workspaces[0].ProjectRoot)
	}
	if st.Workspaces[0].Project != projectBasename {
		t.Errorf("legacy Project field should be preserved: got %q", st.Workspaces[0].Project)
	}
}

// TestNew_RefusesBasenameCollision proves the basename-uniqueness invariant.
// Pre-load state with project "cravd" at /a/cravd, then try workspace.New
// for a different /b/cravd. Must error with ErrBasenameCollision and NOT
// mutate state.json.
//
// This is the critical IRON-RULE guard: state on disk must be unchanged
// after a refused init. Otherwise a half-allocated PortBase or stray entry
// would corrupt the user's state.
func TestNew_RefusesBasenameCollision(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	canopyHome := filepath.Join(fakeHome, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir canopyHome: %v", err)
	}

	// Pre-existing project A at /a/cravd, registered in state.
	preexisting := fmt.Sprintf(`{
		"schema_version": 2,
		"projects": {
			"/a/cravd": {"root": "/a/cravd", "port_base": 7000}
		},
		"workspaces": []
	}`)
	statePath := filepath.Join(canopyHome, "state.json")
	if err := os.WriteFile(statePath, []byte(preexisting), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// Snapshot state file bytes before the attempted collision.
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state before: %v", err)
	}

	// Build a colliding project B: different absolute path, same basename.
	parent := t.TempDir()
	collidingRoot := filepath.Join(parent, "cravd")
	if err := os.MkdirAll(collidingRoot, 0o755); err != nil {
		t.Fatalf("mkdir colliding root: %v", err)
	}
	cfgJSON := `{"scripts": {}}`
	if err := os.WriteFile(filepath.Join(collidingRoot, "canopy.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}

	cfg, err := config.DiscoverAndLoad(collidingRoot)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}
	if filepath.Base(cfg.ProjectRoot) != "cravd" {
		t.Fatalf("test setup: expected basename cravd, got %q", filepath.Base(cfg.ProjectRoot))
	}

	// workspace.New should refuse with ErrBasenameCollision.
	_, err = workspace.New(cfg)
	if !errors.Is(err, workspace.ErrBasenameCollision) {
		t.Fatalf("workspace.New: got %v, want errors.Is(... ErrBasenameCollision)", err)
	}

	// CRITICAL: state on disk must be byte-identical. A refused construction
	// must not have mutated state in any way.
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state after: %v", err)
	}
	if string(stateBefore) != string(stateAfter) {
		t.Errorf("state.json mutated by refused workspace.New\nbefore: %s\nafter:  %s",
			stateBefore, stateAfter)
	}
}
