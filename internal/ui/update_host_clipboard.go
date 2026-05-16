// `c` keybind on the Hosts tab: runs `canopy host clipboard <name>`
// for the cursor's host, with a y/N confirm-modal in front of it.
// v0.18 Lane C.4.
//
// The install runs as a subprocess via tea.ExecProcess — the TUI hands
// the terminal over, the user sees the install transcript (which the
// internal/clipboard.HostInstaller streams to stdout), and the TUI
// resumes when the command exits. Same UX pattern the host-install
// (`I` key) flow uses, just routed at a different binary entry point.
//
// The subprocess is `os.Executable() host clipboard <name>` — i.e.,
// the CURRENTLY-running canopy binary, NOT `~/.local/bin/canopy` as
// resolved by the user's PATH. This matters because the user might
// have `canopy use release` active (the release binary doesn't have
// `host clipboard`); using os.Executable() guarantees the install
// runs against the canopy that knows how to do it.

package ui

import (
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// actionHostClipboard is the handler for the `c` key on the Hosts tab.
// Captures the cursor's host into the model so the confirm-modal
// handler can run the install regardless of where the cursor ends up
// during the modal lifetime (a refresh tick can shuffle rows).
func actionHostClipboard(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	h, ok := m.selectedHost()
	if !ok || h.SSHTarget == "" {
		return m, nil
	}
	m.hostClipboardName = h.Name
	m.hostClipboardTarget = h.SSHTarget
	m.mode = confirmHostClipboardMode
	return m, nil
}

// handleConfirmHostClipboardKey is the y/N gate. y/Y exec's
// `canopy host clipboard <name>` via tea.ExecProcess; anything else
// cancels. State is cleared either way.
func (m *Model) handleConfirmHostClipboardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	name := m.hostClipboardName
	m.mode = listMode
	m.hostClipboardName = ""
	m.hostClipboardTarget = ""
	if msg.String() != "y" && msg.String() != "Y" {
		return m, nil
	}
	if name == "" {
		return m, nil
	}
	canopyBin, err := os.Executable()
	if err != nil {
		log.Warn("ui.host-clipboard.executable", "err", err)
		return m, nil
	}
	cmd := exec.Command(canopyBin, "host", "clipboard", name)
	cmd.Env = os.Environ()
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.host-clipboard.failed", "host", name, "err", err)
		}
		// Refresh after the install completes so the Hosts-tab pill
		// flips to `📋 bridged` (or `📋!` on probe failure) without
		// waiting for the next 2s tick.
		return refreshAllMsg{}
	})
}

// availableHostClipboard gates the `c` key. Hosts tab only, and only
// when the cursor row has an SSH target — pressing `c` on a hosts row
// with no ssh_target would have nothing to install against.
func availableHostClipboard(m *Model) bool {
	if m.tab != tabHosts {
		return false
	}
	h, ok := m.selectedHost()
	return ok && h.SSHTarget != ""
}

// renderConfirmHostClipboard is the body of confirmHostClipboardMode.
// Mirrors renderConfirmHostSSH's shape so the two modals feel like
// peers (same friction, same prompt style).
func (m *Model) renderConfirmHostClipboard() string {
	return fmt.Sprintf(
		"%s\n\n  Install the clipboard bridge on %s (%s)?\n\n"+
			"  %s\n  %s\n\n  %s to install  ·  any other key to cancel",
		titleStyle.Render("clipboard bridge"),
		m.hostClipboardName,
		m.hostClipboardTarget,
		subtleStyle.Render("Detects the remote UID, pushes wl-paste/wl-copy wrappers,"),
		subtleStyle.Render("writes the SSH snippet, and verifies the round-trip."),
		brokenStyle.Render("y"),
	)
}
