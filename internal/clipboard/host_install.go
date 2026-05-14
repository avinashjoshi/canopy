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
	HomeDir string
	Version string
	LocalUID int
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
		SSHExec:  defaultSSHExec,
		HomeDir:  home,
		Version:  version,
		LocalUID: os.Getuid(),
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

	for _, w := range []WrapperScript{WrapperWlPaste, WrapperWlCopy} {
		if err := h.pushWrapper(ctx, sshTarget, w, out); err != nil {
			return fmt.Errorf("InstallOnHost: %w", err)
		}
	}

	if err := h.writeSSHSnippet(hostName, remoteUID, out); err != nil {
		return fmt.Errorf("InstallOnHost: %w", err)
	}

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
func (h *HostInstaller) writeSSHSnippet(hostName string, remoteUID int, out io.Writer) error {
	dir := filepath.Join(h.HomeDir, ".ssh", "config.d", "canopy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("writeSSHSnippet: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, hostName+".conf")
	content, err := SnippetContent(SnippetData{
		HostName:  hostName,
		Version:   h.Version,
		LocalUID:  h.LocalUID,
		RemoteUID: remoteUID,
	})
	if err != nil {
		return fmt.Errorf("writeSSHSnippet: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writeSSHSnippet: write %s: %w", path, err)
	}
	fmt.Fprintf(out, "  wrote %s\n", path)
	return nil
}

// verifyBridge runs the freshly-deployed wl-paste wrapper over SSH and
// asserts that the basic round-trip works. `--list-types` is the
// cheapest invocation (one socket open, eight bytes read for the PNG
// probe, no full clipboard transfer) and exercises the same code path
// Claude Code uses when deciding whether image/png is available.
//
// Output must contain `text/plain` for the bridge to be considered
// healthy — our wrapper always emits it. If we don't see it, something
// in the chain broke: wrapper missing (PATH problem), socat missing
// (auto-install needed), daemon down, sshd ForwardLocalSockets
// disabled, or the SSH config snippet not picked up by sshd's reload.
func (h *HostInstaller) verifyBridge(ctx context.Context, sshTarget string, out io.Writer) error {
	// `bash -lc` so ~/.bashrc's PATH-prepend for ~/.local/bin runs
	// before we invoke the wrapper. Non-interactive SSH-exec'd
	// commands skip .bashrc by default on Arch/omarchy/Debian.
	stdout, stderr, err := h.SSHExec(ctx, sshTarget, nil, "bash", "-lc", "wl-paste --list-types")
	if err != nil {
		return fmt.Errorf("verifyBridge: wl-paste --list-types on %s: %w (stderr: %s)", sshTarget, err, strings.TrimSpace(string(stderr)))
	}
	body := string(stdout)
	if !strings.Contains(body, "text/plain") {
		return fmt.Errorf("verifyBridge: wrapper ran but did not emit text/plain (got %q) — wrapper may be the system wl-paste, not ours", strings.TrimSpace(body))
	}
	fmt.Fprintf(out, "  verified wrapper on %s\n", sshTarget)
	return nil
}
