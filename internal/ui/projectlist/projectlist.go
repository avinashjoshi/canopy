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
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/state"
)

// staleThreshold is how long since the host's last successful refresh
// before remote rows from that host render dimmed + the section header
// shows a stale banner. The TUI refresh tick is 2s; 10s = 5 missed
// ticks, well past noise from a single slow tick. Tuned with the
// per-host 3s refresh timeout in mind — a single timeout doesn't
// trigger stale UX, but a sustained outage does. v0.19.
const staleThreshold = 10 * time.Second

// isStale reports whether a remote row's host hasn't been heard from
// recently enough to trust the row's data. Local rows (Host=="") and
// rows from hosts we haven't yet contacted (LastSeen zero) are never
// stale — there's no "last successful refresh" to compare against.
// v0.19.
func isStale(r state.GlobalRow) bool {
	if r.Host == "" {
		return false
	}
	if r.LastSeen.IsZero() {
		return false
	}
	return time.Since(r.LastSeen) > staleThreshold
}

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

	// currentRoot+currentName mark the workspace whose dir hosts the
	// running canopy invocation (popup launched from inside a workspace).
	// Set via SetCurrent. Used by the renderer to decorate that row
	// with a "you are here" marker in addition to the cursor — without
	// it, navigating away from the auto-preselected row loses the
	// "this is where I am" cue.
	currentRoot string
	currentName string

	// agentStates is the per-row agent-state map keyed by tmux session
	// name (project-prefixed: `canopy/add-oauth`). Populated by the
	// parent's SetAgentStates whenever its agent.Detector poll tick
	// fires. Empty map = no badges rendered (initial state before the
	// first tick lands, or workspaces without an agent pane). Keyed
	// by session (not workspace name) to avoid Global-tab collisions
	// across projects.
	agentStates map[string]agent.State

	// agentPolled is true once at least one agent-state poll has
	// landed. Before that, the badge column stays blank for ALL rows
	// (we don't know the layout yet). After it flips true, rows whose
	// session is missing from agentStates are interpreted as "no agent
	// pane in this workspace" and rendered with the No-AI glyph.
	agentPolled bool

	// spinnerFrame indexes the Braille rotation rendered next to
	// Loading=true placeholder rows (registered hosts whose first
	// refresh hasn't returned yet). Parent advances it via
	// SetSpinnerFrame on every hostsSpinnerTickMsg so the animation
	// keeps time with the Hosts tab. v0.22.
	spinnerFrame int

	// loadingHosts is the set of remote-host names whose refresh is
	// in flight. The renderer appends a spinner glyph to the host
	// section header for any host in this set so the workspaces tab
	// signals "we're checking" — visible even when stale rows from a
	// previous refresh are still showing under the header. Parent
	// updates via SetLoadingHosts on every refresh dispatch +
	// completion. v0.22 follow-up to the placeholder-row mechanism so
	// loaders are visible alongside cached rows, not only when the
	// host has no rows yet.
	loadingHosts map[string]bool

	// idleExpanded tracks which hosts have their "idle projects"
	// section unrolled. Default zero-value (nil map) means every host
	// is collapsed — idle projects (no workspaces, main not running,
	// see ClassifyIdle) hide behind a "+N idle projects · e expand"
	// roll-up line at the bottom of each host's section. Pressing `e`
	// while the cursor is in a host toggles its entry here. Toggled
	// state is per-host, not per-process — opening a project tab then
	// returning resets to collapsed; that's intentional, since the
	// noisy idle list is exactly what we're hiding by default.
	idleExpanded map[string]bool
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
				hidden, _ := ClassifyIdle(m.rows, m.idleExpanded)
				if next := nextVisible(hidden, m.cursor-1, -1); !hidden[next] {
					m.cursor = next
				}
			}
			return m, nil

		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				hidden, _ := ClassifyIdle(m.rows, m.idleExpanded)
				if next := nextVisible(hidden, m.cursor+1, 1); !hidden[next] {
					m.cursor = next
				}
			}
			return m, nil

		case "g", "home":
			if len(m.rows) > 0 {
				hidden, _ := ClassifyIdle(m.rows, m.idleExpanded)
				m.cursor = nextVisible(hidden, 0, 1)
			}
			return m, nil

		case "G", "end":
			if len(m.rows) > 0 {
				hidden, _ := ClassifyIdle(m.rows, m.idleExpanded)
				m.cursor = nextVisible(hidden, len(m.rows)-1, -1)
			}
			return m, nil

		case "e":
			// Toggle the current host's idle roll-up. The host the
			// cursor is in owns the toggle so users can independently
			// expand local without unrolling every stale remote at
			// once. No-op when the row list is empty (no host to
			// target). When collapsing leaves the cursor stranded on
			// a now-hidden idle row, advance to the next visible row
			// so the highlight stays meaningful.
			if len(m.rows) == 0 {
				return m, nil
			}
			host := m.rows[m.cursor].Host
			if m.idleExpanded == nil {
				m.idleExpanded = map[string]bool{}
			}
			m.idleExpanded[host] = !m.idleExpanded[host]
			hidden, _ := ClassifyIdle(m.rows, m.idleExpanded)
			if hidden[m.cursor] {
				fwd := nextVisible(hidden, m.cursor, 1)
				if hidden[fwd] {
					fwd = nextVisible(hidden, m.cursor, -1)
				}
				if !hidden[fwd] {
					m.cursor = fwd
				}
			}
			return m, nil

		case "enter":
			if len(m.rows) == 0 || m.onActivate == nil {
				return m, nil
			}
			if m.rows[m.cursor].Loading {
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
			if m.rows[m.cursor].Loading {
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
// When the clamped cursor lands on a hidden idle row (a fresh refresh
// can reshuffle which rows are idle), step forward to the nearest
// visible row so the highlight stays meaningful.
func (m *Model) SetRows(rows []state.GlobalRow) {
	m.rows = rows
	if m.cursor >= len(m.rows) {
		m.cursor = max0(len(m.rows) - 1)
	}
	if len(m.rows) == 0 {
		return
	}
	hidden, _ := ClassifyIdle(m.rows, m.idleExpanded)
	if hidden[m.cursor] {
		fwd := nextVisible(hidden, m.cursor, 1)
		if hidden[fwd] {
			fwd = nextVisible(hidden, m.cursor, -1)
		}
		if !hidden[fwd] {
			m.cursor = fwd
		}
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

// SetCurrent records which (projectRoot, name) is the "you are here"
// row — the workspace whose dir cwd was inside at canopy launch. Empty
// values disable the marker.

// SetAgentStates pushes a fresh agent-state map into the projectlist.
// Map is keyed by tmux session name (e.g. "canopy/add-oauth"); rows
// whose TmuxSession is in the map render the corresponding badge.
//
// Pass polled=true once the FIRST poll tick has actually landed.
// After that point, rows whose session is alive but missing from the
// map are interpreted as "this workspace has no agent pane" (vs the
// pre-first-poll case where we don't know yet).
//
// nil/empty map with polled=true means "we polled, found no agent
// panes anywhere" — every Alive row becomes No-AI badged.
func (m *Model) SetAgentStates(states map[string]agent.State, polled bool) {
	m.agentStates = states
	m.agentPolled = polled
}

// SetSpinnerFrame pushes the current animation frame for Loading
// placeholder rows. The renderer reads it via spinnerGlyph; parent
// advances it on every hostsSpinnerTickMsg while a remote fan-out is
// in flight (and holds it steady otherwise). v0.22.
func (m *Model) SetSpinnerFrame(frame int) {
	m.spinnerFrame = frame
}

// SetLoadingHosts pushes the set of hostnames whose refresh is in
// flight. The renderer appends a spinner glyph next to each matching
// host section header. Pass nil/empty when no remote refresh is
// outstanding so existing headers render without animation.
//
// Distinct from the Loading=true placeholder row mechanism: that one
// inserts synthetic rows for hosts with NO data yet, while this one
// decorates the header for hosts whose existing data is being
// re-checked. Both can fire at once — a host with no rows on first
// launch gets both the header spinner and the placeholder row. v0.22.
func (m *Model) SetLoadingHosts(hosts map[string]bool) {
	m.loadingHosts = hosts
}

// LoadingHosts returns whether the named host is currently flagged as
// loading. Exposed for parent-package tests that drive
// SetLoadingHosts indirectly (via the model's pushLoadingHosts
// helper) and want to assert the wire-up without scraping rendered
// output.
func (m Model) LoadingHosts(name string) bool {
	return m.loadingHosts[name]
}

func (m *Model) SetCurrent(projectRoot, name string) {
	m.currentRoot = projectRoot
	m.currentName = name
}

// SetCursorTo moves the cursor to the first row matching (projectRoot,
// name). Returns true on hit, false if no row matched. Used by the
// unified TUI to pre-select the workspace whose dir the popup was
// launched from (load-bearing UX: opening the popup in workspace X
// should highlight X, not row 0). No-op when projectRoot is empty.
func (m *Model) SetCursorTo(projectRoot, name string) bool {
	if projectRoot == "" || name == "" {
		return false
	}
	for i, r := range m.rows {
		if r.ProjectRoot == projectRoot && r.Name == name {
			m.cursor = i
			return true
		}
	}
	return false
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

// ClassifyIdle decides which rows are "idle" — a project whose only row
// is its synthetic (main) row, where main is not running and not in a
// transitional state (broken/orphaned/setting_up). Idle projects make
// up the bulk of the noise on the global workspaces tab (every project
// canopy knows about contributes one row whether it's interesting or
// not), so the default rendering rolls them up behind a one-line
// "+N idle projects · e expand" hint per host.
//
// Returns:
//
//	hidden       — len(rows)-sized slice; hidden[i]=true means skip
//	               rendering this row in the default (collapsed) view.
//	               Only set when idleExpanded[r.Host] is false; expanding
//	               a host unhides every idle row in it.
//	idleByHost   — count of idle projects per host. The renderer uses
//	               this to size the "+N idle" pill even when the host
//	               is expanded (so the user knows what they unrolled).
//
// Exported so the parent's tests and the renderer share one definition
// of "idle." Loading-placeholder rows (r.Loading=true) are never
// classified idle — those carry a "we're still checking" signal, not
// "nothing's here."
func ClassifyIdle(rows []state.GlobalRow, idleExpanded map[string]bool) (hidden []bool, idleByHost map[string]int) {
	hidden = make([]bool, len(rows))
	idleByHost = map[string]int{}

	// Pass 1: count non-loading rows per (host, project). Projects with
	// >1 row (i.e. main + ≥1 workspace) are never idle — the workspaces
	// themselves are the interesting content even if main is dormant.
	// \x00 as a separator picks a byte that can't appear in either field.
	rowsPerPair := map[string]int{}
	for _, r := range rows {
		if r.Loading {
			continue
		}
		rowsPerPair[r.Host+"\x00"+r.Project]++
	}

	for i, r := range rows {
		if r.Loading || !r.IsMain {
			continue
		}
		if rowsPerPair[r.Host+"\x00"+r.Project] > 1 {
			continue
		}
		if r.Alive {
			continue
		}
		// Only the dormant statuses qualify. Empty status, StatusStopped,
		// and the literal "main" string (stamped by BuildGlobalRows on
		// every synthetic main row) all mean "nothing's running here" —
		// the !Alive check above already excluded running mains.
		// Broken/orphaned/setting_up are attention states — keep them
		// visible.
		if r.Status != "" && r.Status != state.StatusStopped && r.Status != "main" {
			continue
		}
		idleByHost[r.Host]++
		if !idleExpanded[r.Host] {
			hidden[i] = true
		}
	}
	return hidden, idleByHost
}

// nextVisible walks the cursor over hidden idle rows. Starting at from
// (inclusive) and stepping by delta (+1 or -1), returns the first index
// where hidden[i] is false. Returns from unchanged if the walk falls
// off the end without finding a visible row — caller decides what that
// degenerate state means (in practice it can't happen: at least the
// running workspaces stay visible, so the cursor always has somewhere
// to land).
func nextVisible(hidden []bool, from, delta int) int {
	n := len(hidden)
	if n == 0 || delta == 0 {
		return from
	}
	for i := from; i >= 0 && i < n; i += delta {
		if !hidden[i] {
			return i
		}
	}
	return from
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
	table, cursorLine := m.renderTable()
	if m.height > 0 {
		// Subtract two for the err banner ("banner\n\n") when present
		// so the visible window fits inside whatever envelope the
		// parent gave us.
		budget := m.height
		if m.err != nil {
			budget -= 2
		}
		table = clipTableToHeight(table, budget, cursorLine)
	}
	b.WriteString(table)
	return b.String()
}

// clipTableToHeight crops a multi-line table to at most `height` lines
// while keeping `cursorLine` visible. Returns the original input unchanged
// when it already fits or height is non-positive. Reserves one line at
// top/bottom for a dim "↑N more" / "↓N more" scroll indicator when there
// are rows hidden in that direction.
func clipTableToHeight(s string, height int, cursorLine int) string {
	if height <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	// Many renderers (including renderTable) append a trailing newline
	// after the last row, producing a phantom empty final element from
	// strings.Split. Trim it so it doesn't eat a viewport slot.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= height {
		return s
	}
	// Center the cursor in the window when possible; clamp at edges so
	// we never show fewer than `height` lines.
	start := cursorLine - height/2
	if start < 0 {
		start = 0
	}
	if start+height > len(lines) {
		start = len(lines) - height
	}
	end := start + height
	above := start
	below := len(lines) - end

	out := make([]string, 0, height)
	// Replace the first viewport line with an indicator if rows are
	// hidden above. Same idea for the bottom. The indicator REPLACES a
	// content line rather than adding one, so the cropped view's total
	// height stays exactly `height`.
	if above > 0 {
		out = append(out, scrollIndicator(above, true))
		start++
	}
	mid := end
	if below > 0 {
		mid--
	}
	for i := start; i < mid; i++ {
		out = append(out, lines[i])
	}
	if below > 0 {
		out = append(out, scrollIndicator(below, false))
	}
	return strings.Join(out, "\n")
}

func scrollIndicator(n int, up bool) string {
	arrow := "↓"
	if up {
		arrow = "↑"
	}
	// Subtle/dim styling so it reads as chrome, not content.
	return subtleHelper().Render(fmt.Sprintf("  %s %d more", arrow, n))
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
func (m Model) renderTable() (string, int) {
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
	// prevHost starts as a sentinel value that no real Host can equal
	// (real values are either "" for local or a registered remote name)
	// so the first iteration always enters the host-transition block.
	// This is what makes LOCAL get its own pill at the top of the
	// listing when remote hosts are also present — without it the
	// loop sees `r.Host == "" == prevHost` and skips the header.
	const prevHostUninit = "\x00uninit\x00"
	prevHost := prevHostUninit
	prevProject := ""
	// lineCount tracks how many \n-separated lines we've emitted so far.
	// We snapshot it when we emit the cursor row so View() can crop
	// around the cursor and keep it visible. Each WriteString("\n")
	// bumps the count; project + host headers count too.
	lineCount := 0
	cursorLine := 0

	// Detect whether we have ANY non-local rows. If yes, render a
	// "local" host section header above the laptop's own rows for
	// clarity. If everything is local (status quo before v0.17.0),
	// skip the header entirely — it would just be noise.
	hasRemote := false
	for _, r := range m.rows {
		if r.Host != "" {
			hasRemote = true
			break
		}
	}

	// Idle classification + per-host roll-up. Computed once up front
	// because the renderer skips hidden rows in the main loop AND
	// emits a roll-up line at the bottom of each host's section.
	hidden, idleByHost := ClassifyIdle(m.rows, m.idleExpanded)

	// Width-tiered column drop, mirrors internal/ui/hosts.Render's D2
	// policy. The memory cell ("530M 2%") is the first thing to go
	// when the terminal narrows because it's the most superfluous
	// signal — the user can attach to the workspace to see live
	// resource use. m.width == 0 (pre-WindowSizeMsg) treats the
	// terminal as wide so the very first paint doesn't drop columns
	// it'll need a tick later.
	showMem := m.width == 0 || m.width >= 100
	emitIdleRollup := func(host string) {
		n := idleByHost[host]
		if n == 0 {
			return
		}
		noun := "projects"
		if n == 1 {
			noun = "project"
		}
		hint := "e expand"
		if m.idleExpanded[host] {
			hint = "e collapse"
		}
		// Blank line above the roll-up so it doesn't glue to the last
		// project's last workspace row. Cheap visual breath; the host
		// transition / end-of-loop separators around this call still
		// fire, giving the roll-up its own band.
		b.WriteString("\n")
		lineCount++
		line := subtleHelper().Render(
			fmt.Sprintf("  + %d idle %s  ·  %s", n, noun, hint),
		)
		b.WriteString(line)
		b.WriteString("\n")
		lineCount++
	}

	for i, r := range m.rows {
		// New HOST section: blank separator + bolder flush-left header.
		// Renders the host name only when we have at least one remote
		// row in the listing (otherwise the listing looks like it
		// always did pre-v0.17.0).
		if r.Host != prevHost {
			// Flush previous host's idle roll-up before transitioning.
			// Sentinel-guard: at i == 0 the "previous host" is the
			// uninit sentinel, never a real one, so the roll-up call
			// is a no-op (idleByHost[sentinel] is 0).
			emitIdleRollup(prevHost)
			if prevHost != prevHostUninit && (prevHost != "" || hasRemote) {
				b.WriteString("\n")
				lineCount++
			}
			if hasRemote {
				label := r.Host
				if label == "" {
					label = "local"
				}
				header := hostPill(label)
				// v0.22 follow-up: append a spinner glyph for hosts
				// whose refresh is in flight. Visible even when stale
				// rows are still on screen from a previous refresh —
				// the placeholder-row mechanism only fires for hosts
				// with zero rows, so without this the workspaces tab
				// silently hides the "we're checking" signal once the
				// host has any cached data. Local rows (Host=="") and
				// hosts not currently being refreshed render unchanged.
				if r.Host != "" && m.loadingHosts[r.Host] {
					header += "  " + loadingRowStyle().Render(spinnerGlyph(m.spinnerFrame))
				}
				// v0.19 remote-status-observability: when this host's
				// most-recent refresh is older than staleThreshold, append
				// a "⚠ stale Ns" pill so the user knows the rows below
				// are last-known, not live. Local rows (Host=="") never
				// get this treatment.
				if r.Host != "" && !r.LastSeen.IsZero() && time.Since(r.LastSeen) > staleThreshold {
					ago := time.Since(r.LastSeen).Round(time.Second)
					header += "  " + stalePillStyle().Render(fmt.Sprintf("⚠ stale %s", ago))
				}
				b.WriteString(header)
				b.WriteString("\n")
				lineCount++
			}
			prevHost = r.Host
			prevProject = "" // reset so the first project under this host gets a header
		}

		// Idle-row hiding sits between host transition (which emits
		// the host pill above) and the per-row rendering. Skipping an
		// idle row leaves prevProject untouched on purpose — the next
		// visible row still gets its project header, even if that's
		// several rows later. (idle rows are always alone in their
		// project, so this can't desync project-header rendering.)
		if hidden[i] {
			continue
		}

		// Loading placeholder: skip the project-header machinery (these
		// rows carry no project) and emit a single spinner line under
		// the host header instead. Caret slot left blank so the row
		// indents the same as a workspace row. v0.22.
		if r.Loading {
			loadingLine := loadingRowStyle().Render(
				"  " + spinnerGlyph(m.spinnerFrame) + "  loading…",
			)
			if i == m.cursor {
				cursorLine = lineCount
			}
			b.WriteString(loadingLine)
			b.WriteString("\n")
			lineCount++
			// Reset prevProject so the next real row under this host
			// re-emits its project header rather than colliding with
			// the previous host's last project name.
			prevProject = ""
			continue
		}

		// New project group within the current host: blank separator
		// + 2-space-indented header so projects read as subordinate
		// to the host pill above. Workspace rows sit at the same
		// col-2 indent (their caret slot is col 0); the bold-white
		// project name + the row's presence glyph at the same column
		// still tell apart by content, and the indentation cleanly
		// signals "this header belongs under the host."
		if r.Project != prevProject {
			if prevProject != "" {
				b.WriteString("\n")
				lineCount++
			}
			b.WriteString("  " + projectHeaderStyle().Render(r.Project))
			b.WriteString("\n")
			lineCount++
			prevProject = r.Project
		}

		port := "—"
		if r.Port > 0 {
			port = fmt.Sprintf("%d", r.Port)
		}
		isSelected := i == m.cursor
		isCurrent := m.currentRoot != "" && m.currentName != "" &&
			r.ProjectRoot == m.currentRoot && r.Name == m.currentName
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

		badge := agentBadge(r, m.agentStates, m.agentPolled)
		if isSelected {
			// Selected row: `❯ ` caret + plain content (no inner ANSI)
			// wrapped with the selection bg padded to terminal width.
			// Non-selected rows pad with two spaces in the caret slot
			// so columns stay put as the cursor moves.
			plainContent := fmt.Sprintf("❯ %s%s %-*s  %s%-*s  %s%-*s  %*s",
				presenceGlyph,
				stripAnsi(badge),
				colName, r.Name,
				branchIcon,
				colBranch, r.Branch,
				displayGlyph(r)+" ",
				colStatus, statusText,
				colPort, port,
			)
			if showMem {
				plainContent += fmt.Sprintf("  %*s", colMem, memCell(r))
			}
			if hintBadges := RenderHintBadges(r.Hints); hintBadges != "" {
				plainContent += "  " + stripAnsi(hintBadges)
			}
			if isCurrent {
				plainContent += "  ← here"
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
			line = fmt.Sprintf("  %s%s %-*s  %s  %s  %*s",
				styledPresence,
				badge,
				colName, r.Name,
				branchDisplay,
				statusCell,
				colPort, port,
			)
			if showMem {
				line += "  " + memCellStyled(r, colMem)
			}
			if hintBadges := RenderHintBadges(r.Hints); hintBadges != "" {
				line += "  " + hintBadges
			}
			if isCurrent {
				line += "  " + currentMarkerStyle().Render("← here")
			}
		}
		// v0.19 remote-status-observability: dim the entire row when its
		// host hasn't been heard from recently. Faint applies ANSI CSI 2m
		// (low-intensity) which most terminals render as ~50% opacity —
		// signals "this is last-known data" without losing legibility.
		// Skip for the selected row: selectionStyle's bg already commands
		// the eye, and faint over selection-bg is muddier than selection
		// alone. The host header banner still shows "⚠ stale Ns" so the
		// signal isn't lost on the cursor's row.
		if isStale(r) && i != m.cursor {
			line = staleRowStyle().Render(line)
		}
		b.WriteString(line)
		if i == m.cursor {
			cursorLine = lineCount
		}
		b.WriteString("\n")
		lineCount++
	}
	// Flush the final host's idle roll-up. The in-loop emitter only
	// fires on host transitions, so the last host's section never gets
	// its roll-up otherwise.
	emitIdleRollup(prevHost)
	return b.String(), cursorLine
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
//	stuck_state                  →  ⚠ rebasing / ⚠ merging / ⚠ pick / ⚠ detached
//	rename_suggested             →  ↻ rename
//	mergeability                 →  ⚠ conflict        (orange, attention)
//	push_state                   →  ⇡N / ⇅            (orange, "remote is out of sync")
//	pr_status (open)             →  PR open
//	pr_status (approved)         →  PR approved
//	pr_status (changes-requested)→  PR changes
//	pr_status (merged)           →  ✓ PR merged
//	pr_status (closed)           →  PR closed
//	shipped (no PR)              →  ✓ shipped (local)
//
// Multiple hints surface as space-separated badges. Order is:
// stuck_state → rename → mergeability → git_stats → push_state →
// pr_status / shipped. stuck_state sits leftmost because mid-rebase /
// mid-merge / detached states are easy to forget across parallel
// workspaces and become the most actionable thing the user can do —
// finish the in-flight git op first. Mergeability sits left of
// git_stats so the "this won't merge clean" warning reads next to the
// divergence counts that explain why. push_state sits right of
// git_stats so the "is my work on origin?" answer reads next to the
// "ahead of main" count — different axes (origin/<branch> vs
// origin/<default>) but related divergence story.
//
// Precedence rule (v0.14 closeout): when stuck_state fires, git_stats
// is suppressed. The ahead/behind/dirty numbers are unstable while git
// is rewriting history (rebase) or while the index holds a partial
// merge — showing `↑3 ↓1 *5` next to `⚠ rebasing` adds noise without
// adding signal because the user can't act on those numbers until the
// in-flight op resolves. Other badges (rename, mergeability,
// push_state, pr_status, shipped) keep rendering — they describe
// distinct facts about the branch (its name, its mergeability against
// main, its relationship to origin/<branch>, its PR state) that don't
// move under git's feet during a local rebase.
func RenderHintBadges(hints []state.Hint) string {
	if len(hints) == 0 {
		return ""
	}
	hasPR := false
	hasStuck := false
	for _, h := range hints {
		if h.Kind == "pr_status" {
			hasPR = true
		}
		if h.Kind == "stuck_state" && h.Message != "" {
			hasStuck = true
		}
	}
	parts := make([]string, 0, len(hints))
	// stuck_state goes first — orange + bold, leftmost. The user can't
	// reason about ahead/behind/dirty while mid-rebase, so this badge
	// preempts attention even before rename. Detector emits this only
	// when the worktree is genuinely stuck mid-operation, so the badge
	// always represents real action-required state.
	for _, h := range hints {
		if h.Kind == "stuck_state" && h.Message != "" {
			parts = append(parts, stuckStateStyle().Render(h.Message))
		}
	}
	// Render rename next so it sits left of the lifecycle-state badge.
	for _, h := range hints {
		if h.Kind == "rename_suggested" {
			parts = append(parts, hintRenameStyle().Render("↻ rename"))
		}
	}
	// Mergeability next — loud, attention-grabbing, sits left of the
	// numeric divergence counts so the warning reads before the
	// explanation. Detector emits this only when the simulated merge
	// against origin/<default> would conflict, so the badge always
	// represents real action-required state.
	for _, h := range hints {
		if h.Kind == "mergeability" && h.Message != "" {
			parts = append(parts, mergeabilityStyle().Render(h.Message))
		}
	}
	// Then git stats — show whenever there's a non-empty message, EXCEPT
	// when stuck_state has fired. During a rebase / mid-merge / mid-
	// cherry-pick the ahead/behind/dirty numbers reflect git's transient
	// internal state (rewritten HEAD, partial index) and are not signals
	// the user can act on; the actionable signal is "finish that op
	// first." Detector returns nil when all three counts are zero, so
	// clean workspaces don't get a noisy "↑0 ↓0 *0" badge.
	if !hasStuck {
		for _, h := range hints {
			if h.Kind == "git_stats" && h.Message != "" {
				parts = append(parts, gitStatsStyle().Render(h.Message))
			}
		}
	}
	// push_state next — answers the question git_stats does not: "is
	// my work backed up on origin/<branch>?" Distinct from `↑N`
	// (ahead of default) because a PR-ready branch can be ↑5 *0 yet
	// ⇡5 (unpushed) at the same time. Detector emits this only when
	// the local branch has commits not in upstream, so the badge
	// always represents real "you should push" state.
	for _, h := range hints {
		if h.Kind == "push_state" && h.Message != "" {
			parts = append(parts, pushStateStyle().Render(h.Message))
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

// projectHeaderStyle is the project-name banner above each group.
// Bold-white (231) so the name stands out as a section header without
// borrowing the violet that now belongs to the brand pill alone.
// Demoted from pale-violet 99 in the v0.22 palette work that contracted
// violet to brand-only — project headers, host banners (now pills), and
// selection (now teal) each got their own distinct visual lane so the
// eye can tell brand chrome, section heading, and cursor apart at a
// glance.
func projectHeaderStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("231")).
		Bold(true)
}

// hostPill renders the host-section banner as a rounded pill matching
// the top-bar scope pill (`global`). Same vocabulary so the eye reads
// host banners as peer chrome to the brand/scope pills, not as a
// separate hierarchy. ALL CAPS content because pills don't need bold
// to anchor — the shape carries the weight — and caps separates host
// names from project headers (sentence case) below.
//
// Glyph dependency: requires a Nerd Font for the powerline end-caps
// (U+E0B6 left, U+E0B4 right). Without them the caps render as tofu
// but the pill body still works. Same glyphs render.go's brand/scope
// pills already use, so any terminal that renders the top bar correctly
// will render these too.
func hostPill(label string) string {
	return roundedPillSubtleLocal(strings.ToUpper(label), "245", "237")
}

// roundedPillSubtleLocal duplicates render.roundedPillSubtle inside the
// projectlist package. Duplicated rather than imported because
// internal/ui imports projectlist; pulling render.go's pill helper
// across would create a cycle. The two should stay byte-identical so
// the chrome reads as one continuous visual system.
func roundedPillSubtleLocal(content, fgColor, bgColor string) string {
	cap := lipgloss.NewStyle().Foreground(lipgloss.Color(bgColor))
	body := lipgloss.NewStyle().
		Foreground(lipgloss.Color(fgColor)).
		Background(lipgloss.Color(bgColor))
	return cap.Render("") + body.Render(content) + cap.Render("")
}

// stalePillStyle is the "⚠ stale Ns" pill appended to a host section
// header when the host's most-recent successful refresh is older than
// staleThreshold. Amber matches StatusAuthFailed on the Hosts tab so
// "something needs attention" reads consistently across surfaces.
// v0.19 remote-status-observability.
func stalePillStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("214")).
		Bold(true)
}

// staleRowStyle dims an entire remote-row line when its host is stale.
// Uses lipgloss.Faint which emits the CSI 2m ANSI escape — terminals
// render that as low-intensity, typically ~50% opacity. Keeps the
// row legible while signaling "this is last-known, not live."
// v0.19 remote-status-observability.
func staleRowStyle() lipgloss.Style {
	return lipgloss.NewStyle().Faint(true)
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

// mergeabilityStyle: orange + bold. Conflicts are blocking work — louder
// than the informational grey of git_stats, on par with rename/PR-changes
// to slot into the existing "attention required" tier of the palette.
func mergeabilityStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
}

// stuckStateStyle: orange + bold. Mid-rebase / mid-merge / mid-pick /
// detached HEAD are blocking — the user can't do useful work until the
// in-flight git op is resolved. Same intensity tier as mergeability;
// sits in the row's "loud action required" palette slot.
func stuckStateStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
}

// pushStateStyle: cyan + bold. "Your work isn't on origin" is
// informational-but-actionable — not destructive like a stuck rebase
// or a merge conflict, but the user almost certainly wants to act on
// it (push). Distinct hue from the orange "warning" tier so the eye
// can scan a row of badges and tell at a glance which are "fix this
// before continuing" (orange) and which are "remember to push"
// (cyan).
func pushStateStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("51")).Bold(true)
}

// hintPRStyle: cyan — informational, not urgent.
func hintPRStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
}

// spinnerFrames mirrors hosts.spinnerFrames — duplicated here so the
// projectlist package stays free of the cyclic import that pulling in
// hosts would create (hosts already imports state; ui imports both).
// Same Braille rotation read at the same cadence, so the two surfaces
// animate in lockstep.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinnerGlyph(frame int) string {
	n := len(spinnerFrames)
	idx := frame % n
	if idx < 0 {
		idx += n
	}
	return spinnerFrames[idx]
}

// loadingRowStyle is the cyan styling for the "⠋ loading…" placeholder
// shown under a registered host whose first refresh hasn't landed yet.
// Same hue as the Hosts tab's StatusLoading glyph so the two surfaces
// share a "we're checking" vocabulary. v0.22.
func loadingRowStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("87"))
}

