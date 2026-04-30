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

	"github.com/oncactus/canopy/internal/state"
)

// Options is the parent's wiring for a projectlist Model. All callbacks
// are optional; supplying nil makes the corresponding key a no-op.
type Options struct {
	// OnActivate fires when the user presses enter on a row. The parent
	// returns a tea.Cmd (typically tea.ExecProcess for a tmux attach, or
	// a status-line update for a non-attachable row). nil is allowed.
	//
	// Hints (rename / shipped / pr_status) decorate the row visually
	// but do NOT change enter's dispatch — attach is always the
	// behavior. Close-out is left as a manual `canopy rm` step so the
	// user retains full control over destructive actions.
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

// UpdateRowHints replaces the Hints slice for the row matching
// (project, name). Used by the parent's two-phase refresh: rows render
// immediately with no badges, then per-row lifecycle detector results
// arrive as separate messages and merge in via this method.
//
// Identifier is (project, name) rather than slice index because rows
// can be reordered or removed between dispatch and arrival (e.g. a
// concurrent reconcile drops an orphan workspace mid-flight). Returns
// silently if no matching row exists — late-arriving hints for a now-
// gone workspace are dropped on the floor.
func (m *Model) UpdateRowHints(project, name string, hints []state.Hint) {
	for i := range m.rows {
		if m.rows[i].Project == project && m.rows[i].Name == name {
			m.rows[i].Hints = hints
			return
		}
	}
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
	// branchIcon prefixes every Branch cell. `⎇` is U+2387 (alternative
	// key symbol) — renders as a small fork on terminals without nerd
	// fonts, no font dependency. Single visible cell so column math is
	// unaffected; the trailing space gives a gap before the branch text.
	const branchIcon = "⎇ "

	colName, colBranch, colStatus, colPort, colMem := 4, 6, 6, 4, 4
	for _, r := range m.rows {
		colName = maxInt(colName, len(r.Name))
		colBranch = maxInt(colBranch, len(r.Branch))
		colStatus = maxInt(colStatus, len(string(r.Status)))
		if r.Port > 0 {
			colPort = maxInt(colPort, lenInt(r.Port))
		}
		colMem = maxInt(colMem, len(memCell(r)))
	}

	// Recompute colStatus to account for IsMain rows showing "running"
	// or "not started" instead of the bare "main" status — those labels
	// can be longer than the original column width.
	for _, r := range m.rows {
		colStatus = maxInt(colStatus, len(displayStatus(r)))
	}

	var b strings.Builder
	prevProject := ""

	for i, r := range m.rows {
		// New project group: blank separator + flush-left header. Header
		// sits at column 0 so it visually outdents from the rows below
		// (which start at column 2 = caret + space) — gives the eye a
		// clear "section / contents" hierarchy.
		if r.Project != prevProject {
			if prevProject != "" {
				b.WriteString("\n")
			}
			b.WriteString(projectHeaderStyle().Render(r.Project))
			b.WriteString("\n")
			prevProject = r.Project
		}

		port := "—"
		if r.Port > 0 {
			port = fmt.Sprintf("%d", r.Port)
		}
		isSelected := i == m.cursor
		statusText := displayStatus(r)

		var line string
		// presenceGlyph: 2-char slot before the name encoding session
		// presence in three states:
		//   ⊙   tmux alive AND a client is attached — "you're looking
		//       at it." Rendered green so it pops at a scan.
		//   ○   tmux alive, no client attached — "running quietly in
		//       the background." Subtle grey.
		//   (blank) no tmux session — the status column says why
		//       (stopped, broken, orphaned, not started).
		// The three-state encoding distinguishes "running quietly"
		// from "doesn't exist," which a single attached/blank glyph
		// conflated.
		presenceGlyph := "  "
		switch {
		case r.Alive && r.Attached:
			presenceGlyph = "⊙ "
		case r.Alive:
			presenceGlyph = "○ "
		}

		if isSelected {
			// Selected row: `❯ ` caret + plain content (no inner ANSI)
			// wrapped with the selection bg padded to terminal width.
			// Non-selected rows pad with two spaces in the caret slot
			// so columns stay put as the cursor moves.
			plainContent := fmt.Sprintf("❯ %s%-*s  %s%-*s  %s%-*s  %*s  %*s",
				presenceGlyph,
				colName, r.Name,
				branchIcon,
				colBranch, r.Branch,
				displayGlyph(r)+" ",
				colStatus, statusText,
				colPort, port,
				colMem, memCell(r),
			)
			if hintBadges := RenderHintBadges(r.Hints); hintBadges != "" {
				plainContent += "  " + stripAnsi(hintBadges)
			}

			rowStyle := selectionStyle()
			if m.width > 0 {
				rowStyle = rowStyle.Width(m.width)
			}
			line = rowStyle.Render(plainContent)
		} else {
			// Non-selected row: full per-column styling for visual
			// density. Status color reflects lifecycle state (and for
			// IsMain rows, alive vs not-started); branch styling tags
			// renamed branches in bold-white.
			statusCell := mainAwareStatusStyle(r).Render(displayGlyph(r) + " " + fmt.Sprintf("%-*s", colStatus, statusText))
			// Branch cell: icon + padded text, styled together so the
			// glyph picks up the same color as the name. Three styling
			// modes:
			//   - main row     → gray (the project's default branch is
			//                    informational, not actionable here)
			//   - Branch==Name → gray (auto-generated namegen, unrenamed)
			//   - else         → bold-white (user/agent renamed it on
			//                    purpose; this is the "I named this"
			//                    visual cue)
			branchDisplay := branchIcon + fmt.Sprintf("%-*s", colBranch, r.Branch)
			switch {
			case r.IsMain:
				branchDisplay = subtleHelper().Render(branchDisplay)
			case r.Branch == r.Name:
				branchDisplay = subtleHelper().Render(branchDisplay)
			case r.Branch != "" && r.Branch != "—":
				branchDisplay = renamedBranchStyle().Render(branchDisplay)
			}
			// Presence glyph color: green for attached (the eye-catcher
			// — "you're in this one"), subtle grey for detached-alive
			// (background presence), blank for not-running (status
			// column carries the meaning).
			var styledPresence string
			switch {
			case r.Alive && r.Attached:
				styledPresence = lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render(presenceGlyph)
			case r.Alive:
				styledPresence = subtleHelper().Render(presenceGlyph)
			default:
				styledPresence = presenceGlyph // blank, no styling needed
			}
			// Two-space caret slot (matches the `❯ ` width of the
			// selected branch) so everything after stays put when the
			// cursor moves between rows.
			line = fmt.Sprintf("  %s%-*s  %s  %s  %*s  %s",
				styledPresence,
				colName, r.Name,
				branchDisplay,
				statusCell,
				colPort, port,
				memCellStyled(r, colMem),
			)
			if hintBadges := RenderHintBadges(r.Hints); hintBadges != "" {
				line += "  " + hintBadges
			}
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// displayStatus returns the user-facing status string for a row.
//
// Three layers of mapping:
//
//  1. Main rows promote Alive into the status itself: "running" when
//     the main session is up, "not started" otherwise.
//  2. Workspace rows whose recorded Status is "ready" are displayed
//     as "running" (matches the main-row vocabulary; "ready" was
//     ambiguous between "ready to attach" and "actively running").
//  3. Stale-ready override: a workspace whose Status says ready but
//     whose freshly-probed Alive is false (tmux died out-of-band,
//     Reconcile hasn't caught up) is displayed as "stopped" so the
//     column doesn't lie. The Enter handler also routes these rows
//     through resurrect via effectiveStatus — same idea, same data.
//  4. setting_up renders with a space ("setting up") rather than the
//     internal underscore form for readability.
//
// Other workspace statuses (stopped, broken, orphaned) display as-is.
func displayStatus(r state.GlobalRow) string {
	if r.IsMain {
		if r.Alive {
			return "running"
		}
		return "not started"
	}
	switch r.Status {
	case state.StatusReady:
		if !r.Alive {
			return "stopped"
		}
		return "running"
	case state.StatusSettingUp:
		return "setting up"
	}
	return string(r.Status)
}

// mainAwareStatusStyle picks the right color for the status cell.
//
//   - Main rows: green when alive ("running"), gray when not started.
//   - Workspace rows with status=ready but Alive=false: amber, matching
//     the stopped-workspace color since displayStatus also renders them
//     as "stopped" (stale-ready override). Without this, a stale row
//     shows the green "running" color even though the text says
//     "stopped" — visual inconsistency.
//   - Other workspace rows: statusStyleFor(Status).
func mainAwareStatusStyle(r state.GlobalRow) lipgloss.Style {
	if r.IsMain {
		if r.Alive {
			return lipgloss.NewStyle().Foreground(lipgloss.Color("46"))
		}
		return subtleHelper()
	}
	if r.Status == state.StatusReady && !r.Alive {
		return statusStyleFor(state.StatusStopped)
	}
	return statusStyleFor(r.Status)
}

// stripAnsi removes ANSI SGR escape sequences from s. Used to flatten
// pre-styled fragments (RenderHintBadges output) before embedding them
// inside selectionStyle, where inner ANSI codes would break the outer
// bg propagation. Tiny inline implementation rather than a regexp —
// canopy emits only SGR (\x1b[...m), no cursor-movement etc.
func stripAnsi(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if c == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// RenderHintBadges produces the short-form badge text appended to a row
// when detector hints are active. Returns "" when no hints — the caller
// appends nothing in that case, keeping rows visually identical to v0.5
// for workspaces without active lifecycle signals.
//
// Exported so the project-mode TUI (internal/ui/view.go) can render the
// same badges next to its own table rows. Both surfaces should produce
// identical badge output for the same hints.
//
// Badge precedence: PR status wins over the local "shipped" detector
// when both fire for the same workspace. The local-shipped signal only
// proves "branch is in main" — for repos with PRs that's "merged",
// which is one step before "shipped." PR state is the authoritative
// signal; the shipped badge serves as the local-only fallback for
// purely-local workflows where no PR exists.
//
// Badge mapping (kept short to fit on a single row):
//
//	rename_suggested  →  ↻ rename
//	pr_status (open)             →  PR open
//	pr_status (approved)         →  PR approved
//	pr_status (changes-requested)→  PR changes
//	pr_status (merged)           →  ✓ PR merged
//	pr_status (closed)           →  PR closed
//	shipped (no PR)              →  ✓ shipped (local)
//
// Multiple hints surface as space-separated badges; order is rename →
// pr_status / shipped so the "what next action" badge stays rightmost.
func RenderHintBadges(hints []state.Hint) string {
	if len(hints) == 0 {
		return ""
	}
	hasPR := false
	for _, h := range hints {
		if h.Kind == "pr_status" {
			hasPR = true
			break
		}
	}
	parts := make([]string, 0, len(hints))
	// Render rename first so it sits left of the lifecycle-state badge.
	for _, h := range hints {
		if h.Kind == "rename_suggested" {
			parts = append(parts, hintRenameStyle().Render("↻ rename"))
		}
	}
	// Then git stats — show always (regardless of PR/merged state) so
	// the user sees in-flight commit counts + dirty file count at a
	// glance. Detector returns nil when all three counts are zero, so
	// clean workspaces don't get a noisy "↑0 ↓0 *0" badge.
	for _, h := range hints {
		if h.Kind == "git_stats" && h.Message != "" {
			parts = append(parts, gitStatsStyle().Render(h.Message))
		}
	}
	// Then the lifecycle-state badge: PR if present, shipped fallback
	// otherwise.
	for _, h := range hints {
		switch h.Kind {
		case "pr_status":
			parts = append(parts, prBadge(h.Message))
		case "shipped":
			if hasPR {
				continue // PR state wins; suppress the local fallback
			}
			// "✓ merged" reflects what we actually detect: the
			// branch's commits are now in the default branch via
			// squash-merge. Action is `canopy rm`.
			parts = append(parts, hintShippedStyle().Render("✓ merged"))
		}
	}
	return strings.Join(parts, " ")
}

// gitStatsStyle returns the lipgloss style for the git-stats badge.
// Subtle grey — the badge is informational chrome, not an actionable
// alert. The arrows + count communicate everything; loud color would
// over-claim attention against the actionable rename/PR badges.
func gitStatsStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
}

// prBadge formats the pr_status hint into a short, color-coded label.
// State is parsed from the hint's message (the detector controls both
// the format and the renderer, so keyword matching is a closed system).
//
// Colors:
//   - merged       → green bold (the "done" state)
//   - approved     → cyan       (positive but not yet merged)
//   - open         → cyan       (informational, no action implied)
//   - changes      → orange     (attention needed)
//   - closed       → gray       (PR didn't ship)
//
// Checks rollup: when the message carries a " · checks <state>" suffix
// (only present on OPEN PRs), append a second badge so the user sees
// CI status at a glance — green for passing, orange for failing, cyan
// for running.
func prBadge(message string) string {
	var pr string
	switch {
	case strings.Contains(message, "merged"):
		pr = hintShippedStyle().Render("✓ PR merged")
	case strings.Contains(message, "approved"):
		pr = hintPRStyle().Render("PR approved")
	case strings.Contains(message, "changes requested"):
		pr = hintRenameStyle().Render("PR changes")
	case strings.Contains(message, "closed"):
		pr = subtleHelper().Render("PR closed")
	case strings.Contains(message, "open"):
		pr = hintPRStyle().Render("PR open")
	default:
		// Unknown state (forward-compat for new gh review states):
		// fall back to a generic badge.
		pr = hintPRStyle().Render("PR")
	}

	switch {
	case strings.Contains(message, "checks failing"):
		return pr + " " + hintRenameStyle().Render("✗ checks")
	case strings.Contains(message, "checks running"):
		return pr + " " + hintPRStyle().Render("… checks")
	case strings.Contains(message, "checks passing"):
		return pr + " " + hintShippedStyle().Render("✓ checks")
	}
	return pr
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

// projectHeaderStyle is the project-name banner above each group. Bold +
// pale-violet so the name stands out as a section header without competing
// with the alive/dead badges below it.
func projectHeaderStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("99")).
		Bold(true)
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
	// Soft dark-grey bg + bright white fg + bold. Lighter than the
	// v0.7 bright-violet bg (62) which was too punchy. Bright white
	// (231) for the fg so the inline `>` caret + row content read
	// boldly against the grey bg — the eye snaps to the row without
	// the bg color shouting.
	return lipgloss.NewStyle().
		Background(lipgloss.Color("237")).
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

// statusGlyphFor returns a 1-rune shape prefix for a raw Status enum
// value. Provides a non-color signal so status reads correctly under
// protanopia and on monochrome terminals. Mirrors render.statusGlyph
// in the parent ui pkg — duplicated rather than imported because
// projectlist intentionally stands alone for reuse from `canopy ls
// --all` and other future surfaces.
func statusGlyphFor(s state.Status) string {
	switch s {
	case state.StatusSettingUp:
		return "…"
	case state.StatusStopped:
		return "⏸"
	case state.StatusBroken:
		return "✗"
	case state.StatusOrphaned:
		return "!"
	}
	return " "
}

// displayGlyph is the row-aware version of statusGlyphFor. The glyph
// must agree with displayStatus's text — otherwise stale-ready rows
// render as " stopped" (ready glyph + stopped text), which the user
// rightly called out as inconsistent. Always go through this helper
// from row-rendering code; the bare statusGlyphFor is for legends
// and other contexts where we want the literal status mapping.
func displayGlyph(r state.GlobalRow) string {
	if r.IsMain {
		return " "
	}
	// Stale-ready: status says ready but freshly-probed Alive is false.
	// Match the displayStatus override so glyph + text agree.
	if r.Status == state.StatusReady && !r.Alive {
		return statusGlyphFor(state.StatusStopped)
	}
	return statusGlyphFor(r.Status)
}

// memCell returns the human-readable load text for a row: "320M 12%"
// shape combining MemRSS and CPU in one cell. Returns "—" when there's
// no meaningful data (main rows where the probe didn't run yet, dead
// sessions, probe-failed rows).
//
// CPU is rendered as an integer percentage (sum of pcpu across panes,
// can exceed 100 on multi-core boxes — that's fine, it's signal that
// the workspace is hammering multiple cores). Always shown when mem
// is shown so the cell format stays consistent across rows; an idle
// workspace renders "320M 0%" rather than collapsing to "320M". The
// "0%" carries the "genuinely idle" signal and avoids the ambiguity
// of "is CPU missing because <1% or because we don't have data?"
func memCell(r state.GlobalRow) string {
	if r.IsMain || !r.Alive || r.MemRSS <= 0 {
		return "—"
	}
	return fmt.Sprintf("%s %d%%", humanRSS(r.MemRSS), int(r.CPU+0.5))
}

// memCellStyled returns memCell padded to width with a heat-aware
// color: subtle when low, amber when medium (>500MB), red when high
// (>2GB). Heaviness draws the eye so the user can scan a long list
// and immediately spot the workspace eating their machine. CPU
// participates in the heat threshold by getting the same color as the
// RSS-driven decision — keeps the cell as one visual unit.
func memCellStyled(r state.GlobalRow, width int) string {
	cell := memCell(r)
	padded := fmt.Sprintf("%*s", width, cell)
	if cell == "—" {
		return subtleHelper().Render(padded)
	}
	switch {
	case r.MemRSS >= 2*1024*1024*1024 || r.CPU >= 200:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(padded) // red
	case r.MemRSS >= 500*1024*1024 || r.CPU >= 50:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(padded) // amber
	default:
		return subtleHelper().Render(padded)
	}
}

// humanRSS formats bytes as a 4-character compact string (e.g. " 12M",
// "320M", "1.2G"). Optimized for column scanning, not exact
// reporting.
func humanRSS(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1fG", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%dM", n/mb)
	case n >= kb:
		return fmt.Sprintf("%dK", n/kb)
	default:
		return fmt.Sprintf("%dB", n)
	}
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
