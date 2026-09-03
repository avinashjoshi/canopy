// In-TUI flow for running canopy maintenance commands on a remote
// host. Triggered by:
//   - `U` on the Hosts tab → `canopy upgrade --yes`
//   - `S` on the Hosts tab → `canopy use release` (for dev-binary hosts)
//
// Both share the confirm → run → done state machine; the action/verb/
// success labels + the remote command are stored on the Model so the
// renderer + handler can stay variant-agnostic. The local `U` flow for
// the laptop's own canopy lives in upgrade.go.
//
// Why streaming instead of tea.ExecProcess: the earlier ExecProcess
// version flickered the alt-screen on entry, hid stderr inside the
// suspended TUI when the remote refused (e.g., dev-binary check), and
// gave no visual confirmation that U did anything. Capturing the SSH
// subprocess output into a safeBuffer mirrors the local upgrade flow's
// UX and keeps errors on screen.
//
// State machine:
//
//	confirming → running → doneOK
//	             running → doneError
//
// Esc cancels from confirming. Ctrl-C in running cancels the SSH
// subprocess. Any key dismisses from doneOK / doneError back to
// listMode and fires a refresh so the Hosts tab picks up whatever
// changed (new version, new symlink target, etc).

package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/avinashjoshi/canopy/internal/host"
)

// hostUpgradeState identifies the host-upgrade flow phase. Drives the
// renderer (renderHostUpgrade switches on this) and key handler
// (handleHostUpgradeKey gates per-state).
type hostUpgradeState int

const (
	hostUpgradeStateNone       hostUpgradeState = iota
	hostUpgradeStateConfirming                  // y/N gate before SSH spawn
	hostUpgradeStateRunning                     // SSH subprocess streaming
	hostUpgradeStateDoneOK                      // remote command succeeded
	hostUpgradeStateDoneError                   // remote command failed
)

// resetHostUpgradeMode clears flow state back to zero. Called on
// dismiss (Esc from confirming, any key from doneOK/doneError) and
// guards against re-entry mid-flow. The cancel func, if present, fires
// best-effort so a still-running subprocess doesn't outlive the user
// pressing q.
func (m *Model) resetHostUpgradeMode() {
	m.hostUpgradeState = hostUpgradeStateNone
	m.hostUpgradeHost = ""
	m.hostUpgradeTarget = ""
	m.hostUpgradeVersion = ""
	m.hostUpgradeAction = ""
	m.hostUpgradeVerb = ""
	m.hostUpgradeSuccess = ""
	m.hostUpgradeRemoteCmd = ""
	m.hostUpgradeOutput = ""
	m.hostUpgradeErr = nil
	m.hostUpgradeBuf = nil
	if m.hostUpgradeCancel != nil {
		m.hostUpgradeCancel()
	}
	m.hostUpgradeCancel = nil
}

// hostUpgradeShellStartedMsg is the lazy-spawn bridge from
// hostUpgradeStartCmd to Update: carries the buffer + done channel +
// cancel func so Update can store the cancel for Ctrl-C handling and
// kick off the tick + waitDone Cmds.
type hostUpgradeShellStartedMsg struct {
	buf    *safeBuffer
	done   <-chan hostUpgradeShellDoneMsg
	cancel context.CancelFunc
}

// hostUpgradeShellDoneMsg fires when the SSH subprocess completes.
// err is nil on success; non-nil includes "context canceled" when the
// user hit Ctrl-C in the running state.
type hostUpgradeShellDoneMsg struct {
	err    error
	output string // trailing buffer content the final tick missed
}

// hostUpgradeTickMsg streams the running subprocess's accumulated
// output into the View. Distinct from progressTickMsg /
// upgradeProgressTickMsg so the per-flow Update handler doesn't have
// to disambiguate by inspecting mode.
type hostUpgradeTickMsg struct {
	chunk string
	buf   *safeBuffer
}

// hostUpgradeTickCmd schedules the next drain tick at 150ms cadence
// (same as the new-workspace busyMode and the local upgrade flow).
// Stops naturally when Update sees we've left the running state.
func hostUpgradeTickCmd(buf *safeBuffer) tea.Cmd {
	return tea.Tick(progressTickInterval, func(time.Time) tea.Msg {
		return hostUpgradeTickMsg{chunk: buf.Drain(), buf: buf}
	})
}

