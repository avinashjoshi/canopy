// Package ui hosts canopy's Bubbletea TUI. The TUI is the front door
// when `canopy` is invoked with no subcommand — it shows the list of
// workspaces (plus the main session if alive), lets the user navigate,
// attach, create, and remove without leaving the visual surface.
//
// Architecture: the Model holds a snapshot of state (loaded via
// workspace.Manager.List + the main-session check) and a cursor. Every
// keypress maps to an Update that mutates the Model, possibly returning
// a tea.Cmd to fetch fresh data or hand off to tmux. The View renders
// the Model into a styled table via lipgloss.
//
// We deliberately keep the Manager + state.Store wiring outside this
// package — Model takes a *workspace.Manager and dispatches to it. That
// keeps internal/ui from owning the lifecycle, mirroring how
// cmd/canopy/* subcommands work.
package ui

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/ghx"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/lifecycle"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/ui/projectlist"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

var log = clog.Pkg("ui")

// Row is a back-compat alias for state.GlobalRow. v0.8 unification
// promoted state.GlobalRow to the canonical row shape (it's what the
// projectlist sub-component renders) and ui.Row went away. Tests in
// the package still write `ui.Row{...}` literals; the alias keeps
// those compiling without rewriting every test.
type Row = state.GlobalRow

// viewMode tracks which screen the TUI is showing.
//
// listMode is the default table.
//
// The new-workspace flow is a two-step picker (canopy convention,
// lazygit-flavored):
//
//   - newPickerMode: variant chooser. Single-key shortcuts pick a
//     branch direction (Fresh / PR / Issue / Branch). Self-evident,
//     no syntax to recall.
//   - newFreshMode: name input for the "blank workspace" path.
//   - newPRMode / newIssueMode / newBranchMode: per-variant sub-modals
//     that load live data (gh / git) and let the user pick from a
//     filtered list.
//
// Esc steps back one level; from a sub-modal back to the picker, from
// the picker back to listMode. Never exits canopy outright.
//
// confirmDeleteMode is the y/N prompt before tearing down a workspace;
// busyMode is the wait/output screen during a long-running operation.
type viewMode int

const (
	listMode viewMode = iota
	newPickerMode
	newFreshMode
	newPRMode
	newIssueMode
	newBranchMode
	confirmDeleteMode
	// confirmRetryMode is the y/N gate for `R` on a non-broken workspace
	// (D3/CP1). Mirrors the CLI's `canopy retry --force` friction so a
	// muscle-memory R-press doesn't accidentally re-run scripts.setup on
	// a healthy workspace and clobber whatever state setup mutates
	// (db reseed, env regen, etc.).
	confirmRetryMode
	// confirmKillMode is the y/N gate before `K` tears down a workspace's
	// tmux session. Distinct from confirmDeleteMode because K is much
	// less destructive: state.json + worktree dir + branch all survive,
	// only the tmux session goes. Re-pressing Enter after kill resurrects
	// the workspace cleanly. The friction here is "did you really mean
	// to drop this session?", not "are you sure you want to lose work?".
	confirmKillMode
	// drawerMode is the diagnostic detail drawer (opened with `i`).
	// Read-only view of one workspace's process tree, recent logs, env,
	// status history, and last setup log. The drawer is opt-in (no
	// auto-open) and scope-capped to diagnostics — no editing, no live
	// dev-server tailing, no canopy.json mutation. See the CEO plan at
	// ~/.gstack/projects/canopy/ceo-plans/2026-04-29-tmux-health-and-resurrect.md
	// for the load-bearing scope cap rationale.
	drawerMode
	busyMode
	// upgradeMode is the in-TUI canopy-upgrade flow. Reachable via
	// the `U` key from listMode when the auto-check pill is showing.
	// Owns the screen end-to-end while the upgrade runs (no top-bar
	// pills) and dispatches to its own four-state sub-flow:
	// loading → preview → running → doneOK/doneError. See
	// internal/ui/upgrade.go for the state machine and key handling.
	upgradeMode
)

// inNewFlow reports whether the current mode is any step of the
// new-workspace flow. Used by Update to route messages to the right
// per-mode handler without listing all five constants every time.
func (m viewMode) inNewFlow() bool {
	return m == newPickerMode ||
		m == newFreshMode ||
		m == newPRMode ||
		m == newIssueMode ||
		m == newBranchMode
}

// busyOpKind identifies which long-running operation is currently in
// busyMode. The View uses this to render the right success message
// ("Workspace created" vs "Workspace removed" vs "Workspace recovered")
// and decides what to do after dismiss (e.g., retry's success could
// offer to attach automatically).
type busyOpKind int

