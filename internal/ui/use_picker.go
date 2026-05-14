// UsePickerModel is the interactive single-screen picker behind
// `canopy use` (no args, on an interactive terminal). One screen,
// arrow keys + Enter, exits to the caller for the actual symlink
// swap.
//
// Why a separate Bubbletea model (vs the main TUI's project/global
// tabs): consistency with InitSplashModel. Both are one-screen
// "pick one thing, exit" surfaces that don't fit the main TUI's
// tab+drawer chrome and don't want to. The main TUI is for browsing
// state; this picker is for a single decision.
//
// Boundary: the picker does NOT call switchToRelease /
// switchToWorkspace itself. It exits cleanly (tea.Quit) with the
// chosen target string; the cmd/canopy caller dispatches in a
// normal (post-altscreen) terminal. Three reasons documented in
// docs/design/v0.18-canopy-use-tui.md — TL;DR: build output
// ("Building canopy in …") wants a normal terminal, errors flow
// through cobra's RunE chain, and we follow the InitSplashModel
// precedent.
//
//   ┌────────────────────────────────────────────────────────────┐
//   │ canopy use                                                 │
//   │                                                            │
//   │   Active: ~/.local/bin/canopy -> canopy.bin                │
//   │                                                            │
//   │ ▶ release      —          v0.17.5.0    built 2h ago        │
//   │   feature-foo  —          DEV          built 5m ago        │
//   │   wip-bar      wip-bar    DEV          (not built)         │  ← dim
//   │                                                            │
//   │   ↑/↓ select • enter switch • b build then switch • q ...  │
//   └────────────────────────────────────────────────────────────┘

package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// UseRow is one selectable target — the release binary or a canopy
// source worktree. Built by cmd/canopy/use.useRows() and rendered by
// both this picker and the tabular CLI list (sharing one source of
// truth so the columns can't drift).
type UseRow struct {
	// Target is the name the user types to `canopy use` (e.g.
	// "release" or "feature-foo"). Picker returns this string on
	// Enter for the caller to feed into the existing switch funcs.
	Target string

	// Branch is the column display. "—" when branch matches name
	// or for the release row (no branch).
	Branch string

	// Version is the column display ("v0.17.5.0+abc", "DEV", "—").
	Version string

	// Built is the column display ("built 2h ago" / "(not built)").
	Built string

	// BinaryPath is the on-disk path of the binary this row points
	// at. The picker compares it to the current symlink target to
	// derive Active; renderers don't otherwise read it.
	BinaryPath string

	// IsRelease distinguishes the release pseudo-row from real
	// workspace rows. 'b' (build-then-switch) is no-op on release
	// rows; the build flow is workspace-only.
	IsRelease bool

	// HasBinary is true if the on-disk binary exists at BinaryPath.
	// Missing-binary rows render dim and refuse Enter (caller would
	// fail-fast anyway, but the picker gives a clearer hint inline).
	HasBinary bool

	// Active is true for the row that matches the current symlink
	// target. Renders with the ▶ marker.
	Active bool
}

// useActiveStyle styles the ▶ marker on the active row. Subtle
// accent so selectedStyle (the cursor highlight) still wins
// visually when the cursor is on the active row.
var useActiveStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("99")). // matches titleStyle violet
	Bold(true)

// usePickerMinWidth is the lower bound below which the picker
// degrades to a single-line hint. Picked so a typical "Active: <path>
// -> <target>" header still fits; under this width lipgloss row
// layout wraps unpredictably and the picker looks frozen.
const usePickerMinWidth = 40

// UsePickerModel is the single-screen picker. State is small: the
// rows, cursor index, terminal dims, and the chosen target + build
// flag handed back to the caller after Quit.
type UsePickerModel struct {
	rows []UseRow

	cursor    int
	activeIdx int // -1 if no row matches current symlink

	width, height int

	// activeSymlinkText is the "Active: …" header copy. Built once
	// by NewUsePicker because os.Readlink + the symlink path don't
	// change during the picker's lifetime.
	activeSymlinkText string

	// hint is a one-frame inline message rendered below the rows.
	// Used by Enter-on-missing-binary and 'b'-on-release to nudge
	// the user instead of just doing nothing.
	hint string

	// chosenTarget is the row.Target the user selected, or "" if
	// they cancelled. Read by the caller after Run returns.
	chosenTarget string

	// chosenBuild is true if the user pressed 'b' on a workspace
	// row. Caller dispatches to switchToWorkspace with build=true.
	chosenBuild bool
}

// NewUsePicker constructs a picker over rows with the given header
// "Active: …" line. activeSymlinkText is rendered verbatim — the
// caller (cmd/canopy/use.go) formats it the same way it formats the
// CLI list header so the two surfaces stay visually aligned.
func NewUsePicker(rows []UseRow, activeSymlinkText string) *UsePickerModel {
	activeIdx := -1
	cursor := 0
	for i := range rows {
		if rows[i].Active {
			activeIdx = i
			cursor = i // start on the active row — Enter is a no-op then
			break
		}
	}
	return &UsePickerModel{
		rows:              rows,
		cursor:            cursor,
		activeIdx:         activeIdx,
		activeSymlinkText: activeSymlinkText,
	}
}

