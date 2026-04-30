package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/oncactus/canopy/internal/config"
	"github.com/oncactus/canopy/internal/ghx"
	"github.com/oncactus/canopy/internal/state"
	"github.com/oncactus/canopy/internal/tmux"
	"github.com/oncactus/canopy/internal/ui/projectlist"
	"github.com/oncactus/canopy/internal/workspace"
)

// newTestModel builds a minimal *Model for unit-testing keymap and render
// paths that don't actually exercise the workspace.Manager. Used widely
// across this file; the bool param is unused after v0.8 unification but
// kept for callsite stability during the merge transition.
func newTestModel(_ bool) *Model {
	m := &Model{
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
	m.list = projectlist.New(projectlist.Options{})
	return m
}

// setTestRows is a test helper that pushes rows into both m.allRows
// (the unfiltered set) and m.list (the rendered set). Tests use this
// instead of mutating m.allRows directly so the projectlist sub-
// component sees the same data the model holds.
func (m *Model) setTestRows(rows []Row) {
	m.allRows = rows
	m.list.SetRows(m.filteredRows())
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
		m := NewUnified(nil, store, tc, "", "", "")
		if !m.inPopup {
			t.Errorf("inPopup = false; want true when CANOPY_IN_POPUP=1")
		}
	})

	t.Run("env unset → fullscreen mode", func(t *testing.T) {
		t.Setenv("CANOPY_IN_POPUP", "")
		m := NewUnified(nil, store, tc, "", "", "")
		if m.inPopup {
			t.Errorf("inPopup = true; want false when env unset")
		}
	})

	t.Run("env=other → fullscreen mode (strict eq)", func(t *testing.T) {
		t.Setenv("CANOPY_IN_POPUP", "true")
		m := NewUnified(nil, store, tc, "", "", "")
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
		m := NewUnified(nil, store, tc, "/some/project", "", "")
		if m.tab != tabLocal {
			t.Errorf("tab = %v; want tabLocal when currentProject != \"\"", m.tab)
		}
	})

	t.Run("no current project → tabGlobal", func(t *testing.T) {
		m := NewUnified(nil, store, tc, "", "", "")
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

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got := model.(*Model)
	if got.tab != tabGlobal {
		t.Errorf("after tab key: tab = %v; want tabGlobal", got.tab)
	}

	model, _ = got.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got = model.(*Model)
	if got.tab != tabLocal {
		t.Errorf("after second tab key: tab = %v; want tabLocal (round-trip)", got.tab)
	}
}

// TestHandleKey_TabSwitch_GlobalToLocalAutoFocus is the regression test
// for the user-reported "tab from Global doesn't enter Local" bug:
// when canopy is launched outside any project (currentProject == ""),
// Local has no meaningful filter, so Tab → Local would either show
// every row (no-op) or feel broken. The fix routes Global → Local
// through actionFocusProject when there's a cursor row, so Tab
// behaves as "enter the project I'm looking at."
func TestHandleKey_TabSwitch_GlobalToLocalAutoFocus(t *testing.T) {
	// Set up a temp project with canopy.json so actionFocusProject's
	// LoadFrom + workspace.New succeeds. The test only cares about the
	// post-Tab Model state, not Manager construction success — but we
	// need the load to not fail loudly.
	projectRoot := t.TempDir()
	canopyJSON := []byte(`{"scripts": {"setup": "x", "run": "y", "archive": "z"}}`)
	if err := os.WriteFile(filepath.Join(projectRoot, "canopy.json"), canopyJSON, 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}

	m := newTestModel(false)
	m.tab = tabGlobal
	m.currentProject = ""
	m.mgr = nil
	m.setTestRows([]Row{
		{Project: filepath.Base(projectRoot), ProjectRoot: projectRoot, IsMain: true, Name: "(main)", Status: "main"},
	})

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got := model.(*Model)
	if got.tab != tabLocal {
		t.Errorf("after Tab from Global w/ empty currentProject: tab = %v; want tabLocal", got.tab)
	}
	if got.currentProject != projectRoot {
		t.Errorf("after Tab: currentProject = %q; want %q (auto-focus from cursor row)", got.currentProject, projectRoot)
	}
}

// TestHandleKey_TabSwitch_NoRowsLeavesEmptyContext covers the no-row
// edge case: Tab from Global with no rows can't auto-focus (nothing to
// focus on), so it falls through to the plain tab flip. currentProject
// stays empty; user sees the "no projects" empty state.
func TestHandleKey_TabSwitch_NoRowsLeavesEmptyContext(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.currentProject = ""
	m.mgr = nil
	m.setTestRows(nil)

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got := model.(*Model)
	if got.tab != tabLocal {
		t.Errorf("after Tab w/ no rows: tab = %v; want tabLocal (plain flip)", got.tab)
	}
	if got.currentProject != "" {
		t.Errorf("after Tab w/ no rows: currentProject = %q; want \"\" (nothing to focus)", got.currentProject)
	}
}

// TestHandleKey_NOnGlobalTab covers the v0.10+ cross-project `n` flow.
// Global tab no longer hides `n` outright; it derives the target project
// from the cursor row (mirroring how d/R/K already work). The two cases
// here pin both halves of the predicate: with a row, n is available and
// opens the picker; without one (or with a row missing ProjectRoot),
// n stays a no-op so the binding's help-line entry disappears too.
//
// Bindings-table semantics: a binding whose Available returns false
// doesn't fire its Action — no err set, no mode change, silent. The
// visual cue (n missing from help) does the user-facing signaling.
func TestHandleKey_NOnGlobalTab(t *testing.T) {
	t.Run("with cursor row → opens picker (cross-project)", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabGlobal
		// Cursor row's ProjectRoot will be loaded by managerForRow.
		// Use the test model's own project (which newTestModel set up
		// with Cfg.ProjectRoot = "/tmp/test-project") so managerForRow
		// hits the m.mgr fast-path and skips config.LoadFrom.
		m.setTestRows([]Row{
			{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "ws-1", Status: state.StatusReady},
		})

		model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		got := model.(*Model)
		if got.mode != newPickerMode {
			t.Errorf("n on Global tab w/ row: mode = %v; want newPickerMode", got.mode)
		}
		if got.newTargetMgr == nil {
			t.Errorf("newTargetMgr unset; want target resolved from cursor row")
		}
		if got.newTargetRoot != "/tmp/test-project" {
			t.Errorf("newTargetRoot = %q; want /tmp/test-project", got.newTargetRoot)
		}
		if got.newTargetName != "test-project" {
			t.Errorf("newTargetName = %q; want test-project", got.newTargetName)
		}
	})

	t.Run("no rows → silent no-op", func(t *testing.T) {
		m := newTestModel(false)
		m.mgr = nil // Pure global-mode invocation (canopy from outside any project).
		m.tab = tabGlobal
		m.setTestRows(nil)

		model, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		got := model.(*Model)
		if got.mode != listMode {
			t.Errorf("n on empty Global tab: mode = %v; want listMode", got.mode)
		}
		if cmd != nil {
			t.Errorf("n on empty Global tab: cmd = %v; want nil (no-op)", cmd)
		}
		if got.newTargetMgr != nil {
			t.Errorf("newTargetMgr set without resolution; want nil")
		}
	})

	t.Run("row missing ProjectRoot → silent no-op", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabGlobal
		m.setTestRows([]Row{
			{Project: "ghost", ProjectRoot: "", Name: "orphan", Status: state.StatusReady},
		})

		model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		got := model.(*Model)
		if got.mode != listMode {
			t.Errorf("n on row w/ empty ProjectRoot: mode = %v; want listMode", got.mode)
		}
	})
}

