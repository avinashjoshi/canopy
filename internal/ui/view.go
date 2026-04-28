package ui

import (
	"fmt"
	"strings"

	"github.com/avinashjoshi/canopy/internal/ui/projectlist"
)

// Styles + helpers live in render.go (shared with the new GlobalModel and
// projectlist sub-component). This file holds only the project-mode
// View() implementation.

// View implements tea.Model. Renders the current Model into a string.
// Sections: title, table, status/help line, optional error banner.
func (m *Model) View() string {
	if m.showHelp {
		return m.renderHelp()
	}
	switch m.mode {
	case newMode:
		return m.renderNewModal()
	case confirmDeleteMode:
		return m.renderConfirmDelete()
	case busyMode:
		return m.renderBusyView()
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("canopy") + " " + subtleStyle.Render(m.projectName))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("error: %v", m.err)))
		b.WriteString("\n\n")
	}

	if len(m.rows) == 0 {
		b.WriteString(subtleStyle.Render("No workspaces. Press 'n' to create one, or 'q' to quit."))
		b.WriteString("\n\n")
	} else {
		b.WriteString(m.renderTable())
		b.WriteString("\n")
		if hint := m.selectedHint(); hint != "" {
			b.WriteString("  ")
			b.WriteString(brokenStyle.Render("hint:"))
			b.WriteString(" ")
			b.WriteString(hint)
			b.WriteString("\n  ")
			b.WriteString(subtleStyle.Render("press R to retry scripts.setup against the existing worktree"))
			b.WriteString("\n\n")
		}
	}

	b.WriteString(m.renderHelpLine())
	return b.String()
}

// renderTable draws the workspace list. tabwriter would simplify column
// alignment but doesn't play with lipgloss styled cells, so we compute
// widths manually. Five columns: TMUX badge, NAME, BRANCH, STATUS, PORT.
// SESSION is dropped from the TUI table (visible enough via tmux's own
// status bar after attach) to keep the line narrow.
func (m *Model) renderTable() string {
	// Column widths derived from data, with sane minimums.
	colName, colBranch, colStatus, colPort := 4, 6, 6, 4
	for _, r := range m.rows {
		colName = maxInt(colName, len(r.Name))
		colBranch = maxInt(colBranch, len(r.Branch))
		colStatus = maxInt(colStatus, len(string(r.Status)))
		if r.Port > 0 {
			colPort = maxInt(colPort, len(fmt.Sprintf("%d", r.Port)))
		}
	}

	header := fmt.Sprintf("  %-*s  %-*s  %-*s  %*s",
		colName, "NAME",
		colBranch, "BRANCH",
		colStatus, "STATUS",
		colPort, "PORT")

	var b strings.Builder
	b.WriteString(subtleStyle.Render(header))
	b.WriteString("\n")

	for i, r := range m.rows {
		port := "—"
		if r.Port > 0 {
			port = fmt.Sprintf("%d", r.Port)
		}
		badge := deadStyle.Render("○")
		if r.Alive {
			badge = aliveStyle.Render("●")
		}
		statusCell := statusStyle(r.Status).Render(fmt.Sprintf("%-*s", colStatus, r.Status))
		line := fmt.Sprintf("%s %-*s  %-*s  %s  %*s",
			badge,
			colName, r.Name,
			colBranch, r.Branch,
			statusCell,
			colPort, port,
		)
		// Append v0.6 lifecycle hint badges (rename / shipped /
		// pr_status). Reuses the same renderer as the global TUI so
		// both surfaces produce identical output. Empty string when
		// no hints are active — keeps rows visually unchanged for
		// workspaces without lifecycle signals.
		if hintBadges := projectlist.RenderHintBadges(r.Hints); hintBadges != "" {
			line += "  " + hintBadges
		}
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// renderHelpLine is the one-line keybind cheat at the bottom of the
// main view. The full help (?) shows more.
func (m *Model) renderHelpLine() string {
	keys := []string{
		"↑/↓ navigate",
		"enter attach",
		"n new",
		"d delete",
		"R retry",
		"r refresh",
	}
	if m.fromGlobal {
		keys = append(keys, "b back")
	}
	keys = append(keys, "? help", "q quit")
	return subtleStyle.Render(strings.Join(keys, "  ·  "))
}

// renderNewModal is the new-workspace prompt. Shows the textinput plus
// a one-line hint. Esc cancels, Enter submits.
func (m *Model) renderNewModal() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("canopy new") + " " + subtleStyle.Render(m.projectName))
	b.WriteString("\n\n")
	b.WriteString("  Workspace name (leave blank for a random one):")
	b.WriteString("\n\n  ")
	b.WriteString(m.nameInput.View())
	b.WriteString("\n\n")
	b.WriteString(subtleStyle.Render("  enter to create  ·  esc to cancel"))
	return b.String()
}