const (
	busyOpNone busyOpKind = iota
	busyOpCreate
	busyOpRemove
	busyOpRetry
)

// Model is the Bubbletea state. Constructed via New() (project mode,
// mgr non-nil) or NewUnified() (mgr-optional, used for popup + global
// invocations). Updated via Update(), rendered via View().
//
// v0.8 unification: the same Model serves three contexts — project TUI
// (mgr non-nil, single-project rows), global TUI (mgr nil, cross-project
// rows), and popup mode (CANOPY_IN_POPUP=1, single-line tab bar +
// switch-client attach). The viewMode + popup* fields drive the runtime
// dispatch.
type Model struct {
	// mgr is the current-project Manager. Non-nil when canopy was
	// invoked from inside a project (cwd has canopy.json walk-up).
	// Nil when invoked outside any project (global TUI startup) —
	// rows still populate from state.BuildGlobalRows, but the `n`
	// keybind is hidden and Local tab shows onboarding text.
	mgr *workspace.Manager
	tc  *tmux.Client

	// store is the state.Store the unified model uses for cross-project
	// row aggregation (state.BuildGlobalRows) and transient Manager
	// construction (cross-project d/R). Always set, even when mgr is
	// non-nil — mgr.Store and store point to the same on-disk file
	// in that case.
	store *state.Store

	// list is the embedded projectlist sub-component that owns row
	// rendering + cursor state. The unified TUI delegates rendering
	// to projectlist (the same sub-component the popup used in v0.7),
	// matching the popup's grouped-by-project visual treatment.
	list projectlist.Model

	width  int
	height int

	// Toggles + ephemeral UI state.
	mode     viewMode
	showHelp bool
	err      error // last operational error to surface; cleared on next refresh

	// New-workspace flow state.
	//
	// Step 1 (newPickerMode): newPickerCursor selects which variant
	// to launch. 0..3 maps to fresh / pr / issue / branch.
	//
	// Step 2 (newFreshMode): nameInput captures the optional workspace
	// name. Empty → namegen picks a random one.
	//
	// Step 2b/c (newPRMode, newIssueMode): list-with-filter pickers.
	// listInput is the number-or-filter input; listCursor is the
	// arrow-selected index into the (filtered) list. newPRs / newIssues
	// hold the live data once the async loader returns.
	newPickerCursor int
	nameInput       textinput.Model

	listInput   textinput.Model
	listCursor  int
	newLoading  bool
	newLoadErr  error
	newPRs      []ghx.PRSummary
	newIssues   []ghx.IssueSummary
	newBranches []string // local + remote branches; remote prefixed "origin/"

	// In-flight new-workspace target. Set by actionNewWorkspace before
	// the picker opens, used by every submit/load handler in the flow,
	// cleared when the flow exits (esc back to listMode, or busy dismiss).
	//
	// Decoupled from m.mgr because the Global tab's cursor may point at a
	// project DIFFERENT from the launch-context project (m.mgr). On Local
	// tab these all collapse to m.mgr / its config; on Global tab they're
	// resolved from the cursor row's ProjectRoot via managerForRow.
	//
	// The picker, sub-modal headers, busy title, and success line all
	// render newTargetName so the user sees which project they're
	// creating in — load-bearing for cross-project intent clarity.
	newTargetMgr  *workspace.Manager
	newTargetRoot string // ProjectRoot of the target project
	newTargetName string // display name (Cfg.Project) for the chip + headers

	// Confirm-delete modal (mode == confirmDeleteMode).
	deleteTarget string // workspace name pending removal
	// deleteTargetRoot scopes deleteTarget to a specific project. Without
	// this, `handleConfirmDeleteKey.resolveTargetMgr` would match by Name
	// only — and on the Global tab, two projects each with a workspace
	// named "foo" would be ambiguous: a refresh between modal-open and
	// confirm could re-order rows so the FIRST `foo` in filteredRows is
	// project B's even though the user pressed d on project A's. Storing
	// the project root snapshots the user's intent and forces an exact
	// (Project, Name) match at confirm time.
	deleteTargetRoot string
	deleteHangs      []string // v0.6 safety check results — populated when 'd' is pressed; non-empty triggers the force-required path in renderConfirmDelete + handleConfirmDeleteKey

	// Long-running operation in progress (mode == busyMode). Reused by
	// Create, Remove, and Retry flows.
	busyOp     busyOpKind // distinguishes the success message + post-action
	busyTitle  string     // e.g. "Creating workspace 'bold-falcon'..." / "Removing 'foo'..."
	busyOutput string     // captured stdout/stderr after the goroutine returns
	busyDone   bool       // true once the goroutine completes
	busyErr    error      // the goroutine's error if any (separate from m.err)

	// Loaded once at startup, used in title rendering.
	projectName string

	// ─── v0.8 unification fields ─────────────────────────────────────
	// inPopup is true when CANOPY_IN_POPUP=1 was set by the tmux
	// display-popup -E invocation. Toggles single-line tab bar,
	// switch-client attach (via tea.QuitMsg after tmux switch-client),
	// and the compact help line. Determined once at New time and
	// immutable for the program's lifetime.
	inPopup bool

	// currentProject is the canonical ProjectRoot that the Local tab
	// filters to. Resolved at startup via workspace.ResolveCurrentProject.
	// Empty when the user is outside any registered project — Local tab
	// is then shown but empty (with onboarding text).
	currentProject string

	// currentWorkspace is the registered workspace name whose Path
	// matches cwd at startup. Set when cwd is inside a workspace dir;
	// used to pre-select that workspace in the list on the first
	// rowsLoadedMsg. Empty otherwise (popup launched from main session
	// or outside any workspace — fall back to row 0).
	currentWorkspace string

	// currentWorkspaceRoot is the ProjectRoot of currentWorkspace.
	// Tracked alongside the name so escape/preselect logic disambiguates
	// across projects with same-named workspaces — e.g. project A and
	// B both have a "foo" workspace, and from A/foo deleting B/foo on
	// the Global tab must not trigger an escape switch to B's main.
	currentWorkspaceRoot string

	// initialCursorPlaced flips to true once the first rowsLoadedMsg
	// has been used to position the cursor on currentWorkspace. Without
	// this latch, every subsequent refresh would yank the cursor back
	// to currentWorkspace mid-session, losing whatever the user was
	// hovering on.
	initialCursorPlaced bool

	// tab tracks which top-level tab is active. tabLocal filters to
	// rows whose ProjectRoot matches currentProject; tabGlobal shows
	// every row from state.BuildGlobalRows.
	tab tabKind

	// allRows is the unfiltered row set (cross-project when mgr is nil
	// or global tab is selected). The tab + searchQuery filter
	// projects allRows down to what list renders.
	allRows []state.GlobalRow

	// searchMode is true while the user is typing in the fuzzy-search
	// box (entered via /). Captures keystrokes into searchQuery
	// instead of forwarding to the listMode keymap.
	searchMode bool
	// searchQuery is the current fuzzy-search filter string. Empty
	// means no filter. Subsequence match (fzf-style) against
	// row name + project + branch.
	searchQuery string

	// confirmRetryNonBroken triggers a y/N modal before R re-runs
	// scripts.setup on a non-broken workspace (D3/CP1). Mirrors the
	// CLI's --force friction. The pending row name is held in
	// retryTarget; the modal is the busyMode parent waiting for input.
	retryTarget string

	// memCache caches per-session RSS+CPU values for the load column
	// with a 5s TTL. Without caching, every refresh tick would spawn
	// a `ps -A` per workspace row. Invalidated on K (kill) so the
	// just-killed row flips to "—" immediately rather than lagging
	// the actual state by up to TTL seconds.
	memCache *state.MemCache

	// Confirm-kill modal (mode == confirmKillMode). K kills the
	// workspace's tmux session; the y/N gate prevents accidental
	// keypress. killTargetRoot scopes by ProjectRoot for the same
	// reason deleteTargetRoot does — see comment on deleteTargetRoot.
	killTarget     string
	killTargetRoot string

	// Drawer state (mode == drawerMode). The drawer snapshots the row
	// it was opened against and the loaded diagnostic data so re-renders
	// during async refreshes don't pull data from underneath the user.
	drawerRow      Row
	drawerProcInfo string // pre-rendered process tree text, "" while loading
	drawerLogTail  string // last N lines of ~/.canopy/log/canopy-<ws>.log
	drawerSetupLog string // last setup run output, or "no setup log captured"
	drawerErr      error  // non-fatal load error to surface in-drawer

	// Version pill state. Set by SetVersionInfo from cmd/canopy on
	// startup so the top bar can surface "release vs DEV, and which
	// workspace if dev" at a glance. Both fields empty means "no
	// version info available" — the pill is omitted from the top bar
	// and the existing chrome is unchanged.
	//
	// versionLabel is the human-friendly version string ("v0.12.0",
	// "main-abc1234", or "dev"). Shown muted gray for release builds.
	//
	// devWorkspace is the canopy workspace name when the running
	// canopy is a dev build inside a known worktree, or "" when not
	// detectable. Non-empty triggers the cyan DEV pill regardless of
	// versionLabel; empty + versionLabel == "dev" still renders DEV
	// (untracked); empty + versionLabel != "dev" renders the version
	// pill normally.
	versionLabel string
	devWorkspace string

	// upgradeAvailable is the bare semver of a newer canopy release
	// that's available, or "" when no upgrade is available / has
	// been dismissed / running on a DEV build. Mutates the version
	// pill from "v0.12.3" to "v0.12.3 ⇑ v0.13.0" (yellow arrow).
	// Set by SetUpgradeAvailable on startup (sync read from the
	// auto-check cache) and updated mid-session by the async
	// refresh tea.Cmd when the cache was stale at startup.
	upgradeAvailable string

	// upgradeRefreshFn is the closure that performs the async
	// network fetch. Wired unconditionally by route.go (when not
	// DEV) so the `r` key can force a refresh regardless of whether
	// the cache was fresh at startup. Init() decides whether to
	// fire it on launch via upgradeRefreshOnInit.
	upgradeRefreshFn UpgradeRefreshFn

	// upgradeRefreshOnInit gates whether Init() fires the refresh
	// closure on TUI startup. True when the auto-check cache was
	// missing or stale (TTL expired) at construction. False when
	// the cache was fresh — we trust the cached value at startup
	// and only refresh on explicit user action (`r` key).
	upgradeRefreshOnInit bool

	// In-TUI upgrade flow state. Active only when mode == upgradeMode.
	// Reset to zero on dismiss (resetUpgradeMode). Lives in upgrade.go
	// alongside the state machine.
	upgradeState         upgradeState
	upgradeChangelog     string
	upgradeChangelogVP   viewport.Model // scrollable preview pane
	upgradeChangelogInit bool           // viewport sized + content set
	upgradeShipped       string         // version that just installed (for doneOK message)
	upgradeOutput        string
	upgradeErr           error
	upgradeBuf           *safeBuffer
	upgradeCancel        context.CancelFunc
	upgradeChangelogFn   UpgradeChangelogFn
	upgradeShellFn       UpgradeShellFn
	upgradeDismissFn     UpgradeDismissFn
}

