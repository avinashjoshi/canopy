package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/ghx"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/lifecycle"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// Update implements tea.Model. Routes incoming messages to focused
// handlers. The Model is always returned by value — Bubbletea owns the
// "current" Model, we own the next one.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Reserve lines for title + tab bar + help + spacing. The exact
		// reserve count isn't critical — projectlist truncates if it
		// runs out of vertical space rather than overflowing the
		// terminal.
		reserve := 6
		if m.inPopup {
			reserve = 5 // single-line tab bar + tighter chrome
		}
		m.list.SetSize(msg.Width, msg.Height-reserve)
		// Resize the upgrade viewport too if it's currently in use.
		// initUpgradeViewport reads m.width/m.height, so this re-fits
		// the changelog pane when the terminal resizes mid-preview.
		if m.upgradeChangelogInit {
			m.initUpgradeViewport()
		}
		return m, nil

	case agentPollTickMsg:
		// Generation gate: if Init re-fired (m.agentPollGen++ in
		// startAgentPolling), this scheduled tick is stale. Drop it
		// without rescheduling. The new generation's tick is already
		// in flight via the new startAgentPolling call.
		if msg.gen != m.agentPollGen {
			return m, nil
		}
		// Run the actual poll. Result lands as agentPollResultMsg.
		// Reschedule does NOT happen here — only after the result
		// applies, so we're guaranteed at-most-one tick in flight.
		return m, runAgentPoll(m.detector, m.tc, msg.gen)

	case agentPollResultMsg:
		// Same generation gate (defensive — runAgentPoll won't
		// outlive the gen, but be explicit).
		if msg.gen != m.agentPollGen {
			return m, nil
		}
		m.agentStates = msg.states
		// polled=true so the projectlist can distinguish "no agent
		// pane in this workspace" (No-AI badge) from "first tick
		// hasn't landed yet" (blank). Always true once a result lands.
		m.list.SetAgentStates(msg.states, true)
		// Skip Prune when active is nil — that's the "ListAgentPanes
		// failed transiently" signal. Pruning with nil would wipe
		// every pane's history and force a cold-start StateUnknown
		// across all rows on the next tick.
		if m.detector != nil && msg.active != nil {
			m.detector.Prune(msg.active)
		}
		return m, scheduleAgentPollTick(m.agentPollGen)

	case upgradeCheckedMsg:
		// Async upgrade refresh landed. Update pill state — empty
		// latest means "no upgrade available after refresh" (cleared)
		// or "fetch failed" (closure swallows errors). Either way,
		// trust what the closure returned. Re-render is automatic.
		m.upgradeAvailable = msg.latest
		return m, nil

	case changelogLoadedMsg:
		// Changelog fetch returned. Even on error we flip to preview —
		// the changelog is best-effort, the upgrade flow proceeds
		// without it. The renderer surfaces "(changelog unavailable)"
		// when preview is empty.
		if msg.err != nil {
			log.Warn("upgrade.changelog_fetch_failed", "err", msg.err)
		}
		m.upgradeChangelog = msg.preview
		m.upgradeState = upgradeStatePreview
		m.initUpgradeViewport()
		return m, nil

	case upgradeShellStartedMsg:
		// Lazy-spawn bridge from runUpgradeShellCmd. Latch the
		// buffer + cancel func, flip to running state, and dispatch
		// the streaming + completion cmds (same shape as the
		// create / remove flows).
		m.upgradeBuf = msg.buf
		m.upgradeCancel = msg.cancel
		m.upgradeState = upgradeStateRunning
		m.upgradeOutput = ""
		return m, tea.Batch(
			upgradeProgressTickCmd(msg.buf),
			waitUpgradeShellDoneCmd(msg.done),
		)

	case upgradeProgressTickMsg:
		// Stream tail. Append the new chunk and reschedule the next
		// tick unless the upgrade already finished. Mirrors busyMode's
		// progressTickMsg handling exactly.
		if msg.chunk != "" {
			m.upgradeOutput += msg.chunk
		}
		if m.upgradeState != upgradeStateRunning {
			return m, nil
		}
		return m, upgradeProgressTickCmd(msg.buf)

	case upgradeShellDoneMsg:
		// Terminal state. Append any trailing buffer content the
		// final tick missed; flip to doneOK or doneError so the
		// renderer shows the success/failure summary. The user
		// presses any key to return to listMode (handleUpgradeKey).
		if msg.output != "" {
			m.upgradeOutput += msg.output
		}
		m.upgradeErr = msg.err
		if msg.err == nil {
			m.upgradeState = upgradeStateDoneOK
			// Capture the shipped version for the doneOK message
			// before clearing the pill. The renderer needs this
			// to tell the user "you shipped v0.13.0; quit + restart
			// to use it."
			m.upgradeShipped = m.upgradeAvailable
			// Pill clears immediately on success — the running
			// canopy IS the just-installed binary (well, the
			// next invocation will be; this process is doomed to
			// be replaced). Setting upgradeAvailable to "" keeps
			// the pill from leaking the old "v0.13 available"
			// signal during the brief window between done and
			// any-key dismiss.
			m.upgradeAvailable = ""
		} else {
			m.upgradeState = upgradeStateDoneError
		}
		// Drop the cancel ref; the goroutine has already returned.
		m.upgradeCancel = nil
		return m, nil

	case remoteRowsLoadedMsg:
		// v0.17.0 Phase 1b: result of the remote-host fan-out. Stash the
		// rows in m.remoteRows; the next filteredRows() call combines
		// them with local m.allRows. Errors are non-fatal — last-known
		// remote rows stay visible.
		//
		// Phase 1c also stashes the host registry list + per-host
		// snapshots so the Hosts tab can render fleet status without
		// re-reading the registry on every frame.
		m.remoteRefreshing = false
		if msg.rows != nil {
			m.remoteRows = msg.rows
		}
		if msg.hosts != nil {
			m.hostList = msg.hosts
		}
		if msg.snaps != nil {
			m.remoteSnaps = msg.snaps
		}
		m.list.SetRows(m.filteredRows())
		return m, nil

	case rowsLoadedMsg:
		// Refresh result. Apply rows to allRows + push the filtered
		// (tab + search) subset to projectlist for rendering.
		m.err = msg.err
		if msg.rows != nil {
			m.allRows = msg.rows
		}
		m.list.SetRows(m.filteredRows())
		// Position the cursor on the workspace whose dir cwd was inside
		// (popup launched from inside a workspace → highlight that
		// workspace, not row 0). Latches on the first NON-EMPTY load so
		// subsequent refreshes don't yank the cursor back. The "non-empty"
		// gate handles two failure modes:
		//   - early empty rowsLoadedMsg (state racing the refresh): don't
		//     burn the preselect opportunity, the next non-empty load
		//     gets to try.
		//   - target missing from the *filtered* set (user changed tab
		//     before the load completed): we still latch, because the
		//     row was reachable through allRows but the user's tab
		//     filter excluded it. Auto-jumping the cursor on a later
		//     refresh when the row reappears would surprise the user
		//     mid-navigation.
		if !m.initialCursorPlaced && len(m.allRows) > 0 {
			if m.currentWorkspace != "" && m.currentWorkspaceRoot != "" {
				m.list.SetCursorTo(m.currentWorkspaceRoot, m.currentWorkspace)
			}
			m.initialCursorPlaced = true
		}
		// Phase 2: kick off per-row hint loaders in parallel.
		if msg.err != nil || len(m.allRows) == 0 {
			return m, nil
		}
		return m, loadRowHintsCmds(m.allRows)

	case rowHintsMsg:
		// Late-arriving lifecycle detector result. Merge into m.allRows
		// (the source of truth) by (project, name) THEN re-push the
		// filtered set to projectlist. Mutating only the projectlist's
		// rows would lose hints on the next tab-switch or search-mutation
		// SetRows call (which projects from m.allRows).
		for i := range m.allRows {
			if m.allRows[i].Project == msg.project && m.allRows[i].Name == msg.name {
				m.allRows[i].Hints = msg.hints
				break
			}
		}
		m.list.SetRows(m.filteredRows())
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
		// Resurrection completed, now attach. attachOrSwitch picks the
		// right verb (popup → switch-client + quit, fullscreen → exec).
		return m, m.attachOrSwitch(msg.session)


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
			// "From a prompt" path: send the initial prompt to the
			// agent pane BEFORE attaching. The send blocks while the
			// trust-dialog state machine runs (≤5s + ≤5s + verify),
			// so we stay in busyMode with a "Sending prompt..." title
			// instead of attaching immediately — the user otherwise
			// lands in the agent pane mid-trust-dismiss and watches
			// keys appear out of nowhere, which looks like a bug.
			if m.pendingPrompt != "" {
				m.busyTitle = "Sending prompt to agent..."
				m.busyOutput += "\nWorkspace ready. Sending initial prompt to the agent...\n"
				m.busyDone = false
				prompt := m.pendingPrompt
				session := msg.tmuxSession
				m.pendingPrompt = "" // consumed; don't double-send on a retry path
				return m, sendPromptCmd(m.newTargetMgr, session, prompt)
			}
			m.mode = listMode
			m.busyOp = busyOpNone
			m.busyTitle = ""
			m.busyOutput = ""
			m.busyDone = false
			return m, m.attachOrSwitch(msg.tmuxSession)
		}
		return m, nil

	case promptSentMsg:
		// SendInitialPrompt finished. On success: attach. On failure
		// (workspace.ErrPromptFailed): still attach — the workspace
		// is alive and the user should land in it; surface the error
		// inline so they know to re-issue the prompt by hand. Same
		// posture as the CLI's exit-code 2 behavior (workspace OK,
		// prompt skipped).
		if msg.err != nil {
			// Stash the error so the next listMode render surfaces it
			// in the error banner (m.err is the standard surface).
			m.err = fmt.Errorf("workspace created but prompt was not delivered: %w", msg.err)
		}
		m.mode = listMode
		m.busyOp = busyOpNone
		m.busyTitle = ""
		m.busyOutput = ""
		m.busyDone = false
		return m, m.attachOrSwitch(msg.session)

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
		// Auto-dismiss on success. Mirrors createDoneMsg's "drop the
		// busy view as soon as the work is done" pattern. There's no
		// equivalent of attach-to-the-new-session — nothing to attach
		// to after a delete — so the right move is just to leave busy
		// mode. In popup mode that means tea.Quit (the parent client
		// has already been switched off the doomed session by
		// escapeIfDeletingCurrent, so closing the popup lands the user
		// in the project main). In fullscreen mode return to listMode
		// and refresh so the row disappears.
		//
		// On error, stay in busyMode with busyDone=true so the
		// captured archive output + error are visible — the user
		// dismisses with any key via handleBusyModeKey.
		if msg.err == nil {
			m.busyOp = busyOpNone
			m.busyTitle = ""
			m.busyOutput = ""
			m.busyDone = false
			if m.inPopup {
				return m, tea.Quit
			}
			m.mode = listMode
			return m, m.refresh()
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

	case drawerLoadedMsg:
		// Stale message guard: if the user closed the drawer or
		// re-opened on a different row, drop the stale data. Match
		// on (Name, ProjectRoot) — Name alone collides across
		// projects in the global TUI (e.g. two projects each having
		// a workspace named "feature-a").
		if m.mode != drawerMode || m.drawerRow.Name != msg.forName || m.drawerRow.ProjectRoot != msg.forRoot {
			return m, nil
		}
		m.drawerProcInfo = msg.procInfo
		m.drawerLogTail = msg.logTail
		m.drawerSetupLog = msg.setupLog
		m.drawerErr = msg.err
		return m, nil

	case drawerActionMsg:
		if msg.err != nil {
			m.drawerErr = msg.err
		}
		return m, nil

	case drawerAttachAfterMsg:
		// Bare attach session is ready — attach via the normal path.
		// attachOrSwitch handles popup vs fullscreen.
		m.mode = listMode
		m.drawerRow = Row{}
		return m, m.attachOrSwitch(msg.session)

	case errMsg:
		// Deferred error from a tea.Cmd (e.g. openRemoteBrowser). Surface
		// it in the status bar but don't kick off a refresh — the cmd has
		// already failed and a refresh won't re-attempt it.
		if msg.err != nil {
			m.err = msg.err
		}
		return m, nil

	case killDoneMsg:
		// `K` kill finished. Invalidate the cache for the killed
		// session so a later resurrect re-probes immediately rather
		// than serving up to TTL seconds of stale RSS/CPU from the
		// dead session's pre-kill snapshot.
		if m.memCache != nil && msg.session != "" {
			m.memCache.Invalidate(msg.session)
		}
		if msg.err != nil {
			m.err = msg.err
		}
		return m, m.refresh()

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
	case newPromptMode:
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
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
	case newPromptMode:
		return m.handleNewPromptKey(msg)
	case newPRMode:
		return m.handleNewPRKey(msg)
	case newIssueMode:
		return m.handleNewIssueKey(msg)
	case newBranchMode:
		return m.handleNewBranchKey(msg)
	case confirmDeleteMode:
		return m.handleConfirmDeleteKey(msg)
	case confirmKillMode:
		return m.handleConfirmKillKey(msg)
	case confirmRetryMode:
		return m.handleConfirmRetryKey(msg)
	case drawerMode:
		return m.handleDrawerKey(msg)
	case busyMode:
		return m.handleBusyModeKey(msg)
	case upgradeMode:
		return m.handleUpgradeKey(msg)
	}

	// Search-mode keystrokes: capture into searchQuery, refilter on each
	// keystroke. Esc clears + exits search; Enter exits keeping the
	// query so arrow nav works on the filtered list. Active in listMode
	// only — other view modes own their own input loop.
	if m.searchMode {
		return m.handleSearchKey(msg)
	}

	// listMode keymap: iterate the bindings table; first match-and-
	// available fires its Action. Order matches listModeBindings —
	// no shadowing concerns because the bindings have disjoint Keys
	// (k.Matches is exact-match per binding, not a regex).
	for _, b := range listModeBindings {
		if b.Matches(msg, m) {
			return b.Action(m, msg)
		}
	}

	return m, nil
}