// TestAvailableNewWorkspace exercises the binding's Available predicate
// directly. Both the help-line filter AND the dispatch gate read from
// this; one source of truth for "is n usable right now."
//
// v0.10+ broadened semantics: Global tab is now a yes when the cursor
// row has a non-empty ProjectRoot — managerForRow does the heavy lift
// at action time. We deliberately don't require m.mgr on the Global
// path so canopy launched from outside any project still gets `n`
// once the user points at a row.
func TestAvailableNewWorkspace(t *testing.T) {
	type rowSpec struct {
		project, root string
	}
	cases := []struct {
		name string
		mgr  bool
		tab  tabKind
		rows []rowSpec
		want bool
	}{
		{"mgr + Local → enabled", true, tabLocal, nil, true},
		{"no mgr + Local → disabled (no canopy.json context)", false, tabLocal, nil, false},
		{"mgr + Global w/ row+root → enabled (cross-project)", true, tabGlobal, []rowSpec{{"p", "/tmp/p"}}, true},
		{"no mgr + Global w/ row+root → enabled (pure-global launch)", false, tabGlobal, []rowSpec{{"p", "/tmp/p"}}, true},
		{"mgr + Global w/ no rows → disabled", true, tabGlobal, nil, false},
		{"no mgr + Global w/ no rows → disabled", false, tabGlobal, nil, false},
		{"Global w/ row missing ProjectRoot → disabled", true, tabGlobal, []rowSpec{{"p", ""}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(false)
			if !tc.mgr {
				m.mgr = nil
			}
			m.tab = tc.tab
			rows := make([]Row, 0, len(tc.rows))
			for _, r := range tc.rows {
				rows = append(rows, Row{Project: r.project, ProjectRoot: r.root, Name: "ws", Status: state.StatusReady})
			}
			m.setTestRows(rows)
			got := availableNewWorkspace(m)
			if got != tc.want {
				t.Errorf("availableNewWorkspace(mgr=%v, tab=%v, rows=%v) = %v; want %v",
					tc.mgr, tc.tab, tc.rows, got, tc.want)
			}
		})
	}
}