// UpgradeRefreshFn performs the async cache refresh: fetches the
// latest VERSION from upstream, writes the cache, returns the
// upgrade-available semver (or "" when the user is up to date or
// has dismissed the latest). Network errors surface here so the
// caller (the UI) can log; the UI does NOT change pill state on
// error — the existing cached value stays visible.
type UpgradeRefreshFn func(ctx context.Context) (latest string, err error)

// SetVersionInfo records the running binary's version surface for the
// top-bar pill. Called by cmd/canopy after constructing the model so
// the UI never has to know about ldflags or BuildInfo — it just renders
// the strings it's given. Safe to call with all-empty arguments to
// suppress the pill (e.g., in tests that don't care about chrome).
func (m *Model) SetVersionInfo(versionLabel, devWorkspace string) {
	m.versionLabel = versionLabel
	m.devWorkspace = devWorkspace
}

// SetUpgradeAvailable records the bare semver of an available newer
// canopy release. Empty string suppresses the upgrade-arrow branch on
// the version pill. Caller is responsible for the gating logic
// (DEV-binary check, dismissal, version equality) — this setter just
// stores the value the renderer should display.
//
// Called by RunUnified on startup (sync read from
// ~/.canopy/upgrade-check.json) and updated mid-session by the
// upgradeCheckedMsg handler when the async refresh lands.
func (m *Model) SetUpgradeAvailable(latest string) {
	m.upgradeAvailable = latest
}