// ─── listMode action handlers ──────────────────────────────────────
// Each handler is the body that used to live inline in handleKey's
// switch statement. Extracted as named functions so the keymap.go
// bindings table can reference them as data. Same return shape
// (tea.Model, tea.Cmd) as the Bubbletea Update contract.

func actionQuit(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m, tea.Quit
}

func actionHelpToggle(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.showHelp = true
	return m, nil
}

func actionRefresh(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Explicit refresh: user asked for fresh data, so bust every cache
	// that gates fresh reads. `r` means "I want truth right now," not
	// "respect the TTL." Without the pr_status cache bust, a user who
	// just merged a PR or pushed a review change would still see the
	// stale "PR #142 awaiting review" hint for up to 10 minutes —
	// classic "I refreshed and nothing happened" bug. Same intent as
	// memCache.InvalidateAll() right next to it.
	//
	// Background ticks and reconcile do NOT invalidate; they keep the
	// 10-min TTL to stay inside the GitHub API budget. Only deliberate
	// user action busts.
	if m.memCache != nil {
		m.memCache.InvalidateAll()
	}
	lifecycle.ResetPRStatusCache()

	// Force-refresh the upgrade-check cache too. The auto-check has a
	// 6h TTL; if the user just shipped a new release outside canopy
	// (manual `git pull && make install`), the pill won't notice
	// until the next TTL window without this. `r` should mean "I want
	// truth right now" across every cache, including upgrade.
	cmds := []tea.Cmd{m.refresh()}
	if cmd := upgradeRefreshCmd(m.upgradeRefreshFn); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// actionTabSwitch flips Local ↔ Global. The new filtered set is pushed
// to projectlist via SetRows; projectlist clamps its cursor automatically
// so a long-list scroll position from the previous tab doesn't carry
// over past the end of the new tab.
//
// Special case: Global → Local with no currentProject (canopy invoked
// outside any project) routes through actionFocusProject so Tab acts
// as "enter the project I'm looking at." Without this, Local would
// either show every row (empty currentProject = no filter) or feel
// broken — neither helps the user. The cursor row's ProjectRoot
// becomes the new Local context.
func actionTabSwitch(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Tab key uses the same "next" cycle as right/l. v0.17.0 Phase 1c
	// polish unified the three forward-cycle keys behind one helper.
	return actionTabNext(m, msg)
}

// actionTabNext cycles forward through tabs: Local → Global → Hosts
// → back to Local. Hosts tab inserts into the cycle only when at
// least one host is registered (empty Hosts tab is uninteresting and
// hitting tab repeatedly to land on nothing is confusing). v0.17.0
// Phase 1c polish — bound to Tab AND `right`/`l`.
//
// One quirk preserved from the original actionTabSwitch: tabGlobal →
// tabLocal when currentProject is unset routes through
// actionFocusProject so the cursor row's project becomes Local's
// context. Without that, Local would either show every row (no
// filter) or feel broken.
func actionTabNext(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.tab {
	case tabLocal:
		m.tab = tabGlobal
		m.list.SetRows(m.filteredRows())
		return m, nil
	case tabGlobal:
		if m.hostsHasEntries() {
			m.tab = tabHosts
			return m, nil
		}
		// No hosts → skip Hosts and wrap to Local.
		if m.currentProject == "" {
			row, ok := m.list.CursorRow()
			if ok && row.ProjectRoot != "" {
				return actionFocusProject(m, msg)
			}
		}
		m.tab = tabLocal
		m.list.SetRows(m.filteredRows())
		return m, nil
	case tabHosts:
		if m.currentProject == "" {
			row, ok := m.list.CursorRow()
			if ok && row.ProjectRoot != "" {
				return actionFocusProject(m, msg)
			}
		}
		m.tab = tabLocal
		m.list.SetRows(m.filteredRows())
		return m, nil
	}
	return m, nil
}

// actionTabPrev cycles backward: Local ← Global ← Hosts. Bound to
// `left`/`h`. Same Hosts-skip-when-empty logic as actionTabNext.
func actionTabPrev(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.tab {
	case tabLocal:
		// Wrap to Hosts if any registered, else Global.
		if m.hostsHasEntries() {
			m.tab = tabHosts
			return m, nil
		}
		m.tab = tabGlobal
		m.list.SetRows(m.filteredRows())
		return m, nil
	case tabGlobal:
		m.tab = tabLocal
		m.list.SetRows(m.filteredRows())
		return m, nil
	case tabHosts:
		m.tab = tabGlobal
		m.list.SetRows(m.filteredRows())
		return m, nil
	}
	return m, nil
}

// actionSearchEntry enters fuzzy-search mode. Subsequent keystrokes are
// captured into searchQuery via handleSearchKey (which the search-mode
// bypass at the top of handleKey routes to).
func actionSearchEntry(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.searchMode = true
	m.searchQuery = ""
	return m, nil
}

// actionNewWorkspace opens the new-workspace variant picker. Resolves
// the TARGET project before the picker opens so every downstream
// handler (loaders, submits, busy renderer) can read m.newTargetMgr
// without re-deriving it.
//
// Two resolution paths:
//
//   - Local tab: target = m.mgr. Trivial; this is how `n` always
//     worked.
//   - Global tab: target = managerForRow(cursor). Cross-project entry
//     point; mirrors d/R/K. config.LoadFrom + workspace.New runs here,
//     not in availableNewWorkspace, so the cheap predicate stays cheap.
//
// On any resolution failure (canopy.json missing/broken, manager
// construction error), surface via m.err and stay in listMode — the
// picker doesn't open against a half-resolved target.
func actionNewWorkspace(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	// v0.17.0 Phase 1c: on Hosts tab, `n` opens the add-host wizard
	// instead of the new-workspace picker. The wizard runs as a
	// subprocess via tea.ExecProcess so it can take over the terminal
	// for huh's form and the ssh-copy-id offer.
	if m.tab == tabHosts {
		return m, m.execHostAddWizard()
	}
	var (
		mgr      *workspace.Manager
		root     string
		projName string
	)
	if m.tab == tabLocal {
		if m.mgr == nil {
			// Should be unreachable — availableNewWorkspace guards this —
			// but defend the invariant rather than panic'ing on a nil deref.
			return m, nil
		}
		mgr = m.mgr
		root = m.mgr.Cfg.ProjectRoot
		projName = m.mgr.Cfg.Project
	} else {
		row, ok := m.list.CursorRow()
		if !ok || row.ProjectRoot == "" {
			return m, nil
		}
		var err error
		mgr, err = m.managerForRow(row)
		if err != nil {
			m.err = fmt.Errorf("new in %s: %w", row.Project, err)
			return m, nil
		}
		root = mgr.Cfg.ProjectRoot
		projName = mgr.Cfg.Project
	}
	m.newTargetMgr = mgr
	m.newTargetRoot = root
	m.newTargetName = projName
	m.openNewPicker()
	return m, nil
}

// clearNewTarget zeroes out the in-flight new-workspace target. Called
// when the flow exits (esc back to listMode, busy dismiss after create
// success/failure) so a future `n` press starts from a clean slate
// rather than inheriting the previous target's project context.
func (m *Model) clearNewTarget() {
	m.newTargetMgr = nil
	m.newTargetRoot = ""
	m.newTargetName = ""
	m.pendingPrompt = ""
}

// actionFocusProject "loads into" the cursor row's project: sets it as
// the current context, constructs its Manager (so `n` becomes available),
// switches to Local tab. The unified TUI now behaves as if it were
// launched from inside that project's source repo.
//
// Doesn't change the parent shell's cwd — that requires a shell wrapper
// (lazygit-style env-var protocol) which canopy doesn't ship today
// because the typical workflow uses `enter` on a project's main row to
// switch tmux clients into that project's main session, which already
// has shells in the project root.
func actionFocusProject(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok || row.ProjectRoot == "" {
		return m, nil
	}
	// v0.17.0: remote rows don't have a local ProjectRoot. Focus is a
	// laptop-side filter ("show only this project's local workspaces"),
	// which has no meaning for a project that lives on tower. Inform
	// the user instead of silently failing.
	if row.Host != "" {
		m.err = fmt.Errorf("can't focus a remote project — %s/%s lives on %s; cd into your local copy and run `canopy` there", row.Host, row.Project, row.Host)
		return m, nil
	}
	// Short-circuit when re-focusing the already-current project: just
	// flip to Local tab and re-filter rows. Avoids a needless canopy.json
	// reload (which would surface a spurious m.err if the file is
	// transiently unreadable, even though the existing m.mgr is still
	// valid). The visible side effect — landing on Local tab — happens
	// either way.
	if row.ProjectRoot == m.currentProject {
		m.tab = tabLocal
		m.list.SetRows(m.filteredRows())
		return m, nil
	}
	cfg, err := config.LoadFrom(row.ProjectRoot)
	if err != nil {
		m.err = fmt.Errorf("focus %s: %w", row.Project, err)
		return m, nil
	}
	mgr, err := workspace.New(cfg)
	if err != nil {
		// Don't fail loudly — focus still works for read + cross-project
		// d/R via the transient-Manager path. Just `n` stays unavailable
		// until the user fixes the underlying state issue.
		m.err = fmt.Errorf("focus %s (read-only — Manager construction failed: %v)",
			row.Project, err)
		m.mgr = nil
	} else {
		m.mgr = mgr
		m.err = nil
	}
	m.currentProject = row.ProjectRoot
	m.projectName = cfg.Project
	m.tab = tabLocal
	m.list.SetRows(m.filteredRows())
	return m, nil
}

// actionOpenPR opens the cursor row's pull request in the user's
// default browser via `gh pr view --web`. Runs gh from the workspace
// directory so gh resolves the PR for the worktree's checked-out
// branch (matches what the pr_status hint surfaced). gh handles the
// browser handoff itself — we just spawn and forget.
//
// Errors (gh missing, no PR, network) surface in m.err so the user
// sees them on the status line. The TUI doesn't quit; the user can
// retry or just ignore.
func actionOpenPR(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok || row.Path == "" {
		return m, nil
	}
	cmd := exec.Command("gh", "pr", "view", "--web")
	cmd.Dir = row.Path
	if err := cmd.Start(); err != nil {
		m.err = fmt.Errorf("open PR: %w", err)
		return m, nil
	}
	// gh's --web returns immediately after handing off to the browser;
	// Wait in a goroutine so we don't leave a zombie if the user is
	// quick to act on something else.
	go func() { _ = cmd.Wait() }()
	return m, nil
}

// actionOpenBrowser opens http://localhost:<row.Port> in the user's
// default browser via `xdg-open`. Linux-only: canopy is positioned as
// "Conductor for Linux" and xdg-open is the standard freedesktop
// handoff. Spawn-and-forget shape mirrors actionOpenPR — errors
// (xdg-open missing, no handler registered) surface in m.err so the
// user gets a status-line hint without the TUI hanging.
func actionOpenBrowser(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok || row.Port <= 0 {
		return m, nil
	}
	// v0.17.0: remote workspaces auto-open via ssh -L port forwarding.
	// Backgrounds `ssh -fNL <port>:localhost:<port> <target>` (one-
	// shot port-forward listener) then opens http://localhost:<port>.
	// The tunnel persists in the background; killed when the user logs
	// out or runs `pkill ssh` (not pretty but functional for v0.17).
	if row.Host != "" {
		target, err := m.resolveHostForExec(row.Host)
		if err != nil {
			m.err = fmt.Errorf("open browser on %s: %w", row.Host, err)
			return m, nil
		}
		return m, m.openRemoteBrowser(target, row.Host, row.Port)
	}
	url := fmt.Sprintf("http://localhost:%d", row.Port)
	cmd := exec.Command("xdg-open", url)
	if err := cmd.Start(); err != nil {
		m.err = fmt.Errorf("open browser: %w", err)
		return m, nil
	}
	go func() { _ = cmd.Wait() }()
	return m, nil
}

// openRemoteBrowser establishes an SSH port forward to the remote host
// (background, one-shot listener) then xdg-opens the localhost URL.
// v0.17.0 Phase 1. The tunnel uses `ssh -fNL` which:
//
//	-f  fork to background after auth
//	-N  no remote command (port-forward only, no shell)
//	-L  local-port:remote-host:remote-port
//
// The forwarded listener stays alive until ssh exits (e.g., user
// logout, system reboot, pkill ssh). Not pretty for production —
// proper tunnel management is post-v0.17 — but unblocks the daily
// "press B on a remote row, see the dev server" loop.
//
// Idempotent: if a tunnel for this port already exists (laptop's port
// is in use), ssh -L errors with "bind: Address already in use"; we
// ignore that and proceed to xdg-open assuming the existing tunnel
// is the one we want.
func (m *Model) openRemoteBrowser(sshTarget, hostName string, port int) tea.Cmd {
	return func() tea.Msg {
		// Stand up the tunnel (background process; returns when forked).
		tunnel := exec.Command("ssh",
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+filepath.Join(os.Getenv("HOME"), ".canopy", "ssh-%C.sock"),
			"-o", "ControlPersist=300",
			"-o", "BatchMode=yes",
			"-o", "ExitOnForwardFailure=yes",
			"-fNL", fmt.Sprintf("%d:localhost:%d", port, port),
			sshTarget,
		)
		if out, err := tunnel.CombinedOutput(); err != nil {
			outStr := string(out)
			// "bind: Address already in use" means we already have a
			// tunnel (probably from a previous B press). That's fine —
			// just proceed to open the URL.
			if !strings.Contains(outStr, "Address already in use") {
				log.Warn("ui.open-remote-browser.tunnel-failed",
					"host", hostName, "port", port, "err", err, "out", outStr)
				return errMsg{err: fmt.Errorf("ssh -L tunnel to %s failed: %v (%s)", hostName, err, outStr)}
			}
		}
		// Open the browser at the laptop-local port (now forwarded to
		// the remote's port).
		url := fmt.Sprintf("http://localhost:%d", port)
		open := exec.Command("xdg-open", url)
		if err := open.Start(); err != nil {
			return errMsg{err: fmt.Errorf("xdg-open %s: %w", url, err)}
		}
		go func() { _ = open.Wait() }()
		return nil
	}
}

// errMsg lets a tea.Cmd report a deferred error back to the Update
// handler, which surfaces it on m.err.
type errMsg struct{ err error }

// actionDelete opens the confirm-delete modal for the cursor row. Cross-
// project rows construct a transient Manager via managerForRow; same
// path as the same-project case so the confirm modal copy is uniform.
func actionDelete(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok {
		return m, nil
	}
	if row.IsMain {
		m.err = fmt.Errorf("can't delete the main session via canopy rm — use `tmux kill-session -t %s` if you want it gone",
			row.TmuxSession)
		return m, nil
	}
	// v0.17.0: remote rows skip the local Manager + SafetyPreflight
	// path entirely. The local canopy doesn't have a Manager for a
	// project that lives on tower — Manager construction needs a
	// canopy.json on disk locally, which doesn't exist for a remote
	// project. The REMOTE canopy runs its own SafetyPreflight when
	// `canopy rm <name> --on tower --yes` dispatches. So we just
	// open the modal with the row's name; deleteHangs stays empty
	// (the remote will refuse if hanging work exists).
	if row.Host != "" {
		m.mode = confirmDeleteMode
		m.deleteTarget = row.Name
		m.deleteTargetRoot = "" // remote rows have no local ProjectRoot
		m.deleteHangs = nil     // remote-side preflight runs on confirm
		return m, nil
	}
	mgr, err := m.managerForRow(row)
	if err != nil {
		m.err = err
		return m, nil
	}
	hangs, _ := mgr.SafetyPreflight(context.Background(), row.Name)
	m.mode = confirmDeleteMode
	m.deleteTarget = row.Name
	m.deleteTargetRoot = row.ProjectRoot
	m.deleteHangs = hangs
	return m, nil
}

// actionAttach is the enter-key flow. Resurrects stopped workspaces
// first; popup-mode uses switch-client + tea.Quit instead of attach.
func actionAttach(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.attachSelected()
}

// actionKill opens the confirm-kill modal for the cursor row. K kills
// the tmux session only — state.json row (if any), worktree dir, and
// branch all survive. Re-pressing Enter after kill resurrects: workspace
// rows go through Manager.Resurrect, main rows go through
// EnsureMainSession.
//
// Works on both workspace and main rows: K is a session-lifecycle
// operation, not a workspace-identity operation. Killing main is no
// more dangerous than killing a workspace session — both are
// recoverable via Enter, and `claude --continue` keeps the AI
// conversation history per-directory.
//
// Stopped/broken/orphaned rows (any row with Alive=false) are no-ops
// — nothing to kill — surfaced as a status-line hint rather than
// silently doing nothing.
func actionKill(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok {
		return m, nil
	}
	if !row.Alive {
		label := row.Name
		if row.IsMain {
			label = "main session"
		}
		m.err = fmt.Errorf("%s has no live tmux session to kill", label)
		return m, nil
	}
	m.mode = confirmKillMode
	m.killTarget = row.Name
	m.killTargetRoot = row.ProjectRoot
	return m, nil
}

// handleConfirmKillKey is the keymap while the kill prompt is up.
// y or Y kills the session; anything else cancels. Cancel-by-default
// is the safe posture even though K is far less destructive than d.
func (m *Model) handleConfirmKillKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "y" || msg.String() == "Y" {
		// Resolve the target — same (Project, Name) match as confirm-delete.
		var target Row
		var found bool
		for _, r := range m.filteredRows() {
			if r.Name != m.killTarget {
				continue
			}
			if m.killTargetRoot != "" && r.ProjectRoot != m.killTargetRoot {
				continue
			}
			target = r
			found = true
			break
		}
		m.mode = listMode
		m.killTarget = ""
		m.killTargetRoot = ""
		if !found {
			// Row went away between modal open and confirm — treat as cancel.
			return m, nil
		}
		// v0.17.0: remote rows dispatch to the host's tmux via SSH;
		// canopy doesn't ship a `kill` verb (the existing TUI kill is
		// "tmux kill-session" — workspace dir + state row + branch all
		// survive, status flips to stopped, re-attach resurrects).
		if target.Host != "" {
			return m, m.execRemoteKill(target.Host, target.TmuxSession)
		}
		return m, killCmd(m.tc, target.TmuxSession, target.Name)
	}
	// Anything else cancels.
	m.mode = listMode
	m.killTarget = ""
	m.killTargetRoot = ""
	return m, nil
}

