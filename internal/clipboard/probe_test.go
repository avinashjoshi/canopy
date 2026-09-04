package clipboard

import (
	"os"
	"path/filepath"
	"testing"
)

// seedWrappers creates wl-paste/wl-copy files at homeDir's .local/bin/
// with content carrying the OSC52 marker, i.e. simulating the CURRENT
// (post-rewrite) wrapper. Used by tests that need ProbeBridgeStatus to
// see a fully up-to-date install.
func seedWrappers(t *testing.T, homeDir string, names ...string) {
	t.Helper()
	seedWrapperContent(t, homeDir, "#!/bin/sh\necho osc52 52;c; marker\nexit 0\n", names...)
}

// seedStalePreOSC52Wrappers creates wl-paste/wl-copy files with content
// shaped like the pre-rewrite (daemon/socat-era) wrapper — present on
// disk, but missing the OSC52 marker entirely. Simulates a host that
// had the bridge installed before the OSC52 rewrite and never got
// upgraded.
func seedStalePreOSC52Wrappers(t *testing.T, homeDir string, names ...string) {
	t.Helper()
	seedWrapperContent(t, homeDir, "#!/bin/sh\n# Forwards through clip-copy.sock via socat\nexit 0\n", names...)
}

func seedWrapperContent(t *testing.T, homeDir, content string, names ...string) {
	t.Helper()
	bin := filepath.Join(homeDir, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, n := range names {
		path := filepath.Join(bin, n)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
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
// are installed AND carry the OSC52 marker, nothing more. Unlike the
// pre-OSC52 probe, this deliberately does NOT invoke wl-paste at all —
// ProbeBridgeStatus runs inside `canopy ls --json`, called by the
// laptop over BatchMode SSH (no pty), and OSC 52 requires a real
// attached terminal to round-trip through. There is no way to verify
// liveness from this code path; see BridgeStatusBridged's doc comment.
func TestProbeBridgeStatus_BridgedWhenBothWrappersPresent(t *testing.T) {
	home := t.TempDir()
	seedWrappers(t, home, "wl-paste", "wl-copy")
	if got := ProbeBridgeStatus(home); got != BridgeStatusBridged {
		t.Errorf("status = %q, want %q", got, BridgeStatusBridged)
	}
}

// TestProbeBridgeStatus_BridgedWithRealWrapperContent ties the probe
// directly to WrapperContent's actual production output (not a
// hand-rolled fixture), so a future change to the wrapper templates
// that accidentally drops the OSC52 marker would fail this test too.
func TestProbeBridgeStatus_BridgedWithRealWrapperContent(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, w := range []WrapperScript{WrapperWlPaste, WrapperWlCopy} {
		content, _, err := WrapperContent(w, "v0.24.0.0+test")
		if err != nil {
			t.Fatalf("WrapperContent(%q): %v", w, err)
		}
		if err := os.WriteFile(filepath.Join(bin, w.RemoteName()), []byte(content), 0o755); err != nil {
			t.Fatalf("write %s: %v", w, err)
		}
	}
	if got := ProbeBridgeStatus(home); got != BridgeStatusBridged {
		t.Errorf("status = %q, want %q for real wrapper content", got, BridgeStatusBridged)
	}
}

// TestProbeBridgeStatus_OffWhenWrapperPredatesOSC52Rewrite is the
// regression test for the auto-setup staleness bug: a host that had
// the clipboard bridge installed BEFORE the OSC52 rewrite still has
// the old wrapper sitting at ~/.local/bin/wl-copy and wl-paste — files
// exist, but they're dead scripts that try to reach a daemon
// (`canopy clipboard-server`) that no longer exists in the codebase.
// An existence-only probe would report "bridged" here, and
// maybeAutoSetupClipboardBridge (internal/ui/update_clipboard_autosetup.go)
// treats "bridged" as "nothing to do" — silently stranding every
// pre-rewrite install with a broken wrapper forever, since the
// `--remote <host>` auto-setup path never gets a signal to redeploy.
// The content check (osc52Marker) is what makes this probe correctly
// report "off" so auto-setup redeploys the current wrapper.
func TestProbeBridgeStatus_OffWhenWrapperPredatesOSC52Rewrite(t *testing.T) {
	home := t.TempDir()
	seedStalePreOSC52Wrappers(t, home, "wl-paste", "wl-copy")
	if got := ProbeBridgeStatus(home); got != BridgeStatusOff {
		t.Errorf("status = %q, want %q for a pre-OSC52 wrapper still on disk", got, BridgeStatusOff)
	}
}

// TestProbeBridgeStatus_OffWhenOnlyOneWrapperIsStale covers the mixed
// case — e.g. a partially-completed prior install, or one wrapper
// manually restored from a backup — where only one of the two files
// lacks the marker. Must report "off": a half-upgraded bridge isn't
// safe to call "bridged".
func TestProbeBridgeStatus_OffWhenOnlyOneWrapperIsStale(t *testing.T) {
	home := t.TempDir()
	seedWrappers(t, home, "wl-paste")
	seedStalePreOSC52Wrappers(t, home, "wl-copy")
	if got := ProbeBridgeStatus(home); got != BridgeStatusOff {
		t.Errorf("status = %q, want %q when one wrapper is stale", got, BridgeStatusOff)
	}
}

func TestProbeBridgeStatus_OffWhenEmptyHomeDir(t *testing.T) {
	// Defensive: caller passed an empty homeDir. Don't crash; report
	// "off" deterministically.
	if got := ProbeBridgeStatus(""); got != BridgeStatusOff {
		t.Errorf("status = %q, want %q for empty homeDir", got, BridgeStatusOff)
	}
}
