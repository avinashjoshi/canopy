package tmux_test

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestKill_ReapsDetachedNvimEmbed: regression test for the
// `nvim --embed` orphan accumulation discovered 2026-04-29 (~50
// processes, ~1.5GB). Spawn an nvim pane in a tmux session, observe
// its `nvim --embed` child, then call Kill — verify the embed child
// is dead afterward, not just the tmux session.
//
// Without the CWD-scan in collectPanePIDs, this test fails: nvim
// --embed deliberately detaches its session on launch (so it can
// outlive its launcher when used as a programmatic editor backend),
// which means the kill-session SIGHUP cascade doesn't reach it.
// The fix scans /proc/*/cwd for processes whose cwd matches a pane's
// cwd, catching detached children regardless of parent PID.
func TestKill_ReapsDetachedNvimEmbed(t *testing.T) {
	requireTmux(t)
	if _, err := exec.LookPath("nvim"); err != nil {
		t.Skip("nvim not on PATH; skipping reap test")
	}
	c := newClient(t)
	ctx := context.Background()

	cwd := t.TempDir()
	session := "reap-test"
	// Start an interactive nvim pane. nvim immediately forks
	// `nvim --embed .` as its editor backend.
	if err := c.Create(ctx, session, cwd, "nvim ."); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Give nvim a moment to fully launch and fork its --embed child.
	// 200ms is enough on a real system (nvim's startup is dominated
	// by plugin loading; the embed fork itself is microseconds, but
	// we want the fork plus child's own initialization to settle so
	// /proc/<pid>/cwd is set).
	time.Sleep(200 * time.Millisecond)

	embedPID := findNvimEmbedAtCWD(t, cwd)
	if embedPID == 0 {
		t.Skip("no nvim --embed child found within 200ms — nvim may have failed to launch in this minimal env; skipping")
	}

	if err := c.Kill(ctx, session); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Give the kill cascade + reap a beat to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(embedPID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("nvim --embed (PID %d) still alive 2s after Kill — reap regressed (workspace would leak this process to PID 1)", embedPID)
}

// TestKillServerAndReap_ReapsDetachedNvimEmbed: regression test for
// the symmetric leak path discovered 2026-05-01 (~12 orphans, ~3.5GB
// of zombie RAM accumulated from canopy's own e2e suite).
//
// Client.Kill (production workspace removal) routes the detach-on-
// launch reap through cwdScanForReap. KillServerAndReap (test
// cleanup) historically did NOT — it only collected pane-tree PIDs.
// Result: every e2e test that spawned an `nvim .` pane leaked one
// nvim --embed orphan to systemd-user. The fix unifies both paths
// on cwdScanForReap; this test guards the union.
//
// Without the cwd-scan in KillServerAndReap, this test fails the same
// way TestKill_ReapsDetachedNvimEmbed would without it: the embed
// child outlives the kill-server cascade and shows up as a 700MB+
// zombie a day later.
func TestKillServerAndReap_ReapsDetachedNvimEmbed(t *testing.T) {
	requireTmux(t)
	if _, err := exec.LookPath("nvim"); err != nil {
		t.Skip("nvim not on PATH; skipping reap test")
	}
	c := newClient(t)
	ctx := context.Background()

	cwd := t.TempDir()
	session := "reap-server-test"
	if err := c.Create(ctx, session, cwd, "nvim ."); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Same 200ms settle window as the Kill variant — nvim's startup is
	// dominated by plugin loading, the --embed fork itself is microseconds.
	time.Sleep(200 * time.Millisecond)

	embedPID := findNvimEmbedAtCWD(t, cwd)
	if embedPID == 0 {
		t.Skip("no nvim --embed child found within 200ms — nvim may have failed to launch in this minimal env; skipping")
	}

	if err := c.KillServerAndReap(ctx); err != nil {
		t.Fatalf("KillServerAndReap: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(embedPID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("nvim --embed (PID %d) still alive 2s after KillServerAndReap — symmetric reap regressed (e2e tests would leak this process to systemd-user)", embedPID)
}

// findNvimEmbedAtCWD walks /proc looking for a process whose comm is
// "nvim" and whose cwd points at the given path. Returns 0 if not
// found. Useful because the embed PID isn't known up front; nvim
// forks it itself.
func findNvimEmbedAtCWD(t *testing.T, wantCWD string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		// cmdline is null-separated; we want a command containing "--embed".
		cmd := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !strings.Contains(cmd, "nvim") || !strings.Contains(cmd, "--embed") {
			continue
		}
		cwd, err := os.Readlink("/proc/" + entry.Name() + "/cwd")
		if err != nil {
			continue
		}
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		if cwd == wantCWD {
			return pid
		}
	}
	return 0
}

// pidAlive reports whether a PID is currently alive. kill -0 is the
// canonical "exists" probe — sends no signal, just permission-checks.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run()
	return err == nil
}
