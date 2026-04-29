package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/ghx"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// Update implements tea.Model. Routes incoming messages to focused
// handlers. The Model is always returned by value — Bubbletea owns the
// "current" Model, we own the next one.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case rowsLoadedMsg:
		// Refresh result. Apply rows; clamp the cursor if the list shrank.
		m.err = msg.err
		if msg.rows != nil {
			m.rows = msg.rows
		}
		if m.cursor >= len(m.rows) {
			m.cursor = max0(len(m.rows) - 1)
		}
		// Phase 2: kick off per-row hint loaders in parallel. Each
		// returns a rowHintsMsg as it completes. Skipped on error
		// (no rows to decorate) and on empty lists.
		if msg.err != nil || len(m.rows) == 0 {
			return m, nil
		}
		return m, loadRowHintsCmds(m.rows, m.mgr.Cfg.ProjectRoot)

	case rowHintsMsg:
		// Late-arriving lifecycle detector result. Find the row by
		// name and merge the hints in. Silent no-op if the row is
		// gone (e.g. concurrent rm).
		for i := range m.rows {
			if m.rows[i].Name == msg.name {
				m.rows[i].Hints = msg.hints
				break
			}
		}
		return m, nil

	case prListLoadedMsg:
		// PR picker's async loader returned. Stage the results so
		// the View renderer can show the list + clear the loading
		// indicator. Stale arrival (user already left newPRMode)
		// is a no-op except for the ignored merge — won't cause a
		// re-render of the wrong screen because m.mode gates
		// renderNewPR.
		m.newLoading = false
		if msg.err != nil {
			m.newLoadErr = msg.err
			return m, nil
		}
		m.newPRs = msg.prs
		m.listCursor = 0
		return m, nil

	case issueListLoadedMsg:
		m.newLoading = false
		if msg.err != nil {
			m.newLoadErr = msg.err
			return m, nil
		}
		m.newIssues = msg.issues
		m.listCursor = 0
		return m, nil

	case branchListLoadedMsg:
		m.newLoading = false
		if msg.err != nil {
			m.newLoadErr = msg.err
			return m, nil
		}
		m.newBranches = msg.branches
		m.listCursor = 0
		return m, nil

	case attachAfterMsg:
		// Resurrection completed, now attach. Same exec-process flow as
		// the direct ready-status path.
		return m, attachCmd(m.mgr, msg.session)

	case createStartedMsg:
		// First dispatch from createCmd. Kick off the streaming +
		// completion cmds as a batch — both run concurrently.
		return m, tea.Batch(progressTickCmd(msg.buf), waitDoneCmd(msg.done))

	case removeStartedMsg:
		return m, tea.Batch(progressTickCmd(msg.buf), waitRemoveDoneCmd(msg.done))

	case retryStartedMsg:
		return m, tea.Batch(progressTickCmd(msg.buf), waitRetryDoneCmd(msg.done))

	case progressTickMsg:
		// Live streaming output during a Create. Append the new
		// chunk and schedule the next tick — unless the create
		// already finished (busyDone is true), in which case stop
		// ticking. The final flush happens in createDoneMsg below
		// so trailing output isn't lost between the last tick and
		// the goroutine's exit.
		if msg.chunk != "" {
			m.busyOutput += msg.chunk
		}
		if m.busyDone || m.mode != busyMode {
			return m, nil
		}
		return m, progressTickCmd(msg.buf)

	case createDoneMsg:
		// Workspace creation finished. On success we auto-attach to the
		// new workspace's tmux session — that's what the user pressed `n`
		// to do, and dropping them at the workspace beats one extra
		// "press any key" gate. On error we stay in busyMode so the
		// captured setup output is visible for diagnosis; any key
		// dismisses back to the list (handleBusyModeKey).
		m.busyDone = true
		m.busyErr = msg.err
		// Append any trailing output from the buffer drain that the
		// final tick missed. m.busyOutput already contains everything
		// streamed via progressTickMsg; msg.output is just the tail.
		if msg.output != "" {
			m.busyOutput += msg.output
		}
		if msg.err == nil && msg.tmuxSession != "" {
			m.mode = listMode
			m.busyOp = busyOpNone
			m.busyTitle = ""
			m.busyOutput = ""
			m.busyDone = false
			return m, attachCmd(m.mgr, msg.tmuxSession)
		}
		return m, nil

	case removeDoneMsg:
		// Workspace removal finished. busyOutput already contains
		// the streamed archive-script output via progressTickMsg;
		// msg.output is just the trailing bytes that didn't make a
		// tick. Append (not overwrite) to preserve the live stream.
		m.busyDone = true
		m.busyErr = msg.err
		if msg.output != "" {
			m.busyOutput += msg.output
		}
		return m, nil

	case retryDoneMsg:
		// scripts.setup re-run finished. Same shape: append the
		// trailing chunk that the last tick missed; renderBusyView
		// branches on busyOp to label success.
		m.busyDone = true
		m.busyErr = msg.err
		if msg.output != "" {
			m.busyOutput += msg.output
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// In any new-flow sub-mode that owns a textinput, route non-key
	// messages (cursor blink ticks, etc.) through the active input.
	switch m.mode {
	case newFreshMode:
		var cmd tea.Cmd
		m.nameInput, cmd = m.nameInput.Update(msg)
		return m, cmd
	case newPRMode, newIssueMode, newBranchMode:
		var cmd tea.Cmd
		m.listInput, cmd = m.listInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleKey is the keymap. Conductor-flavored: small, opinionated, no
// clever chords. Help is one keypress (?), nav is the standard
// arrow/jk/gG, attach is enter, refresh is r, quit is q.
//
// Modal modes (newMode, busyMode) intercept first; only listMode
// reaches the navigation block at the bottom.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showHelp {
		// Any key dismisses help.
		m.showHelp = false
		return m, nil
	}

	// Mode-specific handling first.
	switch m.mode {
	case newPickerMode:
		return m.handleNewPickerKey(msg)
	case newFreshMode:
		return m.handleNewFreshKey(msg)
	case newPRMode:
		return m.handleNewPRKey(msg)
	case newIssueMode:
		return m.handleNewIssueKey(msg)
	case newBranchMode:
		return m.handleNewBranchKey(msg)
	case confirmDeleteMode:
		return m.handleConfirmDeleteKey(msg)
	case busyMode:
		return m.handleBusyModeKey(msg)
	}

	// listMode keymap.
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "b", "esc":
		// Back to the global TUI. Only meaningful when this project TUI
		// was launched from the global view (the env-var handshake in
		// model_global.go's goToProject). Outside that flow `b` and `esc`
		// would be surprising — `b` could be a future shortcut, and esc
		// historically belongs to modals — so we no-op for the standalone
		// `canopy` invocation.
		if m.fromGlobal {
			return m, tea.Quit
		}
		return m, nil

	case "?":
		m.showHelp = true
		return m, nil

	case "r":
		// Manual refresh. Same flow as the initial load.
		return m, refreshCmd(m.mgr, m.tc)

	case "n":
		// Open the new-workspace flow. Step 1 is the variant picker —
		// pick fresh / pr / issue / branch via single keystroke.
		m.openNewPicker()
		return m, nil

	case "d":
		// Open the confirm-delete modal for the selected row. Refuses
		// to delete the synthetic main row (canopy main is ephemeral —
		// kill the tmux session externally if you want it gone).
		//
		// v0.6: run the workspace safety preflight before showing the
		// modal. When hangs are detected (uncommitted/unpushed/open-PR),
		// the modal renders the list and requires a capital F to force,
		// matching the CLI's `canopy rm --force` semantics. When clean,
		// the modal shows today's normal y/N prompt.
		if len(m.rows) == 0 {
			return m, nil
		}
		row := m.rows[m.cursor]
		if row.IsMain {
			m.err = fmt.Errorf("can't delete the main session via canopy rm — use `tmux kill-session -t %s` if you want it gone",
				row.TmuxSession)
			return m, nil
		}
		// SafetyPreflight returns nil hangs for orphan workspaces (worktree
		// dir gone) — degrade gracefully so the user can still rm orphans.
		// Errors are non-fatal; we proceed with no hangs and let Remove
		// handle the not-found case if state diverged.
		hangs, _ := m.mgr.SafetyPreflight(context.Background(), row.Name)
		m.mode = confirmDeleteMode
		m.deleteTarget = row.Name
		m.deleteHangs = hangs
		return m, nil

	case "enter":
		// Attach to the selected workspace. Resurrects first if the
		// workspace is in `stopped` state. The handoff happens via
		// tea.ExecProcess: tmux takes over the terminal, the user
		// detaches with prefix-d, control returns here, refreshCmd
		// updates the rendered status.
		return m.attachSelected()

	case "R":
		// Retry scripts.setup on a broken workspace. Capital R so it
		// doesn't collide with lowercase r (refresh). Only valid when
		// the selected row is in broken status — we just no-op
		// otherwise. Recovery flow: user fixes the underlying issue
		// (missing config, deps, etc.), presses R, scripts.setup
		// re-runs against the existing worktree.
		if len(m.rows) == 0 {
			return m, nil
		}
		row := m.rows[m.cursor]
		if row.IsMain {
			return m, nil
		}
		if row.Status != state.StatusBroken {
			m.err = fmt.Errorf("retry only applies to broken workspaces; %q is %q",
				row.Name, row.Status)
			return m, nil
		}
		m.mode = busyMode
		m.busyOp = busyOpRetry
		m.busyTitle = fmt.Sprintf("Retrying setup for %q...", row.Name)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		return m, retryCmdUI(m.mgr, row.Name)

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		return m, nil

	case "g", "home":
		m.cursor = 0
		return m, nil

	case "G", "end":
		m.cursor = max0(len(m.rows) - 1)
		return m, nil
	}

	return m, nil
}

