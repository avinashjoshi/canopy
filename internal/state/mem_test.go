package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeMemProbe records calls so tests can assert cache hits/misses.
type fakeMemProbe struct {
	values map[string]int64
	calls  int
	err    error
}

func (f *fakeMemProbe) SessionRSS(ctx context.Context, session string) (int64, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	if v, ok := f.values[session]; ok {
		return v, nil
	}
	return 0, nil
}

// TestMemCache_HitWithinTTL: a second Get for the same session within
// the TTL window does NOT call the probe. This is the load-bearing
// claim of the cache — without it, every refresh tick spawns a `ps -A`
// per row.
func TestMemCache_HitWithinTTL(t *testing.T) {
	probe := &fakeMemProbe{values: map[string]int64{"foo": 12345}}
	mc := NewMemCache(time.Hour) // TTL way in the future

	got1, _ := mc.Get(context.Background(), probe, "foo")
	got2, _ := mc.Get(context.Background(), probe, "foo")

	if got1 != 12345 || got2 != 12345 {
		t.Errorf("cached Get values = (%d, %d); want (12345, 12345)", got1, got2)
	}
	if probe.calls != 1 {
		t.Errorf("probe.calls = %d; want 1 (second Get should hit cache)", probe.calls)
	}
}

// TestMemCache_MissAfterTTL: when the cached entry is older than TTL,
// the next Get re-probes. Uses a tiny TTL + sleep to avoid mocking time.
func TestMemCache_MissAfterTTL(t *testing.T) {
	probe := &fakeMemProbe{values: map[string]int64{"foo": 100}}
	mc := NewMemCache(20 * time.Millisecond)

	mc.Get(context.Background(), probe, "foo")
	time.Sleep(40 * time.Millisecond)
	mc.Get(context.Background(), probe, "foo")

	if probe.calls != 2 {
		t.Errorf("probe.calls = %d; want 2 (TTL expiry should re-probe)", probe.calls)
	}
}

// TestMemCache_Invalidate: explicit invalidate forces a re-probe even
// within TTL. Invariant for the K (kill) flow: just-killed row's RSS
// shouldn't lag the actual state by up to TTL.
func TestMemCache_Invalidate(t *testing.T) {
	probe := &fakeMemProbe{values: map[string]int64{"foo": 100}}
	mc := NewMemCache(time.Hour)

	mc.Get(context.Background(), probe, "foo")
	mc.Invalidate("foo")
	mc.Get(context.Background(), probe, "foo")

	if probe.calls != 2 {
		t.Errorf("probe.calls = %d; want 2 (Invalidate should force re-probe)", probe.calls)
	}
}

// TestMemCache_InvalidateAll: explicit `r` refresh busts everything.
func TestMemCache_InvalidateAll(t *testing.T) {
	probe := &fakeMemProbe{values: map[string]int64{"foo": 100, "bar": 200}}
	mc := NewMemCache(time.Hour)

	mc.Get(context.Background(), probe, "foo")
	mc.Get(context.Background(), probe, "bar")
	calls0 := probe.calls
	mc.InvalidateAll()
	mc.Get(context.Background(), probe, "foo")
	mc.Get(context.Background(), probe, "bar")

	if probe.calls-calls0 != 2 {
		t.Errorf("re-probes after InvalidateAll = %d; want 2", probe.calls-calls0)
	}
}

// TestMemCache_ProbeError_NotCached: a transient probe failure does
// NOT poison the cache for the TTL window. The next call gets to
// re-probe so a flaky tmux/ps doesn't lock the column at "—" for 5s.
func TestMemCache_ProbeError_NotCached(t *testing.T) {
	probe := &fakeMemProbe{err: errors.New("transient")}
	mc := NewMemCache(time.Hour)

	_, err := mc.Get(context.Background(), probe, "foo")
	if err == nil {
		t.Fatal("expected error from failing probe")
	}
	probe.err = nil
	probe.values = map[string]int64{"foo": 999}
	got, _ := mc.Get(context.Background(), probe, "foo")
	if got != 999 {
		t.Errorf("Get after error recovery = %d; want 999 (failed probes must not be cached)", got)
	}
	if probe.calls != 2 {
		t.Errorf("probe.calls = %d; want 2 (error should not have populated cache)", probe.calls)
	}
}

