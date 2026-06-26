package workspace_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/settings"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// stubAgentBinaries creates no-op executables for "claude" and "codex"
// in a temp dir prepended to PATH for the duration of the test. The
// codex review P1 #3 fix (agent_swap.go step 2) calls
// launcher.VerifyInstalled (= exec.LookPath) BEFORE any tmux/state
// mutation. CI runners don't have @openai/codex installed; without
// these stubs every swap test fails at the launcher check. The stubs
// are read-only sentinels — actual launching of the agent pane in
// these tests already hits agentFallbackShell when the binary is
// truly missing in normal runs, but VerifyInstalled doesn't care
// what the binary does, only that LookPath finds it.
//
// Why per-test PATH stub (vs. installing in CI): the test models the
// CONTRACT — "swap fails fast when codex isn't on PATH" — without
// requiring CI to ship a real codex. A separate test could pin the
// VerifyInstalled-rejects-missing-launcher contract by NOT stubbing.
func stubAgentBinaries(t *testing.T, names ...string) {
	t.Helper()
	stubDir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(stubDir, name)
		// Minimal POSIX stub: exec a shell that immediately exits 0.
		// tmux respawn-pane with -K keeps the pane open afterward, so
		// the pane survives long enough for LookupAllPanes to find it.
		body := "#!/bin/sh\nexec /bin/sh\n"
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fixtureWithAgents is fixture(), but the canopy.json declares
// `agents: ["claude", "codex"]` so SwapAgent's allowlist gate has both
// real launchers available. claude is the default (first entry).
func fixtureWithAgents(t *testing.T) (*workspace.Manager, func()) {
	t.Helper()
	stubAgentBinaries(t, "claude", "codex")
	mgr, cleanup := fixture(t)
	// Overwrite canopy.json with one that declares agents. fixture()
	// already wrote a minimal one without an agents block.
	cfgJSON := `{
		"scripts": {"setup": "bin/canopy-setup", "run": "bin/canopy-run", "archive": "bin/canopy-archive"},
		"agents": ["claude", "codex"]
	}`
	cfgPath := filepath.Join(mgr.Cfg.ProjectRoot, "canopy.json")
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("rewrite canopy.json: %v", err)
	}
	// Re-Load so mgr.Cfg picks up the new schema. (DiscoverAndLoad
	// re-resolves project root + populates Agents via validate().)
	newCfg, err := config.DiscoverAndLoad(mgr.Cfg.ProjectRoot)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}
	mgr.Cfg = newCfg
	return mgr, cleanup
}