// hostUpgradeStartCmd kicks off the SSH subprocess on a background
// goroutine. See newHostUpgradeSSHCmd for the argv/stdin shape and
// the login-shell rationale.
//
// remoteCmd is the literal script to feed to the remote shell —
// caller is responsible for non-interactivity (e.g., passing --yes).
// The local confirm modal already gave the user a chance to back out.
//
// CombinedOutput-style streaming: both stdout and stderr feed into
// the same safeBuffer so the user sees the full picture inline. The
// remote `git pull && make install` writes progress to stderr and
// install output to stdout; merging keeps rendering simple.
func hostUpgradeStartCmd(sshTarget, remoteCmd string) tea.Cmd {
	if sshTarget == "" {
		return func() tea.Msg {
			return hostUpgradeShellDoneMsg{err: fmt.Errorf("no ssh target for host")}
		}
	}
	return func() tea.Msg {
		buf := &safeBuffer{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan hostUpgradeShellDoneMsg, 1)
		go func() {
			cmd := newHostUpgradeSSHCmd(ctx, sshTarget, remoteCmd)
			cmd.Stdout = buf
			cmd.Stderr = buf
			err := cmd.Run()
			done <- hostUpgradeShellDoneMsg{err: err, output: buf.Drain()}
		}()
		return hostUpgradeShellStartedMsg{buf: buf, done: done, cancel: cancel}
	}
}

// newHostUpgradeSSHCmd builds the SSH subprocess for an upgrade /
// install / use-release run on a remote host. Two invariants make
// this function load-bearing:
//
//  1. The remote shell is `bash -l` (a LOGIN shell). Login shells
//     source ~/.bash_profile / ~/.profile, which is where version
//     managers like mise and asdf inject the toolchain PATH. Without
//     -l, a non-interactive SSH-command shell inherits the bare
//     default PATH; `make install` then runs `go build` and dies
//     with `make: go: No such file or directory` on every host where
//     Go is version-managed. (This is the regression that motivated
//     extracting this helper — see git log.)
//
//  2. The remote script travels via stdin, NOT as an SSH argv. SSH
//     joins all post-target argv with spaces and re-parses on the
//     remote shell; that silently word-splits `bash -lc <script>`
//     into garbage. Piping the script as bytes to `bash -l` sidesteps
//     the quoting/word-splitting trap entirely. internal/host/refresh.go
//     already uses this pattern for the same reason.
func newHostUpgradeSSHCmd(ctx context.Context, sshTarget, remoteCmd string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(os.Getenv("HOME"), ".canopy", "ssh-%C.sock"),
		"-o", "ControlPersist=300",
		"-o", "BatchMode=yes",
		"-o", "NumberOfPasswordPrompts=0",
		// "--" before sshTarget: without it, a target shaped like an ssh
		// option is parsed as a flag, not a hostname — see
		// internal/host/ssh.go's sshCmdInternal for the full comment;
		// same fix, same class of bug.
		"--", sshTarget,
		"bash", "-l",
	)
	cmd.Stdin = strings.NewReader(remoteCmd + "\n")
	return cmd
}

// hostUpgradeWaitDoneCmd blocks on the done channel and emits the
// completion msg.
func hostUpgradeWaitDoneCmd(done <-chan hostUpgradeShellDoneMsg) tea.Cmd {
	return func() tea.Msg { return <-done }
}

