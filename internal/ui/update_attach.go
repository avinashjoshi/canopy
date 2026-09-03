// Attach flow (enter keybind). Routes per row status: ready → attach,
// stopped → resurrect-then-attach, broken/orphaned/setting_up → status
// hint. Popup mode uses switch-client + tea.Quit; fullscreen uses
// tea.ExecProcess. Confirm-attach gate fires when another tmux client
// is already on the session. Remote rows dispatch through `canopy
// switch --on host` (mosh). Carved out of update.go.

package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// actionAttach is the enter-key flow. Resurrects stopped workspaces
// first; popup-mode uses switch-client + tea.Quit instead of attach.
func actionAttach(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.attachSelected()
}

// handleConfirmAttachKey is the y/N gate for attaching to a session
// that already has another client. v0.17 Phase 1j.
//
// y or Y proceeds with the attach; anything else cancels. Cancel-by-
// default — accidental Enter shouldn't auto-share a live agent
// session. Tmux's native behavior is to share, not steal (multiple
// clients on the same session is fine), but the user wants explicit
// confirmation that two-people-on-one-agent state is intentional.
func (m *Model) handleConfirmAttachKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "y" || msg.String() == "Y" || msg.Type == tea.KeyEnter {
		target := m.attachTarget
		m.mode = listMode
		m.attachTarget = Row{}
		return m.doAttach(target)
	}
	// Anything else (n, N, esc, q, etc.) cancels.
	m.mode = listMode
	m.attachTarget = Row{}
	return m, nil
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
	if !ok || row.Loading {
		return m, nil
	}

	// v0.17 Phase 1j: warn before stealing/sharing a session that
	// already has another client connected. Skip the prompt when the
	// target is the workspace canopy itself was launched from — that's
	// "re-attach my own session," the normal expected flow.
	if row.Attached && !m.isCurrentRow(row) {
		m.mode = confirmAttachMode
		m.attachTarget = row
		return m, nil
	}
	return m.doAttach(row)
}

// isCurrentRow reports whether a row is the workspace canopy was
// invoked from. Used by attachSelected to skip the "already attached"
// warning when the user is re-attaching their own session — that's
// the normal flow, not a steal.
func (m *Model) isCurrentRow(row Row) bool {
	if m.currentWorkspace == "" {
		return false
	}
	return row.Name == m.currentWorkspace && row.ProjectRoot == m.currentWorkspaceRoot
}

