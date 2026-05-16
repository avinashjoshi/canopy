package clipboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BridgeStatus enumerates what the remote canopy reports to the laptop
// about whether its clipboard bridge is wired up.
//
// String values match the wire format emitted in `canopy ls --json`'s
// `clipboard_bridge` field and stored in
// state.RemoteHostSnapshot.ClipboardBridge. Stable; do not rename.
type BridgeStatus string

const (
	// BridgeStatusUnknown is the zero value — used when probing isn't
	// possible (canopy older than v0.18 didn't emit the field; the
	// laptop's JSON decoder leaves the string empty).
	BridgeStatusUnknown BridgeStatus = ""

	// BridgeStatusOff: at least one wrapper script is missing from
	// ~/.local/bin/. The bridge was never installed on this host, OR
	// the user (or a stray uninstall) removed it.
	BridgeStatusOff BridgeStatus = "off"

	// BridgeStatusBridged: both wrappers present AND the wl-paste
	// wrapper successfully reports text/plain in --list-types output.
	// This is the "everything works" state.
	BridgeStatusBridged BridgeStatus = "bridged"

	// BridgeStatusBroken: wrappers present but the probe failed.
	// Common causes: laptop's clipboard-server daemon is down, SSH
	// RemoteForward not active for this connection, sshd disallows
	// stream-local forwarding, $XDG_RUNTIME_DIR/canopy/ doesn't exist
	// at runtime (laptop and remote in different runtime-dir layouts).
	BridgeStatusBroken BridgeStatus = "broken"
)

// probeRunner runs the wl-paste wrapper for the bridge probe. Default
// uses exec.Command; tests substitute a fake to assert behavior
// without needing the wrapper on disk.
type probeRunner func(path string, args ...string) ([]byte, error)

func defaultProbeRunner(path string, args ...string) ([]byte, error) {
	cmd := exec.Command(path, args...)
	// Stderr intentionally discarded — we key off exit code + stdout
	// content only. stderr noise (e.g., socat connection errors when
	// the daemon is down) shouldn't leak into the bridge-status
	// classification.
	return cmd.Output()
}

// ProbeBridgeStatus determines the current bridge state on the host
// where it's called. Designed to run inside the remote canopy's
// `canopy ls --json` invocation so the laptop's refresher gets bridge
// status in the same SSH round-trip as canopy_version (D2 in
// /plan-eng-review).
//
// Sequence:
//
//  1. Stat ~/.local/bin/wl-paste and ~/.local/bin/wl-copy. If either
//     is missing → "off".
//  2. Invoke `<homeDir>/.local/bin/wl-paste --list-types`. If the call
//     errors OR the output doesn't contain `text/plain`, that's a
//     "broken" verdict — the wrapper is there but can't reach the
//     daemon.
//  3. Otherwise → "bridged".
//
// Pure-Go; no SSH. The function takes its inputs (homeDir, runner) so
// tests can drive every branch without filesystem manipulation.
func ProbeBridgeStatus(homeDir string, run probeRunner) BridgeStatus {
	if homeDir == "" {
		// Without $HOME we can't even locate the wrappers. Treat as
		// "off" rather than "unknown" so the laptop's pill renders
		// something deterministic; the user will see the same status
		// across every refresh until they fix the env.
		return BridgeStatusOff
	}
	wlPaste := filepath.Join(homeDir, ".local", "bin", "wl-paste")
	wlCopy := filepath.Join(homeDir, ".local", "bin", "wl-copy")
	for _, p := range []string{wlPaste, wlCopy} {
		if _, err := os.Stat(p); err != nil {
			return BridgeStatusOff
		}
	}
	out, err := run(wlPaste, "--list-types")
	if err != nil {
		return BridgeStatusBroken
	}
	if !strings.Contains(string(out), "text/plain") {
		return BridgeStatusBroken
	}
	return BridgeStatusBridged
}

// DefaultProbeBridgeStatus is the production entry point. Resolves
// the home dir + uses exec.Command. Called by cmd/canopy/ls.go inside
// the JSON-output path.
func DefaultProbeBridgeStatus() BridgeStatus {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Warn("clipboard.probe.home", "err", err)
		return BridgeStatusOff
	}
	return ProbeBridgeStatus(home, defaultProbeRunner)
}
