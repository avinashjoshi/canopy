package ui

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/oncactus/canopy/internal/state"
	"github.com/oncactus/canopy/internal/tmux"
)

// TestNewGlobal_Constructs: NewGlobal returns a non-nil GlobalModel
// whose Init produces a refresh cmd. Smoke test that the wiring is sane.
func TestNewGlobal_Constructs(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	gm := NewGlobal(store, tmux.New())
	if gm == nil {
		t.Fatal("NewGlobal returned nil")
	}
	if cmd := gm.Init(); cmd == nil {
		t.Errorf("Init returned nil cmd; expected refresh dispatch")
	}
}

// TestGlobalModel_RendersWithoutPanic: View on a fresh GlobalModel (no
// rows loaded yet) renders an empty-state without crashing.
func TestGlobalModel_RendersWithoutPanic(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

	out := gm.View()
	if !strings.Contains(out, "canopy") {
		t.Errorf("View missing title: %q", out)
	}
}

// TestGlobalModel_HelpToggle: ? shows help; any next key dismisses.
func TestGlobalModel_HelpToggle(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

	model, _ := gm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	gm = model.(*GlobalModel)
	if !gm.showHelp {
		t.Errorf("? should set showHelp=true")
	}
	if !strings.Contains(gm.View(), "keybindings") {
		t.Errorf("help overlay missing 'keybindings' header: %q", gm.View())
	}
	// Any key dismisses.
	model, _ = gm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	gm = model.(*GlobalModel)
	if gm.showHelp {
		t.Errorf("any key should dismiss help")
	}
}

// TestGlobalModel_QuitKey: q returns tea.Quit.
func TestGlobalModel_QuitKey(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

	_, cmd := gm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q produced nil cmd; expected tea.Quit")
	}
	// Calling the cmd produces a tea.QuitMsg.
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("q cmd produced %T, want tea.QuitMsg", msg)
	}
}

// TestGlobalModel_ActivateOnStoppedSurfacesHint: pressing enter on a
// stopped row pipes a non-fatal error to the list's banner — no quit,
// no attach attempt. Verifies the v0.5 "global mode is read-only for
// stopped/broken/orphaned" boundary.
func TestGlobalModel_ActivateOnStoppedSurfacesHint(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "soft-fox",
		Status:      state.StatusStopped,
		TmuxSession: "cravd-soft-fox",
	}
	cmd := gm.activate(row)
	if cmd == nil {
		t.Fatal("activate returned nil cmd")
	}
	// Run the cmd to extract the message.
	msg := cmd()
	gotErr, ok := msg.(globalErrMsg)
	if !ok {
		t.Fatalf("activate(stopped) produced %T, want globalErrMsg", msg)
	}
	if !strings.Contains(gotErr.err.Error(), "stopped") {
		t.Errorf("hint missing 'stopped': %v", gotErr.err)
	}
	// Hint now points at the 'o' keybind (open the project) instead of
	// telling the user to cd manually.
	if !strings.Contains(gotErr.err.Error(), "`o`") {
		t.Errorf("hint missing 'o' keybind reference: %v", gotErr.err)
	}
}

// TestGlobalModel_AsPopup_ReadyRowFiresSwitchAndQuits: in popup mode,
// activating a ready row invokes the injected switch-client callback
// AND the resulting cmd produces tea.QuitMsg (popup closes).
//
// This is the load-bearing behavior for canopy popup. If it regresses,
// the popup either fails to switch sessions or stays open after pick.
func TestGlobalModel_AsPopup_ReadyRowFiresSwitchAndQuits(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	var called string
	gm := NewGlobal(store, tmux.New()).AsPopup(func(session string) error {
		called = session
		return nil
	}, "")

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "silent-falcon",
		Status:      state.StatusReady,
		TmuxSession: "cravd-silent-falcon",
	}
	cmd := gm.activate(row)
	if cmd == nil {
		t.Fatal("activate returned nil cmd in popup mode")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("popup activate on ready row produced %T, want tea.QuitMsg", msg)
	}
	if called != "cravd-silent-falcon" {
		t.Errorf("switch-client callback got session %q, want %q",
			called, "cravd-silent-falcon")
	}
}

