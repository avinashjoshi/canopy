package projectlist

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/oncactus/canopy/internal/state"
)

// sampleRows builds a small deterministic []state.GlobalRow for tests
// that don't care about the specific contents.
func sampleRows() []state.GlobalRow {
	return []state.GlobalRow{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "bold-falcon", Branch: "feat/x", Status: state.StatusReady, Port: 3000, TmuxSession: "cravd-bold-falcon", Alive: true},
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "soft-fox", Branch: "feat/y", Status: state.StatusStopped, Port: 3010, TmuxSession: "cravd-soft-fox", Alive: false},
		{Project: "canopy", ProjectRoot: "/b/canopy", Name: "ancient-hornet", Branch: "main", Status: state.StatusReady, Port: 4000, TmuxSession: "canopy-ancient-hornet", Alive: true},
	}
}

func key(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// TestNew_EmptyRendersWithoutPanic: a freshly-constructed Model with no
// rows must render — empty state copy, no panic.
func TestNew_EmptyRendersWithoutPanic(t *testing.T) {
	m := New(Options{})
	out := m.View()
	if out == "" {
		t.Errorf("View() empty string")
	}
	if !strings.Contains(out, "No canopy projects") {
		t.Errorf("expected empty-state hint; got %q", out)
	}
}

// TestSetRows_ClampsCursor: rows shrink to fewer than the cursor; cursor
// should clamp.
func TestSetRows_ClampsCursor(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows()) // 3 rows
	// Move cursor to last row.
	m, _ = m.Update(key("end"))
	if m.cursor != 2 {
		t.Fatalf("cursor after end = %d, want 2", m.cursor)
	}
	// Shrink rows to 1.
	m.SetRows(sampleRows()[:1])
	if m.cursor != 0 {
		t.Errorf("cursor after shrink = %d, want clamped to 0", m.cursor)
	}
}

// TestNav_JKAndArrows: ↑/↓/j/k all move cursor, both within bounds.
func TestNav_JKAndArrows(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())

	// Down twice via j → cursor=2.
	m, _ = m.Update(key("j"))
	m, _ = m.Update(key("j"))
	if m.cursor != 2 {
		t.Errorf("cursor after two j = %d, want 2", m.cursor)
	}
	// Past-end stays at last row.
	m, _ = m.Update(key("j"))
	if m.cursor != 2 {
		t.Errorf("cursor past-end = %d, want 2 (clamped)", m.cursor)
	}
	// Up via k → cursor=1.
	m, _ = m.Update(key("k"))
	if m.cursor != 1 {
		t.Errorf("cursor after k = %d, want 1", m.cursor)
	}
	// Above-zero stays at 0.
	m, _ = m.Update(key("k"))
	m, _ = m.Update(key("k"))
	if m.cursor != 0 {
		t.Errorf("cursor above-zero = %d, want 0", m.cursor)
	}
	// Arrow keys also work.
	m, _ = m.Update(key("down"))
	if m.cursor != 1 {
		t.Errorf("cursor after down arrow = %d, want 1", m.cursor)
	}
	m, _ = m.Update(key("up"))
	if m.cursor != 0 {
		t.Errorf("cursor after up arrow = %d, want 0", m.cursor)
	}
}

// TestNav_GAndHomeEnd: g/G/home/end jump to first/last.
func TestNav_GAndHomeEnd(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())

	m, _ = m.Update(key("end"))
	if m.cursor != 2 {
		t.Errorf("end → cursor %d, want 2", m.cursor)
	}
	m, _ = m.Update(key("home"))
	if m.cursor != 0 {
		t.Errorf("home → cursor %d, want 0", m.cursor)
	}
	m, _ = m.Update(key("G"))
	if m.cursor != 2 {
		t.Errorf("G → cursor %d, want 2", m.cursor)
	}
	m, _ = m.Update(key("g"))
	if m.cursor != 0 {
		t.Errorf("g → cursor %d, want 0", m.cursor)
	}
}

