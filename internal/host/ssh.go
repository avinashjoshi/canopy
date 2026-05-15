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

// SSHCmd builds an ssh invocation for INTERACTIVE dispatch (canopy new
// --on, canopy switch --on). BatchMode is off so first-time-user
// password prompts can surface in the terminal the user is actively
// attached to.
//
// For BACKGROUND polling (TUI refresh tick), use SSHCmdBatch instead —
// it sets BatchMode=yes so SSH never prompts for a password, which
// would otherwise hang the refresh goroutine AND corrupt the TUI
// render (SSH writes password prompts to /dev/tty, bypassing our
// stdout/stderr capture).
//
// ControlMaster: keeps one SSH connection alive per (user, host, port)
// tuple. The first call pays the full handshake cost (~50-300ms over
// tailscale-WAN). Subsequent calls reuse the socket and skip the
// handshake — measurable on the second `canopy ls --json` refresh
// tick. ControlPersist=300 keeps the master alive for 5 minutes after
// the last client exits, covering the polling window of an active
// canopy session without leaving zombie sockets forever.
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
	return sshCmdInternal(ctx, target, false /* not batch */, args...)
}

// SSHCmdBatch is the non-interactive variant: BatchMode=yes so SSH
// never prompts for a password. Required for background polling (the
// TUI's refresh tick): without it, a host with no key auth set up
// would hang the refresh goroutine on /dev/tty and visibly corrupt
// the Bubbletea render. With it, the same host fails fast with a
// "Permission denied (publickey)" error that the cache surfaces as
// a "host degraded" pill.
//
// Same ControlMaster / ConnectTimeout / ServerAlive options as the
// interactive variant; only BatchMode differs.
func SSHCmdBatch(ctx context.Context, target string, args ...string) *exec.Cmd {
	return sshCmdInternal(ctx, target, true /* batch */, args...)
}

// sshCmdInternal is the shared implementation. Splits on the batch
// flag, otherwise identical options.
func sshCmdInternal(ctx context.Context, target string, batch bool, args ...string) *exec.Cmd {
	socketPath := filepath.Join(canopyHome(), "ssh-%C.sock")
	sshArgs := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + socketPath,
		"-o", "ControlPersist=300",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
	}
	if batch {
		sshArgs = append(sshArgs,
			"-o", "BatchMode=yes",
			// Belt-and-suspenders: forbid SSH from asking for any password
			// in case BatchMode misses a path. NumberOfPasswordPrompts=0
			// disables interactive password auth entirely; the SSH client
			// gives up immediately if pubkey auth fails.
			"-o", "NumberOfPasswordPrompts=0",
		)
	}
	sshArgs = append(sshArgs, target)
	sshArgs = append(sshArgs, args...)
	log.Debug("ssh.cmd", "target", target, "batch", batch, "args", args)
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

// ExitControlMaster terminates the persistent SSH ControlMaster
// connection for `target` if one is alive. Used by code paths that
// modify SSH config (e.g., per-host RemoteForward snippets written
// by the v0.18 clipboard bridge): an already-open ControlMaster only
// carries the forwards it negotiated at handshake time, so a fresh
// snippet's directives don't apply until the NEXT master is opened.
//
// Calling this between "write snippet" and "use snippet via ssh" makes
// the next SSH command establish a new master that reads the freshly-
// written config. ControlPersist will keep that new master alive for
// the rest of the canopy session.
//
// Errors are deliberately swallowed: "no master alive" is the same
// outcome as "we killed one" from the caller's perspective (next ssh
// gets a fresh master). Returning nil here keeps callers from having
// to branch on a no-op condition.
func ExitControlMaster(target string) {
	socketPath := filepath.Join(canopyHome(), "ssh-%C.sock")
	cmd := exec.Command("ssh",
		"-o", "ControlPath="+socketPath,
		"-O", "exit",
		target,
	)
	// `ssh -O exit` writes "Exit request sent." on success or
	// "Control socket connect(...): No such file or directory" when
	// no master is alive. Both go to stderr; neither is interesting
	// to the user mid-install. Discard.
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Run()
	log.Debug("ssh.control-master.exit", "target", target)
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