// doAttach is the actual attach logic — extracted from attachSelected
// so confirmAttachMode's `y` handler can re-enter without re-running
// the warn check.
//
// shared, when true, attaches without kicking off existing clients on
// the same session (multi-attach). Set when the user explicitly opted
// into sharing via the confirm-attach modal; the warning copy promises
// "share" and this is what actually delivers it. v0.17 Phase 1j.
func (m *Model) doAttach(row Row) (tea.Model, tea.Cmd) {
	shared := row.Attached
	ctx := context.Background()

	// v0.17.0 Phase 1b: remote rows dispatch through `canopy switch
	// --on <host> <name>` as a subprocess. The subprocess does the
	// SSH/mosh dance and exec-replaces itself with mosh; mosh runs in
	// the terminal Bubbletea hands over, and on detach control returns
	// here for the refresh that re-syncs cached remote-row state.
	if row.Host != "" {
		return m, m.attachRemoteRow(row, shared)
	}

	if row.IsMain {
		if row.Alive {
			return m, m.attachOrSwitchWithOpts(row.TmuxSession, shared)
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
		return m, m.attachOrSwitchWithOpts(session, shared)
	}

	switch effectiveStatus(row) {
	case "main", state.StatusReady:
		return m, m.attachOrSwitchWithOpts(row.TmuxSession, shared)

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
func (m *Model) attachRemoteRow(row Row, shared bool) tea.Cmd {
	canopyBin, err := os.Executable()
	if err != nil || canopyBin == "" {
		// Fallback to os.Args[0]; this is what we got launched as so
		// it'll always resolve. Edge case is if the user moved the
		// binary after launch — rare enough to not engineer around.
		canopyBin = os.Args[0]
	}
	args := []string{"switch", "--on", row.Host}
	cwd := m.remoteCwdForRow(row.Host, row.Project, row.RemoteProjectPath)
	// For IsMain rows, --remote-cwd is load-bearing: the remote canopy
	// needs to be cd'd into the right project before `canopy main`
	// walks up looking for canopy.json. Without it, resolveOnForSwitch
	// falls back to the local cwd basename or first registered project
	// — silently attaching to a different project's main session.
	// Refuse rather than silently mis-attach.
	if row.IsMain && cwd == "" {
		return func() tea.Msg {
			return errMsg{err: fmt.Errorf("can't attach to %s on %s: project not registered for that host (run `canopy project add %s <remote-path> --on %s`)", row.Project, row.Host, row.Project, row.Host)}
		}
	}
	if cwd != "" {
		args = append(args, "--remote-cwd", cwd)
	}
	if row.IsMain {
		// Use the explicit flag rather than passing "(main)" as a name —
		// the dispatch keys off the flag so a real workspace named
		// "(main)" wouldn't be silently redirected to the main session.
		args = append(args, "--main")
	} else {
		args = append(args, row.Name)
	}
	if shared {
		// v0.17 Phase 1j: propagates the laptop-side "user confirmed
		// share" decision through to the remote canopy switch so it
		// skips detach-other-clients.
		args = append(args, "--share")
	}
	cmd := exec.Command(canopyBin, args...)
	cmd.Env = os.Environ()
	// Canopy switch wraps itself in a nested-canopy guard that blocks
	// running inside a canopy tmux session. The TUI IS canopy, and
	// we're about to hand off to mosh which is OK (mosh+tmux on a
	// DIFFERENT machine doesn't conflict with the local tmux pane the
	// TUI lives in), so allow nesting here.
	cmd.Env = append(cmd.Env, "CANOPY_ALLOW_NESTED=1")
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.attach-remote.failed", "host", row.Host, "name", row.Name, "err", err)
		}
		// remoteRefreshing is cleared in the refreshAllMsg handler in
		// Update — never from a tea.Cmd goroutine. Touching m here
		// would race the View+spinner read path.
		return refreshAllMsg{}
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

// attachOrSwitch is the non-shared (default) attach: kicks off existing
// clients to match canopy's solo-dev steal default. The shared variant
// (multi-client) is attachOrSwitchWithOpts, dispatched by doAttach when
// the user confirmed the attach-already-attached prompt.
func (m *Model) attachOrSwitch(session string) tea.Cmd {
	return m.attachOrSwitchWithOpts(session, false)
}

// attachOrSwitchWithOpts is the underlying implementation. shared=false
// matches the historical AttachCmd behavior; shared=true skips
// detach-other-clients + the -d flag so multiple tmux clients can
// coexist on the target session. v0.17 Phase 1j.
//
// Uses m.tc directly (always non-nil) rather than reaching into m.mgr.Tmux,
// which would panic when invoked from outside any project (mgr nil).
// The post-attach refresh is dispatched via m.store + m.tc rather than
// the project-only mgr.Reconcile path so it works in both contexts.
func (m *Model) attachOrSwitchWithOpts(session string, shared bool) tea.Cmd {
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
			// SwitchClientWithOptions honors `shared`: when true it
			// skips the internal detachOtherClients call so pre-existing
			// clients keep their attach. v0.17 Phase 1j.
			if err := m.tc.SwitchClientWithOptions(context.Background(), session, tmux.AttachOptions{Shared: shared}); err != nil {
				log.Warn("ui.popup.switch_client_failed", "session", session, "err", err.Error())
			}
			return tea.QuitMsg{}
		}
	}
	// Fullscreen mode: tea.ExecProcess attach. Build the tmux command
	// directly via the embedded tmux.Client; refresh on detach.
	cmd, err := m.tc.AttachCmdWithOptions(context.Background(), session, tmux.AttachOptions{Shared: shared})
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
