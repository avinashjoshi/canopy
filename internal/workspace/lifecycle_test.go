package workspace_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/settings"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

func TestMain(m *testing.M) {
	teardown, _ := clog.Init(false)
	defer teardown()
	m.Run()
}

func requireGitAndTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

// fixture builds a complete scratch project for E2E testing:
//  1. Temp dir as the project root.
//  2. git init + initial commit (worktree-add needs HEAD).
//  3. canopy.json with three trivial scripts (echo, true, true).
//  4. Three executable scripts that just exit 0.
//  5. A Manager pointing at the project + an isolated state dir + the
//     test tmux socket so production tmux is unaffected.
//
// Returns the Manager and a cleanup func that kills the test tmux server.
func fixture(t *testing.T) (*workspace.Manager, func()) {
	t.Helper()

	projectRoot := t.TempDir()
	stateDir := t.TempDir()

	// git init + initial commit so worktree add succeeds.
	gitSteps := [][]string{
		{"init", "--initial-branch=main", projectRoot},
		{"-C", projectRoot, "config", "user.email", "test@canopy.local"},
		{"-C", projectRoot, "config", "user.name", "canopy-test"},
		{"-C", projectRoot, "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range gitSteps {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	// Three trivial scripts. Each prints its name + the CANOPY_PORT env
	// so the tests can assert env propagation if they want.
	for _, name := range []string{"setup", "run", "archive"} {
		path := filepath.Join(projectRoot, "bin", "canopy-"+name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir bin: %v", err)
		}
		body := fmt.Sprintf("#!/usr/bin/env bash\necho 'canopy-%s ran on port '${CANOPY_PORT}\nexit 0\n", name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// canopy.json wiring the three scripts.
	cfgJSON := `{"scripts": {"setup": "bin/canopy-setup", "run": "bin/canopy-run", "archive": "bin/canopy-archive"}}`
	if err := os.WriteFile(filepath.Join(projectRoot, "canopy.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}

	cfg, err := config.DiscoverAndLoad(projectRoot)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}

	store, err := state.NewStore(stateDir)
	if err != nil {
		t.Fatalf("state.NewStore: %v", err)
	}

	tmuxClient := tmux.WithSocket("canopy-test")
	mgr := &workspace.Manager{
		Cfg:        cfg,
		Store:      store,
		Tmux:       tmuxClient,
		CanopyHome: stateDir, // workspaces dir is derived from this
		// Test-friendly port plan: well above typical dev-server ranges,
		// 100 ports per project (so 10 workspaces per project at stride 10
		// before any wrap), big project stride to keep tests readable.
		Settings: settings.Settings{
			Ports: settings.PortPlan{
				Base:            39000,
				ProjectStride:   100,
				WorkspaceStride: 10,
			},
		},
	}

	cleanup := func() {
		_ = tmuxClient.KillServer(context.Background())
	}
	t.Cleanup(cleanup)
	return mgr, cleanup
}

// TestCreate_HappyPath is the wedge demo as a test: Create returns a
// ready workspace, state.json has a row, the tmux session exists with
// 4 panes.
func TestCreate_HappyPath(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "feature-x", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if ws.Status != state.StatusReady {
		t.Errorf("status = %q; want ready", ws.Status)
	}
	if ws.Port < 39000 || ws.Port > 39100 {
		t.Errorf("port = %d; want in [39000, 39100]", ws.Port)
	}
	if !strings.Contains(stdout.String(), "canopy-setup ran on port") {
		t.Errorf("stdout missing setup output: %q", stdout.String())
	}

	// Workspace dir must exist on disk.
	if _, err := os.Stat(ws.Path); err != nil {
		t.Errorf("workspace dir missing at %s: %v", ws.Path, err)
	}

	// tmux session must exist with the standard 3-pane layout
	// (nvim top-left, claude top-right, shell full-width bottom).
	out, err := exec.Command("tmux", "-L", "canopy-test", "list-panes", "-t", ws.TmuxSession).Output()
	if err != nil {
		t.Errorf("list-panes: %v", err)
	} else if got := len(strings.Split(strings.TrimSpace(string(out)), "\n")); got != 3 {
		t.Errorf("pane count = %d; want 3", got)
	}
}

// TestCreate_GeneratesNameWhenEmpty verifies that an empty name triggers
// namegen and produces a generated workspace name.
func TestCreate_GeneratesNameWhenEmpty(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v\nstderr: %s", err, stderr.String())
	}
	if ws.Name == "" {
		t.Errorf("Name is empty; expected generated name")
	}
	// Generated names are adj-noun shape; not a perfect check but a smell test.
	if !strings.Contains(ws.Name, "-") {
		t.Errorf("Name = %q; expected hyphen", ws.Name)
	}
}

// TestCreate_AlreadyExists verifies the idempotency table: Create on an
// existing workspace name returns ErrWorkspaceExists.
func TestCreate_AlreadyExists(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var b1, b2, b3, b4 bytes.Buffer
	if _, err := mgr.Create(context.Background(), "feature-x", &b1, &b2); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := mgr.Create(context.Background(), "feature-x", &b3, &b4)
	if !errors.Is(err, workspace.ErrWorkspaceExists) {
		t.Errorf("second Create: got %v; want errors.Is(... ErrWorkspaceExists)", err)
	}
}

// TestRemove_HappyPath: Create then Remove. Verify state row gone, tmux
// session gone, workspace dir gone.
func TestRemove_HappyPath(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "to-remove", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wsPath := ws.Path
	tmuxName := ws.TmuxSession

	if err := mgr.Remove(context.Background(), "to-remove", &stdout, &stderr); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// State row gone.
	if _, err := mgr.Find(context.Background(), "to-remove"); !errors.Is(err, workspace.ErrWorkspaceNotFound) {
		t.Errorf("Find after Remove: got %v; want errors.Is(... ErrWorkspaceNotFound)", err)
	}

	// Workspace dir gone.
	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Errorf("workspace dir still exists at %s", wsPath)
	}

	// tmux session gone.
	if has, err := mgr.Tmux.HasSession(context.Background(), tmuxName); err != nil || has {
		t.Errorf("tmux session still alive: has=%v err=%v", has, err)
	}
}

// TestRemove_NotFound: trying to remove a name that isn't in state.
func TestRemove_NotFound(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)
	var stdout, stderr bytes.Buffer
	err := mgr.Remove(context.Background(), "never-existed", &stdout, &stderr)
	if !errors.Is(err, workspace.ErrWorkspaceNotFound) {
		t.Errorf("Remove(missing): got %v; want errors.Is(... ErrWorkspaceNotFound)", err)
	}
}

// TestList: Create two workspaces, List returns both with project filter.
func TestList(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var stdout, stderr bytes.Buffer
	for _, n := range []string{"feature-a", "feature-b"} {
		if _, err := mgr.Create(context.Background(), n, &stdout, &stderr); err != nil {
			t.Fatalf("Create(%s): %v", n, err)
		}
	}
	got, err := mgr.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("List len = %d; want 2", len(got))
	}
}

// TestResurrect_HappyPath: Create -> Kill tmux -> Resurrect -> tmux alive
// again with 4 panes. Per-dir claude history isn't testable here (no
// claude conversation in scratch), but the structural rebuild is.
func TestResurrect_HappyPath(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "resurrect-me", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Kill the tmux session out from under canopy (simulates reboot).
	if err := mgr.Tmux.Kill(context.Background(), ws.TmuxSession); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	revived, err := mgr.Resurrect(context.Background(), "resurrect-me")
	if err != nil {
		t.Fatalf("Resurrect: %v", err)
	}
	if revived.Status != state.StatusReady {
		t.Errorf("status after resurrect = %q; want ready", revived.Status)
	}

	out, err := exec.Command("tmux", "-L", "canopy-test", "list-panes", "-t", ws.TmuxSession).Output()
	if err != nil {
		t.Errorf("list-panes: %v", err)
	} else if got := len(strings.Split(strings.TrimSpace(string(out)), "\n")); got != 3 {
		t.Errorf("pane count after resurrect = %d; want 3", got)
	}
}
