package ui

import (
	"context"
	"errors"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

// TestActionKill_OpensModalForMainRow: K on a live main row opens
// the kill modal. K is a session-lifecycle operation; main rows have
// real tmux sessions to kill, and EnsureMainSession idempotently
// rebuilds them on next Enter. The original implementation refused
// main-row K out of caution; relaxed 2026-04-29 once the `is-a-
// workspace for lifecycle / is-not for identity` distinction was
// made explicit.
//
// Compare to TestActionKill_RefusesDeadRow: dead rows (Alive=false)
// stay refused regardless of IsMain — nothing to kill.
func TestActionKill_OpensModalForMainRow(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{IsMain: true, Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "(main)", TmuxSession: "test-project-main", Alive: true},
	})

	_, _ = actionKill(m, tea.KeyMsg{})
	if m.mode != confirmKillMode {
		t.Errorf("mode = %v; want confirmKillMode (live main row should open the kill modal)", m.mode)
	}
	if m.killTarget != "(main)" {
		t.Errorf("killTarget = %q; want %q", m.killTarget, "(main)")
	}
	if m.err != nil {
		t.Errorf("unexpected err for main-row kill: %v", m.err)
	}
}

// TestActionKill_RefusesDeadMainRow: a main row with no live tmux
// session (Alive=false) has nothing to kill. Same refusal shape as
// for dead workspace rows.
func TestActionKill_RefusesDeadMainRow(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{IsMain: true, Project: "test-project", Name: "(main)", TmuxSession: "test-project-main", Alive: false},
	})

	_, _ = actionKill(m, tea.KeyMsg{})
	if m.mode == confirmKillMode {
		t.Error("dead main row should not open the kill modal")
	}
	if m.err == nil {
		t.Error("expected err for dead main-row kill")
	}
}

// TestActionKill_RefusesDeadRow: K on a row whose tmux session isn't
// alive (status: stopped/broken/orphaned) is a no-op with a hint —
// nothing to kill.
func TestActionKill_RefusesDeadRow(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{ProjectRoot: "/tmp/test-project", Project: "test-project", Name: "ws", Status: state.StatusStopped, Alive: false, TmuxSession: "test-project-ws"},
	})

	_, _ = actionKill(m, tea.KeyMsg{})
	if m.mode == confirmKillMode {
		t.Error("dead row should not open the kill modal")
	}
	if m.err == nil {
		t.Error("expected err to be surfaced for dead-row kill")
	}
}

// TestActionKill_OpensModal: K on a live workspace row enters
// confirmKillMode with the right target.
func TestActionKill_OpensModal(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{ProjectRoot: "/tmp/test-project", Project: "test-project", Name: "ws", Status: state.StatusReady, Alive: true, TmuxSession: "test-project-ws"},
	})

	_, _ = actionKill(m, tea.KeyMsg{})
	if m.mode != confirmKillMode {
		t.Errorf("mode = %v; want confirmKillMode", m.mode)
	}
	if m.killTarget != "ws" {
		t.Errorf("killTarget = %q; want %q", m.killTarget, "ws")
	}
	if m.killTargetRoot != "/tmp/test-project" {
		t.Errorf("killTargetRoot = %q; want %q", m.killTargetRoot, "/tmp/test-project")
	}
}

// TestHandleConfirmKillKey_CancelsOnN: any key other than y/Y cancels
// the modal and returns to listMode.
func TestHandleConfirmKillKey_CancelsOnN(t *testing.T) {
	m := newTestModel(false)
	m.mode = confirmKillMode
	m.killTarget = "ws"
	m.killTargetRoot = "/tmp/test-project"
	m.setTestRows([]Row{
		{ProjectRoot: "/tmp/test-project", Project: "test-project", Name: "ws", TmuxSession: "test-project-ws"},
	})

	_, cmd := m.handleConfirmKillKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})

	if m.mode != listMode {
		t.Errorf("mode after cancel = %v; want listMode", m.mode)
	}
	if m.killTarget != "" {
		t.Errorf("killTarget after cancel = %q; want empty", m.killTarget)
	}
	if cmd != nil {
		t.Error("expected nil cmd on cancel")
	}
}

// TestKillCmd_TreatsErrSessionNotFoundAsSuccess: ErrSessionNotFound
// from the underlying Kill is success-equivalent — the user's intent
// is "make this session gone," and gone is gone whether we did the
// killing or someone else did first.
//
// Uses the real tmux.ErrSessionNotFound sentinel because isErrSessionNotFound
// matches via errors.Is (not string substring), as it should.
func TestKillCmd_TreatsErrSessionNotFoundAsSuccess(t *testing.T) {
	fake := &fakeTmuxKiller{err: fmt.Errorf("tmux.Kill(test-session): %w", tmux.ErrSessionNotFound)}
	cmd := killCmd(fake, "test-session", "test-ws")
	msg := cmd().(killDoneMsg)
	if msg.err != nil {
		t.Errorf("ErrSessionNotFound surfaced as error: %v; want nil (already-dead is success)", msg.err)
	}
}

// TestKillCmd_RealErrorSurfaces: any other error makes it back to the
// Update loop so the user sees what happened.
func TestKillCmd_RealErrorSurfaces(t *testing.T) {
	fake := &fakeTmuxKiller{err: errors.New("tmux server crashed")}
	cmd := killCmd(fake, "test-session", "test-ws")
	msg := cmd().(killDoneMsg)
	if msg.err == nil {
		t.Error("expected error from killCmd; got nil")
	}
}

// TestKillCmd_HappyPath: success returns a clean killDoneMsg.
func TestKillCmd_HappyPath(t *testing.T) {
	fake := &fakeTmuxKiller{}
	cmd := killCmd(fake, "test-session", "test-ws")
	msg := cmd().(killDoneMsg)
	if msg.err != nil {
		t.Errorf("happy-path Kill returned error: %v", msg.err)
	}
	if msg.name != "test-ws" {
		t.Errorf("msg.name = %q; want %q", msg.name, "test-ws")
	}
	if !fake.called {
		t.Error("expected Kill to be called")
	}
}

// fakeTmuxKiller is a stand-in tmuxKiller for testing killCmd without
// spinning up a real tmux server.
type fakeTmuxKiller struct {
	err    error
	called bool
}

func (f *fakeTmuxKiller) Kill(ctx context.Context, name string) error {
	f.called = true
	return f.err
}