// SetUpgradeRefreshFn wires the async refresh closure that fires
// from Init() when the auto-check cache was missing or stale at
// startup. Pass nil to skip refresh entirely (tests, popup mode
// where we want minimal startup work, etc.).
func (m *Model) SetUpgradeRefreshFn(fn UpgradeRefreshFn) {
	m.upgradeRefreshFn = fn
}

// upgradeCheckedMsg lands when the async refresh started by Init
// completes. Carries the new latest_version (or "" if up-to-date).
// Errors from the refresh are not propagated — they're logged at
// the closure layer; the UI just doesn't update pill state on
// failure.
type upgradeCheckedMsg struct {
	latest string
}

// upgradeRefreshCmd wraps the closure in a tea.Cmd. Returns nil when
// no refresh fn is wired (test path, or the caller intentionally
// suppressed it). Uses a 10s timeout so a stalled HTTP/git fetch
// can't hang Bubbletea forever.
func upgradeRefreshCmd(fn UpgradeRefreshFn) tea.Cmd {
	if fn == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		latest, _ := fn(ctx)
		return upgradeCheckedMsg{latest: latest}
	}
}

// tabKind identifies which top-level tab is active in the unified TUI.
type tabKind int

const (
	// tabLocal shows only rows whose ProjectRoot matches m.currentProject.
	// Pre-selected when canopy was invoked from inside a project; the
	// "scope is what I'm working on right now" view.
	tabLocal tabKind = iota
	// tabGlobal shows every workspace canopy knows about across all
	// projects. Pre-selected when canopy was invoked from outside any
	// project; the "give me everything" view.
	tabGlobal
)

