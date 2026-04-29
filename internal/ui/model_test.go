package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/oncactus/canopy/internal/config"
	"github.com/oncactus/canopy/internal/ghx"
	"github.com/oncactus/canopy/internal/state"
	"github.com/oncactus/canopy/internal/tmux"
	"github.com/oncactus/canopy/internal/workspace"
)

// newTestModel builds a minimal *Model for unit-testing keymap and render
// paths that don't actually exercise the workspace.Manager. Used widely
// across this file; the bool param is unused after v0.8 unification but
// kept for callsite stability during the merge transition.
func newTestModel(_ bool) *Model {
	return &Model{
		mgr: &workspace.Manager{
			Tmux: tmux.WithSocket("canopy-test"),
			Cfg:  &config.Config{Project: "test-project", ProjectRoot: "/tmp/test-project"},
		},
		tc:             tmux.WithSocket("canopy-test"),
		projectName:    "test-project",
		nameInput:      textinput.New(),
		listInput:      textinput.New(),
		mode:           listMode,
		currentProject: "/tmp/test-project",
		tab:            tabLocal,
	}
}

// TestNewUnified_PopupModeFromEnv: NewUnified picks up CANOPY_IN_POPUP=1
// from the environment and stores it as m.inPopup. This is the single
// source of truth for popup-mode rendering after v0.8 unification —
// the env var is set inline by tmux's `display-popup -E` invocation.
func TestNewUnified_PopupModeFromEnv(t *testing.T) {
	store := &state.Store{}
	tc := tmux.WithSocket("canopy-test")

	t.Run("env=1 → popup mode", func(t *testing.T) {
		t.Setenv("CANOPY_IN_POPUP", "1")
		m := NewUnified(nil, store, tc, "")
		if !m.inPopup {
			t.Errorf("inPopup = false; want true when CANOPY_IN_POPUP=1")
		}
	})

	t.Run("env unset → fullscreen mode", func(t *testing.T) {
		t.Setenv("CANOPY_IN_POPUP", "")
		m := NewUnified(nil, store, tc, "")
		if m.inPopup {
			t.Errorf("inPopup = true; want false when env unset")
		}
	})

	t.Run("env=other → fullscreen mode (strict eq)", func(t *testing.T) {
		t.Setenv("CANOPY_IN_POPUP", "true")
		m := NewUnified(nil, store, tc, "")
		if m.inPopup {
			t.Errorf("inPopup = true; want false when env != \"1\"")
		}
	})
}

// TestNewUnified_DefaultTab: Local tab is pre-selected when a current
// project is resolved; Global tab pre-selected when not. Reflects the
// "scope is what I'm working on" / "give me everything" intuition from
// the unification design.
func TestNewUnified_DefaultTab(t *testing.T) {
	store := &state.Store{}
	tc := tmux.WithSocket("canopy-test")

	t.Run("with current project → tabLocal", func(t *testing.T) {
		m := NewUnified(nil, store, tc, "/some/project")
		if m.tab != tabLocal {
			t.Errorf("tab = %v; want tabLocal when currentProject != \"\"", m.tab)
		}
	})

	t.Run("no current project → tabGlobal", func(t *testing.T) {
		m := NewUnified(nil, store, tc, "")
		if m.tab != tabGlobal {
			t.Errorf("tab = %v; want tabGlobal when currentProject == \"\"", m.tab)
		}
	})
}

// TestHandleKey_TabSwitch: tab key flips m.tab between Local and Global.
// Resets cursor to 0 so a long-list scroll position doesn't carry over
// into a different tab confusingly.
func TestHandleKey_TabSwitch(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabLocal
	m.cursor = 5

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got := model.(*Model)
	if got.tab != tabGlobal {
		t.Errorf("after tab key: tab = %v; want tabGlobal", got.tab)
	}
	if got.cursor != 0 {
		t.Errorf("after tab key: cursor = %d; want 0 (reset on tab)", got.cursor)
	}

	model, _ = got.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got = model.(*Model)
	if got.tab != tabLocal {
		t.Errorf("after second tab key: tab = %v; want tabLocal (round-trip)", got.tab)
	}
}