// max0 returns max(0, n). Avoids the Bubbletea-standard generic max
// for first-time-Go simplicity (and Go 1.21+ has built-in max but
// we're staying close to the old toolchain to keep the dep surface
// boring).
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// attachSelected wires the enter-key flow: figure out what to do based
// on the selected row's status, build the right exec.Cmd, and hand it
// to tea.ExecProcess. Returns the model + a tea.Cmd; the actual tmux
// handoff happens after the Cmd fires. After detach the followup
// refreshCmd reloads rows so the status the user sees matches reality.
func (m *Model) attachSelected() (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, nil
	}
	row := m.rows[m.cursor]
	ctx := context.Background()

	// Main row: attach if alive, else hint the user toward
	// `canopy main` to start the session. The row is always
	// rendered now, so we have to handle both states explicitly.
	if row.IsMain {
		if row.Alive {
			return m, attachCmd(m.mgr, row.TmuxSession)
		}
		m.err = fmt.Errorf("main session not running — run `canopy main` in a terminal to start it")
		return m, nil
	}

	// Decide what to do based on status. broken/orphaned/setting_up cases
	// surface an error in the TUI rather than handing off to a dead
	// session. ready and main go straight to attach. stopped resurrects
	// first.
	switch row.Status {
	case "main", state.StatusReady:
		// Attach directly.
		return m, attachCmd(m.mgr, row.TmuxSession)

	case state.StatusStopped:
		// Resurrect, then attach. Workspace lookup uses the row's name
		// (which is the canopy workspace name, not the tmux session).
		return m, resurrectAndAttachCmd(m.mgr, row.Name)

	case state.StatusBroken:
		m.err = fmt.Errorf("workspace %q is broken — see ~/.canopy/log/canopy.log; press R to retry scripts.setup, or `canopy rm %s` to drop it",
			row.Name, row.Name)
		return m, nil

	case state.StatusOrphaned:
		m.err = fmt.Errorf("workspace %q has no on-disk dir — run `canopy rm %s` to drop the row",
			row.Name, row.Name)
		return m, nil

	case state.StatusSettingUp:
		m.err = fmt.Errorf("workspace %q is still setting up — try refresh (r) in a moment",
			row.Name)
		return m, nil
	}

	// Unknown status: log and ignore.
	log.Warn("ui.attach.unknown-status", "name", row.Name, "status", row.Status)
	_ = ctx
	return m, nil
}

