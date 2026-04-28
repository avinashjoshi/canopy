// Package projectlist is a reusable Bubbletea sub-component that renders
// canopy's cross-project workspace listing: rows of (project, workspace,
// branch, status, port, session) grouped by project, with a cursor for
// navigation and dispatch on enter.
//
// "Sub-component" is the operative word — Model implements tea.Model so
// it can be the top-level program when run alone, but it deliberately
// owns NO chrome (no title, no help line, no quit handling). Parent
// Models embed it, decide what enter and r mean via the OnActivate /
// OnRefresh callbacks, and surround it with their own viewport.
//
// Two consumers today:
//
//  1. internal/ui/model_global.go (GlobalModel): top-level TUI when
//     `canopy` is invoked from outside a project. GlobalModel adds the
//     title, help line, ? overlay, and q/ctrl+c quit handling.
//
//  2. The future v1 in-session overlay (TODOS.md): a popup inside an
//     attached tmux session. Will embed Model as a small bordered
//     viewport, add its own activate handler (e.g. tmux switch-client
//     to the chosen session), and gate cursor focus.
//
// Adding a third surface? Just embed Model and inject your callbacks.
// Don't re-render the table elsewhere — that's the whole reason this
// package exists.

package projectlist

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/avinashjoshi/canopy/internal/state"
)

// Options is the parent's wiring for a projectlist Model. All callbacks
// are optional; supplying nil makes the corresponding key a no-op.
type Options struct {
	// OnActivate fires when the user presses enter on a row. The parent
	// returns a tea.Cmd (typically tea.ExecProcess for a tmux attach,
	// or a status-line update for a non-attachable row). nil is allowed.
	OnActivate func(state.GlobalRow) tea.Cmd

	// OnGoToProject fires when the user presses 'o' (for "open project").
	// The parent typically tea.ExecProcess's `canopy` with cwd set to the
	// row's ProjectRoot — that way the user lands in the project's TUI
	// without having to leave the global view first.
	// nil is allowed; key becomes a no-op.
	OnGoToProject func(state.GlobalRow) tea.Cmd

	// OnRefresh fires when the user presses r. Parent returns a tea.Cmd
	// that re-fetches state and pushes new rows via SetRows. nil is allowed.
	OnRefresh func() tea.Cmd
}

// Model is the projectlist sub-component. Implements tea.Model so it can
// be used standalone (Init/Update/View shape), but designed to compose
// into a parent Model's lifecycle.
//
// Parent contract:
//   - Construct via New(opts).
//   - Push fresh data via SetRows whenever the parent's refresh resolves.
//   - Call SetSize on tea.WindowSizeMsg from the parent's Update.
//   - Forward every other message into Model.Update; integrate the
//     returned (Model, tea.Cmd) back into your own state.
type Model struct {
	rows   []state.GlobalRow
	cursor int

	width, height int

	// err is rendered as a one-line banner above the table when non-nil.
	// Parent sets it via SetError to surface (typically) the result of
	// an OnActivate that didn't dispatch — e.g. "workspace 'foo' is
	// stopped — cd into <project> to resurrect."
	err error

	onActivate    func(state.GlobalRow) tea.Cmd
	onGoToProject func(state.GlobalRow) tea.Cmd
	onRefresh     func() tea.Cmd
}

// New constructs a Model with no rows. The parent typically follows up
// immediately with SetRows once its first state load resolves.
func New(opts Options) Model {
	return Model{
		onActivate:    opts.OnActivate,
		onGoToProject: opts.OnGoToProject,
		onRefresh:     opts.OnRefresh,
	}
}

// Init implements tea.Model. The sub-component has no startup work — the
// parent owns refresh dispatch.
func (m Model) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model. Handles only the keys that belong to the
// list (nav + activate + refresh) and bubbles everything else back to
// the parent unchanged. The cursor is clamped on every key event so a
// mid-stream SetRows that shrinks the row list doesn't strand the cursor
// past the last row.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
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

		case "enter":
			if len(m.rows) == 0 || m.onActivate == nil {
				return m, nil
			}
			return m, m.onActivate(m.rows[m.cursor])

		case "o":
			// "Open project" — let the parent cd into the project's repo
			// and re-launch canopy there. Works on any row regardless of
			// status, so users have an escape hatch from broken/stopped
			// rows that would otherwise just show a hint.
			if len(m.rows) == 0 || m.onGoToProject == nil {
				return m, nil
			}
			return m, m.onGoToProject(m.rows[m.cursor])

		case "r":
			if m.onRefresh == nil {
				return m, nil
			}
			return m, m.onRefresh()
		}
	}
	return m, nil
}

// SetRows replaces the row data and clamps the cursor. Idempotent; safe
// to call from inside the parent's Update on every refresh result.
func (m *Model) SetRows(rows []state.GlobalRow) {
	m.rows = rows
	if m.cursor >= len(m.rows) {
		m.cursor = max0(len(m.rows) - 1)
	}
}

// Rows returns the current row slice (for parent inspection — e.g. when
// the parent needs to know whether the list is empty to decide what to
// render around the projectlist).
func (m Model) Rows() []state.GlobalRow {
	return m.rows
}

// CursorRow returns the row currently under the cursor, or zero value +
// false if the list is empty. Parents that need to act on selection
// outside an enter keystroke (e.g. a 'd' delete from the parent's keymap)
// use this.
func (m Model) CursorRow() (state.GlobalRow, bool) {
	if len(m.rows) == 0 {
		return state.GlobalRow{}, false
	}
	return m.rows[m.cursor], true
}

