// Tab, cursor, and search-mode handlers. Carved out of update.go so
// the listMode chrome (left/right/h/l/tab/shift+tab, j/k/g/G, `/`)
// lives in one place instead of interleaved with workspace verbs.

package ui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// actionTabSwitch flips Local ↔ Global. The new filtered set is pushed
// to projectlist via SetRows; projectlist clamps its cursor automatically
// so a long-list scroll position from the previous tab doesn't carry
// over past the end of the new tab.
//
// Tab key uses the same "next" cycle as right/l (v0.17.0 Phase 1c
// polish unified the three forward-cycle keys behind one helper).
func actionTabSwitch(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	return actionTabNext(m, msg)
}

// actionTabNext cycles forward through the visible tabs. Tab order is
// contextual (v0.17 Phase 1h):
//
//	inside a project: <project> → Projects → Hosts → wrap
//	outside (no currentProject): Projects → Hosts → wrap
//
// tabLocal is only reachable when m.currentProject is set — without a
// project context it has nothing to filter to, so it isn't part of the
// cycle at all. Hosts is similarly skipped when no hosts are registered.
//
// Bound to Tab AND `right`/`l`.
func actionTabNext(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	tabs := m.visibleTabs()
	m.tab = nextInCycle(tabs, m.tab, +1)
	m.list.SetRows(m.filteredRows())
	return m, nil
}

// actionTabPrev cycles backward through the visible tabs. Bound to
// `left`/`h` and shift+tab.
func actionTabPrev(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	tabs := m.visibleTabs()
	m.tab = nextInCycle(tabs, m.tab, -1)
	m.list.SetRows(m.filteredRows())
	return m, nil
}

// visibleTabs returns the tabs the user can cycle through right now.
// tabLocal only appears when a project context exists; tabHosts only
// when at least one host is registered. tabGlobal is always present.
func (m *Model) visibleTabs() []tabKind {
	// Pinned thin-client mode (`canopy --remote <host>`, v0.22): there's
	// no local project and the Hosts fleet-management tab doesn't apply
	// — the whole point is a single pinned host's workspaces. See
	// NewRemotePinned.
	if m.pinnedHost.Name != "" {
		return []tabKind{tabGlobal}
	}
	out := make([]tabKind, 0, 3)
	if m.currentProject != "" {
		out = append(out, tabLocal)
	}
	out = append(out, tabGlobal)
	if m.hostsHasEntries() {
		out = append(out, tabHosts)
	}
	return out
}

// nextInCycle returns the tab `step` positions away from current in
// the cycle (step=+1 for next, -1 for prev). If `current` isn't in the
// cycle (e.g., user was on tabLocal but currentProject just became
// empty — defensive case, shouldn't happen) we land on the first
// visible tab.
func nextInCycle(tabs []tabKind, current tabKind, step int) tabKind {
	if len(tabs) == 0 {
		return current
	}
	idx := -1
	for i, t := range tabs {
		if t == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		return tabs[0]
	}
	n := len(tabs)
	return tabs[((idx+step)%n+n)%n]
}

// Cursor-nav actions forward to projectlist's Update so it can clamp
// the cursor against its own row count. Bubbletea's Update returns
// (Model, tea.Cmd); projectlist returns (Model value, tea.Cmd) so we
// reassign m.list with the returned value.
//
// Hosts tab uses its own m.hostsCursor (independent of projectlist's
// internal cursor) so the two tabs navigate independently. v0.17
// Phase 1l.
func actionCursorUp(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.tab == tabHosts {
		if m.hostsCursor > 0 {
			m.hostsCursor--
		}
		return m, nil
	}
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

func actionCursorDown(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.tab == tabHosts {
		maxIdx := len(m.hostList) - 1
		if m.hostsCursor < maxIdx {
			m.hostsCursor++
		}
		return m, nil
	}
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

func actionCursorTop(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

func actionCursorBottom(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

// actionToggleIdleExpand forwards `e` to projectlist's Update so it can
// flip the current host's idle-rollup expansion. Thin wrapper — the
// per-host bookkeeping lives in projectlist.Model.idleExpanded; the
// parent just relays the key.
func actionToggleIdleExpand(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

// actionSearchEntry enters fuzzy-search mode. Subsequent keystrokes are
// captured into searchQuery via handleSearchKey (which the search-mode
// bypass at the top of handleKey routes to).
func actionSearchEntry(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.searchMode = true
	m.searchQuery = ""
	return m, nil
}

// handleSearchKey handles keystrokes while m.searchMode is true.
// Each query mutation pushes a fresh filtered set to projectlist so
// the user sees results live as they type.
func (m *Model) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.searchMode = false
		m.searchQuery = ""
		m.list.SetRows(m.filteredRows())
		return m, nil
	case tea.KeyEnter:
		// Enter exits search mode keeping the query, so arrow nav
		// works on the filtered list.
		m.searchMode = false
		return m, nil
	case tea.KeyBackspace:
		if len(m.searchQuery) > 0 {
			runes := []rune(m.searchQuery)
			m.searchQuery = string(runes[:len(runes)-1])
			m.list.SetRows(m.filteredRows())
		}
		return m, nil
	case tea.KeyRunes:
		m.searchQuery += string(msg.Runes)
		m.list.SetRows(m.filteredRows())
		return m, nil
	case tea.KeySpace:
		m.searchQuery += " "
		m.list.SetRows(m.filteredRows())
		return m, nil
	}
	return m, nil
}