// attachCmd dispatches tea.ExecProcess against tmux's attach command.
// On detach, sends a refreshCmd so the rendered status updates.
func attachCmd(mgr *workspace.Manager, session string) tea.Cmd {
	cmd, err := mgr.Tmux.AttachCmd(context.Background(), session)
	if err != nil {
		return func() tea.Msg {
			return rowsLoadedMsg{err: err}
		}
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.attach.exec-failed", "session", session, "err", err)
			return rowsLoadedMsg{err: fmt.Errorf("attach %s: %w", session, err)}
		}
		// Detach completed cleanly; refresh rows so any state changes
		// during the session (rare but possible if the user ran canopy
		// commands inside a pane) show up.
		return refreshCmd(mgr, mgr.Tmux)()
	})
}

// resurrectAndAttachCmd handles the stopped-status flow. Runs Resurrect
// (which rebuilds the tmux session with claude --continue || claude),
// then attaches via the normal path.
func resurrectAndAttachCmd(mgr *workspace.Manager, name string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		ws, err := mgr.Resurrect(ctx, name)
		if err != nil {
			return rowsLoadedMsg{err: fmt.Errorf("resurrect %s: %w", name, err)}
		}
		// We can't return a tea.Cmd from a tea.Msg — instead we kick
		// off the attach as a follow-on by returning an attachAfterMsg
		// that Update routes through.
		return attachAfterMsg{session: ws.TmuxSession}
	}
}

// attachAfterMsg is the bridge between resurrectAndAttachCmd's
// async resurrection and the synchronous tea.ExecProcess attach. Update
// catches it and dispatches attachCmd.
type attachAfterMsg struct {
	session string
}

// openNewPicker resets state and opens the variant picker. Called
// from the listMode 'n' keypress and from sub-modal esc handlers
// (back-one-step). Idempotent; safe to call from any mode.
func (m *Model) openNewPicker() {
	m.mode = newPickerMode
	m.newPickerCursor = 0
	m.nameInput.Reset()
	m.nameInput.Blur()
}