// RunUsePicker launches the picker. Returns (target, withBuild, err):
//   - target == "" means the user cancelled (caller exits 0 silently)
//   - target == "release" or a workspace name → caller dispatches to
//     the existing switch funcs
//   - withBuild == true means the user pressed 'b' → caller passes
//     build=true to switchToWorkspace (never set on release rows)
func RunUsePicker(rows []UseRow, activeSymlinkText string) (string, bool, error) {
	m := NewUsePicker(rows, activeSymlinkText)
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return "", false, err
	}
	pm, ok := final.(*UsePickerModel)
	if !ok {
		return "", false, nil
	}
	return pm.chosenTarget, pm.chosenBuild, nil
}

// Init implements tea.Model — purely reactive, no startup work.
func (m *UsePickerModel) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
//
// Key handling:
//
//	↑ / k       cursor up (wraps to bottom)
//	↓ / j       cursor down (wraps to top)
//	home / g    jump to first row
//	end / G     jump to last row
//	enter       pick (no-op on missing-binary; sets hint instead)
//	b           pick with build flag (workspace rows only)
//	esc / q / ^c cancel
func (m *UsePickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		// Clear any prior hint on the next keypress — hint is one-
		// frame only, so a fresh ↑/↓ doesn't carry "press b to build"
		// stale across renders.
		m.hint = ""

		switch msg.String() {
		case "up", "k":
			if len(m.rows) == 0 {
				return m, nil
			}
			m.cursor--
			if m.cursor < 0 {
				m.cursor = len(m.rows) - 1
			}
			return m, nil

		case "down", "j":
			if len(m.rows) == 0 {
				return m, nil
			}
			m.cursor = (m.cursor + 1) % len(m.rows)
			return m, nil

		case "home", "g":
			m.cursor = 0
			return m, nil

		case "end", "G":
			if len(m.rows) > 0 {
				m.cursor = len(m.rows) - 1
			}
			return m, nil

		case "enter":
			if len(m.rows) == 0 {
				return m, nil
			}
			row := m.rows[m.cursor]
			if !row.HasBinary {
				// Missing-binary row: inline hint instead of erroring
				// out post-altscreen.
				if row.IsRelease {
					m.hint = "release binary missing — run `make install` on main first"
				} else {
					m.hint = "no dev binary built — press b to build it now"
				}
				return m, nil
			}
			m.chosenTarget = row.Target
			return m, tea.Quit

		case "b", "B":
			if len(m.rows) == 0 {
				return m, nil
			}
			row := m.rows[m.cursor]
			if row.IsRelease {
				// Release is never built by `canopy use`.
				m.hint = "release isn't built by canopy use — see `make install`"
				return m, nil
			}
			m.chosenTarget = row.Target
			m.chosenBuild = true
			return m, tea.Quit

		case "esc", "q", "Q", "ctrl+c":
			m.chosenTarget = ""
			m.chosenBuild = false
			return m, tea.Quit
		}
	}
	return m, nil
}

// View implements tea.Model. Renders the title, active-line header,
// rows, optional inline hint, and footer help legend.
//
// Narrow-terminal fallback: under usePickerMinWidth columns, lipgloss
// row layout wraps unpredictably and the user thinks the picker is
// frozen. We render a single-line hint pointing at `canopy use --list`
// (the documented escape hatch).
func (m *UsePickerModel) View() string {
	if m.width > 0 && m.width < usePickerMinWidth {
		return "Terminal too narrow — try `canopy use --list` for a tabular view.\n"
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("canopy use"))
	b.WriteString("\n\n")
	if m.activeSymlinkText != "" {
		b.WriteString("  ")
		b.WriteString(subtleStyle.Render(m.activeSymlinkText))
		b.WriteString("\n\n")
	}

	// Column widths computed per render from the row data so columns
	// stay aligned regardless of how the rows were built. lipgloss
	// Width() handles unicode (e.g., "—" is 1 cell wide but 3 bytes).
	targetW := lipgloss.Width("TARGET")
	branchW := lipgloss.Width("BRANCH")
	versionW := lipgloss.Width("VERSION")
	for _, r := range m.rows {
		if w := lipgloss.Width(r.Target); w > targetW {
			targetW = w
		}
		if w := lipgloss.Width(r.Branch); w > branchW {
			branchW = w
		}
		if w := lipgloss.Width(r.Version); w > versionW {
			versionW = w
		}
	}

	for i, r := range m.rows {
		marker := "  "
		if r.Active {
			marker = useActiveStyle.Render("▶ ")
		}
		line := marker + padRight(r.Target, targetW) + "  " +
			padRight(r.Branch, branchW) + "  " +
			padRight(r.Version, versionW) + "  " +
			r.Built
		switch {
		case i == m.cursor:
			line = selectedStyle.Render(line)
		case !r.HasBinary:
			line = subtleStyle.Render(line)
		}
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}

	if m.hint != "" {
		b.WriteString("\n  ")
		b.WriteString(subtleStyle.Render(m.hint))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(
		"  ↑/↓ select • enter switch • b build then switch • q cancel"))
	b.WriteString("\n")
	return b.String()
}

// padRight pads s on the right with spaces to visual width w.
// lipgloss.Width handles unicode + ANSI escapes so columns stay
// aligned for em-dashes and styled fragments.
func padRight(s string, w int) string {
	actual := lipgloss.Width(s)
	if actual >= w {
		return s
	}
	return s + strings.Repeat(" ", w-actual)
}

// Compile-time check.
var _ tea.Model = (*UsePickerModel)(nil)
