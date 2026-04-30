package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/oncactus/canopy/internal/tmux"
)

// teaKeyMsg is a tiny alias to keep the test file readable.
type teaKeyMsg = tea.KeyMsg

// teaKeyMsgFromString builds a tea.KeyMsg whose String() matches s.
// Bubbletea's key matching uses the .String() result for non-rune keys
// and Type+Runes for rune keys.
func teaKeyMsgFromString(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// TestTailFile_BasicTail: read the last N lines of a file with more
// than N lines. Bound is honored.
func TestTailFile_BasicTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	lines := []string{"a", "b", "c", "d", "e"}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(path, 3)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	want := "c\nd\ne\n"
	if got != want {
		t.Errorf("tailFile last-3 = %q; want %q", got, want)
	}
}

// TestTailFile_FewerThanN: file shorter than n returns everything.
func TestTailFile_FewerThanN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	os.WriteFile(path, []byte("only-line\n"), 0o644)
	got, err := tailFile(path, 10)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if got != "only-line\n" {
		t.Errorf("tailFile = %q; want %q", got, "only-line\n")
	}
}

// TestTailFile_Missing_ReturnsIsNotExist: the drawer relies on
// os.IsNotExist to distinguish "no log captured yet" from "read
// failed" — those are different UX messages.
func TestTailFile_Missing_ReturnsIsNotExist(t *testing.T) {
	_, err := tailFile("/nonexistent/path/that/does/not/exist", 10)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected IsNotExist error; got %v", err)
	}
}

// TestReadBoundedFile_TruncatesHead: when file is larger than max,
// readBoundedFile keeps the LAST max bytes (the most recent setup
// output, which is where the failure context lives).
func TestReadBoundedFile_TruncatesHead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setup.log")
	// 1000 bytes total, last 100 should be returned.
	contents := make([]byte, 1000)
	for i := range contents {
		contents[i] = byte('A' + (i % 26))
	}
	os.WriteFile(path, contents, 0o644)

	got, err := readBoundedFile(path, 100)
	if err != nil {
		t.Fatalf("readBoundedFile: %v", err)
	}
	if len(got) != 100 {
		t.Errorf("returned %d bytes; want 100", len(got))
	}
	if got[0] != contents[900] {
		t.Errorf("returned bytes are wrong slice — got starts with %c, want %c (kept the head, not the tail)",
			got[0], contents[900])
	}
}

// TestReadBoundedFile_SmallFileWholeRead: a file smaller than max is
// returned in full.
func TestReadBoundedFile_SmallFileWholeRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.log")
	os.WriteFile(path, []byte("hi"), 0o644)
	got, err := readBoundedFile(path, 1024)
	if err != nil {
		t.Fatalf("readBoundedFile: %v", err)
	}
	if string(got) != "hi" {
		t.Errorf("readBoundedFile = %q; want %q", got, "hi")
	}
}

// TestRenderPaneInfos_RendersTreeOrder: renderPaneInfos prints the
// process tree in the order PaneInfos provides. The contract is that
// PaneInfos (via psTree) returns Tree pre-sorted by RSS desc — the
// rendering layer is a faithful printer, not a re-sorter.
func TestRenderPaneInfos_RendersTreeOrder(t *testing.T) {
	infos := []tmux.PaneInfo{
		{
			Index: 0,
			Title: "claude",
			Tree: []tmux.ProcInfo{
				{PID: 101, RSS: 1024 * 1024 * 800, CPU: "20.0", Comm: "claude"},
				{PID: 100, RSS: 1024 * 1024 * 200, CPU: "5.0", Comm: "shell"},
			},
			TotalRSS: 1024 * 1024 * 1000,
		},
	}
	out := renderPaneInfos(infos)
	// Match by comm name to avoid false positives in totals like "1000M"
	// containing the literal "100".
	idxClaude := strings.Index(out, "  claude\n")
	idxShell := strings.Index(out, "  shell\n")
	if idxClaude < 0 || idxShell < 0 {
		t.Fatalf("expected both proc rows in output; got %q", out)
	}
	if idxClaude >= idxShell {
		t.Errorf("expected claude proc row before shell proc row (input order); got %q", out)
	}
	if !strings.Contains(out, "1000M") {
		t.Errorf("expected total RSS 1000M in output; got %q", out)
	}
}

// TestRenderPaneInfos_EmptyMessage: a session with no panes (rare but
// possible) shouldn't render an empty block.
func TestRenderPaneInfos_EmptyMessage(t *testing.T) {
	out := renderPaneInfos(nil)
	if !strings.Contains(out, "no panes") {
		t.Errorf("empty PaneInfos rendered = %q; want a 'no panes' line", out)
	}
}