// managerForRow returns a *workspace.Manager scoped to the row's project.
// When the row's ProjectRoot matches the current Manager's project root,
// returns m.mgr directly (no construction cost). When the row is in a
// different project (cross-project d/R from the Global tab), constructs
// a transient Manager via config.LoadFrom + workspace.New.
//
// Returns an error when the row's canopy.json is missing/parse-broke or
// Manager construction fails. Caller surfaces via m.err so the user
// sees a status-line hint instead of a panic.
//
// Why no caching: per-action canopy.json reads are <1ms; caching would
// add staleness bugs (project's canopy.json edited mid-session) for
// negligible perf gain. Boring choice.
func (m *Model) managerForRow(row Row) (*workspace.Manager, error) {
	if m.mgr != nil && row.ProjectRoot == m.mgr.Cfg.ProjectRoot {
		return m.mgr, nil
	}
	if row.ProjectRoot == "" {
		return nil, fmt.Errorf("row %q has no ProjectRoot — can't resolve project Manager", row.Name)
	}
	cfg, err := config.LoadFrom(row.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("project config unavailable at %s: %w", row.ProjectRoot, err)
	}
	mgr, err := workspace.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("manager construction failed for %s: %w", cfg.Project, err)
	}
	return mgr, nil
}

// branchInWorkspace reports whether a branch name is currently
// checked out by an existing canopy workspace in this project. Used
// by the PR + branch pickers to tag conflicting rows so the user
// doesn't try to create a duplicate (which would fail at git-
// worktree-add time anyway with a confusing error).
//
// Match is exact-string. Caller is responsible for normalizing the
// branch name (stripping "origin/" prefix etc.) before passing it
// in. The main row is excluded — its branch is "—" sentinel and
// shouldn't shadow real workspaces.
func (m *Model) branchInWorkspace(branch string) (string, bool) {
	if branch == "" {
		return "", false
	}
	for _, r := range m.allRows {
		if r.IsMain {
			continue
		}
		// Project-context check: only match against rows in the current
		// project's source repo. Cross-project branch collisions are
		// not the same conflict (each project has its own git tree).
		// Rows with empty ProjectRoot pass through (legacy project-mode
		// rows + tests that don't set the field). Scope check uses the
		// in-flight new-flow target when set (cross-project from Global
		// tab); falls back to m.mgr for the Local-tab path and any
		// caller outside the new flow.
		scopeRoot := ""
		switch {
		case m.newTargetRoot != "":
			scopeRoot = m.newTargetRoot
		case m.mgr != nil:
			scopeRoot = m.mgr.Cfg.ProjectRoot
		}
		if scopeRoot != "" && r.ProjectRoot != "" && r.ProjectRoot != scopeRoot {
			continue
		}
		if r.Branch == branch {
			return r.Name, true
		}
	}
	return "", false
}

// New constructs a project-mode Model. mgr is required. Used by the
// project TUI entry path (when canopy.json walk-up succeeds).
//
// Wraps NewUnified with the project-mode defaults: tabLocal pre-selected,
// currentProject = mgr.Cfg.ProjectRoot.
func New(mgr *workspace.Manager) *Model {
	return NewUnified(mgr, mgr.Store, mgr.Tmux, mgr.Cfg.ProjectRoot, "", "")
}