// TestSwapAgent_HappyPath: a claude workspace swapped to codex ends up
// with codex as the persisted CurrentAgent AND the agent pane's role
// tag updated to "agent:codex". The IDE + shell panes survive untouched.
func TestSwapAgent_HappyPath(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixtureWithAgents(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "swap-happy", workspace.CreateOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if ws.CurrentAgent != "claude" {
		t.Fatalf("pre-swap CurrentAgent = %q; want claude (project default)", ws.CurrentAgent)
	}

	updated, err := mgr.SwapAgent(context.Background(), ws.Name, "codex")
	if err != nil {
		t.Fatalf("SwapAgent: %v", err)
	}
	if updated.CurrentAgent != "codex" {
		t.Errorf("post-swap CurrentAgent = %q; want codex", updated.CurrentAgent)
	}

	// Tmux: the agent:* pane's role tag should now be agent:codex.
	panes, err := mgr.Tmux.LookupAllPanes(context.Background(), ws.TmuxSessionName(), "agent:*")
	if err != nil {
		t.Fatalf("LookupAllPanes: %v", err)
	}
	if len(panes) != 1 {
		t.Fatalf("agent:* pane count = %d; want 1", len(panes))
	}
	if panes[0].Role != "agent:codex" {
		t.Errorf("agent pane role = %q; want agent:codex", panes[0].Role)
	}

	// IDE + shell panes are still present (not killed by the swap).
	idePanes, err := mgr.Tmux.LookupAllPanes(context.Background(), ws.TmuxSessionName(), "ide")
	if err != nil || len(idePanes) != 1 {
		t.Errorf("IDE pane after swap: count=%d err=%v; want exactly 1", len(idePanes), err)
	}
	shellPanes, err := mgr.Tmux.LookupAllPanes(context.Background(), ws.TmuxSessionName(), "terminal:shell")
	if err != nil || len(shellPanes) != 1 {
		t.Errorf("Shell pane after swap: count=%d err=%v; want exactly 1", len(shellPanes), err)
	}

	// State.json reflects the new agent (durable, not just in the
	// returned struct).
	store, err := state.NewStore(mgr.CanopyHome)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	row, err := st.Find(mgr.Cfg.ProjectRoot, ws.Name)
	if err != nil {
		t.Fatalf("state.Find: %v", err)
	}
	if row.CurrentAgent != "codex" {
		t.Errorf("state.json CurrentAgent = %q; want codex (persisted)", row.CurrentAgent)
	}
}

// TestSwapAgent_DisallowedAgent: trying to swap to an agent that's
// registered as a launcher but NOT in canopy.json's agents allowlist
// returns ErrAgentNotAllowed. The tmux session is left untouched.
func TestSwapAgent_DisallowedAgent(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixtureWithAgents(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "swap-disallowed", workspace.CreateOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// aider is a known launcher but NOT in our agents list.
	_, err = mgr.SwapAgent(context.Background(), ws.Name, "aider")
	if !errors.Is(err, agent.ErrAgentNotAllowed) {
		t.Fatalf("SwapAgent(aider) err = %v; want errors.Is(..., agent.ErrAgentNotAllowed)", err)
	}

	// State unchanged.
	store, _ := state.NewStore(mgr.CanopyHome)
	st, _ := store.Load()
	row, _ := st.Find(mgr.Cfg.ProjectRoot, ws.Name)
	if row.CurrentAgent != "claude" {
		t.Errorf("state.CurrentAgent after rejected swap = %q; want claude (unchanged)", row.CurrentAgent)
	}

	// Agent pane is still claude.
	panes, _ := mgr.Tmux.LookupAllPanes(context.Background(), ws.TmuxSessionName(), "agent:*")
	if len(panes) != 1 || panes[0].Role != "agent:claude" {
		t.Errorf("agent pane after rejected swap = %v; want exactly 1 agent:claude", panes)
	}
}

// TestSwapAgent_AlreadyCurrent: swapping to the same agent that's
// already running is rejected with ErrSwapAlreadyCurrent (typo guard +
// avoid pointless kill/respawn churn).
func TestSwapAgent_AlreadyCurrent(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixtureWithAgents(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "swap-noop", workspace.CreateOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = mgr.SwapAgent(context.Background(), ws.Name, "claude")
	if !errors.Is(err, workspace.ErrSwapAlreadyCurrent) {
		t.Fatalf("SwapAgent(claude→claude) err = %v; want ErrSwapAlreadyCurrent", err)
	}
}

// Note: an integration test for the codex review P1 #3 fix
// ("VerifyInstalled before kill-pane") was attempted but PATH poisoning
// also breaks the tmux/git binaries SwapAgent reaches BEFORE the
// launcher check, making it impossible to isolate "agent binary
// missing" from the test harness. The fix lives at agent_swap.go
// step 2 (launcher.Resolve + VerifyInstalled before any tmux op);
// future test could mock VerifyInstalled via an injectable Resolver
// on Manager.

// TestSwapAgent_FirstSwapUsesFresh_SecondSwapResumes pins the v0.22
// per-(workspace, agent) launch-counter semantics:
//
//   - On the FIRST swap to an agent that's never run in this
//     workspace, AgentLaunches[target]==0 → spawn with Fresh argv
//     (claude without --continue, codex without resume), so the
//     agent doesn't fail with "No conversation found to continue".
//   - On the SECOND swap to that same agent (after at least one
//     prior launch + a swap-away), AgentLaunches[target]>0 → spawn
//     with Resume argv. The agent's own resume verb reaches its
//     prior session.
//
// This test inspects state.json's AgentLaunches map after each swap
// rather than the actual argv (which would require capturing the
// spawned process's args — possible but heavier). The map is the
// source of truth that drives the decision.
func TestSwapAgent_FirstSwapUsesFresh_SecondSwapResumes(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixtureWithAgents(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "swap-resume", workspace.CreateOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// After Create, AgentLaunches should record one claude launch.
	{
		store, _ := state.NewStore(mgr.CanopyHome)
		st, _ := store.Load()
		row, _ := st.Find(mgr.Cfg.ProjectRoot, ws.Name)
		if got := row.AgentLaunches["claude"]; got != 1 {
			t.Errorf("post-Create AgentLaunches[claude] = %d; want 1", got)
		}
		if got := row.AgentLaunches["codex"]; got != 0 {
			t.Errorf("post-Create AgentLaunches[codex] = %d; want 0", got)
		}
	}

	// First swap claude → codex. codex has never launched in this
	// workspace; AgentLaunches[codex] should be 0 BEFORE the swap
	// fires (so SwapAgent picks Fresh), then bumped to 1 AFTER.
	if _, err := mgr.SwapAgent(context.Background(), ws.Name, "codex"); err != nil {
		t.Fatalf("SwapAgent claude→codex: %v", err)
	}
	{
		store, _ := state.NewStore(mgr.CanopyHome)
		st, _ := store.Load()
		row, _ := st.Find(mgr.Cfg.ProjectRoot, ws.Name)
		if got := row.AgentLaunches["codex"]; got != 1 {
			t.Errorf("post-first-swap AgentLaunches[codex] = %d; want 1", got)
		}
	}

	// Swap back codex → claude. claude has 1 prior launch (from
	// Create), so swap-back should pick Resume.
	if _, err := mgr.SwapAgent(context.Background(), ws.Name, "claude"); err != nil {
		t.Fatalf("SwapAgent codex→claude: %v", err)
	}
	{
		store, _ := state.NewStore(mgr.CanopyHome)
		st, _ := store.Load()
		row, _ := st.Find(mgr.Cfg.ProjectRoot, ws.Name)
		if got := row.AgentLaunches["claude"]; got != 2 {
			t.Errorf("post-swap-back AgentLaunches[claude] = %d; want 2", got)
		}
	}
}

// TestMigrateCurrentAgents_BackfillsAgentLaunches: a pre-v0.22 row
// with non-zero AgentLaunchCount and nil AgentLaunches should get
// AgentLaunches populated from the legacy total counter under the
// "all prior launches were CurrentAgent" assumption (strictly true
// before swap existed).
func TestMigrateCurrentAgents_BackfillsAgentLaunches(t *testing.T) {
	stateDir := t.TempDir()
	projA := t.TempDir()

	store, err := state.NewStore(stateDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	preMigration := &state.State{
		Workspaces: []state.Workspace{
			{
				ProjectRoot:      projA,
				Name:             "legacy",
				Branch:           "legacy",
				Status:           state.StatusReady,
				CurrentAgent:     "claude",
				AgentLaunchCount: 5,
				// AgentLaunches intentionally nil — pre-v0.22 shape
			},
		},
	}
	if err := store.Save(preMigration); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfgJSON := `{"scripts":{"setup":"x","run":"x","archive":"x"},"agents":["claude","codex"]}`
	if err := os.WriteFile(filepath.Join(projA, "canopy.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}
	cfg, err := config.DiscoverAndLoad(projA)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}

	mgr := &workspace.Manager{
		Cfg:        cfg,
		Store:      store,
		Tmux:       tmux.WithSocket("canopy-migrate-launches-test"),
		CanopyHome: stateDir,
		Settings:   settings.Settings{},
	}
	if err := workspace.RunMigrateCurrentAgentsForTest(mgr); err != nil {
		t.Fatalf("RunMigrateCurrentAgentsForTest: %v", err)
	}

	st, _ := store.Load()
	row, _ := st.Find(projA, "legacy")
	if row.AgentLaunches == nil {
		t.Fatal("AgentLaunches still nil after migration")
	}
	if got := row.AgentLaunches["claude"]; got != 5 {
		t.Errorf("AgentLaunches[claude] = %d; want 5 (mirrors AgentLaunchCount)", got)
	}
}

// TestMigrateCurrentAgents_PreV022Row: a row written before v0.22 (no
// CurrentAgent field, JSON omits it) gets populated to the project's
// default agent the first time a Manager constructs for that project.
// Cross-project rows are NOT touched — verifies the per-project scope
// of the migration.
func TestMigrateCurrentAgents_PreV022Row(t *testing.T) {
	stateDir := t.TempDir()
	projA := t.TempDir()
	projB := t.TempDir()

	// Hand-write a state.json with two rows: one for projA (this
	// manager's project), one for projB (someone else's). Both have
	// empty CurrentAgent — simulating the pre-v0.22 on-disk shape.
	store, err := state.NewStore(stateDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	preMigration := &state.State{
		Workspaces: []state.Workspace{
			{ProjectRoot: projA, Name: "alpha", Branch: "alpha", Status: state.StatusReady},
			{ProjectRoot: projB, Name: "beta", Branch: "beta", Status: state.StatusReady},
		},
	}
	if err := store.Save(preMigration); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Build a canopy.json for projA that declares agents: ["codex", "claude"]
	// so the default agent is codex (not claude).
	cfgJSON := `{
		"scripts": {"setup": "x", "run": "x", "archive": "x"},
		"agents": ["codex", "claude"]
	}`
	if err := os.WriteFile(filepath.Join(projA, "canopy.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}
	cfg, err := config.DiscoverAndLoad(projA)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}

	// Manually compose a Manager (bypassing workspace.New since New
	// owns CanopyHome resolution and we want stateDir, not ~/.canopy).
	// Call migrateCurrentAgents indirectly by constructing the same
	// way New does, minus settings noise.
	mgr := &workspace.Manager{
		Cfg:        cfg,
		Store:      store,
		Tmux:       tmux.WithSocket("canopy-migrate-test"),
		CanopyHome: stateDir,
		Settings:   settings.Settings{},
	}
	// MigrateAgents is exported via the Manager's method on construction;
	// New() calls it via workspace.New. Here we exercise the same code
	// path by re-running it directly. The test helper exists so tests
	// can invoke without going through the home-dir setup.
	if err := workspace.RunMigrateCurrentAgentsForTest(mgr); err != nil {
		t.Fatalf("MigrateCurrentAgents: %v", err)
	}

	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load post-migration: %v", err)
	}
	got := map[string]string{}
	for _, w := range st.Workspaces {
		got[w.Name] = w.CurrentAgent
	}
	if got["alpha"] != "codex" {
		t.Errorf("projA row CurrentAgent = %q; want codex (project default)", got["alpha"])
	}
	// projB row stays empty: this Manager isn't the right context to
	// migrate it (would need projB's canopy.json to know the default).
	if got["beta"] != "" {
		t.Errorf("projB row CurrentAgent = %q; want empty (cross-project, untouched)", got["beta"])
	}
}