// TestMemCache_NilSafe: nil receivers and nil probes should not panic.
// Callers that don't want the column pass a nil cache and should still
// get a working build.
func TestMemCache_NilSafe(t *testing.T) {
	var mc *MemCache // nil
	got, err := mc.Get(context.Background(), nil, "foo")
	if got != 0 || err != nil {
		t.Errorf("nil cache.Get = (%d, %v); want (0, nil)", got, err)
	}
	mc.Invalidate("foo")    // must not panic
	mc.InvalidateAll()      // must not panic
	NewMemCache(0)          // zero TTL falls back to default
}

// TestBuildGlobalRowsWithMem_PopulatesAliveRows: live workspace rows
// get RSS values via the cache+probe; main rows and dead rows skip
// the probe (probe-on-dead returns ErrSessionNotFound, expensive).
func TestBuildGlobalRowsWithMem_PopulatesAliveRows(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/p/foo": {Root: "/p/foo", PortBase: 3000},
		},
		Workspaces: []Workspace{
			{ProjectRoot: "/p/foo", Name: "alive", Status: StatusReady, TmuxSession: "foo-alive"},
			{ProjectRoot: "/p/foo", Name: "dead", Status: StatusStopped, TmuxSession: "foo-dead"},
		},
	}
	live := &fakeProbe{alive: map[string]bool{"foo-alive": true}}
	mem := &fakeMemProbe{values: map[string]int64{"foo-alive": 42 * 1024 * 1024}}
	mc := NewMemCache(time.Hour)

	rows := s.BuildGlobalRowsWithMem(context.Background(), live, mem, mc)

	var aliveRow, deadRow, mainRow GlobalRow
	for _, r := range rows {
		switch {
		case r.IsMain:
			mainRow = r
		case r.Name == "alive":
			aliveRow = r
		case r.Name == "dead":
			deadRow = r
		}
	}
	if aliveRow.MemRSS != 42*1024*1024 {
		t.Errorf("alive row MemRSS = %d; want 44040192 (42MB)", aliveRow.MemRSS)
	}
	if deadRow.MemRSS != 0 {
		t.Errorf("dead row MemRSS = %d; want 0 (skip probe on dead)", deadRow.MemRSS)
	}
	if mainRow.MemRSS != 0 {
		t.Errorf("main row MemRSS = %d; want 0 (main rows skip probe)", mainRow.MemRSS)
	}
	if mem.calls != 1 {
		t.Errorf("mem.calls = %d; want 1 (only the alive workspace row should have triggered a probe)", mem.calls)
	}
}

// fakeLoadProbe satisfies LoadProbe with deterministic per-session
// load values for tests.
type fakeLoadProbe struct {
	values map[string]LoadValue
	calls  int
}

func (f *fakeLoadProbe) SessionLoad(ctx context.Context, session string) (LoadValue, error) {
	f.calls++
	if v, ok := f.values[session]; ok {
		return v, nil
	}
	return LoadValue{}, nil
}

// TestLoadCache_GetLoad_HitWithinTTL: GetLoad caches RSS+CPU together
// — second call inside TTL returns the same struct without re-probing.
func TestLoadCache_GetLoad_HitWithinTTL(t *testing.T) {
	probe := &fakeLoadProbe{values: map[string]LoadValue{
		"foo": {RSS: 100, CPU: 12.5},
	}}
	mc := NewMemCache(time.Hour)

	got1, _ := mc.GetLoad(context.Background(), probe, "foo")
	got2, _ := mc.GetLoad(context.Background(), probe, "foo")

	want := LoadValue{RSS: 100, CPU: 12.5}
	if got1 != want || got2 != want {
		t.Errorf("GetLoad caches = (%+v, %+v); want (%+v, %+v)", got1, got2, want, want)
	}
	if probe.calls != 1 {
		t.Errorf("probe.calls = %d; want 1 (second GetLoad should hit cache)", probe.calls)
	}
}

