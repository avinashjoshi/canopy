// Keybinds for the unified TUI's top-level list mode.
//
// Bubbletea ships `bubbles/key.Binding` for key + help + enabled-state
// tracking, and `bubbles/help.Bubble` to render help lines from a slice of
// bindings. We wrap `key.Binding` with an Action func so the same data
// structure that powers help rendering also drives the dispatch.
//
// Two important properties:
//
//  1. Help-line rendering reads SetEnabled(): a disabled binding is hidden
//     from help (no flicker, no inactive-key noise). This is how `n` is
//     hidden when the cursor is on the Global tab — it's not a special-case
//     in the renderer, it's a property of the binding.
//
//  2. Key matching uses bubbles/key.Matches, which checks both the key
//     literal AND the enabled state. A pressed key against a disabled
//     binding does not fire. The Update path is therefore "match against
//     bindings → fire Action → fall through to projectlist if nothing
//     matched", which is what the eng review required (see CEO plan
//     "Key routing" section).
//
// viewMode-specific handlers (newPicker, newFresh, newPR/Issue/Branch,
// confirmDelete, busy) stay as their own methods — they're in-modal
// navigation, not top-level. The Binding mechanism is for listMode keys.

package ui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// Binding is the unified TUI's keybinding shape. K provides the key
// literals, help text, and enabled-state tracking from bubbles/key.
// Action runs when the key matches AND K.Enabled() is true.
//
// Action returns (tea.Model, tea.Cmd) rather than just tea.Cmd because
// canopy's existing handler shape mutates the Model and may need to
// return tea.Quit, tea.Batch, or a custom message. Wrapping with a
// uniform signature would force every handler through an awkward
// closure dance for what's already idiomatic Bubbletea.
type Binding struct {
	K      key.Binding
	Action func(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd)
}

// Matches reports whether the pressed key is one of K's keys AND K is
// currently enabled. Thin wrapper around bubbles/key.Matches that exists
// so callers don't have to import bubbles/key just to check.
func (b Binding) Matches(msg tea.KeyMsg) bool {
	return key.Matches(msg, b.K)
}

// HelpKey returns the binding's K so a future bubbles/help.Bubble can
// pull help text out without callers reaching into the struct field.
// Methodized to keep K's exported-ness as an implementation detail
// callers don't need to know about.
func (b Binding) HelpKey() key.Binding { return b.K }
