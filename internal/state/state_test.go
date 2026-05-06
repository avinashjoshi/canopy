package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/state"
)

func TestMain(m *testing.M) {
	teardown, err := clog.Init(false)
	if err != nil {
		_ = err
	}
	defer teardown()
	m.Run()
}

// TestLoad_Missing covers the first-run case: no state.json on disk yet.
// Load must return an empty State with SchemaVersion set, not error out.
func TestLoad_Missing(t *testing.T) {
	t.Parallel()
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.SchemaVersion != state.SchemaVersion {
		t.Errorf("SchemaVersion = %d; want %d", st.SchemaVersion, state.SchemaVersion)
	}
	if len(st.Workspaces) != 0 {
		t.Errorf("Workspaces len = %d; want 0", len(st.Workspaces))
	}
}

// TestSave_RoundTrip covers the basic write+read cycle.
func TestSave_RoundTrip(t *testing.T) {
	t.Parallel()
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	original := &state.State{
		SchemaVersion: state.SchemaVersion,
		Workspaces: []state.Workspace{
			{
				ProjectRoot: "/home/avi/Work/cravd",
				Name:        "bold-falcon",
				Branch:      "bold-falcon",
				Path:        "/home/avi/Work/cravd/worktrees/bold-falcon",
				Port:        3001,
				Status:      state.StatusReady,
				CreatedAt:   time.Now().UTC().Round(time.Second),
			},
		},
	}
	if err := store.Save(original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Workspaces) != 1 {
		t.Fatalf("loaded len = %d; want 1", len(loaded.Workspaces))
	}
	got := loaded.Workspaces[0]
	want := original.Workspaces[0]
	if got.Name != want.Name || got.Port != want.Port || got.Status != want.Status {
		t.Errorf("loaded mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestSave_RoundTrip_v06Fields verifies the v0.6 additions
// (AgentLaunchCount, SourceKind) survive a Save → Load round-trip and
// preserve omitempty behavior for zero values.
func TestSave_RoundTrip_v06Fields(t *testing.T) {
	t.Parallel()
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	original := &state.State{
		SchemaVersion: state.SchemaVersion,
		Workspaces: []state.Workspace{
			{
				ProjectRoot:      "/home/avi/Work/cravd",
				Name:             "review-pr-142",
				Branch:           "feat/oauth",
				Path:             "/home/avi/.canopy/workspaces/cravd/review-pr-142",
				Port:             3010,
				Status:           state.StatusReady,
				CreatedAt:        time.Now().UTC().Round(time.Second),
				AgentLaunchCount: 3,
				SourceKind:       "pr",
			},
			{
				// Workspace with zero-value v0.6 fields — should omit
				// from JSON via omitempty so we don't bloat existing
				// state.json files for users who haven't created
				// anything via the new flags.
				ProjectRoot: "/home/avi/Work/canopy",
				Name:        "fresh-falcon",
				Branch:      "fresh-falcon",
				Path:        "/home/avi/.canopy/workspaces/canopy/fresh-falcon",
				Port:        4000,
				Status:      state.StatusSettingUp,
				CreatedAt:   time.Now().UTC().Round(time.Second),
				// AgentLaunchCount: 0 (omitempty kicks in)
				// SourceKind: "" (omitempty kicks in)
			},
		},
	}
	if err := store.Save(original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Workspaces) != 2 {
		t.Fatalf("loaded len = %d; want 2", len(loaded.Workspaces))
	}

	// Workspace[0]: v0.6 fields populated, must survive round-trip.
	got := loaded.Workspaces[0]
	if got.AgentLaunchCount != 3 {
		t.Errorf("AgentLaunchCount = %d; want 3", got.AgentLaunchCount)
	}
	if got.SourceKind != "pr" {
		t.Errorf("SourceKind = %q; want %q", got.SourceKind, "pr")
	}

	// Workspace[1]: zero values, must NOT appear in serialized JSON
	// (omitempty). Reading them back gives zero values either way, so
	// we verify by inspecting the on-disk file.
	got2 := loaded.Workspaces[1]
	if got2.AgentLaunchCount != 0 {
		t.Errorf("AgentLaunchCount on fresh = %d; want 0", got2.AgentLaunchCount)
	}
	if got2.SourceKind != "" {
		t.Errorf("SourceKind on fresh = %q; want empty", got2.SourceKind)
	}

	// Verify the on-disk JSON omits zero-value v0.6 fields. Reading
	// state.json's bytes and grepping for the field names is the
	// honest way to confirm omitempty is doing its job.
	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	rawStr := string(raw)

	// Workspace[0] has both fields set, so they MUST appear.
	if !contains(rawStr, "agent_launch_count") {
		t.Errorf("expected agent_launch_count to appear in state.json for populated workspace; got:\n%s", rawStr)
	}
	if !contains(rawStr, "source_kind") {
		t.Errorf("expected source_kind to appear in state.json for populated workspace; got:\n%s", rawStr)
	}
	// Workspace[1]'s zero values should NOT appear; counting occurrences
	// of the field tags should match the populated count (just one).
	if got := count(rawStr, "agent_launch_count"); got != 1 {
		t.Errorf("agent_launch_count appeared %d times; want 1 (only populated workspace)", got)
	}
	if got := count(rawStr, "source_kind"); got != 1 {
		t.Errorf("source_kind appeared %d times; want 1 (only populated workspace)", got)
	}
}

// contains is a tiny helper to keep the round-trip test readable without
// pulling in strings.* into the test file.
func contains(haystack, needle string) bool {
	return count(haystack, needle) > 0
}

// count returns the number of occurrences of needle in haystack. Used to
// verify omitempty serialization (1 occurrence = only the populated
// workspace; 0 = neither).
func count(haystack, needle string) int {
	n := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			n++
		}
	}
	return n
}

// TestSave_AtomicRename verifies the tmpfile+rename pattern: a state.json
// that would crash mid-write leaves either the old contents or the new,
// never half-written JSON. We can't easily simulate a crash mid-write, but
// we can verify no .tmp file is left behind on success.
func TestSave_AtomicRename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(&state.State{SchemaVersion: state.SchemaVersion}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected no leftover .tmp file; got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Errorf("state.json missing after Save: %v", err)
	}
}