// killDoneMsg carries the result of an async tmux kill back to Update.
// session is plumbed through so the Update handler can invalidate the
// load cache for the just-killed session — without that, a later
// resurrect on the same row would serve up to TTL seconds of stale
// RSS/CPU from the pre-kill snapshot.
type killDoneMsg struct {
	name    string
	session string
	err     error
}

// killCmd runs `tmux kill-session -t <session>` async via tea.Cmd. The
// kill is fast (~5ms) but we still go through tea.Cmd so the UI stays
// responsive and we can surface errors via the message channel rather
// than blocking the Update goroutine.
//
// ErrSessionNotFound (the session was already dead) is treated as
// success — the user's intent is "make this session gone," and gone
// is gone whether we did the killing or someone else did.
func killCmd(tc tmuxKiller, session, name string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		err := tc.Kill(ctx, session)
		if err != nil && !isErrSessionNotFound(err) {
			return killDoneMsg{name: name, session: session, err: fmt.Errorf("kill %s: %w", session, err)}
		}
		return killDoneMsg{name: name, session: session}
	}
}

// tmuxKiller is the slice of *tmux.Client that killCmd needs. Decoupled
// as an interface so tests can substitute a fake without spinning up a
// real tmux server.
type tmuxKiller interface {
	Kill(ctx context.Context, name string) error
}

