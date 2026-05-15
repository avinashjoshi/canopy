package clipboard

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	// SystemdRun manages the per-host clipboard-tunnel user unit
	// (write → daemon-reload → enable+restart → is-active).
	// Default production impl shells out to `systemctl --user ...`;
	// tests substitute a recording fake.
	SystemdRun  systemctlRunner
	HomeDir     string
	Version     string
	LocalUID    int
	// SSHPath is the absolute path to the ssh binary on the laptop,
	// baked into the tunnel unit's ExecStart (systemd user services
	// have a minimal PATH that doesn't include /usr/local/bin and
	// similar). Default resolved via exec.LookPath at constructor;
	// tests override.
	SSHPath string
}

// NewHostInstaller returns an installer keyed to the current process's
// home dir and UID. Version stamps the wrapper headers so re-installs
// can detect drift later (Lane C.4 fast-skip).
func NewHostInstaller(version string) (*HostInstaller, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("NewHostInstaller: %w", err)
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("NewHostInstaller: ssh not on PATH: %w", err)
	}
	return &HostInstaller{
		SSHExec:     defaultSSHExec,
		CloseMaster: host.ExitControlMaster,
		SystemdRun:  defaultSystemctlRunner,
		HomeDir:     home,
		Version:     version,
		LocalUID:    os.Getuid(),
		SSHPath:     sshPath,
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

	if err := h.EnsureTunnelUnit(hostName, sshTarget, remoteUID, out); err != nil {
		return fmt.Errorf("InstallOnHost: %w", err)
	}

	if err := h.verifyBridge(ctx, hostName, sshTarget, out); err != nil {
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
// SSH forward silently fails on every connect (sshd emits "remote
// port forwarding failed for listen path ..." warnings client-side)
// and the wrapper sees "No such file or directory" when trying to
// reach the (never-created) socket.
//
// Script is piped via SSH stdin to `bash`, NOT passed as `bash -c
// <script>` argv. Same reason refresh.go documents at length: SSH
// joins all post-target args with spaces, so `bash -c "mkdir -p
// /foo && chmod ..."` ends up as `bash -c mkdir -p /foo && chmod
// ...` on the remote's shell command line, which tokenizes such
// that bash receives only "mkdir" as its script and mkdir runs with
// no args ("missing operand"). Stdin avoids the quoting nightmare
// entirely — bash reads the script as one stream of bytes.
//
// Mode 0700 matches the local daemon's MkdirAll permissions. Belt-
// and-suspenders mkdir -p tolerates a re-install where the dir
// already exists.
func (h *HostInstaller) ensureRemoteSocketDir(ctx context.Context, sshTarget string, remoteUID int, out io.Writer) error {
	dir := fmt.Sprintf("/run/user/%d/canopy", remoteUID)
	script := fmt.Sprintf("set -e\nmkdir -p %s\nchmod 0700 %s\n", dir, dir)
	_, stderr, err := h.SSHExec(ctx, sshTarget, strings.NewReader(script), "bash")
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
	user, host, port := parseSSHTarget(sshTarget)
	if host == "" {
		return fmt.Errorf("writeSSHSnippet: could not parse hostname from ssh target %q", sshTarget)
	}
	content, err := SnippetContent(SnippetData{
		HostName:    hostName,
		SSHHostname: host,
		SSHUser:     user,
		SSHPort:     port,
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
	fmt.Fprintf(out, "  wrote %s (Host canopy-tunnel-%s → %s)\n", path, hostName, host)
	return nil
}

// hostnameFromSSHTarget extracts the hostname portion of an SSH target
// string. See parseSSHTarget for the full three-component breakdown.
func hostnameFromSSHTarget(target string) string {
	_, host, _ := parseSSHTarget(target)
	return host
}

// userFromSSHTarget extracts the user portion of an SSH target
// string, or "" if none. See parseSSHTarget.
func userFromSSHTarget(target string) string {
	user, _, _ := parseSSHTarget(target)
	return user
}

// portFromSSHTarget extracts the port portion of an SSH target
// string, or "" if none. See parseSSHTarget.
func portFromSSHTarget(target string) string {
	_, _, port := parseSSHTarget(target)
	return port
}

// parseSSHTarget breaks a `[user@]host[:port]` string into its three
// components. Used by writeSSHSnippet to bake explicit User/Port
// directives into the `Host canopy-tunnel-<name>` block so `ssh
// canopy-tunnel-<name>` resolves to the real target without the
// caller spelling it out.
//
// Edge cases handled:
//
//	avi@tower.lan       → ("avi", "tower.lan", "")
//	avi@tower.lan:22    → ("avi", "tower.lan", "22")
//	tower.lan           → ("",    "tower.lan", "")
//	tower.lan:22        → ("",    "tower.lan", "22")
//	weird@u@host        → ("weird@u", "host", "")  (LastIndex of @)
//	host:notaport       → ("",    "host:notaport", "")  (IPv6-safety)
//
// IPv6-bracketed targets are not parsed correctly — out of scope
// for Phase 1.
func parseSSHTarget(target string) (user, host, port string) {
	host = target
	if at := strings.LastIndex(host, "@"); at >= 0 {
		user = host[:at]
		host = host[at+1:]
	}
	if colon := strings.LastIndex(host, ":"); colon > 0 {
		candidate := host[colon+1:]
		allDigits := candidate != ""
		for _, r := range candidate {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			port = candidate
			host = host[:colon]
		}
	}
	return user, host, port
}

// EnsureTunnelUnit writes the per-host systemd user unit that holds
// the persistent SSH tunnel + enables/restarts it. Three steps:
//
//  1. Resolve ssh binary path (already done at construction time) and
//     render the unit body with the host's particulars baked in.
//  2. Write or refresh ~/.config/systemd/user/<unit>.service. If the
//     file content already matches, no-op the write.
//  3. systemctl --user daemon-reload + enable --now + restart. Restart
//     is the load-bearing step for re-installs: enable --now is a
//     no-op when the unit is already enabled, even if its content
//     just changed.
//
// Called after writeSSHSnippet and CloseMaster — the snippet defines
// the `Host canopy-tunnel-<name>` alias the unit's ExecStart uses;
// closing the canopy ControlMaster guarantees the tunnel-spawned ssh
// process establishes a fresh connection without inheriting state.
func (h *HostInstaller) EnsureTunnelUnit(hostName, sshTarget string, remoteUID int, out io.Writer) error {
	unitDir := filepath.Join(h.HomeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("EnsureTunnelUnit: mkdir %s: %w", unitDir, err)
	}
	unitName := TunnelUnitName(hostName) + ".service"
	unitPath := filepath.Join(unitDir, unitName)

	content, err := TunnelUnitContent(TunnelUnitData{
		HostName:  hostName,
		SSHTarget: sshTarget,
		RemoteUID: remoteUID,
		SSHPath:   h.SSHPath,
		Version:   h.Version,
	})
	if err != nil {
		return fmt.Errorf("EnsureTunnelUnit: %w", err)
	}

	existing, err := os.ReadFile(unitPath)
	if err == nil && string(existing) == content {
		fmt.Fprintf(out, "  tunnel unit %s already up to date\n", unitName)
	} else {
		if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("EnsureTunnelUnit: write %s: %w", unitPath, err)
		}
		fmt.Fprintf(out, "  wrote %s\n", unitPath)
	}

	if err := h.SystemdRun("--user", "daemon-reload"); err != nil {
		return fmt.Errorf("EnsureTunnelUnit: daemon-reload: %w", err)
	}
	if err := h.SystemdRun("--user", "enable", "--now", unitName); err != nil {
		return fmt.Errorf("EnsureTunnelUnit: enable --now %s: %w", unitName, err)
	}
	// Restart so the fresh unit body OR the cleaned-up state takes
	// effect even when the unit was already enabled+running.
	if err := h.SystemdRun("--user", "restart", unitName); err != nil {
		return fmt.Errorf("EnsureTunnelUnit: restart %s: %w", unitName, err)
	}
	fmt.Fprintf(out, "  enabled and restarted %s\n", unitName)
	return nil
}

// verifyBridge confirms three things, in order:
//
//  1. The systemd tunnel unit is active — `systemctl --user
//     is-active`. If this fails, the persistent SSH tunnel that
//     owns the RemoteForward sockets isn't running, and nothing
//     downstream will work.
//  2. The wrapper script round-trips text/plain — invoked by
//     absolute path over a NORMAL ssh (no RemoteForward; the
//     snippet alias is the only thing that triggers it). The
//     wrapper connects to sockets the tunnel unit owns.
//  3. The wrapper takes precedence in PATH lookup for Claude Code's
//     shell. Failure here is a WARNING, not a hard error.
func (h *HostInstaller) verifyBridge(ctx context.Context, hostName, sshTarget string, out io.Writer) error {
	// Step 1: tunnel unit alive.
	unitName := TunnelUnitName(hostName) + ".service"
	if err := h.SystemdRun("--user", "is-active", "--quiet", unitName); err != nil {
		return fmt.Errorf("verifyBridge: tunnel unit %s is not active — check `systemctl --user status %s` and `journalctl --user -u %s -n 30` for the cause: %w", unitName, unitName, unitName, err)
	}
	fmt.Fprintf(out, "  tunnel unit %s is active ✓\n", unitName)

	// Give the tunnel a brief moment to negotiate the RemoteForwards
	// after restart — systemctl restart returns when the unit is
	// active, but ssh's stream-local-forward negotiation runs after
	// auth and may take a beat. Polling is more robust but adds
	// complexity; a fixed short sleep is good enough for v0.18.
	// (Without this, the next verify step occasionally races and
	// reports "Connection refused" because the unit is active but
	// ssh's forward setup hasn't finished.)
	time.Sleep(750 * time.Millisecond)

	// Step 1: invoke the wrapper by absolute path. Script piped via
	// SSH stdin to a bare `bash`, NOT passed as `bash -c <script>`
	// argv — same word-splitting trap that broke ensureRemoteSocketDir
	// would otherwise feed `--list-types` to bash-c's $0 instead of
	// to the wrapper's argv, making the wrapper run with no args and
	// silently hit its default text case. exec passes the wrapper's
	// exit code straight through (no bash wrapper in the chain).
	stdout, stderr, err := h.SSHExec(ctx, sshTarget, strings.NewReader(`exec "$HOME/.local/bin/wl-paste" --list-types`+"\n"), "bash")
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
	// Script piped via stdin so SSH word-splitting doesn't mangle
	// `command -v wl-paste` into `command` (with `-v` as $0). `bash
	// -l` makes it a login shell so ~/.bash_profile / ~/.profile run
	// and PATH includes the user's ~/.local/bin if their config
	// adds it.
	pathOut, _, _ := h.SSHExec(ctx, sshTarget, strings.NewReader("command -v wl-paste\n"), "bash", "-l")
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
