package ui

import (
	"bytes"
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
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
		// Phase 2: kick off per-row hint loaders in parallel. Each
		// returns a rowHintsMsg as it completes. Skipped on error
		// (no rows to decorate) and on empty lists.
		if msg.err != nil || len(m.rows) == 0 {
			return m, nil
		}
		return m, loadRowHintsCmds(m.rows, m.mgr.Cfg.ProjectRoot)

	case rowHintsMsg:
		// Late-arriving lifecycle detector result. Find the row by
		// name and merge the hints in. Silent no-op if the row is
		// gone (e.g. concurrent rm).
		for i := range m.rows {
			if m.rows[i].Name == msg.name {
				m.rows[i].Hints = msg.hints
				break
			}
		}
		return m, nil

	case attachAfterMsg:
		// Resurrection completed, now attach. Same exec-process flow as
		// the direct ready-status path.
		return m, attachCmd(m.mgr, msg.session)

	case createDoneMsg:
		// Workspace creation finished. On success we auto-attach to the
		// new workspace's tmux session — that's what the user pressed `n`
		// to do, and dropping them at the workspace beats one extra
		// "press any key" gate. On error we stay in busyMode so the
		// captured setup output is visible for diagnosis; any key
		// dismisses back to the list (handleBusyModeKey).
		m.busyDone = true
		m.busyErr = msg.err
		m.busyOutput = msg.output
		if msg.err == nil && msg.tmuxSession != "" {
			m.mode = listMode
			m.busyOp = busyOpNone
			m.busyTitle = ""
			m.busyOutput = ""
			m.busyDone = false
			return m, attachCmd(m.mgr, msg.tmuxSession)
		}
		return m, nil

	case removeDoneMsg:
		// Workspace removal finished. Same shape as createDoneMsg —
		// dismiss flips back to listMode and refreshes.
		m.busyDone = true
		m.busyErr = msg.err
		m.busyOutput = msg.output
		return m, nil

	case retryDoneMsg:
		// scripts.setup re-run finished. Same shape as the others;
		// renderBusyView branches on busyOp to label success.
		m.busyDone = true
		m.busyErr = msg.err
		m.busyOutput = msg.output
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// In newMode, route non-handled messages (mostly textinput-internal
	// like cursor blink ticks) through the input.
	if m.mode == newMode {
		var cmd tea.Cmd
		m.nameInput, cmd = m.nameInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleKey is the keymap. Conductor-flavored: small, opinionated, no
// clever chords. Help is one keypress (?), nav is the standard
// arrow/jk/gG, attach is enter, refresh is r, quit is q.
//
// Modal modes (newMode, busyMode) intercept first; only listMode
// reaches the navigation block at the bottom.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showHelp {
		// Any key dismisses help.
		m.showHelp = false
		return m, nil
	}

	// Mode-specific handling first.
	switch m.mode {
	case newMode:
		return m.handleNewModeKey(msg)
	case confirmDeleteMode:
		return m.handleConfirmDeleteKey(msg)
	case busyMode:
		return m.handleBusyModeKey(msg)
	}

	// listMode keymap.
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "b", "esc":
		// Back to the global TUI. Only meaningful when this project TUI
		// was launched from the global view (the env-var handshake in
		// model_global.go's goToProject). Outside that flow `b` and `esc`
		// would be surprising — `b` could be a future shortcut, and esc
		// historically belongs to modals — so we no-op for the standalone
		// `canopy` invocation.
		if m.fromGlobal {
			return m, tea.Quit
		}
		return m, nil

	case "?":
		m.showHelp = true
		return m, nil

	case "r":
		// Manual refresh. Same flow as the initial load.
		return m, refreshCmd(m.mgr, m.tc)

	case "n":
		// Open the new-workspace modal. Reset input each time so a
		// previous attempt doesn't leak into the fresh prompt.
		m.mode = newMode
		m.nameInput.Reset()
		m.nameInput.Focus()
		return m, textinputBlink()

	case "d":
		// Open the confirm-delete modal for the selected row. Refuses
		// to delete the synthetic main row (canopy main is ephemeral —
		// kill the tmux session externally if you want it gone).
		//
		// v0.6: run the workspace safety preflight before showing the
		// modal. When hangs are detected (uncommitted/unpushed/open-PR),
		// the modal renders the list and requires a capital F to force,
		// matching the CLI's `canopy rm --force` semantics. When clean,
		// the modal shows today's normal y/N prompt.
		if len(m.rows) == 0 {
			return m, nil
		}
		row := m.rows[m.cursor]
		if row.IsMain {
			m.err = fmt.Errorf("can't delete the main session via canopy rm — use `tmux kill-session -t %s` if you want it gone",
				row.TmuxSession)
			return m, nil
		}
		// SafetyPreflight returns nil hangs for orphan workspaces (worktree
		// dir gone) — degrade gracefully so the user can still rm orphans.
		// Errors are non-fatal; we proceed with no hangs and let Remove
		// handle the not-found case if state diverged.
		hangs, _ := m.mgr.SafetyPreflight(context.Background(), row.Name)
		m.mode = confirmDeleteMode
		m.deleteTarget = row.Name
		m.deleteHangs = hangs
		return m, nil

	case "enter":
		// Attach to the selected workspace. Resurrects first if the
		// workspace is in `stopped` state. The handoff happens via
		// tea.ExecProcess: tmux takes over the terminal, the user
		// detaches with prefix-d, control returns here, refreshCmd
		// updates the rendered status.
		return m.attachSelected()

	case "R":
		// Retry scripts.setup on a broken workspace. Capital R so it
		// doesn't collide with lowercase r (refresh). Only valid when
		// the selected row is in broken status — we just no-op
		// otherwise. Recovery flow: user fixes the underlying issue
		// (missing config, deps, etc.), presses R, scripts.setup
		// re-runs against the existing worktree.
		if len(m.rows) == 0 {
			return m, nil
		}
		row := m.rows[m.cursor]
		if row.IsMain {
			return m, nil
		}
		if row.Status != state.StatusBroken {
			m.err = fmt.Errorf("retry only applies to broken workspaces; %q is %q",
				row.Name, row.Status)
			return m, nil
		}
		m.mode = busyMode
		m.busyOp = busyOpRetry
		m.busyTitle = fmt.Sprintf("Retrying setup for %q...", row.Name)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		return m, retryCmdUI(m.mgr, row.Name)

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
		m.err = fmt.Errorf("workspace %q is broken — see ~/.canopy/log/canopy.log; press R to retry scripts.setup, or `canopy rm %s` to drop it",
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

// handleNewModeKey is the keymap while the new-workspace modal is open.
// Esc cancels back to the list. Enter submits with whatever's typed
// (empty -> namegen picks a random name). Anything else falls through
// to textinput's own Update.
func (m *Model) handleNewModeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = listMode
		m.nameInput.Blur()
		return m, nil
	case "enter":
		name := m.nameInput.Value()
		m.mode = busyMode
		m.busyOp = busyOpCreate
		m.busyTitle = "Creating workspace..."
		if name != "" {
			m.busyTitle = fmt.Sprintf("Creating workspace %q...", name)
		}
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.nameInput.Blur()
		return m, createCmd(m.mgr, name)
	}
	// Forward to textinput for character handling.
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

// handleBusyModeKey: the busy view shows the in-progress or completed
// workspace creation. While in progress, every key is ignored. Once
// done (busyDone=true), any key dismisses the view and triggers a
// refresh so the new row shows up.
func (m *Model) handleBusyModeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.busyDone {
		return m, nil
	}
	// Dismiss + refresh.
	m.mode = listMode
	m.busyOutput = ""
	m.busyTitle = ""
	m.busyDone = false
	m.busyOp = busyOpNone
	if m.busyErr != nil {
		m.err = m.busyErr
		m.busyErr = nil
	}
	return m, refreshCmd(m.mgr, m.tc)
}