// TestAvailableFocusProject covers the `o` predicate. Available on
// Global tab whenever the cursor row has a non-empty ProjectRoot —
// the same-project case is allowed (re-focus is a harmless no-op
// switch instead of muscle-memory-killing friction).
func TestAvailableFocusProject(t *testing.T) {
	cases := []struct {
		name        string
		tab         tabKind
		rowRoot     string
		currentProj string
		want        bool
	}{
		{"Local tab → never", tabLocal, "/tmp/p", "/tmp/p", false},
		{"Global, row root empty → no", tabGlobal, "", "", false},
		{"Global, row root != current → yes", tabGlobal, "/tmp/p", "/tmp/q", true},
		{"Global, row root == current → yes (re-focus is fine)", tabGlobal, "/tmp/p", "/tmp/p", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(false)
			m.tab = tc.tab
			m.currentProject = tc.currentProj
			if tc.rowRoot != "" {
				m.setTestRows([]Row{{Project: "p", ProjectRoot: tc.rowRoot, Name: "ws", Status: state.StatusReady}})
			} else if tc.tab == tabGlobal {
				m.setTestRows([]Row{{Project: "p", Name: "ws", Status: state.StatusReady}})
			}
			if got := availableFocusProject(m); got != tc.want {
				t.Errorf("availableFocusProject = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestActionNewWorkspace_Local: from Local tab, n populates newTargetMgr
// from m.mgr (the launch-context Manager). Title/root mirror the project
// the user is actively working in.
func TestActionNewWorkspace_Local(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabLocal

	model, _ := actionNewWorkspace(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	got := model.(*Model)
	if got.mode != newPickerMode {
		t.Fatalf("mode = %v; want newPickerMode", got.mode)
	}
	if got.newTargetMgr != m.mgr {
		t.Errorf("newTargetMgr = %p; want m.mgr (%p)", got.newTargetMgr, m.mgr)
	}
	if got.newTargetRoot != m.mgr.Cfg.ProjectRoot {
		t.Errorf("newTargetRoot = %q; want %q", got.newTargetRoot, m.mgr.Cfg.ProjectRoot)
	}
	if got.newTargetName != m.mgr.Cfg.Project {
		t.Errorf("newTargetName = %q; want %q", got.newTargetName, m.mgr.Cfg.Project)
	}
}

// TestActionNewWorkspace_ClearsOnEsc: pressing esc out of the picker
// clears the in-flight new-workspace target so the next `n` press
// starts from a clean slate.
func TestActionNewWorkspace_ClearsOnEsc(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabLocal
	model, _ := actionNewWorkspace(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = model.(*Model)
	if m.newTargetMgr == nil {
		t.Fatal("setup: newTargetMgr should be set after actionNewWorkspace")
	}

	model, _ = m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	got := model.(*Model)
	if got.mode != listMode {
		t.Errorf("after esc: mode = %v; want listMode", got.mode)
	}
	if got.newTargetMgr != nil || got.newTargetRoot != "" || got.newTargetName != "" {
		t.Errorf("after esc: newTarget* should be cleared; got mgr=%v root=%q name=%q",
			got.newTargetMgr != nil, got.newTargetRoot, got.newTargetName)
	}
}

// TestRenderTargetBanner_ShowsProjectName: the banner must render the
// target project as a primary identifier (chip + path), not subtle
// chrome — load-bearing for cross-project intent clarity.
func TestRenderTargetBanner_ShowsProjectName(t *testing.T) {
	m := newTestModel(false)
	m.newTargetName = "cravd"
	m.newTargetRoot = "/Users/avi/Work/cravd"

	out := stripAnsi(m.renderTargetBanner())
	if !strings.Contains(out, "creating in") {
		t.Errorf("banner missing 'creating in' label: %q", out)
	}
	if !strings.Contains(out, "cravd") {
		t.Errorf("banner missing project name: %q", out)
	}
	if !strings.Contains(out, "/Users/avi/Work/cravd") {
		t.Errorf("banner missing project root: %q", out)
	}
}

// TestRenderTargetBanner_EmptyWhenUnset: outside the new-workspace flow
// the banner returns "" so render paths that include it (busy view's
// non-create ops, future call sites) emit nothing rather than a stray
// blank line.
func TestRenderTargetBanner_EmptyWhenUnset(t *testing.T) {
	m := newTestModel(false)
	if got := m.renderTargetBanner(); got != "" {
		t.Errorf("renderTargetBanner with no target = %q; want empty string", got)
	}
}

// TestBusySuccessMessage_CreateNamesProject: the success line for a
// completed create includes the target project so the banner's promise
// ("creating in cravd") is fulfilled at completion ("created in cravd").
// Empty projectName falls back to the legacy generic message.
func TestBusySuccessMessage_CreateNamesProject(t *testing.T) {
	t.Run("create with project → named line", func(t *testing.T) {
		got := busySuccessMessage(busyOpCreate, "cravd")
		if !strings.Contains(got, "cravd") {
			t.Errorf("create success = %q; want it to name the project", got)
		}
	})
	t.Run("create without project → generic line", func(t *testing.T) {
		got := busySuccessMessage(busyOpCreate, "")
		if got != "Workspace created successfully." {
			t.Errorf("create success (no project) = %q; want generic legacy line", got)
		}
	})
	t.Run("remove ignores projectName", func(t *testing.T) {
		got := busySuccessMessage(busyOpRemove, "cravd")
		if got != "Workspace removed." {
			t.Errorf("remove success = %q; want 'Workspace removed.'", got)
		}
	})
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
	m.setTestRows([]Row{
		{Project: "foo", ProjectRoot: "/p/foo", Name: "ws-a"},
		{Project: "foo", ProjectRoot: "/p/foo", Name: "ws-b"},
		{Project: "bar", ProjectRoot: "/p/bar", Name: "ws-c"},
	})

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
	m.setTestRows([]Row{
		{Project: "foo", ProjectRoot: "/p/foo", Name: "silent-falcon"},
		{Project: "foo", ProjectRoot: "/p/foo", Name: "misty-aspen"},
		{Project: "bar", ProjectRoot: "/p/bar", Name: "bold-ox", Branch: "feat/falcon"},
	})

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

// TestActionDelete_StoresProjectRoot is the regression test for the C5
// adversarial finding: cross-project delete must match by (Project, Name)
// pair, not Name alone. Two projects each with a workspace named "foo"
// would otherwise be ambiguous if a refresh re-orders rows between
// modal-open and confirm — the user could delete project B's foo when
// they meant project A's foo (data loss).
//
// Verifies actionDelete records BOTH deleteTarget AND deleteTargetRoot
// when opening the confirm modal.
func TestActionDelete_StoresProjectRoot(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.setTestRows([]Row{
		{Project: "alpha", ProjectRoot: "/p/alpha", Name: "foo", Status: state.StatusReady, Path: "/ws/alpha-foo"},
		{Project: "bravo", ProjectRoot: "/p/bravo", Name: "foo", Status: state.StatusReady, Path: "/ws/bravo-foo"},
	})
	// Cursor starts at row 0 (alpha's foo).

	// We can't call actionDelete directly on cross-project rows here
	// because managerForRow needs canopy.json on disk for /p/alpha. Test
	// the field-recording invariant by calling actionDelete and checking
	// state — error path included.
	_, _ = actionDelete(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})

	// Even if managerForRow errored (no canopy.json at /p/alpha in this
	// fake state), the precondition for the data-loss scenario is that
	// deleteTarget+deleteTargetRoot get set BEFORE managerForRow is
	// called. Verify the structure: when modal opens (mode change), root
	// must be set; when modal aborts (managerForRow err), neither is set.
	if m.mode == confirmDeleteMode {
		// Modal opened: both fields must be populated and consistent.
		if m.deleteTarget != "foo" {
			t.Errorf("deleteTarget = %q; want 'foo'", m.deleteTarget)
		}
		if m.deleteTargetRoot != "/p/alpha" {
			t.Errorf("deleteTargetRoot = %q; want '/p/alpha' (cursor row's project)", m.deleteTargetRoot)
		}
	} else {
		// Modal didn't open (managerForRow failed before mode change).
		// Both fields should remain empty so a stale value can't leak
		// into the next attempt.
		if m.deleteTarget != "" || m.deleteTargetRoot != "" {
			t.Errorf("modal didn't open but deleteTarget=%q / deleteTargetRoot=%q leaked",
				m.deleteTarget, m.deleteTargetRoot)
		}
	}
}

// TestRetryConfirmModal_NonBrokenTriggers: pressing R on a non-broken
// workspace opens the confirmRetry y/N gate (D3/CP1) instead of
// erroring. Mirrors the CLI's --force friction in TUI form.
func TestRetryConfirmModal_NonBrokenTriggers(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{Project: "test-project", ProjectRoot: "/tmp/test-project",
			Name: "healthy-ws", Status: state.StatusReady},
	})

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

// TestEscapeIfDeletingCurrent_NoOpWhenMismatched: the escape helper
// short-circuits when any of the gating conditions fail — nil mgr,
// empty currentWorkspace, name mismatch, OR project-root mismatch
// (workspace names are unique per-project, not globally — A/foo and
// B/foo coexist, and switching when deleting B/foo from inside A/foo
// would trip the user into the wrong project's main session).
func TestEscapeIfDeletingCurrent_NoOpWhenMismatched(t *testing.T) {
	cases := []struct {
		name                 string
		mgr                  *workspace.Manager
		currentWorkspace     string
		currentWorkspaceRoot string
		targetRoot           string
		targetName           string
	}{
		{"nil mgr", nil, "ws-a", "/a", "/a", "ws-a"},
		{"empty currentWorkspace", &workspace.Manager{}, "", "/a", "/a", "ws-a"},
		{"name mismatch", &workspace.Manager{}, "ws-b", "/a", "/a", "ws-a"},
		{"project root mismatch (cross-project name collision)", &workspace.Manager{}, "ws-a", "/a", "/b", "ws-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{currentWorkspace: tc.currentWorkspace, currentWorkspaceRoot: tc.currentWorkspaceRoot, tc: nil}
			// No panic = pass: we never reached the tmux calls.
			m.escapeIfDeletingCurrent(tc.mgr, tc.targetRoot, tc.targetName)
		})
	}
}

// TestUpdate_RowsLoaded_EmptyFirstThenPreselects: the latch must not
// fire on an early empty rowsLoadedMsg — popup invocations sometimes
// see an initial probe with no rows yet (state racing the refresh
// goroutine), and consuming the preselect opportunity there leaves
// cursor=0 even after real rows arrive on the next refresh. The
// preselect should still hit when the actual rows show up.
func TestUpdate_RowsLoaded_EmptyFirstThenPreselects(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.currentProject = "/b/canopy"
	m.currentWorkspaceRoot = "/b/canopy"
	m.currentWorkspace = "ancient-hornet"

	// First refresh arrives empty (or with an error).
	next, _ := m.Update(rowsLoadedMsg{rows: nil, err: nil})
	got := next.(*Model)
	if got.initialCursorPlaced {
		t.Errorf("latch fired on empty first refresh; preselect would be lost")
	}

	// Second refresh has the real rows.
	rows := []Row{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "bold-falcon", Status: state.StatusReady},
		{Project: "canopy", ProjectRoot: "/b/canopy", Name: "ancient-hornet", Status: state.StatusReady},
	}
	next2, _ := got.Update(rowsLoadedMsg{rows: rows})
	got2 := next2.(*Model)
	if !got2.initialCursorPlaced {
		t.Errorf("latch did not fire after preselect succeeded")
	}
	cur, _ := got2.list.CursorRow()
	if cur.Name != "ancient-hornet" {
		t.Errorf("cursor row name = %q; want ancient-hornet", cur.Name)
	}
}

// TestUpdate_RowsLoaded_LatchFiresOnMiss: when the first non-empty
// load doesn't contain the target row (e.g. the user changed tab
// before rows arrived, filtering it out), the latch must still fire
// so a later refresh — when the row reappears in the filtered set —
// doesn't yank the cursor away from wherever the user navigated to.
func TestUpdate_RowsLoaded_LatchFiresOnMiss(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.currentProject = "/b/canopy"
	m.currentWorkspaceRoot = "/b/canopy"
	m.currentWorkspace = "ancient-hornet"

	// Rows that DON'T contain the target — simulates user-flipped tab
	// or search hiding the row at first-load time.
	rows := []Row{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "bold-falcon", Status: state.StatusReady},
	}
	next, _ := m.Update(rowsLoadedMsg{rows: rows})
	got := next.(*Model)
	if !got.initialCursorPlaced {
		t.Errorf("latch did not fire on first non-empty load with miss; would auto-jump on later refresh")
	}
}

// TestUpdate_RowsLoaded_PreselectsCurrentWorkspace: when currentWorkspace
// is set (popup launched from inside a workspace dir), the first
// rowsLoadedMsg moves the cursor onto that workspace's row. Subsequent
// refreshes are no-ops on cursor position (latched via initialCursorPlaced)
// so the user's hovering doesn't get yanked back.
func TestUpdate_RowsLoaded_PreselectsCurrentWorkspace(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal // multi-row scenario; cursor=0 initially
	m.currentProject = "/b/canopy"
	m.currentWorkspaceRoot = "/b/canopy"
	m.currentWorkspace = "ancient-hornet"

	rows := []Row{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "bold-falcon", Status: state.StatusReady},
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "soft-fox", Status: state.StatusStopped},
		{Project: "canopy", ProjectRoot: "/b/canopy", Name: "ancient-hornet", Status: state.StatusReady},
	}

	next, _ := m.Update(rowsLoadedMsg{rows: rows})
	got := next.(*Model)
	if !got.initialCursorPlaced {
		t.Errorf("initialCursorPlaced = false after first load; want true")
	}
	cur, ok := got.list.CursorRow()
	if !ok {
		t.Fatalf("CursorRow not ok after rowsLoaded")
	}
	if cur.Name != "ancient-hornet" {
		t.Errorf("cursor row name = %q; want %q (preselect should land here)", cur.Name, "ancient-hornet")
	}

	// Second refresh: user has navigated to row 0 in the meantime;
	// we simulate by setting cursor manually, then dispatching another
	// rowsLoadedMsg. Cursor should NOT yank back.
	got.list.SetCursorTo("/a/cravd", "bold-falcon")
	next2, _ := got.Update(rowsLoadedMsg{rows: rows})
	got2 := next2.(*Model)
	cur2, _ := got2.list.CursorRow()
	if cur2.Name != "bold-falcon" {
		t.Errorf("cursor after second refresh = %q; want bold-falcon (latched)", cur2.Name)
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

// TestRenderHelpLine_TabSwitch: help line shows nav, tab, and search
// keybinds always. `n` desc ("new") shows only on Local tab with
// non-nil mgr.
//
// stripAnsi the rendered output so assertions don't couple to the
// keybind-pill styling — these tests are about which BINDINGS appear,
// not how they look.
func TestRenderHelpLine_TabSwitch(t *testing.T) {
	t.Run("Local tab with mgr → n shown", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabLocal
		out := stripAnsi(m.renderHelpLine())
		if !strings.Contains(out, "new") {
			t.Errorf("Local tab help missing 'new' desc: %q", out)
		}
		if !strings.Contains(out, "switch-tab") {
			t.Errorf("help line missing 'switch-tab': %q", out)
		}
	})

	t.Run("Global tab w/ row → n shown (cross-project)", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabGlobal
		m.setTestRows([]Row{
			{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "ws", Status: state.StatusReady},
		})
		out := stripAnsi(m.renderHelpLine())
		if !strings.Contains(out, "new") {
			t.Errorf("Global tab w/ row help should show 'new' desc (cross-project n): %q", out)
		}
	})

	t.Run("Global tab w/ no rows → n hidden", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabGlobal
		m.setTestRows(nil)
		out := stripAnsi(m.renderHelpLine())
		if strings.Contains(out, "new") {
			t.Errorf("Global tab w/o rows should not show 'new' desc: %q", out)
		}
	})

	t.Run("nil mgr + Local tab → n hidden", func(t *testing.T) {
		m := newTestModel(false)
		m.mgr = nil
		m.tab = tabLocal
		out := stripAnsi(m.renderHelpLine())
		if strings.Contains(out, "new") {
			t.Errorf("nil mgr Local help should not show 'new' desc: %q", out)
		}
	})
}

