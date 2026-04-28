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
	// OnActivate fires when the user presses enter on a row WITHOUT an
	// active "shipped" hint. The parent returns a tea.Cmd (typically
	// tea.ExecProcess for a tmux attach, or a status-line update for a
	// non-attachable row). nil is allowed.
	//
	// Special case: when the row's hints include kind="shipped", enter
	// fires OnCloseOut instead (the v0.6 close-out flow). OnActivate
	// is reserved for "this is in flight, attach" semantics.
	OnActivate func(state.GlobalRow) tea.Cmd

	// OnCloseOut fires when the user presses enter on a row whose hints
	// include kind="shipped". The parent typically prompts confirm-close
	// and runs `canopy rm <name>` on yes. nil is allowed; falls back to
	// OnActivate (which usually attaches — same as v0.5 behavior).
	OnCloseOut func(state.GlobalRow) tea.Cmd

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
	onCloseOut    func(state.GlobalRow) tea.Cmd
	onGoToProject func(state.GlobalRow) tea.Cmd
	onRefresh     func() tea.Cmd
}

// New constructs a Model with no rows. The parent typically follows up
// immediately with SetRows once its first state load resolves.
func New(opts Options) Model {
	return Model{
		onActivate:    opts.OnActivate,
		onCloseOut:    opts.OnCloseOut,
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
			if len(m.rows) == 0 {
				return m, nil
			}
			row := m.rows[m.cursor]
			// v0.6 close-out flow: a row with an active "shipped" hint
			// (the branch is reachable from origin/main) routes enter
			// through OnCloseOut instead of OnActivate. The parent
			// prompts "Close out '<branch>'? [y/N]" and runs canopy rm
			// on yes. Falls back to OnActivate when OnCloseOut isn't
			// wired (preserves v0.5 attach behavior for parents that
			// haven't adopted the close-out flow yet).
			if hasShippedHint(row.Hints) && m.onCloseOut != nil {
				return m, m.onCloseOut(row)
			}
			if m.onActivate == nil {
				return m, nil
			}
			return m, m.onActivate(row)

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
		// Branch is emphasized (the renamed-intent name) when it differs
		// from the workspace name. When they match (fresh workspace,
		// branch hasn't been renamed yet), the branch column dims so
		// the duplicate doesn't visually shout. Subtle distinction —
		// the user sees at a glance which workspaces have been "named"
		// vs which still wear their auto-generated label.
		branchDisplay := fmt.Sprintf("%-*s", colBranch, r.Branch)
		if r.Branch == r.Name {
			branchDisplay = subtleHelper().Render(branchDisplay)
		} else if r.Branch != "" && r.Branch != "—" {
			branchDisplay = renamedBranchStyle().Render(branchDisplay)
		}
		line := fmt.Sprintf("    %s  %-*s  %s  %s  %*s",
			badge,
			colName, r.Name,
			branchDisplay,
			statusCell,
			colPort, port,
		)
		// Append v0.6 hint badges (rename / shipped / pr_status) to the
		// right of the row. Subtle styling so they decorate without
		// stealing focus from the workspace's primary fields. Badges
		// only appear when the corresponding detector fired; rows with
		// no active hints render exactly as before.
		if hintBadges := renderHintBadges(r.Hints); hintBadges != "" {
			line += "  " + hintBadges
		}
		if i == m.cursor {
			line = selectionStyle().Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// renderHintBadges produces the short-form badge text appended to a row
// when v0.6 detector hints are active. Returns "" when no hints — the
// caller appends nothing in that case, keeping rows visually identical
// to v0.5 for workspaces without active lifecycle signals.
//
// Badge mapping (kept short to fit on a single row):
//
//	rename_suggested  →  ↻ rename
//	shipped           →  ✓ shipped
//	pr_status         →  PR (or PR ✓ when the message indicates merged)
//
// Multiple hints surface as space-separated badges. Order matches the
// hints slice order (whatever the detector framework returned).
func renderHintBadges(hints []state.Hint) string {
	if len(hints) == 0 {
		return ""
	}
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		switch h.Kind {
		case "rename_suggested":
			parts = append(parts, hintRenameStyle().Render("↻ rename"))
		case "shipped":
			parts = append(parts, hintShippedStyle().Render("✓ shipped"))
		case "pr_status":
			// Disambiguate by message keywords. Merged PRs render
			// distinctly from open PRs.
			label := "PR"
			if strings.Contains(h.Message, "merged") {
				label = "✓ PR"
			}
			parts = append(parts, hintPRStyle().Render(label))
		default:
			// Unknown hint kind (forward-compat): render the kind name.
			parts = append(parts, subtleHelper().Render(h.Kind))
		}
	}
	return strings.Join(parts, " ")
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

// hasShippedHint returns true when the hint slice contains a
// kind="shipped" entry. Used by the enter-key dispatch to route close-
// out actions instead of attach.
func hasShippedHint(hints []state.Hint) bool {
	for _, h := range hints {
		if h.Kind == "shipped" {
			return true
		}
	}
	return false
}

// hintRenameStyle: amber/orange — "your attention is wanted but it's
// not destructive."
func hintRenameStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
}

// renamedBranchStyle: subtle white/bold — emphasizes the branch when
// it differs from the workspace name (i.e., the agent or user has
// renamed it to reflect feature intent). Visible at a glance which
// workspaces have been "named" vs which still wear their auto-generated
// namegen label.
func renamedBranchStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("253")).Bold(true)
}

// hintShippedStyle: green — "this is good, ready to close out."
func hintShippedStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Bold(true)
}

// hintPRStyle: cyan — informational, not urgent.
func hintPRStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
}

func subtleHelper() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
}

func selectionStyle() lipgloss.Style {
	// Bright violet bg + near-white fg + bold. Mirrors selectedStyle in
	// internal/ui/render.go so the selection looks identical between the
	// project TUI and the global TUI. The previous bg=237 (dim gray)
	// was too subtle on a lot of terminals.
	return lipgloss.NewStyle().
		Background(lipgloss.Color("62")).
		Foreground(lipgloss.Color("231")).
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
