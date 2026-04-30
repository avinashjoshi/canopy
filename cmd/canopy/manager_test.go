package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveCfgForCwd_workspaceCwd is the regression test for the
// "canopy reconcile from inside a workspace doesn't update branches"
// bug: CLI loadManager used to call DiscoverAndLoad(cwd) directly, so
// running any CLI verb from a workspace dir would resolve cfg.ProjectRoot
// to the worktree's path (the worktree contains the project's checked-in
// canopy.json). Reconcile then filtered every state.Workspace row out
// because their ProjectRoot is the source-repo path, not the worktree
// path — silently a no-op for the workspace the user is actually in.
//
// resolveCfgForCwd now mirrors the unified TUI's route resolution:
// ResolveCurrentProject (workspace-path-prefix wins) + LoadFrom against
// the canonical root, falling back to walk-up only when the cwd is
// outside any registered workspace.
func TestResolveCfgForCwd_workspaceCwd(t *testing.T) {
	root := t.TempDir()
	srcRepo := filepath.Join(root, "src", "myproj")
	if err := os.MkdirAll(srcRepo, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	wsDir := filepath.Join(root, "workspaces", "myproj", "feat-x")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}

	canopyJSON := []byte(`{"scripts": {"setup": "x", "run": "y", "archive": "z"}}`)
	for _, dir := range []string{srcRepo, wsDir} {
		if err := os.WriteFile(filepath.Join(dir, "canopy.json"), canopyJSON, 0o644); err != nil {
			t.Fatalf("write canopy.json: %v", err)
		}
	}

	canonicalSrc, err := filepath.EvalSymlinks(srcRepo)
	if err != nil {
		t.Fatalf("evalsymlinks src: %v", err)
	}
	canonicalWs, err := filepath.EvalSymlinks(wsDir)
	if err != nil {
		t.Fatalf("evalsymlinks ws: %v", err)
	}

	// Stage a fake home with a state.json that registers the project +
	// workspace. The resolver walks state.Workspaces[*].Path looking
	// for cwd's prefix, so the recorded Path must equal the cwd we
	// pass in for the prefix match to fire.
	home := t.TempDir()
	canopyDir := filepath.Join(home, ".canopy")
	if err := os.MkdirAll(canopyDir, 0o755); err != nil {
		t.Fatalf("mkdir canopy home: %v", err)
	}
	stateBytes, err := json.Marshal(map[string]any{
		"version": 2,
		"projects": map[string]any{
			canonicalSrc: map[string]any{"root": canonicalSrc, "port_base": 39000},
		},
		"workspaces": []map[string]any{
			{
				"name":         "feat-x",
				"path":         canonicalWs,
				"project_root": canonicalSrc,
				"project":      "myproj",
				"branch":       "feat-x",
				"status":       "ready",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(canopyDir, "state.json"), stateBytes, 0o644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	t.Setenv("HOME", home)

	t.Run("from_workspace_dir_resolves_to_source_repo", func(t *testing.T) {
		cfg, err := resolveCfgForCwd(canonicalWs)
		if err != nil {
			t.Fatalf("resolveCfgForCwd: %v", err)
		}
		if cfg.ProjectRoot != canonicalSrc {
			t.Errorf("ProjectRoot = %q; want %q (canonical source repo, not worktree)", cfg.ProjectRoot, canonicalSrc)
		}
	})

	t.Run("from_source_repo_walks_up_normally", func(t *testing.T) {
		cfg, err := resolveCfgForCwd(canonicalSrc)
		if err != nil {
			t.Fatalf("resolveCfgForCwd: %v", err)
		}
		if cfg.ProjectRoot != canonicalSrc {
			t.Errorf("ProjectRoot = %q; want %q", cfg.ProjectRoot, canonicalSrc)
		}
	})

	t.Run("subdir_of_workspace_resolves_to_source_repo", func(t *testing.T) {
		sub := filepath.Join(canonicalWs, "internal", "ui")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir subdir: %v", err)
		}
		cfg, err := resolveCfgForCwd(sub)
		if err != nil {
			t.Fatalf("resolveCfgForCwd: %v", err)
		}
		if cfg.ProjectRoot != canonicalSrc {
			t.Errorf("ProjectRoot = %q; want %q (subdir-of-workspace should also resolve to source)", cfg.ProjectRoot, canonicalSrc)
		}
	})
}
