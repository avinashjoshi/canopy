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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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

// SSHRunUser builds an ssh invocation for ad-hoc commands that need
// to run as if the user typed them in a real terminal on the remote.
// Two things differ from SSHCmd:
//
//  1. The remote command is wrapped in `bash -lc` (login shell). SSH's
//     default non-interactive command does NOT source the user's
//     profile (.bashrc / .zshrc / .profile etc.), so $PATH is the bare
//     /usr/bin:/bin shape. Tools installed under ~/.local/bin or
//     ~/.cargo/bin etc. aren't visible without -l. Login shell sources
//     the user's full profile and produces the same $PATH they see in
//     a regular terminal.
//
//  2. -t forces remote pty allocation so the remote process can read
//     from /dev/tty. Required for any command that prompts the user —
//     git asking for an SSH passphrase or HTTPS credentials, an
//     editor opening, etc. Without -t the prompt would silently hang.
//
// remoteCmd is passed verbatim to `bash -lc`. The caller is responsible
// for shell-quoting any user-provided values inside it.
//
//	cmd := host.SSHRunUser(ctx, "avi@tower", "canopy init 'https://x'")
//	cmd.Stdin = os.Stdin
//	cmd.Stdout = os.Stdout
//	cmd.Stderr = os.Stderr
//	err := cmd.Run()
//
// Uses the same ControlMaster + ConnectTimeout config as SSHCmd so a
// previously-established master socket gets reused (no extra
// handshake cost on the second + Nth call).
func SSHRunUser(ctx context.Context, target string, remoteCmd string) *exec.Cmd {
	socketPath := filepath.Join(canopyHome(), "ssh-%C.sock")
	// Prepend $HOME/.local/bin to PATH defensively. `bash -l` SHOULD
	// source the user's profile and pick up ~/.local/bin (canopy's
	// conventional install dir), but plenty of real-world setups
	// don't: a minimal bashrc, a non-default login shell, an Arch box
	// where PATH is owned by /etc/profile, etc. Prepending here costs
	// nothing if the dir is already on PATH (duplicate entry, harmless)
	// and fixes the "canopy: command not found" failure mode users hit
	// the first time they `canopy init --on <host>`.
	//
	// $HOME and $PATH stay LITERAL through the outer-quote step below
	// — the wire-level shell strips the single quotes and bash -lc
	// then expands the variables when it parses its argument as a
	// shell command body. (Inside single quotes, expansion is
	// suppressed; once the outer shell unwraps them, the inner string
	// reaches bash with raw $HOME / $PATH tokens to expand.)
	withPath := `export PATH="$HOME/.local/bin:$PATH"; ` + remoteCmd
	// SSH joins all post-target argv with spaces and sends ONE string
	// to the remote shell. So `bash -lc <quoted-string>` must arrive
	// as 3 tokens — not 3+N tokens where N is the word count of
	// remoteCmd. Outer-shell-quote so the remote shell sees it as one
	// arg to bash -lc:
	//
	//   ssh ... target bash -lc 'canopy init '\''https://x'\'''
	//                            └─────────── one arg ──────────┘
	//
	// Pre-fix bug: a bare remoteCmd like "canopy init 'url'" became
	// `bash -lc canopy init 'url'` on the wire; bash -lc consumed
	// only "canopy", set $0 to "init", and the URL leaked into $1.
	// Symptom: "init: line 1: canopy: command not found".
	quoted := "'" + strings.ReplaceAll(withPath, "'", `'\''`) + "'"
	sshArgs := []string{
		"-t", // allocate remote pty for interactive auth prompts
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + socketPath,
		"-o", "ControlPersist=300",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		// "--" before target: without it, a target string shaped like an
		// option (e.g. "-oProxyCommand=...") is parsed by ssh as a FLAG,
		// not a hostname — confirmed by PoC to achieve local arbitrary
		// command execution. target ultimately traces back to
		// host.Host.SSHTarget, which a raw `--remote`/`--on` spec or a
		// tampered hosts.json entry can set to anything. "--" makes ssh
		// treat everything after it as positional, closing that off.
		"--", target,
		"bash", "-lc", quoted,
	}
	log.Debug("ssh.run-user", "target", target, "remote_cmd", remoteCmd)
	return exec.CommandContext(ctx, "ssh", sshArgs...)
}