// stripAnsi removes ANSI SGR escape sequences from s. Tiny inline impl
// rather than pulling in a dep; canopy's help-line render only emits
// SGR (\x1b[...m), no cursor-movement codes.
func stripAnsi(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if c == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// TestRowHintsMsg_MergesIntoMatchingRow: late-arriving lifecycle hints
// merge into the matching row by name + project. After the v0.8 pivot
// to projectlist for rendering, hint storage lives inside the embedded
// list (list.UpdateRowHints). The model's allRows is unaffected; the
// View sees the merged hints because it delegates to list.View().
func TestRowHintsMsg_MergesIntoMatchingRow(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "soft-fox"},
		{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "ancient-hornet"},
	})
	hints := []state.Hint{{Kind: "shipped", Message: "merged"}}

	model, _ := m.Update(rowHintsMsg{project: "test-project", name: "soft-fox", hints: hints})
	m = model.(*Model)

	// projectlist owns the rendered rows; UpdateRowHints mutates them.
	// View() includes the badge text from the rendered hints.
	out := m.list.View()
	if !strings.Contains(out, "shipped") && !strings.Contains(out, "merged") {
		// Specific badge text may differ; confirm the hint reached
		// projectlist by checking the badge exists at all.
		t.Errorf("hint not surfaced in projectlist view:\n%s", out)
	}
}