// SetSize updates the rendering envelope. Parent forwards this from its
// own tea.WindowSizeMsg handler (the parent is responsible for figuring
// out how much vertical space it wants to give the list after subtracting
// title + help line).
func (m *Model) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// SetError sets the one-line banner shown above the table. Pass nil to
// clear. Typically set by the parent's onActivate callback when the
// chosen row isn't actionable (stopped/broken/orphaned in global mode).
func (m *Model) SetError(err error) {
	m.err = err
}

// View implements tea.Model. Renders the table only — no title, no help
// line. Parents wrap with their own chrome.
//
// Empty list: returns the parent-friendly empty-state copy. Parents that
// want different empty-state behavior can check Rows() before delegating
// to View.
func (m Model) View() string {
	var b strings.Builder
	if m.err != nil {
		b.WriteString(errorBanner(m.err.Error()))
		b.WriteString("\n\n")
	}
	if len(m.rows) == 0 {
		b.WriteString(emptyState())
		return b.String()
	}
	b.WriteString(m.renderTable())
	return b.String()
}

// renderTable groups rows by project, emits a project-name header line for
// each group, then indents the workspace rows underneath. The cursor still
// moves linearly over rows (no skipping); headers are visual chrome only.
//
// Layout:
//
//	canopy
//	  ●  (main)          —              main    40000
//	  ●  ancient-hornet  ancient-hornet  ready  40010
//
//	cravd
//	  ○  misty-aspen     misty-aspen     broken 41010
//
// Why grouped instead of flat-with-PROJECT-column: with basename uniqueness
// invariant (every project basename is unique in v0.5+), repeating the
// project name on every row was wasted ink. Headers + indentation read
// faster and free up horizontal space for the actual workspace identity
// (NAME, BRANCH).
//
// Column widths are derived once from the longest entries; lipgloss
// doesn't help with column alignment when rows have styled cells of
// different visible widths, so we compute widths ourselves.
func (m Model) renderTable() string {
	colName, colBranch, colStatus, colPort := 4, 6, 6, 4
	for _, r := range m.rows {
		colName = maxInt(colName, len(r.Name))
		colBranch = maxInt(colBranch, len(r.Branch))
		colStatus = maxInt(colStatus, len(string(r.Status)))
		if r.Port > 0 {
			colPort = maxInt(colPort, lenInt(r.Port))
		}
	}

	var b strings.Builder
	prevProject := ""

	for i, r := range m.rows {
		// New project group: emit a blank separator + project header line.
		// First group skips the blank line so the table flush-aligns under
		// the title.
		if r.Project != prevProject {
			if prevProject != "" {
				b.WriteString("\n")
			}
			b.WriteString("  ")
			b.WriteString(projectHeaderStyle().Render(r.Project))
			b.WriteString("\n")
			prevProject = r.Project
		}

		port := "—"
		if r.Port > 0 {
			port = fmt.Sprintf("%d", r.Port)
		}
		badge := badgeFor(r.Alive)
		statusCell := statusStyleFor(r.Status).Render(fmt.Sprintf("%-*s", colStatus, r.Status))
		// Two-space indent under the project header so rows visually nest.
		line := fmt.Sprintf("    %s  %-*s  %-*s  %s  %*s",
			badge,
			colName, r.Name,
			colBranch, r.Branch,
			statusCell,
			colPort, port,
		)
		if i == m.cursor {
			line = selectionStyle().Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// emptyState is the table-area copy when there are no rows. Parents
// (notably GlobalModel) may override this by checking Rows() before
// rendering and substituting a richer empty state, but the default
// keeps the sub-component renderable in isolation.
func emptyState() string {
	return subtleHelper().Render(
		"No canopy projects yet.\n\n" +
			"  cd into a git repo and run `canopy init` to onboard your\n" +
			"  first project. Then `canopy` from anywhere lists everything.")
}

// lenInt returns the decimal-digit length of n. Used for port column
// width computation. n is assumed positive (caller checks > 0 before
// using).
func lenInt(n int) int {
	if n == 0 {
		return 1
	}
	count := 0
	for n > 0 {
		count++
		n /= 10
	}
	return count
}

// max0 returns max(0, n). Used to clamp the cursor without dropping
// below zero on an empty list.
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- style helpers ---
//
// We reach into internal/ui's render.go for the canonical styles. Doing
// it via tiny accessor functions in this file (rather than a direct
// import) keeps projectlist a leaf-ish package that doesn't pull in
// internal/ui's Bubbletea program plumbing — only the styling.
//
// Implementation detail: these functions just construct the same
// lipgloss styles inline so projectlist doesn't have to import its
// parent (which would create a cycle since internal/ui imports
// projectlist).

func headerStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
}

// projectHeaderStyle is the project-name banner above each group. Bold +
// pale-violet so the name stands out as a section header without competing
// with the alive/dead badges below it.
func projectHeaderStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("99")).
		Bold(true)
}

func subtleHelper() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
}

func selectionStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Background(lipgloss.Color("237")).
		Foreground(lipgloss.Color("230")).
		Bold(true)
}

func errorBanner(msg string) string {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("196")).
		Padding(0, 1).
		Render("error: " + msg)
}

func badgeFor(alive bool) string {
	if alive {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render("●")
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("○")
}

func statusStyleFor(s state.Status) lipgloss.Style {
	switch s {
	case state.StatusReady:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("46"))
	case state.StatusStopped:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	case state.StatusBroken:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	case state.StatusOrphaned:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	case state.StatusSettingUp:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	case "main":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("99"))
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
}