// TestEnter_InvokesOnActivate: enter calls OnActivate with the cursor's
// row and returns the resulting tea.Cmd.
func TestEnter_InvokesOnActivate(t *testing.T) {
	var captured state.GlobalRow
	called := false
	cmd := func() tea.Msg { return "activated" }
	m := New(Options{
		OnActivate: func(r state.GlobalRow) tea.Cmd {
			captured = r
			called = true
			return cmd
		},
	})
	m.SetRows(sampleRows())

	// Move to row 1 (cravd / soft-fox), then enter.
	m, _ = m.Update(key("j"))
	_, gotCmd := m.Update(key("enter"))

	if !called {
		t.Fatalf("OnActivate not called")
	}
	if captured.Name != "soft-fox" {
		t.Errorf("OnActivate received row %q, want soft-fox", captured.Name)
	}
	if gotCmd == nil {
		t.Errorf("enter returned nil cmd, want forwarded OnActivate cmd")
	}
}

// TestEnter_NoCallbackIsNoOp: enter without OnActivate set is a no-op,
// no panic.
func TestEnter_NoCallbackIsNoOp(t *testing.T) {
	m := New(Options{}) // no OnActivate
	m.SetRows(sampleRows())

	_, cmd := m.Update(key("enter"))
	if cmd != nil {
		t.Errorf("enter without OnActivate returned non-nil cmd")
	}
}

// TestEnter_EmptyListIsNoOp: enter on an empty list is a no-op even
// when OnActivate is set (no panic, no callback fired).
func TestEnter_EmptyListIsNoOp(t *testing.T) {
	called := false
	m := New(Options{
		OnActivate: func(r state.GlobalRow) tea.Cmd {
			called = true
			return nil
		},
	})
	// no SetRows — list stays empty.

	_, _ = m.Update(key("enter"))
	if called {
		t.Errorf("OnActivate fired on empty list")
	}
}

// TestGoToProject_InvokesCallback: 'c' calls OnGoToProject with the row
// at the cursor and forwards its tea.Cmd.
func TestGoToProject_InvokesCallback(t *testing.T) {
	var captured state.GlobalRow
	called := false
	m := New(Options{
		OnGoToProject: func(r state.GlobalRow) tea.Cmd {
			captured = r
			called = true
			return func() tea.Msg { return "go-to" }
		},
	})
	m.SetRows(sampleRows())
	// Move to row 2 (canopy / ancient-hornet) and press c.
	m, _ = m.Update(key("end"))
	_, gotCmd := m.Update(key("o"))

	if !called {
		t.Fatalf("OnGoToProject not invoked")
	}
	if captured.Name != "ancient-hornet" {
		t.Errorf("captured row %q, want ancient-hornet", captured.Name)
	}
	if gotCmd == nil {
		t.Errorf("c returned nil cmd; expected forwarded callback cmd")
	}
}

// TestGoToProject_NoCallback: o without OnGoToProject is a no-op.
func TestGoToProject_NoCallback(t *testing.T) {
	m := New(Options{}) // no OnGoToProject
	m.SetRows(sampleRows())

	_, cmd := m.Update(key("o"))
	if cmd != nil {
		t.Errorf("o without OnGoToProject returned non-nil cmd")
	}
}

// TestRender_HintBadges: rows with active hints show appropriate badges.
func TestRender_HintBadges(t *testing.T) {
	rows := []state.GlobalRow{
		{
			Project: "canopy", ProjectRoot: "/a/canopy",
			Name: "ancient-hornet", Branch: "ancient-hornet",
			Status: state.StatusReady, Port: 40010,
			Hints: []state.Hint{
				{Kind: "rename_suggested", Message: "rename me"},
			},
		},
		{
			Project: "cravd", ProjectRoot: "/a/cravd",
			Name: "shipped-feat", Branch: "feat/oauth",
			Status: state.StatusReady, Port: 41010,
			Hints: []state.Hint{
				{Kind: "shipped", Message: "ready to close"},
			},
		},
		{
			Project: "brain", ProjectRoot: "/a/brain",
			Name: "in-flight", Branch: "feat/x",
			Status: state.StatusReady, Port: 42010,
			Hints: []state.Hint{
				{Kind: "pr_status", Message: "PR #42 merged"},
			},
		},
	}
	m := New(Options{})
	m.SetRows(rows)
	out := m.View()

	// Each badge text should appear exactly once for its row. Badges
	// are styled via lipgloss but the literal text is preserved.
	for _, want := range []string{"↻ rename", "✓ shipped", "✓ PR"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing badge %q\nfull output:\n%s", want, out)
		}
	}
}

