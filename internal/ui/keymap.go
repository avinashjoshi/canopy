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
		// v0.17.0 Phase 1c polish: left/h prev-tab, right/l next-tab.
		// Mirrors j/k for up/down so the keymap stays vim-ergonomic.
		// Single help entry covers both bindings; rendering is in the
		// tab bar (← Local | Global | Hosts →) so the directionality
		// is visible at a glance.
		K:      key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "next tab")),
		Action: actionTabNext,
	},
	{
		// shift+tab is the standard "reverse tab" idiom across nearly
		// every TUI / form library. Aliased onto actionTabPrev so it
		// composes with ← / h as one consistent prev-tab affordance.
		K:      key.NewBinding(key.WithKeys("left", "h", "shift+tab"), key.WithHelp("←/h/⇧⇥", "prev tab")),
		Action: actionTabPrev,
	},
	{
		K:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Action: actionSearchEntry,
	},
	{
		K:         key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "attach")),
		Available: availableInWorkspaceContext, // Hosts tab enter handled separately
		Action:    actionAttach,
	},
	{
		// Hosts tab: enter on a selected host runs `canopy host show
		// <name>` semantics in-TUI — for v0.17 Phase 1l just opens the
		// detail drawer-ish surface (a follow-up).
		K:         key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "host detail")),
		Available: availableOnHostsTab,
		Action:    actionHostEnter,
	},
	{
		K:         key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Available: availableNewWorkspace,
		Action:    actionNewWorkspace,
	},
	{
		// P (capital) opens the cursor row's PR in the browser. Lower-
		// case p is unbound: a stray `p` press used to fire an external
		// `gh` invocation, which made the lowercase key feel hostile to
		// the same muscle memory that uses k for cursor-up. Shift-key
		// friction matches K (kill) and B (browser) — destructive or
		// side-effecting verbs require a deliberate keypress.
		K:         key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "open PR")),
		Available: availableOpenPR,
		Action:    actionOpenPR,
	},
	{
		// B opens the workspace's running app in the user's default
		// browser at http://localhost:<row.Port>. Requires a live tmux
		// session and a non-zero port — pressing B on a stopped or
		// portless row would either 404 or open a port that some other
		// process now owns, both worse than silently hiding the binding.
		K:         key.NewBinding(key.WithKeys("B"), key.WithHelp("B", "open browser")),
		Available: availableOpenBrowser,
		Action:    actionOpenBrowser,
	},
	{
		// d on Hosts tab deletes the cursor's host from the registry;
		// elsewhere it deletes a workspace. Single key, tab-aware
		// dispatch via actionDeleteRouter — both bindings exist so the
		// help line surfaces the right verb on the right tab.
		K:         key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Available: availableInWorkspaceContext,
		Action:    actionDelete,
	},
	{
		K:         key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "remove host")),
		Available: availableOnHostsTab,
		Action:    actionHostRemove,
	},
	{
		// K (capital) kills the workspace's tmux session without
		// removing state. Lower-case k is cursor-up, intentional —
		// the muscle-memory case is nav, the deliberate-keypress
		// case is destructive. Same shift-key friction as F (force-
		// remove). Re-pressing Enter after kill resurrects.
		K:         key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "kill tmux")),
		Available: availableInWorkspaceContext,
		Action:    actionKill,
	},
	{
		// `i` opens the diagnostic detail drawer for the selected
		// workspace. Read-only; scope-capped to "what's the state
		// of this one workspace right now?". See drawerMode docstring
		// in model.go for the load-bearing scope cap rationale.
		K:         key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "inspect")),
		Available: availableInWorkspaceContext,
		Action:    actionInspect,
	},
	{
		K:         key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "retry")),
		Available: availableInWorkspaceContext,
		Action:    actionRetry,
	},
	{
		K:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Action: actionRefresh,
	},
	{
		// U opens the in-TUI canopy upgrade flow. Only bound when an
		// upgrade is available and the closures are wired (via
		// availableUpgrade); pressing U otherwise is silently
		// ignored. The flow is full-screen and owned by upgradeMode.
		K:         key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "upgrade")),
		Available: availableUpgrade,
		Action:    actionUpgrade,
	},
	{
		// D dismisses the current "upgrade available" pill. Only
		// bound when an upgrade is available + dismissal closure
		// wired (via availableDismissUpgrade). Lowercase d is
		// already taken by delete; capital D matches the convention
		// of K-for-kill being the "deliberate keypress" case.
		K:         key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "dismiss upgrade")),
		Available: availableDismissUpgrade,
		Action:    actionDismissUpgrade,
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

// availableNewWorkspace is the Available predicate for `n`. Two paths:
//
//   - Local tab: needs m.mgr (the current-project Manager from canopy.json
//     walk-up). The classic case — `n` operates on the launch-context
//     project.
//   - Global tab: needs a cursor row with a non-empty ProjectRoot. The
//     row's project becomes the target; managerForRow resolves it lazily
//     at action-time. Mirrors how d/R/K already behave for cross-project
//     verbs.
//
// We deliberately don't load canopy.json here — that's a syscall. The
// availability check stays cheap (struct-field reads); the action-time
// resolver surfaces any load failure via m.err so the user sees a
// status-line hint instead of silent disabling.
func availableNewWorkspace(m *Model) bool {
	if m.tab == tabLocal {
		return m.mgr != nil
	}
	if m.tab == tabHosts {
		// Hosts tab: `n` opens the add-host wizard.
		return true
	}
	row, ok := m.list.CursorRow()
	if !ok {
		return false
	}
	// Remote row: `n` dispatches to `canopy new --on <host>`. Local row:
	// need a non-empty ProjectRoot so managerForRow can resolve.
	return row.Host != "" || row.ProjectRoot != ""
}

// availableOpenPR is the Available predicate for `P`. Hidden when the
// cursor row has no pr_status hint — `gh pr view --web` would error
// out with "no pull requests found" otherwise. Skips main rows
// (their branch is the project default; opening the "PR" for main
// makes no sense). Workspace rows with the hint show the binding.
func availableOpenPR(m *Model) bool {
	row, ok := m.list.CursorRow()
	if !ok || row.IsMain || row.Path == "" {
		return false
	}
	for _, h := range row.Hints {
		if h.Kind == "pr_status" {
			return true
		}
	}
	return false
}

// availableOpenBrowser is the Available predicate for `B`. Requires a
// live session (Alive) and a non-zero Port. Both main and workspace
// rows qualify — `scripts.run` exposes a server on CANOPY_PORT in
// either context, and the user's intent ("show me my running app") is
// the same regardless of row kind.
func availableOpenBrowser(m *Model) bool {
	row, ok := m.list.CursorRow()
	if !ok {
		return false
	}
	return row.Alive && row.Port > 0
}

// availableInWorkspaceContext is the predicate for verbs that act on
// the workspace list (d/K/i/R/B/P/enter). Hidden on the Hosts tab so
// pressing them there doesn't accidentally fire against whatever
// workspace cursor row is stale. v0.17 Phase 1l.
func availableInWorkspaceContext(m *Model) bool {
	return m.tab != tabHosts
}

// availableOnHostsTab is the inverse — gates host-specific verbs to
// only fire while the Hosts tab is active.
func availableOnHostsTab(m *Model) bool {
	return m.tab == tabHosts && len(m.hostList) > 0
}

// availableOpenPR and availableOpenBrowser already short-circuit when
// the cursor row has no Path or no Port; both implicitly disqualify
// the Hosts tab (no cursor row). The explicit gate above is for the
// other verbs whose predicates would otherwise pass through.

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
