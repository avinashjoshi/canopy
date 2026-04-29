package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// newTestModel builds a minimal *Model for unit-testing keymap and render
// paths that don't actually exercise the workspace.Manager. Tests that need
// the env-var read path go through New() instead.
func newTestModel(fromGlobal bool) *Model {
	return &Model{
		mgr: &workspace.Manager{
			Tmux: tmux.WithSocket("canopy-test"),
			Cfg:  &config.Config{Project: "test-project", ProjectRoot: "/tmp/test-project"},
		},
		tc:          tmux.WithSocket("canopy-test"),
		projectName: "test-project",
		nameInput:   textinput.New(),
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

// TestRowHintsMsg_MergesIntoMatchingRow: late-arriving lifecycle hints
// merge into the matching row by name. Two-phase refresh: the project
// TUI renders rows immediately, then per-row hint loaders deliver their
// results as separate rowHintsMsg arrivals.
func TestRowHintsMsg_MergesIntoMatchingRow(t *testing.T) {
	m := newTestModel(false)
	m.rows = []Row{
		{Name: "soft-fox"},
		{Name: "ancient-hornet"},
	}
	hints := []state.Hint{{Kind: "shipped", Message: "merged"}}

	model, _ := m.Update(rowHintsMsg{name: "soft-fox", hints: hints})
	m = model.(*Model)

	if len(m.rows[0].Hints) != 1 {
		t.Errorf("hints not merged into soft-fox; got %v", m.rows[0].Hints)
	}
	if len(m.rows[1].Hints) != 0 {
		t.Errorf("ancient-hornet hints should be untouched")
	}
}

// TestRowHintsMsg_NoMatchIsSilent: a hint update for a row that no
// longer exists (concurrent rm dropped it) is a no-op, not a panic.
func TestRowHintsMsg_NoMatchIsSilent(t *testing.T) {
	m := newTestModel(false)
	m.rows = []Row{{Name: "soft-fox"}}

	model, _ := m.Update(rowHintsMsg{name: "ghost-row", hints: []state.Hint{{Kind: "shipped"}}})
	m = model.(*Model)

	if len(m.rows[0].Hints) != 0 {
		t.Errorf("unrelated hint mutated existing row")
	}
}

// TestRenderTable_HintBadgesAppearedNextToRow: the project TUI's
// renderTable picks up Hints via projectlist.RenderHintBadges so the
// surface matches the global TUI. Critical for consistency — both
// surfaces showing the same hints means the user doesn't have to
// learn two badge vocabularies.
func TestRenderTable_HintBadgesAppearedNextToRow(t *testing.T) {
	m := newTestModel(false)
	m.rows = []Row{{
		Name:   "soft-fox",
		Branch: "soft-fox",
		Status: state.StatusReady,
		Hints:  []state.Hint{{Kind: "pr_status", Message: "PR #42 merged; ready to close workspace"}},
	}}
	out := m.renderTable()
	if !strings.Contains(out, "PR merged") {
		t.Errorf("PR merged badge missing in project TUI table:\n%s", out)
	}
}

// TestRenderTable_NoHintsNoBadges: rows without hints render unchanged
// (no trailing badge text). Regression check that the badge rendering
// is purely additive.
func TestRenderTable_NoHintsNoBadges(t *testing.T) {
	m := newTestModel(false)
	m.rows = []Row{{
		Name:   "soft-fox",
		Branch: "soft-fox",
		Status: state.StatusReady,
	}}
	out := m.renderTable()
	for _, badge := range []string{"↻ rename", "✓ shipped", "PR open", "PR merged"} {
		if strings.Contains(out, badge) {
			t.Errorf("unexpected badge %q in row without hints:\n%s", badge, out)
		}
	}
}

// TestNewPicker_LetterShortcuts: each variant key dispatches directly
// to the right sub-modal. Recognition over recall — the user sees the
// letter inline with the option and presses it.
func TestNewPicker_LetterShortcuts(t *testing.T) {
	cases := []struct {
		key      string
		wantMode viewMode
	}{
		{"n", newFreshMode},
		{"f", newFreshMode}, // alias
		{"p", newPRMode},
		{"i", newIssueMode},
		{"b", newBranchMode},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			m := newTestModel(false)
			m.openNewPicker()

			model, _ := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
			m = model.(*Model)
			if m.mode != tc.wantMode {
				t.Errorf("key %q: mode = %v; want %v", tc.key, m.mode, tc.wantMode)
			}
		})
	}
}