// TestEnsureProjectBase covers first-come-first-served base assignment.
// Three projects in succession should get bases at firstBase, firstBase+stride,
// firstBase+2*stride. Re-asking for an existing project returns the same base
// without allocating a new one.
func TestEnsureProjectBase(t *testing.T) {
	t.Parallel()
	st := &state.State{SchemaVersion: state.SchemaVersion}

	// First project gets the first base, isNew=true.
	base1, isNew1, err := st.EnsureProjectBase("cravd", 3000, 1000, 100)
	if err != nil || !isNew1 || base1 != 3000 {
		t.Errorf("first call: got base=%d isNew=%v err=%v; want 3000, true, nil", base1, isNew1, err)
	}

	// Second project gets the next base.
	base2, isNew2, _ := st.EnsureProjectBase("brain", 3000, 1000, 100)
	if base2 != 4000 || !isNew2 {
		t.Errorf("second call: got base=%d isNew=%v; want 4000, true", base2, isNew2)
	}

	// Re-ask for the first project — same base, isNew=false.
	rebase1, isNew1Again, _ := st.EnsureProjectBase("cravd", 3000, 1000, 100)
	if rebase1 != 3000 || isNew1Again {
		t.Errorf("re-ask cravd: got base=%d isNew=%v; want 3000, false", rebase1, isNew1Again)
	}

	// Third project after another -> 5000.
	base3, _, _ := st.EnsureProjectBase("hey-cli", 3000, 1000, 100)
	if base3 != 5000 {
		t.Errorf("third project: got base=%d; want 5000", base3)
	}
}

