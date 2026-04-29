package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/oncactus/canopy/internal/ghx"
	"github.com/oncactus/canopy/internal/state"
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
	case newPRMode:
		return m.renderNewPR()
	case newIssueMode:
		return m.renderNewIssue()
	case newBranchMode:
		return m.renderNewBranch()
	case confirmDeleteMode:
		return m.renderConfirmDelete()
	case busyMode:
		return m.renderBusyView()
	}

	if m.mode == confirmRetryMode {
		return m.renderConfirmRetry()
	}

	var b strings.Builder
	// Title: just "canopy" — the tab bar tells the user what scope
	// they're in (Local: <project> vs Global), so a project subtitle
	// in the title would be redundant chrome.
	b.WriteString(titleStyle.Render("canopy"))
	b.WriteString("\n\n")

	// Tab bar + search-line always at the top.
	b.WriteString(m.renderTabBar())
	b.WriteString("    ")
	b.WriteString(m.renderSearchLine())
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("error: %v", m.err)))
		b.WriteString("\n\n")
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
			b.WriteString(subtleStyle.Render("press R to retry scripts.setup against the existing worktree"))
			b.WriteString("\n\n")
		}
	}

	b.WriteString(m.renderHelpLine())
	return b.String()
}

// renderTabBar draws the Local/Global tab bar. Single-line in popup mode,
// two-line in fullscreen. The active tab is bracketed; the inactive tab
// is plain. An empty tab dims its label regardless of active/inactive
// state — empty-tab dim is independent of popup-mode dim, both can apply.
func (m *Model) renderTabBar() string {
	localCount := 0
	globalCount := 0
	for _, r := range m.allRowsOrFallback() {
		globalCount++
		if m.currentProject != "" && r.ProjectRoot == m.currentProject {
			localCount++
		}
	}

	localLabel := "Local"
	if m.currentProject != "" {
		// Show the project name so the user knows what "Local" means in
		// this invocation. Truncate aggressively for narrow popups.
		proj := m.currentProject
		// filepath.Base would import filepath; cheap inline.
		for i := len(proj) - 1; i >= 0; i-- {
			if proj[i] == '/' {
				proj = proj[i+1:]
				break
			}
		}
		if len(proj) > 16 {
			proj = proj[:16]
		}
		localLabel = "Local: " + proj
	}

	var local, global string
	if m.tab == tabLocal {
		if localCount == 0 {
			local = activeTabStyle.Render("[ " + localLabel + " ]")
			local = subtleStyle.Render(local)
		} else {
			local = activeTabStyle.Render("[ " + localLabel + " ]")
		}
		if globalCount == 0 {
			global = subtleStyle.Render("  Global  ")
		} else {
			global = inactiveTabStyle.Render("  Global  ")
		}
	} else {
		if localCount == 0 {
			local = subtleStyle.Render("  " + localLabel + "  ")
		} else {
			local = inactiveTabStyle.Render("  " + localLabel + "  ")
		}
		global = activeTabStyle.Render("[ Global ]")
	}
	return local + " " + global
}

// renderSearchLine returns the search input box (when in search mode) or
// a static "filter: ..." indicator when a query is active but the user
// has exited search mode.
func (m *Model) renderSearchLine() string {
	if m.searchMode {
		return searchActiveStyle.Render("/" + m.searchQuery + "█")
	}
	if m.searchQuery != "" {
		return subtleStyle.Render("filter: " + m.searchQuery + "  (esc to clear)")
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

// renderHelpLine is the one-line keybind cheat at the bottom of the
// main view. Driven by the listModeBindings table — bindings whose
// Available returns false are filtered out, so e.g. `n` disappears
// from help when the user is on the Global tab without a Manager.
//
// Format: each binding renders as "<keys> <help>" joined by a separator.
// Up/down/g/G are folded into a single "↑/↓ navigate" entry — they're
// muscle-memory keys whose individual help would just be noise.
func (m *Model) renderHelpLine() string {
	parts := []string{"↑/↓ navigate"}
	// Skip the four cursor-nav bindings (handled by the static
	// "↑/↓ navigate" entry above) so we don't double-render them.
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
		parts = append(parts, h.Key+" "+h.Desc)
	}
	return subtleStyle.Render(strings.Join(parts, "  ·  "))
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
	b.WriteString(titleStyle.Render("canopy new") + " " + subtleStyle.Render(m.projectName))
	b.WriteString("\n\n")
	b.WriteString("  How do you want to start?\n\n")

	for i, opt := range newPickerOptions {
		cursor := "    "
		if i == m.newPickerCursor {
			cursor = "  > "
		}
		// Letter shortcut + label, then a dim description on the
		// next line. Two-line entries give the picker breathing
		// room and let the description carry the "why this option"
		// without bloating the headline.
		b.WriteString(cursor)
		b.WriteString(brokenStyle.Render("[" + opt.key + "] "))
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
	{"p", "From a pull request", "check out a PR's branch (uses gh)"},
	{"i", "From an issue", "implement work from an issue (uses gh)"},
	{"b", "From a branch", "check out an existing branch"},
}

// renderNewFresh is step 2a — name input for the fresh-workspace
// path. Same shape as the v0 modal that the picker replaced; this
// is the simple/common case that most `n` presses end at.
func (m *Model) renderNewFresh() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("canopy new"))
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
	b.WriteString(titleStyle.Render("canopy new"))
	b.WriteString(" ")
	b.WriteString(subtleStyle.Render("· pull request"))
	b.WriteString("\n\n")

	b.WriteString("  PR number or filter:\n  ")
	b.WriteString(m.listInput.View())
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
					marker = "  ● "
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
	b.WriteString(titleStyle.Render("canopy new"))
	b.WriteString(" ")
	b.WriteString(subtleStyle.Render("· issue"))
	b.WriteString("\n\n")

	b.WriteString("  Issue number or filter:\n  ")
	b.WriteString(m.listInput.View())
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
					marker = "  ● "
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
	b.WriteString(titleStyle.Render("canopy new"))
	b.WriteString(" ")
	b.WriteString(subtleStyle.Render("· branch"))
	b.WriteString("\n\n")

	b.WriteString("  Filter:\n  ")
	b.WriteString(m.listInput.View())
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
					marker = "  ● "
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

// renderBusyView is shown while a Create or Remove is in progress and
// immediately after it completes (so the user can see the captured
// output). While busy, it's a simple "working..." line; once done, it
// shows the success/error summary plus the captured output buffer.
func (m *Model) renderBusyView() string {
	var b strings.Builder
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
