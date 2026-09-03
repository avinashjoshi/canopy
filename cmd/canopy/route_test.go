package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/state"
)

// routeRoot is hard to test end-to-end (each branch launches a Bubbletea
// program that takes over the terminal). The unit tests here focus on
// the routing decision: for each cwd shape, did we reach the expected
// branch?
//
// Strategy: each test sets up a cwd with a specific shape (canopy.json
// present / fresh git repo / neither), then calls a small probe function
// that mirrors routeRoot's switch but returns a route name instead of
// running a TUI. That way the routing logic is testable without the
// Bubbletea side effects.
//
// The probe function lives in this test file (not the production code)
// because exposing routeRoot's branch decisions in production isn't
// useful — only tests need that view.

type route int

const (
	routeProject route = iota
	routeSplash
	routeGlobal
	routeError
)

// probeRoute mirrors routeRoot's branch decision without launching any
// Bubbletea program or workspace.Manager. Used only by tests in this file.
func probeRoute(ctx context.Context, cwd string) (route, error) {
	cfg, cfgErr := config.DiscoverAndLoad(cwd)
	switch {
	case cfgErr == nil:
		_ = cfg
		return routeProject, nil
	case errors.Is(cfgErr, config.ErrNotFound):
		isRepo, _ := isGitRepoForTest(ctx, cwd)
		if isRepo {
			return routeSplash, nil
		}
		return routeGlobal, nil
	default:
		return routeError, cfgErr
	}
}

// isGitRepoForTest is a tiny inline call to git rev-parse so the test
// doesn't need to import internal/git (which would be the cleaner long-
// term shape but creates a vendoring concern in a test file). Behavior
// matches git.IsRepo: any error → not a repo.
func isGitRepoForTest(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// TestRoute_FoundCanopyJSON: cwd is inside a canopy project → project route.
func TestRoute_FoundCanopyJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "canopy.json"), []byte(`{"scripts": {}}`), 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}

	got, err := probeRoute(context.Background(), dir)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got != routeProject {
		t.Errorf("route = %v, want routeProject", got)
	}
}

// TestRoute_FreshGitRepo: cwd is a fresh git repo with no canopy.json → splash.
func TestRoute_FreshGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init").Run(); err != nil {
		t.Skipf("git init failed: %v", err)
	}

	got, err := probeRoute(context.Background(), dir)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got != routeSplash {
		t.Errorf("route = %v, want routeSplash", got)
	}
}

// TestRoute_NoRepoNoCanopy: cwd is neither a git repo nor a canopy
// project → global route.
func TestRoute_NoRepoNoCanopy(t *testing.T) {
	dir := t.TempDir()

	got, err := probeRoute(context.Background(), dir)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got != routeGlobal {
		// Possible false positive: the user's tempdir lives under a
		// directory that's already a git worktree. Skip gracefully.
		if got == routeSplash {
			t.Skipf("tempdir %q is unexpectedly inside a git repo; skipping", dir)
		}
		t.Errorf("route = %v, want routeGlobal", got)
	}
}

// TestRoute_DiscoverErrorBubbles: a non-NotFound discover error (e.g.
// permission denied) should bubble. Hard to simulate without root; we
// substitute a malformed canopy.json mid-tree so config.Load errors with
// ErrInvalid (still not ErrNotFound).
func TestRoute_DiscoverErrorBubbles(t *testing.T) {
	dir := t.TempDir()
	bad := []byte(`{not valid json`)
	if err := os.WriteFile(filepath.Join(dir, "canopy.json"), bad, 0o644); err != nil {
		t.Fatalf("write bad canopy.json: %v", err)
	}

	got, err := probeRoute(context.Background(), dir)
	if got != routeError {
		t.Errorf("route = %v, want routeError", got)
	}
	if err == nil {
		t.Errorf("expected error, got nil")
	}
}

// silenceWriter discards output; used to call routeRoot in tests where
// we just want to check that it doesn't panic, without inspecting prints.
type silenceWriter struct{}

func (silenceWriter) Write(p []byte) (int, error) { return len(p), nil }

var _ io.Writer = silenceWriter{}