// TestGlobalModel_AsPopup_AliveMainFiresSwitchAndQuits: alive main rows
// (the synthetic <project>-main session) also use switch-client + quit,
// matching default-mode behavior of attaching to alive main sessions.
func TestGlobalModel_AsPopup_AliveMainFiresSwitchAndQuits(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	var called string
	gm := NewGlobal(store, tmux.New()).AsPopup(func(session string) error {
		called = session
		return nil
	}, "")

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "(main)",
		IsMain:      true,
		Alive:       true,
		TmuxSession: "cravd-main",
	}
	cmd := gm.activate(row)
	if cmd == nil {
		t.Fatal("activate returned nil cmd in popup mode for alive main")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("popup activate on alive main produced %T, want tea.QuitMsg", msg)
	}
	if called != "cravd-main" {
		t.Errorf("switch-client callback got session %q, want %q",
			called, "cravd-main")
	}
}

// TestGlobalModel_AsPopup_StoppedRowSurfacesHintNotQuit: non-alive rows
// in popup mode surface the same hints as default mode (popup stays open
// so the user can read the hint and pick another row). Specifically NOT
// QuitMsg.
func TestGlobalModel_AsPopup_StoppedRowSurfacesHintNotQuit(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	switchCalled := false
	gm := NewGlobal(store, tmux.New()).AsPopup(func(session string) error {
		switchCalled = true
		return nil
	}, "")

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "soft-fox",
		Status:      state.StatusStopped,
		TmuxSession: "cravd-soft-fox",
	}
	cmd := gm.activate(row)
	if cmd == nil {
		t.Fatal("activate returned nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); ok {
		t.Errorf("popup activate on stopped row produced QuitMsg; expected hint (popup must stay open)")
	}
	if _, ok := msg.(globalErrMsg); !ok {
		t.Errorf("popup activate on stopped row produced %T, want globalErrMsg hint", msg)
	}
	if switchCalled {
		t.Errorf("switch-client callback should NOT fire on stopped row")
	}
}

// TestGlobalModel_AsPopup_SwitchFailureStillQuits: even when the
// switch-client callback returns an error, the model still quits (popup
// closes). tmux's own error message reaches the user via the parent
// client; a popup hanging open on error is worse UX than collapsing.
func TestGlobalModel_AsPopup_SwitchFailureStillQuits(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(func(session string) error {
		return fmt.Errorf("simulated switch-client failure")
	}, "")

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "silent-falcon",
		Status:      state.StatusReady,
		TmuxSession: "cravd-silent-falcon",
	}
	cmd := gm.activate(row)
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("popup activate with switch failure produced %T, want tea.QuitMsg", msg)
	}
}

// TestPopupHint_StoppedRowPointsAtShellAction: in popup mode, a stopped
// row's hint must NOT mention `o` (which is broken in popup) and MUST
// suggest a shell-based recovery action.
//
// IRON RULE: regression guard for the v0.7.1 user-reported bug where
// popup pressing `o` printed "open project at /path: exit status 1".
func TestPopupHint_StoppedRowPointsAtShellAction(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(
		func(string) error { return nil },
		"",
	)

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "soft-fox",
		Status:      state.StatusStopped,
		TmuxSession: "cravd-soft-fox",
	}
	cmd := gm.activate(row)
	msg := cmd()
	got, ok := msg.(globalErrMsg)
	if !ok {
		t.Fatalf("popup activate(stopped): got %T, want globalErrMsg", msg)
	}
	hint := got.err.Error()

	// MUST NOT reference `o` (the broken-in-popup recovery key).
	if strings.Contains(hint, "`o`") || strings.Contains(hint, "press `o`") {
		t.Errorf("popup hint references `o` (broken in popup): %s", hint)
	}
	// MUST suggest the shell recovery: `canopy switch <name>`.
	if !strings.Contains(hint, "canopy switch") {
		t.Errorf("popup hint missing `canopy switch` recovery: %s", hint)
	}
	if !strings.Contains(hint, "soft-fox") {
		t.Errorf("popup hint missing workspace name: %s", hint)
	}
}

