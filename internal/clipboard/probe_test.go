package clipboard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedWrappers creates dummy wl-paste/wl-copy files at homeDir's
// .local/bin/. Used by tests that need both wrappers to "exist" so the
// Stat checks in ProbeBridgeStatus pass before reaching the run step.
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
	got := ProbeBridgeStatus(home, func(string, ...string) ([]byte, error) {
		t.Error("runner should not be called when a wrapper is missing")
		return nil, nil
	})
	if got != BridgeStatusOff {
		t.Errorf("status = %q, want %q", got, BridgeStatusOff)
	}
}

func TestProbeBridgeStatus_OffWhenWlCopyMissing(t *testing.T) {
	home := t.TempDir()
	seedWrappers(t, home, "wl-paste") // wl-copy deliberately absent
	got := ProbeBridgeStatus(home, func(string, ...string) ([]byte, error) {
		t.Error("runner should not be called when a wrapper is missing")
		return nil, nil
	})
	if got != BridgeStatusOff {
		t.Errorf("status = %q, want %q", got, BridgeStatusOff)
	}
}

func TestProbeBridgeStatus_BridgedWhenProbeReportsTextPlain(t *testing.T) {
	home := t.TempDir()
	seedWrappers(t, home, "wl-paste", "wl-copy")
	got := ProbeBridgeStatus(home, func(path string, args ...string) ([]byte, error) {
		if len(args) != 1 || args[0] != "--list-types" {
			t.Errorf("expected wl-paste --list-types, got args=%v", args)
		}
		return []byte("text/plain;charset=utf-8\nimage/png\n"), nil
	})
	if got != BridgeStatusBridged {
		t.Errorf("status = %q, want %q", got, BridgeStatusBridged)
	}
}

func TestProbeBridgeStatus_BrokenWhenProbeErrors(t *testing.T) {
	home := t.TempDir()
	seedWrappers(t, home, "wl-paste", "wl-copy")
	got := ProbeBridgeStatus(home, func(string, ...string) ([]byte, error) {
		return nil, errors.New("timeout: socat: connection refused")
	})
	if got != BridgeStatusBroken {
		t.Errorf("status = %q, want %q", got, BridgeStatusBroken)
	}
}

func TestProbeBridgeStatus_BrokenWhenProbeOutputMissingTextPlain(t *testing.T) {
	// Wrapper ran, exit 0, but the output doesn't carry text/plain.
	// Could happen if the wrapper failed silently and emitted only
	// the image/png line (the conditional emit at the bottom of the
	// --list-types branch).
	home := t.TempDir()
	seedWrappers(t, home, "wl-paste", "wl-copy")
	got := ProbeBridgeStatus(home, func(string, ...string) ([]byte, error) {
		return []byte("image/png\n"), nil
	})
	if got != BridgeStatusBroken {
		t.Errorf("status = %q, want %q (wrapper output didn't include text/plain)", got, BridgeStatusBroken)
	}
}

func TestProbeBridgeStatus_OffWhenEmptyHomeDir(t *testing.T) {
	// Defensive: caller passed an empty homeDir. Don't crash; report
	// "off" deterministically.
	got := ProbeBridgeStatus("", func(string, ...string) ([]byte, error) { return nil, nil })
	if got != BridgeStatusOff {
		t.Errorf("status = %q, want %q for empty homeDir", got, BridgeStatusOff)
	}
}
