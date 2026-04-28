package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Visual style is intentionally calm — single accent color, clear
// typography hierarchy, no gratuitous color use. lipgloss does the
// heavy lifting for terminal-aware styling.

var (
	// titleStyle is the canopy header at the top of the TUI.
	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("99")). // pale violet
			Bold(true).
			Padding(0, 1)

	// subtleStyle dims chrome that isn't the main content (column headers,
	// help line, project label).
	subtleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	// selectedStyle highlights the row the cursor is on. Background highlight
	// + bold cell so it pops without going garish.
	selectedStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("237")).
			Foreground(lipgloss.Color("230")).
			Bold(true)

	// status colors keyed by the state.Status enum. Picked for legibility
	// on both light and dark terminals.
	readyStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))   // green
	stoppedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))  // amber
	brokenStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))  // red
	orphanedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))  // orange
	settingUpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))   // blue
	mainStatusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("99"))   // violet (matches title)

	aliveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("46")) // green ●
	deadStyle  = subtleStyle                                          // dim ○

	helpBodyStyle = lipgloss.NewStyle().Padding(1, 2)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Padding(0, 1)
)

// View implements tea.Model. Renders the current Model into a string.
// Sections: title, table, status/help line, optional error banner.
func (m *Model) View() string {
	if m.showHelp {
		return m.renderHelp()
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
		"r refresh",
		"? help",
		"q quit",
	}
	return subtleStyle.Render(strings.Join(keys, "  ·  "))
}

// renderHelp draws the full keybind reference (toggled with ?). Any key
// dismisses it back to the main view.
func (m *Model) renderHelp() string {
	body := strings.Join([]string{
		titleStyle.Render("canopy — keybindings"),
		"",
		"  ↑/↓, j/k       move selection",
		"  g, home        first row",
		"  G, end         last row",
		"",
		"  enter          attach to selected workspace (coming step 6b)",
		"  n              new workspace            (coming step 6c)",
		"  d              delete selected          (coming step 6d)",
		"  r              refresh state",
		"",
		"  ?              this help",
		"  q, ctrl-c      quit",
		"",
		subtleStyle.Render("Press any key to dismiss."),
	}, "\n")
	return helpBodyStyle.Render(body)
}

// statusStyle picks the lipgloss color for a given status string. Falls
// back to subtleStyle for the synthetic "main" status and any unknown
// values so the table still renders cleanly.
func statusStyle(s interface{}) lipgloss.Style {
	switch fmt.Sprintf("%v", s) {
	case "ready":
		return readyStyle
	case "stopped":
		return stoppedStyle
	case "broken":
		return brokenStyle
	case "orphaned":
		return orphanedStyle
	case "setting_up":
		return settingUpStyle
	case "main":
		return mainStatusStyle
	}
	return subtleStyle
}

// maxInt is the same shape as max0 but for general comparison. Avoids
// pulling in Go 1.21's built-in max for the same toolchain reasons.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