// handleNewPickerKey is the keymap for the variant picker (step 1
// of the new-workspace flow). Single-letter shortcuts launch each
// sub-modal directly; arrow-then-enter is the keyboard-discoverable
// alternative for users who scan before they type.
//
// Esc returns to listMode (one step back). q is suppressed here so
// the user can't accidentally quit canopy from inside the picker;
// they have to esc back first. ctrl+c is the global escape hatch.
func (m *Model) handleNewPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = listMode
		return m, nil
	case "ctrl+c":
		return m, tea.Quit

	// Single-key shortcuts — launch the corresponding sub-modal.
	case "n", "f":
		// 'n' = "new (fresh)" — same letter as the keymap, no surprise.
		// 'f' is an alias if the user thinks "fresh".
		return m.openNewFresh(), textinputBlink()
	case "p":
		return m, m.openNewPR()
	case "i":
		return m, m.openNewIssue()
	case "b":
		return m, m.openNewBranch()

	// Arrow nav for keyboard-discovery users.
	case "up", "k":
		if m.newPickerCursor > 0 {
			m.newPickerCursor--
		}
		return m, nil
	case "down", "j":
		if m.newPickerCursor < newPickerOptionCount-1 {
			m.newPickerCursor++
		}
		return m, nil
	case "enter":
		// Same dispatch as the letter shortcuts, just keyed off cursor.
		switch m.newPickerCursor {
		case 0:
			return m.openNewFresh(), textinputBlink()
		case 1:
			return m, m.openNewPR()
		case 2:
			return m, m.openNewIssue()
		case 3:
			return m, m.openNewBranch()
		}
		return m, nil
	}
	return m, nil
}

// newPickerOptionCount is the number of options in the variant
// picker. Used to bound cursor nav. Update if newPickerOption is
// extended.
const newPickerOptionCount = 4

// openNewFresh prepares the fresh-workspace sub-modal (step 2a).
// Reused by the picker's 'n'/'f'/enter-on-Fresh dispatch and any
// future direct-entry shortcut. Returns the model so the caller can
// chain the textinputBlink cmd.
func (m *Model) openNewFresh() *Model {
	m.mode = newFreshMode
	m.nameInput.Reset()
	m.nameInput.Focus()
	return m
}

// handleNewFreshKey is the keymap for the fresh-workspace name input
// (step 2a). Esc steps back to the picker. Enter submits with the
// typed name (or empty → namegen). Anything else falls through to
// the textinput.
func (m *Model) handleNewFreshKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil
	case "enter":
		name := m.nameInput.Value()
		spec := workspace.SourceSpec{} // fresh = zero spec
		m.busyOp = busyOpCreate
		m.busyTitle = newBusyTitle(name, spec)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.mode = busyMode
		m.nameInput.Blur()
		return m, createCmd(m.mgr, name, spec)
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

// openNewPR transitions to the PR picker sub-modal and kicks off the
// async loader. The loader returns prListLoadedMsg; until it arrives,
// the renderer shows a "Loading PRs..." state.
func (m *Model) openNewPR() tea.Cmd {
	m.mode = newPRMode
	m.listInput.Reset()
	m.listInput.Placeholder = "type a PR number, or arrow to a row below"
	m.listInput.Focus()
	m.listCursor = 0
	m.newLoading = true
	m.newLoadErr = nil
	m.newPRs = nil
	return tea.Batch(textinputBlink(), loadPRsCmd(m.mgr.Cfg.ProjectRoot))
}

// prListLoadedMsg carries the result of an async ghx.ListPRs call.
// Update on receipt: clear loading, populate newPRs, surface any
// error inline.
type prListLoadedMsg struct {
	prs []ghx.PRSummary
	err error
}

// loadPRsCmd dispatches ghx.ListPRs in a goroutine. Limit 20 keeps
// the picker scannable; users with > 20 open PRs can still type the
// number directly.
func loadPRsCmd(projectRoot string) tea.Cmd {
	return func() tea.Msg {
		prs, err := ghx.ListPRs(context.Background(), projectRoot, 20)
		return prListLoadedMsg{prs: prs, err: err}
	}
}

// handleNewPRKey is the keymap for the PR picker sub-modal. Two
// dispatch shapes:
//
//   - User types a number: enter creates a workspace from PR #<num>
//     directly (works even when the list is empty / unloaded — covers
//     the "I know the number, just go" power-user path).
//   - User arrows into the loaded list: enter creates from the
//     selected PR (recognition path — see the PR title before
//     committing).
//
// Esc returns to the picker (back-one-step).
func (m *Model) handleNewPRKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil

	case "up", "ctrl+k":
		// Arrow nav on the list. Doesn't consume the textinput's
		// own up-arrow (we don't bind that in textinput) so users
		// can scan without losing typed-in number.
		if m.listCursor > 0 {
			m.listCursor--
		}
		return m, nil
	case "down", "ctrl+j":
		// Bound by FILTERED length so the cursor can't drift past
		// what's visible in the picker.
		if m.listCursor < len(filterPRs(m.newPRs, m.listInput.Value()))-1 {
			m.listCursor++
		}
		return m, nil

	case "enter":
		// Two paths: typed-number wins if the input parses as an
		// integer. Otherwise, fall back to the cursor's selection
		// in the FILTERED list.
		if num, ok := parsePositiveInt(m.listInput.Value()); ok {
			return m.submitNewPR(num)
		}
		filtered := filterPRs(m.newPRs, m.listInput.Value())
		if len(filtered) > 0 && m.listCursor < len(filtered) {
			return m.submitNewPR(filtered[m.listCursor].Number)
		}
		// Nothing typed, no list — surface a hint.
		m.newLoadErr = fmt.Errorf("type a PR number or wait for the list to load")
		return m, nil
	}

	// Forward to textinput. Reset cursor when filter changes so
	// the highlighted row doesn't drift past the visible list.
	prevValue := m.listInput.Value()
	var cmd tea.Cmd
	m.listInput, cmd = m.listInput.Update(msg)
	if m.listInput.Value() != prevValue {
		m.listCursor = 0
		m.newLoadErr = nil
	}
	return m, cmd
}