// TestRowHintsMsg_NoMatchIsSilent: a hint update for a row that no
// longer exists (concurrent rm dropped it) is a no-op, not a panic.
// projectlist.UpdateRowHints handles this — silent on no-match.
func TestRowHintsMsg_NoMatchIsSilent(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "soft-fox"},
	})

	model, _ := m.Update(rowHintsMsg{project: "test-project", name: "ghost-row", hints: []state.Hint{{Kind: "shipped"}}})
	m = model.(*Model)

	// No panic, no mutation observable on the matched row's view.
	out := m.list.View()
	if strings.Contains(out, "ghost-row") {
		t.Errorf("ghost-row should not appear in view: %s", out)
	}
}

// TestView_HintBadgesAppearInProjectlist: the unified TUI renders rows
// via projectlist, which picks up Hints and renders the corresponding
// badges. Critical for consistency — same badge vocabulary across
// every canopy surface.
func TestView_HintBadgesAppearInProjectlist(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{{
		Project:     "test-project",
		ProjectRoot: "/tmp/test-project",
		Name:        "soft-fox",
		Branch:      "soft-fox",
		Status:      state.StatusReady,
		Hints:       []state.Hint{{Kind: "pr_status", Message: "PR #42 merged; ready to close workspace"}},
	}})
	out := m.list.View()
	if !strings.Contains(out, "PR merged") {
		t.Errorf("PR merged badge missing in projectlist view:\n%s", out)
	}
}

