// Keybinds for the unified TUI's top-level list mode.
//
// Bubbletea ships `bubbles/key.Binding` for key + help + enabled-state
// tracking, and `bubbles/help` to render help lines from a slice of
// bindings. We wrap `key.Binding` with an Action func so the same data
// structure that powers help rendering also drives the dispatch.
//
// Two important properties:
//
//  1. Help-line rendering filters by Available(): a binding whose
//     Available returns false is hidden from help (no flicker, no
//     inactive-key noise). This is how `n` is hidden when the cursor
//     is on the Global tab — it's not a special-case in the renderer,
//     it's a property of the binding.
//
//  2. Key matching uses bubbles/key.Matches AND the Available check:
//     a pressed key against a disabled binding does not fire. The
//     Update path is "iterate bindings; first match-and-available
//     fires its Action; fall through to projectlist if nothing
//     matched."
//
// viewMode-specific handlers (newPicker, newFresh, newPR/Issue/Branch,
// confirmDelete, confirmRetry, busy) stay as their own methods —
// they're in-modal navigation, not top-level. The Binding mechanism
// is for listMode keys.

package ui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// Binding is the unified TUI's keybinding shape. K provides the key
// literals + help text; Available is the runtime visibility predicate;
// Action runs when the key matches AND Available is true (or nil).
//
// Action returns (tea.Model, tea.Cmd) rather than just tea.Cmd because
// canopy's existing handler shape mutates the Model and may need to
// return tea.Quit, tea.Batch, or a custom message. Wrapping with a
// uniform single-return signature would force every handler through
// an awkward closure dance for what's already idiomatic Bubbletea.
type Binding struct {
	K         key.Binding
	Available func(*Model) bool
	Action    func(*Model, tea.KeyMsg) (tea.Model, tea.Cmd)
}

// Matches reports whether the pressed key is one of K's keys AND the
// binding is currently available. nil Available means "always
// available" — most bindings.
func (b Binding) Matches(msg tea.KeyMsg, m *Model) bool {
	if b.Available != nil && !b.Available(m) {
		return false
	}
	return key.Matches(msg, b.K)
}

// IsAvailable returns Available's result, or true when Available is nil.
// Used by the help-line renderer to filter unavailable bindings out.
func (b Binding) IsAvailable(m *Model) bool {
	if b.Available == nil {
		return true
	}
	return b.Available(m)
}

// listModeBindings is the source of truth for the unified TUI's
// top-level (listMode) keymap. Order matters for help-line rendering:
// bindings are shown left-to-right in this order.
//
// Order is deliberate: nav first (most muscle-memory), then tab/search
// (the v0.8 unification verbs), then attach (the primary action), then
// destructive verbs (n/d/R), then meta (refresh/help/quit).
var listModeBindings = []Binding{
	{
		K:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Action: actionCursorUp,
	},
	{
		K:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Action: actionCursorDown,
	},
	{
		K:      key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "first")),
		Action: actionCursorTop,
	},
	{
		K:      key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "last")),
		Action: actionCursorBottom,
	},
	{
		K:      key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch-tab")),
		Action: actionTabSwitch,
	},
	{
		K:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Action: actionSearchEntry,
	},
	{
		K:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "attach")),
		Action: actionAttach,
	},
	{
		K:         key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Available: availableNewWorkspace,
		Action:    actionNewWorkspace,
	},
	{
		K:         key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "focus project")),
		Available: availableFocusProject,
		Action:    actionFocusProject,
	},
	{
		K:      key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Action: actionDelete,
	},
	{
		K:      key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "retry")),
		Action: actionRetry,
	},
	{
		K:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Action: actionRefresh,
	},
	{
		K:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Action: actionHelpToggle,
	},
	{
		K:      key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Action: actionQuit,
	},
}

// availableNewWorkspace is the Available predicate for `n`. n is hidden
// (and disabled) when the user is on the Global tab OR has no current-
// project Manager — both cases lack the canopy.json walk-up that n
// requires.
//
// CP4 / D6 asymmetry: d/R work cross-project (their Available is nil =
// always available); n is local-only because it operates on cwd, not
// on the cursor row's project.
func availableNewWorkspace(m *Model) bool {
	return m.mgr != nil && m.tab == tabLocal
}

// availableFocusProject is the Available predicate for `o`. Only
// meaningful on the Global tab (Local already IS the current project's
// scope; pressing o there would be a no-op). Hidden when the cursor
// row's ProjectRoot equals the current focus, since refocusing onto
// the same project is also a no-op.
func availableFocusProject(m *Model) bool {
	if m.tab != tabGlobal {
		return false
	}
	row, ok := m.list.CursorRow()
	if !ok {
		return false
	}
	if row.ProjectRoot == "" {
		return false
	}
	return row.ProjectRoot != m.currentProject
}

// availableShortHelp returns the bindings that pass IsAvailable, in
// declared order. This is what bubbles/help.Bubble's ShortHelp consumes
// to render the bottom help line.
func availableShortHelp(m *Model) []key.Binding {
	out := make([]key.Binding, 0, len(listModeBindings))
	for _, b := range listModeBindings {
		if b.IsAvailable(m) {
			out = append(out, b.K)
		}
	}
	return out
}