// renderConfirmDelete is the modal shown before tearing down a workspace.
//
// Two visual modes based on whether the v0.6 SafetyPreflight detected
// hangs (uncommitted/unpushed/open-PR):
//
//   - Clean (no hangs): standard y/N prompt as in v0.5.
//   - Hanging work: lists each hang as a bullet point in red, requires
//     CAPITAL F to force. Lowercase y or any other key cancels — capital
//     F mirrors the CLI's --force flag and makes the destructive intent
//     explicit. The prompt copy spells out the consequences ("uncommitted
//     work will be lost") so the user can't accidentally torch progress.
func (m *Model) renderConfirmDelete() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("canopy rm"))
	b.WriteString("\n\n")

	if len(m.deleteHangs) > 0 {
		b.WriteString(fmt.Sprintf("  ! Refusing to remove workspace %q — hanging work detected:\n\n", m.deleteTarget))
		for _, h := range m.deleteHangs {
			b.WriteString("    ")
			b.WriteString(brokenStyle.Render("•"))
			b.WriteString(" ")
			b.WriteString(h)
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render("  Resolve the issues above, OR press the force key to remove anyway.\n"))
		b.WriteString(subtleStyle.Render("  Forced removal still runs scripts.archive + tmux kill + git worktree remove.\n"))
		b.WriteString(subtleStyle.Render("  Uncommitted work is lost permanently.\n"))
		b.WriteString("\n")
		b.WriteString("  ")
		b.WriteString(brokenStyle.Render("F"))
		b.WriteString(" (capital) to force-remove  ·  any other key to cancel")
		return b.String()
	}

	b.WriteString(fmt.Sprintf("  Remove workspace %q?\n\n", m.deleteTarget))
	b.WriteString(subtleStyle.Render("  This runs scripts.archive, kills the tmux session, removes the\n"))
	b.WriteString(subtleStyle.Render("  git worktree, deletes the underlying branch, and drops the row\n"))
	b.WriteString(subtleStyle.Render("  from state.json.\n"))
	b.WriteString("\n")
	b.WriteString("  ")
	b.WriteString(brokenStyle.Render("y"))
	b.WriteString(" to remove  ·  any other key to cancel")
	return b.String()
}

// renderBusyView is shown while a Create or Remove is in progress and
// immediately after it completes (so the user can see the captured
// output). While busy, it's a simple "working..." line; once done, it
// shows the success/error summary plus the captured output buffer.
func (m *Model) renderBusyView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.busyTitle))
	b.WriteString("\n\n")

	if !m.busyDone {
		b.WriteString(subtleStyle.Render("  Working — this may take a few seconds while scripts.setup runs."))
		b.WriteString("\n\n")
		b.WriteString(subtleStyle.Render("  (The TUI is responsive; canopy is doing the heavy lifting in a goroutine.)"))
		return b.String()
	}

	// Done: show the result. Success message pivots on which operation
	// just finished — same view, three different verbs.
	if m.busyErr != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("Failed: %v", m.busyErr)))
	} else {
		b.WriteString(readyStyle.Render(busySuccessMessage(m.busyOp)))
	}
	b.WriteString("\n\n")
	if m.busyOutput != "" {
		b.WriteString(subtleStyle.Render("Output:"))
		b.WriteString("\n")
		b.WriteString(m.busyOutput)
		b.WriteString("\n")
	}
	b.WriteString(subtleStyle.Render("Press any key to dismiss."))
	return b.String()
}

// renderHelp draws the full keybind reference (toggled with ?). Any key
// dismisses it back to the main view.
func (m *Model) renderHelp() string {
	lines := []string{
		titleStyle.Render("canopy — keybindings"),
		"",
		"  ↑/↓, j/k       move selection",
		"  g, home        first row",
		"  G, end         last row",
		"",
		"  enter          attach to selected workspace",
		"  n              new workspace",
		"  d              delete selected (with confirmation)",
		"  R              retry scripts.setup on a broken workspace",
		"  r              refresh state",
		"",
		"  ?              this help",
	}
	if m.fromGlobal {
		lines = append(lines, "  b, esc         back to canopy global")
	}
	lines = append(lines,
		"  q, ctrl-c      quit",
		"",
		subtleStyle.Render("Press any key to dismiss."),
	)
	return helpBodyStyle.Render(strings.Join(lines, "\n"))
}

// selectedHint returns the auto-diagnosis hint for the row currently
// under the cursor, but only when it's a broken row that has a hint
// captured. Empty otherwise so the caller can skip the whole hint
// line. Defensive against an empty rows slice.
func (m *Model) selectedHint() string {
	if len(m.rows) == 0 {
		return ""
	}
	r := m.rows[m.cursor]
	if r.IsMain || r.Status != "broken" || r.LastErrorHint == "" {
		return ""
	}
	return r.LastErrorHint
}

// busySuccessMessage maps a completed busy-mode op to the right success
// line. Kept as a small switch so the View stays declarative and so we
// can extend without touching renderBusyView again.
func busySuccessMessage(op busyOpKind) string {
	switch op {
	case busyOpRemove:
		return "Workspace removed."
	case busyOpRetry:
		return "Workspace recovered — scripts.setup re-ran cleanly."
	case busyOpCreate:
		return "Workspace created successfully."
	}
	return "Done."
}

// statusStyle, statusCell, liveBadge, portCell, maxInt all live in
// render.go now (shared with the global TUI + projectlist sub-component).
