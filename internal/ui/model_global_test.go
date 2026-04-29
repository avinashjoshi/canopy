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
