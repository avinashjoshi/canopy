package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// TestHandleAgentSwapDone_SuccessRendersAndUnbusy: a clean swap clears
// busy state and stages a success message that mentions the new agent.
func TestHandleAgentSwapDone_SuccessRendersAndUnbusy(t *testing.T) {
	m := &Model{mode: agentSwapPickerMode, agentSwapBusy: true}
	_, _ = m.handleAgentSwapDone(agentSwapDoneMsg{newAgent: "codex", err: nil})
	if m.agentSwapBusy {
		t.Error("agentSwapBusy still true after success")
	}
	if !strings.Contains(m.agentSwapResult, "codex") {
		t.Errorf("agentSwapResult = %q; want it to mention 'codex'", m.agentSwapResult)
	}
}

// TestHandleAgentSwapDone_SentinelErrors maps each known sentinel to a
// friendly message prefix. Catches the case where a new sentinel gets
// added in the workspace package without a corresponding UI branch
// here — the default branch would render the raw error chain.
func TestHandleAgentSwapDone_SentinelErrors(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantSub   string
	}{
		{
			name:    "ErrAgentNotAllowed",
			err:     agent.ErrAgentNotAllowed,
			wantSub: "not in this project's agents allowlist",
		},
		{
			name:    "ErrSwapAlreadyCurrent",
			err:     workspace.ErrSwapAlreadyCurrent,
			wantSub: "Already running",
		},
		{
			name:    "ErrSwapNoAgentPane",
			err:     workspace.ErrSwapNoAgentPane,
			wantSub: "No agent pane",
		},
		{
			name:    "generic error",
			err:     errors.New("kaboom"),
			wantSub: "Swap failed:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{mode: agentSwapPickerMode, agentSwapBusy: true}
			_, _ = m.handleAgentSwapDone(agentSwapDoneMsg{newAgent: "codex", err: tc.err})
			if m.agentSwapBusy {
				t.Error("agentSwapBusy still true after err")
			}
			if !strings.Contains(m.agentSwapResult, tc.wantSub) {
				t.Errorf("agentSwapResult = %q; want substring %q", m.agentSwapResult, tc.wantSub)
			}
		})
	}
}

// TestHandleAgentSwapPickerKey_NavCursor: up/down/j/k move the cursor
// within bounds; out-of-bounds keys are no-ops. Picks the simplest
// invariant: cursor never goes negative, never exceeds list length-1.
func TestHandleAgentSwapPickerKey_NavCursor(t *testing.T) {
	m := &Model{
		mode:            agentSwapPickerMode,
		agentSwapList:   []string{"claude", "codex", "aider"},
		agentSwapCursor: 0,
	}

	// up at top is a no-op
	_, _ = m.handleAgentSwapPickerKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.agentSwapCursor != 0 {
		t.Errorf("up at top moved cursor to %d; want 0", m.agentSwapCursor)
	}

	// down twice
	_, _ = m.handleAgentSwapPickerKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleAgentSwapPickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.agentSwapCursor != 2 {
		t.Errorf("down x2 cursor = %d; want 2", m.agentSwapCursor)
	}

	// down at bottom is a no-op
	_, _ = m.handleAgentSwapPickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.agentSwapCursor != 2 {
		t.Errorf("down at bottom moved cursor to %d; want 2", m.agentSwapCursor)
	}

	// up returns toward top
	_, _ = m.handleAgentSwapPickerKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.agentSwapCursor != 1 {
		t.Errorf("up cursor = %d; want 1", m.agentSwapCursor)
	}
}

// TestHandleAgentSwapPickerKey_EscCancels: pressing esc returns to
// listMode and clears the snapshotted state so the next open starts
// fresh.
func TestHandleAgentSwapPickerKey_EscCancels(t *testing.T) {
	m := &Model{
		mode:                agentSwapPickerMode,
		agentSwapTarget:     "foo",
		agentSwapTargetRoot: "/tmp/proj",
		agentSwapList:       []string{"claude", "codex"},
		agentSwapCursor:     1,
	}
	_, _ = m.handleAgentSwapPickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != listMode {
		t.Errorf("mode = %v; want listMode", m.mode)
	}
	if m.agentSwapTarget != "" || m.agentSwapTargetRoot != "" || m.agentSwapList != nil {
		t.Errorf("agentSwap state not cleared after esc: target=%q root=%q list=%v",
			m.agentSwapTarget, m.agentSwapTargetRoot, m.agentSwapList)
	}
}