// Drawer (i / b) lives in drawer.go. actionInspect, handleDrawerKey,
// drawerLoadCmd, drawerLoadedMsg, bareAttachCmd are defined there.

// isErrSessionNotFound checks whether err's chain includes the tmux
// "session not found" sentinel via errors.Is. The ui package already
// imports internal/tmux for *tmux.Client, so there's no cycle risk —
// the sentinel match is the right tool here, not string matching
// (which would silently break if tmux ever rephrased the error or
// internationalized it).
func isErrSessionNotFound(err error) bool {
	return errors.Is(err, tmux.ErrSessionNotFound)
}

// actionRetry handles `R`. v0.6: only ran on broken (no friction).
// v0.8 (D3/CP1): non-broken triggers the y/N gate; broken still runs
// setup directly. Cross-project goes through managerForRow.
func actionRetry(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok {
		return m, nil
	}
	if row.IsMain {
		return m, nil
	}
	// v0.17.0: remote rows dispatch to the host's canopy retry via
	// subprocess. The remote canopy handles status checks + the actual
	// scripts.setup re-run; we just route the verb. The confirm-retry
	// modal is skipped for remote rows (remote canopy will print its
	// own confirmation messages in the subprocess output).
	if row.Host != "" {
		// Remote retry always uses --force for broken-status retries;
		// for non-broken we still want to retry on the remote even if
		// not broken (mirrors the local "F to force on healthy" flow,
		// but folded into one path since the TUI confirm doesn't
		// really translate to a subprocess flow). User can pass
		// --force themselves via CLI for explicit safety.
		force := row.Status != state.StatusBroken
		return m, m.execRemoteVerb(row.Host, "retry", []string{row.Name}, force)
	}
	if _, err := m.managerForRow(row); err != nil {
		m.err = err
		return m, nil
	}
	if row.Status != state.StatusBroken {
		m.mode = confirmRetryMode
		m.retryTarget = row.Name
		return m, nil
	}
	mgr, _ := m.managerForRow(row) // already validated above
	m.mode = busyMode
	m.busyOp = busyOpRetry
	m.busyTitle = fmt.Sprintf("Retrying setup for %q...", row.Name)
	m.busyDone = false
	m.busyOutput = ""
	m.busyErr = nil
	return m, retryCmdUI(mgr, row.Name)
}

