// GlobalModel is the top-level Bubbletea Model for the cross-project view
// — what `canopy` shows when invoked from outside any canopy project.
// It wraps internal/ui/projectlist.Model (which does the table) and adds
// the surrounding chrome: title bar, help line, ? overlay, q/ctrl+c quit,
// and the refresh-state-from-disk pipeline.
//
// Why a thin wrapper rather than a copy of model.go's project Model:
// the v1 in-session overlay (TODOS.md) will embed the same projectlist
// component to render the project list inside an attached tmux session.
// If we re-rendered the table here, that overlay would have to either
// duplicate the table code or awkwardly host a tea.Program inside another
// tea.Program. Factoring out projectlist as a sub-component lets us share
// rendering once across every consumer present and future.

package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/ui/projectlist"
)

// Compile-time check that GlobalModel satisfies tea.Model.
var _ tea.Model = (*GlobalModel)(nil)

// GlobalModel is the read-only cross-project Bubbletea program. It does
// not depend on workspace.Manager (and so doesn't require a canopy.json
// to construct), which is the whole point — `canopy` runs from anywhere.
//
// Action surface in v0.5:
//   - enter on `ready` workspace → tmux attach
//   - enter on alive `<project>-main` → tmux attach
//   - enter on stopped/broken/orphaned/setting_up → status-line hint
//     ("cd into <project> to ...") with no destructive action attempted
//   - r → re-read state, re-probe tmux liveness
//   - q / ctrl+c → quit
//   - ? → keybind help overlay
//
// Create/remove from global mode is deferred to v0.6 — see TODOS.md
// "v0.6 — global mode lifecycle." The user's choice was to ship the
// minimal cross-project surface now and grow into the project picker
// flow once they've felt the friction.
type GlobalModel struct {
	store *state.Store
	tc    *tmux.Client

	list     projectlist.Model
	showHelp bool

	width, height int
}

// NewGlobal constructs a GlobalModel. Caller passes a state.Store and a
// tmux.Client — both are leaf primitives that don't require a canopy.json.
//
// The injected callbacks bind the projectlist's enter and r keys to:
//   - enter: dispatch attach via tea.ExecProcess for ready/main rows;
//     for non-ready rows, surface a status-line hint via SetError.
//   - r: kick off a refresh that re-reads state and pushes new rows back.
func NewGlobal(store *state.Store, tc *tmux.Client) *GlobalModel {
	gm := &GlobalModel{
		store: store,
		tc:    tc,
	}
	gm.list = projectlist.New(projectlist.Options{
		OnActivate: gm.activate,
		OnRefresh:  gm.refreshCmd,
	})
	return gm
}

// RunGlobal is the public entry point used by cmd/canopy/route.go when
// `canopy` is invoked from a directory that has no canopy.json and isn't
// inside a fresh git repo (the init-splash case is handled separately
// in InitSplashModel).
func RunGlobal(store *state.Store, tc *tmux.Client) error {
	gm := NewGlobal(store, tc)
	p := tea.NewProgram(gm, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// Init implements tea.Model. Kicks off the first refresh so the table
// populates as soon as the program runs.
func (m *GlobalModel) Init() tea.Cmd {
	return m.refreshCmd()
}

// globalRowsLoadedMsg is the message GlobalModel sends to itself when a
// state load completes. Carries the assembled rows or an error; Update
// pushes them into the embedded projectlist.
type globalRowsLoadedMsg struct {
	rows []state.GlobalRow
	err  error
}

// refreshCmd builds the tea.Cmd that re-reads state.json + re-probes
// tmux liveness. Used by Init and as the OnRefresh callback for the
// embedded projectlist.
func (m *GlobalModel) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		st, err := m.store.Load()
		if err != nil {
			return globalRowsLoadedMsg{err: err}
		}
		rows := st.BuildGlobalRows(context.Background(), m.tc)
		return globalRowsLoadedMsg{rows: rows}
	}
}

// Update implements tea.Model. Routes messages: window size to projectlist,
// refresh results into projectlist's row data, top-level keys (q, ?) here,
// everything else forwards to projectlist.Update.
func (m *GlobalModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Reserve 4 lines for title + spacing + help. Rest goes to the list.
		m.list.SetSize(msg.Width, msg.Height-4)
		return m, nil

	case globalRowsLoadedMsg:
		if msg.err != nil {
			m.list.SetError(msg.err)
			return m, nil
		}
		m.list.SetError(nil)
		m.list.SetRows(msg.rows)
		return m, nil

	case globalErrMsg:
		// Non-fatal hint surfaced from an activate callback (e.g. user
		// pressed enter on a stopped row). Pipe to the list's error
		// banner; cleared on next refresh.
		m.list.SetError(msg.err)
		return m, nil

	case tea.KeyMsg:
		// Help overlay swallows all keys (any key dismisses it).
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = true
			return m, nil
		}
		// Fall through to projectlist for nav + enter + r.
		next, cmd := m.list.Update(msg)
		m.list = next
		return m, cmd
	}

	// Non-key, non-known messages fall through to projectlist (handles
	// any future bubbles internal to the sub-component).
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