// submitNewPR is the shared "go fetch this PR and create the
// workspace" path used by both enter-with-number and enter-on-row.
// Flips to busyMode and dispatches the existing createCmd; the
// resolver does the gh + git fetch in the goroutine.
func (m *Model) submitNewPR(num int) (tea.Model, tea.Cmd) {
	spec := workspace.SourceSpec{PR: num}
	m.busyOp = busyOpCreate
	m.busyTitle = newBusyTitle("", spec)
	m.busyDone = false
	m.busyOutput = ""
	m.busyErr = nil
	m.mode = busyMode
	m.listInput.Blur()
	return m, createCmd(m.mgr, "", spec)
}

// openNewIssue is the issue-picker analog of openNewPR. Same shape,
// different data type: ghx.IssueSummary instead of PRSummary.
func (m *Model) openNewIssue() tea.Cmd {
	m.mode = newIssueMode
	m.listInput.Reset()
	m.listInput.Placeholder = "type an issue number, or arrow to a row below"
	m.listInput.Focus()
	m.listCursor = 0
	m.newLoading = true
	m.newLoadErr = nil
	m.newIssues = nil
	return tea.Batch(textinputBlink(), loadIssuesCmd(m.mgr.Cfg.ProjectRoot))
}

// issueListLoadedMsg is the issue analog of prListLoadedMsg.
type issueListLoadedMsg struct {
	issues []ghx.IssueSummary
	err    error
}

// loadIssuesCmd dispatches ghx.ListIssues in a goroutine.
func loadIssuesCmd(projectRoot string) tea.Cmd {
	return func() tea.Msg {
		issues, err := ghx.ListIssues(context.Background(), projectRoot, 20)
		return issueListLoadedMsg{issues: issues, err: err}
	}
}

// handleNewIssueKey mirrors handleNewPRKey for issues. Two enter
// dispatch shapes: typed-number → fetch by ID; arrow-then-enter →
// use cursor's selection. Esc returns to picker.
func (m *Model) handleNewIssueKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil

	case "up", "ctrl+k":
		if m.listCursor > 0 {
			m.listCursor--
		}
		return m, nil
	case "down", "ctrl+j":
		if m.listCursor < len(filterIssues(m.newIssues, m.listInput.Value()))-1 {
			m.listCursor++
		}
		return m, nil

	case "enter":
		if num, ok := parsePositiveInt(m.listInput.Value()); ok {
			return m.submitNewIssue(num)
		}
		filtered := filterIssues(m.newIssues, m.listInput.Value())
		if len(filtered) > 0 && m.listCursor < len(filtered) {
			return m.submitNewIssue(filtered[m.listCursor].Number)
		}
		m.newLoadErr = fmt.Errorf("type an issue number or wait for the list to load")
		return m, nil
	}

	prev := m.listInput.Value()
	var cmd tea.Cmd
	m.listInput, cmd = m.listInput.Update(msg)
	if m.listInput.Value() != prev {
		m.listCursor = 0
		m.newLoadErr = nil
	}
	return m, cmd
}

// submitNewIssue is the shared "go fetch this issue and create the
// workspace" path. Same shape as submitNewPR.
func (m *Model) submitNewIssue(num int) (tea.Model, tea.Cmd) {
	spec := workspace.SourceSpec{Issue: num}
	m.busyOp = busyOpCreate
	m.busyTitle = newBusyTitle("", spec)
	m.busyDone = false
	m.busyOutput = ""
	m.busyErr = nil
	m.mode = busyMode
	m.listInput.Blur()
	return m, createCmd(m.mgr, "", spec)
}

// openNewBranch is the branch-picker analog. Doesn't need gh —
// `git for-each-ref` is fast enough that we can load synchronously
// in the open path. Loading state is kept for parity with PR/issue
// pickers and to handle the (rare) slow-disk case.
func (m *Model) openNewBranch() tea.Cmd {
	m.mode = newBranchMode
	m.listInput.Reset()
	m.listInput.Placeholder = "type to filter, e.g. `feat`"
	m.listInput.Focus()
	m.listCursor = 0
	m.newLoading = true
	m.newLoadErr = nil
	m.newBranches = nil
	return tea.Batch(textinputBlink(), loadBranchesCmd(m.mgr.Cfg.ProjectRoot))
}

// branchListLoadedMsg carries the result of an async git
// for-each-ref. Same shape as the PR/issue load messages.
type branchListLoadedMsg struct {
	branches []string
	err      error
}

// loadBranchesCmd dispatches git.ListBranches in a goroutine. Even
// though git is fast, putting it in a goroutine keeps the open
// path consistent (bubbletea Cmd → Msg) and avoids blocking the
// initial render.
func loadBranchesCmd(projectRoot string) tea.Cmd {
	return func() tea.Msg {
		branches, err := git.ListBranches(context.Background(), projectRoot)
		return branchListLoadedMsg{branches: branches, err: err}
	}
}