// TestPopupHint_BrokenRowPointsAtRetry: broken rows hint at `canopy retry`.
func TestPopupHint_BrokenRowPointsAtRetry(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(
		func(string) error { return nil },
		"",
	)
	row := state.GlobalRow{
		Project: "cravd", Name: "broken-thing", Status: state.StatusBroken,
		TmuxSession: "cravd-broken-thing",
	}
	msg := gm.activate(row)()
	got := msg.(globalErrMsg).err.Error()
	if !strings.Contains(got, "canopy retry") {
		t.Errorf("broken hint should suggest `canopy retry`: %s", got)
	}
}

// TestPopupHint_OrphanedRowPointsAtRm: orphaned rows hint at `canopy rm`.
func TestPopupHint_OrphanedRowPointsAtRm(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(
		func(string) error { return nil },
		"",
	)
	row := state.GlobalRow{
		Project: "cravd", Name: "orphan", Status: state.StatusOrphaned,
		TmuxSession: "cravd-orphan",
	}
	msg := gm.activate(row)()
	got := msg.(globalErrMsg).err.Error()
	if !strings.Contains(got, "canopy rm") {
		t.Errorf("orphaned hint should suggest `canopy rm`: %s", got)
	}
}

// TestGoToProjectEnv_AllowsNestedTmux: the env passed to a spawned
// canopy must include CANOPY_ALLOW_NESTED=1, otherwise pressing `o`
// from inside any tmux session (popup mode OR any default-mode
// invocation that's inside tmux) hits the inner canopy's
// nested-tmux guard and returns exit 1. Regression guard for the
// user-reported 2026-04-29 popup `o` bug.
func TestGoToProjectEnv_AllowsNestedTmux(t *testing.T) {
	env := goToProjectEnv()

	want := map[string]bool{
		"CANOPY_FROM_GLOBAL=1": false,
		"CANOPY_ALLOW_NESTED=1": false,
	}
	for _, kv := range env {
		if _, ok := want[kv]; ok {
			want[kv] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("goToProjectEnv missing %q\nfull env: %v", k, env)
		}
	}
}

// TestPopupGoToProject_SetsFromPopupEnv: in popup mode (popupSwitchClient
// non-nil), goToProject's env passed to the spawned canopy includes
// CANOPY_FROM_POPUP=1. The inner project TUI uses that env var to
// signal "I attached on popup's behalf" via exit code 7. Without this,
// attaches from project TUI in popup leave the popup hanging open.
//
// We can't observe tea.ExecProcess args directly without running the
// program. Instead, exercise the env-construction logic by calling
// the helper-build path and inspecting the constructed cmd.Env. This
// requires factoring the env build out of goToProject; for now, we
// rely on the contract that goToProjectEnv() + ", CANOPY_FROM_POPUP=1"
// composes correctly. The cleaner factoring is a v0.8 polish.
func TestPopupGoToProject_SetsFromPopupEnv(t *testing.T) {
	// Sanity: goToProjectEnv stays the same; popup mode adds one more
	// var on top. If the helper ever inlines the popup var, this test
	// catches the change.
	base := goToProjectEnv()
	for _, kv := range base {
		if kv == "CANOPY_FROM_POPUP=1" {
			t.Errorf("goToProjectEnv leaked CANOPY_FROM_POPUP=1; should be added by goToProject only in popup mode\nenv: %v", base)
		}
	}
}

// TestPopupSubseqMatcher covers the fuzzy subsequence matcher used
// by the popup's / search filter.
func TestPopupSubseqMatcher(t *testing.T) {
	cases := []struct {
		haystack, needle string
		want             bool
	}{
		{"silent-falcon", "sf", true},
		{"silent-falcon", "sln", true},
		{"silent-falcon", "silent", true},
		{"silent-falcon", "xyz", false},
		{"silent-falcon", "", true},
		{"", "x", false},
		{"", "", true},
		{"misty-aspen", "ma", true},
	}
	for _, tc := range cases {
		t.Run(tc.haystack+"_"+tc.needle, func(t *testing.T) {
			if got := isSubseq(tc.haystack, tc.needle); got != tc.want {
				t.Errorf("isSubseq(%q, %q) = %v; want %v",
					tc.haystack, tc.needle, got, tc.want)
			}
		})
	}
}