// TestUpdateRowHints_MergesIntoMatchingRow: late-arriving hint update
// finds the matching (project, name) row and replaces its Hints slice.
// Two-phase refresh relies on this — rows render first, hints catch up.
func TestUpdateRowHints_MergesIntoMatchingRow(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "canopy", Name: "soft-fox"},
		{Project: "canopy", Name: "ancient-hornet"},
		{Project: "cravd", Name: "soft-fox"}, // same name, different project
	})

	hints := []state.Hint{{Kind: "shipped", Message: "merged"}}
	m.UpdateRowHints("canopy", "soft-fox", hints)

	if len(m.rows[0].Hints) != 1 {
		t.Errorf("expected hints set on canopy/soft-fox; got %v", m.rows[0].Hints)
	}
	if len(m.rows[1].Hints) != 0 {
		t.Errorf("ancient-hornet hints should be untouched")
	}
	if len(m.rows[2].Hints) != 0 {
		t.Errorf("cravd/soft-fox hints should be untouched (same name, different project)")
	}
}

// TestUpdateRowHints_NoMatchIsSilent: a hint update for a row that no
// longer exists (concurrent rm dropped it) is a no-op, not a panic.
func TestUpdateRowHints_NoMatchIsSilent(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{Project: "canopy", Name: "soft-fox"}})
	// Before: original row has no hints.
	m.UpdateRowHints("canopy", "ghost-row", []state.Hint{{Kind: "shipped"}})
	if len(m.rows[0].Hints) != 0 {
		t.Errorf("unrelated update mutated existing row's hints")
	}
}

// TestRender_NoHintBadges: rows without hints render exactly as before
// (no badge column, no extra whitespace at the row's tail).
func TestRender_NoHintBadges(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows()) // sampleRows have no Hints set
	out := m.View()

	for _, badge := range []string{"↻ rename", "✓ shipped", "PR"} {
		if strings.Contains(out, badge) {
			t.Errorf("unexpected badge %q in row without hints:\n%s", badge, out)
		}
	}
}

// TestEnter_AlwaysRoutesToActivate: enter on every row — including
// shipped/PR-merged ones — fires OnActivate. Lifecycle hints decorate
// the row visually but never change enter's destination; close-out is
// a manual `canopy rm` step the user runs explicitly.
func TestEnter_AlwaysRoutesToActivate(t *testing.T) {
	cases := []struct {
		name  string
		hints []state.Hint
	}{
		{"no hints", nil},
		{"rename only", []state.Hint{{Kind: "rename_suggested"}}},
		{"shipped local", []state.Hint{{Kind: "shipped"}}},
		{"PR merged", []state.Hint{{Kind: "pr_status", Message: "PR #42 merged; ready to close workspace"}}},
		{"shipped + PR", []state.Hint{{Kind: "shipped"}, {Kind: "pr_status", Message: "PR #42 merged; ready to close workspace"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var activated state.GlobalRow
			m := New(Options{
				OnActivate: func(r state.GlobalRow) tea.Cmd {
					activated = r
					return nil
				},
			})
			m.SetRows([]state.GlobalRow{{
				Project: "canopy", Name: "soft-fox",
				Status: state.StatusReady,
				Hints:  tc.hints,
			}})
			m, _ = m.Update(key("enter"))
			if activated.Name != "soft-fox" {
				t.Errorf("OnActivate didn't fire (%s); got %q", tc.name, activated.Name)
			}
		})
	}
}

// TestRender_PRStatusSupersedesShipped: when a row has both shipped and
// pr_status hints, the badge column shows ONLY the PR-state badge —
// the local "shipped" fallback is suppressed because the PR is the
// authoritative signal.
func TestRender_PRStatusSupersedesShipped(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "canopy", Name: "soft-fox",
		Hints: []state.Hint{
			{Kind: "shipped"},
			{Kind: "pr_status", Message: "PR #42 merged; ready to close workspace"},
		},
	}})
	out := m.View()

	if !strings.Contains(out, "PR merged") {
		t.Errorf("PR merged badge missing:\n%s", out)
	}
	if strings.Contains(out, "✓ shipped (local)") {
		t.Errorf("local shipped badge should be hidden when PR is present:\n%s", out)
	}
}

