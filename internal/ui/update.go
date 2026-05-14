package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/lifecycle"
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
		// Reserve lines for the chrome above + below the table:
		//   1 brand pill row
		//   1 tab bar
		//   1 blank
		//   1 "add a project" hint (Global tab) OR per-row "hint:" badge
		//   5 help legend (one group per line — see renderHelpLine)
		//   1 trailing blank / margin
		// projectlist now crops + shows ↑N/↓N more markers when its
		// envelope is too small, so under-reserving is safe (the user
		// still sees the scroll indicators) — over-reserving just
		// shrinks the table unnecessarily.
		reserve := 9
		if m.height > 0 && m.height < 20 {
			// Compact help collapses to one line — shave 4 lines.
			reserve -= 4
		}
		if m.inPopup {
			reserve-- // single-line tab bar / tighter chrome
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

	case hostUpgradeShellStartedMsg:
		// Lazy-spawn bridge from hostUpgradeStartCmd. Same shape as
		// upgradeShellStartedMsg: latch buf + cancel, kick off the
		// tick + waitDone Cmds. State was already set to Running by
		// the confirming-state Y handler.
		m.hostUpgradeBuf = msg.buf
		m.hostUpgradeCancel = msg.cancel
		return m, tea.Batch(
			hostUpgradeTickCmd(msg.buf),
			hostUpgradeWaitDoneCmd(msg.done),
		)

	case hostUpgradeTickMsg:
		if msg.chunk != "" {
			m.hostUpgradeOutput += msg.chunk
		}
		if m.hostUpgradeState != hostUpgradeStateRunning {
			return m, nil
		}
		return m, hostUpgradeTickCmd(msg.buf)

	case hostUpgradeShellDoneMsg:
		// Terminal state. Same shape as upgradeShellDoneMsg but writes
		// into the host-upgrade fields so both flows can coexist
		// without aliasing.
		if msg.output != "" {
			m.hostUpgradeOutput += msg.output
		}
		m.hostUpgradeErr = msg.err
		if msg.err == nil {
			m.hostUpgradeState = hostUpgradeStateDoneOK
		} else {
			m.hostUpgradeState = hostUpgradeStateDoneError
		}
		m.hostUpgradeCancel = nil
		return m, nil

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
		// Remote create: parse the workspace name from the streamed
		// output and mosh-attach directly. canopy new --on always runs
		// with --no-attach on the remote (we can't local-attach to a
		// session that lives on tower), but we can immediately follow
		// up with `canopy switch --on host <name>` which handles the
		// mosh handoff. Falls back to "press Enter on the new row" if
		// the name can't be parsed (e.g., partial output on error).
		//
		// Exit code 2 from the remote canopy = "workspace OK, prompt
		// delivery failed" (workspace.IsPromptFailed). Treat as success
		// for auto-attach: the workspace is alive on disk, only the
		// initial agent prompt didn't land. Better to drop the user into
		// the live workspace than to strand them in busyMode over a
		// prompt issue they can re-issue manually.
		if msg.remote {
			workspaceOK := msg.err == nil
			if !workspaceOK {
				if ee, ok := msg.err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
					workspaceOK = true
				}
			}
			if workspaceOK {
				if name := parseRemoteWorkspaceName(m.busyOutput); name != "" && m.newTargetHost != "" {
					host := m.newTargetHost
					project := m.newTargetName
					row := Row{Host: host, Name: name, Project: project}
					m.mode = listMode
					m.busyOp = busyOpNone
					m.busyTitle = ""
					m.busyOutput = ""
					m.busyDone = false
					m.clearNewTarget()
					return m, m.attachRemoteRow(row, false)
				}
				m.busyTitle = "Remote workspace ready. Press any key, then Enter on the new row to attach."
			}
			return m, nil
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

	case refreshAllMsg:
		// Trigger a full local+remote refresh. Emitted by tea.Cmds (e.g.
		// post-remote-rm) that need to invalidate the cached remote rows
		// too — refreshCmd alone only updates local. v0.17 Phase 1h.
		return m, m.refresh()

	case hostProbeResultMsg:
		// Post-Add probe result. AuthFailed → open the ssh-copy-id
		// offer modal. Other errors surface on m.err so the user
		// knows the host is registered but unreachable. Success is
		// silent — the refresh will surface the green online pill.
		if msg.authFail {
			m.pendingProbeHost = msg.hostName
			m.pendingProbeTarget = msg.sshTarget
			m.mode = confirmSSHCopyIDMode
			return m, nil
		}
		if msg.err != nil {
			m.err = fmt.Errorf("host %q registered, but probe failed: %w", msg.hostName, msg.err)
		}
		return m, nil

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
	case confirmAttachMode:
		return m.handleConfirmAttachKey(msg)
	case confirmHostRemoveMode:
		return m.handleConfirmHostRemoveKey(msg)
	case addHostFormMode:
		return m.handleAddHostFormKey(msg)
	case hostDetailMode:
		return m.handleHostDetailKey(msg)
	case confirmSSHCopyIDMode:
		return m.handleConfirmSSHCopyIDKey(msg)
	case confirmHostSSHMode:
		return m.handleConfirmHostSSHKey(msg)
	case confirmHostClipboardMode:
		return m.handleConfirmHostClipboardKey(msg)
	case confirmRetryMode:
		return m.handleConfirmRetryKey(msg)
	case drawerMode:
		return m.handleDrawerKey(msg)
	case busyMode:
		return m.handleBusyModeKey(msg)
	case upgradeMode:
		return m.handleUpgradeKey(msg)
	case hostUpgradeMode:
		return m.handleHostUpgradeKey(msg)
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
		return m, m.openAddHostForm()
	}
	// v0.17 Phase 1k: on a remote row, open the SAME TUI picker as
	// local rows — Fresh + Prompt submit handlers branch on
	// newTargetHost to dispatch `canopy new --on <host>` instead of
	// the local createCmd. PR/Issue/Branch options are hidden in the
	// picker for remote targets (they need remote gh integration).
	if row, ok := m.list.CursorRow(); ok && row.Host != "" {
		m.newTargetHost = row.Host
		m.newTargetRemoteCwd = m.remoteCwdForRow(row.Host, row.Project)
		m.newTargetName = row.Project
		m.newTargetRoot = ""
		m.newTargetMgr = nil
		m.openNewPicker()
		return m, nil
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
	m.newTargetHost = ""
	m.newTargetRemoteCwd = ""
	m.pendingPrompt = ""
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

// refreshAllMsg asks Update to dispatch a combined local+remote refresh.
// Use this from inside tea.Cmd closures (e.g. post-remote-action
// callbacks) when both row sources may have changed — local-only
// refreshCmd would leave remote rows stale until the next 2s tick.
type refreshAllMsg struct{}
// Drawer (i / b) lives in drawer.go. actionInspect, handleDrawerKey,
// drawerLoadCmd, drawerLoadedMsg, bareAttachCmd are defined there.

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

