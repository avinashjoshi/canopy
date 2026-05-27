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
	"strings"

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
	K key.Binding
	// Group is the logical bucket renderHelpLine uses for wrap decisions.
	// One of: "nav", "tabs", "open", "act", "meta". Empty groups land in
	// "act" by default. The renderer keeps a group's chips together on
	// one line when they fit; if a group is wider than the screen, it
	// falls back to wrapping chips inside the group.
	Group     string
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
		Group:  "nav",
		Action: actionCursorUp,
	},
	{
		K:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Group:  "nav",
		Action: actionCursorDown,
	},
	{
		K:      key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "first")),
		Group:  "nav",
		Action: actionCursorTop,
	},
	{
		K:      key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "last")),
		Group:  "nav",
		Action: actionCursorBottom,
	},
	{
		K:      key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch-tab")),
		Group:  "tabs",
		Action: actionTabSwitch,
	},
	{
		// v0.17.0 Phase 1c polish: left/h prev-tab, right/l next-tab.
		// Mirrors j/k for up/down so the keymap stays vim-ergonomic.
		// Single help entry covers both bindings; rendering is in the
		// tab bar (← Local | Global | Hosts →) so the directionality
		// is visible at a glance.
		K:      key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "next tab")),
		Group:  "tabs",
		Action: actionTabNext,
	},
	{
		// shift+tab is the standard "reverse tab" idiom across nearly
		// every TUI / form library. Aliased onto actionTabPrev so it
		// composes with ← / h as one consistent prev-tab affordance.
		K:      key.NewBinding(key.WithKeys("left", "h", "shift+tab"), key.WithHelp("←/h/⇧⇥", "prev tab")),
		Group:  "tabs",
		Action: actionTabPrev,
	},
	{
		K:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Group:  "tabs",
		Action: actionSearchEntry,
	},
	{
		K:         key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "attach")),
		Group:     "open",
		Available: availableInWorkspaceContext, // Hosts tab enter handled separately
		Action:    actionAttach,
	},
	{
		// Hosts tab: enter on a selected host runs `canopy host show
		// <name>` semantics in-TUI — for v0.17 Phase 1l just opens the
		// detail drawer-ish surface (a follow-up).
		K:         key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "host detail")),
		Group:     "open",
		Available: availableOnHostsTab,
		Action:    actionHostEnter,
	},
	{
		K:         key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Group:     "act",
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
		Group:     "act",
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
		Group:     "act",
		Available: availableOpenBrowser,
		Action:    actionOpenBrowser,
	},
	{
		// d on Hosts tab deletes the cursor's host from the registry;
		// elsewhere it deletes a workspace. Single key, tab-aware
		// dispatch via actionDeleteRouter — both bindings exist so the
		// help line surfaces the right verb on the right tab.
		K:         key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Group:     "act",
		Available: availableInWorkspaceContext,
		Action:    actionDelete,
	},
	{
		K:         key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "remove host")),
		Group:     "act",
		Available: availableOnHostsTab,
		Action:    actionHostRemove,
	},
	{
		// `a` on a host with status=auth-failed offers ssh-copy-id.
		// Lets the user recover from an earlier "skip" without
		// re-adding the host. v0.17 Phase 1l polish.
		K:         key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "set up auth")),
		Group:     "act",
		Available: availableHostAuth,
		Action:    actionHostSetupAuth,
	},
	{
		// `a` on Local/Global tabs opens the Add Project form. The
		// host-tab `a` (above) wins when both bindings match because
		// availableHostAuth gates it to a host row; for non-host
		// tabs, availableAddProject fires this. v0.18.
		K:         key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add project")),
		Group:     "act",
		Available: availableAddProject,
		Action:    actionAddProject,
	},
	{
		// `,` opens the settings modal (v0.18 Phase D1). Top-level
		// entry point for editing source-root without going through
		// Add Project + ctrl+s. Comma is unused elsewhere in canopy's
		// keymap; ergonomic on most layouts. Tab-agnostic.
		K:      key.NewBinding(key.WithKeys(","), key.WithHelp(",", "settings")),
		Group:  "meta",
		Action: actionOpenSettings,
	},
	{
		// `s` on the Hosts tab opens an interactive SSH session to the
		// cursor's host. Drops the user into a shell on the remote;
		// closing the shell returns to the TUI.
		K:         key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "ssh")),
		Group:     "act",
		Available: availableHostSSH,
		Action:    actionHostSSH,
	},
	{
		// `c` on the Hosts tab installs (or repairs) the v0.18 clipboard
		// bridge on the cursor's host. Runs `canopy host clipboard <name>`
		// via tea.ExecProcess so the user sees the install transcript
		// inline. Confirm modal first to match the `s` key's friction.
		K:         key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "clipboard bridge")),
		Group:     "act",
		Available: availableHostClipboard,
		Action:    actionHostClipboard,
	},
	{
		// K (capital) kills the workspace's tmux session without
		// removing state. Lower-case k is cursor-up, intentional —
		// the muscle-memory case is nav, the deliberate-keypress
		// case is destructive. Same shift-key friction as F (force-
		// remove). Re-pressing Enter after kill resurrects.
		K:         key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "kill tmux")),
		Group:     "act",
		Available: availableInWorkspaceContext,
		Action:    actionKill,
	},
	{
		// `i` opens the diagnostic detail drawer for the selected
		// workspace. Read-only; scope-capped to "what's the state
		// of this one workspace right now?". See drawerMode docstring
		// in model.go for the load-bearing scope cap rationale.
		K:         key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "inspect")),
		Group:     "act",
		Available: availableInWorkspaceContext,
		Action:    actionInspect,
	},
	{
		K:         key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "retry")),
		Group:     "act",
		Available: availableInWorkspaceContext,
		Action:    actionRetry,
	},
	{
		K:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Group:  "meta",
		Action: actionRefresh,
	},
	{
		// U opens the in-TUI canopy upgrade flow. Only bound when an
		// upgrade is available and the closures are wired (via
		// availableUpgrade); pressing U otherwise is silently
		// ignored. The flow is full-screen and owned by upgradeMode.
		// Tab-gated to non-Hosts contexts so it doesn't collide with
		// the per-host upgrade dispatch below.
		K:         key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "upgrade")),
		Group:     "meta",
		Available: availableLocalUpgrade,
		Action:    actionUpgrade,
	},
	{
		// U on Hosts tab triggers `canopy upgrade --yes` on the
		// cursor's host over SSH. Output streams into the TUI
		// (no flicker, errors stay on screen). Available whenever
		// the selected host reported a non-dev version on its most
		// recent successful refresh — see availableHostUpgrade.
		K:         key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "upgrade host")),
		Group:     "meta",
		Available: availableHostUpgrade,
		Action:    actionHostUpgrade,
	},
	{
		// S on Hosts tab triggers `canopy use release` on the
		// cursor's host — the recovery path for hosts running a
		// dev binary, where `canopy upgrade` would refuse. Same
		// streaming machinery as U; differs only in the remote
		// command + labels. Hidden on release-binary hosts (where
		// the call would be a no-op).
		K:         key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "switch to release")),
		Group:     "meta",
		Available: availableHostSwitchRelease,
		Action:    actionHostSwitchRelease,
	},
	{
		// I on Hosts tab installs (or reinstalls) canopy on the
		// cursor's host. Reuses the same SSH-streaming machinery as
		// U/S; the remote command is install.sh piped to bash with
		// --yes. The flow is idempotent: on a host that already has
		// canopy, install.sh prints "already installed" and exits 0,
		// so pressing I on a healthy host is safe.
		//
		// Always available on Hosts tab — install is the recovery
		// path for `StatusBroken` hosts AND a no-op reinstall for
		// healthy ones. Per-status gating would hide it from the
		// hosts most in need (the broken ones).
		K:         key.NewBinding(key.WithKeys("I"), key.WithHelp("I", "install canopy")),
		Group:     "meta",
		Available: availableOnHostsTab,
		Action:    actionHostInstall,
	},
	{
		// D dismisses the current "upgrade available" pill. Only
		// bound when an upgrade is available + dismissal closure
		// wired (via availableDismissUpgrade). Lowercase d is
		// already taken by delete; capital D matches the convention
		// of K-for-kill being the "deliberate keypress" case.
		K:         key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "dismiss upgrade")),
		Group:     "meta",
		Available: availableDismissUpgrade,
		Action:    actionDismissUpgrade,
	},
	{
		K:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Group:  "meta",
		Action: actionHelpToggle,
	},
	{
		K:      key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Group:  "meta",
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
	if !ok || row.Loading {
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
// the same regardless of row kind. Gated off the Hosts tab — there's
// no port to forward for a host row.
func availableOpenBrowser(m *Model) bool {
	if m.tab == tabHosts {
		return false
	}
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

// availableAddProject gates the v0.18 Add Project keybind to non-Hosts
// tabs AND to TUI invocations where main() wired the RunInitFunc
// callback. Without the callback the form has no way to run init, so
// hide it cleanly rather than show a broken option.
func availableAddProject(m *Model) bool {
	return m.tab != tabHosts && m.RunInitFunc != nil
}

// availableHostAuth gates the `a` key (set up auth) to Hosts-tab rows
// whose most recent refresh failed with Permission-denied. Other
// auth-related statuses (Online, Offline due to network) don't need
// ssh-copy-id. v0.17 Phase 1l polish.
func availableHostAuth(m *Model) bool {
	if !availableOnHostsTab(m) {
		return false
	}
	h, ok := m.selectedHost()
	if !ok {
		return false
	}
	snap := m.remoteSnaps[h.Name]
	if snap == nil {
		// Unknown status — allow `a` so the user can pre-emptively
		// set up auth without first triggering a refresh failure.
		return true
	}
	return strings.Contains(snap.LastError, "Permission denied") ||
		strings.Contains(snap.LastError, "publickey")
}

// availableHostSSH gates the `s` key on the Hosts tab. Surfaces
// whenever the cursored host has a known SSH target — the action
// itself just execs `ssh <target>` and hands the terminal off.
func availableHostSSH(m *Model) bool {
	if !availableOnHostsTab(m) {
		return false
	}
	h, ok := m.selectedHost()
	if !ok {
		return false
	}
	return h.SSHTarget != ""
}

// availableLocalUpgrade is the gate for the in-TUI upgrade flow (U on
// the workspace lists). Hosts tab gets its own U binding for remote
// dispatch, so we exclude it here to avoid two bindings firing on the
// same key.
func availableLocalUpgrade(m *Model) bool {
	if m.tab == tabHosts {
		return false
	}
	return availableUpgrade(m)
}

// availableHostUpgrade gates the U key on the Hosts tab. Three
// conditions must hold:
//   - Last refresh succeeded (snap.LastError == "")
//   - Remote reported a canopy_version (proves canopy is installed
//     and the JSON output schema is the one we expect)
//   - The reported version is not "dev" — `canopy upgrade` on the
//     remote will refuse a dev build ("Switch to the released canopy
//     first"), and surfacing U for those hosts would dispatch a
//     guaranteed-to-fail SSH command. Hide it instead.
func availableHostUpgrade(m *Model) bool {
	if !availableOnHostsTab(m) {
		return false
	}
	h, ok := m.selectedHost()
	if !ok {
		return false
	}
	snap := m.remoteSnaps[h.Name]
	if snap == nil {
		return false
	}
	if snap.LastError != "" || snap.CanopyVersion == "" {
		return false
	}
	return snap.CanopyVersion != "dev"
}

// availableHostSwitchRelease gates the S key. Surface it whenever we
// CAN'T rule out that the remote is a dev binary:
//
//   - CanopyVersion == "dev"           — clearly dev, S is the recovery.
//   - CanopyVersion == "" or "(unknown)" — old canopy that predates the
//     version-emit fix (legitimately can't tell if it's dev or release).
//     Better to surface S as a no-op-if-already-release than to hide it
//     from a user who knows their fleet has dev installs.
//
// On hosts whose snapshot reports a real semver, `canopy use release`
// would be a no-op — hide S there to keep the help bar honest.
// Requires a successful most-recent refresh so we don't act on stale data.
func availableHostSwitchRelease(m *Model) bool {
	if !availableOnHostsTab(m) {
		return false
	}
	h, ok := m.selectedHost()
	if !ok {
		return false
	}
	snap := m.remoteSnaps[h.Name]
	if snap == nil || snap.LastError != "" {
		return false
	}
	v := snap.CanopyVersion
	return v == "dev" || v == "" || v == "(unknown)"
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
