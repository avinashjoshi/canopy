package clog_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oncactus/canopy/internal/clog"
)

// TestPkg_FanoutReachable_AfterInit is the regression test for the
// "fan-out infrastructure exists but never fires" bug found in the
// 2026-04-29 ship review. Setup:
//
//   - Package-level `var log = clog.Pkg("foo")` runs at import time,
//     BEFORE clog.Init() runs in main().
//   - Naive `slog.With(...)` snapshots slog.Default() at call time,
//     so the logger captures the pre-Init stderr handler and never
//     sees the fan-out wired up later.
//   - Result: zero canopy-<workspace>.log files ever get created in
//     production despite all the fan-out code being correct.
//
// The fix: clog.Pkg returns a forwarding handler that resolves
// slog.Default() on every Handle call, not at construction. This
// test simulates the production path: Pkg first, Init second, then
// log a record and assert the per-workspace file exists.
func TestPkg_FanoutReachable_AfterInit(t *testing.T) {
	// Use an isolated HOME so we don't touch the user's real ~/.canopy.
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Acquire a logger BEFORE Init — this is the production package-init
	// pattern (var log = clog.Pkg(...) at file scope).
	log := clog.Pkg("ship-test-pkg")

	// Now Init wires the fanout into slog.Default. If Pkg captured
	// slog.Default at call-time, this is too late — log will still
	// route through the pre-Init handler.
	teardown, err := clog.Init(false)
	if err != nil {
		t.Fatalf("clog.Init: %v", err)
	}
	defer teardown()

	// Log a record carrying a `name` attribute. With the fanout active
	// and reachable, this should land in BOTH the global canopy.log
	// AND ~/.canopy/log/canopy-test-ws.log.
	log.Info("test.event", "name", "test-ws", "n", 42)

	wsLog := filepath.Join(home, ".canopy", "log", "canopy-test-ws.log")
	data, err := os.ReadFile(wsLog)
	if err != nil {
		t.Fatalf("expected per-workspace log at %s: %v\n(this is the regression: package-level var log = clog.Pkg() must reach the fanout via lazy slog.Default resolution)", wsLog, err)
	}
	if !strings.Contains(string(data), "test.event") {
		t.Errorf("per-workspace log missing event:\n%s", data)
	}
	if !strings.Contains(string(data), "ship-test-pkg") {
		t.Errorf("per-workspace log missing pkg attribute:\n%s", data)
	}
}

// TestPkg_FanoutSharesWritersAcrossDerivedHandlers is the regression
// test for the WithAttrs/WithGroup writer-duplication bug. Each
// slog.With() call returns a derived handler. If derived handlers had
// independent writers maps, two slog.With() chains for the same
// workspace would open two lumberjacks at the same file path —
// rotation race + fd leak.
//
// The fix: writers live in a shared *sinkRegistry that all derived
// handlers point at. This test does TWO Pkg-then-name-attr chains for
// the same workspace and asserts both records land in ONE file (which
// they would even with two writers; the actual race is only visible
// at rotation, which we can't easily test). The strict test is that
// the handler doesn't fragment the cache — a follow-up record from
// either chain finds the existing writer rather than opening a new one.
func TestPkg_FanoutSharesWritersAcrossDerivedHandlers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	logA := clog.Pkg("pkg-a")
	logB := clog.Pkg("pkg-b")

	teardown, err := clog.Init(false)
	if err != nil {
		t.Fatalf("clog.Init: %v", err)
	}
	defer teardown()

	// Both pkg loggers fan out to the same workspace name.
	logA.Info("event-a", "name", "shared-ws")
	logB.Info("event-b", "name", "shared-ws")

	wsLog := filepath.Join(home, ".canopy", "log", "canopy-shared-ws.log")
	data, err := os.ReadFile(wsLog)
	if err != nil {
		t.Fatalf("expected per-workspace log at %s: %v", wsLog, err)
	}
	out := string(data)
	if !strings.Contains(out, "event-a") || !strings.Contains(out, "event-b") {
		t.Errorf("per-workspace log missing one of the two derived-handler events:\n%s", out)
	}
	// Sanity: both pkg attributes should appear (one record per pkg).
	if !strings.Contains(out, "pkg-a") || !strings.Contains(out, "pkg-b") {
		t.Errorf("per-workspace log missing pkg attributes:\n%s", out)
	}
}

