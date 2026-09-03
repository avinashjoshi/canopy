// Delete flow (d keybind). Confirm-delete modal (y / F for force on
// hanging work) → Manager.Remove streaming through the busy view.
// Remote rows dispatch through `canopy rm --on host`. "Deleting the
// workspace I'm in" gets the detached-subprocess escape so canopy can
// exit before the session hosting it dies. Carved out of update.go.

package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/workspace"
)

// actionDelete opens the confirm-delete modal for the cursor row. Cross-
// project rows construct a transient Manager via managerForRow; same
// path as the same-project case so the confirm modal copy is uniform.
func actionDelete(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok || row.Loading {
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

// findDeleteTargetRemoteHost returns (host, project, true) when the
// confirmed delete target is a remote row. Mirrors the resolveTargetMgr
// lookup but stops at the Host/Project fields instead of building a
// Manager. The project comes along so the remote-dispatch path can
// pass --remote-cwd and bypass cmd/canopy's first-project-on-host
// fallback (which renders as the scary "(fallback)" annotation in
// dispatch source strings).
func (m *Model) findDeleteTargetRemoteHost() (host, project, remoteProjectPath string, ok bool) {
	for _, r := range m.filteredRows() {
		if r.Name != m.deleteTarget {
			continue
		}
		if m.deleteTargetRoot != "" && r.ProjectRoot != m.deleteTargetRoot {
			continue
		}
		if r.Host != "" {
			return r.Host, r.Project, r.RemoteProjectPath, true
		}
		return "", "", "", false
	}
	return "", "", "", false
}

// handleConfirmDeleteKey is the keymap while the delete prompt is up.
//
// Two modes based on whether the v0.6 SafetyPreflight detected hangs:
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
	// the local Manager.
	if remoteHost, remoteProject, remoteProjectPath, isRemote := m.findDeleteTargetRemoteHost(); isRemote {
		// Remote path: laptop didn't run the safety check (canopy.json
		// only exists on tower), so the modal offers both y AND F.
		// y → dispatch without --force (remote will refuse on hanging
		//     work but otherwise proceed).
		// F → dispatch with --force (skip the safety check).
		// Anything else cancels. v0.17 Phase 1l polish.
		force := msg.String() == "F"
		yes := msg.String() == "y" || msg.String() == "Y"
		if !force && !yes {
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
		// Pin the dispatch to the row's known (host, project) so cmd/canopy's
		// resolveOnForSwitch doesn't fall back to "first project on host"
		// — that fallback path is the one that prints `(fallback)` in the
		// dispatch source and can land an rm in the WRONG project entirely
		// when the host has multiple registered projects.
		args := append([]string{name, "--yes"}, m.remoteCwdArg(remoteHost, remoteProject, remoteProjectPath)...)
		return m, m.execRemoteVerb(remoteHost, "rm", args, force)
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
// sitting in" case for both popup and fullscreen modes:
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

// removeDoneMsg is the Remove counterpart to createDoneMsg. Same shape;
// kept distinct so future Update logic can branch (e.g. don't try to
// attach to a workspace that was just removed).
type removeDoneMsg struct {
	output string
	err    error
}

// removeStartedMsg is the lazy-spawn bridge for the remove flow,
// mirroring createStartedMsg. Update on receipt: dispatch
// tea.Batch(progressTickCmd, waitRemoveDoneCmd) so the archive
// script's output streams live just like Create.
type removeStartedMsg struct {
	buf  *safeBuffer
	done <-chan removeDoneMsg
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

// waitRemoveDoneCmd blocks on the remove done-chan and emits the
// removeDoneMsg when archive completes.
func waitRemoveDoneCmd(done <-chan removeDoneMsg) tea.Cmd {
	return func() tea.Msg { return <-done }
}