// ShellSingleQuote wraps s in single quotes for safe embedding inside
// a shell command body. Embedded single quotes are escaped via the
// standard `'\''` trick (close-quote, escaped-quote, re-open-quote)
// so paths containing apostrophes still parse correctly.
//
// Exported so callers building remote shell commands for SSHRunUserBatch
// don't have to redo this each site. Pure function; no side effects.
func ShellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SSHRunUserBatch is the non-interactive sibling of SSHRunUser: runs
// the given remote command under `bash -lc` (login shell, full $PATH)
// but with BatchMode=yes and no remote pty allocation. Suitable for
// background TUI loaders that must never block on a password prompt
// or hang a goroutine on /dev/tty.
//
// Same outer-quote-then-unwrap mechanism as SSHRunUser so multi-word
// remoteCmd strings reach the remote shell as one bash -lc argument.
// Reuses the ControlMaster socket so the SSHCmd handshake cost is paid
// once per (user, host) tuple across the canopy session.
func SSHRunUserBatch(ctx context.Context, target string, remoteCmd string) *exec.Cmd {
	socketPath := filepath.Join(canopyHome(), "ssh-%C.sock")
	withPath := `export PATH="$HOME/.local/bin:$PATH"; ` + remoteCmd
	quoted := "'" + strings.ReplaceAll(withPath, "'", `'\''`) + "'"
	sshArgs := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + socketPath,
		"-o", "ControlPersist=300",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "BatchMode=yes",
		"-o", "NumberOfPasswordPrompts=0",
		// "--" before target: see SSHRunUser's identical comment — without
		// it, a target shaped like an ssh option is parsed as a flag, not
		// a hostname (confirmed by PoC: local arbitrary command execution
		// via a "-oProxyCommand=..." target).
		"--", target,
		"bash", "-lc", quoted,
	}
	log.Debug("ssh.run-user-batch", "target", target, "remote_cmd", remoteCmd)
	return exec.CommandContext(ctx, "ssh", sshArgs...)
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
	// "--" before target: see SSHRunUser's identical comment — without
	// it, a target shaped like an ssh option is parsed as a flag, not a
	// hostname (confirmed by PoC: local arbitrary command execution via
	// a "-oProxyCommand=..." target). This is the shared implementation
	// behind SSHCmd/SSHCmdBatch, both of which run on every background
	// refresh tick (host.Refresher.Tick), so a poisoned hosts.json entry
	// would fire this repeatedly, not just on manual dispatch.
	sshArgs = append(sshArgs, "--", target)
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
	// mosh syntax: `mosh [options] [--] <ssh-target> [command...]`. The
	// leading "--" is load-bearing, not decorative: mosh's own usage
	// line documents "[options] [--] [user@]host" — without it, a
	// target shaped like a mosh option (e.g. "--ssh=...") would be
	// parsed as a flag rather than the host, the same class of bug
	// fixed in sshCmdInternal/SSHRunUser for the ssh binary itself.
	//
	// No trailing "--" between target and args: mosh's usage has no
	// separator there ("[user@]host [command...]"), and adding one
	// confirmed-live breaks real attach — mosh forwards everything
	// after target verbatim as [command...], so a stray "--" becomes
	// the FIRST element of the command mosh-server tries to execvp,
	// which fails with "mosh-server: execvp: --: No such file or
	// directory" (found in cmd/canopy/switch.go's identical argv
	// construction; fixed there too).
	moshArgs := []string{"--", target}
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
		// "--" before target: same option-injection risk as the other
		// ssh call sites in this file — see sshCmdInternal's comment.
		"--", target,
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
//
// v0.22.x: no longer a hard stop. cmd/canopy/switch.go's
// chooseAttachMode falls back to SSHAttachLoop (below) automatically
// when this error fires, and --no-mosh opts into the same path even
// when mosh IS installed. ErrMoshMissing stays exported for callers
// that still want the old hard-fail behavior (e.g. a future explicit
// `canopy doctor` check).
type ErrMoshMissing struct {
	Inner error
}

func (e *ErrMoshMissing) Error() string {
	return "mosh is not installed locally. Install with: sudo pacman -S mosh (Arch) / sudo apt install mosh (Debian). Mosh is required to attach to remote canopy workspaces."
}

func (e *ErrMoshMissing) Unwrap() error { return e.Inner }

