package clipboard

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/avinashjoshi/canopy/internal/host"
)

// sshExec runs `<args...>` on the remote `target` via SSH. stdin is
// optional. Returns stdout bytes, stderr bytes, and the exec error
// (non-nil if the remote command exited non-zero).
//
// Default impl shells through internal/host.SSHCmd, which carries
// ControlMaster + timeout knobs canopy already uses for every other
// remote dispatch path. Tests substitute a fake to assert call shape
// without needing a real SSH connection.
type sshExec func(ctx context.Context, target string, stdin io.Reader, args ...string) (stdout, stderr []byte, err error)

func defaultSSHExec(ctx context.Context, target string, stdin io.Reader, args ...string) (stdout, stderr []byte, err error) {
	cmd := host.SSHCmd(ctx, target, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if stdin != nil {
		cmd.Stdin = stdin
	}
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// HostInstaller runs the per-host bridge install (the laptop-side
// orchestrator that targets one remote host at a time). One construction
// per canopy process — both the CLI surface (`canopy host clipboard
// <name>`) and the TUI surface (`c` keybind on the Hosts tab) call into
// the same InstallOnHost method.
//
// Sequencing (intentionally minimal in v0.18):
//
//  1. SSH `id -u` on the remote → resolves the UID the snippet's
//     RemoteForward paths need.
//  2. Push wl-paste + wl-copy wrappers via stdin to `cat > ~/.local/
//     bin/<name>` then `chmod +x` them. Same delivery pattern
//     internal/host.InstallScript uses for the canopy installer.
//  3. Write the per-host SSH snippet to
//     ~/.ssh/config.d/canopy/<host>.conf using SnippetContent. The
//     directory + Include directive in ~/.ssh/config are set up by
//     Lane B's `canopy install clipboard-bridge`, so this write
//     plugs straight in.
//  4. Verify by running the freshly-deployed `wl-paste --list-types`
//     over SSH. A clean exit confirms PATH precedence, socat
//     presence, and end-to-end forwarding all work.
type HostInstaller struct {
	SSHExec sshExec
	// CloseMaster terminates the SSH ControlMaster for `target` if one
	// is alive. Called between writeSSHSnippet and verifyBridge so the
	// verify SSH (and every subsequent canopy command to this host)
	// re-establishes a master that picks up the freshly-written
	// RemoteForward directives. Default production impl is
	// internal/host.ExitControlMaster; tests substitute a recording
	// fake so the call shape is verifiable.
	CloseMaster func(target string)
	HomeDir     string
	Version     string
	LocalUID    int
}

// NewHostInstaller returns an installer keyed to the current process's
// home dir and UID. Version stamps the wrapper headers so re-installs
// can detect drift later (Lane C.4 fast-skip).
func NewHostInstaller(version string) (*HostInstaller, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("NewHostInstaller: %w", err)
	}
	return &HostInstaller{
		SSHExec:     defaultSSHExec,
		CloseMaster: host.ExitControlMaster,
		HomeDir:     home,
		Version:     version,
		LocalUID:    os.Getuid(),
	}, nil
}

// InstallOnHost performs the four-step install end-to-end for a single
// host. Idempotent — re-running rewrites every artifact (wrappers,
// SSH snippet) so the only thing the user needs to do after a canopy
// upgrade is press `c` on the Hosts tab again.
//
// Returns the first error and aborts. Each step is a hard precondition
// for the next: pushing wrappers without a verified UID would bake
// wrong socket paths; writing the snippet without wrappers in place
// would render the bridge half-installed.
func (h *HostInstaller) InstallOnHost(ctx context.Context, hostName, sshTarget string, out io.Writer) error {
	if h.LocalUID <= 0 {
		return fmt.Errorf("InstallOnHost: refusing — local UID is %d (sockets would land in /run/user/0/)", h.LocalUID)
	}
	fmt.Fprintf(out, "Installing clipboard bridge on %s (%s):\n", hostName, sshTarget)

	remoteUID, err := h.detectRemoteUID(ctx, sshTarget)
	if err != nil {
		return fmt.Errorf("InstallOnHost: %w", err)
	}
	fmt.Fprintf(out, "  remote UID: %d (local: %d)\n", remoteUID, h.LocalUID)

	if err := h.ensureRemoteSocketDir(ctx, sshTarget, remoteUID, out); err != nil {
		return fmt.Errorf("InstallOnHost: %w", err)
	}

	for _, w := range []WrapperScript{WrapperWlPaste, WrapperWlCopy} {
		if err := h.pushWrapper(ctx, sshTarget, w, out); err != nil {
			return fmt.Errorf("InstallOnHost: %w", err)
		}
	}

	if err := h.writeSSHSnippet(hostName, sshTarget, remoteUID, out); err != nil {
		return fmt.Errorf("InstallOnHost: %w", err)
	}

	// The snippet's RemoteForward directives only take effect when SSH
	// reads them at handshake time. Any existing ControlMaster for
	// this target was opened by a prior canopy command (host install,
	// refresh probe, etc.) BEFORE the snippet existed — it carries no
	// forwards. Kill it so the verify SSH (and every subsequent canopy
	// command) opens a fresh master that picks up the new config.
	h.CloseMaster(sshTarget)
	fmt.Fprintln(out, "  reset SSH ControlMaster so RemoteForward picks up the new config")

	if err := h.verifyBridge(ctx, sshTarget, out); err != nil {
		return fmt.Errorf("InstallOnHost: bridge installed but verify failed: %w", err)
	}

	fmt.Fprintln(out, "  bridge active.")
	return nil
}

// detectRemoteUID resolves the remote user's numeric UID. Baked into
// the SSH snippet at write time (D2 in /plan-eng-review: re-detect
// on every install rather than caching) so a host whose SSH user
// changes is picked up the next time `canopy host clipboard <name>`
// runs.
func (h *HostInstaller) detectRemoteUID(ctx context.Context, sshTarget string) (int, error) {
	stdout, stderr, err := h.SSHExec(ctx, sshTarget, nil, "id", "-u")
	if err != nil {
		return 0, fmt.Errorf("detectRemoteUID: ssh %s id -u: %w (stderr: %s)", sshTarget, err, strings.TrimSpace(string(stderr)))
	}
	uidStr := strings.TrimSpace(string(stdout))
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		return 0, fmt.Errorf("detectRemoteUID: parse %q from `id -u`: %w", uidStr, err)
	}
	if uid <= 0 {
		return 0, fmt.Errorf("detectRemoteUID: refusing UID %d on remote (sockets would land in /run/user/0/)", uid)
	}
	return uid, nil
}