// Cursor-nav actions forward to projectlist's Update so it can clamp
// the cursor against its own row count. Bubbletea's Update returns
// (Model, tea.Cmd); projectlist returns (Model value, tea.Cmd) so we
// reassign m.list with the returned value.
func actionCursorUp(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

func actionCursorDown(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

func actionCursorTop(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

func actionCursorBottom(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, cmd := m.list.Update(msg)
	m.list = next
	return m, cmd
}

// handleSearchKey handles keystrokes while m.searchMode is true.
// Each query mutation pushes a fresh filtered set to projectlist so
// the user sees results live as they type.
func (m *Model) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.searchMode = false
		m.searchQuery = ""
		m.list.SetRows(m.filteredRows())
		return m, nil
	case tea.KeyEnter:
		// Enter exits search mode keeping the query, so arrow nav
		// works on the filtered list.
		m.searchMode = false
		return m, nil
	case tea.KeyBackspace:
		if len(m.searchQuery) > 0 {
			runes := []rune(m.searchQuery)
			m.searchQuery = string(runes[:len(runes)-1])
			m.list.SetRows(m.filteredRows())
		}
		return m, nil
	case tea.KeyRunes:
		m.searchQuery += string(msg.Runes)
		m.list.SetRows(m.filteredRows())
		return m, nil
	case tea.KeySpace:
		m.searchQuery += " "
		m.list.SetRows(m.filteredRows())
		return m, nil
	}
	return m, nil
}

// handleConfirmRetryKey is the y/N gate for `R` on a non-broken workspace
// (D3/CP1). Mirrors handleConfirmDeleteKey's shape.
//
// y → run scripts.setup with force=true (the CLI's --force semantics).
// n / esc / any other key → cancel, back to listMode.
func (m *Model) handleConfirmRetryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		// User confirmed. Build the busy view and dispatch.
		row, ok := m.list.CursorRow()
		if !ok {
			m.mode = listMode
			m.retryTarget = ""
			return m, nil
		}
		mgr, err := m.managerForRow(row)
		if err != nil {
			m.err = err
			m.mode = listMode
			m.retryTarget = ""
			return m, nil
		}
		m.mode = busyMode
		m.busyOp = busyOpRetry
		m.busyTitle = fmt.Sprintf("Retrying setup for %q (forced)...", row.Name)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.retryTarget = ""
		return m, retryCmdUIForce(mgr, row.Name)
	}
	// Anything else cancels.
	m.mode = listMode
	m.retryTarget = ""
	return m, nil
}

// filteredRows projects m.allRows into the rows currently rendered,
// applying the active tab filter and search query. Returns a slice of
// state.GlobalRow that the projectlist component renders.
//
// Tab filter: tabLocal includes rows whose ProjectRoot matches
// m.currentProject. tabGlobal includes everything.
// Search filter: fzf-style subsequence match against name + project +
// branch. Empty query passes everything.
func (m *Model) filteredRows() []state.GlobalRow {
	// Phase 1b: combine local m.allRows (Host="") and remote m.remoteRows
	// (Host=<hostname>) into one slice for the projectlist renderer.
	// Order: local first, then remote rows grouped by host (host names
	// are sorted in refreshRemoteCmd's output already, but the
	// projectlist render path relies on prevHost transitions so any
	// row order works visually — local first is the principle).
	combined := make([]state.GlobalRow, 0, len(m.allRows)+len(m.remoteRows))
	combined = append(combined, m.allRows...)
	combined = append(combined, m.remoteRows...)

	out := make([]state.GlobalRow, 0, len(combined))
	for _, r := range combined {
		// Local tab filtering applies only to local rows. Remote rows
		// never have a ProjectRoot that matches the current local
		// project (different filesystems), so the Local tab simply
		// excludes all remote rows. Global tab shows everything.
		if m.tab == tabLocal {
			if r.Host != "" {
				continue
			}
			if m.currentProject != "" && r.ProjectRoot != "" && r.ProjectRoot != m.currentProject {
				continue
			}
		}
		if m.searchQuery != "" && !rowMatchesQuery(r, m.searchQuery) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// rowMatchesQuery returns true if the lowercased query is a subsequence
// of the row's name, project, OR branch. fzf-style; lowercases the row
// fields on each call (cheap relative to the search call rate).
func rowMatchesQuery(r state.GlobalRow, query string) bool {
	q := lowerASCII(query)
	return isSubseq(lowerASCII(r.Name), q) ||
		isSubseq(lowerASCII(r.Project), q) ||
		isSubseq(lowerASCII(r.Branch), q)
}

// lowerASCII is a fast lowercase for ASCII. Avoids the allocation of
// strings.ToLower for the common case of ASCII row names. Falls back to
// the byte-level rule (a-z = A-Z + 32) which is correct for the
// ASCII-only project/branch/name space canopy operates in.
func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

// isSubseq lives in model_global.go (will move here once that file is
// deleted in the cleanup commit). Both files are package ui so the
// definition is shared at link time — no need to re-declare.

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
//
// In popup mode (CANOPY_IN_POPUP=1), attach is replaced by switch-client
// + tea.Quit so the user lands in the workspace from the parent tmux
// client and the popup closes itself.
//
// Cross-project rows resolve their Manager via managerForRow — same
// path as d/R. A stopped cross-project row resurrects via the transient
// Manager.
func (m *Model) attachSelected() (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok {
		return m, nil
	}
	ctx := context.Background()

	// v0.17.0 Phase 1b: remote rows dispatch through `canopy switch
	// --on <host> <name>` as a subprocess. The subprocess does the
	// SSH/mosh dance and exec-replaces itself with mosh; mosh runs in
	// the terminal Bubbletea hands over, and on detach control returns
	// here for the refresh that re-syncs cached remote-row state.
	if row.Host != "" {
		return m, m.attachRemoteRow(row)
	}

	if row.IsMain {
		if row.Alive {
			return m, m.attachOrSwitch(row.TmuxSession)
		}
		// Auto-start the main session and attach. Without this, enter on
		// a dead main row sent the user back to a shell to run `canopy
		// main` — which the popup user can't even reach without first
		// closing the popup. EnsureMainSession is idempotent (no-op on a
		// live session) so the dispatch is uniform.
		mgr, err := m.managerForRow(row)
		if err != nil {
			m.err = err
			return m, nil
		}
		session, err := mgr.EnsureMainSession(ctx)
		if err != nil {
			m.err = fmt.Errorf("start main session: %w", err)
			return m, nil
		}
		return m, m.attachOrSwitch(session)
	}

	switch effectiveStatus(row) {
	case "main", state.StatusReady:
		return m, m.attachOrSwitch(row.TmuxSession)

	case state.StatusStopped:
		// Resurrect, then attach. Cross-project: managerForRow gives the
		// right Manager; popup mode still uses tea.ExecProcess for the
		// resurrect path (it spawns a workspace setup which doesn't fit
		// switch-client semantics) and falls through to attach after.
		//
		// Reached via two paths: a row recorded as stopped, OR a row
		// recorded as ready but whose tmux session is dead (effectiveStatus
		// downgrades the latter so Enter on a stale-ready row resurrects
		// instead of attempting an attach that would fail with
		// ErrSessionNotFound).
		mgr, err := m.managerForRow(row)
		if err != nil {
			m.err = err
			return m, nil
		}
		return m, resurrectAndAttachCmd(mgr, row.Name)

	case state.StatusBroken:
		m.err = fmt.Errorf("workspace %q is broken — see ~/.canopy/log/canopy.log; press R to re-run setup, or `canopy rm %s` to drop it",
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

	log.Warn("ui.attach.unknown-status", "name", row.Name, "status", row.Status)
	_ = ctx
	return m, nil
}

// findDeleteTargetRemoteHost returns (host, true) when the confirmed
// delete target is a remote row. Mirrors the resolveTargetMgr lookup
// but stops at the Host field instead of building a Manager.
func (m *Model) findDeleteTargetRemoteHost() (string, bool) {
	for _, r := range m.filteredRows() {
		if r.Name != m.deleteTarget {
			continue
		}
		if m.deleteTargetRoot != "" && r.ProjectRoot != m.deleteTargetRoot {
			continue
		}
		if r.Host != "" {
			return r.Host, true
		}
		return "", false
	}
	return "", false
}

// execRemoteVerb runs `<canopy-bin> <verb> --on <host> <args...>` as a
// subprocess via tea.ExecProcess. Same handoff pattern as
// attachRemoteRow + execHostAddWizard. Used for rm, retry, and the
// project-scoped dispatch paths. v0.17.0 Phase 1.
//
// force, when true, appends --force to the remote canopy invocation.
// Used by the delete handler when the user pressed F on hanging-work
// confirmation (mirrors the local --force path).
func (m *Model) execRemoteVerb(hostName, verb string, args []string, force bool) tea.Cmd {
	canopyBin, err := os.Executable()
	if err != nil || canopyBin == "" {
		canopyBin = os.Args[0]
	}
	cmdArgs := []string{verb, "--on", hostName}
	cmdArgs = append(cmdArgs, args...)
	if force {
		cmdArgs = append(cmdArgs, "--force")
	}
	cmd := exec.Command(canopyBin, cmdArgs...)
	cmd.Env = append(os.Environ(), "CANOPY_ALLOW_NESTED=1")
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.remote-verb.failed", "verb", verb, "host", hostName, "err", err)
		}
		// Refresh so the row updates / disappears as appropriate.
		m.remoteRefreshing = false
		return refreshCmd(m.mgr, m.tc, m.store)()
	})
}

// execRemoteKill kills a workspace's tmux session on a remote host by
// SSHing `tmux kill-session -t <session>` directly. Doesn't go through
// canopy on the remote because there's no `canopy kill` verb — kill is
// a tmux operation that leaves the workspace's worktree + state intact,
// transitioning it to stopped (the existing canopy convention).
func (m *Model) execRemoteKill(hostName, sessionName string) tea.Cmd {
	canopyBin, err := os.Executable()
	if err != nil || canopyBin == "" {
		canopyBin = os.Args[0]
	}
	// Use canopy as a stable entry point: `canopy host exec --on tower tmux ...`
	// would be cleaner, but that verb doesn't exist. Inline the SSH via
	// a one-shot subprocess instead. We use ssh directly here (not via
	// host.SSHCmd) because we're already in the parent process — no
	// need to re-resolve through cmd/canopy. The TUI's host registry is
	// the source of truth.
	resolved, err := m.resolveHostForExec(hostName)
	if err != nil {
		return func() tea.Msg {
			log.Warn("ui.remote-kill.resolve-failed", "host", hostName, "err", err)
			return refreshCmd(m.mgr, m.tc, m.store)()
		}
	}
	cmd := exec.Command("ssh",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(os.Getenv("HOME"), ".canopy", "ssh-%C.sock"),
		"-o", "ControlPersist=300",
		"-o", "BatchMode=yes",
		"-o", "NumberOfPasswordPrompts=0",
		resolved,
		"tmux", "kill-session", "-t", sessionName,
	)
	_ = canopyBin // placate the imports if we add a fallback later
	return func() tea.Msg {
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Warn("ui.remote-kill.failed", "host", hostName, "session", sessionName, "err", err, "out", string(out))
		}
		m.remoteRefreshing = false
		return refreshCmd(m.mgr, m.tc, m.store)()
	}
}

