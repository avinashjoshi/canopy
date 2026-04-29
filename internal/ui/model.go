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

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/oncactus/canopy/internal/clog"
	"github.com/oncactus/canopy/internal/config"
	"github.com/oncactus/canopy/internal/ghx"
	"github.com/oncactus/canopy/internal/lifecycle"
	"github.com/oncactus/canopy/internal/state"
	"github.com/oncactus/canopy/internal/tmux"
	"github.com/oncactus/canopy/internal/ui/projectlist"
	"github.com/oncactus/canopy/internal/workspace"
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
	busyMode
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

	// Confirm-delete modal (mode == confirmDeleteMode).
	deleteTarget string   // workspace name pending removal
	deleteHangs  []string // v0.6 safety check results — populated when 'd' is pressed; non-empty triggers the force-required path in renderConfirmDelete + handleConfirmDeleteKey

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
		// rows + tests that don't set the field).
		if m.mgr != nil && r.ProjectRoot != "" && r.ProjectRoot != m.mgr.Cfg.ProjectRoot {
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
	return NewUnified(mgr, mgr.Store, mgr.Tmux, mgr.Cfg.ProjectRoot)
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
func NewUnified(mgr *workspace.Manager, store *state.Store, tc *tmux.Client, currentProject string) *Model {
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
		mgr:            mgr,
		tc:             tc,
		store:          store,
		projectName:    projectName,
		nameInput:      ti,
		listInput:      li,
		mode:           listMode,
		inPopup:        os.Getenv("CANOPY_IN_POPUP") == "1",
		currentProject: currentProject,
		tab:            defaultTab,
	}
	// projectlist owns row rendering + cursor. We supply nil callbacks
	// because the unified TUI's bindings table dispatches activate /
	// goToProject / refresh — projectlist's own keymap (up/down/enter)
	// fires through Update but the activate path is handled by the
	// parent's `enter` binding (actionAttach), not projectlist's
	// OnActivate. SetRows happens after each refresh.
	m.list = projectlist.New(projectlist.Options{})
	return m
}

// RunUnified is the v0.8 public entry point used by cmd/canopy/route.go.
// Single bubbletea program for every canopy invocation: project, global,
// popup. mgr is optional — nil when invoked from outside a registered
// project. currentProject is the resolved Local-tab filter root.
//
// In popup mode (CANOPY_IN_POPUP=1) we omit MouseCellMotion since the
// popup is keyboard-driven and mouse handling adds latency.
func RunUnified(mgr *workspace.Manager, store *state.Store, tc *tmux.Client, currentProject string) error {
	m := NewUnified(mgr, store, tc, currentProject)
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if !m.inPopup {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, opts...)
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
	return refreshCmd(m.mgr, m.tc, m.store)
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
		rows := st.BuildGlobalRows(ctx, tc)
		return rowsLoadedMsg{rows: rows}
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