// TestResolveProjectContext_workspaceCwd is the regression test for the
// popup-mode "Local tab is empty" bug: when canopy is launched from
// inside a workspace dir (a git worktree of the source repo containing
// the project's checked-in canopy.json), the canonical currentProject
// must resolve to the SOURCE REPO root, not the workspace dir. Without
// this, every state.GlobalRow for the project carries the source-repo
// ProjectRoot but the Local tab filters by the worktree path → zero
// rows → the user sees "No workspaces in this project" until they
// switch tabs and focus-back to refresh currentProject from a row.
//
// The fix calls workspace.ResolveCurrentProject (workspace-path-prefix
// match first) before falling back to DiscoverAndLoad. This test
// verifies the resolution in isolation from the rest of routeRoot,
// which launches a Bubbletea program and is hard to drive end-to-end.
func TestResolveProjectContext_workspaceCwd(t *testing.T) {
	root := t.TempDir()
	srcRepo := filepath.Join(root, "src", "myproj")
	if err := os.MkdirAll(srcRepo, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	wsDir := filepath.Join(root, "workspaces", "myproj", "feat-x")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}

	// canopy.json at BOTH the source repo root AND the workspace dir
	// (the worktree carries it via git). This is the exact shape that
	// fooled the old DiscoverAndLoad-first resolution.
	canopyJSON := []byte(`{"scripts": {"setup": "x", "run": "y", "archive": "z"}}`)
	for _, dir := range []string{srcRepo, wsDir} {
		if err := os.WriteFile(filepath.Join(dir, "canopy.json"), canopyJSON, 0o644); err != nil {
			t.Fatalf("write canopy.json: %v", err)
		}
	}

	// Resolve src repo through EvalSymlinks since state stores
	// canonical paths and ResolveCurrentProject canonicalizes cwd
	// before prefix matching. tmpdirs on macOS go through /var → /private/var,
	// which would defeat string equality otherwise.
	canonicalSrc, err := filepath.EvalSymlinks(srcRepo)
	if err != nil {
		t.Fatalf("evalsymlinks src: %v", err)
	}
	canonicalWs, err := filepath.EvalSymlinks(wsDir)
	if err != nil {
		t.Fatalf("evalsymlinks ws: %v", err)
	}

	st := &state.State{
		Projects: map[string]state.ProjectMeta{
			canonicalSrc: {Root: canonicalSrc},
		},
		Workspaces: []state.Workspace{
			{
				Name:        "feat-x",
				Path:        canonicalWs,
				ProjectRoot: canonicalSrc,
			},
		},
	}

	t.Run("cwd_inside_workspace_resolves_to_source_repo", func(t *testing.T) {
		got, cfg, err := resolveProjectContext(canonicalWs, st)
		if err != nil {
			t.Fatalf("resolveProjectContext: %v", err)
		}
		if got != canonicalSrc {
			t.Errorf("currentProject = %q; want %q (source-repo root, not workspace dir)", got, canonicalSrc)
		}
		// Workspace-prefix match returns no walk-up cfg — caller
		// is expected to LoadFrom the canonical root itself.
		if cfg != nil {
			t.Errorf("walk-up cfg = %v; want nil (workspace match should not load cfg)", cfg)
		}
	})

	t.Run("cwd_inside_source_repo_uses_walkup", func(t *testing.T) {
		got, cfg, err := resolveProjectContext(canonicalSrc, st)
		if err != nil {
			t.Fatalf("resolveProjectContext: %v", err)
		}
		if got != canonicalSrc {
			t.Errorf("currentProject = %q; want %q", got, canonicalSrc)
		}
		// Source-repo walk-up: ResolveCurrentProject also returns the
		// root via its step-2 walk-up + state.Projects check, which
		// short-circuits before our outer DiscoverAndLoad fallback.
		// Either way the caller will LoadFrom the canonical root.
		_ = cfg
	})

	t.Run("cwd_outside_any_project_returns_empty", func(t *testing.T) {
		outside := filepath.Join(root, "elsewhere")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatalf("mkdir outside: %v", err)
		}
		got, cfg, err := resolveProjectContext(outside, st)
		if err != nil {
			t.Fatalf("resolveProjectContext: %v", err)
		}
		if got != "" {
			t.Errorf("currentProject = %q; want \"\" (no project)", got)
		}
		if cfg != nil {
			t.Errorf("cfg = %v; want nil", cfg)
		}
	})
}

// TestResolveProjectContext_unregisteredProjectFallsThrough covers the
// fresh-clone path: cwd has canopy.json but state.json doesn't know
// about the project yet (no `canopy init` run). ResolveCurrentProject
// returns "" because step 2 only matches REGISTERED projects; the
// outer DiscoverAndLoad fallback then populates currentProject so the
// user still gets project context (and the splash gate doesn't fire,
// since cfg is non-nil).
func TestResolveProjectContext_unregisteredProjectFallsThrough(t *testing.T) {
	dir := t.TempDir()
	canopyJSON := []byte(`{"scripts": {"setup": "x", "run": "y", "archive": "z"}}`)
	if err := os.WriteFile(filepath.Join(dir, "canopy.json"), canopyJSON, 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}

	// Empty state — project is unregistered.
	st := &state.State{}

	got, cfg, err := resolveProjectContext(canonical, st)
	if err != nil {
		t.Fatalf("resolveProjectContext: %v", err)
	}
	if got != canonical {
		t.Errorf("currentProject = %q; want %q (walk-up fallback)", got, canonical)
	}
	if cfg == nil {
		t.Errorf("cfg = nil; want non-nil (walk-up should populate cfg)")
	}
}

// TestRouteRemote_UnregisteredHost covers routeRemote's fail-fast path:
// an unregistered host name must error out BEFORE any Bubbletea program
// launches (routeRemote resolves the host from the registry first).
// This is the only branch of routeRemote that's safely unit-testable —
// the success path hands off to ui.RunRemotePinned, which takes over
// the terminal, same reason routeRoot itself isn't driven end-to-end
// in these tests (see the file header comment).
func TestRouteRemote_UnregisteredHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := routeRemote("nonexistent-host")
	if err == nil {
		t.Fatal("routeRemote(unregistered host) = nil error; want an error")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error = %q; want it to mention the host isn't registered", err.Error())
	}
}

// TestRouteRemote_EmptyRegistry is the zero-hosts case: ~/.canopy has
// no hosts.json at all yet (fresh install, never ran `canopy host add`).
// Should fail the same clear way as an unregistered name, not panic on
// a missing file.
func TestRouteRemote_EmptyRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := routeRemote("tower")
	if err == nil {
		t.Fatal("routeRemote(no hosts.json) = nil error; want an error")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error = %q; want it to mention the host isn't registered", err.Error())
	}
}