// TestBuildGlobalRowsWithLoad_PopulatesBoth: the load-aware build
// populates both MemRSS and CPU on alive workspace rows; main + dead
// rows skip the probe.
func TestBuildGlobalRowsWithLoad_PopulatesBoth(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{"/p/foo": {Root: "/p/foo"}},
		Workspaces: []Workspace{
			{ProjectRoot: "/p/foo", Name: "alive", Status: StatusReady, TmuxSession: "foo-alive"},
			{ProjectRoot: "/p/foo", Name: "dead", Status: StatusStopped, TmuxSession: "foo-dead"},
		},
	}
	live := &fakeProbe{alive: map[string]bool{"foo-alive": true}}
	probe := &fakeLoadProbe{values: map[string]LoadValue{
		"foo-alive": {RSS: 42 * 1024 * 1024, CPU: 7.5},
	}}
	mc := NewMemCache(time.Hour)

	rows := s.BuildGlobalRowsWithLoad(context.Background(), live, probe, mc)

	var alive, dead, main GlobalRow
	for _, r := range rows {
		switch {
		case r.IsMain:
			main = r
		case r.Name == "alive":
			alive = r
		case r.Name == "dead":
			dead = r
		}
	}
	if alive.MemRSS != 42*1024*1024 {
		t.Errorf("alive.MemRSS = %d; want 44040192", alive.MemRSS)
	}
	if alive.CPU != 7.5 {
		t.Errorf("alive.CPU = %v; want 7.5", alive.CPU)
	}
	if dead.MemRSS != 0 || dead.CPU != 0 {
		t.Errorf("dead row got load values populated; want zero (probe must skip)")
	}
	if main.MemRSS != 0 || main.CPU != 0 {
		t.Errorf("main row got load values populated; want zero (probe must skip)")
	}
	if probe.calls != 1 {
		t.Errorf("probe.calls = %d; want 1 (only the alive workspace row should have been probed)", probe.calls)
	}
}

// fakeProbe lives in listing_test.go in this package — reuse it.

// fakeProbeWithAttached extends the listing-test fakeProbe to also
// implement AttachedProbe so we can verify the type-assertion path
// in BuildGlobalRows lights up correctly.
type fakeProbeWithAttached struct {
	fakeProbe
	attached map[string]bool
}

func (f *fakeProbeWithAttached) AttachedSessions(ctx context.Context) (map[string]bool, error) {
	return f.attached, nil
}

// TestBuildGlobalRows_PopulatesAttached: when the probe also satisfies
// AttachedProbe, GlobalRow.Attached gets populated from the batch
// AttachedSessions call. Liveness-only probes (the existing tests'
// fakeProbe) don't trigger the path — backward-compat preserved.
func TestBuildGlobalRows_PopulatesAttached(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/p/foo": {Root: "/p/foo", PortBase: 3000},
		},
		Workspaces: []Workspace{
			{ProjectRoot: "/p/foo", Name: "current", Status: StatusReady, TmuxSession: "foo-current"},
			{ProjectRoot: "/p/foo", Name: "background", Status: StatusReady, TmuxSession: "foo-background"},
		},
	}
	probe := &fakeProbeWithAttached{
		fakeProbe: fakeProbe{alive: map[string]bool{
			"foo-current":    true,
			"foo-background": true,
		}},
		attached: map[string]bool{"foo-current": true},
	}

	rows := s.BuildGlobalRows(context.Background(), probe)
	var current, background GlobalRow
	for _, r := range rows {
		switch r.Name {
		case "current":
			current = r
		case "background":
			background = r
		}
	}
	if !current.Attached {
		t.Errorf("current.Attached = false; want true (probe says attached)")
	}
	if background.Attached {
		t.Errorf("background.Attached = true; want false")
	}
}

// TestBuildGlobalRows_LivenessOnlyProbe_NoAttached: a probe that does
// NOT implement AttachedProbe falls through cleanly — every row's
// Attached is left at zero value (false). Backward-compat for the
// listing_test fakeProbe and any other liveness-only callers.
func TestBuildGlobalRows_LivenessOnlyProbe_NoAttached(t *testing.T) {
	s := &State{
		Projects: map[string]ProjectMeta{
			"/p/foo": {Root: "/p/foo"},
		},
		Workspaces: []Workspace{
			{ProjectRoot: "/p/foo", Name: "ws", Status: StatusReady, TmuxSession: "foo-ws"},
		},
	}
	rows := s.BuildGlobalRows(context.Background(), &fakeProbe{alive: map[string]bool{"foo-ws": true}})
	for _, r := range rows {
		if r.Attached {
			t.Errorf("row %q: Attached = true; want false (liveness-only probe)", r.Name)
		}
	}
}