// TestView_NoHintsNoBadges: rows without hints render unchanged
// (no trailing badge text).
func TestView_NoHintsNoBadges(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{{
		Project:     "test-project",
		ProjectRoot: "/tmp/test-project",
		Name:        "soft-fox",
		Branch:      "soft-fox",
		Status:      state.StatusReady,
	}})
	out := m.list.View()
	for _, badge := range []string{"↻ rename", "✓ merged", "PR open", "PR merged"} {
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
	m.setTestRows([]Row{
		{IsMain: true, Name: "(main)", Branch: "—"},
		{Name: "pr-1185", Branch: "pdx91/inbox-improvements"},
		{Name: "soft-fox", Branch: "feat/oauth"},
	})
	wsName, taken := m.branchInWorkspace("feat/oauth")
	if !taken || wsName != "soft-fox" {
		t.Errorf("expected (soft-fox, true); got (%q, %v)", wsName, taken)
	}
}

// TestBranchInWorkspace_NoMatch: branch with no matching workspace
// returns false. Empty branch string also returns false (defensive).
func TestBranchInWorkspace_NoMatch(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{{Name: "soft-fox", Branch: "feat/oauth"}})
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
	m.setTestRows([]Row{{IsMain: true, Branch: "—"}})
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
	m.setTestRows([]Row{{Name: "pr-1185", Branch: "pdx91/inbox-improvements"}})
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
	m.setTestRows([]Row{{Name: "pr-1185", Branch: "pdx91/inbox-improvements"}})
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