// TestRender_PRStatusStateBadges: each PR state maps to a distinct
// human-readable badge. Renders the message-keyword decoder.
func TestRender_PRStatusStateBadges(t *testing.T) {
	cases := []struct {
		message  string
		wantText string
	}{
		{"PR #42 open; awaiting reviews", "PR open"},
		{"PR #42 open, approved; awaiting merge", "PR approved"},
		{"PR #42 open; changes requested", "PR changes"},
		{"PR #42 merged; ready to close workspace", "PR merged"},
		{"PR #42 closed without merging", "PR closed"},
	}
	for _, tc := range cases {
		t.Run(tc.wantText, func(t *testing.T) {
			m := New(Options{})
			m.SetRows([]state.GlobalRow{{
				Project: "canopy", Name: "soft-fox",
				Hints: []state.Hint{{Kind: "pr_status", Message: tc.message}},
			}})
			if !strings.Contains(m.View(), tc.wantText) {
				t.Errorf("badge missing %q for message %q:\n%s", tc.wantText, tc.message, m.View())
			}
		})
	}
}

// TestRender_ShippedFallbackWhenNoPR: a row with only the local "shipped"
// hint (no pr_status) shows the "✓ shipped (local)" fallback badge.
// Critical for purely-local-repo workflows where there's no GitHub PR.
func TestRender_ShippedFallbackWhenNoPR(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "canopy", Name: "soft-fox",
		Hints: []state.Hint{{Kind: "shipped"}},
	}})
	if !strings.Contains(m.View(), "✓ shipped (local)") {
		t.Errorf("shipped fallback badge missing:\n%s", m.View())
	}
}

// TestRender_GroupsByProject: consecutive rows with the same Project share
// one header line; a project change emits a new header. Verifies the
// grouped layout doesn't repeat project names per row.
func TestRender_GroupsByProject(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	out := m.View()

	// Project headers appear exactly once per project even when there are
	// multiple rows. cravd has 2 rows; the literal "cravd" should appear
	// only on its header line in the rendered output (status cells, etc.,
	// don't echo the project basename).
	cravdCount := strings.Count(out, "cravd")
	if cravdCount != 1 {
		t.Errorf("rendered output mentions 'cravd' %d times, want exactly 1 (header only)\noutput:\n%s",
			cravdCount, out)
	}
	// canopy has 1 row; same expectation.
	canopyCount := strings.Count(out, "canopy")
	if canopyCount != 1 {
		t.Errorf("rendered output mentions 'canopy' %d times, want exactly 1\noutput:\n%s",
			canopyCount, out)
	}
}

// TestRefresh_InvokesOnRefresh: r calls OnRefresh and forwards its cmd.
func TestRefresh_InvokesOnRefresh(t *testing.T) {
	called := false
	m := New(Options{
		OnRefresh: func() tea.Cmd {
			called = true
			return func() tea.Msg { return "refreshed" }
		},
	})

	_, gotCmd := m.Update(key("r"))
	if !called {
		t.Errorf("OnRefresh not called")
	}
	if gotCmd == nil {
		t.Errorf("r returned nil cmd, want forwarded OnRefresh cmd")
	}
}

// TestSetError_RendersBanner: SetError above the table, cleared by passing nil.
func TestSetError_RendersBanner(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())

	m.SetError(errors.New("boom"))
	if !strings.Contains(m.View(), "boom") {
		t.Errorf("error banner not rendered")
	}

	m.SetError(nil)
	if strings.Contains(m.View(), "boom") {
		t.Errorf("error banner not cleared")
	}
}

// TestCursorRow_Empty: returns false when list is empty.
func TestCursorRow_Empty(t *testing.T) {
	m := New(Options{})
	_, ok := m.CursorRow()
	if ok {
		t.Errorf("CursorRow on empty list returned ok=true")
	}
}

// TestCursorRow_NonEmpty: returns the row at cursor.
func TestCursorRow_NonEmpty(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	m, _ = m.Update(key("end"))

	row, ok := m.CursorRow()
	if !ok {
		t.Fatalf("CursorRow ok=false, want true")
	}
	if row.Name != "ancient-hornet" {
		t.Errorf("CursorRow at end = %q, want ancient-hornet", row.Name)
	}
}