// TestNewPicker_ArrowsThenEnter: keyboard-discovery flow — arrow
// down to the desired option, hit enter. Equivalent to pressing the
// letter directly.
func TestNewPicker_ArrowsThenEnter(t *testing.T) {
	m := newTestModel(false)
	m.openNewPicker()

	// Down twice → cursor on issue (index 2).
	for i := 0; i < 2; i++ {
		model, _ := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyDown})
		m = model.(*Model)
	}
	if m.newPickerCursor != 2 {
		t.Fatalf("cursor = %d; want 2", m.newPickerCursor)
	}

	model, _ := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != newIssueMode {
		t.Errorf("enter on cursor=2 should open newIssueMode; got %v", m.mode)
	}
}

// TestNewPicker_EscReturnsToList: esc on the picker steps back to
// listMode (one level up). q is suppressed inside the picker so the
// user can't accidentally quit canopy mid-flow.
func TestNewPicker_EscReturnsToList(t *testing.T) {
	m := newTestModel(false)
	m.openNewPicker()

	model, _ := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = model.(*Model)
	if m.mode != listMode {
		t.Errorf("esc on picker should return to listMode; got %v", m.mode)
	}
}

// TestNewPicker_QSuppressed: pressing 'q' inside the picker is a
// no-op (won't quit canopy). User has to esc back to listMode first
// to quit. Protects against fat-fingered exits in the middle of a
// flow.
func TestNewPicker_QSuppressed(t *testing.T) {
	m := newTestModel(false)
	m.openNewPicker()

	_, cmd := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		t.Errorf("q in picker should be a no-op; got cmd %T", cmd)
	}
}

// TestNewFresh_EnterCreates: in fresh sub-modal, enter submits with
// the typed name and flips to busyMode. Empty name passes through
// to namegen via Manager.Create.
func TestNewFresh_EnterCreates(t *testing.T) {
	m := newTestModel(false)
	m.openNewFresh()
	m.nameInput.SetValue("fresh-one")

	model, cmd := m.handleNewFreshKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode; got %v", m.mode)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
	if !strings.Contains(m.busyTitle, "fresh-one") {
		t.Errorf("busy title should mention name; got %q", m.busyTitle)
	}
}

// TestNewFresh_EscReturnsToPicker: esc in fresh sub-modal goes back
// one step (to the picker, not all the way to listMode).
func TestNewFresh_EscReturnsToPicker(t *testing.T) {
	m := newTestModel(false)
	m.openNewFresh()

	model, _ := m.handleNewFreshKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = model.(*Model)
	if m.mode != newPickerMode {
		t.Errorf("esc on fresh sub-modal should return to picker; got %v", m.mode)
	}
}

// TestNewSubMode_EscReturnsToPicker: from any of the to-be-built
// sub-modals (PR / Issue / Branch placeholders), esc returns to the
// picker. Locks in the back-one-step contract before the live
// pickers land.
func TestNewSubMode_EscReturnsToPicker(t *testing.T) {
	for _, mode := range []viewMode{newPRMode, newIssueMode, newBranchMode} {
		m := newTestModel(false)
		m.mode = mode
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if m.mode != newPickerMode {
			t.Errorf("esc from %v should return to newPickerMode; got %v", mode, m.mode)
		}
	}
}

// errFakeCreate is a sentinel for the create-error test. Lives here
// (not in the test func) so the literal stays readable.
var errFakeCreate = fakeErr("setup failed")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
