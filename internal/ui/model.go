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

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/clog"
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
	Status      state.Status // for main, the literal "main"; otherwise the workspace status
	Port        int          // 0 means "no port to show" -> renders as "—"
	TmuxSession string
	Alive       bool // tmux session liveness, queried at refresh time
}

// viewMode tracks which screen the TUI is showing. listMode is the
// default table; newMode is the new-workspace text input;
// confirmDeleteMode is the y/N prompt before tearing down a workspace;
// busyMode is the wait/output screen during a long-running create or
// remove.
type viewMode int

const (
	listMode viewMode = iota
	newMode
	confirmDeleteMode
	busyMode
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
	deleteTarget string // workspace name pending removal

	// Long-running operation in progress (mode == busyMode). Reused by
	// both Create and Remove flows.
	busyTitle  string // e.g. "Creating workspace 'bold-falcon'..." or "Removing 'bold-falcon'..."
	busyOutput string // captured stdout/stderr after the goroutine returns
	busyDone   bool   // true once the goroutine completes
	busyErr    error  // the goroutine's error if any (separate from m.err to keep busy view focused)

	// Loaded once at startup, used in title rendering.
	projectName string
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

		// Main row: present iff the session is alive.
		mainSession := tmux.SafeName(mgr.Cfg.Project) + "-main"
		if alive, _ := tc.HasSession(ctx, mainSession); alive {
			r := Row{
				IsMain:      true,
				Name:        "(main)",
				Branch:      "—",
				Status:      "main", // not one of the 5 workspace states
				TmuxSession: mainSession,
				Alive:       true,
			}
			if meta, ok := st.Projects[mgr.Cfg.Project]; ok {
				r.Port = meta.PortBase
			}
			rows = append(rows, r)
		}

		// Workspace rows.
		for _, w := range workspaces {
			alive, _ := tc.HasSession(ctx, w.TmuxSession)
			rows = append(rows, Row{
				IsMain:      false,
				Name:        w.Name,
				Branch:      w.Branch,
				Status:      w.Status,
				Port:        w.Port,
				TmuxSession: w.TmuxSession,
				Alive:       alive,
			})
		}
		return rowsLoadedMsg{rows: rows}
	}
}
