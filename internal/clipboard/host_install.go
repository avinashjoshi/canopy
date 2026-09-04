package clipboard

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/avinashjoshi/canopy/internal/host"
)

// sshExec runs `<args...>` on the remote `target` via SSH. stdin is
// optional. Returns stdout bytes, stderr bytes, and the exec error
// (non-nil if the remote command exited non-zero).
//
// Default impl shells through internal/host.SSHCmdBatch — every
// SSHExec call site in this package (the wrapper pushes, the tmux
// config splice, the verify probe) is a fully non-interactive remote
// command with no legitimate need to prompt for a password, so
// BatchMode is correct for all of them, not just the unattended
// v0.22.x auto-setup caller (internal/ui/update_clipboard_autosetup.go)
// that made it load-bearing: host.SSHCmd's own doc comment warns that
// its non-batch mode lets a password prompt open /dev/tty directly,
// bypassing stdout/stderr redirection — harmless from a real terminal
// (canopy host clipboard <name> run by hand, or the Hosts-tab `c` key's
// tea.ExecProcess handoff), but it hangs the goroutine AND corrupts the
// render when InstallOnHost runs unattended inside a live Bubbletea
// alt-screen, which the auto-setup path does. Batch mode makes a host
// with no cached key auth fail fast with "Permission denied
// (publickey)" instead, which is a strict improvement for every caller.
//
// Carries ControlMaster + timeout knobs canopy already uses for every
// other remote dispatch path. Tests substitute a fake to assert call
// shape without needing a real SSH connection.
type sshExec func(ctx context.Context, target string, stdin io.Reader, args ...string) (stdout, stderr []byte, err error)

