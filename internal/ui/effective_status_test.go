package ui

import (
	"context"
	"testing"
	"time"

	"github.com/oncactus/canopy/internal/state"
)

// TestEffectiveStatus_StaleReadyResurrects is the regression test for
// the Enter-on-stale-ready bug: a workspace row whose tmux session was
// killed out-of-band still reads Status=ready in state.json until the
// next Reconcile, but BuildGlobalRows freshly probes HasSession and
// stamps the truth on row.Alive. Before the fix, attachSelected
// dispatched on row.Status alone and the user got
// "tmux.AttachCmd(...): tmux: session not found".
//
// Bug reproducer that this guards against: kill a workspace's tmux
// session manually (`tmux kill-session -t <name>`), refresh the TUI
// without going through Reconcile, press Enter on the row.
func TestEffectiveStatus_StaleReadyResurrects(t *testing.T) {
	row := Row{
		IsMain: false,
		Status: state.StatusReady,
		Alive:  false, // session was killed out-of-band
	}
	if got, want := effectiveStatus(row), state.StatusStopped; got != want {
		t.Errorf("effectiveStatus(stale-ready row) = %q; want %q (REGRESSION: stale-ready Enter must route through resurrect)", got, want)
	}
}

// TestEffectiveStatus_HealthyReadyAttaches: the normal path. A ready
// row whose tmux session is alive routes through plain attach.
func TestEffectiveStatus_HealthyReadyAttaches(t *testing.T) {
	row := Row{
		IsMain: false,
		Status: state.StatusReady,
		Alive:  true,
	}
	if got, want := effectiveStatus(row), state.StatusReady; got != want {
		t.Errorf("effectiveStatus(live-ready row) = %q; want %q", got, want)
	}
}

// TestEffectiveStatus_StoppedPassesThrough: stopped rows always route
// through resurrect, regardless of Alive (which is informational for
// them, not authoritative).
func TestEffectiveStatus_StoppedPassesThrough(t *testing.T) {
	for _, alive := range []bool{true, false} {
		row := Row{IsMain: false, Status: state.StatusStopped, Alive: alive}
		if got, want := effectiveStatus(row), state.StatusStopped; got != want {
			t.Errorf("effectiveStatus(stopped row, alive=%v) = %q; want %q", alive, got, want)
		}
	}
}

// TestEffectiveStatus_BrokenPassesThrough: broken rows do not route to
// resurrect just because Alive is false. They surface the broken-state
// error message so the user runs retry/rm.
func TestEffectiveStatus_BrokenPassesThrough(t *testing.T) {
	row := Row{IsMain: false, Status: state.StatusBroken, Alive: false}
	if got, want := effectiveStatus(row), state.StatusBroken; got != want {
		t.Errorf("effectiveStatus(broken row) = %q; want %q (broken must NOT downgrade to stopped)", got, want)
	}
}

// TestEffectiveStatus_MainRowExcluded: IsMain rows are handled by a
// dedicated branch in attachSelected (EnsureMainSession). The helper
// must NOT downgrade them, even if Alive is false, so the dispatcher
// reaches the IsMain path with the original status.
func TestEffectiveStatus_MainRowExcluded(t *testing.T) {
	row := Row{IsMain: true, Status: state.StatusReady, Alive: false}
	if got, want := effectiveStatus(row), state.StatusReady; got != want {
		t.Errorf("effectiveStatus(main row, alive=false) = %q; want %q (main rows must not be downgraded)", got, want)
	}
}

// TestEffectiveStatus_SettingUpPassesThrough: a workspace mid-setup
// can't be resurrected — the setup hooks need to finish first. Surface
// the setting-up error message instead.
func TestEffectiveStatus_SettingUpPassesThrough(t *testing.T) {
	row := Row{IsMain: false, Status: state.StatusSettingUp, Alive: false}
	if got, want := effectiveStatus(row), state.StatusSettingUp; got != want {
		t.Errorf("effectiveStatus(setting_up row) = %q; want %q", got, want)
	}
}

// TestActionRefresh_InvalidatesCache: `r` busts the Mem cache so the
// next refresh re-probes instead of serving up-to-5s-old values.
// (The auto-load default removed the previous "first r flips a gate"
// behavior — now every render uses the cache, but explicit refresh
// always means "fresh data right now.")
func TestActionRefresh_InvalidatesCache(t *testing.T) {
	m := newTestModel(false)
	m.memCache = state.NewMemCache(time.Hour)
	// Plant a value in the cache via GetLoad with a stub probe.
	planted := state.LoadValue{RSS: 999, CPU: 12.3}
	probe := stubLoadProbe{val: planted}
	if _, err := m.memCache.GetLoad(context.Background(), probe, "ws"); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	_, _ = actionRefresh(m, teaKeyMsg{})

	// After invalidation, GetLoad should re-probe rather than serve
	// the cached value. We verify by switching to a different probe
	// value and observing GetLoad returns the new one.
	probe.val = state.LoadValue{RSS: 1, CPU: 99.9}
	got, err := m.memCache.GetLoad(context.Background(), probe, "ws")
	if err != nil {
		t.Fatalf("post-r GetLoad: %v", err)
	}
	if got != probe.val {
		t.Errorf("post-r GetLoad = %+v; want %+v (cache should have been invalidated)", got, probe.val)
	}
}

type stubLoadProbe struct {
	val state.LoadValue
}

func (s stubLoadProbe) SessionLoad(ctx context.Context, session string) (state.LoadValue, error) {
	return s.val, nil
}
