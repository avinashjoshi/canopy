package ui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"

	"github.com/avinashjoshi/canopy/internal/ghx"
	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/ui/hosts"
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
	case newPickerMode:
		return m.renderNewPicker()
	case newFreshMode:
		return m.renderNewFresh()
	case newPromptMode:
		return m.renderNewPrompt()
	case newPRMode:
		return m.renderNewPR()
	case newIssueMode:
		return m.renderNewIssue()
	case newBranchMode:
		return m.renderNewBranch()
	case confirmDeleteMode:
		return m.renderConfirmDelete()
	case confirmKillMode:
		return m.renderConfirmKill()
	case confirmAttachMode:
		return m.renderConfirmAttach()
	case confirmHostRemoveMode:
		return m.renderConfirmHostRemove()
	case addHostFormMode:
		return m.renderAddHostForm()
	case hostDetailMode:
		return m.renderHostDetail()
	case confirmSSHCopyIDMode:
		return m.renderConfirmSSHCopyID()
	case drawerMode:
		return m.renderDrawer()
	case busyMode:
		return m.renderBusyView()
	case upgradeMode:
		return m.renderUpgrade()
	}

	if m.mode == confirmRetryMode {
		return m.renderConfirmRetry()
	}

	var b strings.Builder
	// Top bar: brand pill ◆ canopy + scope pill + optional version
	// pill. Pills are rounded-end via powerline glyphs — the eye reads
	// brand first, scope second, version (when present) third.
	//
	// Version pill is the design's "you always know which canopy is
	// running" cue: muted gray for release, cyan for DEV. Suppressed
	// when neither field is set (tests + edge cases).
	b.WriteString(roundedPill("◆ canopy", "231", "99"))           // bright white on violet
	b.WriteString(" ")
	b.WriteString(roundedPillSubtle(m.scopeLabel(), "250", "237")) // grey on dark grey
	if pill := m.renderVersionPill(); pill != "" {
		b.WriteString(" ")
		b.WriteString(pill)
	}
	b.WriteString("\n\n")

	// Tab bar + search-line on the row below the top bar.
	b.WriteString(m.renderTabBar())
	b.WriteString("    ")
	b.WriteString(m.renderSearchLine())
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("error: %v", m.err)))
		b.WriteString("\n\n")
	}

	// v0.17.0 Phase 1c: Hosts tab dispatches to its own subpackage
	// renderer. Bypasses the projectlist + empty-tab handling below
	// because hosts have their own row shape (and own empty state).
	if m.tab == tabHosts {
		b.WriteString(m.renderHostsTab())
		b.WriteString("\n\n")
		b.WriteString(m.renderHelpLine())
		return b.String()
	}

	// Empty-tab onboarding text. projectlist's own emptyState() shows
	// when SetRows([]) — but we want context-specific text (Local vs
	// Global), so we override here when filteredRows is empty.
	if len(m.filteredRows()) == 0 {
		if m.tab == tabLocal {
			if m.currentProject == "" {
				b.WriteString("  " + subtleStyle.Render("You're not in a canopy project."))
				b.WriteString("\n  ")
				b.WriteString(subtleStyle.Render("cd to a repo and run `canopy init` to get started."))
			} else if m.searchQuery != "" {
				b.WriteString("  " + subtleStyle.Render(fmt.Sprintf("No matches for %q in this project.", m.searchQuery)))
			} else if m.mgr != nil {
				b.WriteString("  " + subtleStyle.Render("No workspaces. Press 'n' to create one."))
			} else {
				b.WriteString("  " + subtleStyle.Render("No workspaces in this project."))
			}
		} else {
			if m.searchQuery != "" {
				b.WriteString("  " + subtleStyle.Render(fmt.Sprintf("No matches for %q across any project.", m.searchQuery)))
			} else {
				b.WriteString("  " + subtleStyle.Render("No projects yet."))
				b.WriteString("\n  ")
				b.WriteString(subtleStyle.Render("cd to a repo and run `canopy init` to register one."))
			}
		}
		b.WriteString("\n\n")
	} else {
		// Delegate to the projectlist sub-component for table render.
		// Same look as the v0.7 popup — grouped by project, indented
		// rows, hint badges per row, selected row highlighted.
		b.WriteString(m.list.View())
		b.WriteString("\n")
		if hint := m.selectedHint(); hint != "" {
			b.WriteString("  ")
			b.WriteString(brokenStyle.Render("hint:"))
			b.WriteString(" ")
			b.WriteString(hint)
			b.WriteString("\n  ")
			b.WriteString(subtleStyle.Render("press R to re-run setup against the existing worktree"))
			b.WriteString("\n\n")
		}
	}

	// Onboarding hint sits right above the help line — the natural
	// "next-action" zone of the screen. Global tab only because Local
	// already implies an existing project. Flush-left to match the
	// project headers + help line; blank line above and below so it
	// reads as a distinct row, not glued to the keybind cheatsheet.
	if m.tab == tabGlobal {
		b.WriteString(subtleStyle.Render("add a project: cd to its repo and run `canopy init`"))
		b.WriteString("\n\n")
	}
	b.WriteString(m.renderHelpLine())
	return b.String()
}

// scopeLabel returns the secondary top-bar pill text. Shows the current
// project's basename when focused (Local context resolved); otherwise
// "global" so the user knows the brand pill alone isn't promising
// project context that doesn't exist.
func (m *Model) scopeLabel() string {
	if m.projectName != "" {
		return "🖥 " + m.projectName
	}
	return "global"
}

// renderTabBar draws the tab bar as styled pills. Active tab uses the
// violet brand-color bg; inactive uses a darker grey-bg pill so both
// read as buttons, not text.
//
// v0.17 Phase 1h: the project-scoped tab (tabLocal) only renders when
// a current project context exists — launching canopy outside any
// project drops it from the bar entirely. The Hosts tab dims when the
// registry is empty.
func (m *Model) renderTabBar() string {
	hasLocal := false
	hasGlobal := false
	for _, r := range m.allRowsOrFallback() {
		hasGlobal = true
		if m.currentProject != "" && r.ProjectRoot == m.currentProject {
			hasLocal = true
		}
	}

	// Pill colors: active = bright white on violet (matches brand pill);
	// inactive = grey on dark grey. Empty tabs use a dimmer foreground
	// so the user doesn't feel pulled to switch to nothing.
	tabPill := func(label string, active, hasRows bool) string {
		switch {
		case active && hasRows:
			return roundedPill(label, "231", "99")
		case active && !hasRows:
			return roundedPill(label, "250", "99") // dimmed fg on active bg
		case !active && hasRows:
			return roundedPillSubtle(label, "250", "237")
		default: // !active && !hasRows
			return roundedPillSubtle(label, "241", "237")
		}
	}

	arrow := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	pills := []string{arrow.Render("‹")}

	if m.currentProject != "" {
		// Use the project basename as the tab label so the user knows
		// exactly which project they're scoped to.
		proj := m.currentProject
		for i := len(proj) - 1; i >= 0; i-- {
			if proj[i] == '/' {
				proj = proj[i+1:]
				break
			}
		}
		if len(proj) > 18 {
			proj = proj[:18]
		}
		pills = append(pills, tabPill(proj, m.tab == tabLocal, hasLocal))
	}
	pills = append(pills, tabPill("Workspaces", m.tab == tabGlobal, hasGlobal))
	// Hosts pill is always shown when the registry is non-empty OR when
	// the user is currently on it (so an empty-state view is still
	// reachable as a fallback). v0.17 Phase 1l rename: "Remote hosts"
	// makes the tab's purpose obvious without scanning the body.
	if m.hostsHasEntries() || m.tab == tabHosts {
		pills = append(pills, tabPill("Remote hosts", m.tab == tabHosts, m.hostsHasEntries()))
	}
	pills = append(pills, arrow.Render("›"))
	return joinWithSpaces(pills)
}

