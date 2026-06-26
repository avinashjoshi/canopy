// Agent-swap picker (v0.22). `A` from the workspaces tab opens a list
// of the cursor row's project canopy.json `agents:` allowlist. Arrow
// nav + Enter dispatches into Manager.SwapAgent; Esc cancels back to
// listMode.
//
// Why a separate file (vs folding into update.go): same separation-by-
// flow pattern as update_new.go, update_delete.go, update_kill.go etc.
// The model fields live in model.go alongside the existing modal state;
// the action / handler / dispatch / view all live here. One mental
// model per file.
//
// Out-of-scope failure modes the picker INTENTIONALLY doesn't handle:
//
//   - Remote rows (row.Host != ""). canopy agent swap doesn't yet
//     dispatch over SSH; the action predicate hides the binding for
//     remote rows so the user never sees a binding they can't use.
//
//   - The Main row. Each project's main session has no agent pane —
//     swap is workspace-scoped. Predicate hides too.
//
//   - Cross-project Managers via managerForRow. Same machinery as
//     the delete flow; no special-case needed here.

package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// availableAgentSwap gates the `A` keybind. Hidden unless:
//
//   - we're on the workspace tab (not Hosts),
//   - the cursor is on a real workspace row (not Main, not Loading),
//   - the row is LOCAL (no Host — remote swap is a future feature),
//   - at least one launcher is INSTALLED on PATH.
//
// We don't gate on canopy.json's agents allowlist anymore (D6=A):
// any installed launcher shows in the picker; picking one outside
// the allowlist auto-adds it to canopy.json. The allowlist is now a
// "remember what's been used" set rather than a "gate the picker"
// set.
func availableAgentSwap(m *Model) bool {
	if m.tab == tabHosts {
		return false
	}
	row, ok := m.list.CursorRow()
	if !ok || row.Loading || row.IsMain || row.Host != "" {
		return false
	}
	return len(agent.InstalledLaunchers()) > 0
}

// actionAgentSwap opens the agent-swap picker. Snapshots the
// installed-launchers list + current agent at open time so subsequent
// refresh ticks (which might re-read state.json) can't reshuffle the
// picker mid-decision. Picking a launcher outside canopy.json's
// `agents:` allowlist auto-adds it (D6=A); the picker doesn't pre-
// filter to allowed-only.
func actionAgentSwap(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok || row.Loading {
		return m, nil
	}
	installed := agent.InstalledLaunchers()
	if len(installed) == 0 {
		m.err = fmt.Errorf("no agent launchers installed on PATH (claude / codex / aider / opencode)")
		return m, nil
	}
	m.mode = agentSwapPickerMode
	m.agentSwapTarget = row.Name
	m.agentSwapTargetRoot = row.ProjectRoot
	m.agentSwapCurrent = currentAgentOnRow(row)
	m.agentSwapList = installed
	m.agentSwapBusy = false
	m.agentSwapResult = ""

	// Initial cursor: first agent in the list that is NOT the row's
	// current agent. Asking the picker to land on the agent you're
	// already running would be a mis-press magnet (Enter would no-op
	// via ErrSwapAlreadyCurrent).
	m.agentSwapCursor = 0
	for i, a := range m.agentSwapList {
		if a != m.agentSwapCurrent {
			m.agentSwapCursor = i
			break
		}
	}
	return m, nil
}

// currentAgentOnRow extracts the row's current agent from
// state.GlobalRow. This UI knows about agents through the row data
// (state.BuildGlobalRows populates it from state.Workspace.CurrentAgent).
// Empty string is a defensive fallback for pre-v0.22 rows that haven't
// been migrated yet — the picker treats them as "no current," which
// means no entry gets dimmed.
func currentAgentOnRow(row Row) string {
	return row.CurrentAgent
}