// SSHAttachLoopOptions configures SSHAttachLoop's retry behavior. The
// zero value uses production defaults; tests override MaxAttempts and
// Sleep so retry tests run instantly and deterministically.
type SSHAttachLoopOptions struct {
	// MaxAttempts caps how many times SSHAttachLoop retries after a
	// transport-level failure before giving up. 0 means the default
	// (30) — the --no-mosh path is meant to survive a laptop
	// sleep/wake or a flaky network, not retry forever against a
	// genuinely dead host.
	MaxAttempts int
	// InitialBackoff / MaxBackoff bound the exponential backoff between
	// reconnect attempts. Zero means defaults (1s / 15s).
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// Sleep is the backoff delay function; defaults to time.Sleep.
	// Tests override with a no-op recorder so retry tests don't
	// actually block.
	Sleep func(time.Duration)
}

// SSHAttachLoop is the --no-mosh / auto-detect-missing-mosh substitute
// for mosh's own roaming and laptop-suspend tolerance: mosh survives a
// network drop transparently over its UDP transport; plain ssh just
// dies. SSHAttachLoop re-dials automatically on a transport-level
// failure instead, so a canopy attach over plain SSH degrades
// gracefully on a flaky connection rather than dropping the user back
// to a shell.
//
// newAttempt must return a FRESH, fully-wired *exec.Cmd on every call
// (exec.Cmd is single-use) — production callers build via SSHRunUser
// with Stdin/Stdout/Stderr wired to the real terminal; tests substitute
// a fake command (e.g. `sh -c 'exit 255'`).
//
// Exit-code classification follows ssh(1)'s documented convention:
// "ssh exits with the exit status of the remote command, or with 255
// if an error occurred." So:
//
//   - exit 0            → the remote command ended normally (e.g. the
//     user detached from tmux deliberately). Stop the loop, return nil.
//   - exit 255           → ssh's OWN transport failed. Two sub-cases,
//     distinguished by scanning the attempt's stderr (see
//     isPermanentSSHFailure): a TRANSIENT failure (dropped connection,
//     temporary DNS hiccup) retries with exponential backoff up to
//     MaxAttempts; a PERMANENT failure (host key changed, publickey
//     auth rejected, hostname genuinely doesn't resolve) stops
//     immediately — waiting and retrying can't fix a rejected key or a
//     changed host key, and a changed host key is potentially a MITM
//     warning that deserves to reach the user immediately, not get
//     buried under minutes of "reconnecting..." noise.
//   - anything else       → the remote command ran and failed on its
//     own terms (e.g. "workspace not found"). Retrying would just
//     repeat the same failure — stop the loop and surface the error.
//
// statusOut receives human-readable "reconnecting..." lines between
// attempts so the user sees why their terminal went quiet instead of
// staring at a silent hang. Each attempt's stderr is still teed through
// to whatever the caller wired newAttempt's cmd.Stderr to (the actual
// ssh error text reaches the user's terminal live) — SSHAttachLoop only
// additionally captures a copy to classify it.
func SSHAttachLoop(ctx context.Context, newAttempt func() *exec.Cmd, statusOut io.Writer, opts SSHAttachLoopOptions) error {
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 30
	}
	backoff := opts.InitialBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	maxBackoff := opts.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 15 * time.Second
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		cmd := newAttempt()
		var errBuf bytes.Buffer
		if cmd.Stderr != nil {
			cmd.Stderr = io.MultiWriter(cmd.Stderr, &errBuf)
		} else {
			cmd.Stderr = &errBuf
		}
		err := cmd.Run()
		if err == nil {
			return nil
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 255 {
			// Not a transport-level failure (or the binary itself
			// couldn't even start) — surface as-is rather than retry.
			return err
		}
		if isPermanentSSHFailure(errBuf.String()) {
			return fmt.Errorf("ssh attach: %w", err)
		}
		if attempt == maxAttempts {
			return fmt.Errorf("ssh attach: gave up after %d attempts: %w", attempt, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(statusOut, "connection lost, reconnecting in %s (attempt %d/%d)...\n", backoff, attempt+1, maxAttempts)
		sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return nil // unreachable: loop always returns via one of the branches above
}

// isPermanentSSHFailure reports whether an exit-255 ssh stderr indicates
// a failure that retrying cannot fix — mirrors the classification
// pattern already used in internal/clipboard/host_install.go's
// verifyBridge for remote-error messages. A changed host key
// specifically is a potential MITM warning; burying it under retry
// noise instead of surfacing it immediately would be actively harmful,
// not just wasted time.
func isPermanentSSHFailure(stderr string) bool {
	for _, marker := range []string{
		"Host key verification failed",
		"REMOTE HOST IDENTIFICATION HAS CHANGED",
		"Permission denied",
		"Could not resolve hostname",
	} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}