func subtleHelper() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
}

// currentMarkerStyle is the "← here" suffix on the row whose dir hosts
// the running canopy invocation. Cyan + bold so it pops against the
// surrounding subtle-grey hint badges without competing with the
// selection-bg highlight.
func currentMarkerStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("51")).Bold(true)
}

func selectionStyle() lipgloss.Style {
	// Teal bg (38) + bright white fg + bold. Picked over the previous
	// dark-grey 237 to break a three-way collision: scopePillStyle and
	// inactiveTabStyle in render.go both also live at bg=237, so a
	// selected row sat at the same visual weight as the static chrome
	// pills. Teal pulls the cursor distinctly forward without entering
	// the violet brand-pill family.
	return lipgloss.NewStyle().
		Background(lipgloss.Color("38")).
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

// agentBadge returns a single-character glyph for the row's agent
// state plus a per-state color. Returns "  " (two-space slot) when no
// badge is appropriate.
//
// All badge glyphs are width-1 in standard terminals (verified with
// lipgloss.Width); we still pad to a 2-cell slot so the column width
// math stays simple.
//
// State decision tree:
//   - row not alive, main row, or no tmux session → blank
//   - state map has an entry → render that state's glyph
//   - first poll hasn't landed yet (agentPolled=false) → blank
//     (we don't know if this workspace has an agent pane yet)
//   - first poll HAS landed but this session isn't in the map →
//     No-AI badge (workspace exists but has no agent pane)
//
// Color choices:
//   - 226 (yellow)  AwaitingInput — "you're blocking on this"
//   - 51  (cyan)    Thinking      — "claude is working"
//   - 244 (gray)    Idle          — "ready, waiting on you"
//   - 240 (subtle)  No-AI         — "this workspace has no agent"
func agentBadge(r state.GlobalRow, states map[string]agent.State, polled bool) string {
	if !r.Alive || r.IsMain || r.TmuxSession == "" {
		return "  "
	}
	// v0.17 Phase 1d.2: remote rows carry their agent state as a
	// string on r.AgentState (populated by host.Refresher from the
	// canopy ls --json wire field). The remote canopy classified via
	// single-shot pattern match so "thinking" is never set from this
	// path — only idle / awaiting_input / "" (unknown). We still get
	// the load-bearing ✋ awaiting-input badge, which is the
	// "blocked on me" signal users care about most. Blank when the
	// remote couldn't classify, matches the local "polled, no state"
	// fallback's quietness.
	if r.Host != "" {
		switch r.AgentState {
		case "awaiting_input":
			return lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Render("✋")
		case "thinking":
			return lipgloss.NewStyle().Foreground(lipgloss.Color("51")).Render("⚡")
		case "idle":
			return lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("💤")
		}
		return "  "
	}
	s, ok := states[r.TmuxSession]
	if !ok {
		// Not in the map. Two cases:
		//   1. First poll hasn't landed yet → blank, we don't know.
		//   2. Poll has landed → workspace has no agent pane → No-AI.
		if !polled {
			return "  "
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("·")
	}
	switch s {
	case agent.StateAwaitingInput:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Render("✋")
	case agent.StateThinking:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("51")).Render("⚡")
	case agent.StateIdle:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("💤")
	}
	// StateUnknown or unrecognized — blank, we don't have signal.
	return "  "
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
