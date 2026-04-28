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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/lifecycle"
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
		OnActivate:    gm.activate,
		OnGoToProject: gm.goToProject,
		OnRefresh:     gm.refreshCmd,
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

// globalRowHintsMsg is a per-row hint-loading result. Update merges it
// into the embedded projectlist via UpdateRowHints. Identified by
// (project, name) rather than slice index so a concurrent row
// rearrangement doesn't strand a hint update on the wrong row.
type globalRowHintsMsg struct {
	project string
	name    string
	hints   []state.Hint
}

// refreshCmd builds the tea.Cmd that re-reads state.json and re-probes
// tmux liveness. Returns rows WITHOUT hints — those load asynchronously
// after the rows render via loadHintsCmds.
//
// Two-phase rationale: lifecycle.RunFast includes pr_status (a `gh pr
// view` shellout). On first launch with cold caches, that's ~1-2s per
// workspace. Sequential loading of N workspaces would freeze the UI
// for N-2N seconds before any row appeared. The two-phase split keeps
// the table responsive: rows show up immediately, badges fade in as
// each per-row detector finishes.
func (m *GlobalModel) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		st, err := m.store.Load()
		if err != nil {
			return globalRowsLoadedMsg{err: err}
		}
		ctx := context.Background()
		rows := st.BuildGlobalRows(ctx, m.tc)
		return globalRowsLoadedMsg{rows: rows}
	}
}

// loadHintsCmds builds a tea.Batch of per-row hint-loading cmds. Each
// cmd runs lifecycle.RunFast for one row in its own goroutine and
// emits a globalRowHintsMsg when done. tea.Batch dispatches them
// concurrently — N gh shellouts run in parallel rather than serially,
// so the worst-case cold-start latency is dominated by the slowest
// single row instead of the sum.
//
// Skips main rows and rows with empty Path (no worktree to detect
// against). Rows that match get exactly one msg per refresh.
func loadHintsCmds(rows []state.GlobalRow) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(rows))
	for _, r := range rows {
		if r.IsMain || r.Path == "" {
			continue
		}
		row := r // capture by value for the goroutine closure
		cmds = append(cmds, func() tea.Msg {
			ws := state.Workspace{
				Name:        row.Name,
				Branch:      row.Branch,
				Path:        row.Path,
				ProjectRoot: row.ProjectRoot,
				Status:      row.Status,
			}
			return globalRowHintsMsg{
				project: row.Project,
				name:    row.Name,
				hints:   lifecycle.RunFast(context.Background(), ws),
			}
		})
	}
	return tea.Batch(cmds...)
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
		// Phase 2: kick off per-row hint loaders in parallel. Each
		// emits a globalRowHintsMsg as it completes; Update merges
		// them into the already-rendered rows.
		return m, loadHintsCmds(msg.rows)

	case globalRowHintsMsg:
		// Late-arriving hint result — merge into the matching row.
		// UpdateRowHints is silent on no-match so a concurrent
		// reconcile that dropped the row doesn't crash here.
		m.list.UpdateRowHints(msg.project, msg.name, msg.hints)
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
		"o open project",
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
		"  o              open the selected row's project (cd in + launch canopy)",
		"  r              refresh state",
		"",
		"  ?              this help",
		"  q, ctrl-c      quit",
		"",
		subtleStyle.Render("Tip: press o on any row to land in that project's TUI,"),
		subtleStyle.Render("where you can create/remove/retry/resurrect workspaces."),
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
		// Press 'o' to land in the project TUI where 'enter' on this same
		// row will resurrect the session.
		hint := fmt.Errorf("workspace %q is stopped — press `o` to open the project and resurrect it", row.Name)
		return func() tea.Msg { return globalErrMsg{err: hint} }

	case state.StatusBroken:
		hint := fmt.Errorf("workspace %q is broken — press `o` to open the project (then `R` to retry)", row.Name)
		return func() tea.Msg { return globalErrMsg{err: hint} }

	case state.StatusOrphaned:
		hint := fmt.Errorf("workspace %q has no on-disk dir — press `o` to open the project (then `d` to clean up)", row.Name)
		return func() tea.Msg { return globalErrMsg{err: hint} }

	case state.StatusSettingUp:
		hint := fmt.Errorf("workspace %q is still setting up — press `r` to refresh in a moment", row.Name)
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

