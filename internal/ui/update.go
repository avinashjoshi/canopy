package ui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Update implements tea.Model. Routes incoming messages to focused
// handlers. The Model is always returned by value — Bubbletea owns the
// "current" Model, we own the next one.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case rowsLoadedMsg:
		// Refresh result. Apply rows; clamp the cursor if the list shrank.
		m.err = msg.err
		if msg.rows != nil {
			m.rows = msg.rows
		}
		if m.cursor >= len(m.rows) {
			m.cursor = max0(len(m.rows) - 1)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey is the keymap. Conductor-flavored: small, opinionated, no
// clever chords. Help is one keypress (?), nav is the standard
// arrow/jk/gG, attach is enter, refresh is r, quit is q.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showHelp {
		// Any key dismisses help.
		m.showHelp = false
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "?":
		m.showHelp = true
		return m, nil

	case "r":
		// Manual refresh. Same flow as the initial load.
		return m, refreshCmd(m.mgr, m.tc)

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
	}

	return m, nil
}

// max0 returns max(0, n). Avoids the Bubbletea-standard generic max
// for first-time-Go simplicity (and Go 1.21+ has built-in max but
// we're staying close to the old toolchain to keep the dep surface
// boring).
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
