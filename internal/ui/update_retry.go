// Retry flow (R keybind). Re-runs Manager.RetrySetup for the cursor
// workspace; non-broken status requires a y/N confirm (D3/CP1).
// Cross-project goes through managerForRow. Carved out of update.go.

package ui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// actionRetry handles `R`. v0.6: only ran on broken (no friction).
// v0.8 (D3/CP1): non-broken triggers the y/N gate; broken still runs
// setup directly. Cross-project goes through managerForRow.
func actionRetry(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok || row.Loading {
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
		// Pin to the row's project so the dispatcher doesn't fall back
		// to "first project on host" and retry a workspace under the
		// wrong project's setup script.
		args := append([]string{row.Name}, m.remoteCwdArg(row.Host, row.Project)...)
		return m, m.execRemoteVerb(row.Host, "retry", args, force)
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

// retryDoneMsg is the RetrySetup counterpart to createDoneMsg/removeDoneMsg.
// Same shape; kept distinct so the success-message branch in
// renderBusyView (and any future post-retry behavior, e.g. auto-attach
// on success) can pivot on type rather than spelunking the busyOp field.
type retryDoneMsg struct {
	output string
	err    error
}

// retryStartedMsg is the lazy-spawn bridge for the retry flow, mirroring
// createStartedMsg + removeStartedMsg. Update on receipt: dispatch
// tea.Batch(progressTickCmd, waitRetryDoneCmd) so scripts.setup output
// streams live just like Create.
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

// waitRetryDoneCmd blocks on the retry done-chan and emits the
// retryDoneMsg when scripts.setup returns.
func waitRetryDoneCmd(done <-chan retryDoneMsg) tea.Cmd {
	return func() tea.Msg { return <-done }
}
