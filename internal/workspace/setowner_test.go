package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// setOwnerFixture builds a Manager backed by a real flock'd Store with a
// single workspace row already inserted, without needing git or tmux —
// SetOwner only touches state.json. Returns the manager and the row name.
func setOwnerFixture(t *testing.T, sourceKind string) (*workspace.Manager, string) {
	t.Helper()
	root := t.TempDir()
	cfgJSON := `{"scripts": {"setup": "bin/s", "run": "bin/r", "archive": "bin/a"}}`
	if err := os.WriteFile(filepath.Join(root, "canopy.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}
	cfg, err := config.DiscoverAndLoad(root)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mgr := &workspace.Manager{Cfg: cfg, Store: store}

	const name = "ws1"
	if err := store.WithLock(func(s *state.State) error {
		return s.Add(state.Workspace{
			ProjectRoot: cfg.ProjectRoot,
			Name:        name,
			Branch:      name,
			Path:        filepath.Join(root, name),
			Port:        3000,
			Status:      state.StatusReady,
			SourceKind:  sourceKind,
		})
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return mgr, name
}

// TestSetOwner_SetThenClear exercises both mutating branches: setting a
// foreign login (marks the row as reviewing) and clearing it back to the
// self-marker (mine), each persisted under the flock.
func TestSetOwner_SetThenClear(t *testing.T) {
	mgr, name := setOwnerFixture(t, "pr")
	ctx := context.Background()

	if err := mgr.SetOwner(ctx, name, "octocat"); err != nil {
		t.Fatalf("SetOwner(set): %v", err)
	}
	got, err := mgr.Find(ctx, name)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Owner != "octocat" {
		t.Errorf("after set: Owner = %q; want octocat", got.Owner)
	}
	if !state.OwnerIsReviewing(got.Owner, got.SourceKind) {
		t.Errorf("after set: row should read as reviewing")
	}

	if err := mgr.SetOwner(ctx, name, state.OwnerSelfMarker); err != nil {
		t.Fatalf("SetOwner(clear): %v", err)
	}
	got, err = mgr.Find(ctx, name)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Owner != state.OwnerSelfMarker {
		t.Errorf("after clear: Owner = %q; want self-marker", got.Owner)
	}
	// Clearing a pr-sourced row must override the legacy REVIEW fallback.
	if state.OwnerIsReviewing(got.Owner, got.SourceKind) {
		t.Errorf("after clear: pr row should read as mine, not reviewing")
	}
}

// TestSetOwner_NotFound surfaces the canonical sentinel so the CLI verb
// and TUI can distinguish a bad name from a genuine write failure.
func TestSetOwner_NotFound(t *testing.T) {
	mgr, _ := setOwnerFixture(t, "fresh")
	err := mgr.SetOwner(context.Background(), "no-such-ws", "octocat")
	if !errors.Is(err, workspace.ErrWorkspaceNotFound) {
		t.Errorf("err = %v; want ErrWorkspaceNotFound", err)
	}
}