// ensureRemoteSocketDir creates /run/user/<uid>/canopy/ on the remote
// host. SSH's RemoteForward of a unix socket calls bind() at the
// configured path, and bind() returns ENOENT if the parent directory
// doesn't exist — sshd does NOT mkdir for you. Without this step the
// SSH forward silently fails on every connect and the wrapper sees
// "No such file or directory" when trying to reach the (never-created)
// socket.
//
// Mode 0700 matches the local daemon's MkdirAll permissions. Belt-
// and-suspenders mkdir -p tolerates a re-install where the dir
// already exists.
func (h *HostInstaller) ensureRemoteSocketDir(ctx context.Context, sshTarget string, remoteUID int, out io.Writer) error {
	dir := fmt.Sprintf("/run/user/%d/canopy", remoteUID)
	cmd := fmt.Sprintf("mkdir -p %s && chmod 0700 %s", dir, dir)
	_, stderr, err := h.SSHExec(ctx, sshTarget, nil, "bash", "-c", cmd)
	if err != nil {
		return fmt.Errorf("ensureRemoteSocketDir: %s: %w (stderr: %s)", dir, err, strings.TrimSpace(string(stderr)))
	}
	fmt.Fprintf(out, "  ensured %s exists on remote\n", dir)
	return nil
}