// TestHandleAgentSwapPickerKey_EnterOnSameAgentDoesntDispatch: pressing
// Enter when the highlighted agent IS the current agent surfaces an
// "already running" message inline WITHOUT dispatching SwapAgent (which
// would be a wasted call returning ErrSwapAlreadyCurrent). Catches the
// optimization wire.
func TestHandleAgentSwapPickerKey_EnterOnSameAgentDoesntDispatch(t *testing.T) {
	m := &Model{
		mode:             agentSwapPickerMode,
		agentSwapList:    []string{"claude", "codex"},
		agentSwapCursor:  0, // claude
		agentSwapCurrent: "claude",
	}
	_, cmd := m.handleAgentSwapPickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Errorf("Enter on same-agent dispatched a cmd; want nil (inline short-circuit)")
	}
	if !strings.Contains(m.agentSwapResult, "Already running") {
		t.Errorf("agentSwapResult = %q; want 'Already running' message", m.agentSwapResult)
	}
}

// TestHandleAgentSwapPickerKey_BusyIgnoresKeys: while a swap is in
// flight, almost all keypresses are swallowed (the swap can't be
// safely cancelled mid-flight without leaving tmux in a half-swapped
// state). Only ctrl+c (quit canopy entirely) is allowed through.
func TestHandleAgentSwapPickerKey_BusyIgnoresKeys(t *testing.T) {
	m := &Model{mode: agentSwapPickerMode, agentSwapBusy: true, agentSwapList: []string{"claude"}}
	// Esc, j, k, enter all no-op
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyEsc},
		{Type: tea.KeyDown},
		{Type: tea.KeyUp},
		{Type: tea.KeyEnter},
	} {
		_, _ = m.handleAgentSwapPickerKey(key)
	}
	if m.mode != agentSwapPickerMode {
		t.Errorf("mode = %v after busy keypresses; want agentSwapPickerMode (no exit)", m.mode)
	}
	// ctrl+c still works (returns tea.Quit)
	_, cmd := m.handleAgentSwapPickerKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Error("ctrl+c during busy returned nil cmd; want tea.Quit")
	}
}

// TestHandleAgentSwapPickerKey_ResultDismissedByAnyKey: after a swap
// completes (success or error), the picker enters "result shown" mode
// where any key dismisses back to listMode. Tests the dismiss-on-any-key
// contract specifically.
func TestHandleAgentSwapPickerKey_ResultDismissedByAnyKey(t *testing.T) {
	m := &Model{
		mode:            agentSwapPickerMode,
		agentSwapResult: "Swapped to codex.",
		agentSwapList:   []string{"claude", "codex"},
		agentSwapCursor: 1,
	}
	_, _ = m.handleAgentSwapPickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if m.mode != listMode {
		t.Errorf("mode after dismiss = %v; want listMode", m.mode)
	}
	if m.agentSwapResult != "" {
		t.Errorf("agentSwapResult not cleared after dismiss: %q", m.agentSwapResult)
	}
}

// TestRenderAgentSwapPicker_HappyPath: render the picker's three
// states and assert each one has the expected scaffolding lines. We
// don't pin exact glyphs / colors (too brittle); we check for the
// distinguishing text.
func TestRenderAgentSwapPicker_HappyPath(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(*Model)
		wantSubs  []string
	}{
		{
			name: "picker state lists agents + cursor + current marker",
			setup: func(m *Model) {
				m.agentSwapTarget = "feature-x"
				m.agentSwapList = []string{"claude", "codex"}
				m.agentSwapCursor = 1
				m.agentSwapCurrent = "claude"
			},
			wantSubs: []string{"feature-x", "claude", "codex", "(current)", "swap", "cancel"},
		},
		{
			name: "busy state shows swapping message",
			setup: func(m *Model) {
				m.agentSwapTarget = "feature-x"
				m.agentSwapList = []string{"claude", "codex"}
				m.agentSwapBusy = true
			},
			wantSubs: []string{"feature-x", "Swapping"},
		},
		{
			name: "result state shows message + dismiss hint",
			setup: func(m *Model) {
				m.agentSwapTarget = "feature-x"
				m.agentSwapList = []string{"claude", "codex"}
				m.agentSwapResult = "Swapped to codex. Press any key."
			},
			wantSubs: []string{"Swapped to codex", "Press any key"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{mode: agentSwapPickerMode}
			tc.setup(m)
			got := m.renderAgentSwapPicker()
			for _, want := range tc.wantSubs {
				if !strings.Contains(got, want) {
					t.Errorf("renderAgentSwapPicker output missing %q\nfull output:\n%s", want, got)
				}
			}
		})
	}
}