// resolveHostForExec looks up the SSH target for a host name from the
// in-memory registry snapshot the TUI already has (m.hostList).
// Avoids re-loading hosts.json every time the user kills/deletes a row.
func (m *Model) resolveHostForExec(name string) (string, error) {
	for _, h := range m.hostList {
		if h.Name == name {
			return h.SSHTarget, nil
		}
	}
	return "", fmt.Errorf("host %q not found in registry snapshot", name)
}

// execHostAddWizard hands the terminal off to `canopy host add
// --interactive` as a subprocess via tea.ExecProcess. The subprocess
// runs the huh form, probes connectivity, offers ssh-copy-id, and
// registers the host. On return, the TUI refreshes so the new host
// appears in the Hosts tab. v0.17.0 Phase 1c.
func (m *Model) execHostAddWizard() tea.Cmd {
	canopyBin, err := os.Executable()
	if err != nil || canopyBin == "" {
		canopyBin = os.Args[0]
	}
	cmd := exec.Command(canopyBin, "host", "add", "--interactive")
	cmd.Env = append(os.Environ(), "CANOPY_ALLOW_NESTED=1")
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.host-add-wizard.failed", "err", err)
		}
		// Force a fresh refresh so the new host appears immediately.
		// m.remoteRefreshing is reset by the rowsLoadedMsg handler;
		// we drop the in-flight latch here so refreshRemoteCmd fires.
		m.remoteRefreshing = false
		return refreshCmd(m.mgr, m.tc, m.store)()
	})
}

// attachRemoteRow dispatches a remote-host attach via canopy-as-subprocess.
// v0.17.0 Phase 1b. The subprocess is `<this-canopy> switch --on <host>
// <name>`, which itself uses syscall.Exec to mosh-attach. tea.ExecProcess
// hands the terminal to that subprocess; when mosh exits (user `prefix d`,
// network drop, or remote tmux ended), control returns and we kick a
// refresh to repopulate cached rows.
//
// Why subprocess (not direct mosh exec from the TUI)? Two reasons:
//  1. We get the entire canopy-switch flow for free — reconcile,
//     resurrect-if-stopped, role backfill, error-message normalization.
//     Reproducing that in the TUI doubles the maintenance burden.
//  2. Bubbletea stays alive across the attach. syscall.Exec inside the
//     TUI would replace the whole process tree; the TUI couldn't redraw
//     on detach. tea.ExecProcess is the canonical tmux-attach idiom for
//     Bubbletea apps; reusing it for mosh-attach matches the pattern.
func (m *Model) attachRemoteRow(row Row) tea.Cmd {
	canopyBin, err := os.Executable()
	if err != nil || canopyBin == "" {
		// Fallback to os.Args[0]; this is what we got launched as so
		// it'll always resolve. Edge case is if the user moved the
		// binary after launch — rare enough to not engineer around.
		canopyBin = os.Args[0]
	}
	args := []string{"switch", "--on", row.Host, row.Name}
	cmd := exec.Command(canopyBin, args...)
	cmd.Env = os.Environ()
	// Canopy switch wraps itself in a nested-canopy guard that blocks
	// running inside a canopy tmux session. The TUI IS canopy, and
	// we're about to hand off to mosh which is OK (mosh+tmux on a
	// DIFFERENT machine doesn't conflict with the local tmux pane the
	// TUI lives in), so allow nesting here. Removing the env var
	// after the subprocess exits would be ideal but tea.ExecProcess
	// inherits env at exec time; mosh's child gets the var too, which
	// is harmless (only affects in-mosh-session canopy invocations
	// that the user would have to do deliberately).
	cmd.Env = append(cmd.Env, "CANOPY_ALLOW_NESTED=1")
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		// Detached (or error). Trigger a refresh so cached remote rows
		// reflect any state changes that happened during the attach
		// (e.g., workspace deleted on the remote, status flipped).
		if err != nil {
			log.Warn("ui.attach-remote.failed", "host", row.Host, "name", row.Name, "err", err)
		}
		return refreshCmd(m.mgr, m.tc, m.store)()
	})
}

// effectiveStatus returns the status the Enter dispatcher should act on.
// For workspace rows recorded as ready, BuildGlobalRows already probes
// the tmux session via HasSession and stamps the result on row.Alive —
// when that probe says the session is gone, the recorded "ready" status
// is stale (someone killed the session out-of-band) and the right action
// is to resurrect, not attempt an attach that will fail with
// ErrSessionNotFound.
//
// Main rows are excluded: attachSelected handles the IsMain branch
// before reaching here, and main-row liveness drives a different path
// (EnsureMainSession). Stopped/broken/orphaned rows pass through
// unchanged — Alive is informational for them, not authoritative.
func effectiveStatus(row Row) state.Status {
	if !row.IsMain && row.Status == state.StatusReady && !row.Alive {
		return state.StatusStopped
	}
	return row.Status
}

