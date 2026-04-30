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
	for _, want := range []string{"↻ rename", "✓ merged", "✓ PR"} {
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

	for _, badge := range []string{"↻ rename", "✓ merged", "PR"} {
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

// TestRender_GitStatsBadge_AlwaysShows verifies that the git_stats
// badge appears regardless of PR/merged state — they're complementary
// signals (PR/merged: "is this done?"; stats: "what's in flight right
// now?"). User feedback on 2026-04-29: "git stats which could be
// useful in the ones with PR too."
func TestRender_GitStatsBadge_AlwaysShows(t *testing.T) {
	cases := []struct {
		name  string
		hints []state.Hint
	}{
		{"stats alone", []state.Hint{{Kind: "git_stats", Message: "↑3 ↓1 *5"}}},
		{"stats + pr_status", []state.Hint{
			{Kind: "git_stats", Message: "↑3 ↓1 *5"},
			{Kind: "pr_status", Message: "PR #42 open; awaiting reviews"},
		}},
		{"stats + merged", []state.Hint{
			{Kind: "git_stats", Message: "↑3"},
			{Kind: "shipped"},
		}},
		{"stats + rename", []state.Hint{
			{Kind: "git_stats", Message: "*2"},
			{Kind: "rename_suggested"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			m.SetRows([]state.GlobalRow{{
				Project: "p", Name: "ws", Branch: "b", Hints: tc.hints,
			}})
			out := m.View()
			// The git_stats message should appear verbatim somewhere in
			// the output regardless of which other hints are present.
			for _, h := range tc.hints {
				if h.Kind != "git_stats" {
					continue
				}
				if !strings.Contains(out, h.Message) {
					t.Errorf("git_stats message %q missing from render with %d hints:\n%s",
						h.Message, len(tc.hints), out)
				}
			}
		})
	}
}

// TestRender_GitStatsBadge_Suppressed: an empty git_stats message is
// the contract for "no signal" (clean workspace). Renderer must skip
// it so the row stays visually quiet rather than showing a blank pill.
func TestRender_GitStatsBadge_Suppressed(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "p", Name: "ws", Branch: "b",
		Hints: []state.Hint{{Kind: "git_stats", Message: ""}},
	}})
	out := m.View()
	// No leading arrows or asterisk should appear — empty stats should
	// not produce visible badge chrome.
	if strings.Contains(out, "↑") || strings.Contains(out, "↓") || strings.Contains(out, "*") {
		t.Errorf("clean workspace rendered git_stats glyphs:\n%s", out)
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
	if strings.Contains(out, "✓ merged") {
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
// hint (no pr_status) shows the "✓ merged" fallback badge.
// Critical for purely-local-repo workflows where there's no GitHub PR.
func TestRender_ShippedFallbackWhenNoPR(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "canopy", Name: "soft-fox",
		Hints: []state.Hint{{Kind: "shipped"}},
	}})
	if !strings.Contains(m.View(), "✓ merged") {
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

	// Status text is "running" for live ready rows after the
	// 2026-04-29 vocabulary unification (was "ready"). The recorded
	// state.Status enum is unchanged; only the display string flipped
	// to match main rows' vocabulary.
	for _, want := range []string{"cravd", "bold-falcon", "feat/x", "running", "3000"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRender_PresenceGlyphs locks in the three-state row-prefix glyph
// (added 2026-04-29 in response to user feedback: "blank vs blank-with-
// dot was confusing; can we have circle for running quietly and dotted
// circle for actively attached?"):
//
//   ⊙   alive AND attached (someone's looking at this session)
//   ○   alive AND not attached (running quietly in the background)
//   (none) not alive (status column says why: stopped/broken/etc.)
//
// The previous design used a single ⊙ for attached and blank for
// everything else, which conflated "alive but detached" with "dead."
// The user rightly called that out as too many distinctions packed
// into too few glyphs.
func TestRender_PresenceGlyphs(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "p", Name: "attached-ws", Branch: "a", Status: state.StatusReady, Alive: true, Attached: true, TmuxSession: "p-attached"},
		{Project: "p", Name: "detached-ws", Branch: "b", Status: state.StatusReady, Alive: true, Attached: false, TmuxSession: "p-detached"},
		{Project: "p", Name: "stopped-ws", Branch: "c", Status: state.StatusStopped, Alive: false, TmuxSession: "p-stopped"},
	})
	m.cursor = -1
	out := m.View()

	// Each line has the row's name, so we can locate the line by name
	// and check the glyph that precedes it.
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "attached-ws"):
			if !strings.Contains(line, "⊙") {
				t.Errorf("attached row missing ⊙ glyph:\n%s", line)
			}
			if strings.Contains(line, "○") {
				t.Errorf("attached row should not contain ○:\n%s", line)
			}
		case strings.Contains(line, "detached-ws"):
			if !strings.Contains(line, "○") {
				t.Errorf("detached-alive row missing ○ glyph:\n%s", line)
			}
			if strings.Contains(line, "⊙") {
				t.Errorf("detached row should not contain ⊙:\n%s", line)
			}
		case strings.Contains(line, "stopped-ws"):
			if strings.Contains(line, "⊙") || strings.Contains(line, "○") {
				t.Errorf("dead row should have no presence glyph (status column carries the meaning):\n%s", line)
			}
		}
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