// remoteEnvPrep is the shell snippet we prepend to every remote canopy
// command so the toolchain is on PATH before `canopy upgrade` shells out
// to `make install`. `bash -l` (see newHostUpgradeSSHCmd) sources
// ~/.bash_profile / ~/.profile, which is enough on setups that put
// toolchain init there — but a large share of users have a ~/.bashrc
// that opens with one of:
//
//	[[ $- != *i* ]] && return            # Arch/Omarchy default
//	[ -z "$PS1" ] && return              # legacy
//	case $- in *i*) ;; *) return;; esac  # Ubuntu default
//
// All three guards bail under a non-interactive shell, so a `bash -l`
// (login but not interactive) skips bashrc entirely — and that's where
// mise / asdf / nvm typically inject their PATH activation. The
// v0.21.4.0 fix that added `-l` solved the login-file half of the
// population; the other half ended up exactly where we started:
// `make install` → `make: go: No such file or directory`.
//
// Rather than fight the guards (e.g., `bash -li` works but spews
// `bash: no job control in this shell` on every run into the TUI's
// captured output), we activate the common version managers directly.
// Ordering matters here — the static path enumeration runs FIRST so
// version-manager binaries are reachable before we try to invoke them:
//
//  1. Static toolchain spots are prepended first: ~/.local/bin (where
//     `curl mise.run | sh` and canopy's own installer put their
//     binaries), /usr/local/go/bin (the official Go tarball), ~/go/bin
//     (where `go install` places third-party binaries). The PATH-dedupe
//     `case` avoids accumulating duplicates across repeated invocations
//     (long-lived SSH multiplex sessions hit this).
//
//     This step's load-bearing job is to make `mise` itself findable.
//     If we ran the `command -v mise` check first, hosts with mise at
//     ~/.local/bin/mise (the canonical install path) but no `~/.local/bin`
//     on the non-interactive `bash -l` PATH would silently skip mise
//     activation — leaving go off PATH and reproducing the v0.21.4.0
//     regression. The static prepend has to come before the activation.
//
//  2. `mise activate bash` — emits the same PATH/shim exports that
//     omarchy/default/bash/init runs interactively. Now reachable
//     because step 1 put ~/.local/bin on PATH.
//
//  3. `~/.asdf/asdf.sh` — asdf's canonical activation hook. The
//     `[ -f ]` guard keeps this a no-op on hosts without asdf.
//
// All activation errors are swallowed (`|| true` + `2>/dev/null`) so a
// broken version-manager install on the remote doesn't turn a
// recoverable upgrade into a hard failure — the static path fallback
// from step 1 still gives us a fighting chance to find `go`.
const remoteEnvPrep = `for d in "$HOME/.local/bin" "/usr/local/go/bin" "$HOME/go/bin"; do [ -d "$d" ] && case ":$PATH:" in *":$d:"*) ;; *) PATH="$d:$PATH";; esac; done; export PATH; command -v mise >/dev/null 2>&1 && eval "$(mise activate bash 2>/dev/null)" 2>/dev/null || true; [ -f "$HOME/.asdf/asdf.sh" ] && . "$HOME/.asdf/asdf.sh" 2>/dev/null || true`

const (
	// remoteUpgradeCmd: pull + reinstall, non-interactive. --yes is
	// required because the local TUI confirm already happened; running
	// the remote's own y/n prompt would hang the SSH on a closed stdin.
	remoteUpgradeCmd = remoteEnvPrep + `; exec canopy upgrade --yes`

	// remoteUseReleaseCmd: flip the host's `canopy` symlink back to
	// the released binary. Idempotent — if the host is already on
	// release, the command prints a confirmation and exits 0.
	remoteUseReleaseCmd = remoteEnvPrep + `; exec canopy use release`
)

// actionHostUpgrade is the U-key handler on the Hosts tab. Captures
// the cursor host + its most-recent snapshot's version, then enters
// hostUpgradeMode in the confirming substate. The keymap gate
// (availableHostUpgrade) already excludes hosts with no snapshot, an
// outstanding error, or a "dev" canopy_version, so by the time we get
// here the action is known-safe to surface.
func actionHostUpgrade(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	return enterHostUpgrade(m, hostUpgradeOpts{
		action:    "upgrade",
		verb:      "Upgrading",
		success:   "Upgrade complete",
		remoteCmd: remoteUpgradeCmd,
	})
}

// actionHostInstall is the I-key handler on the Hosts tab. Installs
// canopy on the cursor's host via the same SSH-streaming machinery as
// upgrade — pipes install.sh from main to bash with --yes. The remote
// command comes from host.InstallScript so the CLI surface
// (`canopy host install <name>`) and this in-TUI surface stay in
// lockstep.
//
// Available on every Hosts-tab row (no per-status gate): install
// works whether the host is broken, online, or never-refreshed.
// install.sh is idempotent — on a host that already has canopy, it
// prints "already installed" and exits 0 cleanly.
func actionHostInstall(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	return enterHostUpgrade(m, hostUpgradeOpts{
		action:    "install",
		verb:      "Installing",
		success:   "Install complete",
		remoteCmd: host.InstallScript(false),
	})
}

