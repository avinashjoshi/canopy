package clipboard

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SystemdUnitName is the user-level unit that supervises the daemon.
// Hard-coded because the wrapper scripts and the install-clipboard
// subcommand both reference it; keeping it in one place avoids drift.
const SystemdUnitName = "canopy-clipboard.service"

// SSH config marker delimiters for canopy's managed block. Same shape
// as cmd/canopy/install_tmux.go's tmux block — anything between these
// two lines is canopy-managed and may be rewritten on reinstall;
// anything outside is the user's.
const (
	SSHIncludeMarkerStart = "# canopy:start clipboard-bridge"
	SSHIncludeMarkerEnd   = "# canopy:end clipboard-bridge"
)

// systemdUnitBody is the unit content written to disk.
//
// ExecStart references the ~/.local/bin/canopy symlink so `canopy use
// <workspace>` and `canopy use release` swaps don't break the daemon
// (D3 in /plan-eng-review 2026-05-14). The %h specifier is systemd's
// expansion for the user's home dir; resolved by systemd at unit-run
// time, not at file-write time.
//
// Restart=on-failure recovers from a wedged wl-paste / wl-copy without
// user intervention. 5-second RestartSec spaces failed restarts so a
// persistent error doesn't pin a CPU.
const systemdUnitBody = `[Unit]
Description=Canopy clipboard bridge daemon
Documentation=https://github.com/avinashjoshi/canopy/blob/main/docs/design/v0.18-clipboard-bridge.md
After=default.target

[Service]
ExecStart=%h/.local/bin/canopy clipboard-server
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

// sshIncludeBlock is the marker-delimited block added to ~/.ssh/config.
// One leading newline so the block sits in its own paragraph even when
// appended to a file that doesn't end with a newline.
const sshIncludeBlock = "\n" +
	SSHIncludeMarkerStart + " — managed by canopy; do not edit between markers\n" +
	"Include ~/.ssh/config.d/canopy/*.conf\n" +
	SSHIncludeMarkerEnd + "\n"

// systemctlRunner is the swap point for tests. Default impl shells out
// via exec.Command; tests substitute fakes to assert call shape without
// requiring a real user systemd session.
type systemctlRunner func(args ...string) error

func defaultSystemctlRunner(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w (output: %s)", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LocalInstaller bootstraps the laptop side of the clipboard bridge —
// the bits that are per-machine, not per-host:
//
//  1. Systemd user unit at ~/.config/systemd/user/canopy-clipboard.service.
//  2. SSH config-include marker block in ~/.ssh/config.
//  3. ~/.ssh/config.d/canopy/ directory for per-host snippets (Lane C
//     writes per-host .conf files into this dir).
//  4. systemctl --user daemon-reload + enable --now.
//
// One-time per laptop. Re-runs are idempotent: same systemd unit body
// → no rewrite; SSH config already has the marker block → no append;
// systemctl daemon-reload + enable --now is itself idempotent.
type LocalInstaller struct {
	HomeDir    string
	SystemdRun systemctlRunner
}

// NewLocalInstaller returns an installer rooted at the user's home dir.
// Errors when $HOME isn't resolvable — canopy doesn't run in
// containers without a home dir today, but fail loudly rather than
// silently writing to /tmp.
func NewLocalInstaller() (*LocalInstaller, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("clipboard.NewLocalInstaller: %w", err)
	}
	return &LocalInstaller{
		HomeDir:    home,
		SystemdRun: defaultSystemctlRunner,
	}, nil
}

// Install runs all four steps. Returns the first error and aborts —
// each step is a precondition for the next (no point loading the
// systemd unit if the unit file write failed).
//
// out receives progress lines so the CLI/TUI surface streams them to
// the user.
func (l *LocalInstaller) Install(out io.Writer) error {
	fmt.Fprintln(out, "Installing canopy clipboard bridge (laptop-side):")
	if err := l.EnsureSystemdUnit(out); err != nil {
		return err
	}
	if err := l.EnsureCanopySSHDir(out); err != nil {
		return err
	}
	if err := l.EnsureSSHInclude(out); err != nil {
		return err
	}
	if err := l.EnableSystemdUnit(out); err != nil {
		return err
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Done. Next: register a remote host with `canopy host add`, then")
	fmt.Fprintln(out, "press `c` on the Hosts tab to bridge clipboard to that host.")
	return nil
}

// EnsureSystemdUnit writes (or refreshes) the user-level systemd unit
// that supervises the daemon. Idempotent: existing file with matching
// content → no-op; otherwise overwrite.
//
// Does NOT call daemon-reload or enable — those happen in
// EnableSystemdUnit so the unit-file-write half can be tested without
// requiring a real systemd session.
func (l *LocalInstaller) EnsureSystemdUnit(out io.Writer) error {
	unitDir := filepath.Join(l.HomeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("EnsureSystemdUnit: mkdir %s: %w", unitDir, err)
	}
	path := filepath.Join(unitDir, SystemdUnitName)

	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == systemdUnitBody {
		fmt.Fprintf(out, "  systemd unit at %s already up to date\n", path)
		return nil
	}

	if err := os.WriteFile(path, []byte(systemdUnitBody), 0o644); err != nil {
		return fmt.Errorf("EnsureSystemdUnit: write %s: %w", path, err)
	}
	fmt.Fprintf(out, "  wrote %s\n", path)
	return nil
}

// EnsureCanopySSHDir creates ~/.ssh/config.d/canopy/ if missing. Mode
// 0700 matches ~/.ssh's standard permissions — sshd refuses to honor
// Include directives pointing at world-readable dirs.
func (l *LocalInstaller) EnsureCanopySSHDir(out io.Writer) error {
	dir := filepath.Join(l.HomeDir, ".ssh", "config.d", "canopy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("EnsureCanopySSHDir: mkdir %s: %w", dir, err)
	}
	fmt.Fprintf(out, "  ensured %s exists\n", dir)
	return nil
}

// EnsureSSHInclude adds the canopy marker block to ~/.ssh/config so the
// per-host snippets in ~/.ssh/config.d/canopy/ are picked up.
//
// Idempotent: if the start marker is already present, no-op.
// Conservative: doesn't rewrite the block in place even if its content
// drifted (e.g., user manually edited the Include path between
// markers). Phase 1.5+ may add a --reinstall flag that force-rewrites;
// for now the user can delete the block by hand and re-run.
//
// If ~/.ssh/config doesn't exist, creates it with just the marker
// block. If ~/.ssh/ doesn't exist (very fresh user account), creates
// it with mode 0700.
func (l *LocalInstaller) EnsureSSHInclude(out io.Writer) error {
	sshDir := filepath.Join(l.HomeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("EnsureSSHInclude: mkdir %s: %w", sshDir, err)
	}
	sshConfig := filepath.Join(sshDir, "config")

	var existing string
	data, err := os.ReadFile(sshConfig)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("EnsureSSHInclude: read %s: %w", sshConfig, err)
	}
	if err == nil {
		existing = string(data)
	}

	if strings.Contains(existing, SSHIncludeMarkerStart) {
		fmt.Fprintf(out, "  %s already has canopy Include block\n", sshConfig)
		return nil
	}

	// Compose new content. Three cases:
	//   1. config missing or empty → marker block only (no leading newline).
	//   2. config exists, ends with newline → append block (block has its own leading newline).
	//   3. config exists, no trailing newline → insert one before the block.
	var content string
	switch {
	case existing == "":
		content = strings.TrimLeft(sshIncludeBlock, "\n")
	case strings.HasSuffix(existing, "\n"):
		content = existing + sshIncludeBlock
	default:
		content = existing + sshIncludeBlock
	}

	if err := os.WriteFile(sshConfig, []byte(content), 0o600); err != nil {
		return fmt.Errorf("EnsureSSHInclude: write %s: %w", sshConfig, err)
	}
	fmt.Fprintf(out, "  added canopy Include block to %s\n", sshConfig)
	return nil
}

// EnableSystemdUnit reloads the user systemd manager (pickup the new
// unit file) and enables-+-starts the canopy-clipboard service.
//
// `enable --now` is idempotent: already-enabled units stay enabled,
// already-running services don't restart. Safe to call on every
// re-install.
func (l *LocalInstaller) EnableSystemdUnit(out io.Writer) error {
	if err := l.SystemdRun("--user", "daemon-reload"); err != nil {
		return fmt.Errorf("EnableSystemdUnit: daemon-reload: %w", err)
	}
	if err := l.SystemdRun("--user", "enable", "--now", SystemdUnitName); err != nil {
		return fmt.Errorf("EnableSystemdUnit: enable --now %s: %w", SystemdUnitName, err)
	}
	fmt.Fprintf(out, "  enabled and started %s\n", SystemdUnitName)
	return nil
}