// TestMemCell_FormatsLoad covers the combined Mem+CPU cell
// formatting. CPU is shown as integer percent on every alive row,
// including 0% — keeping cell format consistent ("320M N%" always)
// and avoiding the "is CPU missing because <1% or because no data?"
// ambiguity that the previous omit-under-1% rule introduced.
// Dead/main/zero-RSS rows render "—".
func TestMemCell_FormatsLoad(t *testing.T) {
	cases := []struct {
		name string
		row  state.GlobalRow
		want string
	}{
		{"idle workspace, cpu < 1% → 320M 0%", state.GlobalRow{Status: state.StatusReady, Alive: true, MemRSS: 320 * 1024 * 1024, CPU: 0.4}, "320M 0%"},
		{"alive workspace, cpu rounds up to 13%", state.GlobalRow{Status: state.StatusReady, Alive: true, MemRSS: 320 * 1024 * 1024, CPU: 12.5}, "320M 13%"},
		{"alive workspace, cpu > 100% multi-core", state.GlobalRow{Status: state.StatusReady, Alive: true, MemRSS: 1024 * 1024 * 1024, CPU: 250}, "1.0G 250%"},
		{"dead workspace → —", state.GlobalRow{Status: state.StatusStopped, Alive: false, MemRSS: 0, CPU: 0}, "—"},
		{"main row → —", state.GlobalRow{IsMain: true, Alive: true, MemRSS: 100, CPU: 5}, "—"},
		{"alive but probe didn't run yet (zero rss) → —", state.GlobalRow{Status: state.StatusReady, Alive: true, MemRSS: 0, CPU: 0}, "—"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := memCell(tc.row); got != tc.want {
				t.Errorf("memCell(%+v) = %q; want %q", tc.row, got, tc.want)
			}
		})
	}
}

// TestDisplayGlyph_AgreesWithDisplayStatus is the regression test for
// the "stopped without ⏸" bug. Reading the row was confusing: a stale-
// ready row showed " stopped" (ready glyph + stopped text) because
// the glyph helper used the recorded Status while the text helper
// used the displayed status. The two must agree — that's why
// displayGlyph exists alongside displayStatus.
//
// 2026-04-29: stale-ready rows now render with the stopped glyph
// instead of the ready one, matching their displayStatus override.
func TestDisplayGlyph_AgreesWithDisplayStatus(t *testing.T) {
	cases := []struct {
		name string
		row  state.GlobalRow
	}{
		{"workspace ready + alive", state.GlobalRow{Status: state.StatusReady, Alive: true}},
		{"workspace ready + !alive (stale)", state.GlobalRow{Status: state.StatusReady, Alive: false}},
		{"workspace stopped", state.GlobalRow{Status: state.StatusStopped}},
		{"workspace broken", state.GlobalRow{Status: state.StatusBroken}},
		{"workspace orphaned", state.GlobalRow{Status: state.StatusOrphaned}},
		{"workspace setting up", state.GlobalRow{Status: state.StatusSettingUp}},
		{"main alive", state.GlobalRow{IsMain: true, Alive: true}},
		{"main dead", state.GlobalRow{IsMain: true, Alive: false}},
	}
	// Map status text → expected glyph. Captures the legend the help
	// screen displays. If we add a new status, the test forces an
	// explicit decision about its glyph.
	textToGlyph := map[string]string{
		"running":     " ",
		"stopped":     "⏸",
		"broken":      "✗",
		"orphaned":    "!",
		"setting up":  "…",
		"not started": " ",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := displayStatus(tc.row)
			gotGlyph := displayGlyph(tc.row)
			wantGlyph, ok := textToGlyph[text]
			if !ok {
				t.Fatalf("displayStatus returned %q which has no expected glyph mapping; update textToGlyph or fix the helper", text)
			}
			if gotGlyph != wantGlyph {
				t.Errorf("row=%+v displayStatus=%q displayGlyph=%q; want glyph %q (glyph and text must agree)",
					tc.row, text, gotGlyph, wantGlyph)
			}
		})
	}
}