// TestActionInspect_OpensDrawerForMainRow: i on a (main) row opens
// the drawer just like for workspace rows. Process tree / env / status
// are useful for main sessions too — claude eating 4GB in your main
// session is exactly what you'd want to see. Setup-log and bare-attach
// degrade gracefully (they're workspace-only by nature, surfaced as
// N/A hints in the drawer view).
//
// This was a refusal in the initial drawer ship; relaxed 2026-04-29
// after the user pointed out main rows have process trees worth
// inspecting too.
func TestActionInspect_OpensDrawerForMainRow(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{IsMain: true, Project: "test-project", Name: "(main)", TmuxSession: "test-project-main", Alive: true},
	})

	model, _ := actionInspect(m, teaKeyMsg{})
	mm := model.(*Model)
	if mm.mode != drawerMode {
		t.Errorf("mode = %v; want drawerMode (main rows should open the drawer)", mm.mode)
	}
	if mm.err != nil {
		t.Errorf("unexpected err for main-row inspect: %v", mm.err)
	}
	if !mm.drawerRow.IsMain {
		t.Error("drawerRow should be the main row")
	}
}

// TestHandleDrawerKey_BWorksForMainRow: bare attach `b` opens a
// one-pane shell at the project root for main rows (was refused
// originally; relaxed 2026-04-29 once the `is-a-workspace for
// lifecycle / is-not for identity` distinction was made explicit).
//
// We can't fully assert the bareAttachMainCmd executes without a
// real tmux server (the cmd is dispatched async), but we can assert
// the handler doesn't refuse and that drawer state stays clean.
func TestHandleDrawerKey_BWorksForMainRow(t *testing.T) {
	m := newTestModel(false)
	m.mode = drawerMode
	m.drawerRow = Row{
		IsMain:      true,
		Name:        "(main)",
		Project:     "test-project",
		ProjectRoot: "/tmp/test-project",
		TmuxSession: "test-project-main",
		Alive:       true,
	}

	_, cmd := m.handleDrawerKey(teaKeyMsgFromString("b"))
	if m.drawerErr != nil {
		t.Errorf("unexpected drawerErr for `b` on main row: %v", m.drawerErr)
	}
	if m.mode != drawerMode {
		t.Errorf("mode = %v; want drawerMode (drawer should stay open while bare attach dispatches)", m.mode)
	}
	if cmd == nil {
		t.Error("expected a tea.Cmd dispatched for bare attach on main; got nil")
	}
}

// TestActionInspect_OpensDrawer: i on a workspace row enters drawerMode
// and snapshots the row.
func TestActionInspect_OpensDrawer(t *testing.T) {
	m := newTestModel(false)
	row := Row{
		ProjectRoot: "/tmp/test-project", Project: "test-project", Name: "ws",
		TmuxSession: "test-ws", Alive: true,
	}
	m.setTestRows([]Row{row})

	_, _ = actionInspect(m, teaKeyMsg{})
	if m.mode != drawerMode {
		t.Errorf("mode = %v; want drawerMode", m.mode)
	}
	if m.drawerRow.Name != "ws" {
		t.Errorf("drawerRow.Name = %q; want %q", m.drawerRow.Name, "ws")
	}
}

// TestHandleDrawerKey_EscClosesDrawer: Esc resets the drawer state and
// transitions back to listMode.
func TestHandleDrawerKey_EscClosesDrawer(t *testing.T) {
	m := newTestModel(false)
	m.mode = drawerMode
	m.drawerRow = Row{Name: "ws"}
	m.drawerProcInfo = "stale"

	_, _ = m.handleDrawerKey(teaKeyMsgFromString("esc"))

	if m.mode != listMode {
		t.Errorf("mode after Esc = %v; want listMode", m.mode)
	}
	if m.drawerRow.Name != "" {
		t.Errorf("drawerRow not cleared after Esc")
	}
	if m.drawerProcInfo != "" {
		t.Error("drawerProcInfo not cleared after Esc")
	}
}

// TestHandleDrawerKey_QClosesDrawer: lowercase q does the same as Esc.
func TestHandleDrawerKey_QClosesDrawer(t *testing.T) {
	m := newTestModel(false)
	m.mode = drawerMode
	_, _ = m.handleDrawerKey(teaKeyMsgFromString("q"))
	if m.mode != listMode {
		t.Errorf("mode after q = %v; want listMode", m.mode)
	}
}

// TestHumanBytes_Scales: spot-check the unit boundaries.
func TestHumanBytes_Scales(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{500, "500B"},
		{1024, "1K"},
		{1024 * 1024, "1M"},
		{1024 * 1024 * 1024, "1.0G"},
		{2*1024*1024*1024 + 512*1024*1024, "2.5G"},
	}
	for _, tc := range cases {
		got := humanBytes(tc.in)
		if got != tc.want {
			t.Errorf("humanBytes(%d) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