// TestRender_IncludesRowFields: the rendered output contains the row data
// (project, name, branch, status, port, badge). Smoke test only — exact
// formatting is left to lipgloss + visual review.
func TestRender_IncludesRowFields(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	out := m.View()

	for _, want := range []string{"cravd", "bold-falcon", "feat/x", "ready", "3000"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRender_NoAliveDot covers the legend cleanup: with the alive dot
// dropped from the row prefix, no `●` or `○` should appear in any row
// rendering. The status column carries that information now (running /
// not started / ready / stopped) — the dot was redundant for 4 of 6
// statuses and confusing alongside the status text.
func TestRender_NoAliveDot(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "p", Name: "ws", Branch: "feat", Status: state.StatusReady, Alive: true},
		{Project: "p", Name: "stopped-ws", Branch: "feat-2", Status: state.StatusStopped, Alive: false},
	})
	m.cursor = -1
	out := m.View()
	if strings.Contains(out, "●") {
		t.Errorf("rendered output still contains alive dot ●:\n%s", out)
	}
	if strings.Contains(out, "○") {
		t.Errorf("rendered output still contains dead dot ○:\n%s", out)
	}
}

// TestRender_MainStatusText covers the "main row reuses status column
// for alive bit" simplification: instead of a separate dot column,
// (main) rows render "running" when Alive and "not started" otherwise.
func TestRender_MainStatusText(t *testing.T) {
	cases := []struct {
		alive bool
		want  string
	}{
		{true, "running"},
		{false, "not started"},
	}
	for _, tc := range cases {
		m := New(Options{})
		m.SetRows([]state.GlobalRow{
			{Project: "p", IsMain: true, Name: "(main)", Branch: "main", Status: "main", Alive: tc.alive},
		})
		m.cursor = -1
		out := m.View()
		if !strings.Contains(out, tc.want) {
			t.Errorf("Alive=%v: rendered output missing %q:\n%s", tc.alive, tc.want, out)
		}
	}
}

// TestRender_ChecksBadge covers the new pr_checks suffix: when the
// pr_status hint message includes " · checks <state>", the badge
// renderer surfaces it as a separate colored pill alongside the PR
// state badge so the user sees CI status at a glance.
func TestRender_ChecksBadge(t *testing.T) {
	cases := []struct {
		message string
		want    string
	}{
		{"PR #1 open; awaiting reviews · checks passing", "✓ checks"},
		{"PR #1 open; awaiting reviews · checks failing", "✗ checks"},
		{"PR #1 open; awaiting reviews · checks running", "… checks"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			m := New(Options{})
			m.SetRows([]state.GlobalRow{
				{
					Project: "p", Name: "ws", Branch: "feat",
					Status: state.StatusReady,
					Hints:  []state.Hint{{Kind: "pr_status", Message: tc.message}},
				},
			})
			m.cursor = -1
			out := m.View()
			if !strings.Contains(out, tc.want) {
				t.Errorf("message %q: rendered output missing %q:\n%s", tc.message, tc.want, out)
			}
		})
	}
}

// TestRender_BranchIcon: every workspace row's Branch column is prefixed
// with the U+2387 branch glyph so the column reads as a branch at a
// glance. Smoke test — exact column position is left to visual review,
// but the glyph must appear at least once in any non-empty render.
func TestRender_BranchIcon(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	out := m.View()
	if !strings.Contains(out, "⎇") {
		t.Errorf("rendered output missing branch icon ⎇:\n%s", out)
	}
}

// TestRender_MainRowBranchInGray: a (main) row carrying the project's
// default branch must render in the gray "subtle" style. Lipgloss
// suppresses color in non-TTY envs (like `go test`), so we can't
// assert ANSI escapes here — instead we assert the unstyled text shape
// (icon + branch name appears) and that the styling code path doesn't
// crash. The visual gray is exercised by the runtime TUI.
func TestRender_MainRowBranchInGray(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{
			Project: "canopy", ProjectRoot: "/a/canopy",
			IsMain: true, Name: "(main)", Branch: "main",
			Status: "main",
		},
	})
	m.cursor = -1 // off-row so the non-selected styling path fires
	out := m.View()

	// Branch icon + "main" branch name both visible.
	if !strings.Contains(out, "⎇") {
		t.Errorf("main-row missing branch icon:\n%s", out)
	}
	if !strings.Contains(out, "main") {
		t.Errorf("main-row missing branch name:\n%s", out)
	}
}

// TestSetSize_DoesNotPanic: SetSize is a setter; tested only for
// not-blowing-up on extreme values.
func TestSetSize_DoesNotPanic(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	m.SetSize(0, 0)
	m.SetSize(10000, 10000)
	_ = m.View()
}