// TestDisplayStatus_VocabularyMapping locks in the 2026-04-29
// status-vocabulary unification:
//
//   - workspace ready + alive → "running" (was "ready")
//   - workspace ready + !alive → "stopped" (stale-ready override —
//     the recorded status hasn't caught up yet but Alive probe says
//     the session is gone)
//   - workspace setting_up → "setting up" (display only — the enum
//     value stays setting_up for backward compat)
//   - main rows: unchanged ("running"/"not started")
//   - other workspace statuses: unchanged
//
// Why this matters: "ready" was ambiguous between "ready to attach"
// and "actively running"; aligning workspace + main vocabulary on
// "running" plus letting the green ⊙ glyph carry "client attached"
// gives one job per visual element.
func TestDisplayStatus_VocabularyMapping(t *testing.T) {
	cases := []struct {
		name string
		row  state.GlobalRow
		want string
	}{
		{
			name: "workspace ready + alive → running",
			row:  state.GlobalRow{Status: state.StatusReady, Alive: true},
			want: "running",
		},
		{
			name: "workspace ready + !alive (stale) → stopped",
			row:  state.GlobalRow{Status: state.StatusReady, Alive: false},
			want: "stopped",
		},
		{
			name: "workspace setting_up → setting up (with space)",
			row:  state.GlobalRow{Status: state.StatusSettingUp},
			want: "setting up",
		},
		{
			name: "workspace stopped → stopped (unchanged)",
			row:  state.GlobalRow{Status: state.StatusStopped},
			want: "stopped",
		},
		{
			name: "workspace broken → broken (unchanged)",
			row:  state.GlobalRow{Status: state.StatusBroken},
			want: "broken",
		},
		{
			name: "workspace orphaned → orphaned (unchanged)",
			row:  state.GlobalRow{Status: state.StatusOrphaned},
			want: "orphaned",
		},
		{
			name: "main alive → running",
			row:  state.GlobalRow{IsMain: true, Alive: true},
			want: "running",
		},
		{
			name: "main dead → not started",
			row:  state.GlobalRow{IsMain: true, Alive: false},
			want: "not started",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayStatus(tc.row); got != tc.want {
				t.Errorf("displayStatus(%+v) = %q; want %q", tc.row, got, tc.want)
			}
		})
	}
}

// TestRender_NameColumnAlignmentDoesNotShiftOnSelection is the
// regression test for the "row jumps right when I move the cursor
// onto it" bug discovered 2026-04-29. Selected rows have a `❯ `
// caret (2 chars); non-selected rows must pad with 2 spaces in the
// same slot so the attach glyph + name + branch + status all stay
// in fixed columns regardless of which row is selected.
//
// Reproducer for the bug: selecting a row shifted everything after
// the caret 2 chars to the right because the non-selected branch
// of renderTable lacked the 2-space caret slot after the attach
// glyph addition.
func TestRender_NameColumnAlignmentDoesNotShiftOnSelection(t *testing.T) {
	rows := []state.GlobalRow{
		{Project: "p", ProjectRoot: "/p", Name: "alpha", Branch: "feat/a", Status: state.StatusReady, Port: 3000, Alive: true, TmuxSession: "p-alpha"},
		{Project: "p", ProjectRoot: "/p", Name: "bravo", Branch: "feat/b", Status: state.StatusReady, Port: 3001, Alive: true, TmuxSession: "p-bravo"},
	}

	m := New(Options{})
	m.SetRows(rows)

	// Cursor on first row by default.
	rendered1 := m.View()
	idxBravoSel0 := strings.Index(rendered1, "bravo")

	// Move cursor down to second row.
	m, _ = m.Update(key("down"))
	rendered2 := m.View()
	idxBravoSel1 := strings.Index(rendered2, "bravo")

	if idxBravoSel0 == -1 || idxBravoSel1 == -1 {
		t.Fatalf("bravo missing from one of the renders")
	}
	if idxBravoSel0 != idxBravoSel1 {
		t.Errorf("REGRESSION: row position shifted on selection. cursor-on-alpha bravo@%d, cursor-on-bravo bravo@%d (expected identical column position)\n--- cursor-on-alpha ---\n%s\n--- cursor-on-bravo ---\n%s",
			idxBravoSel0, idxBravoSel1, rendered1, rendered2)
	}
}