// actionHostSwitchRelease is the S-key handler on the Hosts tab.
// Reaches for hosts running a dev binary — `canopy upgrade` refuses
// to run on those, but `canopy use release` flips the symlink back to
// the released binary and unblocks a subsequent upgrade. Pairs with
// availableHostSwitchRelease for the keymap.
func actionHostSwitchRelease(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	return enterHostUpgrade(m, hostUpgradeOpts{
		action:    "use release",
		verb:      "Switching to release",
		success:   "Switched to release canopy",
		remoteCmd: remoteUseReleaseCmd,
	})
}

// hostUpgradeOpts bundles the per-action variants the state machine
// is parameterized over: short label for the title, ongoing verb for
// the running screen, success headline for doneOK, and the actual
// remote command. Keeps actionHostUpgrade / actionHostSwitchRelease
// short and the renderer free of branching on action kind.
type hostUpgradeOpts struct {
	action    string
	verb      string
	success   string
	remoteCmd string
}

// enterHostUpgrade is the shared setup path. Resolves the cursor's
// host + ssh target + reported version, then primes the Model fields
// the confirm screen reads. Caller's responsibility to ensure the
// keymap predicate already excluded "this host can't run remoteCmd."
func enterHostUpgrade(m *Model, opts hostUpgradeOpts) (tea.Model, tea.Cmd) {
	h, ok := m.selectedHost()
	if !ok {
		return m, nil
	}
	if h.SSHTarget == "" {
		log.Warn("ui.host-upgrade.no-target", "host", h.Name, "action", opts.action)
		return m, nil
	}
	version := ""
	if snap := m.remoteSnaps[h.Name]; snap != nil {
		version = snap.CanopyVersion
	}
	m.mode = hostUpgradeMode
	m.hostUpgradeState = hostUpgradeStateConfirming
	m.hostUpgradeHost = h.Name
	m.hostUpgradeTarget = h.SSHTarget
	m.hostUpgradeVersion = version
	m.hostUpgradeAction = opts.action
	m.hostUpgradeVerb = opts.verb
	m.hostUpgradeSuccess = opts.success
	m.hostUpgradeRemoteCmd = opts.remoteCmd
	m.hostUpgradeOutput = ""
	m.hostUpgradeErr = nil
	return m, nil
}

// handleHostUpgradeKey routes key events while mode == hostUpgradeMode.
// Per-state gating mirrors handleUpgradeKey: y/Enter confirms, Esc/q
// cancels from confirming; Ctrl-C cancels the subprocess in running;
// any key dismisses from doneOK / doneError. Ctrl-C in non-running
// states is the global quit escape hatch, matching upgrade.go's
// convention so users don't get trapped in a flow.
func (m *Model) handleHostUpgradeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" {
		if m.hostUpgradeState == hostUpgradeStateRunning {
			if m.hostUpgradeCancel != nil {
				m.hostUpgradeCancel()
			}
			return m, nil
		}
		return m, tea.Quit
	}

	switch m.hostUpgradeState {
	case hostUpgradeStateConfirming:
		switch key {
		case "y", "Y", "enter":
			target := m.hostUpgradeTarget
			remoteCmd := m.hostUpgradeRemoteCmd
			m.hostUpgradeState = hostUpgradeStateRunning
			m.hostUpgradeOutput = ""
			return m, hostUpgradeStartCmd(target, remoteCmd)
		case "esc", "q", "n", "N":
			m.resetHostUpgradeMode()
			m.mode = listMode
			return m, nil
		}
		return m, nil

	case hostUpgradeStateRunning:
		// Block accidental dismissal mid-build; Ctrl-C is the only way
		// out (handled above).
		return m, nil

	case hostUpgradeStateDoneOK, hostUpgradeStateDoneError:
		// Any key dismisses; refresh so the Hosts tab repaints with the
		// new version / symlink target on the next tick.
		m.resetHostUpgradeMode()
		m.mode = listMode
		return m, m.refresh()
	}
	return m, nil
}

