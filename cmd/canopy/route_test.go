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

	"github.com/oncactus/canopy/internal/config"
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
