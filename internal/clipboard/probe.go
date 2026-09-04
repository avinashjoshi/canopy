package clipboard

import (
	"os"
	"path/filepath"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("clipboard")

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

	// BridgeStatusBridged: both wrapper scripts are present on disk.
	//
	// This is a WEAKER guarantee than the name historically implied.
	// Before the OSC 52 rewrite, ProbeBridgeStatus additionally
	// invoked `wl-paste --list-types` and verified it actually
	// reached the laptop's clipboard daemon over the SSH RemoteForward
	// tunnel — a real liveness check. That's no longer possible: OSC
	// 52 requires a real attached terminal (tty) to round-trip
	// through, and this probe runs INSIDE `canopy ls --json`, invoked
	// by the laptop over BatchMode SSH (no pty, no tty, no terminal on
	// the other end to answer). There is structurally no way to verify
	// "does OSC 52 actually work on this host" from a background,
	// non-interactive connection — only from an ACTUALLY attached
	// session, which is exactly where the wrapper scripts themselves
	// already fail loudly (non-zero exit, clear stderr) if OSC 52
	// isn't working. So "bridged" here means only "the wrapper scripts
	// are installed", not "confirmed working right now" — matching
	// what canopy CAN actually observe from a refresh tick.
	BridgeStatusBridged BridgeStatus = "bridged"

	// BridgeStatusBroken is no longer emitted by ProbeBridgeStatus (see
	// BridgeStatusBridged's doc comment for why a background probe
	// can't distinguish "installed" from "installed and working").
	// Kept defined for wire-format stability — an older on-disk
	// remotes-cache.json snapshot, or a future manual/interactive
	// diagnostic, may still carry this value, and internal/ui/hosts
	// still renders a pill for it if it ever appears.
	BridgeStatusBroken BridgeStatus = "broken"
)

// ProbeBridgeStatus determines the current bridge state on the host
// where it's called. Designed to run inside the remote canopy's
// `canopy ls --json` invocation so the laptop's refresher gets bridge
// status in the same SSH round-trip as canopy_version (D2 in
// /plan-eng-review).
//
// Stats ~/.local/bin/wl-paste and ~/.local/bin/wl-copy: "off" if
// either is missing, "bridged" (installed, not verified — see
// BridgeStatusBridged) if both are present. This is deliberately just
// an existence check, not a liveness check — see BridgeStatusBridged's
// doc comment for why a background probe can't do more than that
// under the OSC 52 mechanism. Pure-Go; no SSH, no exec. The function
// takes homeDir as a parameter so tests can drive both branches
// without real filesystem manipulation of $HOME itself.
func ProbeBridgeStatus(homeDir string) BridgeStatus {
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
	return BridgeStatusBridged
}

// DefaultProbeBridgeStatus is the production entry point. Resolves
// the home dir. Called by cmd/canopy/ls.go inside the JSON-output
// path.
func DefaultProbeBridgeStatus() BridgeStatus {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Warn("clipboard.probe.home", "err", err)
		return BridgeStatusOff
	}
	return ProbeBridgeStatus(home)
}