// NewUnified is the v0.8 unified-TUI constructor. Single entry point for
// every canopy invocation: project, global, popup. mgr is optional —
// nil when the user invoked canopy from outside any registered project
// or from a popup whose host pane isn't in a known project.
//
// currentProject is the canonical ProjectRoot for the Local tab filter
// (resolved upstream by workspace.ResolveCurrentProject); empty disables
// Local-tab filtering.
//
// Popup-mode rendering is detected via CANOPY_IN_POPUP=1 (set by the
// tmux display-popup -E invocation in install_tmux.go). Single source of
// truth: the env var is what flips chrome from fullscreen to popup.
func NewUnified(mgr *workspace.Manager, store *state.Store, tc *tmux.Client, currentProject, currentWorkspaceRoot, currentWorkspace string) *Model {
	ti := textinput.New()
	ti.Placeholder = "leave blank for a random name"
	ti.CharLimit = 60
	ti.Width = 40

	li := textinput.New()
	li.Placeholder = "type to filter, or a number to fetch by ID"
	li.CharLimit = 80
	li.Width = 60

	// Tab pre-selection: Local when the user has a current project
	// context, Global otherwise. The user can tab away on either side.
	defaultTab := tabLocal
	if currentProject == "" {
		defaultTab = tabGlobal
	}

	projectName := ""
	if mgr != nil {
		projectName = mgr.Cfg.Project
	}

	m := &Model{
		mgr:                  mgr,
		tc:                   tc,
		store:                store,
		projectName:          projectName,
		nameInput:            ti,
		listInput:            li,
		mode:                 listMode,
		inPopup:              os.Getenv("CANOPY_IN_POPUP") == "1",
		currentProject:       currentProject,
		currentWorkspace:     currentWorkspace,
		currentWorkspaceRoot: currentWorkspaceRoot,
		tab:                  defaultTab,
	}
	// projectlist owns row rendering + cursor. We supply nil callbacks
	// because the unified TUI's bindings table dispatches activate /
	// goToProject / refresh — projectlist's own keymap (up/down/enter)
	// fires through Update but the activate path is handled by the
	// parent's `enter` binding (actionAttach), not projectlist's
	// OnActivate. SetRows happens after each refresh.
	m.list = projectlist.New(projectlist.Options{})
	m.list.SetCurrent(currentWorkspaceRoot, currentWorkspace)
	return m
}

// RunUnifiedOptions groups the optional knobs passed into RunUnified.
// All fields are optional; the zero value gives the bare TUI with no
// version pill, no auto-check, no in-TUI upgrade flow.
//
// Lives next to RunUnified rather than being separately documented
// because it exists solely as RunUnified's options bag — when the
// next field is added, it lands here and RunUnified's call sites
// don't shift positionally.
type RunUnifiedOptions struct {
	// VersionLabel is the human-friendly version string for the
	// top-bar pill ("v0.13.0+abc1234"). Empty suppresses the pill.
	VersionLabel string

	// DevWorkspace is the canopy workspace name when the running
	// canopy is a DEV build inside a known worktree. Non-empty
	// triggers the cyan DEV pill regardless of VersionLabel.
	DevWorkspace string

	// InitialUpgrade is the bare semver of an available newer
	// canopy release, read synchronously from the auto-check cache
	// at startup. Empty when no upgrade is available, the cache
	// is missing, the user has dismissed, or running on DEV.
	InitialUpgrade string

	// RefreshFn is the async closure that performs the network
	// fetch + cache write. Result lands as upgradeCheckedMsg and
	// updates the pill mid-session. Wired unconditionally so the
	// `r` key can force a refresh; Init() only fires it on launch
	// when RefreshOnInit is also true. Nil disables refresh.
	RefreshFn UpgradeRefreshFn

	// RefreshOnInit gates whether Init() fires RefreshFn at TUI
	// launch. True when the auto-check cache was stale or missing
	// at construction (caller derived this from initialUpgradeForUI).
	// False when the cache was fresh — startup uses the cached
	// value and skips the network call. The `r` key fires RefreshFn
	// regardless of this flag.
	RefreshOnInit bool

	// ChangelogFn fetches the CHANGELOG slice for the in-TUI
	// upgrade flow's preview state. Nil disables the U key.
	ChangelogFn UpgradeChangelogFn

	// ShellFn runs git pull + make install for the in-TUI upgrade
	// flow's running state. Nil disables the U key.
	ShellFn UpgradeShellFn

	// DismissFn writes dismissed_version into the auto-check cache
	// for the D key. Nil disables D.
	DismissFn UpgradeDismissFn
}

