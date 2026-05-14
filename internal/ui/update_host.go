// Host-management handlers + Cmds for the Remote hosts tab. v0.17
// Phase 1l. Carved out of update.go in the post-1l cleanup so the
// host-specific flow (detail, remove, add-form, ssh-copy-id) lives
// next to its peers instead of mixed with workspace verbs.

package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/host"
)

// actionHostEnter opens the read-only detail view for the selected
// host. v0.17 Phase 1l.
func actionHostEnter(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	h, ok := m.selectedHost()
	if !ok {
		return m, nil
	}
	m.hostDetailTarget = h.Name
	m.mode = hostDetailMode
	return m, nil
}

// handleHostDetailKey: esc or any other key exits back to listMode.
func (m *Model) handleHostDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" || msg.String() == "q" || msg.Type == tea.KeyEnter {
		m.mode = listMode
		m.hostDetailTarget = ""
	}
	return m, nil
}

// actionHostSetupAuth opens the ssh-copy-id offer for the cursor's
// host. Same modal the post-Add probe surfaces on AuthFailed; lets
// the user retry auth setup without deleting and re-adding the host.
// v0.17 Phase 1l polish.
func actionHostSetupAuth(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	h, ok := m.selectedHost()
	if !ok {
		return m, nil
	}
	m.pendingProbeHost = h.Name
	m.pendingProbeTarget = h.SSHTarget
	m.mode = confirmSSHCopyIDMode
	return m, nil
}

// actionHostSSH stages a confirm prompt before exec'ing `ssh <target>`
// for the cursor's host. We don't fire the subprocess directly because
// `s` is a low-friction key (single press, no modifier) and the action
// is high-disruption: it tears the user out of the TUI and into a
// remote shell. The y/N gate makes the intent deliberate.
//
// The actual exec happens in handleConfirmHostSSHKey on y/Y; everything
// else cancels and returns to listMode.
func actionHostSSH(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	h, ok := m.selectedHost()
	if !ok || h.SSHTarget == "" {
		return m, nil
	}
	m.hostSSHName = h.Name
	m.hostSSHTarget = h.SSHTarget
	m.mode = confirmHostSSHMode
	return m, nil
}

// handleConfirmHostSSHKey is the y/N gate for the SSH-into-host
// confirmation. y/Y execs ssh and hands the terminal off via
// tea.ExecProcess; anything else cancels. State is cleared either way.
// On detach we kick a refreshAllMsg so any side effects of the shell
// session (workspaces started/killed, canopy upgraded) repaint
// immediately.
func (m *Model) handleConfirmHostSSHKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	name := m.hostSSHName
	target := m.hostSSHTarget
	m.mode = listMode
	m.hostSSHName = ""
	m.hostSSHTarget = ""
	if msg.String() != "y" && msg.String() != "Y" {
		return m, nil
	}
	if target == "" {
		return m, nil
	}
	// `--` defends against a registry SSHTarget that starts with `-`
	// being interpreted as an ssh flag (e.g. `-oProxyCommand=...`).
	// Registry entries are user-typed today, but the file lives at
	// ~/.canopy/hosts.json and any process that can write it owns the
	// next SSH dispatch — defense in depth, not redundant.
	cmd := exec.Command("ssh", "--", target)
	cmd.Env = os.Environ()
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.host-ssh.failed", "host", name, "err", err)
		}
		return refreshAllMsg{}
	})
}

// actionHostRemove opens the confirm modal for removing the cursor's
// host from the registry. v0.17 Phase 1l.
func actionHostRemove(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	h, ok := m.selectedHost()
	if !ok {
		return m, nil
	}
	m.hostRemoveTarget = h.Name
	m.mode = confirmHostRemoveMode
	return m, nil
}

// selectedHost returns the currently-cursored host on the Hosts tab.
// Returns (zero, false) when the cursor is out of range — e.g. an
// empty registry or a refresh that dropped the row.
func (m *Model) selectedHost() (host.Host, bool) {
	if m.tab != tabHosts {
		return host.Host{}, false
	}
	if m.hostsCursor < 0 || m.hostsCursor >= len(m.hostList) {
		return host.Host{}, false
	}
	// hostList isn't necessarily sorted alphabetically; hosts.BuildRows
	// re-sorts by name for the render. Mirror that ordering here so the
	// cursor index matches what the user sees on screen.
	sorted := make([]host.Host, len(m.hostList))
	copy(sorted, m.hostList)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return sorted[m.hostsCursor], true
}

// handleConfirmHostRemoveKey is the y/N gate for `d` on a host.
// v0.17 Phase 1l. Mirrors handleConfirmKillKey's shape: y/Y proceeds,
// anything else cancels.
func (m *Model) handleConfirmHostRemoveKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	target := m.hostRemoveTarget
	m.mode = listMode
	m.hostRemoveTarget = ""
	if msg.String() != "y" && msg.String() != "Y" {
		return m, nil
	}
	return m, removeHostCmd(target)
}

// removeHostCmd runs registry.Remove in a goroutine and emits
// refreshAllMsg so the Hosts tab repaints without the deleted row.
func removeHostCmd(name string) tea.Cmd {
	return func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Warn("ui.host-remove.home-failed", "err", err)
			return refreshAllMsg{}
		}
		reg, err := host.NewRegistry(filepath.Join(home, ".canopy"))
		if err != nil {
			log.Warn("ui.host-remove.registry-failed", "err", err)
			return refreshAllMsg{}
		}
		if err := reg.Remove(name); err != nil {
			log.Warn("ui.host-remove.remove-failed", "name", name, "err", err)
		}
		return refreshAllMsg{}
	}
}

