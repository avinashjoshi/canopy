// Shared lipgloss styles + small render helpers used by every Bubbletea
// surface in this package: the project Model (today's view.go), the new
// GlobalModel, the InitSplashModel, and the projectlist sub-component.
//
// Keeping the styles in one file means a single edit changes the canopy
// visual identity everywhere — title color, status colors, selection
// highlight, error banner — without hunting through three different
// view files.

package ui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"

	"github.com/oncactus/canopy/internal/state"
)

// Visual identity is intentionally calm: one accent color (violet),
// status colors picked for legibility on both light and dark terminals,
// no gratuitous saturation. lipgloss does the heavy lifting.
var (
	// titleStyle is the canopy header at the top of every TUI screen.
	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("99")). // pale violet
			Bold(true).
			Padding(0, 1)

	// subtleStyle dims chrome that isn't the main content (column headers,
	// help line, project label, hints).
	subtleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	// selectedStyle highlights the row the cursor is on. Bright violet
	// background + near-white foreground + bold so the selected row
	// pops on both light and dark terminals. The previous styling
	// (bg=237 dim gray) was too subtle on terminals with dark themes
	// and high-contrast color palettes.
	selectedStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("62")).
			Foreground(lipgloss.Color("231")).
			Bold(true)

	// status colors keyed by the state.Status enum. Mirrored in lsGlobal's
	// CLI output as terminal escapes for consistency.
	readyStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))  // green
	stoppedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // amber
	brokenStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
	orphanedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("208")) // orange
	settingUpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))  // blue
	mainStatusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("99"))  // violet (matches title)

	aliveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("46")) // green ●
	deadStyle  = subtleStyle                                          // dim ○

	helpBodyStyle = lipgloss.NewStyle().Padding(1, 2)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Padding(0, 1)
)

// statusStyle returns the lipgloss style for a given workspace status.
// Falls back to subtleStyle for unknown values + the synthetic "main"
// status (rendered violet to match the title), so the table renders
// cleanly even if state schema adds new statuses we haven't styled yet.
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

// liveBadge returns the colored ●/○ glyph for a row's tmux liveness.
// The output is fixed-width (one rune) so column alignment doesn't shift
// between alive and dead rows.
func liveBadge(alive bool) string {
	if alive {
		return aliveStyle.Render("●")
	}
	return deadStyle.Render("○")
}

// statusCell renders a status string padded to the given width and styled
// per its semantic color. Used by both project and global table renderers.
func statusCell(status state.Status, width int) string {
	return statusStyle(status).Render(fmt.Sprintf("%-*s", width, status))
}

// portCell formats a port for display: zero is rendered as "—" (the
// no-port-allocated sentinel), positive integers as decimal. Width is
// the column width including padding; the cell is right-aligned because
// numeric columns read more naturally that way.
func portCell(port int, width int) string {
	if port <= 0 {
		return fmt.Sprintf("%*s", width, "—")
	}
	return fmt.Sprintf("%*d", width, port)
}

// maxInt returns the larger of a and b. Avoids Go 1.21's built-in max
// to keep the toolchain floor permissive (canopy targets older Go for
// distribution friendliness — see docs/design/v0-canopy.md).
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
