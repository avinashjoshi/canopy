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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/oncactus/canopy/internal/git"
	"github.com/oncactus/canopy/internal/lifecycle"
	"github.com/oncactus/canopy/internal/state"
	"github.com/oncactus/canopy/internal/tmux"
	"github.com/oncactus/canopy/internal/ui/projectlist"
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

	// popupSwitchClient, when non-nil, rewires Enter behavior on alive
	// rows: instead of `tmux attach` (which would nest tmux clients and
	// fail), we invoke this callback (typically `tmux switch-client -t
	// <session>`) and quit the model — closing the popup. Set by
	// AsPopup; nil in default global-TUI mode.
	popupSwitchClient func(session string) error

	// popupAllRows is the full row set as last loaded from state. Cached
	// here so tab-switches and search-query changes can re-filter without
	// hitting state.json again. Only populated in popup mode.
	popupAllRows []state.GlobalRow

	// popupCurrentProject is the absolute project root path of the
	// canopy.json walked up from the popup-host pane's cwd, or "" if
	// the user pressed the popup keybind from outside any canopy
	// project. Drives the Local tab's filter — rows where
	// row.ProjectRoot == popupCurrentProject. "" disables the Local
	// tab (it's still rendered, but selecting it shows zero rows).
	popupCurrentProject string

	// popupTab tracks which tab is active. popupTabLocal filters to
	// rows in popupCurrentProject; popupTabGlobal shows all rows.
	popupTab popupTab

	// popupSearchMode is true while the user is typing in the search
	// box (after pressing /). Captures keystrokes into popupSearchQuery
	// instead of forwarding to projectlist's nav.
	popupSearchMode bool

	// popupSearchQuery is the current fuzzy-search filter string. Empty
	// means no filter. Filters rows by case-insensitive subsequence
	// match against workspace name and project name.
	popupSearchQuery string
}

// popupTab identifies which tab is visible in the popup.
type popupTab int

const (
	// popupTabLocal shows only workspaces in the current project (the
	// canopy.json walked up from the popup-host pane's cwd). The
	// "scope is what I'm working on right now" view.
	popupTabLocal popupTab = iota

	// popupTabGlobal shows every workspace canopy knows about across
	// all projects. The "give me everything" view.
	popupTabGlobal
)

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