// pushWrapper renders one wrapper script and uploads it via the
// `cat > /path && chmod +x /path` idiom over SSH stdin. The single
// shell command runs cat-then-chmod so a write that succeeds but
// chmod that fails surfaces as one ssh exit rather than two separate
// remote round-trips.
//
// Always-push semantics in v0.18: re-installs unconditionally overwrite
// the on-remote wrapper. Hash-based fast-skip is a follow-up; for now
// the upload is ~1 KB twice per install, well below noise.
func (h *HostInstaller) pushWrapper(ctx context.Context, sshTarget string, w WrapperScript, out io.Writer) error {
	content, hash, err := WrapperContent(w, h.Version)
	if err != nil {
		return fmt.Errorf("pushWrapper(%q): %w", w, err)
	}
	remotePath := "$HOME/.local/bin/" + w.RemoteName()
	// One-line shell pipeline so cat + mkdir + chmod commit or fail
	// together. mkdir -p tolerates a missing ~/.local/bin (fresh user).
	remoteCmd := "set -e; mkdir -p $HOME/.local/bin; cat > " + remotePath + "; chmod +x " + remotePath
	_, stderr, err := h.SSHExec(ctx, sshTarget, strings.NewReader(content), "bash", "-c", remoteCmd)
	if err != nil {
		return fmt.Errorf("pushWrapper(%q): ssh write: %w (stderr: %s)", w, err, strings.TrimSpace(string(stderr)))
	}
	fmt.Fprintf(out, "  pushed %s (hash %s)\n", w.RemoteName(), hash)
	return nil
}

// writeSSHSnippet writes the per-host config to
// ~/.ssh/config.d/canopy/<host>.conf. The directory is created by
// `canopy install clipboard-bridge` (Lane B), but we mkdir again
// here so a first-time `canopy host clipboard <name>` on a fresh
// laptop doesn't require running the install-target first. Mode 0700
// matches the rest of ~/.ssh/.
//
// Filename is the canopy host name, NOT the SSH target. Two hosts
// with the same SSH target (uncommon but legal — same machine reached
// by IP vs hostname) get distinct snippets.
//
// The `Host` directive in the snippet body uses the SSH HOSTNAME (the
// hostname portion of sshTarget, e.g., "tower.lan" extracted from
// "avi@tower.lan:22") — NOT canopy's internal hostName. SSH matches
// Host patterns against the hostname portion of the target string on
// the command line; using canopy's alias silently fails to match when
// canopy (or the user) does `ssh user@hostname`.
func (h *HostInstaller) writeSSHSnippet(hostName, sshTarget string, remoteUID int, out io.Writer) error {
	dir := filepath.Join(h.HomeDir, ".ssh", "config.d", "canopy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("writeSSHSnippet: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, hostName+".conf")
	sshHostname := hostnameFromSSHTarget(sshTarget)
	if sshHostname == "" {
		return fmt.Errorf("writeSSHSnippet: could not parse hostname from ssh target %q", sshTarget)
	}
	content, err := SnippetContent(SnippetData{
		HostName:    hostName,
		SSHHostname: sshHostname,
		Version:     h.Version,
		LocalUID:    h.LocalUID,
		RemoteUID:   remoteUID,
	})
	if err != nil {
		return fmt.Errorf("writeSSHSnippet: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writeSSHSnippet: write %s: %w", path, err)
	}
	fmt.Fprintf(out, "  wrote %s (Host %s)\n", path, sshHostname)
	return nil
}

// hostnameFromSSHTarget extracts the hostname portion of an SSH target
// string. Examples:
//
//	avi@tower.lan      → tower.lan
//	avi@tower.lan:22   → tower.lan
//	tower.lan          → tower.lan
//	tower.lan:22       → tower.lan
//
// SSH's Host pattern matching keys off this string (the hostname part
// of `[user@]host[:port]`). The snippet must use it, not canopy's
// internal alias, or the `Host` directive won't match.
//
// IPv6-bracketed targets ([::1]:22) are not handled — Phase 1 ignores
// IPv6 entirely. Tracked as a follow-up; will revisit if anyone runs
// canopy against an IPv6-only host.
func hostnameFromSSHTarget(target string) string {
	// Strip any `user@` prefix. LastIndex defends against the
	// pathological "weird-user-name-with-@@-in-it@host" case (legal in
	// some shells; SSH parses the LAST @ as the user/host separator).
	if at := strings.LastIndex(target, "@"); at >= 0 {
		target = target[at+1:]
	}
	// Strip `:port` suffix when present. Only strip the LAST colon and
	// only if the remainder is digits — defends against accidental
	// IPv6 mangling (a future IPv6-target user gets a no-op instead of
	// a wrong-host snippet).
	if colon := strings.LastIndex(target, ":"); colon > 0 {
		portPart := target[colon+1:]
		allDigits := portPart != ""
		for _, r := range portPart {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			target = target[:colon]
		}
	}
	return target
}

