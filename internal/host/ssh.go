// Package host orchestrates multi-host workspace dispatch.
//
// v0.17.0 Phase 0 — minimal SSH + mosh command runners.
// Tower (or any SSH-reachable Linux box) hosts its own canopy installation;
// laptop's canopy invokes verbs on tower via this package. Each canopy
// installation is canonical for its own workspaces; laptop is the
// aggregator + UI.
//
// Future (Phase 1):
//   - hosts.json registry (`canopy host add/ls/rm`)
//   - remotes-cache.json read-side aggregator
//   - per-host goroutine refresh with 3s deadline
//   - version drift detection
//
// Phase 0 takes ssh-target literally via `--on <ssh-target>` flag.
// See docs/design/v0.17-remote-workspaces.md.
package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("host")

// canopyHome returns the laptop's ~/.canopy/ root. The directory is
// already created elsewhere (state.go ensures it on first run), so we
// just compute the path. If $HOME is unset we fall back to /tmp so
// SSH still works and the user sees a clear error message rather than
// a nil pointer.
func canopyHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), ".canopy")
	}
	return filepath.Join(home, ".canopy")
}

// SSHCmd builds an ssh invocation that dispatches `canopy <args>` to the
// remote target with ControlMaster multiplex enabled.
//
// ControlMaster keeps one SSH connection alive per (user, host, port)
// tuple. The first call pays the full handshake cost (~50-300ms over
// tailscale-WAN). Subsequent calls reuse the same socket and skip the
// handshake entirely — measurable on the second `canopy ls --json`
// refresh tick. ControlPersist=300 keeps the master alive for 5 minutes
// after the last client exits, which covers the polling window of an
// active canopy session without leaving zombie sockets forever.
//
// %C in ControlPath is ssh's per-target hash, so each distinct target
// gets its own socket and we don't multiplex unrelated connections.
//
// Caller wires stdin/stdout/stderr and Run / Start the returned Cmd.
//
//	cmd := host.SSHCmd(ctx, "avi@tower.tail-abc12.ts.net", "canopy", "ls", "--json")
//	cmd.Stdout = os.Stdout
//	cmd.Stderr = os.Stderr
//	err := cmd.Run()
func SSHCmd(ctx context.Context, target string, args ...string) *exec.Cmd {
	socketPath := filepath.Join(canopyHome(), "ssh-%C.sock")
	sshArgs := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + socketPath,
		"-o", "ControlPersist=300",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		// BatchMode=no by default so first-time-user password / key prompts
		// surface in the terminal. Once an SSH key is set up the prompt
		// never appears.
		target,
	}
	sshArgs = append(sshArgs, args...)
	log.Debug("ssh.cmd", "target", target, "args", args)
	return exec.CommandContext(ctx, "ssh", sshArgs...)
}

// MoshCmd builds a mosh invocation that attaches to the target host and
// runs `<args...>` there. Typically used as:
//
//	cmd := host.MoshCmd(ctx, "avi@tower.tail-abc12.ts.net", "canopy", "switch", "oauth-fix")
//	cmd.Stdin = os.Stdin
//	cmd.Stdout = os.Stdout
//	cmd.Stderr = os.Stderr
//	err := cmd.Run()  // or syscall.Exec for clean handoff
//
// mosh's first step is an SSH handshake to start mosh-server on the
// remote; after that it switches to UDP with state synchronization. The
// SSH handshake DOES reuse our ControlMaster socket if one is already
// open for the same target, so the spawn cost stays low when canopy has
// been polling the host.
//
// We don't pass extra mosh options here. Mosh-server's idle timeout is
// 7 days by default, which is correct for the canopy use case (laptop
// suspended for a long time, returns to attached session).
func MoshCmd(ctx context.Context, target string, args ...string) *exec.Cmd {
	// mosh syntax: `mosh <ssh-target> -- <command...>`
	moshArgs := []string{target, "--"}
	moshArgs = append(moshArgs, args...)
	log.Debug("mosh.cmd", "target", target, "args", args)
	return exec.CommandContext(ctx, "mosh", moshArgs...)
}

// CheckMoshAvailable returns nil if `mosh` is on PATH, else a helpful
// error. Called once at attach time so failure surfaces clearly rather
// than as a confusing exec error.
func CheckMoshAvailable() error {
	_, err := exec.LookPath("mosh")
	if err != nil {
		return &ErrMoshMissing{Inner: err}
	}
	return nil
}

// ErrMoshMissing indicates `mosh` isn't installed locally. Attaching to
// a remote workspace requires mosh on both ends. Phase 0 fails loudly;
// Phase 1 may surface this in the TUI as a host-level pill.
type ErrMoshMissing struct {
	Inner error
}

func (e *ErrMoshMissing) Error() string {
	return "mosh is not installed locally. Install with: sudo pacman -S mosh (Arch) / sudo apt install mosh (Debian). Mosh is required to attach to remote canopy workspaces."
}

func (e *ErrMoshMissing) Unwrap() error { return e.Inner }