// joinWithSpaces joins pieces with a single ASCII space. Tiny helper
// rather than strings.Join so it's obvious at the call site we're
// padding tab pills, not building general text.
func joinWithSpaces(pieces []string) string {
	if len(pieces) == 0 {
		return ""
	}
	out := pieces[0]
	for _, p := range pieces[1:] {
		out += " " + p
	}
	return out
}

// renderSearchLine returns the search input pill (when in search mode)
// or a persistent-filter indicator when a query is set but the user has
// exited search mode.
//
// Three visual states:
//   - active search:  `[🔍 SEARCH] foo█`         (bright; cursor blink)
//   - persistent filter: `🔍 foo` + esc-to-clear hint (subtle)
//   - no search:      empty                     (the help line shows / shortcut)
func (m *Model) renderSearchLine() string {
	if m.searchMode {
		// Active mode: bright label pill + visible query + cursor.
		// Cursor is `▏` (a thin vertical bar) — reads as a real
		// blinking caret on most terminals and doesn't look like
		// a content character the way `█` could.
		label := searchLabelStyle.Render("🔍 SEARCH")
		body := searchInputStyle.Render(" " + m.searchQuery + "▏")
		return label + body
	}
	if m.searchQuery != "" {
		// Persistent filter — the user typed a query then exited
		// search mode (Enter). Show what's filtering AND how to
		// clear, both in dim text so they don't compete with the
		// table content.
		return subtleStyle.Render("🔍 ") +
			subtleStyle.Render(m.searchQuery) +
			subtleStyle.Render("  (esc to clear)")
	}
	return ""
}

// allRowsOrFallback returns m.allRows; the back-compat fallback is
// gone now that the unified TUI's refresh path always populates
// allRows. Kept as a thin accessor so callers don't reach into
// the field directly (decoupling internal storage from renderers).
func (m *Model) allRowsOrFallback() []state.GlobalRow {
	return m.allRows
}

// hostsHasEntries returns whether any hosts are registered. Used by
// renderTabBar to dim/highlight the Hosts pill — empty registry =
// dim, matching the "empty tabs render dim" convention. v0.17.0
// Phase 1c.
func (m *Model) hostsHasEntries() bool {
	return len(m.hostList) > 0
}

// renderHostsTab is the Hosts tab body. Delegates to the
// internal/ui/hosts subpackage's BuildRows + Render. Width-aware per
// the D2 design decision (tiered column drop at narrow widths).
func (m *Model) renderHostsTab() string {
	rows := hosts.BuildRows(m.hostList, m.remoteSnaps)
	w := m.width
	if w <= 0 {
		w = 100 // reasonable default before WindowSizeMsg lands
	}
	// Clamp the cursor to the rendered row count so a host that
	// vanished between ticks doesn't leave the caret hanging past the
	// end. v0.17 Phase 1l.
	cursor := m.hostsCursor
	if cursor >= len(rows) {
		cursor = len(rows) - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	return hosts.Render(rows, w, cursor)
}

// renderConfirmHostRemove is the y/N gate for `d` on the Hosts tab.
// v0.17 Phase 1l. Mirrors renderConfirmKill's shape.
func (m *Model) renderConfirmHostRemove() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("remove host"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  Remove host %q from the registry?\n\n", m.hostRemoveTarget))
	b.WriteString(subtleStyle.Render("  Only the registry entry goes (~/.canopy/hosts.json). The remote\n"))
	b.WriteString(subtleStyle.Render("  canopy install, its workspaces, and any cached state on the\n"))
	b.WriteString(subtleStyle.Render("  laptop (~/.canopy/remotes-cache.json) are not touched.\n"))
	b.WriteString("\n  ")
	b.WriteString(brokenStyle.Render("y"))
	b.WriteString(" to remove  ·  any other key to cancel")
	return b.String()
}

// renderAddHostForm draws the in-TUI add-host form. Two textinputs
// stacked vertically — name first, ssh-target below. Tab/shift+tab
// cycles focus; the focused input shows its caret while the other
// renders dimmed. Enter submits when both fields are non-empty.
// v0.17 Phase 1l polish.
func (m *Model) renderAddHostForm() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("add remote host"))
	b.WriteString("\n\n")
	b.WriteString(subtleStyle.Render("  A remote host is an SSH-reachable machine with canopy installed.\n"))
	b.WriteString(subtleStyle.Render("  After adding, canopy will probe the connection — if key auth\n"))
	b.WriteString(subtleStyle.Render("  isn't set up, you'll be offered ssh-copy-id automatically.\n"))
	b.WriteString("\n")
	b.WriteString("  name:    " + m.nameInput.View() + "\n")
	b.WriteString("  target:  " + m.targetInput.View() + "\n")
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render("  tab to switch  ·  enter to confirm  ·  esc to cancel"))
	return b.String()
}