// attachCmd builds a tea.Cmd that dispatches `tmux attach -t <session>`
// via tea.ExecProcess. The handoff is the same pattern as the project-
// mode TUI: tmux takes over the terminal until the user detaches with
// prefix-d, then we refresh so any state changes during the session
// surface in the next render.
//
// Error from tea.ExecProcess: tmux's stderr was visible to the user
// during the exec attempt, but the err object only carries exit status.
// We surface a friendly hint pointing at the most common cause (session
// died between probe and attach).
func (m *GlobalModel) attachCmd(session string) tea.Cmd {
	cmd, err := m.tc.AttachCmd(context.Background(), session)
	if err != nil {
		return func() tea.Msg { return globalErrMsg{err: err} }
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			return globalErrMsg{err: fmt.Errorf(
				"attach %s failed: %w (session may have died — press r to refresh)",
				session, err)}
		}
		// Detach completed cleanly — refresh state so any changes show up.
		return m.refreshCmd()()
	})
}

// goToProject is the OnGoToProject callback. Re-launches `canopy` with
// cwd set to the row's project source repo — that way the user lands in
// the project's TUI, where they can create/remove/resurrect workspaces
// with full canopy.json context. When they quit the inner canopy,
// control returns here and the global TUI re-renders.
//
// Why this exists: v0.5 global mode is read-only (create/remove deferred
// to v0.6). Until then, "open the project" is the escape hatch for any
// action the global mode can't take. Works on every row regardless of
// status, so users have a clean path even from broken/orphaned rows.
//
// Project-root resolution (in order):
//
//  1. If ProjectRoot is already a canonical absolute path, use it
//     directly. Post-migration rows take this path.
//
//  2. If ProjectRoot is empty or just a basename (v1 unmigrated state),
//     derive the source repo from the row's worktree Path via
//     `git rev-parse --git-common-dir`. This works because every canopy
//     workspace IS a git worktree of the source repo, so git already
//     knows where the source lives.
//
//  3. If neither works (e.g. a main-only row with no Path AND an
//     unmigrated ProjectRoot), surface a clear error instead of
//     chdir'ing into garbage.
//
// Implementation: tea.ExecProcess re-execs the running canopy binary
// (os.Executable) with WorkingDir set to the resolved root. The inner
// canopy goes through routeRoot, finds canopy.json, launches the project
// TUI. On exit, we refresh so any new workspaces / status changes show
// up.
func (m *GlobalModel) goToProject(row state.GlobalRow) tea.Cmd {
	root, err := resolveProjectRoot(row, m.list.Rows())
	if err != nil {
		return func() tea.Msg { return globalErrMsg{err: err} }
	}
	exe, err := os.Executable()
	if err != nil {
		return func() tea.Msg {
			return globalErrMsg{err: fmt.Errorf("locate canopy binary: %w", err)}
		}
	}
	cmd := exec.Command(exe)
	cmd.Dir = root
	// Inherit env. Notably, we do NOT set TMUX or CANOPY_WORKSPACE_PATH;
	// the inner canopy treats this as a normal invocation from the project
	// root and routes to the project TUI. We DO set CANOPY_FROM_GLOBAL=1
	// so the inner TUI knows it was launched from the global view and can
	// expose a `b`/`esc` "back" key that just quits — returning control
	// here, where ExecProcess's onExit refresh re-renders the global list.
	cmd.Env = append(os.Environ(), "CANOPY_FROM_GLOBAL=1")
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			return globalErrMsg{err: fmt.Errorf("open project at %s: %w", root, err)}
		}
		// Inner canopy quit cleanly — refresh in case workspaces changed.
		return m.refreshCmd()()
	})
}

// resolveProjectRoot returns the canonical absolute project root for the
// given row. Tries ProjectRoot first (post-migration); on a v1 unmigrated
// row whose ProjectRoot is just a basename, falls back to deriving the
// source repo from any sibling workspace's worktree via git common-dir.
//
// Sibling fallback (for main-only rows with no own Path): scan allRows
// for any row in the same project group that has a Path, and use that.
// This keeps the resolution best-effort without requiring a project
// registry.
func resolveProjectRoot(row state.GlobalRow, allRows []state.GlobalRow) (string, error) {
	// Case 1: ProjectRoot is already a canonical absolute path.
	if filepath.IsAbs(row.ProjectRoot) {
		return row.ProjectRoot, nil
	}

	// Case 2: this row has its own worktree Path (workspace rows do).
	if row.Path != "" {
		if root, err := git.SourceRepoFromWorktree(context.Background(), row.Path); err == nil {
			return root, nil
		}
	}

	// Case 3: a main-only row has no Path of its own. Find a sibling
	// workspace row in the same project group and derive from its Path.
	for _, sibling := range allRows {
		if sibling.Project == row.Project && sibling.Path != "" {
			if root, err := git.SourceRepoFromWorktree(context.Background(), sibling.Path); err == nil {
				return root, nil
			}
		}
	}

	// All fallbacks exhausted — surface a clear error.
	return "", fmt.Errorf(
		"can't open project %q: no canonical root path on file. "+
			"Run any canopy command from the project's source repo once to migrate state.json (e.g. cd into the repo and run `canopy ls`).",
		row.Project)
}