// RunUnified is the v0.8 public entry point used by cmd/canopy/route.go.
// Single bubbletea program for every canopy invocation: project, global,
// popup. mgr is optional — nil when invoked from outside a registered
// project. currentProject is the resolved Local-tab filter root.
//
// Optional knobs (version pill, auto-check, in-TUI upgrade flow) live
// in RunUnifiedOptions to keep the positional signature short and
// guard against argument-order bugs as more features land. Pass the
// zero value for the bare TUI.
//
// In popup mode (CANOPY_IN_POPUP=1) we omit MouseCellMotion since the
// popup is keyboard-driven and mouse handling adds latency.
func RunUnified(mgr *workspace.Manager, store *state.Store, tc *tmux.Client, currentProject, currentWorkspaceRoot, currentWorkspace string, opts RunUnifiedOptions) error {
	m := NewUnified(mgr, store, tc, currentProject, currentWorkspaceRoot, currentWorkspace)
	m.SetVersionInfo(opts.VersionLabel, opts.DevWorkspace)
	m.SetUpgradeAvailable(opts.InitialUpgrade)
	m.SetUpgradeRefreshFn(opts.RefreshFn)
	m.upgradeRefreshOnInit = opts.RefreshOnInit
	m.SetUpgradeChangelogFn(opts.ChangelogFn)
	m.SetUpgradeShellFn(opts.ShellFn)
	m.SetUpgradeDismissFn(opts.DismissFn)
	teaOpts := []tea.ProgramOption{tea.WithAltScreen()}
	if !m.inPopup {
		teaOpts = append(teaOpts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, teaOpts...)
	_, err := p.Run()
	return err
}

// Run is the legacy project-mode entry point. RunUnified is the v0.8
// unified entry; Run is preserved as a thin wrapper for any external
// callers (none today) and the e2e tests in the workspace package.
//
// Pre-v0.8 Run had an exit-7 signal channel for the popup-from-project
// nested-canopy flow. That flow is gone — popup hosts the unified TUI
// directly, no nested spawn — so this is now a straightforward Bubbletea
// run-loop wrapper.
func Run(mgr *workspace.Manager) error {
	m := New(mgr)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// Init implements tea.Model. Returns the initial command — load the
// workspace list as soon as the program starts. The refresh path is
// dual: when mgr is non-nil it uses mgr.List + mgr.Reconcile (project
// mode); when nil it falls back to state.BuildGlobalRows (global +
// popup-without-project mode). Either way the result lands in
// m.allRows; tab + search filtering happens on every render.
func (m *Model) Init() tea.Cmd {
	if m.memCache == nil {
		m.memCache = state.NewMemCache(state.DefaultMemCacheTTL)
	}
	// Auto-populate the Mem/CPU column on first render. Bubbletea's
	// async-Cmd model means the table still appears instantly with
	// Mem="—" placeholders; the rowsLoadedMsg arrives a moment later
	// (one ps -A per session, ~5-10ms on a typical workstation) and
	// triggers a re-render with the populated values. No explicit `r`
	// gesture required — the column "just works" the way every other
	// column does.
	cmds := []tea.Cmd{m.refresh()}
	// Async upgrade check refresh. Two gates: closure must be wired
	// (skipped for tests, popup mode, DEV builds), AND the caller
	// must have flagged the cache as stale/missing via
	// upgradeRefreshOnInit. Fresh-cache startup uses the cached value
	// and skips the network call to keep TUI launch quiet. The `r`
	// key fires the closure unconditionally (see actionRefresh) so
	// users can force a refresh even when the cache was fresh at
	// startup.
	if m.upgradeRefreshOnInit {
		if cmd := upgradeRefreshCmd(m.upgradeRefreshFn); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// rowsLoadedMsg is the result of a refresh. Carries the new rows or an
// error; Update applies them to the Model.
type rowsLoadedMsg struct {
	rows []state.GlobalRow
	err  error
}

// refreshCmd reconciles state, then loads workspaces + the main row.
// Runs in a goroutine via tea.Cmd so the UI doesn't block on tmux/disk.
//
// Always uses state.BuildGlobalRows so the unified TUI's row data shape
// is uniform across project and global invocations. When mgr is non-nil,
// we run Reconcile first (which mutates state.json) so the freshly-built
// rows reflect the latest stopped/ready transitions.
func refreshCmd(mgr *workspace.Manager, tc *tmux.Client, store *state.Store) tea.Cmd {
	return refreshCmdWithMem(mgr, tc, store, nil)
}

// refresh is the model-bound refresh. Always populates the Mem+CPU
// column when a memCache is configured — auto-load is the right
// default since Bubbletea's async Cmd model keeps the first render
// instant and the populated values arrive on the next tick.
func (m *Model) refresh() tea.Cmd {
	return refreshCmdWithMem(m.mgr, m.tc, m.store, m.memCache)
}

// tmuxLoadAdapter wraps *tmux.Client to satisfy state.LoadProbe. The
// adapter exists because state can't import tmux (would create the
// usual layered-package cycle), so the load-shape struct lives in
// each package separately and we translate at the boundary. Cheap;
// just one struct copy per probe.
type tmuxLoadAdapter struct{ c *tmux.Client }

func (a tmuxLoadAdapter) SessionLoad(ctx context.Context, session string) (state.LoadValue, error) {
	if a.c == nil {
		return state.LoadValue{}, nil
	}
	got, err := a.c.SessionLoad(ctx, session)
	if err != nil {
		return state.LoadValue{}, err
	}
	return state.LoadValue{RSS: got.RSS, CPU: got.CPU}, nil
}

// refreshCmdWithMem is the cache-aware variant of refreshCmd. When
// memCache is non-nil, populates row.MemRSS+CPU via
// BuildGlobalRowsWithLoad so the Mem column has data on first render.
// Default refreshCmd keeps memCache nil for callers that don't want
// the column (cmd/canopy/ls).
func refreshCmdWithMem(mgr *workspace.Manager, tc *tmux.Client, store *state.Store, memCache *state.MemCache) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		// Project-context lazy reconcile so stale ready→stopped before
		// rows render. Errors are non-fatal; rows still build from the
		// latest state we can load.
		if mgr != nil {
			if _, err := mgr.Reconcile(ctx); err != nil {
				log.Warn("ui.refresh.reconcile-failed", "err", err)
			}
		}

		st, err := store.Load()
		if err != nil {
			return rowsLoadedMsg{err: err}
		}
		var rows []state.GlobalRow
		if memCache != nil && tc != nil {
			rows = st.BuildGlobalRowsWithLoad(ctx, tc, tmuxLoadAdapter{c: tc}, memCache)
		} else {
			rows = st.BuildGlobalRows(ctx, tc)
		}
		fillMainBranches(ctx, rows)
		return rowsLoadedMsg{rows: rows}
	}
}

// fillMainBranches replaces the "—" placeholder in main rows with the
// project's actual default branch (origin/main or origin/master). Done
// once per project — DetectDefaultBranch is one git rev-parse call,
// cheap but worth caching across multiple worktrees of the same repo.
//
// Failure is non-fatal: if the project has no remote or uses a
// non-conventional default, the row keeps "main" as a fallback so
// the column doesn't render bare. Either way the user reads
// "this is the main session, branched off X" at a glance.
func fillMainBranches(ctx context.Context, rows []state.GlobalRow) {
	defaults := map[string]string{}
	for i := range rows {
		if !rows[i].IsMain || rows[i].ProjectRoot == "" {
			continue
		}
		root := rows[i].ProjectRoot
		branch, ok := defaults[root]
		if !ok {
			b, err := git.DetectDefaultBranch(ctx, root)
			if err != nil || b == "" {
				b = "main" // fallback when origin/main|master both miss
			}
			branch = b
			defaults[root] = branch
		}
		rows[i].Branch = branch
	}
}

// rowHintsMsg carries a single workspace's lifecycle detector result.
// Update merges it into projectlist via UpdateRowHints (keyed by
// project + name so a concurrent reconcile that reordered rows doesn't
// strand the hint update).
type rowHintsMsg struct {
	project string
	name    string
	hints   []state.Hint
}

// loadRowHintsCmds returns a tea.Batch of per-row hint-loading cmds.
// Each runs lifecycle.RunFast for one workspace in its own goroutine
// and emits a rowHintsMsg when done. tea.Batch dispatches them
// concurrently, so cold-start gh latency parallelizes across rows.
//
// Skips main rows and rows with empty Path. Each row carries its own
// ProjectRoot so cross-project hint loading scopes correctly.
func loadRowHintsCmds(rows []state.GlobalRow) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(rows))
	for _, r := range rows {
		if r.IsMain || r.Path == "" {
			continue
		}
		row := r // capture by value
		cmds = append(cmds, func() tea.Msg {
			ws := state.Workspace{
				Name:        row.Name,
				Branch:      row.Branch,
				Path:        row.Path,
				ProjectRoot: row.ProjectRoot,
				Status:      row.Status,
			}
			return rowHintsMsg{
				project: row.Project,
				name:    row.Name,
				hints:   lifecycle.RunFast(context.Background(), ws),
			}
		})
	}
	return tea.Batch(cmds...)
}