// renderHostUpgrade is the View dispatcher for hostUpgradeMode.
// Branches on hostUpgradeState and renders a self-contained screen
// per phase (title + body + footer). Matches the layout idioms of
// renderUpgrade for visual consistency. The per-action labels come
// from the Model fields populated by enterHostUpgrade.
func (m *Model) renderHostUpgrade() string {
	var b strings.Builder
	action := m.hostUpgradeAction
	if action == "" {
		action = "upgrade"
	}
	verb := m.hostUpgradeVerb
	if verb == "" {
		verb = "Upgrading"
	}
	success := m.hostUpgradeSuccess
	if success == "" {
		success = "Done"
	}

	b.WriteString(titleStyle.Render(fmt.Sprintf("canopy %s on %s", action, m.hostUpgradeHost)))
	b.WriteString("\n\n")

	switch m.hostUpgradeState {
	case hostUpgradeStateConfirming:
		// Suppress the `current: v…` line when the remote hasn't
		// reported a version (e.g., install on a broken host, or a
		// never-refreshed host). Printing `current: v` is uglier than
		// just omitting the line.
		if m.hostUpgradeVersion != "" {
			header := lipgloss.NewStyle().
				Foreground(lipgloss.Color("245")).
				Render(fmt.Sprintf("  current: v%s", m.hostUpgradeVersion))
			b.WriteString(header)
			b.WriteString("\n")
		}
		b.WriteString(subtleStyle.Render(fmt.Sprintf("  target:  %s", m.hostUpgradeTarget)))
		b.WriteString("\n\n")
		// "install" is not literally a `canopy install` invocation
		// (it's `curl|bash`), so route around the back-tick prompt
		// that fits upgrade/use-release. The user sees "Install
		// canopy on this host?" which matches their mental model.
		if action == "install" {
			b.WriteString("  Install canopy on this host? (runs install.sh via SSH; deps installed via\n  the remote's package manager if missing.)\n\n")
		} else {
			b.WriteString(fmt.Sprintf("  Run `canopy %s` on this host?\n\n", action))
		}
		b.WriteString("  " +
			keyPillStyle.Render("y") + subtleStyle.Render(" yes") +
			"   " +
			keyPillStyle.Render("Esc") + subtleStyle.Render(" cancel"))
		return b.String()

	case hostUpgradeStateRunning:
		b.WriteString(subtleStyle.Render(fmt.Sprintf("%s %s...", verb, m.hostUpgradeHost)))
		b.WriteString("\n\n")
		if m.hostUpgradeOutput == "" {
			b.WriteString(subtleStyle.Render("  Connecting over SSH..."))
		} else {
			b.WriteString(m.hostUpgradeOutput)
		}
		b.WriteString("\n\n")
		b.WriteString(subtleStyle.Render("  Ctrl-C to cancel"))
		return b.String()

	case hostUpgradeStateDoneOK:
		b.WriteString(readyStyle.Render(fmt.Sprintf("✓ %s on %s", success, m.hostUpgradeHost)))
		b.WriteString("\n\n")
		if m.hostUpgradeOutput != "" {
			b.WriteString(subtleStyle.Render("Output:"))
			b.WriteString("\n")
			b.WriteString(m.hostUpgradeOutput)
			b.WriteString("\n\n")
		}
		b.WriteString(subtleStyle.Render("  Press any key to return."))
		return b.String()

	case hostUpgradeStateDoneError:
		errMsg := "(unknown error)"
		if m.hostUpgradeErr != nil {
			errMsg = m.hostUpgradeErr.Error()
		}
		b.WriteString(errorStyle.Render(fmt.Sprintf("✗ canopy %s failed: %s", action, errMsg)))
		b.WriteString("\n\n")
		if m.hostUpgradeOutput != "" {
			b.WriteString(subtleStyle.Render("Output:"))
			b.WriteString("\n")
			b.WriteString(m.hostUpgradeOutput)
			b.WriteString("\n\n")
		}
		b.WriteString(subtleStyle.Render("  Press any key to return."))
		return b.String()
	}

	b.WriteString(subtleStyle.Render("(no active host job)"))
	return b.String()
}
