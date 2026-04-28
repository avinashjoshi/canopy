package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// newTestModel builds a minimal *Model for unit-testing keymap and render
// paths that don't actually exercise the workspace.Manager. Tests that need
// the env-var read path go through New() instead.
func newTestModel(fromGlobal bool) *Model {
	return &Model{
		mgr:         &workspace.Manager{Tmux: tmux.WithSocket("canopy-test")},
		tc:          tmux.WithSocket("canopy-test"),
		projectName: "test-project",
		mode:        listMode,
		fromGlobal:  fromGlobal,
	}
}

// TestNew_ReadsFromGlobalEnv: New() picks up CANOPY_FROM_GLOBAL=1 from the
// environment and stores it on the Model. Verifies the env-var handshake
// that lets the inner project TUI know it was launched from the global
// view (model_global.go:goToProject sets it).
func TestNew_ReadsFromGlobalEnv(t *testing.T) {
	mgr := &workspace.Manager{
		Cfg:  &config.Config{Project: "test-project"},
		Tmux: tmux.WithSocket("canopy-test"),
	}

	t.Run("env unset → fromGlobal=false", func(t *testing.T) {
		t.Setenv("CANOPY_FROM_GLOBAL", "")
		m := New(mgr)
		if m.fromGlobal {
			t.Errorf("fromGlobal = true; want false when env unset")
		}
	})

	t.Run("env=1 → fromGlobal=true", func(t *testing.T) {
		t.Setenv("CANOPY_FROM_GLOBAL", "1")
		m := New(mgr)
		if !m.fromGlobal {
			t.Errorf("fromGlobal = false; want true when CANOPY_FROM_GLOBAL=1")
		}
	})

	t.Run("env=other → fromGlobal=false", func(t *testing.T) {
		// Strict equality with "1" — anything else (including "true",
		// "yes") doesn't count. Keeps the contract narrow.
		t.Setenv("CANOPY_FROM_GLOBAL", "true")
		m := New(mgr)
		if m.fromGlobal {
			t.Errorf("fromGlobal = true; want false when env != \"1\"")
		}
	})
}

// TestHandleKey_BackToGlobal: 'b' and 'esc' quit the project TUI when
// fromGlobal=true (the global TUI re-renders after ExecProcess returns).
// When fromGlobal=false, both keys are no-ops — outside the global-launch
// flow we don't want to surprise users by quitting on esc.
func TestHandleKey_BackToGlobal(t *testing.T) {
	cases := []struct {
		name       string
		fromGlobal bool
		key        string
		wantQuit   bool
	}{
		{"b from global quits", true, "b", true},
		{"esc from global quits", true, "esc", true},
		{"b standalone no-ops", false, "b", false},
		{"esc standalone no-ops", false, "esc", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(tc.fromGlobal)

			var msg tea.KeyMsg
			if tc.key == "esc" {
				msg = tea.KeyMsg{Type: tea.KeyEsc}
			} else {
				msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)}
			}

			_, cmd := m.handleKey(msg)

			if tc.wantQuit {
				if cmd == nil {
					t.Fatal("expected tea.Quit cmd; got nil")
				}
				if _, ok := cmd().(tea.QuitMsg); !ok {
					t.Errorf("cmd produced %T; want tea.QuitMsg", cmd())
				}
			} else if cmd != nil {
				// Must be a no-op when not from global.
				if _, ok := cmd().(tea.QuitMsg); ok {
					t.Errorf("standalone %s should not quit", tc.key)
				}
			}
		})
	}
}

// TestUpdate_CreateDoneAutoAttaches: a successful createDoneMsg with a
// non-empty tmuxSession resets busy state and dispatches an attachCmd —
// the user pressed 'n' to create + use a workspace, so we drop them
// straight into the session instead of a "press any key to dismiss"
// gate.
func TestUpdate_CreateDoneAutoAttaches(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate
	m.busyTitle = "Creating workspace \"foo\"..."

	msg := createDoneMsg{
		output:      "setup ran fine\n",
		tmuxSession: "test-project-foo",
		err:         nil,
	}
	next, cmd := m.Update(msg)
	gotModel := next.(*Model)

	if gotModel.mode != listMode {
		t.Errorf("mode = %v; want listMode after auto-attach", gotModel.mode)
	}
	if gotModel.busyOp != busyOpNone {
		t.Errorf("busyOp = %v; want busyOpNone", gotModel.busyOp)
	}
	if gotModel.busyTitle != "" {
		t.Errorf("busyTitle = %q; want empty", gotModel.busyTitle)
	}
	if cmd == nil {
		t.Fatal("expected attach cmd; got nil")
	}
	// Don't actually invoke cmd() — it would try to exec tmux. The
	// non-nil return is the signal that the dispatch happened.
}

// TestUpdate_CreateDoneOnErrorStaysInBusy: a failed createDoneMsg keeps
// busyMode active so the user can read the captured setup output. The
// existing handleBusyModeKey dismisses on any key press.
func TestUpdate_CreateDoneOnErrorStaysInBusy(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate

	msg := createDoneMsg{
		output:      "boom\n",
		tmuxSession: "",
		err:         errFakeCreate,
	}
	next, cmd := m.Update(msg)
	gotModel := next.(*Model)

	if gotModel.mode != busyMode {
		t.Errorf("mode = %v; want busyMode (so user sees error)", gotModel.mode)
	}
	if !gotModel.busyDone {
		t.Errorf("busyDone = false; want true so any key dismisses")
	}
	if gotModel.busyErr == nil {
		t.Errorf("busyErr = nil; want the create error")
	}
	if cmd != nil {
		t.Errorf("expected nil cmd on create error; got %v", cmd)
	}
}

// TestRenderHelpLine_BackKeyConditional: the bottom help line shows
// "b back" only when fromGlobal=true. Keeps the standalone canopy
// invocation uncluttered.
func TestRenderHelpLine_BackKeyConditional(t *testing.T) {
	t.Run("from global shows b back", func(t *testing.T) {
		m := newTestModel(true)
		out := m.renderHelpLine()
		if !strings.Contains(out, "b back") {
			t.Errorf("help line missing 'b back' when fromGlobal=true: %q", out)
		}
	})

	t.Run("standalone hides b back", func(t *testing.T) {
		m := newTestModel(false)
		out := m.renderHelpLine()
		if strings.Contains(out, "b back") {
			t.Errorf("help line should not mention 'b back' standalone: %q", out)
		}
	})
}

// TestRenderHelp_BackKeyConditional: the full ? overlay surfaces the
// "b, esc" back-to-global line only when fromGlobal=true. Same gating
// as the bottom help line.
func TestRenderHelp_BackKeyConditional(t *testing.T) {
	t.Run("from global lists back key", func(t *testing.T) {
		m := newTestModel(true)
		out := m.renderHelp()
		if !strings.Contains(out, "back to canopy global") {
			t.Errorf("help overlay missing back-key line when fromGlobal=true: %q", out)
		}
	})

	t.Run("standalone hides back key", func(t *testing.T) {
		m := newTestModel(false)
		out := m.renderHelp()
		if strings.Contains(out, "back to canopy global") {
			t.Errorf("help overlay should not mention back key standalone: %q", out)
		}
	})
}

// errFakeCreate is a sentinel for the create-error test. Lives here
// (not in the test func) so the literal stays readable.
var errFakeCreate = fakeErr("setup failed")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