// TestEnsureProjectBase_LazyInitEntry covers the bug where `canopy init`
// pre-registers a project with PortBase=0 (lazy allocation by design) and
// the first `canopy new` should allocate a real base instead of returning
// 0 — which would feed port.Allocate the privileged-port range [10, 999]
// and fail with "no ports available".
func TestEnsureProjectBase_LazyInitEntry(t *testing.T) {
	t.Parallel()
	st := &state.State{
		SchemaVersion: state.SchemaVersion,
		Projects: map[string]state.ProjectMeta{
			"/Work/cravd": {Root: "/Work/cravd", PortBase: 3000},
			// init pre-registered fizzy with a zero base.
			"/Work/fizzy": {Root: "/Work/fizzy"},
		},
	}

	base, isNew, err := st.EnsureProjectBase("/Work/fizzy", 3000, 1000, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isNew {
		t.Errorf("isNew=false; want true (this is the first allocation for fizzy)")
	}
	// Should skip 3000 (cravd's base) and not collide with the zero in
	// fizzy's pre-existing entry — 4000 is the next free slot.
	if base != 4000 {
		t.Errorf("base=%d; want 4000 (skip cravd's 3000, ignore fizzy's lazy zero)", base)
	}
	if got := st.Projects["/Work/fizzy"].PortBase; got != 4000 {
		t.Errorf("persisted PortBase=%d; want 4000", got)
	}
}

// TestEnsureProjectBase_Exhaustion: maxProjects guard kicks in.
func TestEnsureProjectBase_Exhaustion(t *testing.T) {
	t.Parallel()
	st := &state.State{SchemaVersion: state.SchemaVersion}
	for i := 0; i < 3; i++ {
		_, _, err := st.EnsureProjectBase(string(rune('a'+i)), 3000, 1000, 3)
		if err != nil {
			t.Fatalf("project #%d: %v", i, err)
		}
	}
	// 4th project should hit the cap.
	_, _, err := st.EnsureProjectBase("d", 3000, 1000, 3)
	if err == nil {
		t.Error("4th project with max=3: want error; got nil")
	}
}

// TestState_AddFindRemove covers the in-memory CRUD on the State struct.
// In v2, the (projectRoot, name) tuple is the key — ProjectRoot is the
// canonical absolute path. Project (basename) is preserved on rows for
// backward compat but isn't used for lookup.
func TestState_AddFindRemove(t *testing.T) {
	t.Parallel()
	st := &state.State{SchemaVersion: state.SchemaVersion}

	const root = "/home/avi/Work/cravd"
	w := state.Workspace{
		ProjectRoot: root,
		Name:        "bold-falcon",
		Status:      state.StatusReady,
	}
	if err := st.Add(w); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Duplicate add must error with sentinel.
	if err := st.Add(w); !errors.Is(err, state.ErrAlreadyExists) {
		t.Fatalf("duplicate Add: got %v; want errors.Is(... ErrAlreadyExists)", err)
	}

	// Find by (projectRoot, name).
	found, err := st.Find(root, "bold-falcon")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.Name != "bold-falcon" {
		t.Errorf("Find got %q; want bold-falcon", found.Name)
	}

	// Find returns ErrNotFound for unknown.
	_, err = st.Find(root, "missing")
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Find(missing): got %v; want errors.Is(... ErrNotFound)", err)
	}

	// Find by basename does NOT match in v2 (ProjectRoot is the key).
	if _, err := st.Find("cravd", "bold-falcon"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Find by basename should return ErrNotFound in v2; got %v", err)
	}

	// Remove and verify.
	if err := st.Remove(root, "bold-falcon"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(st.Workspaces) != 0 {
		t.Errorf("after Remove, len = %d; want 0", len(st.Workspaces))
	}
	if err := st.Remove(root, "bold-falcon"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Remove(missing): got %v; want errors.Is(... ErrNotFound)", err)
	}
}

// TestWithLock_HappyPath covers the basic load-mutate-save cycle through
// WithLock.
func TestWithLock_HappyPath(t *testing.T) {
	t.Parallel()
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.WithLock(func(s *state.State) error {
		return s.Add(state.Workspace{ProjectRoot: "/home/avi/Work/cravd", Name: "bold-falcon", Status: state.StatusReady})
	}); err != nil {
		t.Fatalf("WithLock: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Workspaces) != 1 {
		t.Fatalf("after WithLock, len = %d; want 1", len(loaded.Workspaces))
	}
}

// TestWithLock_FnErrorSkipsSave covers the case where fn returns an error:
// state.json should NOT be modified.
func TestWithLock_FnErrorSkipsSave(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	sentinel := errors.New("intentional fn failure")
	err = store.WithLock(func(s *state.State) error {
		s.Workspaces = append(s.Workspaces, state.Workspace{Name: "should-not-persist"})
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error; got %v", err)
	}

	// state.json should NOT contain the workspace fn tried to add.
	loaded, _ := store.Load()
	if len(loaded.Workspaces) != 0 {
		t.Errorf("after fn failure, persisted len = %d; want 0", len(loaded.Workspaces))
	}
}

// TestWithLock_ParallelWriters is the CRITICAL test from the design doc.
// Two goroutines both run WithLock concurrently; each appends a distinct
// workspace. After both complete, BOTH workspaces must be present.
//
// Without the flock, this would race: both goroutines load (empty slice),
// both append their own workspace, both save — the second save overwrites
// the first. We'd see len == 1.
//
// With the flock, the second WithLock blocks until the first releases,
// then sees the first's write before doing its own. We see len == 2.
//
// Run with -race to also catch any in-process data race in Store/State.
func TestWithLock_ParallelWriters(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)

	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			err := store.WithLock(func(s *state.State) error {
				// Sleep inside the critical section to amplify any race window.
				// 1ms * 10 goroutines means the test takes at least 10ms total
				// even when it succeeds — fine, sub-100ms in practice.
				time.Sleep(1 * time.Millisecond)
				return s.Add(state.Workspace{
					ProjectRoot: "/home/avi/Work/cravd",
					Name:        workspaceName(i),
					Status:      state.StatusReady,
				})
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("WithLock returned error: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Workspaces) != N {
		t.Fatalf("after %d parallel writers, persisted len = %d; want %d", N, len(loaded.Workspaces), N)
	}

	// Verify each writer's workspace landed (no overwrites).
	seen := map[string]bool{}
	for _, w := range loaded.Workspaces {
		seen[w.Name] = true
	}
	for i := 0; i < N; i++ {
		if !seen[workspaceName(i)] {
			t.Errorf("missing workspace #%d", i)
		}
	}
}

// workspaceName returns a stable per-index identifier so the parallel
// test can verify every writer's mutation persisted.
func workspaceName(i int) string {
	const names = "abcdefghijklmnopqrstuvwxyz"
	return "ws-" + string(names[i%len(names)])
}