// TestHandleKey_NDisabledOnGlobalTab: `n` (new workspace) is hidden +
// no-op when the user is on the Global tab — n requires a current-project
// Manager because it walks up canopy.json from cwd. The asymmetry with
// d/R (which work cross-project) is documented in the unification plan.
//
// E1 follow-on: with the bindings-table refactor, an unavailable binding
// doesn't fire its Action AT ALL — no err set, no mode change, completely
// silent. This is cleaner than the v0.7 "set m.err with a hint" approach
// because the help line already hides the key, so a user who types `n`
// on the Global tab sees nothing happen, which matches the visual cue.
func TestHandleKey_NDisabledOnGlobalTab(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.mgr = nil // Global mode: no current-project Manager
	m.allRows = []Row{
		{Project: "other", ProjectRoot: "/some/other", Name: "ws-1", Status: state.StatusReady},
	}

	model, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	got := model.(*Model)
	if got.mode != listMode {
		t.Errorf("after n on Global tab: mode = %v; want listMode (n must no-op)", got.mode)
	}
	if cmd != nil {
		t.Errorf("after n on Global tab: cmd != nil; want nil (no-op)")
	}
	// Bindings table filters by Available before dispatch, so no err
	// is set on disabled-key press. Visual cue (n missing from help)
	// is the user-facing signal.
}

// TestAvailableNewWorkspace exercises the binding's Available predicate
// directly. Both the help-line filter AND the dispatch gate read from
// this; one source of truth for "is n usable right now."
func TestAvailableNewWorkspace(t *testing.T) {
	cases := []struct {
		name string
		mgr  bool
		tab  tabKind
		want bool
	}{
		{"mgr + Local → enabled", true, tabLocal, true},
		{"mgr + Global → disabled (cross-project n is meaningless)", true, tabGlobal, false},
		{"no mgr + Local → disabled (no canopy.json context)", false, tabLocal, false},
		{"no mgr + Global → disabled", false, tabGlobal, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(false)
			if !tc.mgr {
				m.mgr = nil
			}
			m.tab = tc.tab
			got := availableNewWorkspace(m)
			if got != tc.want {
				t.Errorf("availableNewWorkspace(mgr=%v, tab=%v) = %v; want %v",
					tc.mgr, tc.tab, got, tc.want)
			}
		})
	}
}

// TestHandleKey_SearchEntry: pressing "/" enters search mode and
// initializes searchQuery. Any subsequent key goes through
// handleSearchKey via the search-mode bypass in handleKey.
func TestHandleKey_SearchEntry(t *testing.T) {
	m := newTestModel(false)
	if m.searchMode {
		t.Fatal("setup: searchMode should start false")
	}

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	got := model.(*Model)
	if !got.searchMode {
		t.Errorf("after / key: searchMode = false; want true")
	}
	if got.searchQuery != "" {
		t.Errorf("after / key: searchQuery = %q; want empty", got.searchQuery)
	}
}

// TestFilteredRows_TabFilter: tabLocal scopes rows to currentProject;
// tabGlobal returns everything. Empty-current-project Local tab returns
// empty (the "outside any project" case shows onboarding text).
func TestFilteredRows_TabFilter(t *testing.T) {
	m := newTestModel(false)
	m.currentProject = "/p/foo"
	m.allRows = []Row{
		{Project: "foo", ProjectRoot: "/p/foo", Name: "ws-a"},
		{Project: "foo", ProjectRoot: "/p/foo", Name: "ws-b"},
		{Project: "bar", ProjectRoot: "/p/bar", Name: "ws-c"},
	}

	m.tab = tabLocal
	got := m.filteredRows()
	if len(got) != 2 {
		t.Errorf("Local tab: got %d rows; want 2 (foo only)", len(got))
	}
	for _, r := range got {
		if r.ProjectRoot != "/p/foo" {
			t.Errorf("Local tab leaked cross-project row: %+v", r)
		}
	}

	m.tab = tabGlobal
	got = m.filteredRows()
	if len(got) != 3 {
		t.Errorf("Global tab: got %d rows; want 3 (all)", len(got))
	}
}

// TestFilteredRows_SearchFilter: searchQuery matches name OR project OR
// branch via subsequence (fzf-style). Empty query returns all rows.
func TestFilteredRows_SearchFilter(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.allRows = []Row{
		{Project: "foo", ProjectRoot: "/p/foo", Name: "silent-falcon"},
		{Project: "foo", ProjectRoot: "/p/foo", Name: "misty-aspen"},
		{Project: "bar", ProjectRoot: "/p/bar", Name: "bold-ox", Branch: "feat/falcon"},
	}

	m.searchQuery = "fal"
	got := m.filteredRows()
	if len(got) != 2 {
		t.Errorf("search 'fal': got %d; want 2 (silent-falcon name + bold-ox branch)", len(got))
	}

	m.searchQuery = "bar"
	got = m.filteredRows()
	if len(got) != 1 || got[0].Name != "bold-ox" {
		t.Errorf("search 'bar': got %v; want [bold-ox] (project match)", got)
	}

	m.searchQuery = ""
	got = m.filteredRows()
	if len(got) != 3 {
		t.Errorf("empty search: got %d; want 3 (all)", len(got))
	}
}