// handleNewBranchKey is the keymap for the branch picker. Filter
// behavior matches PR/issue pickers (case-insensitive substring),
// but enter takes a STRING (the branch name) instead of a number.
// No "type by name and submit" fast path because branch names can
// contain slashes that conflict with the filter — the user is
// expected to filter then pick.
func (m *Model) handleNewBranchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil

	case "up", "ctrl+k":
		if m.listCursor > 0 {
			m.listCursor--
		}
		return m, nil
	case "down", "ctrl+j":
		// Bound by the filtered length, not the raw length, so the
		// cursor doesn't drift past visible rows.
		if m.listCursor < len(filterBranches(m.newBranches, m.listInput.Value()))-1 {
			m.listCursor++
		}
		return m, nil

	case "enter":
		filtered := filterBranches(m.newBranches, m.listInput.Value())
		if len(filtered) == 0 {
			m.newLoadErr = fmt.Errorf("no branches match — adjust filter or check your remote")
			return m, nil
		}
		idx := m.listCursor
		if idx >= len(filtered) {
			idx = len(filtered) - 1
		}
		// Strip the "origin/" prefix if present so the resolver's
		// origin-vs-local logic sees a bare branch name. The branch
		// resolver already handles both routes; passing the bare
		// name is the unambiguous form.
		ref := filtered[idx]
		bare := strings.TrimPrefix(ref, "origin/")
		// AllowLocal flips on when the picked entry is a local-only
		// branch (no origin/<name> alongside it). Detect that by
		// checking whether the matching origin/<bare> exists in the
		// list.
		spec := workspace.SourceSpec{Branch: bare}
		if !strings.HasPrefix(ref, "origin/") {
			// Local-only entry. The resolver requires --allow-local
			// when the branch isn't on origin.
			if !branchHasOrigin(m.newBranches, bare) {
				spec.AllowLocal = true
			}
		}
		return m.submitNewBranch(spec)
	}

	prev := m.listInput.Value()
	var cmd tea.Cmd
	m.listInput, cmd = m.listInput.Update(msg)
	if m.listInput.Value() != prev {
		m.listCursor = 0
		m.newLoadErr = nil
	}
	return m, cmd
}

// submitNewBranch flips to busyMode with a SourceSpec for the
// chosen branch. Allows the SourceSpec to carry AllowLocal for
// local-only branches.
func (m *Model) submitNewBranch(spec workspace.SourceSpec) (tea.Model, tea.Cmd) {
	m.busyOp = busyOpCreate
	m.busyTitle = newBusyTitle("", spec)
	m.busyDone = false
	m.busyOutput = ""
	m.busyErr = nil
	m.mode = busyMode
	m.listInput.Blur()
	return m, createCmd(m.mgr, "", spec)
}

// branchHasOrigin returns true when the ListBranches output contains
// "origin/<bare>" alongside the local "<bare>". Used by the branch
// picker to decide whether AllowLocal should be set on the spec.
func branchHasOrigin(branches []string, bare string) bool {
	target := "origin/" + bare
	for _, b := range branches {
		if b == target {
			return true
		}
	}
	return false
}

// filterBranches narrows the loaded branch list. Case-insensitive
// substring match against the full ref (so "origin/feat" can match
// "origin/feat/oauth" and a typed "feat" matches both local + remote).
func filterBranches(branches []string, filter string) []string {
	filter = strings.TrimSpace(strings.ToLower(filter))
	if filter == "" {
		return branches
	}
	out := make([]string, 0, len(branches))
	for _, b := range branches {
		if strings.Contains(strings.ToLower(b), filter) {
			out = append(out, b)
		}
	}
	return out
}

// parsePositiveInt is the shared "is this a PR/issue number" check
// for the picker enter handlers. Returns (n, true) only for integers
// > 0; "0" / "-1" / "abc" / "" all return (_, false).
func parsePositiveInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// newBusyTitle picks the busy-mode title shown while a Create
// operation is in flight. Customized per source variant so the user
// sees "Checking out PR #1234..." instead of a generic spinner —
// useful because the gh + git fetch can take a few seconds before
// scripts.setup even starts.
func newBusyTitle(name string, spec workspace.SourceSpec) string {
	switch {
	case spec.PR > 0:
		return fmt.Sprintf("Checking out PR #%d...", spec.PR)
	case spec.Issue > 0:
		return fmt.Sprintf("Setting up workspace for issue #%d...", spec.Issue)
	case spec.Branch != "":
		return fmt.Sprintf("Checking out branch %q...", spec.Branch)
	}
	if name != "" {
		return fmt.Sprintf("Creating workspace %q...", name)
	}
	return "Creating workspace..."
}