// TestSelectedHint covers the four-corner truth table of selectedHint:
// empty list → "", main row → "", non-broken row → "", broken row with
// hint → hint. The v0.8 unification promoted LastErrorHint onto
// state.GlobalRow; this is the regression test for the renderer that
// surfaces it under the table.
func TestSelectedHint(t *testing.T) {
	cases := []struct {
		name string
		rows []Row
		want string
	}{
		{
			name: "empty_list",
			rows: nil,
			want: "",
		},
		{
			name: "main_row_skipped",
			rows: []Row{
				{IsMain: true, Status: "broken", LastErrorHint: "ignored on main"},
			},
			want: "",
		},
		{
			name: "non_broken_skipped",
			rows: []Row{
				{Status: state.StatusReady, LastErrorHint: "stale hint"},
			},
			want: "",
		},
		{
			name: "broken_no_hint",
			rows: []Row{
				{Status: state.StatusBroken, LastErrorHint: ""},
			},
			want: "",
		},
		{
			name: "broken_with_hint",
			rows: []Row{
				{Status: state.StatusBroken, LastErrorHint: "missing bin/dev script"},
			},
			want: "missing bin/dev script",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(false)
			m.setTestRows(tc.rows)
			if got := m.selectedHint(); got != tc.want {
				t.Errorf("selectedHint() = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestFillMainBranches_DefaultsToMain covers the fallback path: a project
// with no origin/main or origin/master remote (e.g. a freshly-init local
// repo) gets "main" as the displayed default branch rather than the
// "—" placeholder. The function must not error or skip the row when
// DetectDefaultBranch fails — the UI relies on every main row carrying
// a non-empty Branch so the branch column renders consistently.
func TestFillMainBranches_DefaultsToMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "--initial-branch=trunk").Run(); err != nil {
		t.Skipf("git init: %v", err)
	}

	rows := []state.GlobalRow{
		{IsMain: true, ProjectRoot: dir, Project: "fresh", Name: "(main)", Branch: "—"},
		{IsMain: false, ProjectRoot: dir, Project: "fresh", Name: "ws", Branch: "feat-x"},
	}
	fillMainBranches(context.Background(), rows)

	if rows[0].Branch != "main" {
		t.Errorf("main row Branch = %q; want %q (fallback when origin/main|master miss)", rows[0].Branch, "main")
	}
	if rows[1].Branch != "feat-x" {
		t.Errorf("non-main row Branch = %q; want %q (untouched)", rows[1].Branch, "feat-x")
	}
}

// TestFillMainBranches_DetectsOriginMain wires up a real git repo with
// an origin/main ref so DetectDefaultBranch's happy path exercises end-
// to-end. The fallback test covers the error path; this covers the
// detection path.
func TestFillMainBranches_DetectsOriginMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	steps := [][]string{
		{"-C", repo, "init", "--initial-branch=main"},
		{"-C", repo, "config", "user.email", "t@x"},
		{"-C", repo, "config", "user.name", "t"},
		{"-C", repo, "commit", "--allow-empty", "-m", "x"},
		// Synthesize an origin/main remote-tracking ref by pointing
		// refs/remotes/origin/main at HEAD. No real network round-trip
		// — DetectDefaultBranch only reads the local ref.
		{"-C", repo, "update-ref", "refs/remotes/origin/main", "HEAD"},
	}
	for _, args := range steps {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	rows := []state.GlobalRow{
		{IsMain: true, ProjectRoot: repo, Project: "p", Name: "(main)", Branch: "—"},
	}
	fillMainBranches(context.Background(), rows)

	if rows[0].Branch != "main" {
		t.Errorf("main row Branch = %q; want %q", rows[0].Branch, "main")
	}
}

// TestRenderNewPicker_NoBracketRegression: variant picker shortcut
// letters used to render as `[n] `, `[p] ` etc. wrapped in
// brokenStyle (red 196 — same hue as broken-status workspaces and
// error banners). The new style uses keyPillStyle: the pill chrome
// itself implies "press this," so the literal brackets are gone.
//
// Asserting structurally (no `[n]` in the output) rather than against
// SGR color codes — lipgloss strips colors when there's no TTY, so a
// color-based check would silently pass even if the brokenStyle wrap
// came back. Bracket presence is a stable structural signal.
func TestRenderNewPicker_NoBracketRegression(t *testing.T) {
	m := newTestModel(false)
	// Banner needs a target so the picker renders cleanly.
	m.newTargetName = "test-project"
	m.newTargetRoot = "/tmp/test-project"

	out := stripAnsi(m.renderNewPicker())

	for _, opt := range newPickerOptions {
		bracket := "[" + opt.key + "]"
		if strings.Contains(out, bracket) {
			t.Errorf("variant picker rendered literal %q — regression to red-bracket era. want keyPillStyle pill chrome.\noutput:\n%s",
				bracket, out)
		}
		if !strings.Contains(out, opt.key) {
			t.Errorf("variant picker missing shortcut letter %q in output:\n%s",
				opt.key, out)
		}
	}

	// Positive check via the same render pipeline: the pill chrome
	// for the cursor row's letter is what keyPillStyle.Render emits.
	// Comparing the rendered substring is profile-agnostic — works
	// in both colored and stripped output.
	wantPill := keyPillStyle.Render(newPickerOptions[m.newPickerCursor].key)
	if !strings.Contains(m.renderNewPicker(), wantPill) {
		t.Errorf("variant picker missing expected keyPillStyle render for cursor letter %q",
			newPickerOptions[m.newPickerCursor].key)
	}
}

// TestRenderFilterPill_CaretMatchesMainScreen: the filter pill caret
// must be "▏" (a thin vertical bar) — same as renderSearchLine's
// caret. The block-cursor that bubbles' textinput.View() emits would
// look out of place against the main TUI's vocabulary.
func TestRenderFilterPill_CaretMatchesMainScreen(t *testing.T) {
	ti := textinput.New()
	ti.SetValue("foo")
	out := renderFilterPill(ti)
	if !strings.Contains(out, "▏") {
		t.Errorf("filter pill missing main-screen caret '▏': %q", out)
	}
	if !strings.Contains(stripAnsi(out), "foo▏") {
		t.Errorf("caret should sit immediately after typed value; got: %q",
			stripAnsi(out))
	}
}

// TestRenderFilterPill_LabelIsFilter: pill label is "🔍 FILTER" (not
// "SEARCH") because this narrows a fixed list rather than searching
// across all rows. Same chrome family as the main TUI, different verb.
func TestRenderFilterPill_LabelIsFilter(t *testing.T) {
	ti := textinput.New()
	out := stripAnsi(renderFilterPill(ti))
	if !strings.Contains(out, "🔍 FILTER") {
		t.Errorf("filter pill missing 'FILTER' label: %q", out)
	}
	if strings.Contains(out, "SEARCH") {
		t.Errorf("filter pill should NOT use SEARCH label (that's the main-screen verb): %q", out)
	}
}

// TestNewFlow_CursorCaretUnified: every screen in the new-workspace
// flow (variant picker + PR + issue + branch) uses the same "❯ "
// cursor caret that the main workspace list uses. One vocabulary
// across the TUI — the eye reads "here's what's selected" without
// learning a per-modal indicator.
//
// Catches the v0.10 era where the variant picker used "> " and the
// sub-modals used "● " — three different cursor glyphs in flows the
// user moves between in the same task. Regression test: if anyone
// re-introduces a per-screen caret, this fails loudly with the
// rendered output.
func TestNewFlow_CursorCaretUnified(t *testing.T) {
	const wantCaret = "  ❯ "

	t.Run("variant picker", func(t *testing.T) {
		m := newTestModel(false)
		m.newTargetName = "test-project"
		m.newTargetRoot = "/tmp/test-project"
		out := stripAnsi(m.renderNewPicker())
		if !strings.Contains(out, wantCaret) {
			t.Errorf("variant picker missing %q caret:\n%s", wantCaret, out)
		}
	})

	t.Run("PR picker", func(t *testing.T) {
		m := newTestModel(false)
		m.mode = newPRMode
		m.newTargetName = "test-project"
		m.newPRs = []ghx.PRSummary{{Number: 1, Title: "x", HeadRefName: "x"}}
		out := stripAnsi(m.renderNewPR())
		if !strings.Contains(out, wantCaret) {
			t.Errorf("PR picker missing %q caret:\n%s", wantCaret, out)
		}
	})

	t.Run("issue picker", func(t *testing.T) {
		m := newTestModel(false)
		m.mode = newIssueMode
		m.newTargetName = "test-project"
		m.newIssues = []ghx.IssueSummary{{Number: 1, Title: "x"}}
		out := stripAnsi(m.renderNewIssue())
		if !strings.Contains(out, wantCaret) {
			t.Errorf("issue picker missing %q caret:\n%s", wantCaret, out)
		}
	})

	t.Run("branch picker", func(t *testing.T) {
		m := newTestModel(false)
		m.mode = newBranchMode
		m.newTargetName = "test-project"
		m.newBranches = []string{"main"}
		out := stripAnsi(m.renderNewBranch())
		if !strings.Contains(out, wantCaret) {
			t.Errorf("branch picker missing %q caret:\n%s", wantCaret, out)
		}
	})
}

// TestRenderFilterPill_PlaceholderShownWhenEmpty: when the user hasn't
// typed anything, the textinput's Placeholder surfaces as a dim hint
// to the right of the pill so per-modal guidance ("type a PR number,
// or arrow to a row below") still has a home. Once the user types,
// the hint must get out of the way — otherwise it competes with the
// typed value for screen real estate.
func TestRenderFilterPill_PlaceholderShownWhenEmpty(t *testing.T) {
	ti := textinput.New()
	ti.Placeholder = "type a PR number, or arrow to a row below"

	t.Run("empty value → placeholder shown", func(t *testing.T) {
		ti.SetValue("")
		out := stripAnsi(renderFilterPill(ti))
		if !strings.Contains(out, "type a PR number") {
			t.Errorf("empty filter pill should show placeholder hint: %q", out)
		}
	})

	t.Run("non-empty value → placeholder hidden", func(t *testing.T) {
		ti.SetValue("1234")
		out := stripAnsi(renderFilterPill(ti))
		if strings.Contains(out, "type a PR number") {
			t.Errorf("non-empty filter pill should NOT show placeholder: %q", out)
		}
		if !strings.Contains(out, "1234") {
			t.Errorf("non-empty filter pill missing typed value: %q", out)
		}
	})
}