// TestPopupTabFilterLocal: with popupTabLocal and a current project,
// only rows from that project survive.
func TestPopupTabFilterLocal(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(
		func(string) error { return nil },
		"/a/cravd",
	)
	gm.popupAllRows = []state.GlobalRow{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "silent-falcon"},
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "misty-aspen"},
		{Project: "canopy", ProjectRoot: "/b/canopy", Name: "bold-otter"},
	}
	got := gm.filteredPopupRows()
	if len(got) != 2 {
		t.Fatalf("local filter: got %d rows, want 2", len(got))
	}
	for _, r := range got {
		if r.ProjectRoot != "/a/cravd" {
			t.Errorf("non-cravd row leaked: %+v", r)
		}
	}
}

// TestPopupTabFilterGlobal: popupTabGlobal yields all rows.
func TestPopupTabFilterGlobal(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(
		func(string) error { return nil },
		"/a/cravd",
	)
	gm.popupTab = popupTabGlobal
	gm.popupAllRows = []state.GlobalRow{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "silent-falcon"},
		{Project: "canopy", ProjectRoot: "/b/canopy", Name: "bold-otter"},
	}
	if got := gm.filteredPopupRows(); len(got) != 2 {
		t.Errorf("global filter dropped rows: got %d, want 2", len(got))
	}
}

// TestPopupTabFilterLocal_noProject: AsPopup("") starts on Global.
func TestPopupTabFilterLocal_noProject(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(
		func(string) error { return nil },
		"",
	)
	if gm.popupTab != popupTabGlobal {
		t.Errorf("AsPopup(\"\"): popupTab = %d, want popupTabGlobal", gm.popupTab)
	}
}

// TestPopupSearchFilter: search query reduces rows by subseq match
// against name, project, OR branch.
func TestPopupSearchFilter(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(
		func(string) error { return nil },
		"",
	)
	gm.popupAllRows = []state.GlobalRow{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "silent-falcon", Branch: "feat/login"},
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "misty-aspen", Branch: "fix/timezone-bug"},
		{Project: "canopy", ProjectRoot: "/b/canopy", Name: "bold-otter", Branch: "main"},
	}

	gm.popupSearchQuery = "sf"
	if got := gm.filteredPopupRows(); len(got) != 1 || got[0].Name != "silent-falcon" {
		t.Errorf("search 'sf' (name match): got %+v, want [silent-falcon]", got)
	}

	gm.popupSearchQuery = "cra"
	if got := gm.filteredPopupRows(); len(got) != 2 {
		t.Errorf("search 'cra' (project match): got %d, want 2", len(got))
	}

	// Branch-only match: "tz" subseq of "fix/timezone-bug" → only misty.
	gm.popupSearchQuery = "tz"
	got := gm.filteredPopupRows()
	if len(got) != 1 || got[0].Name != "misty-aspen" {
		t.Errorf("search 'tz' (branch match): got %+v, want [misty-aspen]", got)
	}

	// Another branch-only match: "login" full-word in branch.
	gm.popupSearchQuery = "login"
	got = gm.filteredPopupRows()
	if len(got) != 1 || got[0].Name != "silent-falcon" {
		t.Errorf("search 'login' (branch match): got %+v, want [silent-falcon]", got)
	}

	gm.popupSearchQuery = ""
	if got := gm.filteredPopupRows(); len(got) != 3 {
		t.Errorf("empty query: got %d, want 3", len(got))
	}
}

// TestPopupTabKeyCyclesTabs: Tab cycles local↔global.
func TestPopupTabKeyCyclesTabs(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(
		func(string) error { return nil },
		"/a/cravd",
	)
	if gm.popupTab != popupTabLocal {
		t.Fatalf("setup: tab = %d, want popupTabLocal", gm.popupTab)
	}
	if _, _, h := gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyTab}); !h {
		t.Fatal("Tab not handled")
	}
	if gm.popupTab != popupTabGlobal {
		t.Errorf("after Tab: tab = %d, want Global", gm.popupTab)
	}
	gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyTab})
	if gm.popupTab != popupTabLocal {
		t.Errorf("after Tab×2: tab = %d, want Local", gm.popupTab)
	}
}