func defaultSSHExec(ctx context.Context, target string, stdin io.Reader, args ...string) (stdout, stderr []byte, err error) {
	cmd := host.SSHCmdBatch(ctx, target, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if stdin != nil {
		cmd.Stdin = stdin
	}
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// defaultLocalSystemctl runs `systemctl <args...>` on THIS machine
// (the laptop). Used only by cleanupLegacyArtifacts to tear down
// pre-OSC52 systemd units; never invoked over SSH.
func defaultLocalSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %v: %w (stderr: %s)", args, err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// HostInstaller runs the per-host bridge install (the laptop-side
// orchestrator that targets one remote host at a time). One construction
// per canopy process — both the CLI surface (`canopy host clipboard
// <name>`) and the TUI surface (`c` keybind on the Hosts tab) call into
// the same InstallOnHost method.
//
// Sequencing (post-OSC52 rewrite — no UID detection, no SSH snippet, no
// persistent tunnel; see docs/design/v0.18-clipboard-bridge.md's
// OSC52 follow-up section):
//
//  1. Push wl-paste + wl-copy wrappers via stdin to `cat > ~/.local/
//     bin/<name>` then `chmod +x` them. Same delivery pattern
//     internal/host.InstallScript uses for the canopy installer.
//  2. Splice the tmux copy-mode binds (plus `allow-passthrough on`)
//     into the remote's ~/.tmux.conf. Best-effort.
//  3. Remove any pre-OSC52 artifacts left on THIS machine by an older
//     canopy version (the persistent SSH-tunnel systemd unit, the
//     clipboard-server daemon unit, the per-host SSH RemoteForward
//     snippet) — see cleanupLegacyArtifacts.
//  4. Confirm the wrapper will actually be found on the remote's PATH
//     (login-shell `command -v wl-paste`). This is the only thing left
//     to "verify": OSC 52 itself needs a real attached terminal to
//     round-trip through, which this SSH BatchMode connection doesn't
//     have — see verifyBridge's doc comment.
type HostInstaller struct {
	SSHExec sshExec
	HomeDir string
	Version string
	// LocalSystemctl runs `systemctl <args...>` on THIS machine,
	// used only for best-effort cleanup of pre-OSC52 artifacts. Default
	// production impl is defaultLocalSystemctl; tests substitute a
	// recording fake.
	LocalSystemctl func(args ...string) error
}

// NewHostInstaller returns an installer keyed to the current process's
// home dir. Version stamps the wrapper headers so re-installs can
// detect drift later (hash-based fast-skip is a possible follow-up).
func NewHostInstaller(version string) (*HostInstaller, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("NewHostInstaller: %w", err)
	}
	return &HostInstaller{
		SSHExec:        defaultSSHExec,
		HomeDir:        home,
		Version:        version,
		LocalSystemctl: defaultLocalSystemctl,
	}, nil
}

// InstallOnHost performs the install end-to-end for a single host.
// Idempotent — re-running rewrites every artifact (wrappers, tmux
// config block) so the only thing the user needs to do after a canopy
// upgrade is press `c` on the Hosts tab again.
//
// Returns the first error and aborts. Only the wrapper pushes are hard
// preconditions; the tmux config splice and legacy cleanup are UX
// polish / migration hygiene and never abort the install on failure.
func (h *HostInstaller) InstallOnHost(ctx context.Context, hostName, sshTarget string, out io.Writer) error {
	fmt.Fprintf(out, "Installing clipboard bridge on %s (%s):\n", hostName, sshTarget)

	for _, w := range []WrapperScript{WrapperWlPaste, WrapperWlCopy} {
		if err := h.pushWrapper(ctx, sshTarget, w, out); err != nil {
			return fmt.Errorf("InstallOnHost: %w", err)
		}
	}

	// Configuring tmux copy-mode binds on the remote is UX polish, not
	// load-bearing for the bridge mechanism itself. If the SSH or
	// shell-script side fails, log + continue — the bridge still
	// works for command-line `wl-copy`/`wl-paste` and Claude Code
	// paste; only tmux's `y` / Enter shortcuts would be missing.
	if err := h.EnsureRemoteTmuxConfig(ctx, sshTarget, out); err != nil {
		fmt.Fprintf(out, "  ⚠  warning: tmux copy-mode binds not configured on remote: %v\n", err)
		fmt.Fprintln(out, "     Bridge still works; manually add the binds from")
		fmt.Fprintln(out, "     docs/remote-workspaces.md if you want copy-mode selections to flow.")
	}

	h.cleanupLegacyArtifacts(hostName, out)

	if err := h.verifyBridge(ctx, sshTarget, out); err != nil {
		return fmt.Errorf("InstallOnHost: bridge installed but verify failed: %w", err)
	}

	fmt.Fprintln(out, "  bridge active.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Optional nvim config on the remote (one-time, manual) so yanks reach the laptop clipboard:")
	fmt.Fprintln(out, "  vim.opt.clipboard = \"unnamedplus\"   -- init.lua")
	fmt.Fprintln(out, "  set clipboard+=unnamedplus           \" init.vim")
	return nil
}

// remoteTmuxConfigScript splices a marker-bounded block into the
// remote's ~/.tmux.conf containing the canopy bindings:
//
//	# canopy:start clipboard-bridge ...
//	set -g allow-passthrough on
//	bind-key -T copy-mode-vi y     send-keys -X copy-pipe-and-cancel "wl-copy"
//	bind-key -T copy-mode-vi Enter send-keys -X copy-pipe-and-cancel "wl-copy"
//	bind-key -T copy-mode-vi MouseDragEnd1Pane send-keys -X copy-pipe-and-cancel "wl-copy"
//	bind-key -T copy-mode M-w     send-keys -X copy-pipe-and-cancel "wl-copy"
//	# canopy:end clipboard-bridge
//
// `allow-passthrough on` is load-bearing, not cosmetic: tmux 3.3+
// defaults it to off, which silently drops the DCS-wrapped OSC 52
// sequences wl-copy.sh/wl-paste.sh emit from inside a tmux pane —
// every canopy workspace is a tmux session, so without this line the
// bridge would appear "installed" but never actually move a byte.
//
// Idempotent: re-running deletes any existing canopy block before
// appending a fresh one. Best-effort `tmux source-file` at the end
// re-reads the config in any running tmux server so existing
// sessions pick up the new binds without restart; failure (no tmux
// running on the remote) is silenced.
const remoteTmuxConfigScript = `set -e
CONF="$HOME/.tmux.conf"
touch "$CONF"

sed -i '/# canopy:start clipboard-bridge/,/# canopy:end clipboard-bridge/d' "$CONF"

cat >> "$CONF" <<'CANOPY_TMUX_EOF'

# canopy:start clipboard-bridge - managed by canopy; reinstall via 'canopy host clipboard <name>'
# Routes tmux copy-mode selections through wl-copy to the laptop clipboard
# via OSC 52. allow-passthrough is required for tmux 3.3+ to relay the
# DCS-wrapped escape sequence to the outer terminal instead of eating it.
set -g allow-passthrough on
# extended-keys on lets tmux distinguish modifier combinations like
# Ctrl+Shift+C from plain Ctrl+C (tmux 3.2+ feature).
set -g extended-keys on

bind-key -T copy-mode-vi y     send-keys -X copy-pipe-and-cancel "wl-copy"
bind-key -T copy-mode-vi Enter send-keys -X copy-pipe-and-cancel "wl-copy"
bind-key -T copy-mode-vi C-S-c send-keys -X copy-pipe-and-cancel "wl-copy"
bind-key -T copy-mode-vi MouseDragEnd1Pane send-keys -X copy-pipe-and-cancel "wl-copy"
bind-key -T copy-mode    M-w   send-keys -X copy-pipe-and-cancel "wl-copy"
bind-key -T copy-mode    C-S-c send-keys -X copy-pipe-and-cancel "wl-copy"
# canopy:end clipboard-bridge
CANOPY_TMUX_EOF

tmux source-file "$CONF" 2>/dev/null || true
`

// EnsureRemoteTmuxConfig pushes the tmux copy-mode bindings to the
// remote's ~/.tmux.conf via marker-block splice. After-install side
// effect: a tmux yank/select inside any tmux session on the remote
// pipes the selection through wl-copy → canopy wrapper → an OSC 52
// escape sequence → the outer terminal's clipboard.
func (h *HostInstaller) EnsureRemoteTmuxConfig(ctx context.Context, sshTarget string, out io.Writer) error {
	_, stderr, err := h.SSHExec(ctx, sshTarget, strings.NewReader(remoteTmuxConfigScript), "bash")
	if err != nil {
		return fmt.Errorf("EnsureRemoteTmuxConfig: %w (stderr: %s)", err, strings.TrimSpace(string(stderr)))
	}
	fmt.Fprintln(out, "  configured tmux copy-mode binds on remote (~/.tmux.conf, allow-passthrough on)")
	return nil
}

// pushWrapper renders one wrapper script and uploads it via the
// `cat > /path && chmod +x /path` idiom over SSH stdin. The single
// shell command runs cat-then-chmod so a write that succeeds but
// chmod that fails surfaces as one ssh exit rather than two separate
// remote round-trips.
//
// Always-push semantics: re-installs unconditionally overwrite the
// on-remote wrapper. Hash-based fast-skip is a follow-up; for now the
// upload is ~1 KB twice per install, well below noise.
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

// cleanupLegacyArtifacts removes on-disk remnants of the pre-OSC52
// bridge (the persistent SSH-tunnel systemd unit, the clipboard-server
// daemon unit, and the per-host SSH RemoteForward snippet) that an
// older canopy version may have installed on THIS machine. Every step
// is best-effort: a host that never had the old bridge installed hits
// only no-ops (file doesn't exist, systemctl reports the unit
// unknown), which must never surface as an install failure.
//
// This directly resolves the class of bug that motivated retiring the
// tunnel architecture: a stuck systemd unit reporting "start of the
// service was attempted too often" (StartLimitBurst), which used to
// require the user to manually run `systemctl --user reset-failed`.
// Now the unit is just removed outright on the next install.
//
// Does NOT touch the `Include ~/.ssh/config.d/canopy/*.conf` marker
// block in ~/.ssh/config itself (if a prior `canopy install
// clipboard-bridge` added one) — an empty/dangling Include is inert,
// and removing lines from the user's main SSH config unprompted is
// more invasive than this cleanup should be.
func (h *HostInstaller) cleanupLegacyArtifacts(hostName string, out io.Writer) {
	unitDir := filepath.Join(h.HomeDir, ".config", "systemd", "user")
	units := []string{
		"canopy-clipboard-tunnel-" + hostName + ".service",
		"canopy-clipboard.service",
	}

	removedAny := false
	for _, unit := range units {
		unitPath := filepath.Join(unitDir, unit)
		if _, err := os.Stat(unitPath); err != nil {
			continue // never installed here; nothing to clean up
		}
		// reset-failed first: disable --now's implicit stop on a unit
		// stuck in StartLimitBurst can itself report "attempted too
		// often" otherwise. Ignore its error too — the unit may not be
		// in a failed state at all, which is also fine.
		_ = h.LocalSystemctl("--user", "reset-failed", unit)
		if err := h.LocalSystemctl("--user", "disable", "--now", unit); err != nil {
			log.Warn("clipboard.cleanup.disable-failed", "unit", unit, "err", err)
		}
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			log.Warn("clipboard.cleanup.remove-unit-failed", "unit", unitPath, "err", err)
			continue
		}
		removedAny = true
		fmt.Fprintf(out, "  removed legacy systemd unit %s\n", unit)
	}
	if removedAny {
		if err := h.LocalSystemctl("--user", "daemon-reload"); err != nil {
			log.Warn("clipboard.cleanup.daemon-reload-failed", "err", err)
		}
	}

	snippetPath := filepath.Join(h.HomeDir, ".ssh", "config.d", "canopy", hostName+".conf")
	if err := os.Remove(snippetPath); err == nil {
		fmt.Fprintf(out, "  removed legacy SSH snippet %s\n", snippetPath)
	} else if !os.IsNotExist(err) {
		log.Warn("clipboard.cleanup.remove-snippet-failed", "path", snippetPath, "err", err)
	}
}

// verifyBridge confirms the wrapper will actually be found on the
// remote's PATH — invoked via a login shell (`bash -l`) so
// ~/.bash_profile / ~/.profile run and PATH reflects what an
// interactive/Claude Code shell would see. Failure here is a WARNING,
// not a hard error: it's a discoverability check, not a liveness
// check.
//
// This is deliberately the ONLY thing left to verify. The pre-OSC52
// bridge additionally round-tripped `wl-paste --list-types` over SSH
// to confirm the tunnel + daemon actually worked end-to-end — that
// check is gone because it's no longer possible: OSC 52 requires a
// real attached terminal (tty) to round-trip through, and InstallOnHost
// runs over BatchMode SSH (no pty). There's no way to verify "does
// OSC 52 actually work on this host" from here; only from an actually
// attached session, where the wrapper scripts themselves already fail
// loudly (non-zero exit, clear stderr) if OSC 52 isn't working. See
// BridgeStatusBridged's doc comment in probe.go for the same
// constraint on the background health probe.
func (h *HostInstaller) verifyBridge(ctx context.Context, sshTarget string, out io.Writer) error {
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
