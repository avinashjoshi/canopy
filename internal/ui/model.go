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
	"os"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/lifecycle"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

var log = clog.Pkg("ui")

// Row is the display shape for a single line in the list. Combines
// regular workspace rows with the synthetic main-session row so the
// View doesn't have to special-case at render time.
type Row struct {
	IsMain      bool         // true for the synthetic (main) row
	Name        string       // "(main)" or the workspace name
	Branch      string       // "—" for main, the branch name otherwise
	Path        string       // worktree path; empty for main / orphan rows
	Status      state.Status // for main, the literal "main"; otherwise the workspace status
	Port        int          // 0 means "no port to show" -> renders as "—"
	TmuxSession string
	Alive       bool // tmux session liveness, queried at refresh time
	// LastErrorHint is the auto-detected diagnosis from
	// workspace.Diagnose, populated only when Status==broken AND
	// canopy recognized the failure signature. Rendered as a
	// "hint:" line under the table when the cursor sits on this row.
	LastErrorHint string
	// Hints are the v0.6 lifecycle detector results (rename / shipped
	// / pr_status). Loaded asynchronously in a second refresh phase
	// so the table renders immediately without waiting for the gh
	// shellout. Empty slice on first render; populated by rowHintsMsg
	// after each per-row detector returns.
	Hints []state.Hint
}

// viewMode tracks which screen the TUI is showing. listMode is the
// default table; newMode is the new-workspace text input;
// confirmDeleteMode is the y/N prompt before tearing down a workspace;
// busyMode is the wait/output screen during a long-running operation.
type viewMode int

const (
	listMode viewMode = iota
	newMode
	confirmDeleteMode
	busyMode
)

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

// Model is the Bubbletea state. Constructed via New(), updated via
// Update(), rendered via View().
type Model struct {
	mgr *workspace.Manager
	tc  *tmux.Client

	rows   []Row
	cursor int

	width  int
	height int

	// Toggles + ephemeral UI state.
	mode     viewMode
	showHelp bool
	err      error // last operational error to surface; cleared on next refresh

	// New-workspace modal (mode == newMode).
	nameInput textinput.Model

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

	// fromGlobal is true when this project TUI was launched from the
	// cross-project global view (model_global.go's goToProject sets the
	// CANOPY_FROM_GLOBAL=1 env var on the re-execed canopy). When true,
	// the listMode keymap accepts `b`/`esc` as a "back to global" key —
	// they just quit the inner program; the outer global TUI's
	// ExecProcess onExit refreshes and re-renders.
	fromGlobal bool
}

// New constructs a Model. The caller must already have a workspace.Manager
// (loaded via cmd/canopy's loadManager helper). Initial rows are empty;
// the first tea.Cmd returned by Init() loads them.
func New(mgr *workspace.Manager) *Model {
	ti := textinput.New()
	ti.Placeholder = "leave blank for a random name"
	ti.CharLimit = 60
	ti.Width = 40
	return &Model{
		mgr:         mgr,
		tc:          mgr.Tmux,
		projectName: mgr.Cfg.Project,
		nameInput:   ti,
		mode:        listMode,
		fromGlobal:  os.Getenv("CANOPY_FROM_GLOBAL") == "1",
	}
}

// Run is the public entry point. It builds a Model, hands it to a
// Bubbletea program, and blocks until the user quits. Returns whatever
// error the program surfaced (or nil on a clean quit).
func Run(mgr *workspace.Manager) error {
	m := New(mgr)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// Init implements tea.Model. Returns the initial command — load the
// workspace list as soon as the program starts.
func (m *Model) Init() tea.Cmd {
	return refreshCmd(m.mgr, m.tc)
}

// rowsLoadedMsg is the result of a refresh. Carries the new rows or an
// error; Update applies them to the Model.
type rowsLoadedMsg struct {
	rows []Row
	err  error
}

// refreshCmd reconciles state, then loads workspaces + the main row.
// Runs in a goroutine via tea.Cmd so the UI doesn't block on tmux/disk.
func refreshCmd(mgr *workspace.Manager, tc *tmux.Client) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		// Lazy reconcile so stale ready -> stopped before we render.
		// Errors here are non-fatal; we continue with the latest state
		// we can load.
		if _, err := mgr.Reconcile(ctx); err != nil {
			log.Warn("ui.refresh.reconcile-failed", "err", err)
		}

		workspaces, err := mgr.List(ctx)
		if err != nil {
			return rowsLoadedMsg{err: err}
		}

		// Load full state once for the main-row port lookup.
		st, err := mgr.Store.Load()
		if err != nil {
			return rowsLoadedMsg{err: err}
		}

		rows := []Row{}

		// Main row (synthetic) — always present so the user can see
		// the project has a main concept and reach for `canopy main`
		// even when no session is active. Alive reflects whether the
		// tmux session is currently up; the activate handler decides
		// what enter does (attach if alive, "run canopy main" hint
		// otherwise).
		mainSession := tmux.SafeName(mgr.Cfg.Project) + "-main"
		alive, _ := tc.HasSession(ctx, mainSession)
		r := Row{
			IsMain:      true,
			Name:        "(main)",
			Branch:      "—",
			Status:      "main", // not one of the 5 workspace states
			TmuxSession: mainSession,
			Alive:       alive,
		}
		if meta, ok := st.Projects[mgr.Cfg.Project]; ok {
			// ProjectRoot key — matches BuildGlobalRows. The basename
			// key path (legacy) is also tolerated by st.Projects
			// readers, so this lookup works for v1 and v2 state.
			r.Port = meta.PortBase
		}
		rows = append(rows, r)

		// Workspace rows.
		for _, w := range workspaces {
			alive, _ := tc.HasSession(ctx, w.TmuxSession)
			rows = append(rows, Row{
				IsMain:        false,
				Name:          w.Name,
				Branch:        w.Branch,
				Path:          w.Path,
				Status:        w.Status,
				Port:          w.Port,
				TmuxSession:   w.TmuxSession,
				Alive:         alive,
				LastErrorHint: w.LastErrorHint,
			})
		}
		return rowsLoadedMsg{rows: rows}
	}
}

// rowHintsMsg carries a single workspace's lifecycle detector result.
// Update merges it into the matching Row by Name. Identified by name
// rather than slice index so a concurrent state mutation doesn't
// strand the hint update on the wrong row.
type rowHintsMsg struct {
	name  string
	hints []state.Hint
}

// loadRowHintsCmds returns a tea.Batch of per-row hint-loading cmds.
// Each runs lifecycle.RunFast for one workspace in its own goroutine
// and emits a rowHintsMsg when done. tea.Batch dispatches them
// concurrently, so cold-start gh latency parallelizes across rows.
//
// Skips main rows and rows with empty Path. Mirror of GlobalModel's
// loadHintsCmds — same shape, same rationale, two-phase rendering.
func loadRowHintsCmds(rows []Row, projectRoot string) tea.Cmd {
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
				ProjectRoot: projectRoot,
				Status:      row.Status,
			}
			return rowHintsMsg{
				name:  row.Name,
				hints: lifecycle.RunFast(context.Background(), ws),
			}
		})
	}
	return tea.Batch(cmds...)
}