// renderHostDetail is the read-only detail view for one host. Shows
// everything the registry + remotes-cache knows about it. v0.17
// Phase 1l polish. Esc dismisses back to the Hosts tab.
func (m *Model) renderHostDetail() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("host: " + m.hostDetailTarget))
	b.WriteString("\n\n")
	var h host.Host
	for _, hh := range m.hostList {
		if hh.Name == m.hostDetailTarget {
			h = hh
			break
		}
	}
	if h.Name == "" {
		b.WriteString("  (host no longer in registry)\n")
		b.WriteString("\n  " + subtleStyle.Render("esc to go back"))
		return b.String()
	}
	b.WriteString(fmt.Sprintf("  ssh target:  %s\n", h.SSHTarget))
	b.WriteString(fmt.Sprintf("  type:        %s\n", h.Type))
	b.WriteString(fmt.Sprintf("  added:       %s\n", h.AddedAt.Format("2006-01-02 15:04")))
	b.WriteString("\n")
	if len(h.Projects) == 0 {
		b.WriteString(subtleStyle.Render("  no projects registered\n"))
		b.WriteString(subtleStyle.Render(fmt.Sprintf("  → canopy project add <name> <remote-path> --on %s\n", h.Name)))
	} else {
		b.WriteString("  projects:\n")
		names := make([]string, 0, len(h.Projects))
		for n := range h.Projects {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			b.WriteString(fmt.Sprintf("    %s  →  %s\n", n, h.Projects[n]))
		}
	}
	if snap := m.remoteSnaps[h.Name]; snap != nil {
		b.WriteString("\n")
		if snap.CanopyVersion != "" {
			b.WriteString(fmt.Sprintf("  canopy:      v%s\n", snap.CanopyVersion))
		}
		if !snap.LastSeen.IsZero() {
			b.WriteString(fmt.Sprintf("  last seen:   %s\n", snap.LastSeen.Format("2006-01-02 15:04:05")))
		}
		b.WriteString(fmt.Sprintf("  workspaces:  %d\n", len(snap.Workspaces)))
		if snap.LastError != "" {
			b.WriteString("\n  ")
			b.WriteString(brokenStyle.Render("last error: " + snap.LastError))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n  " + subtleStyle.Render("esc to go back"))
	return b.String()
}

// renderConfirmSSHCopyID prompts the user to run ssh-copy-id after a
// post-Add probe came back AuthFailed. y/Y kicks off the subprocess
// (which prompts for the remote password); anything else dismisses
// and the host stays registered without key auth. v0.17 Phase 1l.
func (m *Model) renderConfirmSSHCopyID() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("set up passwordless ssh?"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  %s is registered, but ssh key auth isn't set up yet.\n", m.pendingProbeHost))
	b.WriteString(fmt.Sprintf("  Without it, every canopy operation on %s would hang waiting\n", m.pendingProbeHost))
	b.WriteString("  for a password (BatchMode in the refresher prevents that).\n\n")
	b.WriteString(subtleStyle.Render(fmt.Sprintf("  Run: ssh-copy-id %s\n", m.pendingProbeTarget)))
	b.WriteString(subtleStyle.Render("  (You'll be prompted for the remote password once.)\n"))
	b.WriteString("\n  ")
	b.WriteString(brokenStyle.Render("y"))
	b.WriteString(" to set it up now  ·  any other key to skip")
	return b.String()
}

// renderConfirmRetry renders the y/N gate for `R` on a non-broken
// workspace (D3/CP1). Mirrors renderConfirmDelete's shape.
func (m *Model) renderConfirmRetry() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("canopy") + " " + subtleStyle.Render(m.projectName))
	b.WriteString("\n\n")
	b.WriteString(brokenStyle.Render("Retry setup on healthy workspace?"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  %s is not in `broken` status.\n", m.retryTarget))
	b.WriteString("  Re-running scripts.setup may clobber state\n")
	b.WriteString("  that setup mutates (db reseed, env regen, etc.).\n\n")
	b.WriteString("  Press y to force retry, any other key to cancel.\n")
	return b.String()
}

// renderTable + hasMultipleProjects were the unified TUI's hand-rolled
// table renderer. Removed in favor of the projectlist sub-component,
// whose grouped-by-project layout matches the v0.7 popup look that
// users built muscle memory around.

// renderHelpLine is the bottom-bar keybind cheatsheet. Each binding
// renders as `[key] desc` — key in inverted-bg pill, desc in subtle
// text. Lazyworktree-flavored. Driven by listModeBindings + Available
// predicates so e.g. `n` and `o` appear/disappear contextually.
//
// Up/down/g/G fold into one nav entry; rendering each individually
// would overflow narrow popups with little signal.
func (m *Model) renderHelpLine() string {
	var parts []string
	parts = append(parts, keyPillStyle.Render("↑/↓")+" "+subtleStyle.Render("nav"))

	skip := map[string]bool{"up": true, "down": true, "g": true, "G": true}
	for _, b := range listModeBindings {
		if !b.IsAvailable(m) {
			continue
		}
		keys := b.K.Keys()
		if len(keys) == 0 || skip[keys[0]] {
			continue
		}
		h := b.K.Help()
		parts = append(parts, keyPillStyle.Render(h.Key)+" "+subtleStyle.Render(h.Desc))
	}
	return strings.Join(parts, "  ")
}

// renderTargetBanner is the unmissable "creating in: <project>" header
// shown at the top of every screen in the new-workspace flow (picker,
// sub-modals, busy view). Powered by m.newTargetName + m.newTargetRoot,
// which actionNewWorkspace populates before the picker opens.
//
// Why mandatory: when `n` is triggered from the Global tab targeting a
// project other than the launch context (m.mgr), the banner is the
// signal that prevents creating a workspace in the wrong project.
// Hiding or dimming this defeats the whole point of cross-project `n`.
//
// Layout:
//
//	  creating in   cravd   ~/Work/cravd
//
// The chip uses roundedPill (brand violet bg + bright white fg) so it
// reads as a primary identifier on the same vocabulary as the brand
// pill and active tab — not chrome to be skimmed past.
func (m *Model) renderTargetBanner() string {
	if m.newTargetName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(subtleStyle.Render("creating in"))
	b.WriteString("  ")
	b.WriteString(roundedPill(m.newTargetName, "231", "99"))
	if m.newTargetRoot != "" {
		b.WriteString("  ")
		b.WriteString(subtleStyle.Render(m.newTargetRoot))
	}
	b.WriteString("\n\n")
	return b.String()
}

// renderFilterPill renders the filter input chrome for the PR / issue /
// branch sub-modals. Same visual vocabulary as the main TUI's
// renderSearchLine pill (🔍 + brand-violet label + grey input bg + the
// thin "▏" caret) so the user reads "this is the typing-to-narrow
// box" without learning a modal-specific UI.
//
// Why FILTER instead of SEARCH: the main TUI's `/` is search across
// all rows; here, the input is the modal's primary action — narrowing
// a fixed list of PRs/issues/branches the user opened the modal to
// pick from. Different verb, same chrome family.
//
// Why we render manually instead of calling ti.View(): bubbles'
// textinput overwrites Cursor.SetChar on every render with the
// character at the cursor position, then renders it via Cursor.View()
// (which reverses fg/bg for the block-cursor look). That fights the
// main TUI's "▏" thin-bar caret. We bypass by reading ti.Value()
// directly — textinput stays the capture model for Update, but
// rendering matches renderSearchLine exactly.
//
// Placeholder handling: when the value is empty, surface the
// textinput's Placeholder as a dim hint to the right of the pill so
// the modal-specific "type a PR number, or arrow to a row below"
// guidance still has a home. Once the user types anything, the hint
// gets out of the way.
func renderFilterPill(ti textinput.Model) string {
	label := searchLabelStyle.Render("🔍 FILTER")
	typed := ti.Value()
	body := searchInputStyle.Render(" " + typed + "▏ ")
	if typed == "" && ti.Placeholder != "" {
		return label + body + "  " + subtleStyle.Render(ti.Placeholder)
	}
	return label + body
}

// renderNewPicker is step 1 of the new-workspace flow — the variant
// picker. Each option carries a single-letter shortcut printed in
// brackets so the user sees "press p for pull request" without
// having to read a footer. Arrow nav is a discoverable alternative
// for keyboard-only users who scan before they act.
//
// Self-evident over self-explanatory: the user shouldn't have to
// read the footer to know what to do. The bracketed letters
// telegraph the entire keymap inline with the options.
func (m *Model) renderNewPicker() string {
	var b strings.Builder
	b.WriteString(m.renderTargetBanner())
	b.WriteString(titleStyle.Render("new workspace"))
	b.WriteString("\n\n")
	b.WriteString("  How do you want to start?\n\n")

	options := newPickerOptions
	if m.newTargetHost != "" {
		// Remote target: only Fresh + Prompt are wired through
		// remoteCreateCmd. PR/Issue/Branch need a local gh that knows
		// the remote project's GitHub repo, which we don't have. Hide
		// them rather than show options that error on submit. v0.17
		// Phase 1k.
		options = newPickerOptions[:2]
	}
	for i, opt := range options {
		// Cursor caret matches the main workspace list's "❯ " — same
		// glyph across every screen so the eye reads "here's what's
		// selected" without learning a per-modal indicator. Non-
		// selected rows pad with whitespace so columns don't shift
		// as the cursor moves.
		cursor := "    "
		if i == m.newPickerCursor {
			cursor = "  ❯ "
		}
		// Letter shortcut + label, then a dim description on the
		// next line. Two-line entries give the picker breathing
		// room and let the description carry the "why this option"
		// without bloating the headline.
		//
		// Shortcut pill uses keyPillStyle — the same inverted-bg pill
		// used by the help line at the bottom — so "press this key"
		// reads with the same visual vocabulary across surfaces.
		// Avoid brokenStyle: that's broken-status / error red, and a
		// shortcut letter is neither.
		b.WriteString(cursor)
		b.WriteString(keyPillStyle.Render(opt.key))
		b.WriteString("  ")
		if i == m.newPickerCursor {
			b.WriteString(selectedStyle.Render(opt.label))
		} else {
			b.WriteString(opt.label)
		}
		b.WriteString("\n        ")
		b.WriteString(subtleStyle.Render(opt.description))
		b.WriteString("\n\n")
	}

	b.WriteString(subtleStyle.Render(
		"  press a letter  ·  ↑/↓ then enter  ·  esc back"))
	return b.String()
}

// newPickerOption is one row in the variant picker. Order in the
// slice = visual order = cursor index. Adding a 5th option requires
// updating newPickerOptionCount in update.go.
type newPickerOption struct {
	key         string // single-letter shortcut
	label       string // headline shown next to the cursor
	description string // dim one-liner under the label
}

// newPickerOptions is the canonical list of source variants.
// Mirrors workspace.SourceSpec — one row per variant so adding a
// new SourceKind means adding a row here, a case in update.go's
// dispatch, and a renderer below.
var newPickerOptions = []newPickerOption{
	{"n", "Fresh workspace", "random name, branch off main"},
	// "From a prompt" sits second — directly under the fresh path —
	// because it IS a fresh workspace; the only difference is that
	// you hand the agent a task at launch. Keeping it adjacent to
	// "Fresh workspace" matches that mental model and surfaces it
	// before the gh-driven variants for the common case. The key
	// is `t` (think "task") — `p` already belongs to pull-request,
	// and there's no perfect mnemonic letter for "prompt." The
	// description carries the agent-task framing so the letter
	// doesn't have to.
	{"t", "From a prompt", "fresh workspace; send the agent an initial task"},
	{"p", "From a pull request", "check out a PR's branch (uses gh)"},
	{"i", "From an issue", "implement work from an issue (uses gh)"},
	{"b", "From a branch", "check out an existing branch"},
}

// promptBorderStyle wraps the prompt textarea in a single rounded
// violet box. Drawing the border out here (instead of via the
// textarea's per-line Base style) avoids a subtle bug where the
// top-edge corner glyphs render unevenly under some widths — the
// textarea's internal renderer paints each row independently, and
// the border continuation between rows can break visually. One
// outer lipgloss.Border() draws the box as a single unit.
var promptBorderStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("99")). // matches canopy brand violet
	Padding(0, 1)