// handleConfirmDeleteKey is the keymap while the delete prompt is up.
//
// Two modes based on whether v0.6 SafetyPreflight detected hangs:
//
//   - Clean (no hangs): y or Y kicks off the removal. Lowercase y is
//     an explicit choice for safety (no accidental keypress) but
//     forgiving for the muscle-memory case (most users hit y).
//   - Hanging work: ONLY capital F kicks off the (forced) removal.
//     Lowercase y, n, N, esc, anything else cancels. Capital F mirrors
//     the CLI's --force flag and makes the user's destructive intent
//     explicit ("yes, lose the uncommitted work").
//
// Cancel-by-default is the safe posture for a destructive operation;
// we only proceed on a deliberate keypress.
func (m *Model) handleConfirmDeleteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	hasHangs := len(m.deleteHangs) > 0

	// Force key (capital F): only valid path when hangs exist. Falls
	// through to cancel when no hangs (capital F isn't documented for
	// the clean path, but isn't harmful — just resets the modal).
	if msg.String() == "F" && hasHangs {
		name := m.deleteTarget
		m.mode = busyMode
		m.busyOp = busyOpRemove
		m.busyTitle = fmt.Sprintf("Force-removing workspace %q...", name)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.deleteTarget = ""
		m.deleteHangs = nil
		return m, removeCmd(m.mgr, name)
	}

	// Normal y/Y: only valid when no hangs. When hangs exist, lowercase
	// y is an UNDER-AGREEMENT — user said "yes" to the wrong question.
	// Treat as cancel to force them to acknowledge the hangs explicitly.
	if !hasHangs && (msg.String() == "y" || msg.String() == "Y") {
		name := m.deleteTarget
		m.mode = busyMode
		m.busyOp = busyOpRemove
		m.busyTitle = fmt.Sprintf("Removing workspace %q...", name)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.deleteTarget = ""
		m.deleteHangs = nil
		return m, removeCmd(m.mgr, name)
	}

	// Anything else cancels (n, N, esc, enter, stray keys, lowercase y
	// when hangs are present).
	m.mode = listMode
	m.deleteTarget = ""
	m.deleteHangs = nil
	return m, nil
}

