package clipboard

import (
	"os"
	"path/filepath"
	"testing"
)

// seedWrappers creates dummy wl-paste/wl-copy files at homeDir's
// .local/bin/. Used by tests that need both wrappers to "exist" so the
// Stat checks in ProbeBridgeStatus pass.
func seedWrappers(t *testing.T, homeDir string, names ...string) {
	t.Helper()
	bin := filepath.Join(homeDir, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, n := range names {
		path := filepath.Join(bin, n)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}
}

func TestProbeBridgeStatus_OffWhenWlPasteMissing(t *testing.T) {
	home := t.TempDir()
	seedWrappers(t, home, "wl-copy") // wl-paste deliberately absent
	if got := ProbeBridgeStatus(home); got != BridgeStatusOff {
		t.Errorf("status = %q, want %q", got, BridgeStatusOff)
	}
}

func TestProbeBridgeStatus_OffWhenWlCopyMissing(t *testing.T) {
	home := t.TempDir()
	seedWrappers(t, home, "wl-paste") // wl-copy deliberately absent
	if got := ProbeBridgeStatus(home); got != BridgeStatusOff {
		t.Errorf("status = %q, want %q", got, BridgeStatusOff)
	}
}

// TestProbeBridgeStatus_BridgedWhenBothWrappersPresent covers the
// current (post-OSC52) contract: "bridged" means the wrapper scripts
// are installed, nothing more. Unlike the pre-OSC52 probe, this
// deliberately does NOT invoke wl-paste at all — ProbeBridgeStatus
// runs inside `canopy ls --json`, called by the laptop over BatchMode
// SSH (no pty), and OSC 52 requires a real attached terminal to
// round-trip through. There is no way to verify liveness from this
// code path; see BridgeStatusBridged's doc comment.
func TestProbeBridgeStatus_BridgedWhenBothWrappersPresent(t *testing.T) {
	home := t.TempDir()
	seedWrappers(t, home, "wl-paste", "wl-copy")
	if got := ProbeBridgeStatus(home); got != BridgeStatusBridged {
		t.Errorf("status = %q, want %q", got, BridgeStatusBridged)
	}
}

func TestProbeBridgeStatus_OffWhenEmptyHomeDir(t *testing.T) {
	// Defensive: caller passed an empty homeDir. Don't crash; report
	// "off" deterministically.
	if got := ProbeBridgeStatus(""); got != BridgeStatusOff {
		t.Errorf("status = %q, want %q for empty homeDir", got, BridgeStatusOff)
	}
}