// renderNewPrompt is step 2e — prompt input for the "fresh + send
// the agent a task" path. Workspace name is namegen-only (no name
// input in this mode) so the user can focus on the prompt content.
//
// The input is a multi-line textarea: Enter inserts a newline (so
// users can paste/type a real task brief), Ctrl+S submits. The
// footer telegraphs the submit-vs-newline split because terminal
// users tend to expect Enter = submit by default; without the
// hint, the first Enter feels broken.
func (m *Model) renderNewPrompt() string {
	var b strings.Builder
	b.WriteString(m.renderTargetBanner())
	b.WriteString(titleStyle.Render("new workspace"))
	b.WriteString(" ")
	b.WriteString(subtleStyle.Render("· prompt"))
	b.WriteString("\n\n")
	b.WriteString("  What should the agent work on?\n\n")
	// Indent the bordered textarea by two spaces to line up with the
	// "What should the agent work on?" headline above. lipgloss.JoinVertical
	// would also work; manual indent keeps the chrome consistent with
	// the other new-flow renderers (all of which two-space-indent
	// their primary input).
	boxed := promptBorderStyle.Render(m.promptInput.View())
	for _, line := range strings.Split(boxed, "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(
		"  ctrl+s submit  ·  enter newline  ·  esc back"))
	return b.String()
}

// renderNewFresh is step 2a — name input for the fresh-workspace
// path. Same shape as the v0 modal that the picker replaced; this
// is the simple/common case that most `n` presses end at.
func (m *Model) renderNewFresh() string {
	var b strings.Builder
	b.WriteString(m.renderTargetBanner())
	b.WriteString(titleStyle.Render("new workspace"))
	b.WriteString(" ")
	b.WriteString(subtleStyle.Render("· fresh"))
	b.WriteString("\n\n")
	b.WriteString("  Workspace name (leave blank for a random one):\n\n  ")
	b.WriteString(m.nameInput.View())
	b.WriteString("\n\n")
	b.WriteString(subtleStyle.Render("  enter to create  ·  esc to back"))
	return b.String()
}