// handleAgentSwapPickerKey is the keymap while the picker is open.
//
// Three states the handler distinguishes:
//
//   - Busy (agentSwapBusy true): SwapAgent is in flight. The picker
//     ignores keypresses except ctrl+c (which quits canopy entirely;
//     can't safely cancel a partial swap mid-flight without leaving
//     the tmux session in a half-swapped state).
//
//   - Result shown (agentSwapResult non-empty): SwapAgent completed
//     and the picker is rendering "Swapped to X." or an error. Any
//     keypress dismisses back to listMode and clears the snapshot.
//
//   - Picker (default): arrow nav + Enter dispatches; Esc / q cancels.
//
// q is suppressed in the picker proper so a stray q press doesn't
// accidentally quit canopy from inside a modal. ctrl+c is the
// always-available escape hatch.
func (m *Model) handleAgentSwapPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.agentSwapBusy {
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	}
	if m.agentSwapResult != "" {
		// Any keypress dismisses the result.
		m.mode = listMode
		m.clearAgentSwapState()
		return m, nil
	}

	switch msg.String() {
	case "esc":
		m.mode = listMode
		m.clearAgentSwapState()
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.agentSwapCursor > 0 {
			m.agentSwapCursor--
		}
		return m, nil
	case "down", "j":
		if m.agentSwapCursor < len(m.agentSwapList)-1 {
			m.agentSwapCursor++
		}
		return m, nil
	case "enter":
		if m.agentSwapCursor < 0 || m.agentSwapCursor >= len(m.agentSwapList) {
			return m, nil
		}
		target := m.agentSwapList[m.agentSwapCursor]
		if target == m.agentSwapCurrent {
			// Same-agent shortcut: don't bother dispatching, render the
			// "already running" message immediately.
			m.agentSwapResult = fmt.Sprintf("Already running %s; nothing to swap.", target)
			return m, nil
		}
		// Resolve manager + kick off the swap as a tea.Cmd. Set busy
		// so the UI shows "Swapping..." until the command completes.
		// D6=A: if the chosen agent isn't in the project's allowlist,
		// the cmd auto-adds it to canopy.json + re-Loads the Manager
		// before the swap fires. The user just picked it from the
		// installed-launchers list; that pick IS the consent.
		mgr, err := m.resolveAgentSwapManager()
		if err != nil {
			m.agentSwapResult = "Couldn't resolve workspace: " + err.Error()
			return m, nil
		}
		m.agentSwapBusy = true
		return m, swapAgentCmd(mgr, m.agentSwapTarget, target)
	}
	return m, nil
}

// resolveAgentSwapManager re-resolves the target workspace's Manager
// from the snapshotted (name, ProjectRoot) pair. Same shape as the
// delete flow's resolveTargetMgr — survives row reordering between
// open and Enter.
func (m *Model) resolveAgentSwapManager() (*workspace.Manager, error) {
	for _, r := range m.filteredRows() {
		if r.Name != m.agentSwapTarget {
			continue
		}
		if m.agentSwapTargetRoot != "" && r.ProjectRoot != m.agentSwapTargetRoot {
			continue
		}
		return m.managerForRow(r)
	}
	return nil, fmt.Errorf("workspace %q (root %q) is no longer in the row list", m.agentSwapTarget, m.agentSwapTargetRoot)
}

// clearAgentSwapState resets the picker fields after dismiss. Mirrors
// clearNewTarget's pattern — explicit zero-value reset instead of
// relying on every keypress to leave fields in a sane state.
func (m *Model) clearAgentSwapState() {
	m.agentSwapTarget = ""
	m.agentSwapTargetRoot = ""
	m.agentSwapCurrent = ""
	m.agentSwapList = nil
	m.agentSwapCursor = 0
	m.agentSwapBusy = false
	m.agentSwapResult = ""
}

// agentSwapDoneMsg is the tea.Msg posted by swapAgentCmd when the swap
// command finishes. err is nil on success; populated with the wrapped
// error chain on failure (including ErrAgentNotAllowed,
// ErrSwapAlreadyCurrent, etc.). newAgent echoes the target so the
// success message can render without re-reading state.
type agentSwapDoneMsg struct {
	newAgent string
	err      error
}

