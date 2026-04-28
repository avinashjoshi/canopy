package ui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// Update implements tea.Model. Routes incoming messages to focused
// handlers. The Model is always returned by value — Bubbletea owns the
// "current" Model, we own the next one.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case rowsLoadedMsg:
		// Refresh result. Apply rows; clamp the cursor if the list shrank.
		m.err = msg.err
		if msg.rows != nil {
			m.rows = msg.rows
		}
		if m.cursor >= len(m.rows) {
			m.cursor = max0(len(m.rows) - 1)
		}
		return m, nil

	case attachAfterMsg:
		// Resurrection completed, now attach. Same exec-process flow as
		// the direct ready-status path.
		return m, attachCmd(m.mgr, msg.session)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey is the keymap. Conductor-flavored: small, opinionated, no
// clever chords. Help is one keypress (?), nav is the standard
// arrow/jk/gG, attach is enter, refresh is r, quit is q.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showHelp {
		// Any key dismisses help.
		m.showHelp = false
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "?":
		m.showHelp = true
		return m, nil

	case "r":
		// Manual refresh. Same flow as the initial load.
		return m, refreshCmd(m.mgr, m.tc)

	case "enter":
		// Attach to the selected workspace. Resurrects first if the
		// workspace is in `stopped` state. The handoff happens via
		// tea.ExecProcess: tmux takes over the terminal, the user
		// detaches with prefix-d, control returns here, refreshCmd
		// updates the rendered status.
		return m.attachSelected()

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		return m, nil

	case "g", "home":
		m.cursor = 0
		return m, nil

	case "G", "end":
		m.cursor = max0(len(m.rows) - 1)
		return m, nil
	}

	return m, nil
}

// max0 returns max(0, n). Avoids the Bubbletea-standard generic max
// for first-time-Go simplicity (and Go 1.21+ has built-in max but
// we're staying close to the old toolchain to keep the dep surface
// boring).
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// attachSelected wires the enter-key flow: figure out what to do based
// on the selected row's status, build the right exec.Cmd, and hand it
// to tea.ExecProcess. Returns the model + a tea.Cmd; the actual tmux
// handoff happens after the Cmd fires. After detach the followup
// refreshCmd reloads rows so the status the user sees matches reality.
func (m *Model) attachSelected() (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, nil
	}
	row := m.rows[m.cursor]
	ctx := context.Background()

	// Decide what to do based on status. broken/orphaned/setting_up cases
	// surface an error in the TUI rather than handing off to a dead
	// session. ready and main go straight to attach. stopped resurrects
	// first.
	switch row.Status {
	case "main", state.StatusReady:
		// Attach directly.
		return m, attachCmd(m.mgr, row.TmuxSession)

	case state.StatusStopped:
		// Resurrect, then attach. Workspace lookup uses the row's name
		// (which is the canopy workspace name, not the tmux session).
		return m, resurrectAndAttachCmd(m.mgr, row.Name)

	case state.StatusBroken:
		m.err = fmt.Errorf("workspace %q is broken — see ~/.canopy/log/canopy.log; run `canopy rm %s` to clean up",
			row.Name, row.Name)
		return m, nil

	case state.StatusOrphaned:
		m.err = fmt.Errorf("workspace %q has no on-disk dir — run `canopy rm %s` to drop the row",
			row.Name, row.Name)
		return m, nil

	case state.StatusSettingUp:
		m.err = fmt.Errorf("workspace %q is still setting up — try refresh (r) in a moment",
			row.Name)
		return m, nil
	}

	// Unknown status: log and ignore.
	log.Warn("ui.attach.unknown-status", "name", row.Name, "status", row.Status)
	_ = ctx
	return m, nil
}

// attachCmd dispatches tea.ExecProcess against tmux's attach command.
// On detach, sends a refreshCmd so the rendered status updates.
func attachCmd(mgr *workspace.Manager, session string) tea.Cmd {
	cmd, err := mgr.Tmux.AttachCmd(context.Background(), session)
	if err != nil {
		return func() tea.Msg {
			return rowsLoadedMsg{err: err}
		}
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.attach.exec-failed", "session", session, "err", err)
			return rowsLoadedMsg{err: fmt.Errorf("attach %s: %w", session, err)}
		}
		// Detach completed cleanly; refresh rows so any state changes
		// during the session (rare but possible if the user ran canopy
		// commands inside a pane) show up.
		return refreshCmd(mgr, mgr.Tmux)()
	})
}

// resurrectAndAttachCmd handles the stopped-status flow. Runs Resurrect
// (which rebuilds the tmux session with claude --continue || claude),
// then attaches via the normal path.
func resurrectAndAttachCmd(mgr *workspace.Manager, name string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		ws, err := mgr.Resurrect(ctx, name)
		if err != nil {
			return rowsLoadedMsg{err: fmt.Errorf("resurrect %s: %w", name, err)}
		}
		// We can't return a tea.Cmd from a tea.Msg — instead we kick
		// off the attach as a follow-on by returning an attachAfterMsg
		// that Update routes through.
		return attachAfterMsg{session: ws.TmuxSession}
	}
}

// attachAfterMsg is the bridge between resurrectAndAttachCmd's
// async resurrection and the synchronous tea.ExecProcess attach. Update
// catches it and dispatches attachCmd.
type attachAfterMsg struct {
	session string
}