// verifyBridge confirms two distinct things, in order:
//
//  1. The wrapper script itself works end-to-end — invoked by absolute
//     path so PATH precedence is irrelevant. If this fails, the
//     wrapper, the daemon, or the SSH socket forwarding is broken.
//  2. The wrapper takes precedence in PATH lookup — Claude Code on
//     the remote does plain `wl-paste`, not the absolute path; if
//     real `/usr/bin/wl-paste` wins, the bridge mechanism is fine
//     but Claude can't use it.
//
// Step 2 produces a WARNING, not an error: the install is functionally
// complete (the wrapper exists and works), the user's shell config is
// what needs adjusting. Surfacing the fix inline beats letting the
// user discover the symptom much later when an image paste mysteriously
// fails inside Claude Code.
func (h *HostInstaller) verifyBridge(ctx context.Context, sshTarget string, out io.Writer) error {
	// Step 1: invoke the wrapper by absolute path. `bash -c` (NOT
	// `bash -lc`) — no shell config sourcing, no PATH dependency.
	// Just runs the script we know we deployed.
	stdout, stderr, err := h.SSHExec(ctx, sshTarget, nil, "bash", "-c", `$HOME/.local/bin/wl-paste --list-types`)
	stderrStr := strings.TrimSpace(string(stderr))
	if err != nil {
		// Classify the most common failure modes with actionable hints.
		switch {
		case strings.Contains(stderrStr, "socat: command not found"),
			strings.Contains(stderrStr, "timeout: command not found"):
			return fmt.Errorf("verifyBridge: required tool missing on remote — install with `apt install socat` / `pacman -S socat` and re-run `canopy host clipboard <name>`. Original stderr: %s", stderrStr)
		case strings.Contains(stderrStr, "Connection refused"),
			strings.Contains(stderrStr, "No such file or directory") && strings.Contains(stderrStr, "clip-"):
			return fmt.Errorf("verifyBridge: wrapper ran but couldn't reach the laptop's daemon — either the laptop's `canopy clipboard-server` isn't running OR your SSH connection didn't establish the RemoteForward (try re-SSHing). Stderr: %s", stderrStr)
		default:
			return fmt.Errorf("verifyBridge: $HOME/.local/bin/wl-paste failed: %w (stderr: %s)", err, stderrStr)
		}
	}
	if !strings.Contains(string(stdout), "text/plain") {
		return fmt.Errorf("verifyBridge: wrapper ran (exit 0) but didn't emit text/plain (got %q) — wrapper file may have been overwritten by something else", strings.TrimSpace(string(stdout)))
	}
	fmt.Fprintln(out, "  wrapper round-trips text/plain ✓")

	// Step 2: confirm Claude Code will actually find the wrapper.
	// `bash -lc command -v` resolves wl-paste through the user's
	// login-shell PATH. If `/usr/bin/wl-paste` wins, the install is
	// functional but Claude won't use the wrapper — that's a hint,
	// not a hard failure.
	pathOut, _, _ := h.SSHExec(ctx, sshTarget, nil, "bash", "-lc", "command -v wl-paste")
	resolved := strings.TrimSpace(string(pathOut))
	switch {
	case resolved == "":
		fmt.Fprintln(out, "  ⚠  warning: `command -v wl-paste` returned nothing — login shell can't find any wl-paste at all")
	case !strings.Contains(resolved, ".local/bin/wl-paste"):
		fmt.Fprintf(out, "  ⚠  warning: login-shell PATH resolves wl-paste to %s\n", resolved)
		fmt.Fprintln(out, "     Claude Code on the remote will use the system wl-paste, not the canopy wrapper.")
		fmt.Fprintln(out, "     Fix: append to ~/.bashrc on the remote (and re-source / re-attach tmux):")
		fmt.Fprintln(out, "       export PATH=\"$HOME/.local/bin:$PATH\"")
		fmt.Fprintf(out, "  bridge installed but PATH needs fixing on %s\n", sshTarget)
		return nil
	default:
		fmt.Fprintf(out, "  PATH resolves wl-paste to %s ✓\n", resolved)
	}
	fmt.Fprintf(out, "  verified bridge on %s\n", sshTarget)
	return nil
}