// handleBusyModeKey: the busy view shows the in-progress or completed
// workspace creation. While in progress, every key is ignored. Once
// done (busyDone=true), any key dismisses the view and triggers a
// refresh so the new row shows up.
func (m *Model) handleBusyModeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.busyDone {
		return m, nil
	}
	// Dismiss + refresh.
	m.mode = listMode
	m.busyOutput = ""
	m.busyTitle = ""
	m.busyDone = false
	m.busyOp = busyOpNone
	if m.busyErr != nil {
		m.err = m.busyErr
		m.busyErr = nil
	}
	return m, refreshCmd(m.mgr, m.tc)
}

// handleConfirmDeleteKey is the keymap while the delete prompt is up.
//
// Two modes based on whether v0.6 SafetyPreflight detected hangs:
//
//   - Clean (no hangs): y or Y kicks off the removal. Lowercase y is
//     an explicit choice for safety (no accidental keypress) but
//     forgiving for the muscle-memory case (most users hit y).
//   - Hanging work: ONLY capital F kicks off the (forced) removal.
//     Lowercase y, n, N, esc, anything else cancels. Capital F mirrors
//     the CLI's --force flag and makes the user's destructive intent
//     explicit ("yes, lose the uncommitted work").
//
// Cancel-by-default is the safe posture for a destructive operation;
// we only proceed on a deliberate keypress.
func (m *Model) handleConfirmDeleteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	hasHangs := len(m.deleteHangs) > 0

	// Force key (capital F): only valid path when hangs exist. Falls
	// through to cancel when no hangs (capital F isn't documented for
	// the clean path, but isn't harmful — just resets the modal).
	if msg.String() == "F" && hasHangs {
		name := m.deleteTarget
		m.mode = busyMode
		m.busyOp = busyOpRemove
		m.busyTitle = fmt.Sprintf("Force-removing workspace %q...", name)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.deleteTarget = ""
		m.deleteHangs = nil
		return m, removeCmd(m.mgr, name)
	}

	// Normal y/Y: only valid when no hangs. When hangs exist, lowercase
	// y is an UNDER-AGREEMENT — user said "yes" to the wrong question.
	// Treat as cancel to force them to acknowledge the hangs explicitly.
	if !hasHangs && (msg.String() == "y" || msg.String() == "Y") {
		name := m.deleteTarget
		m.mode = busyMode
		m.busyOp = busyOpRemove
		m.busyTitle = fmt.Sprintf("Removing workspace %q...", name)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.deleteTarget = ""
		m.deleteHangs = nil
		return m, removeCmd(m.mgr, name)
	}

	// Anything else cancels (n, N, esc, enter, stray keys, lowercase y
	// when hangs are present).
	m.mode = listMode
	m.deleteTarget = ""
	m.deleteHangs = nil
	return m, nil
}

// createDoneMsg carries the result of a Manager.Create call back to
// Update. Output is whatever Create wrote to its stdout/stderr writers.
// tmuxSession is the new workspace's session name on success — Update
// uses it to dispatch an immediate attachCmd so the user lands in the
// running session right after `n` instead of bouncing back to the list.
type createDoneMsg struct {
	output      string
	tmuxSession string
	err         error
}

// removeDoneMsg is the Remove counterpart to createDoneMsg. Same shape;
// kept distinct so future Update logic can branch (e.g. don't try to
// attach to a workspace that was just removed).
type removeDoneMsg struct {
	output string
	err    error
}

// retryDoneMsg is the RetrySetup counterpart. Same shape as the others;
// kept distinct so the success-message branch in renderBusyView (and
// any future post-retry behavior, e.g. auto-attach on success) can
// pivot on type rather than spelunking the busyOp field.
type retryDoneMsg struct {
	output string
	err    error
}

// removeStartedMsg / retryStartedMsg are the lazy-spawn bridges for
// the remove + retry flows, mirroring createStartedMsg. Update on
// receipt: dispatch tea.Batch(progressTickCmd, waitXDoneCmd) so the
// archive / setup output streams live just like Create.
type removeStartedMsg struct {
	buf  *safeBuffer
	done <-chan removeDoneMsg
}

type retryStartedMsg struct {
	buf  *safeBuffer
	done <-chan retryDoneMsg
}

// retryCmdUI kicks off Manager.RetrySetup asynchronously and streams
// its output via the same safeBuffer + progressTick pattern as
// createCmd. The "UI" suffix disambiguates from cmd/canopy/retry.go's
// cobra retryCmd.
func retryCmdUI(mgr *workspace.Manager, name string) tea.Cmd {
	return func() tea.Msg {
		buf := &safeBuffer{}
		done := make(chan retryDoneMsg, 1)
		go func() {
			_, err := mgr.RetrySetup(context.Background(), name, buf, buf)
			done <- retryDoneMsg{output: buf.Drain(), err: err}
		}()
		return retryStartedMsg{buf: buf, done: done}
	}
}

// removeCmd kicks off Manager.Remove asynchronously and streams the
// archive script's output via the same pattern as createCmd. Same
// lazy-spawn shape so unit tests that inspect the cmd value don't
// kick off real work.
func removeCmd(mgr *workspace.Manager, name string) tea.Cmd {
	return func() tea.Msg {
		buf := &safeBuffer{}
		done := make(chan removeDoneMsg, 1)
		go func() {
			err := mgr.Remove(context.Background(), name, buf, buf)
			done <- removeDoneMsg{output: buf.Drain(), err: err}
		}()
		return removeStartedMsg{buf: buf, done: done}
	}
}