// View implements tea.Model. Title, then projectlist's rendered table,
// then the one-line help footer. Help overlay (?) replaces the whole
// thing when active.
func (m *GlobalModel) View() string {
	if m.showHelp {
		return m.renderHelp()
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("canopy") + " " + subtleStyle.Render("global"))
	b.WriteString("\n\n")
	b.WriteString(m.list.View())
	b.WriteString("\n")
	b.WriteString(m.renderHelpLine())
	return b.String()
}

func (m *GlobalModel) renderHelpLine() string {
	keys := []string{
		"↑/↓ navigate",
		"enter attach",
		"r refresh",
		"? help",
		"q quit",
	}
	return subtleStyle.Render(strings.Join(keys, "  ·  "))
}

func (m *GlobalModel) renderHelp() string {
	body := strings.Join([]string{
		titleStyle.Render("canopy global — keybindings"),
		"",
		"  ↑/↓, j/k       move selection",
		"  g, home        first row",
		"  G, end         last row",
		"",
		"  enter          attach to selected (ready/main rows only)",
		"  r              refresh state",
		"",
		"  ?              this help",
		"  q, ctrl-c      quit",
		"",
		subtleStyle.Render("Note: create/remove a workspace by cd'ing into the project"),
		subtleStyle.Render("and running `canopy` there. Global mode is read-only in v0.5."),
		"",
		subtleStyle.Render("Press any key to dismiss."),
	}, "\n")
	return helpBodyStyle.Render(body)
}

// activate is the OnActivate callback wired into projectlist. Decides
// what enter does based on the chosen row's status:
//   - ready / main → dispatch tmux attach via tea.ExecProcess
//   - stopped / broken / orphaned / setting_up → set a status-line hint;
//     no action attempted, no error from canopy's perspective
//
// The hint copy points the user back to project mode for any non-trivial
// action. This is the deliberate v0.5 boundary: global mode shows what's
// there, project mode does anything destructive or canopy.json-aware.
func (m *GlobalModel) activate(row state.GlobalRow) tea.Cmd {
	switch row.Status {
	case state.StatusReady, "main":
		return m.attachCmd(row.TmuxSession)

	case state.StatusStopped:
		hint := fmt.Errorf(
			"workspace %q is stopped — cd into %q and run `canopy` to resurrect",
			row.Name, projectHint(row))
		return func() tea.Msg { return globalErrMsg{err: hint} }

	case state.StatusBroken:
		hint := fmt.Errorf(
			"workspace %q is broken — see ~/.canopy/log/canopy.log; cd into %q to clean up",
			row.Name, projectHint(row))
		return func() tea.Msg { return globalErrMsg{err: hint} }

	case state.StatusOrphaned:
		hint := fmt.Errorf(
			"workspace %q has no on-disk dir — cd into %q and run `canopy rm %s`",
			row.Name, projectHint(row), row.Name)
		return func() tea.Msg { return globalErrMsg{err: hint} }

	case state.StatusSettingUp:
		hint := fmt.Errorf(
			"workspace %q is still setting up — try `r` to refresh in a moment",
			row.Name)
		return func() tea.Msg { return globalErrMsg{err: hint} }
	}

	// Unknown status — defensive no-op rather than crash.
	return nil
}

// globalErrMsg surfaces a non-fatal hint to the embedded projectlist's
// error banner. Routed through Update → SetError so the user sees it
// above the table.
type globalErrMsg struct {
	err error
}

// projectHint chooses the most helpful path string for an error message.
// Prefer the canonical root if we have it; fall back to the basename.
func projectHint(row state.GlobalRow) string {
	if row.ProjectRoot != "" {
		return row.ProjectRoot
	}
	return row.Project
}

// attachCmd builds a tea.Cmd that dispatches `tmux attach -t <session>`
// via tea.ExecProcess. The handoff is the same pattern as the project-
// mode TUI: tmux takes over the terminal until the user detaches with
// prefix-d, then we refresh so any state changes during the session
// surface in the next render.
func (m *GlobalModel) attachCmd(session string) tea.Cmd {
	cmd, err := m.tc.AttachCmd(context.Background(), session)
	if err != nil {
		return func() tea.Msg { return globalErrMsg{err: err} }
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			return globalErrMsg{err: fmt.Errorf("attach %s: %w", session, err)}
		}
		// Detach completed cleanly — refresh state so any changes show up.
		return m.refreshCmd()()
	})
}