// AsPopup configures the model for tmux popup hosting. The switchClient
// callback runs instead of `tmux attach` when the user presses Enter on
// an alive row; the model quits afterward (which closes the popup).
//
// currentProject is the absolute path to the canopy.json's project root
// resolved from the popup host pane's cwd, or "" if the user invoked
// the popup outside any canopy project. Drives the Local tab.
//
// Why an injected callback rather than calling tmux directly: keeps the
// internal/ui package free of tmux-process-management concerns and lets
// cmd/canopy/popup_inner.go compose the popup behavior from primitives
// it already imports.
//
// Returns the model so callers can chain: `NewGlobal(...).AsPopup(...)`.
func (m *GlobalModel) AsPopup(switchClient func(session string) error, currentProject string) *GlobalModel {
	m.popupSwitchClient = switchClient
	m.popupCurrentProject = currentProject
	// Default tab: Local if there's a current project, otherwise Global.
	// "I pressed the popup keybind from inside a project" → most likely
	// I want to switch among that project's workspaces first.
	if currentProject != "" {
		m.popupTab = popupTabLocal
	} else {
		m.popupTab = popupTabGlobal
	}
	return m
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
		// In popup mode, cache the full row set so tab-switches and
		// search-query changes can re-filter without re-fetching state.
		// In default mode this is dead state — list.SetRows takes the
		// full set directly below.
		if m.popupSwitchClient != nil {
			m.popupAllRows = msg.rows
			m.list.SetRows(m.filteredPopupRows())
		} else {
			m.list.SetRows(msg.rows)
		}
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

		// Popup-mode key handling (search box + tab cycling). Only
		// active when popupSwitchClient is set; default canopy
		// invocation skips this block entirely.
		if m.popupSwitchClient != nil {
			if next, cmd, handled := m.handlePopupKey(msg); handled {
				return next, cmd
			}
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
	b.WriteString("\n")

	// Popup-mode chrome: tab bar + search hint above the table. Default
	// canopy invocation gets the original (no chrome, just a blank line).
	if m.popupSwitchClient != nil {
		b.WriteString("\n")
		b.WriteString(m.renderPopupTabBar())
		b.WriteString("    ")
		b.WriteString(m.renderPopupSearchLine())
		b.WriteString("\n\n")
	} else {
		b.WriteString("\n")
	}

	b.WriteString(m.list.View())
	b.WriteString("\n")
	b.WriteString(m.renderHelpLine())
	return b.String()
}

func (m *GlobalModel) renderHelpLine() string {
	keys := []string{
		"↑/↓ navigate",
		"enter " + m.activateLabel(),
		"o open project",
		"r refresh",
		"? help",
		"q quit",
	}
	if m.popupSwitchClient != nil {
		// Insert tab/search hints at the front; popup users care about
		// these more than the standard nav hints.
		popupKeys := []string{
			"tab switch-tab",
			"/ search",
		}
		keys = append(popupKeys, keys...)
	}
	return subtleStyle.Render(strings.Join(keys, "  ·  "))
}

// activateLabel returns the verb shown next to "enter" in the help
// line. Default mode says "attach" (the historical behavior); popup
// mode says "switch" because we fire switch-client + quit instead.
func (m *GlobalModel) activateLabel() string {
	if m.popupSwitchClient != nil {
		return "switch"
	}
	return "attach"
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
	// Popup-mode override: for alive rows, switch-client + quit instead
	// of attach. Non-alive rows surface a popup-specific hint (NOT the
	// default-mode `o` hint, because `o` is broken in popup mode — see
	// goToProject for why).
	if m.popupSwitchClient != nil {
		if (row.IsMain && row.Alive) || row.Status == state.StatusReady {
			return m.popupSwitchAndQuit(row.TmuxSession)
		}
		return m.popupHintForNonAlive(row)
	}

	// Main rows: attach if alive, else hint the user toward
	// `canopy main` to start the session. The row is always
	// rendered (so the user knows the project has a main concept)
	// but enter only attaches when there's a live session waiting.
	if row.IsMain {
		if row.Alive {
			return m.attachCmd(row.TmuxSession)
		}
		hint := fmt.Errorf(
			"%s main session not running — press `o` to open the project, "+
				"then run `canopy main` from there to start it",
			row.Project)
		return func() tea.Msg { return globalErrMsg{err: hint} }
	}
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
// goToProjectEnv returns the env vars passed to a spawned canopy when
// the user "opens a project" from the global TUI. Extracted so tests
// can assert the load-bearing vars survive future refactors.
//
//   - CANOPY_FROM_GLOBAL=1 tells the inner project TUI it was
//     launched from the global view, enabling its back-key (b/esc)
//     to just quit instead of falling back to ctrl-c semantics. The
//     refresh on exit then re-renders the global list with any
//     state changes the inner TUI made.
//
//   - CANOPY_ALLOW_NESTED=1 bypasses the inner canopy's nested-tmux
//     guard. Without it, the spawned canopy refuses (exit 1) because
//     it sees TMUX set in its env — which is true for popup mode
//     AND for any default-mode invocation that happens to be inside
//     a tmux session. Both should "just work"; the guard is meant
//     for guarding against humans accidentally nesting workspace
//     creation, not for blocking the global-TUI's own goToProject
//     spawn. Safe because the inner canopy in project-TUI mode reads
//     state and routes destructive verbs through the existing tmux
//     client; it doesn't spawn nested tmux sessions during a TUI
//     session.
func goToProjectEnv() []string {
	return []string{
		"CANOPY_FROM_GLOBAL=1",
		"CANOPY_ALLOW_NESTED=1",
	}
}

// popupHintForNonAlive returns a hint cmd for a non-alive row in popup
// mode. The hint points at a shell-based recovery action (close popup,
// run a canopy command from a shell) because the in-popup `o` recovery
// path doesn't work — see goToProject for the why.
//
// The popup stays open so the user can read the hint, press q to close,
// then act. We don't auto-quit because they may want to switch to a
// different (alive) workspace from the same popup.
func (m *GlobalModel) popupHintForNonAlive(row state.GlobalRow) tea.Cmd {
	var msg string
	switch {
	case row.IsMain && !row.Alive:
		msg = fmt.Sprintf(
			"%s main session not running — press q to close, then run `canopy main` from %s",
			row.Project, row.Project)
	case row.Status == state.StatusStopped:
		msg = fmt.Sprintf(
			"%s is stopped — press q to close, then run `canopy switch %s` from a shell to resurrect it",
			row.Name, row.Name)
	case row.Status == state.StatusBroken:
		msg = fmt.Sprintf(
			"%s is broken — press q to close, then run `canopy retry %s` from a shell to re-run setup",
			row.Name, row.Name)
	case row.Status == state.StatusOrphaned:
		msg = fmt.Sprintf(
			"%s has no on-disk dir — press q to close, then run `canopy rm %s` from a shell to clean up",
			row.Name, row.Name)
	case row.Status == state.StatusSettingUp:
		msg = fmt.Sprintf("%s is still setting up — press r to refresh in a moment", row.Name)
	default:
		msg = fmt.Sprintf("%s is in status %q — can't switch from the popup", row.Name, row.Status)
	}
	hint := fmt.Errorf("%s", msg)
	return func() tea.Msg { return globalErrMsg{err: hint} }
}

// popupSwitchAndQuit fires the popup-mode switch-client callback then
// quits the model — closing the tmux popup. Errors from the callback
// are surfaced via globalErrMsg AND the model still quits, since the
// popup is ephemeral and tmux's own error message will reach the user
// via the parent client. Either way the popup goes away.
func (m *GlobalModel) popupSwitchAndQuit(session string) tea.Cmd {
	return func() tea.Msg {
		if m.popupSwitchClient != nil {
			if err := m.popupSwitchClient(session); err != nil {
				// Switch failed — let the parent client see tmux's
				// error. We still quit (return Quit) on the next
				// tea-loop turn via Sequence; but a single Msg can't
				// carry "show error THEN quit". Choose: prioritize
				// quit because the popup hanging open with an error
				// message is worse UX than the popup disappearing
				// and tmux flashing its own error in the parent bar.
				_ = err // logged inside the callback
			}
		}
		return tea.QuitMsg{}
	}
}

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
	env := append(os.Environ(), goToProjectEnv()...)
	if m.popupSwitchClient != nil {
		// Tell the inner project TUI it's hosted in a popup. The inner
		// TUI uses this to exit with code 7 after a successful attach,
		// which we catch below to close the popup automatically. Without
		// this signal the popup stays open after attach (user has to
		// press q twice — once to leave project TUI, once to leave
		// popup).
		env = append(env, "CANOPY_FROM_POPUP=1")
	}
	cmd.Env = env
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			// Exit code 7 from the inner canopy is the popup-attach
			// signal: "user picked a workspace; close the popup."
			// Only honor it in popup mode (popupSwitchClient != nil).
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 7 && m.popupSwitchClient != nil {
				return tea.QuitMsg{}
			}
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

// ─── Popup-mode key handling, filtering, and view ───────────────────
// Everything below is live only when popupSwitchClient is set (see
// AsPopup). Default `canopy` invocation never reaches this code.

// handlePopupKey handles popup-only keys: search-mode keystrokes,
// Tab to cycle tabs, / to enter search. Returns (model, cmd, true) if
// it consumed the key; (model, nil, false) to let the parent Update
// fall through to default key handling.
func (m *GlobalModel) handlePopupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	// Search-mode keystrokes: capture into popupSearchQuery, refilter
	// rows on every change.
	if m.popupSearchMode {
		switch msg.Type {
		case tea.KeyEsc:
			// Esc clears the query and exits search mode.
			m.popupSearchMode = false
			m.popupSearchQuery = ""
			m.list.SetRows(m.filteredPopupRows())
			return m, nil, true
		case tea.KeyEnter:
			// Enter exits search mode keeping the query, so the user
			// can navigate the filtered list with arrow keys.
			m.popupSearchMode = false
			return m, nil, true
		case tea.KeyBackspace:
			if len(m.popupSearchQuery) > 0 {
				// Trim one rune (handle multibyte cleanly).
				runes := []rune(m.popupSearchQuery)
				m.popupSearchQuery = string(runes[:len(runes)-1])
				m.list.SetRows(m.filteredPopupRows())
			}
			return m, nil, true
		case tea.KeyRunes:
			m.popupSearchQuery += string(msg.Runes)
			m.list.SetRows(m.filteredPopupRows())
			return m, nil, true
		case tea.KeySpace:
			m.popupSearchQuery += " "
			m.list.SetRows(m.filteredPopupRows())
			return m, nil, true
		}
		return m, nil, true // swallow other keys while in search
	}

	// Non-search popup keys.
	switch msg.String() {
	case "tab":
		m.popupTab = (m.popupTab + 1) % 2
		m.list.SetRows(m.filteredPopupRows())
		return m, nil, true
	case "/":
		m.popupSearchMode = true
		return m, nil, true
	}
	return m, nil, false
}

// filteredPopupRows applies the current tab + search filter to
// popupAllRows and returns the filtered view. Called whenever rows are
// loaded, the tab changes, or the search query changes.
//
// Tab filter is applied first (cheap, O(N) on a single field), then
// search filter (O(N*M) where M = query length). For canopy's expected
// scale (<100 rows), neither is hot.
func (m *GlobalModel) filteredPopupRows() []state.GlobalRow {
	rows := m.popupAllRows

	// Apply tab filter.
	if m.popupTab == popupTabLocal && m.popupCurrentProject != "" {
		filtered := make([]state.GlobalRow, 0, len(rows))
		for _, r := range rows {
			if r.ProjectRoot == m.popupCurrentProject {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	// Apply search filter.
	if m.popupSearchQuery != "" {
		q := strings.ToLower(m.popupSearchQuery)
		filtered := make([]state.GlobalRow, 0, len(rows))
		for _, r := range rows {
			if popupRowMatchesQuery(r, q) {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	return rows
}

// popupRowMatchesQuery returns true if the lowercased query is a
// subsequence of the row's name, project, OR branch. Subsequence (not
// contiguous) gives fzf-style matching: "sf" matches "silent-falcon",
// "fix-bug" matches a row whose branch is "feat/fix-bug-123", etc.
// The query is already lowercased by the caller; this function
// lowercases the row fields for comparison.
//
// Trade-off: subsequence over substring catches more typos but yields
// false positives ("a" matches everything). Bounded by short queries
// (1-2 chars usually) and small N (<100 rows).
func popupRowMatchesQuery(r state.GlobalRow, lowerQuery string) bool {
	if isSubseq(strings.ToLower(r.Name), lowerQuery) {
		return true
	}
	if isSubseq(strings.ToLower(r.Project), lowerQuery) {
		return true
	}
	if isSubseq(strings.ToLower(r.Branch), lowerQuery) {
		return true
	}
	return false
}

// isSubseq returns true if needle's characters appear in haystack in
// order (not necessarily contiguous). Both strings are expected to be
// already lowercased by the caller. Empty needle matches everything.
func isSubseq(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	hi, ni := 0, 0
	for hi < len(haystack) && ni < len(needle) {
		if haystack[hi] == needle[ni] {
			ni++
		}
		hi++
	}
	return ni == len(needle)
}

// renderPopupTabBar returns the tab indicator line. Active tab is
// boxed with brackets, inactive tab is plain. If popupCurrentProject
// is empty (popup launched outside a canopy project), the Local tab
// is rendered grey to signal it's available but empty.
func (m *GlobalModel) renderPopupTabBar() string {
	localLabel := "Local"
	if m.popupCurrentProject != "" {
		// Show the project name so the user knows what "Local" means
		// in this invocation. Truncate aggressively — the bar must
		// fit on narrow popups.
		proj := filepath.Base(m.popupCurrentProject)
		if len(proj) > 16 {
			proj = proj[:16]
		}
		localLabel = "Local: " + proj
	}

	var local, global string
	if m.popupTab == popupTabLocal {
		local = activeTabStyle.Render("[ " + localLabel + " ]")
		global = inactiveTabStyle.Render("  Global  ")
	} else {
		local = inactiveTabStyle.Render("  " + localLabel + "  ")
		global = activeTabStyle.Render("[ Global ]")
	}

	bar := local + " " + global
	if m.popupCurrentProject == "" {
		bar = subtleStyle.Render("  Local (no project)  ") + " " + activeTabStyle.Render("[ Global ]")
	}
	return bar
}

// renderPopupSearchLine returns the search input box (when in search
// mode) or a hint about how to enter search mode. Shown above the
// table so it doesn't compete with the help line at the bottom.
func (m *GlobalModel) renderPopupSearchLine() string {
	if m.popupSearchMode {
		return searchActiveStyle.Render("/" + m.popupSearchQuery + "█")
	}
	if m.popupSearchQuery != "" {
		return subtleStyle.Render("filter: " + m.popupSearchQuery + "  (esc to clear)")
	}
	return subtleStyle.Render("tab: switch  ·  / search")
}

// activeTabStyle, inactiveTabStyle, searchActiveStyle define the popup
// chrome. Defined as vars rather than consts so lipgloss.Style methods
// (which return new Style values) chain cleanly.
var (
	activeTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("231")). // bright white
			Background(lipgloss.Color("62"))   // blue, matches title

	inactiveTabStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("241")) // muted grey

	searchActiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("231")).
				Background(lipgloss.Color("236")).
				Padding(0, 1)
)