// TestRetryConfirmModal_NonBrokenTriggers: pressing R on a non-broken
// workspace opens the confirmRetry y/N gate (D3/CP1) instead of
// erroring. Mirrors the CLI's --force friction in TUI form.
func TestRetryConfirmModal_NonBrokenTriggers(t *testing.T) {
	m := newTestModel(false)
	m.allRows = []Row{
		{Project: "test-project", ProjectRoot: "/tmp/test-project",
			Name: "healthy-ws", Status: state.StatusReady},
	}

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	got := model.(*Model)
	if got.mode != confirmRetryMode {
		t.Errorf("after R on healthy ws: mode = %v; want confirmRetryMode", got.mode)
	}
	if got.retryTarget != "healthy-ws" {
		t.Errorf("retryTarget = %q; want healthy-ws", got.retryTarget)
	}
}

// TestRetryConfirmModal_CancelOnN: pressing n in confirmRetryMode cancels
// back to listMode without dispatching a retry.
func TestRetryConfirmModal_CancelOnN(t *testing.T) {
	m := newTestModel(false)
	m.mode = confirmRetryMode
	m.retryTarget = "healthy-ws"

	model, cmd := m.handleConfirmRetryKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	got := model.(*Model)
	if got.mode != listMode {
		t.Errorf("after n: mode = %v; want listMode", got.mode)
	}
	if got.retryTarget != "" {
		t.Errorf("after n: retryTarget = %q; want empty", got.retryTarget)
	}
	if cmd != nil {
		t.Errorf("after n: cmd != nil; want nil (no retry dispatched)")
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

// TestRenderHelpLine_TabSwitch: help line shows `tab switch-tab` and
// `/ search` always. `n new` shows only on Local tab with non-nil mgr.
func TestRenderHelpLine_TabSwitch(t *testing.T) {
	t.Run("Local tab with mgr → n shown", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabLocal
		out := m.renderHelpLine()
		if !strings.Contains(out, "n new") {
			t.Errorf("Local tab help missing 'n new': %q", out)
		}
		if !strings.Contains(out, "tab switch-tab") {
			t.Errorf("help line missing 'tab switch-tab': %q", out)
		}
	})

	t.Run("Global tab → n hidden", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabGlobal
		out := m.renderHelpLine()
		if strings.Contains(out, "n new") {
			t.Errorf("Global tab help should not show 'n new': %q", out)
		}
	})

	t.Run("nil mgr → n hidden even on Local tab", func(t *testing.T) {
		m := newTestModel(false)
		m.mgr = nil
		m.tab = tabLocal
		out := m.renderHelpLine()
		if strings.Contains(out, "n new") {
			t.Errorf("nil mgr help should not show 'n new': %q", out)
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

// TestNewIssue_TypedNumberSubmits: same fast path as PR — typed
// number → submit, no list-load wait.
func TestNewIssue_TypedNumberSubmits(t *testing.T) {
	m := newTestModel(false)
	m.mode = newIssueMode
	m.listInput.SetValue("42")

	model, cmd := m.handleNewIssueKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode; got %v", m.mode)
	}
	if !strings.Contains(m.busyTitle, "issue #42") {
		t.Errorf("busy title should mention issue #42; got %q", m.busyTitle)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
}

// TestNewIssue_ArrowsThenEnter: recognition path — load fixture
// issues, arrow to row, enter.
func TestNewIssue_ArrowsThenEnter(t *testing.T) {
	m := newTestModel(false)
	m.mode = newIssueMode
	m.newIssues = []ghx.IssueSummary{
		{Number: 17, Title: "Add feature"},
		{Number: 18, Title: "Fix bug"},
	}

	model, _ := m.handleNewIssueKey(tea.KeyMsg{Type: tea.KeyDown})
	m = model.(*Model)
	model, _ = m.handleNewIssueKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if !strings.Contains(m.busyTitle, "issue #18") {
		t.Errorf("expected #18 in busy title; got %q", m.busyTitle)
	}
}

// TestNewBranch_FilterAndPick: load remote+local branches, filter
// down to one, hit enter. The picked branch goes into a SourceSpec.
func TestNewBranch_FilterAndPick(t *testing.T) {
	m := newTestModel(false)
	m.mode = newBranchMode
	m.newBranches = []string{
		"main",
		"origin/main",
		"origin/feat/oauth",
		"origin/feat/billing",
	}
	m.listInput.SetValue("oauth")

	model, cmd := m.handleNewBranchKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode; got %v", m.mode)
	}
	if !strings.Contains(m.busyTitle, "feat/oauth") {
		t.Errorf("busy title should mention feat/oauth; got %q", m.busyTitle)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
}

// TestNewBranch_LocalOnlyFlipsAllowLocal: a branch that exists only
// locally (no origin/<name>) submits with AllowLocal=true so the
// resolver doesn't reject it. Required for the workflow where the
// user has a local-only branch from before they pushed it.
func TestNewBranch_LocalOnlyFlipsAllowLocal(t *testing.T) {
	m := newTestModel(false)
	m.mode = newBranchMode
	m.newBranches = []string{
		"main",
		"origin/main",
		"local-experiment", // local only
	}
	m.listInput.SetValue("local-experiment")

	// Capture the SourceSpec via spying on submitNewBranch isn't
	// straightforward without a mock; instead, verify the flow
	// reaches busyMode. The AllowLocal logic is small enough to
	// test directly via branchHasOrigin (separate test below).
	model, _ := m.handleNewBranchKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode; got %v", m.mode)
	}
}

// TestBranchHasOrigin: helper that decides AllowLocal.
func TestBranchHasOrigin(t *testing.T) {
	branches := []string{"main", "origin/main", "feat/x", "origin/feat/x"}
	if !branchHasOrigin(branches, "main") {
		t.Errorf("main has origin counterpart; should return true")
	}
	if !branchHasOrigin(branches, "feat/x") {
		t.Errorf("feat/x has origin counterpart; should return true")
	}
	if branchHasOrigin(branches, "local-only") {
		t.Errorf("local-only has no origin; should return false")
	}
}

// TestPickerWindow_FitsInVisible: when the list is shorter than
// the visible window, return the full range (no scroll).
func TestPickerWindow_FitsInVisible(t *testing.T) {
	top, end := pickerWindow(3, 5, 10)
	if top != 0 || end != 5 {
		t.Errorf("expected (0,5); got (%d,%d)", top, end)
	}
}

// TestPickerWindow_CursorTopOfWindow: cursor at index 0 — window
// starts at 0, regardless of list length.
func TestPickerWindow_CursorTopOfWindow(t *testing.T) {
	top, end := pickerWindow(0, 100, 10)
	if top != 0 || end != 10 {
		t.Errorf("expected (0,10); got (%d,%d)", top, end)
	}
}

// TestPickerWindow_CursorBottomOfWindow: cursor below the visible
// height — window scrolls so cursor is the LAST visible row.
func TestPickerWindow_CursorBottomOfWindow(t *testing.T) {
	// Cursor at 50 in a 100-item list with 10 visible rows.
	// Window should be [41, 51) so cursor at 50 is the last visible.
	top, end := pickerWindow(50, 100, 10)
	if top != 41 || end != 51 {
		t.Errorf("expected (41,51); got (%d,%d)", top, end)
	}
}

// TestPickerWindow_CursorAtListEnd: cursor at the last item — window
// is the bottom slice, not past-the-end.
func TestPickerWindow_CursorAtListEnd(t *testing.T) {
	top, end := pickerWindow(99, 100, 10)
	if top != 90 || end != 100 {
		t.Errorf("expected (90,100); got (%d,%d)", top, end)
	}
}

// TestBranchInWorkspace_Match: a row whose Branch matches an
// existing workspace returns the workspace name + true.
func TestBranchInWorkspace_Match(t *testing.T) {
	m := newTestModel(false)
	m.rows = []Row{
		{IsMain: true, Name: "(main)", Branch: "—"},
		{Name: "pr-1185", Branch: "pdx91/inbox-improvements"},
		{Name: "soft-fox", Branch: "feat/oauth"},
	}
	wsName, taken := m.branchInWorkspace("feat/oauth")
	if !taken || wsName != "soft-fox" {
		t.Errorf("expected (soft-fox, true); got (%q, %v)", wsName, taken)
	}
}

// TestBranchInWorkspace_NoMatch: branch with no matching workspace
// returns false. Empty branch string also returns false (defensive).
func TestBranchInWorkspace_NoMatch(t *testing.T) {
	m := newTestModel(false)
	m.rows = []Row{{Name: "soft-fox", Branch: "feat/oauth"}}
	if _, taken := m.branchInWorkspace("other-branch"); taken {
		t.Errorf("non-matching branch should return false")
	}
	if _, taken := m.branchInWorkspace(""); taken {
		t.Errorf("empty branch should return false")
	}
}

// TestBranchInWorkspace_SkipsMain: the synthetic main row has
// Branch="—" which must not match anything (defensive against a
// hypothetical workspace literally named "—").
func TestBranchInWorkspace_SkipsMain(t *testing.T) {
	m := newTestModel(false)
	m.rows = []Row{{IsMain: true, Branch: "—"}}
	if _, taken := m.branchInWorkspace("—"); taken {
		t.Errorf("main row should be excluded from branch-conflict check")
	}
}

// TestRenderNewBranch_TagsTakenBranches: rendering the branch picker
// includes a "(in workspace X)" tag on rows whose bare branch name
// matches an existing workspace. Verifies the visual cue lands.
func TestRenderNewBranch_TagsTakenBranches(t *testing.T) {
	m := newTestModel(false)
	m.mode = newBranchMode
	m.rows = []Row{{Name: "pr-1185", Branch: "pdx91/inbox-improvements"}}
	m.newBranches = []string{
		"main",
		"origin/pdx91/inbox-improvements",
		"pdx91/inbox-improvements",
		"origin/feat/x",
	}
	out := m.renderNewBranch()
	if !strings.Contains(out, "in workspace pr-1185") {
		t.Errorf("taken-branch tag missing from picker:\n%s", out)
	}
}

// TestRenderNewPR_TagsTakenPRs: rendering the PR picker tags rows
// whose head branch is already in a workspace. PRs with HeadRefName
// matching an existing workspace's branch get the dim treatment.
func TestRenderNewPR_TagsTakenPRs(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.rows = []Row{{Name: "pr-1185", Branch: "pdx91/inbox-improvements"}}
	m.newPRs = []ghx.PRSummary{
		{Number: 1185, Title: "Inbox", HeadRefName: "pdx91/inbox-improvements"},
		{Number: 1182, Title: "Auth", HeadRefName: "feat/oauth"},
	}
	out := m.renderNewPR()
	if !strings.Contains(out, "in workspace pr-1185") {
		t.Errorf("taken-PR tag missing from picker:\n%s", out)
	}
}

// TestPickerCursor_BoundedByFilteredLength: cursor-down stops at the
// filtered length, not the raw list length. Without this bound the
// cursor could drift into invisible rows after a filter narrows the
// list.
func TestPickerCursor_BoundedByFilteredLength(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.newPRs = []ghx.PRSummary{
		{Number: 1, Title: "match"},
		{Number: 2, Title: "no"},
		{Number: 3, Title: "match also"},
	}
	m.listInput.SetValue("match") // filters to 2 rows

	// Press down twice — should land at index 1 (last filtered) and
	// stay there on subsequent presses.
	for i := 0; i < 5; i++ {
		model, _ := m.handleNewPRKey(tea.KeyMsg{Type: tea.KeyDown})
		m = model.(*Model)
	}
	if m.listCursor != 1 {
		t.Errorf("cursor should be bounded at 1 (filter has 2 rows); got %d", m.listCursor)
	}
}

// TestFilterBranches_Substring: case-insensitive substring match,
// works across local + remote prefix.
func TestFilterBranches_Substring(t *testing.T) {
	branches := []string{"main", "origin/main", "origin/feat/oauth", "origin/feat/billing"}
	got := filterBranches(branches, "FEAT")
	if len(got) != 2 {
		t.Errorf("expected 2 matches for 'FEAT'; got %d", len(got))
	}
	got = filterBranches(branches, "main")
	if len(got) != 2 {
		t.Errorf("expected main + origin/main; got %d", len(got))
	}
}

// TestNewPR_TypedNumberSubmits: the power-user fast path — type a
// PR number into the filter, hit enter, get a workspace from that
// PR. Doesn't require the list to have loaded; works even on a
// cold cache.
func TestNewPR_TypedNumberSubmits(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.listInput.SetValue("1234")

	model, cmd := m.handleNewPRKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("enter on number should flip to busyMode; got %v", m.mode)
	}
	if !strings.Contains(m.busyTitle, "PR #1234") {
		t.Errorf("busy title should mention PR #1234; got %q", m.busyTitle)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
}

// TestNewPR_ArrowsThenEnter: recognition path — wait for the list
// to load, arrow to a row, hit enter. The picker reads the row's
// PR number and submits.
func TestNewPR_ArrowsThenEnter(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.newPRs = []ghx.PRSummary{
		{Number: 1185, Title: "Inbox improvements"},
		{Number: 1182, Title: "Fix oauth"},
	}

	// Down once → cursor on #1182.
	model, _ := m.handleNewPRKey(tea.KeyMsg{Type: tea.KeyDown})
	m = model.(*Model)
	if m.listCursor != 1 {
		t.Fatalf("listCursor = %d; want 1", m.listCursor)
	}

	model, cmd := m.handleNewPRKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("enter on row should flip to busyMode; got %v", m.mode)
	}
	if !strings.Contains(m.busyTitle, "PR #1182") {
		t.Errorf("busy title should reference selected row's PR; got %q", m.busyTitle)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
}

// TestNewPR_LoadedMsgPopulatesList: prListLoadedMsg from the async
// loader populates m.newPRs and clears the loading flag. View can
// then render the list.
func TestNewPR_LoadedMsgPopulatesList(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.newLoading = true

	prs := []ghx.PRSummary{{Number: 42, Title: "Test"}}
	model, _ := m.Update(prListLoadedMsg{prs: prs})
	m = model.(*Model)

	if m.newLoading {
		t.Errorf("newLoading should be false after msg")
	}
	if len(m.newPRs) != 1 || m.newPRs[0].Number != 42 {
		t.Errorf("newPRs not populated; got %+v", m.newPRs)
	}
}

// TestNewPR_LoadedMsgWithError: error in the loader surfaces as
// newLoadErr, list stays empty. View renders the error banner.
func TestNewPR_LoadedMsgWithError(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.newLoading = true

	model, _ := m.Update(prListLoadedMsg{err: fmt.Errorf("gh missing")})
	m = model.(*Model)

	if m.newLoading {
		t.Errorf("newLoading should be false after msg")
	}
	if m.newLoadErr == nil {
		t.Errorf("newLoadErr should be set on failure")
	}
}

// TestFilterPRs_NumericPrefix: typing "11" matches all PRs whose
// number starts with 11 — the "I half-remember the number" path.
func TestFilterPRs_NumericPrefix(t *testing.T) {
	prs := []ghx.PRSummary{
		{Number: 1185, Title: "A"},
		{Number: 1182, Title: "B"},
		{Number: 999, Title: "C"},
	}
	got := filterPRs(prs, "11")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (1185 + 1182)", len(got))
	}
}

// TestFilterPRs_TitleSubstring: non-numeric input matches title +
// author case-insensitively.
func TestFilterPRs_TitleSubstring(t *testing.T) {
	prs := []ghx.PRSummary{
		{Number: 1, Title: "Inbox improvements"},
		{Number: 2, Title: "Fix oauth"},
	}
	got := filterPRs(prs, "INBOX")
	if len(got) != 1 || got[0].Number != 1 {
		t.Errorf("expected single match for INBOX; got %+v", got)
	}
}

// TestProgressTickMsg_AppendsToBusyOutput: streaming chunks from
// the safeBuffer end up in m.busyOutput so the renderer can show
// live output. Each tick adds the drained chunk to the running
// total and schedules another tick (unless done).
func TestProgressTickMsg_AppendsToBusyOutput(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate

	buf := &safeBuffer{}
	model, cmd := m.Update(progressTickMsg{chunk: "Setting up...\n", buf: buf})
	m = model.(*Model)

	if !strings.Contains(m.busyOutput, "Setting up...") {
		t.Errorf("chunk should be appended to busyOutput; got %q", m.busyOutput)
	}
	if cmd == nil {
		t.Errorf("tick should re-schedule itself while not done")
	}
}

// TestProgressTickMsg_StopsTickingWhenDone: once the create
// completes (busyDone=true), the tick loop must stop. Otherwise we
// burn redraws on a finished operation.
func TestProgressTickMsg_StopsTickingWhenDone(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate
	m.busyDone = true

	_, cmd := m.Update(progressTickMsg{chunk: "trailing\n", buf: &safeBuffer{}})
	if cmd != nil {
		t.Errorf("tick after done should NOT re-schedule; got cmd %T", cmd)
	}
}

// TestProgressTickMsg_StopsTickingWhenLeftBusyMode: a stale tick
// arriving after the user dismissed the busy view (e.g. on
// successful auto-attach which flips mode back to listMode) must
// not keep the tick loop alive in the wrong mode.
func TestProgressTickMsg_StopsTickingWhenLeftBusyMode(t *testing.T) {
	m := newTestModel(false)
	m.mode = listMode

	_, cmd := m.Update(progressTickMsg{chunk: "", buf: &safeBuffer{}})
	if cmd != nil {
		t.Errorf("tick outside busyMode should be dropped; got cmd %T", cmd)
	}
}

// TestRemoveStartedMsg_DispatchesStreamingBatch: receiving a
// removeStartedMsg from the lazy-spawn path returns a tea.Batch that
// contains both the progress tick and the wait-done cmd. Without
// this dispatch the archive-script output wouldn't stream.
func TestRemoveStartedMsg_DispatchesStreamingBatch(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode

	buf := &safeBuffer{}
	done := make(chan removeDoneMsg, 1)
	_, cmd := m.Update(removeStartedMsg{buf: buf, done: done})

	if cmd == nil {
		t.Fatalf("removeStartedMsg should dispatch a streaming batch; got nil")
	}
}

// TestRetryStartedMsg_DispatchesStreamingBatch: same shape for the
// retry flow.
func TestRetryStartedMsg_DispatchesStreamingBatch(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode

	buf := &safeBuffer{}
	done := make(chan retryDoneMsg, 1)
	_, cmd := m.Update(retryStartedMsg{buf: buf, done: done})

	if cmd == nil {
		t.Fatalf("retryStartedMsg should dispatch a streaming batch; got nil")
	}
}

// TestRemoveDone_AppendsTrailingOutput: removeDoneMsg appends the
// final tail to busyOutput rather than overwriting. busyOutput
// already contains the streamed archive script output via tick
// messages; overwriting would erase it.
func TestRemoveDone_AppendsTrailingOutput(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOutput = "archive ran on port 41010\n"

	model, _ := m.Update(removeDoneMsg{output: "Removed.\n"})
	m = model.(*Model)

	if !strings.Contains(m.busyOutput, "archive ran on port 41010") {
		t.Errorf("removeDoneMsg should preserve streamed output: %q", m.busyOutput)
	}
	if !strings.Contains(m.busyOutput, "Removed.") {
		t.Errorf("removeDoneMsg should append trailing output: %q", m.busyOutput)
	}
}

// TestRetryDone_AppendsTrailingOutput: same contract for retry.
func TestRetryDone_AppendsTrailingOutput(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOutput = "setup running...\n"

	model, _ := m.Update(retryDoneMsg{output: "setup OK\n"})
	m = model.(*Model)

	if !strings.Contains(m.busyOutput, "setup running...") {
		t.Errorf("retryDoneMsg should preserve streamed output: %q", m.busyOutput)
	}
	if !strings.Contains(m.busyOutput, "setup OK") {
		t.Errorf("retryDoneMsg should append trailing output: %q", m.busyOutput)
	}
}

// TestSafeBuffer_DrainResets: the buffer accumulates writes and
// hands the drained content back to the caller, then resets so
// the next drain only returns NEW content. Without this contract,
// each tick would include the entire history and the View would
// show duplicated output.
func TestSafeBuffer_DrainResets(t *testing.T) {
	buf := &safeBuffer{}
	buf.Write([]byte("line one\n"))
	buf.Write([]byte("line two\n"))

	got := buf.Drain()
	if got != "line one\nline two\n" {
		t.Errorf("first drain = %q; want both lines", got)
	}
	if next := buf.Drain(); next != "" {
		t.Errorf("second drain should be empty; got %q", next)
	}

	buf.Write([]byte("line three\n"))
	if next := buf.Drain(); next != "line three\n" {
		t.Errorf("post-reset write should drain alone; got %q", next)
	}
}

// errFakeCreate is a sentinel for the create-error test. Lives here
// (not in the test func) so the literal stays readable.
var errFakeCreate = fakeErr("setup failed")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