// TestPopupSearchModeKeystrokes: / enters search; runes accumulate;
// Backspace deletes; Enter exits keeping query; Esc clears+exits.
func TestPopupSearchModeKeystrokes(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()).AsPopup(
		func(string) error { return nil },
		"",
	)

	gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !gm.popupSearchMode {
		t.Fatal("/ should enter search mode")
	}

	gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if gm.popupSearchQuery != "sf" {
		t.Errorf("typing 'sf': query = %q, want \"sf\"", gm.popupSearchQuery)
	}

	gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if gm.popupSearchQuery != "s" {
		t.Errorf("Backspace: query = %q, want \"s\"", gm.popupSearchQuery)
	}

	gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyEnter})
	if gm.popupSearchMode || gm.popupSearchQuery != "s" {
		t.Errorf("Enter: mode=%v query=%q; want false/\"s\"", gm.popupSearchMode, gm.popupSearchQuery)
	}

	gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	gm.handlePopupKey(tea.KeyMsg{Type: tea.KeyEsc})
	if gm.popupSearchMode || gm.popupSearchQuery != "" {
		t.Errorf("Esc: mode=%v query=%q; want false/\"\"", gm.popupSearchMode, gm.popupSearchQuery)
	}
}

// TestPopupKeysIgnoredInDefaultMode: tab and / must NOT activate
// popup-mode behavior when popupSwitchClient is nil. Verifies popup
// state stays at zero-values after pressing those keys.
func TestPopupKeysIgnoredInDefaultMode(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New()) // no AsPopup

	gm.Update(tea.KeyMsg{Type: tea.KeyTab})
	if gm.popupTab != popupTabLocal { // zero value
		t.Errorf("default mode: Tab mutated popupTab to %d", gm.popupTab)
	}

	gm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if gm.popupSearchMode {
		t.Errorf("default mode: / activated search")
	}
}

// TestGlobalModel_GoToProject_NonEmptyRoot: row with a populated
// ProjectRoot produces a non-nil cmd (tea.ExecProcess against the canopy
// binary). Smoke check that the dispatch happens.
func TestGlobalModel_GoToProject_NonEmptyRoot(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/home/avi/Work/cravd",
		Name:        "soft-fox",
		Status:      state.StatusReady,
	}
	cmd := gm.goToProject(row)
	if cmd == nil {
		t.Errorf("goToProject(populated row) returned nil cmd")
	}
}

// TestGlobalModel_GoToProject_NoFallbackAvailable: a v1-unmigrated row
// with no canonical ProjectRoot AND no worktree Path (e.g. a main-only
// row whose siblings also have no Path) surfaces a clear migration hint.
func TestGlobalModel_GoToProject_NoFallbackAvailable(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

	// Unmigrated row: ProjectRoot is just a basename, no Path. No siblings.
	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "cravd", // basename, not absolute
		IsMain:      true,
		Name:        "(main)",
		Status:      "main",
	}
	cmd := gm.goToProject(row)
	if cmd == nil {
		t.Fatal("expected error cmd, got nil")
	}
	msg := cmd()
	gotErr, ok := msg.(globalErrMsg)
	if !ok {
		t.Fatalf("expected globalErrMsg, got %T", msg)
	}
	if !strings.Contains(gotErr.err.Error(), "no canonical root path on file") {
		t.Errorf("error message should explain the migration path: %v", gotErr.err)
	}
}

// TestGlobalModel_GoToProject_DerivesFromWorktreePath: when ProjectRoot is
// just a basename but the row has a worktree Path inside a real git repo,
// resolveProjectRoot derives the source repo via `git rev-parse
// --git-common-dir`. Verifies the v1-unmigrated escape hatch works without
// a registry.
func TestGlobalModel_GoToProject_DerivesFromWorktreePath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// Create a fake source repo + worktree under it.
	source := t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=main", source},
		{"-C", source, "config", "user.email", "t@e"},
		{"-C", source, "config", "user.name", "t"},
		{"-C", source, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	worktree := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", source, "worktree", "add", worktree).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}

	row := state.GlobalRow{
		Project:     filepath.Base(source),
		ProjectRoot: filepath.Base(source), // unmigrated basename
		Name:        "wt",
		Status:      state.StatusReady,
		Path:        worktree,
	}

	got, err := resolveProjectRoot(row, []state.GlobalRow{row})
	if err != nil {
		t.Fatalf("resolveProjectRoot: %v", err)
	}
	// EvalSymlinks may differ from the raw t.TempDir() on macOS (/var vs
	// /private/var); compare basenames as the cheap correctness signal.
	if filepath.Base(got) != filepath.Base(source) {
		t.Errorf("resolved root basename = %q, want %q", filepath.Base(got), filepath.Base(source))
	}
}

