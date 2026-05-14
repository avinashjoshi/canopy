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
// Ordering: After= AND PartOf= graphical-session.target so the daemon
// starts only once the Wayland compositor (Hyprland/sway/etc.) has
// activated the user's graphical session — and gets stopped if the
// session goes away. Without this, systemd would launch the service
// at default.target activation, before the compositor has imported
// WAYLAND_DISPLAY into the systemd user manager environment, and
// Detect() would fail with ErrNoProvider.
//
// Restart=on-failure + a bounded StartLimit prevents a permanently
// missing WAYLAND_DISPLAY (headless install, broken compositor) from
// pinning the CPU in an infinite restart loop. 5 attempts in 5 minutes
// is plenty for any reasonable session-startup race.
const systemdUnitBody = `[Unit]
Description=Canopy clipboard bridge daemon
Documentation=https://github.com/avinashjoshi/canopy/blob/main/docs/design/v0.18-clipboard-bridge.md
After=graphical-session.target
PartOf=graphical-session.target
StartLimitBurst=5
StartLimitIntervalSec=300

[Service]
ExecStart=%h/.local/bin/canopy clipboard-server
Restart=on-failure
RestartSec=5

[Install]
WantedBy=graphical-session.target
`

// importedSessionEnvVars are the variables Install() copies from the
// user's calling shell into the systemd --user manager environment.
// systemctl --user import-environment makes them visible to every
// subsequent service start, which is how we get WAYLAND_DISPLAY into
// the daemon's process env without depending on the compositor having
// run dbus-update-activation-environment already.
var importedSessionEnvVars = []string{
	"WAYLAND_DISPLAY",  // load-bearing: Detect() keys on this
	"DISPLAY",          // X11 fallback (Phase 2 readiness)
	"XDG_RUNTIME_DIR",  // socket dir resolution
	"XDG_CURRENT_DESKTOP",
}

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

// Install runs all five steps. Returns the first error and aborts —
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
	// ImportSessionEnv must run BEFORE EnableSystemdUnit's enable --now,
	// otherwise the daemon's first start picks up an empty
	// WAYLAND_DISPLAY and exits 1 (Detect → ErrNoProvider). Running it
	// here also no-ops cleanly when the calling shell doesn't have
	// WAYLAND_DISPLAY (logs a warning, doesn't fail the install).
	if err := l.ImportSessionEnv(out); err != nil {
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

// ImportSessionEnv copies WAYLAND_DISPLAY (and friends) from the
// calling shell's environment into the systemd --user manager, so
// subsequent service starts inherit them. Without this, a user who
// runs `canopy install clipboard-bridge` from their normal terminal
// would see the daemon exit-1 on every start because the systemd user
// manager's environment block is empty (compositor hasn't imported
// the vars yet, or hasn't imported the ones we care about).
//
// No-op when WAYLAND_DISPLAY isn't set in the calling shell — prints
// a warning and continues. This covers two cases:
//
//   - Running the installer over plain SSH (no session env at all).
//   - First-boot install before the compositor exists.
//
// The boot-time path is covered separately by the unit's
// PartOf=graphical-session.target + the compositor's own
// dbus-update-activation-environment (Hyprland / omarchy default).
func (l *LocalInstaller) ImportSessionEnv(out io.Writer) error {
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		fmt.Fprintln(out, "  WAYLAND_DISPLAY not set in this shell — skipping systemctl import-environment")
		fmt.Fprintln(out, "    (the daemon will start automatically the next time your compositor activates")
		fmt.Fprintln(out, "    graphical-session.target, assuming your Wayland setup imports the env)")
		return nil
	}
	args := append([]string{"--user", "import-environment"}, importedSessionEnvVars...)
	if err := l.SystemdRun(args...); err != nil {
		return fmt.Errorf("ImportSessionEnv: %w", err)
	}
	fmt.Fprintf(out, "  imported %v into the systemd --user manager environment\n", importedSessionEnvVars)
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

// EnableSystemdUnit reloads the user systemd manager (pickup any new
// unit file content), enables + starts the canopy-clipboard service,
// then explicitly restarts it. The restart is the load-bearing step
// for re-installs: `enable --now` is a no-op when the service is
// already enabled (including the failed-startup-restart-loop state),
// so without the explicit restart a reinstall wouldn't propagate
// either (a) a new unit body or (b) the env vars imported by
// ImportSessionEnv. Restart is idempotent on a fresh install (the
// just-started process gets restarted once, milliseconds of churn).
func (l *LocalInstaller) EnableSystemdUnit(out io.Writer) error {
	if err := l.SystemdRun("--user", "daemon-reload"); err != nil {
		return fmt.Errorf("EnableSystemdUnit: daemon-reload: %w", err)
	}
	if err := l.SystemdRun("--user", "enable", "--now", SystemdUnitName); err != nil {
		return fmt.Errorf("EnableSystemdUnit: enable --now %s: %w", SystemdUnitName, err)
	}
	if err := l.SystemdRun("--user", "restart", SystemdUnitName); err != nil {
		return fmt.Errorf("EnableSystemdUnit: restart %s: %w", SystemdUnitName, err)
	}
	fmt.Fprintf(out, "  enabled and restarted %s\n", SystemdUnitName)
	return nil
}