// attachOrSwitch dispatches the right tmux verb for the current context:
// switch-client + tea.Quit when CANOPY_IN_POPUP=1 (popup mode), or
// tea.ExecProcess attach for fullscreen mode. Single source of truth
// replacing the GlobalModel.popupSwitchAndQuit / Model.attachCmd split.
//
// Uses m.tc directly (always non-nil) rather than reaching into m.mgr.Tmux,
// which would panic when invoked from outside any project (mgr nil).
// The post-attach refresh is dispatched via m.store + m.tc rather than
// the project-only mgr.Reconcile path so it works in both contexts.
func (m *Model) attachOrSwitch(session string) tea.Cmd {
	// Backfill @canopy-role tags for v0.15-style sessions that never
	// went through the v0.16+ buildSession (which tags at creation).
	// Best-effort: errors logged, never block attach. Common path for
	// both popup (switch-client) and fullscreen (tea.ExecProcess) modes.
	// launcherType: pulls from this canopy invocation's project config
	// when available; empty in Global tab cross-project flows where
	// m.mgr is nil. Empty defaults to "agent:claude" via agent.RoleForType,
	// which matches every v0.15 workspace's actual agent.
	var launcherType string
	if m.mgr != nil {
		launcherType = m.mgr.Cfg.Agent.Type
	}
	workspace.BackfillRoles(context.Background(), m.tc, session, launcherType)

	if m.inPopup {
		return func() tea.Msg {
			if err := m.tc.SwitchClient(context.Background(), session); err != nil {
				log.Warn("ui.popup.switch_client_failed", "session", session, "err", err.Error())
			}
			return tea.QuitMsg{}
		}
	}
	// Fullscreen mode: tea.ExecProcess attach. Build the tmux command
	// directly via the embedded tmux.Client; refresh on detach.
	cmd, err := m.tc.AttachCmd(context.Background(), session)
	if err != nil {
		return func() tea.Msg { return rowsLoadedMsg{err: err} }
	}
	mgr, store, tc := m.mgr, m.store, m.tc
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.attach.exec-failed", "session", session, "err", err)
			return rowsLoadedMsg{err: fmt.Errorf("attach %s: %w", session, err)}
		}
		return refreshCmd(mgr, tc, store)()
	})
}

// deletingCurrentSession reports whether the about-to-be-deleted
// workspace is the one this canopy invocation is sitting inside. True
// in two cases:
//
//   - Popup mode: the popup was opened from inside workspace X
//     (`-d "#{pane_current_path}"` carried the cwd) and the user is
//     deleting X.
//   - Fullscreen mode: canopy was launched from inside workspace X
//     (cwd matched X's path at startup, so currentWorkspace was set)
//     and the user is deleting X.
//
// Both shapes of "I'm deleting the workspace I'm in" need the same
// escape: spawn the cleanup as a detached subprocess and detach the
// tmux client, so canopy can exit cleanly before the session it's
// hosted by dies.
//
// Returns false when there's no current workspace tracked, or when
// the (root, name) pair doesn't match.
func (m *Model) deletingCurrentSession(projectRoot, name string) bool {
	if m.currentWorkspace == "" {
		return false
	}
	return name == m.currentWorkspace && projectRoot == m.currentWorkspaceRoot
}

// detachAndRemoveCmd handles the "delete the workspace I'm currently
// sitting in" case for both popup and fullscreen modes. Replaces the
// busyMode flow + escapeIfDeletingCurrent's SwitchClient(canopy-main):
// the older path auto-built the project main session (slow nvim+claude
// spin-up), which the user perceived as "tmux loaded a random
// session." It also doesn't help fullscreen mode — switching the tmux
// *client* to main doesn't move the canopy *process* off the doomed
// pane, so canopy would still die mid-cleanup. Instead:
//
//  1. Spawn a detached `canopy rm <name> --yes --force` subprocess.
//     Setsid + Process.Release disowns it so it survives our exit and
//     completes cleanup independently. Logs go to ~/.canopy/log/canopy.log
//     via the standard logger; stdio is detached.
//  2. Detach the calling tmux client. The popup overlay closes (popup
//     mode) or the user's terminal returns from `tmux attach` to its
//     parent shell (fullscreen mode). When canopy was launched from a
//     non-tmux shell, $TMUX is unset and detach-client errors out
//     harmlessly — the cleanup-then-quit sequence still works.
//  3. tea.Quit. Belt-and-suspenders: detach-client SIGHUPs us anyway,
//     but explicit Quit makes the path predictable for tests.
//
// Errors are logged, not surfaced — canopy is closing and there's no
// UI left to show them in. The detached subprocess will retry-style
// surface any cleanup failure as a `broken`/`orphaned` row next time
// the user opens canopy.
func (m *Model) detachAndRemoveCmd(projectRoot, name string) tea.Cmd {
	tc := m.tc
	return func() tea.Msg {
		if exe, err := os.Executable(); err == nil {
			cmd := exec.Command(exe, "rm", "--yes", "--force", name)
			cmd.Dir = projectRoot
			cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				log.Warn("ui.popup-delete.spawn-failed", "name", name, "err", err.Error())
			} else {
				_ = cmd.Process.Release()
			}
		} else {
			log.Warn("ui.popup-delete.executable-lookup-failed", "err", err.Error())
		}
		if err := tc.DetachClient(context.Background()); err != nil {
			log.Warn("ui.popup-delete.detach-failed", "err", err.Error())
		}
		return tea.QuitMsg{}
	}
}

// escapeIfDeletingCurrent moves the user's tmux client off the workspace
// that's about to be removed. Without this, deleting the workspace whose
// session hosts the popup (or whose tmux session the user is attached to)
// strands the client when Manager.Remove kills the session.
//
// Triggered when (projectRoot, name) matches (currentWorkspaceRoot,
// currentWorkspace). Match is on the full pair because workspace names
// are unique per project, not globally — A/foo and B/foo coexist. We
// bring up the project's main session if it isn't already (idempotent —
// the `enter`-on-dead-main-row path uses the same EnsureMainSession),
// then switch-client to it. Both calls are best-effort: failures don't
// block the delete, just log so the user gets the diagnostic if their
// client did get stranded.
//
// No-op when currentWorkspace is empty (popup launched from outside any
// workspace) or doesn't match the (root, name) pair (user is deleting a
// different workspace than the one they're sitting in).
func (m *Model) escapeIfDeletingCurrent(mgr *workspace.Manager, projectRoot, name string) {
	if mgr == nil || m.currentWorkspace == "" {
		return
	}
	if name != m.currentWorkspace || projectRoot != m.currentWorkspaceRoot {
		return
	}
	ctx := context.Background()
	mainSession, err := mgr.EnsureMainSession(ctx)
	if err != nil {
		log.Warn("ui.delete.ensure-main-failed", "name", name, "err", err.Error())
		return
	}
	if err := m.tc.SwitchClient(ctx, mainSession); err != nil {
		log.Warn("ui.delete.switch-client-failed", "target", mainSession, "err", err.Error())
	}
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
		return attachAfterMsg{session: ws.TmuxSessionName()}
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
		m.clearNewTarget()
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
	case "t":
		// 't' for "task" — see newPickerOptions for the letter-choice
		// rationale (no good mnemonic for "prompt", and `p` is taken).
		return m.openNewPrompt()

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
		// Indices follow newPickerOptions order: fresh, prompt, PR,
		// issue, branch.
		switch m.newPickerCursor {
		case 0:
			return m.openNewFresh(), textinputBlink()
		case 1:
			return m.openNewPrompt()
		case 2:
			return m, m.openNewPR()
		case 3:
			return m, m.openNewIssue()
		case 4:
			return m, m.openNewBranch()
		}
		return m, nil
	}
	return m, nil
}

// newPickerOptionCount is the number of options in the variant
// picker. Used to bound cursor nav. Update if newPickerOption is
// extended.
const newPickerOptionCount = 5

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
		return m, createCmd(m.newTargetMgr, name, spec)
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

// openNewPrompt prepares the "fresh + prompt" sub-modal (the 5th
// picker option). Mirrors openNewFresh but focuses the prompt
// textarea. Workspace name = namegen (no name input in this mode —
// the prompt is the user-facing primary; if they want an explicit
// name they can use the CLI's `canopy new --name foo --prompt ...`).
// Returns the cursor-blink cmd from textarea.Focus so the caret
// blinks immediately on open.
func (m *Model) openNewPrompt() (*Model, tea.Cmd) {
	m.mode = newPromptMode
	m.promptInput.Reset()
	blink := m.promptInput.Focus()
	return m, blink
}

