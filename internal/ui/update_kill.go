// Kill flow (K keybind). Kills a workspace's tmux session without
// removing state — state.json + worktree + branch all survive; status
// flips to stopped; re-attach via Enter resurrects via Manager.Resurrect.
// Carved out of update.go.

package ui

import (
	"context"
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/tmux"
)

// actionKill handles `K`. Opens the y/N confirm modal scoped to the
// cursor row. Stopped/broken/orphaned rows (any row with Alive=false)
// are no-ops — nothing to kill — surfaced as a status-line hint rather
// than silently doing nothing.
//
// K is less destructive than d: state.json + worktree + branch all
// survive; status flips to stopped; re-attach via Enter resurrects.
// Cancel-by-default is still the safe posture in the confirm modal.
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

// isErrSessionNotFound checks whether err's chain includes the tmux
// "session not found" sentinel via errors.Is. The ui package already
// imports internal/tmux for *tmux.Client, so there's no cycle risk —
// the sentinel match is the right tool here, not string matching
// (which would silently break if tmux ever rephrased the error or
// internationalized it).
func isErrSessionNotFound(err error) bool {
	return errors.Is(err, tmux.ErrSessionNotFound)
}