// pickerVisibleRows returns how many list rows fit in the picker
// modal given the terminal height. Reserves a fixed budget for
// chrome (title, blank line, filter label + input, blank, footer,
// scroll-hint lines, margins). When height is unknown (zero —
// before the first WindowSizeMsg arrives) returns a sensible
// default so the picker still renders.
func (m *Model) pickerVisibleRows() int {
	const chrome = 9 // 8 lines of title/input/footer + 1 line slack
	const minVisible = 5
	if m.height <= 0 {
		return 15 // pre-WindowSize fallback
	}
	v := m.height - chrome
	if v < minVisible {
		return minVisible
	}
	return v
}

// pickerWindow clamps a cursor + total length into a (top, end)
// range that holds the cursor in the visible window. Pure function
// so each renderer can inline it without state-tracking. Returns
// indices into the FULL filtered list; the renderer slices it.
//
// Behavior: the cursor sits anywhere in the window. When it crosses
// the bottom edge, the window scrolls so cursor is the LAST visible
// row. When it crosses the top edge (back-arrow into hidden rows),
// cursor becomes the FIRST visible row. This is the standard
// "cursor stays visible, list scrolls" pattern from lazygit/k9s.
func pickerWindow(cursor, total, visible int) (int, int) {
	if total <= visible {
		return 0, total
	}
	top := cursor - visible + 1
	if top < 0 {
		top = 0
	}
	if top > total-visible {
		top = total - visible
	}
	return top, top + visible
}

// renderScrollHint emits a one-line "↑ N more above" / "↓ N more
// below" indicator so the user knows the list extends past what's
// rendered. Empty string when the whole list fits.
func renderScrollHint(top, end, total int) string {
	if total <= end-top {
		return ""
	}
	above := top
	below := total - end
	parts := []string{}
	if above > 0 {
		parts = append(parts, fmt.Sprintf("↑ %d more above", above))
	}
	if below > 0 {
		parts = append(parts, fmt.Sprintf("↓ %d more below", below))
	}
	return subtleStyle.Render("  " + strings.Join(parts, "  ·  "))
}