// createDoneMsg carries the result of a Manager.Create call back to
// Update. Output is whatever Create wrote to its stdout/stderr writers.
// tmuxSession is the new workspace's session name on success — Update
// uses it to dispatch an immediate attachCmd so the user lands in the
// running session right after `n` instead of bouncing back to the list.
type createDoneMsg struct {
	output      string
	tmuxSession string
	err         error
}

// removeDoneMsg is the Remove counterpart to createDoneMsg. Same shape;
// kept distinct so future Update logic can branch (e.g. don't try to
// attach to a workspace that was just removed).
type removeDoneMsg struct {
	output string
	err    error
}

// retryDoneMsg is the RetrySetup counterpart. Same shape as the others;
// kept distinct so the success-message branch in renderBusyView (and
// any future post-retry behavior, e.g. auto-attach on success) can
// pivot on type rather than spelunking the busyOp field.
type retryDoneMsg struct {
	output string
	err    error
}

// retryCmdUI kicks off Manager.RetrySetup asynchronously. Same shape as
// createCmd / removeCmd: capture stdout+stderr to a buffer, send a
// retryDoneMsg back to Update when finished. The "UI" suffix
// disambiguates from cmd/canopy/retry.go's cobra retryCmd.
func retryCmdUI(mgr *workspace.Manager, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		_, err := mgr.RetrySetup(context.Background(), name, &buf, &buf)
		return retryDoneMsg{output: buf.String(), err: err}
	}
}

// removeCmd kicks off Manager.Remove asynchronously. Captures the
// archive script's stdout/stderr to a buffer; sends removeDoneMsg back
// to Update when finished.
func removeCmd(mgr *workspace.Manager, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		err := mgr.Remove(context.Background(), name, &buf, &buf)
		return removeDoneMsg{output: buf.String(), err: err}
	}
}

// createCmd kicks off Manager.Create asynchronously. Captures the
// streamed setup output into a single buffer (no live streaming in v0;
// the user sees output after Create returns). Sends createDoneMsg back
// to Update when finished.
func createCmd(mgr *workspace.Manager, name string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		ws, err := mgr.Create(context.Background(), name, workspace.CreateOptions{}, &buf, &buf)
		msg := createDoneMsg{output: buf.String(), err: err}
		if err == nil && ws != nil {
			msg.tmuxSession = ws.TmuxSession
		}
		return msg
	}
}

// textinputBlink dispatches the cursor blink command for the textinput.
// Wrapper kept so the modal-open code reads cleanly.
func textinputBlink() tea.Cmd {
	return textinput.Blink
}