// TestGlobalModel_RowsLoadedDispatchesHintLoaders: receiving a
// globalRowsLoadedMsg renders the rows immediately and returns a
// follow-up tea.Cmd that fires off the per-row hint loaders. This is
// the core of the two-phase refresh — rows MUST be visible before any
// gh shellouts complete.
func TestGlobalModel_RowsLoadedDispatchesHintLoaders(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

	rows := []state.GlobalRow{
		{Project: "canopy", Name: "soft-fox", Path: "/tmp/wt-1", ProjectRoot: "/a/canopy"},
		{Project: "canopy", Name: "ancient-hornet", Path: "/tmp/wt-2", ProjectRoot: "/a/canopy"},
	}
	model, cmd := gm.Update(globalRowsLoadedMsg{rows: rows})
	gm = model.(*GlobalModel)

	// Rows should be installed in the embedded list right away.
	if len(gm.list.Rows()) != 2 {
		t.Errorf("rows not installed before hint loaders dispatched; got %d", len(gm.list.Rows()))
	}
	// The follow-up cmd is the tea.Batch of per-row hint loaders.
	if cmd == nil {
		t.Errorf("expected hint-loader batch cmd; got nil — rows would render with no badges, ever")
	}
}

// TestGlobalModel_RowsLoadedSkipsHintLoadersOnError: an error from the
// state load short-circuits hint dispatch (no rows to decorate).
func TestGlobalModel_RowsLoadedSkipsHintLoadersOnError(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

	_, cmd := gm.Update(globalRowsLoadedMsg{err: fmt.Errorf("boom")})
	if cmd != nil {
		t.Errorf("error path should not dispatch hint loaders; got %T", cmd)
	}
}

// TestGlobalModel_RowHintsMergeIntoList: a per-row hint result merges
// into the embedded list via UpdateRowHints. Verifies the late-arrival
// merge path that the two-phase refresh depends on.
func TestGlobalModel_RowHintsMergeIntoList(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	gm := NewGlobal(store, tmux.New())

	rows := []state.GlobalRow{
		{Project: "canopy", Name: "soft-fox", Path: "/tmp/wt"},
	}
	model, _ := gm.Update(globalRowsLoadedMsg{rows: rows})
	gm = model.(*GlobalModel)

	model, _ = gm.Update(globalRowHintsMsg{
		project: "canopy",
		name:    "soft-fox",
		hints:   []state.Hint{{Kind: "shipped"}},
	})
	gm = model.(*GlobalModel)

	got := gm.list.Rows()
	if len(got) != 1 || len(got[0].Hints) != 1 {
		t.Errorf("hints did not merge into row; got rows=%+v", got)
	}
}

// TestGlobalModel_ActivateOnReadyAttempts: pressing enter on a ready row
// returns a non-nil cmd (tea.ExecProcess). We don't actually run the
// attach in tests — that would launch tmux — but the cmd shape is the
// signal that the dispatch happened.
func TestGlobalModel_ActivateOnReadyAttempts(t *testing.T) {
	store, _ := state.NewStore(t.TempDir())
	// Use a test tmux client so we don't touch the user's tmux server.
	gm := NewGlobal(store, tmux.WithSocket("canopy-test"))

	row := state.GlobalRow{
		Project:     "cravd",
		ProjectRoot: "/a/cravd",
		Name:        "soft-fox",
		Status:      state.StatusReady,
		TmuxSession: "cravd-soft-fox",
	}
	cmd := gm.activate(row)
	if cmd == nil {
		t.Errorf("activate(ready) returned nil cmd; expected tea.ExecProcess")
	}
}