// swapAgentCmd dispatches Manager.SwapAgent and posts an
// agentSwapDoneMsg back to the Bubbletea event loop. Pattern matches
// the existing tea.Cmd dispatchers in update_new.go (createDoneCmd
// shape — kick off the long op in a goroutine, post the result message
// when done so the Update loop can transition state).
//
// D6=A: if the chosen agent isn't in canopy.json's `agents:` allowlist,
// auto-add it to canopy.json + re-load the Manager's Cfg before the
// swap fires. The user's pick from the installed-launchers picker IS
// the explicit consent. Writes to canopy.json are atomic + preserve
// unknown keys (see config.AddAgentToCanopyJSON).
func swapAgentCmd(mgr *workspace.Manager, wsName, newAgent string) tea.Cmd {
	return func() tea.Msg {
		if !mgr.Cfg.AllowsAgent(newAgent) {
			if err := config.AddAgentToCanopyJSON(mgr.Cfg.ProjectRoot, newAgent); err != nil {
				return agentSwapDoneMsg{newAgent: newAgent, err: fmt.Errorf("auto-add agent to canopy.json: %w", err)}
			}
			// Re-load the config so SwapAgent's AllowsAgent check passes.
			// We mutate the existing Manager's Cfg in place rather than
			// constructing a new Manager — same project, same paths.
			updated, err := config.LoadFrom(mgr.Cfg.ProjectRoot)
			if err != nil {
				return agentSwapDoneMsg{newAgent: newAgent, err: fmt.Errorf("re-load canopy.json after auto-add: %w", err)}
			}
			mgr.Cfg = updated
		}
		_, err := mgr.SwapAgent(context.Background(), wsName, newAgent)
		return agentSwapDoneMsg{newAgent: newAgent, err: err}
	}
}

// handleAgentSwapDone applies the agentSwapDoneMsg result: flips
// agentSwapBusy off and stages a result string for render. Called from
// update.go's main Update switch alongside the other tea.Msg cases.
func (m *Model) handleAgentSwapDone(msg agentSwapDoneMsg) (tea.Model, tea.Cmd) {
	m.agentSwapBusy = false
	if msg.err != nil {
		// Tag known sentinels with a friendly prefix so the user doesn't
		// have to parse the wrapped error chain in their head.
		switch {
		case errors.Is(msg.err, agent.ErrAgentNotAllowed):
			m.agentSwapResult = fmt.Sprintf(
				"%s is not in this project's agents allowlist. Edit canopy.json to add it.",
				msg.newAgent)
		case errors.Is(msg.err, workspace.ErrSwapAlreadyCurrent):
			m.agentSwapResult = fmt.Sprintf("Already running %s; nothing to swap.", msg.newAgent)
		case errors.Is(msg.err, workspace.ErrSwapNoAgentPane):
			m.agentSwapResult = "No agent pane in this workspace's tmux session — run canopy switch first to resurrect it."
		default:
			m.agentSwapResult = "Swap failed: " + msg.err.Error()
		}
		return m, nil
	}
	m.agentSwapResult = fmt.Sprintf("Swapped to %s. Press any key.", msg.newAgent)
	return m, nil
}

// renderAgentSwapPicker draws the modal. Three render states match the
// keymap's three handler states:
//
//   1. Busy → spinner-ish "Swapping..." line, no list interactivity
//   2. Result shown → final message + "press any key"
//   3. Picker → list of agents with cursor + current-agent dim hint
//
// No fancy lipgloss styling — matches the existing confirm-modal density
// (renderConfirmDelete etc) to stay visually consistent with the rest
// of the modal family.
func (m *Model) renderAgentSwapPicker() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nSwap agent in workspace %q\n", m.agentSwapTarget)
	if m.agentSwapCurrent != "" {
		fmt.Fprintf(&b, "Currently running: %s\n\n", m.agentSwapCurrent)
	} else {
		b.WriteString("\n")
	}

	if m.agentSwapBusy {
		b.WriteString("  Swapping... (this kills the running agent pane and respawns)\n")
		return b.String()
	}
	if m.agentSwapResult != "" {
		b.WriteString("  ")
		b.WriteString(m.agentSwapResult)
		b.WriteString("\n\n  Press any key to dismiss.\n")
		return b.String()
	}

	for i, a := range m.agentSwapList {
		marker := "  "
		if i == m.agentSwapCursor {
			marker = "▶ "
		}
		suffix := ""
		if a == m.agentSwapCurrent {
			suffix = "  (current)"
		}
		fmt.Fprintf(&b, "%s%s%s\n", marker, a, suffix)
	}
	b.WriteString("\n")
	b.WriteString("  ↑/↓ select • enter swap • esc cancel\n")
	return b.String()
}