// handleNewPromptKey is the keymap for the "fresh + prompt" sub-modal.
//
// Esc steps back to the picker. Ctrl+S submits when the prompt is
// non-empty (an empty Ctrl+S is a no-op — the placeholder already
// telegraphs the requirement, so an inline error would be noise).
// Enter inserts a newline because the prompt is a textarea, not a
// textinput — multi-line briefings are the whole point of the
// upgrade from single-line input.
//
// Anything else falls through to the textarea so the user can type
// normally.
func (m *Model) handleNewPromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil
	case "ctrl+s":
		prompt := strings.TrimSpace(m.promptInput.Value())
		if prompt == "" {
			return m, nil
		}
		// Stash the prompt on the model so createDoneMsg can pick it
		// up after Create succeeds. The actual send happens between
		// Create-success and attach.
		m.pendingPrompt = prompt
		spec := workspace.SourceSpec{} // fresh = zero spec
		m.busyOp = busyOpCreate
		m.busyTitle = "Creating workspace + prompting agent..."
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.mode = busyMode
		m.promptInput.Blur()
		return m, createCmd(m.newTargetMgr, "", spec)
	}
	var cmd tea.Cmd
	m.promptInput, cmd = m.promptInput.Update(msg)
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
	return tea.Batch(textinputBlink(), loadPRsCmd(m.newTargetRoot))
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
	return m, createCmd(m.newTargetMgr, "", spec)
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
	return tea.Batch(textinputBlink(), loadIssuesCmd(m.newTargetRoot))
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
	return m, createCmd(m.newTargetMgr, "", spec)
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
	return tea.Batch(textinputBlink(), loadBranchesCmd(m.newTargetRoot))
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
	return m, createCmd(m.newTargetMgr, "", spec)
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
	// New-workspace target lives across the whole flow (picker → sub-modal
	// → busy); clear it on busy dismiss so the next `n` press is fresh.
	// Safe even for non-create busy ops (Remove, Retry) — newTargetMgr is
	// only set by actionNewWorkspace and harmless if already nil.
	m.clearNewTarget()
	return m, m.refresh()
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

	// v0.17.0: if the target is a remote row (row.Host != ""), dispatch
	// to that host's canopy rm via subprocess instead of going through
	// the local Manager. The same y/F gate applies — only proceed on
	// the deliberate key.
	if remoteHost, isRemote := m.findDeleteTargetRemoteHost(); isRemote {
		// Same keypress contract as the local path: F for hangs, y/Y
		// otherwise; anything else cancels.
		go_ := (hasHangs && msg.String() == "F") || (!hasHangs && (msg.String() == "y" || msg.String() == "Y"))
		if !go_ {
			m.mode = listMode
			m.deleteTarget = ""
			m.deleteTargetRoot = ""
			m.deleteHangs = nil
			return m, nil
		}
		name := m.deleteTarget
		m.mode = listMode
		m.deleteTarget = ""
		m.deleteTargetRoot = ""
		m.deleteHangs = nil
		return m, m.execRemoteVerb(remoteHost, "rm", []string{name, "--yes"}, hasHangs /* --force */)
	}

	// Resolve the target row's Manager (may be transient for cross-project
	// rows on Global tab). Match against BOTH ProjectRoot AND Name —
	// matching by Name alone would let two projects with same-named
	// workspaces ("foo") confuse the modal: a refresh between modal-open
	// and confirm could put project B's "foo" at the position project
	// A's "foo" was at, leading to deleting the wrong workspace. Store
	// + match the (Project, Name) pair to snapshot the user's intent at
	// modal-open time and survive any reordering.
	//
	// Backward-compat: if deleteTargetRoot is empty (modal opened by an
	// older code path before the field was added), fall through to the
	// name-only match — losing exactness for legacy paths but avoiding
	// a hard cancel mid-upgrade.
	resolveTargetMgr := func() (*workspace.Manager, bool) {
		rows := m.filteredRows()
		for _, r := range rows {
			if r.Name != m.deleteTarget {
				continue
			}
			if m.deleteTargetRoot != "" && r.ProjectRoot != m.deleteTargetRoot {
				continue
			}
			mgr, err := m.managerForRow(r)
			if err != nil {
				m.err = err
				return nil, false
			}
			return mgr, true
		}
		// Row went away between modal open and confirm — treat as cancel.
		return nil, false
	}

	// Force key (capital F): only valid path when hangs exist.
	if msg.String() == "F" && hasHangs {
		mgr, ok := resolveTargetMgr()
		if !ok {
			m.mode = listMode
			m.deleteTarget = ""
		m.deleteTargetRoot = ""
			m.deleteHangs = nil
			return m, nil
		}
		name := m.deleteTarget
		root := m.deleteTargetRoot
		m.deleteTarget = ""
		m.deleteTargetRoot = ""
		m.deleteHangs = nil
		// Deleting-the-workspace-I'm-in (popup OR fullscreen): skip
		// busyMode and run the cleanup as a detached subprocess. See
		// detachAndRemoveCmd for rationale.
		if m.deletingCurrentSession(root, name) {
			return m, m.detachAndRemoveCmd(root, name)
		}
		m.escapeIfDeletingCurrent(mgr, root, name)
		m.mode = busyMode
		m.busyOp = busyOpRemove
		m.busyTitle = fmt.Sprintf("Force-removing workspace %q...", name)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		return m, removeCmd(mgr, name)
	}

	// Normal y/Y: only valid when no hangs.
	if !hasHangs && (msg.String() == "y" || msg.String() == "Y") {
		mgr, ok := resolveTargetMgr()
		if !ok {
			m.mode = listMode
			m.deleteTarget = ""
		m.deleteTargetRoot = ""
			m.deleteHangs = nil
			return m, nil
		}
		name := m.deleteTarget
		root := m.deleteTargetRoot
		m.deleteTarget = ""
		m.deleteTargetRoot = ""
		m.deleteHangs = nil
		if m.deletingCurrentSession(root, name) {
			return m, m.detachAndRemoveCmd(root, name)
		}
		m.escapeIfDeletingCurrent(mgr, root, name)
		m.mode = busyMode
		m.busyOp = busyOpRemove
		m.busyTitle = fmt.Sprintf("Removing workspace %q...", name)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		return m, removeCmd(mgr, name)
	}

	// Anything else cancels (n, N, esc, enter, stray keys, lowercase y
	// when hangs are present).
	m.mode = listMode
	m.deleteTarget = ""
		m.deleteTargetRoot = ""
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

// promptSentMsg fires after workspace.SendInitialPrompt finishes
// (either successfully delivered the prompt, or failed with an
// ErrPromptFailed). Carries the session name so the createDoneMsg
// follow-up can dispatch the attach without re-deriving it. err
// is non-nil only when the prompt didn't get delivered — the
// workspace itself is alive either way.
type promptSentMsg struct {
	session string
	err     error
}

// sendPromptCmd dispatches workspace.SendInitialPrompt in a
// goroutine, then emits promptSentMsg with the result. mgr.Tmux
// is the tmux client that knows about the freshly-created session.
// io.Discard for the progress writer because the TUI doesn't render
// that carriage-return progress line — the busyMode "Sending prompt..."
// title is the equivalent feedback.
func sendPromptCmd(mgr *workspace.Manager, session, prompt string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := workspace.SendInitialPrompt(ctx, mgr.Tmux, session, session, prompt, io.Discard)
		return promptSentMsg{session: session, err: err}
	}
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
//
// force=false matches the CLI's default — RetrySetup refuses to re-run
// on a non-broken workspace. Use retryCmdUIForce when the user has
// confirmed via the y/N modal (D3/CP1).
func retryCmdUI(mgr *workspace.Manager, name string) tea.Cmd {
	return retryCmdUIWithForce(mgr, name, false)
}

// retryCmdUIForce is the post-confirm-modal variant that passes
// force=true to Manager.RetrySetup, mirroring the CLI's --force flag.
// Triggered from confirmRetryMode after the user presses y on a
// non-broken workspace.
func retryCmdUIForce(mgr *workspace.Manager, name string) tea.Cmd {
	return retryCmdUIWithForce(mgr, name, true)
}

func retryCmdUIWithForce(mgr *workspace.Manager, name string, force bool) tea.Cmd {
	return func() tea.Msg {
		buf := &safeBuffer{}
		done := make(chan retryDoneMsg, 1)
		go func() {
			_, err := mgr.RetrySetup(context.Background(), name, force, buf, buf)
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
//     1. progressTickCmd — re-fires every 150ms, drains the
//     buffer, emits progressTickMsg with the new chunk. The
//     tick re-schedules itself in Update until busyDone.
//     2. waitDoneCmd — blocks reading from `done`, emits
//     createDoneMsg when the goroutine finishes.
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
				msg.tmuxSession = ws.TmuxSessionName()
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