// openAddHostForm transitions to the in-TUI add-host form. Two
// textinputs (name + ssh-target) render side-by-side; Tab switches
// focus. v0.17 Phase 1l polish — replaces the prior two-mode
// sequential flow so the user sees both fields at once.
func (m *Model) openAddHostForm() tea.Cmd {
	m.mode = addHostFormMode
	m.hostAddName = ""
	m.hostAddTarget = ""
	m.hostAddFocus = 0
	m.nameInput.Reset()
	m.nameInput.Placeholder = "e.g. tower"
	m.nameInput.Focus()
	m.targetInput.Reset()
	m.targetInput.Placeholder = "user@host or host.tail.ts.net"
	m.targetInput.Blur()
	return textinputBlink()
}

// handleAddHostFormKey routes keys for the in-TUI add-host form.
// Tab / shift+tab cycle focus between name and target inputs.
// Enter submits when both fields are non-empty; esc cancels.
// Anything else forwards to whichever input is focused.
func (m *Model) handleAddHostFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = listMode
		m.hostAddName = ""
		m.hostAddTarget = ""
		m.nameInput.Reset()
		m.nameInput.Blur()
		m.targetInput.Reset()
		m.targetInput.Blur()
		return m, nil
	case "tab", "shift+tab":
		m.hostAddFocus ^= 1
		if m.hostAddFocus == 0 {
			m.nameInput.Focus()
			m.targetInput.Blur()
		} else {
			m.nameInput.Blur()
			m.targetInput.Focus()
		}
		return m, textinputBlink()
	case "enter":
		name := strings.TrimSpace(m.nameInput.Value())
		target := strings.TrimSpace(m.targetInput.Value())
		if name == "" {
			m.hostAddFocus = 0
			m.nameInput.Focus()
			m.targetInput.Blur()
			return m, textinputBlink()
		}
		if target == "" {
			m.hostAddFocus = 1
			m.nameInput.Blur()
			m.targetInput.Focus()
			return m, textinputBlink()
		}
		m.mode = listMode
		m.hostAddName = ""
		m.hostAddTarget = ""
		m.nameInput.Reset()
		m.nameInput.Blur()
		m.targetInput.Reset()
		m.targetInput.Blur()
		// After Add, probe the host. The probe Cmd emits
		// hostProbeResultMsg which the Update handler turns into a
		// confirm-ssh-copy-id modal when auth fails.
		return m, tea.Batch(
			addHostCmd(name, target),
			probeHostCmd(name, target),
		)
	}
	// Forward to focused input.
	var cmd tea.Cmd
	if m.hostAddFocus == 0 {
		m.nameInput, cmd = m.nameInput.Update(msg)
	} else {
		m.targetInput, cmd = m.targetInput.Update(msg)
	}
	return m, cmd
}

// addHostCmd writes the registry on a background goroutine and emits
// refreshAllMsg so the Hosts tab sees the new row. Failures land on
// m.err via a separate errMsg path.
func addHostCmd(name, sshTarget string) tea.Cmd {
	return func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil {
			return errMsg{err: fmt.Errorf("add host: $HOME: %w", err)}
		}
		reg, err := host.NewRegistry(filepath.Join(home, ".canopy"))
		if err != nil {
			return errMsg{err: fmt.Errorf("add host: %w", err)}
		}
		if err := reg.Add(name, host.Host{SSHTarget: sshTarget, Type: "ssh"}); err != nil {
			return errMsg{err: fmt.Errorf("add host: %w", err)}
		}
		return refreshAllMsg{}
	}
}

// hostProbeResultMsg carries the outcome of the post-Add connectivity
// probe. AuthFailed triggers the ssh-copy-id offer modal; other
// errors are surfaced via m.err so the user knows registration
// succeeded but the host isn't reachable. v0.17 Phase 1l.
type hostProbeResultMsg struct {
	hostName  string
	sshTarget string
	authFail  bool
	err       error // any non-auth error (timeout, DNS, etc.)
}

// probeHostCmd runs an SSH BatchMode connection check against the
// newly-added host. BatchMode=yes makes Permission-denied a hard fail
// (no password prompt), which is exactly what we want to detect.
//
// 3s timeout — same shape as the refresh fan-out so we don't wedge
// the TUI on an unreachable host.
func probeHostCmd(name, sshTarget string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := host.SSHCmdBatch(ctx, sshTarget, "true")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return hostProbeResultMsg{hostName: name, sshTarget: sshTarget}
		}
		outStr := string(out)
		// Permission denied = need ssh-copy-id. Everything else
		// (timeout, no route, unknown host) is a non-auth failure.
		if strings.Contains(outStr, "Permission denied") ||
			strings.Contains(outStr, "publickey") {
			return hostProbeResultMsg{hostName: name, sshTarget: sshTarget, authFail: true}
		}
		return hostProbeResultMsg{hostName: name, sshTarget: sshTarget, err: fmt.Errorf("%v: %s", err, strings.TrimSpace(outStr))}
	}
}

// actionHostUpgrade — the U-key handler on the Hosts tab — lives in
// update_host_upgrade.go, which owns the multi-state TUI flow
// (confirm → run → done) for upgrading canopy on a remote host.

// handleConfirmSSHCopyIDKey: y/Y runs ssh-copy-id as a subprocess
// (which prompts for the remote password); anything else dismisses.
// v0.17 Phase 1l.
func (m *Model) handleConfirmSSHCopyIDKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	target := m.pendingProbeTarget
	m.mode = listMode
	m.pendingProbeHost = ""
	m.pendingProbeTarget = ""
	if msg.String() != "y" && msg.String() != "Y" {
		return m, nil
	}
	cmd := exec.Command("ssh-copy-id", target)
	cmd.Env = os.Environ()
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.ssh-copy-id.failed", "target", target, "err", err)
		}
		return refreshAllMsg{}
	})
}