// renderNewPR is step 2b — the PR picker. Layout:
//
//	canopy new · pull request
//
//	  PR number or filter:
//	  [_______________________]
//
//	  ● #1185  pdx91   Inbox improvements
//	    #1182  jess    Fix oauth redirect
//	    #1180  sam     Add export endpoint
//
//	  enter to check out · ↑/↓ pick · esc to back
//
// Three states: loading (spinner-ish hint), error (gh missing /
// network failed), populated (list with cursor). Even with empty
// list the user can type a number directly — that's the power-user
// fast path.
func (m *Model) renderNewPR() string {
	var b strings.Builder
	b.WriteString(m.renderTargetBanner())
	b.WriteString(titleStyle.Render("new workspace"))
	b.WriteString(" ")
	b.WriteString(subtleStyle.Render("· pull request"))
	b.WriteString("\n\n")

	b.WriteString("  ")
	b.WriteString(renderFilterPill(m.listInput))
	b.WriteString("\n\n")

	switch {
	case m.newLoading && len(m.newPRs) == 0:
		b.WriteString(subtleStyle.Render("  Loading PRs from gh..."))
		b.WriteString("\n")
	case m.newLoadErr != nil:
		b.WriteString("  ")
		b.WriteString(errorStyle.Render(fmt.Sprintf("error: %v", m.newLoadErr)))
		b.WriteString("\n")
	case len(m.newPRs) == 0:
		b.WriteString(subtleStyle.Render("  No open PRs found. Type a number to fetch any PR by ID."))
		b.WriteString("\n")
	default:
		filtered := filterPRs(m.newPRs, m.listInput.Value())
		if len(filtered) == 0 {
			b.WriteString(subtleStyle.Render("  No PRs match. Type a number to fetch by ID."))
			b.WriteString("\n")
		} else {
			cursor := m.listCursor
			if cursor >= len(filtered) {
				cursor = len(filtered) - 1
			}
			top, end := pickerWindow(cursor, len(filtered), m.pickerVisibleRows())
			for i := top; i < end; i++ {
				pr := filtered[i]
				marker := "    "
				if i == cursor {
					marker = "  ❯ "
				}
				// Lookup the workspace (if any) currently holding
				// this PR's branch so the row can be tagged "in
				// <ws>". Recognition cue: don't try to re-create
				// what's already a workspace.
				wsName, taken := m.branchInWorkspace(pr.HeadRefName)
				core := fmt.Sprintf("%s#%-5d %-12s %s",
					marker, pr.Number, truncateRight(pr.Author.Login, 12), pr.Title)
				suffix := ""
				if taken {
					suffix = subtleStyle.Render("  (in workspace " + wsName + ")")
					core = subtleStyle.Render(core)
				}
				if i == cursor && !taken {
					b.WriteString(selectedStyle.Render(core))
				} else {
					b.WriteString(core)
				}
				b.WriteString(suffix)
				b.WriteString("\n")
			}
			if hint := renderScrollHint(top, end, len(filtered)); hint != "" {
				b.WriteString(hint)
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(
		"  enter to check out  ·  ↑/↓ pick row  ·  esc back"))
	return b.String()
}

// filterPRs narrows the loaded list when the user types in the
// filter field. Three match modes (in priority order):
//
//   - Numeric: the input is a number, so match the PR number prefix
//     (typing "11" matches #11, #1185, #1182). Useful for power
//     users who half-remember a PR number.
//   - Substring: case-insensitive match against title + author.
//
// Returns the same slice unfiltered when the input is empty.
func filterPRs(prs []ghx.PRSummary, filter string) []ghx.PRSummary {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return prs
	}
	out := make([]ghx.PRSummary, 0, len(prs))
	if _, isNum := parsePositiveIntForView(filter); isNum {
		for _, pr := range prs {
			if strings.HasPrefix(strconv.Itoa(pr.Number), filter) {
				out = append(out, pr)
			}
		}
		return out
	}
	lower := strings.ToLower(filter)
	for _, pr := range prs {
		if strings.Contains(strings.ToLower(pr.Title), lower) ||
			strings.Contains(strings.ToLower(pr.Author.Login), lower) {
			out = append(out, pr)
		}
	}
	return out
}

// parsePositiveIntForView mirrors parsePositiveInt in update.go.
// Duplicated here to avoid a view→update package edge; tiny helper.
func parsePositiveIntForView(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// truncateRight clips a string to width with no ellipsis. Used in
// fixed-column rows where ellipsis would visually compete with
// other markers (cursor `●`, etc.).
func truncateRight(s string, width int) string {
	if len(s) <= width {
		return s
	}
	return s[:width]
}

// renderNewIssue mirrors renderNewPR for the issue picker. Same
// layout and state-switch logic; different data type rendered.
func (m *Model) renderNewIssue() string {
	var b strings.Builder
	b.WriteString(m.renderTargetBanner())
	b.WriteString(titleStyle.Render("new workspace"))
	b.WriteString(" ")
	b.WriteString(subtleStyle.Render("· issue"))
	b.WriteString("\n\n")

	b.WriteString("  ")
	b.WriteString(renderFilterPill(m.listInput))
	b.WriteString("\n\n")

	switch {
	case m.newLoading && len(m.newIssues) == 0:
		b.WriteString(subtleStyle.Render("  Loading issues from gh..."))
		b.WriteString("\n")
	case m.newLoadErr != nil:
		b.WriteString("  ")
		b.WriteString(errorStyle.Render(fmt.Sprintf("error: %v", m.newLoadErr)))
		b.WriteString("\n")
	case len(m.newIssues) == 0:
		b.WriteString(subtleStyle.Render("  No open issues found. Type a number to fetch any issue by ID."))
		b.WriteString("\n")
	default:
		filtered := filterIssues(m.newIssues, m.listInput.Value())
		if len(filtered) == 0 {
			b.WriteString(subtleStyle.Render("  No issues match. Type a number to fetch by ID."))
			b.WriteString("\n")
		} else {
			cursor := m.listCursor
			if cursor >= len(filtered) {
				cursor = len(filtered) - 1
			}
			top, end := pickerWindow(cursor, len(filtered), m.pickerVisibleRows())
			for i := top; i < end; i++ {
				iss := filtered[i]
				marker := "    "
				if i == cursor {
					marker = "  ❯ "
				}
				line := fmt.Sprintf("%s#%-5d %-12s %s",
					marker, iss.Number, truncateRight(iss.Author.Login, 12), iss.Title)
				if i == cursor {
					b.WriteString(selectedStyle.Render(line))
				} else {
					b.WriteString(line)
				}
				b.WriteString("\n")
			}
			if hint := renderScrollHint(top, end, len(filtered)); hint != "" {
				b.WriteString(hint)
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(
		"  enter to use issue  ·  ↑/↓ pick row  ·  esc back"))
	return b.String()
}

// filterIssues mirrors filterPRs but for issues. Same numeric-prefix
// + substring split.
func filterIssues(issues []ghx.IssueSummary, filter string) []ghx.IssueSummary {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return issues
	}
	out := make([]ghx.IssueSummary, 0, len(issues))
	if _, isNum := parsePositiveIntForView(filter); isNum {
		for _, iss := range issues {
			if strings.HasPrefix(strconv.Itoa(iss.Number), filter) {
				out = append(out, iss)
			}
		}
		return out
	}
	lower := strings.ToLower(filter)
	for _, iss := range issues {
		if strings.Contains(strings.ToLower(iss.Title), lower) ||
			strings.Contains(strings.ToLower(iss.Author.Login), lower) {
			out = append(out, iss)
		}
	}
	return out
}

// renderNewBranch is step 2d — the branch picker. Different from
// PR/issue: branches don't have numeric IDs, so the "type number"
// fast-path is gone. Filter + arrow-pick is the only flow. Local-
// only branches get a "(local only)" tag so the user knows they're
// using the --allow-local-equivalent path.
func (m *Model) renderNewBranch() string {
	var b strings.Builder
	b.WriteString(m.renderTargetBanner())
	b.WriteString(titleStyle.Render("new workspace"))
	b.WriteString(" ")
	b.WriteString(subtleStyle.Render("· branch"))
	b.WriteString("\n\n")

	b.WriteString("  ")
	b.WriteString(renderFilterPill(m.listInput))
	b.WriteString("\n\n")

	switch {
	case m.newLoading && len(m.newBranches) == 0:
		b.WriteString(subtleStyle.Render("  Reading branches..."))
		b.WriteString("\n")
	case m.newLoadErr != nil:
		b.WriteString("  ")
		b.WriteString(errorStyle.Render(fmt.Sprintf("error: %v", m.newLoadErr)))
		b.WriteString("\n")
	case len(m.newBranches) == 0:
		b.WriteString(subtleStyle.Render("  No branches found in this repo."))
		b.WriteString("\n")
	default:
		filtered := filterBranches(m.newBranches, m.listInput.Value())
		if len(filtered) == 0 {
			b.WriteString(subtleStyle.Render("  No branches match the filter."))
			b.WriteString("\n")
		} else {
			cursor := m.listCursor
			if cursor >= len(filtered) {
				cursor = len(filtered) - 1
			}
			top, end := pickerWindow(cursor, len(filtered), m.pickerVisibleRows())
			for i := top; i < end; i++ {
				ref := filtered[i]
				marker := "    "
				if i == cursor {
					marker = "  ❯ "
				}
				// Strip "origin/" for the workspace-conflict
				// lookup so a remote-tracking ref that points at
				// the same logical branch as a local checkout
				// still shows the "in workspace X" tag.
				bare := strings.TrimPrefix(ref, "origin/")
				wsName, taken := m.branchInWorkspace(bare)

				suffix := ""
				core := marker + ref
				switch {
				case taken:
					suffix = subtleStyle.Render("  (in workspace " + wsName + ")")
					core = subtleStyle.Render(core)
				case !strings.HasPrefix(ref, "origin/") &&
					!branchHasOriginInline(m.newBranches, ref):
					// Local-only branch — keep the existing tag.
					suffix = subtleStyle.Render("  (local only)")
				}
				if i == cursor && !taken {
					core = selectedStyle.Render(marker + ref)
				}
				b.WriteString(core)
				b.WriteString(suffix)
				b.WriteString("\n")
			}
			if hint := renderScrollHint(top, end, len(filtered)); hint != "" {
				b.WriteString(hint)
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(subtleStyle.Render(
		"  enter to check out  ·  ↑/↓ pick row  ·  esc back"))
	return b.String()
}

// branchHasOriginInline mirrors update.go's branchHasOrigin for the
// view layer. View doesn't import update, so we duplicate this tiny
// helper rather than create a shared package edge for one function.
func branchHasOriginInline(branches []string, bare string) bool {
	target := "origin/" + bare
	for _, b := range branches {
		if b == target {
			return true
		}
	}
	return false
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

// renderConfirmAttach is the y/N prompt before attaching to a tmux
// session that already has another client connected. v0.17 Phase 1j.
//
// Tmux semantics: a second client on the same session sees the same
// panes live — keystrokes and selections from both clients interleave.
// For an active agent (Claude/aider) that's almost always a foot-gun:
// two terminals pasting prompts is chaos. The prompt makes that
// explicit so the user opts in deliberately.
func (m *Model) renderConfirmAttach() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("workspace is already open"))
	b.WriteString("\n\n")
	row := m.attachTarget
	label := row.Name
	if row.Project != "" {
		label = row.Project + "/" + row.Name
	}
	b.WriteString(fmt.Sprintf("  %q has an active tmux client already connected.\n\n", label))
	b.WriteString(subtleStyle.Render("  Attaching here will share the session — keystrokes from both\n"))
	b.WriteString(subtleStyle.Render("  terminals interleave, so any agent in this workspace will see\n"))
	b.WriteString(subtleStyle.Render("  input from both windows. Usually that's not what you want.\n"))
	b.WriteString("\n")
	b.WriteString("  ")
	b.WriteString(brokenStyle.Render("y"))
	b.WriteString(" to attach anyway  ·  any other key to cancel")
	return b.String()
}

// renderConfirmKill is the y/N prompt for K (kill tmux session). Less
// destructive than rm — only the tmux session goes. Re-pressing Enter
// after kill resurrects via Manager.Resurrect (workspace rows) or
// Manager.EnsureMainSession (main rows).
//
// The prompt copy adapts to the row type. For workspace rows it
// reassures that state.json + worktree dir + branch all survive
// (the things that COULD be lost in canopy rm, so worth naming
// explicitly here as "this is NOT that"). For main rows there's
// nothing identity-level to lose, so the copy just describes the
// kill itself.
func (m *Model) renderConfirmKill() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("kill tmux session"))
	b.WriteString("\n\n")
	if m.killTarget == "(main)" {
		b.WriteString("  Kill the project's main tmux session?\n\n")
		b.WriteString(subtleStyle.Render("  This shuts down the main session (panes, running scripts.run,\n"))
		b.WriteString(subtleStyle.Render("  claude/nvim processes). Your source repo and project state are\n"))
		b.WriteString(subtleStyle.Render("  untouched. Press Enter on the main row again to rebuild it\n"))
		b.WriteString(subtleStyle.Render("  (claude --continue keeps history).\n"))
	} else {
		b.WriteString(fmt.Sprintf("  Kill the tmux session for workspace %q?\n\n", m.killTarget))
		b.WriteString(subtleStyle.Render("  This shuts down the workspace's tmux session (panes, running\n"))
		b.WriteString(subtleStyle.Render("  scripts.run, claude/nvim processes). The worktree dir, the\n"))
		b.WriteString(subtleStyle.Render("  branch, and state.json all survive. Press Enter on the row\n"))
		b.WriteString(subtleStyle.Render("  again to resurrect the session (claude --continue keeps history).\n"))
	}
	b.WriteString("\n")
	b.WriteString("  ")
	b.WriteString(brokenStyle.Render("y"))
	b.WriteString(" to kill  ·  any other key to cancel")
	return b.String()
}

// renderDrawer is the diagnostic detail view for one workspace, opened
// with `i`. Read-only, scope-capped: process tree, recent log lines,
// env, last setup output, status. See model.go drawerMode docstring
// for the full scope cap.
func (m *Model) renderDrawer() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("inspect: %s", m.drawerRow.Name)))
	b.WriteString("\n\n")

	// Header line: branch, status, port, path.
	b.WriteString(fmt.Sprintf("  branch: %s\n", m.drawerRow.Branch))
	attached := "no"
	if m.drawerRow.Attached {
		attached = "yes (you're looking at this session)"
	}
	b.WriteString(fmt.Sprintf("  status: %s   tmux-alive: %v   tmux-attached: %s   port: %d\n",
		m.drawerRow.Status, m.drawerRow.Alive, attached, m.drawerRow.Port))
	if m.drawerRow.Path != "" {
		b.WriteString(fmt.Sprintf("  path:   %s\n", m.drawerRow.Path))
	}
	b.WriteString("\n")

	if m.drawerErr != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("load error: %v", m.drawerErr)))
		b.WriteString("\n\n")
	}

	// Process tree section.
	b.WriteString(subtleStyle.Render("─── processes ───"))
	b.WriteString("\n")
	if m.drawerProcInfo == "" {
		b.WriteString(subtleStyle.Render("  (loading…)\n"))
	} else {
		b.WriteString(m.drawerProcInfo)
		if !strings.HasSuffix(m.drawerProcInfo, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")

	// Recent log lines section. Workspace rows read per-workspace
	// log; main rows don't have one (clog fan-out keys on workspace
	// name) so we surface the limitation honestly rather than
	// pretending an empty section is "no log captured yet".
	b.WriteString(subtleStyle.Render("─── recent log entries ───"))
	b.WriteString("\n")
	switch {
	case m.drawerRow.IsMain:
		b.WriteString(subtleStyle.Render("  (main rows don't have per-session logs — main events go to ~/.canopy/log/canopy.log)\n"))
	case m.drawerLogTail == "":
		b.WriteString(subtleStyle.Render("  (no log captured yet — workspace logs land at ~/.canopy/log/canopy-" + m.drawerRow.Name + ".log)\n"))
	default:
		b.WriteString(m.drawerLogTail)
		if !strings.HasSuffix(m.drawerLogTail, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")

	// Setup log section. Main rows don't run scripts.setup, so this
	// section is N/A there.
	b.WriteString(subtleStyle.Render("─── last scripts.setup output ───"))
	b.WriteString("\n")
	switch {
	case m.drawerRow.IsMain:
		b.WriteString(subtleStyle.Render("  (N/A — main rows don't run scripts.setup)\n"))
	case m.drawerSetupLog == "":
		b.WriteString(subtleStyle.Render("  (no setup log captured)\n"))
	default:
		b.WriteString(m.drawerSetupLog)
		if !strings.HasSuffix(m.drawerSetupLog, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")

	b.WriteString(subtleStyle.Render("  "))
	b.WriteString(subtleStyle.Render("Esc/q "))
	b.WriteString(subtleStyle.Render("close  ·  "))
	// `b` only earns its slot when the row is broken (Enter would
	// refuse, so b is the only way to drop in and debug) or main
	// (a one-pane shell at project root with CANOPY env is a
	// distinct gesture from the 3-pane main session). For
	// everyday running/stopped workspaces, Enter does the right
	// thing and b would just spawn redundant clutter — so the
	// keybind stays available (handler still dispatches if you
	// somehow press it) but isn't advertised.
	if m.drawerRow.IsMain || m.drawerRow.Status == state.StatusBroken {
		b.WriteString(subtleStyle.Render("b "))
		if m.drawerRow.IsMain {
			b.WriteString(subtleStyle.Render("bare shell at project root  ·  "))
		} else {
			b.WriteString(subtleStyle.Render("bare attach (skip scripts.setup)  ·  "))
		}
	}
	b.WriteString(subtleStyle.Render("r "))
	b.WriteString(subtleStyle.Render("reload"))
	return b.String()
}

// renderBusyView is shown while a Create or Remove is in progress and
// immediately after it completes (so the user can see the captured
// output). While busy, it's a simple "working..." line; once done, it
// shows the success/error summary plus the captured output buffer.
func (m *Model) renderBusyView() string {
	var b strings.Builder
	// Only the Create flow carries a target-project banner — Remove and
	// Retry operate on a row in the existing list and don't open the
	// new-workspace flow that populates newTargetName.
	if m.busyOp == busyOpCreate {
		b.WriteString(m.renderTargetBanner())
	}
	b.WriteString(titleStyle.Render(m.busyTitle))
	b.WriteString("\n\n")

	if !m.busyDone {
		// Live output: scripts.setup writes to a buffer that the
		// progressTick drain into m.busyOutput every ~150ms. While
		// the buffer is empty (very early, before scripts produce
		// anything) we show the static hint; once output arrives it
		// streams here as it would in a regular shell.
		if m.busyOutput == "" {
			b.WriteString(subtleStyle.Render("  Working — this may take a few seconds while scripts.setup runs."))
			b.WriteString("\n\n")
			b.WriteString(subtleStyle.Render("  (The TUI is responsive; canopy is doing the heavy lifting in a goroutine.)"))
			return b.String()
		}
		// Tail the streaming output. lipgloss has no built-in tail
		// helper; we just print everything and rely on terminal
		// scrollback for long runs. Most scripts.setup output is
		// short (~20-100 lines).
		b.WriteString(m.busyOutput)
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render("  ...running"))
		return b.String()
	}

	// Done: show the result. Success message pivots on which operation
	// just finished — same view, three different verbs.
	if m.busyErr != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("Failed: %v", m.busyErr)))
	} else {
		b.WriteString(readyStyle.Render(busySuccessMessage(m.busyOp, m.newTargetName)))
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

// renderHelp draws the full keybind reference + visual legend
// (toggled with ?). Any key dismisses it back to the main view.
//
// The legend section is the load-bearing part — without it, the
// row's mix of glyphs (⊙ attach indicator, ⏸/✗/!/… status glyphs,
// caret) becomes a memory test. Reading the help once should be
// enough to scan rows without guessing what each shape means.
func (m *Model) renderHelp() string {
	lines := []string{
		titleStyle.Render("canopy — keybindings"),
		"",
		"  ↑/↓, j/k       move selection",
		"  g, home        first row",
		"  G, end         last row",
		"  tab            switch Local / Global tab",
		"  /              fuzzy search",
		"",
		"  enter          attach to selected (resurrects if stopped)",
		"  i              inspect — open diagnostic detail drawer",
		"  K              kill tmux session (state survives; Enter rebuilds)",
		"  n              new workspace",
		"  d              delete selected workspace (with confirmation)",
		"  R              re-run setup on a broken workspace",
		"  o              focus project (Global tab only)",
		"  P              open PR in browser (when row has PR hint)",
		"  B              open running app in browser (http://localhost:<port>)",
		"  r              refresh state (Mem column + upgrade-check cache)",
		"",
		"  U              upgrade canopy (only when an upgrade is available)",
		"  D              dismiss the 'upgrade available' pill until next release",
		"",
		"  ?              this help",
		"  q, ctrl-c      quit",
		"",
		titleStyle.Render("legend"),
		"",
		"  status column:",
		"    running        tmux session alive, processes running",
		"    stopped  ⏸     tmux session is dead (was killed or crashed)",
		"    broken   ✗     scripts.setup failed; press R to re-run setup",
		"    setting up  …  workspace is being created right now",
		"    orphaned !     workspace dir is missing on disk",
		"    not started    main session: no tmux session yet",
		"",
		"  row prefixes:",
		"    ❯              cursor is on this row",
		"    ⊙ (green)      tmux alive, a client is attached (you're looking at it)",
		"    ○ (grey)       tmux alive, no client attached (running in background)",
		"    (blank)        no tmux session — status column says why",
		"",
		"  agent badge (per workspace, polled every 2s):",
		"    ⚡ (cyan)       claude is thinking — response streaming or tool running",
		"    💤 (grey)       claude is idle — at the input prompt, ready for your next message",
		"    ✋ (yellow)     claude is awaiting input — y/N or tool-permission prompt blocking",
		"    · (subtle)     this workspace has no agent pane (shell-only)",
		"    (blank)        not polled yet, dead session, or main row",
		"",
		"  Right-side badges (when applicable):",
		"    ↻ rename       branch is auto-named — the agent should rename it",
		"    ↑3 ↓1 *5       git stats: 3 commits unpushed, 1 commit behind, 5 files dirty",
		"    PR open / approved / changes / merged / closed   GitHub PR state",
		"    ✓ merged       branch's commits are in main (canopy rm now safe)",
		"",
		"  Load column (Mem RSS + CPU%):",
		"    —              no data yet (probe still running, or dead session)",
		"    320M 0%        idle workspace (alive, processes mostly waiting)",
		"    320M 12%       memory + CPU (sum across panes; can exceed 100% on multi-core)",
		"    (amber)        > 500 MB OR > 50% CPU",
		"    (red)          > 2 GB OR > 200% CPU",
		"",
		subtleStyle.Render("Press any key to dismiss."),
	}
	return helpBodyStyle.Render(strings.Join(lines, "\n"))
}

// selectedHint returns the auto-diagnosis hint for the row currently
// under the cursor, but only when it's a broken row that has a hint
// captured. Empty otherwise so the caller can skip the whole hint
// line. Defensive against an empty rows slice.
func (m *Model) selectedHint() string {
	r, ok := m.list.CursorRow()
	if !ok {
		return ""
	}
	if r.IsMain || r.Status != "broken" || r.LastErrorHint == "" {
		return ""
	}
	return r.LastErrorHint
}

// busySuccessMessage maps a completed busy-mode op to the right success
// line. Kept as a small switch so the View stays declarative and so we
// can extend without touching renderBusyView again.
//
// For Create, projectName fills in the target project so the success
// line mirrors the banner's promise ("creating in cravd" → "Workspace
// created in cravd"). Empty projectName falls back to the legacy
// generic line so older code paths and tests keep working.
func busySuccessMessage(op busyOpKind, projectName string) string {
	switch op {
	case busyOpRemove:
		return "Workspace removed."
	case busyOpRetry:
		return "Workspace recovered — scripts.setup re-ran cleanly."
	case busyOpCreate:
		if projectName != "" {
			return "Workspace created in " + projectName + "."
		}
		return "Workspace created successfully."
	}
	return "Done."
}

// statusStyle, statusCell, liveBadge, portCell, maxInt all live in
// render.go now (shared with the global TUI + projectlist sub-component).