// waitRemoveDoneCmd / waitRetryDoneCmd block on the respective done
// chans and emit the corresponding done-msg. Per-channel-type
// helpers because Go's chan types don't unify into a generic
// waitDoneCmd without type parameters.
func waitRemoveDoneCmd(done <-chan removeDoneMsg) tea.Cmd {
	return func() tea.Msg { return <-done }
}

func waitRetryDoneCmd(done <-chan retryDoneMsg) tea.Cmd {
	return func() tea.Msg { return <-done }
}

// createCmd kicks off Manager.Create asynchronously and streams its
// stdout/stderr to the busy view as it runs. spec drives the source
// variant (zero spec = fresh workspace; populated spec = pr/issue/
// branch). The gh shellouts + git fetches happen inside ResolveSource,
// then mgr.Create runs scripts.setup which is the slow, output-y
// part — that's what the user wants to see scroll past in real time.
//
// Mechanism:
//
//   - A safeBuffer captures everything written to the
//     stdout/stderr writers passed to mgr.Create.
//   - Goroutine runs the actual work (resolve + create) and pushes
//     the final result onto a `done` chan.
//   - Returned tea.Batch has TWO cmds:
//       1. progressTickCmd — re-fires every 150ms, drains the
//          buffer, emits progressTickMsg with the new chunk. The
//          tick re-schedules itself in Update until busyDone.
//       2. waitDoneCmd — blocks reading from `done`, emits
//          createDoneMsg when the goroutine finishes.
//
// Both cmds run concurrently under tea.Batch. Update appends ticks
// to m.busyOutput live, then on createDoneMsg appends any final
// bytes the last tick missed.
func createCmd(mgr *workspace.Manager, name string, spec workspace.SourceSpec) tea.Cmd {
	// Lazy spawn: the goroutine + buffer + chan are constructed
	// inside the returned closure, NOT at createCmd's call site.
	// That keeps the cmd value cheap to construct and lets unit
	// tests inspect the returned cmd without accidentally kicking
	// off real work against a nil-mgr fixture.
	//
	// Update sees createStartedMsg first and dispatches the
	// streaming + done cmds via tea.Batch from there.
	return func() tea.Msg {
		buf := &safeBuffer{}
		done := make(chan createDoneMsg, 1)
		go func() {
			ctx := context.Background()
			opts, suggestedName, err := mgr.ResolveSource(ctx, spec)
			if err != nil {
				done <- createDoneMsg{output: buf.Drain(), err: err}
				return
			}
			// Explicit name beats source-derived suggestion beats namegen.
			if name == "" {
				name = suggestedName
			}
			ws, err := mgr.Create(ctx, name, opts, buf, buf)
			// Drain after Create returns so any trailing bytes
			// (the last "Workspace ready" line, etc.) end up in
			// the final createDoneMsg, not stranded in the buffer
			// if the tick timing missed them.
			msg := createDoneMsg{output: buf.Drain(), err: err}
			if err == nil && ws != nil {
				msg.tmuxSession = ws.TmuxSession
			}
			done <- msg
		}()
		return createStartedMsg{buf: buf, done: done}
	}
}

// createStartedMsg is the bridge between createCmd's lazy spawn and
// the streaming machinery. Update receives it once and dispatches
// the per-tick + wait-done cmds as a batch. Carries the buffer +
// done-chan so the dispatched cmds have what they need.
type createStartedMsg struct {
	buf  *safeBuffer
	done <-chan createDoneMsg
}

// progressTickInterval controls how often the busy view refreshes
// during a long-running create. 150ms is invisible to the eye for
// streaming text and far below any practical script output rate;
// shorter intervals just burn redraw cycles for no gain.
const progressTickInterval = 150 * time.Millisecond

// progressTickMsg fires every progressTickInterval while a Create is
// in flight. Carries the freshly-drained chunk and a back-reference
// to the buffer so Update can keep ticking without holding state.
type progressTickMsg struct {
	chunk string
	buf   *safeBuffer
}

// progressTickCmd builds the tick command. The drain happens at
// schedule time (inside the closure) so each tick fetches whatever
// the goroutine has written between this tick and the previous one.
func progressTickCmd(buf *safeBuffer) tea.Cmd {
	return tea.Tick(progressTickInterval, func(time.Time) tea.Msg {
		return progressTickMsg{chunk: buf.Drain(), buf: buf}
	})
}

// waitDoneCmd blocks on the done channel and emits the createDoneMsg
// when the goroutine finishes. Single-shot — only fires once per
// create flow.
func waitDoneCmd(done <-chan createDoneMsg) tea.Cmd {
	return func() tea.Msg {
		return <-done
	}
}

// textinputBlink dispatches the cursor blink command for the textinput.
// Wrapper kept so the modal-open code reads cleanly.
func textinputBlink() tea.Cmd {
	return textinput.Blink
}
