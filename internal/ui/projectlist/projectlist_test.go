package projectlist

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/state"
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

// TestRender_CurrentMarker: the row matching SetCurrent gets a visible
// "← here" suffix in the rendered view, and rows that don't match
// stay clean. Catches accidental marker leak (every row shows it) or
// missed match (no row shows it).
func TestRender_CurrentMarker(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())
	m.SetCurrent("/b/canopy", "ancient-hornet")
	m.SetSize(120, 20)

	out := m.View()
	if !strings.Contains(out, "← here") {
		t.Errorf("View() missing current marker; got:\n%s", out)
	}
	// Marker should appear exactly once — only on the matching row.
	if got := strings.Count(out, "← here"); got != 1 {
		t.Errorf("current marker count = %d; want 1", got)
	}

	// Clearing current → marker disappears.
	m.SetCurrent("", "")
	if strings.Contains(m.View(), "← here") {
		t.Errorf("View() still has marker after SetCurrent(\"\", \"\")")
	}
}

// TestSetCursorTo_HitAndMiss: SetCursorTo positions the cursor on a
// matching (projectRoot, name) row and reports true; misses leave the
// cursor untouched and report false. Empty inputs are no-ops.
func TestSetCursorTo_HitAndMiss(t *testing.T) {
	m := New(Options{})
	m.SetRows(sampleRows())

	if !m.SetCursorTo("/b/canopy", "ancient-hornet") {
		t.Fatalf("SetCursorTo(hit) = false; want true")
	}
	if m.cursor != 2 {
		t.Errorf("cursor after hit = %d; want 2", m.cursor)
	}

	if m.SetCursorTo("/b/canopy", "nonexistent") {
		t.Errorf("SetCursorTo(miss) = true; want false")
	}
	if m.cursor != 2 {
		t.Errorf("cursor after miss = %d; want unchanged (2)", m.cursor)
	}

	if m.SetCursorTo("", "anything") {
		t.Errorf("SetCursorTo(empty project) = true; want false")
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

// TestRender_PushStateBadge: push_state hint surfaces as its own
// badge (⇡N or ⇅) and sits alongside git_stats rather than replacing
// it. The two answer different questions ("ahead of main" vs
// "ahead of origin/<branch>") so both should be visible together
// when both fire — that's the entire reason push_state was carved
// out of git_stats in the design.
func TestRender_PushStateBadge(t *testing.T) {
	cases := []struct {
		name        string
		hints       []state.Hint
		wantSubstrs []string
	}{
		{
			name: "unpushed alone",
			hints: []state.Hint{
				{Kind: "push_state", Message: "⇡3"},
			},
			wantSubstrs: []string{"⇡3"},
		},
		{
			name: "diverged alone",
			hints: []state.Hint{
				{Kind: "push_state", Message: "⇅"},
			},
			wantSubstrs: []string{"⇅"},
		},
		{
			name: "alongside git_stats — both visible",
			hints: []state.Hint{
				{Kind: "git_stats", Message: "↑5 *2"},
				{Kind: "push_state", Message: "⇡5"},
			},
			wantSubstrs: []string{"↑5 *2", "⇡5"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			m.SetRows([]state.GlobalRow{{
				Project: "p", Name: "ws", Branch: "b", Hints: tc.hints,
			}})
			out := m.View()
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in render:\n%s", want, out)
				}
			}
		})
	}
}

// TestRender_PushStateBadge_Suppressed: empty push_state message —
// the "no signal" contract — must not produce a visible glyph. The
// detector returns a hint with empty Message to mean "branch is
// fully synced"; the renderer suppresses to keep clean rows quiet.
func TestRender_PushStateBadge_Suppressed(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "p", Name: "ws", Branch: "b",
		Hints: []state.Hint{{Kind: "push_state", Message: ""}},
	}})
	out := m.View()
	if strings.Contains(out, "⇡") || strings.Contains(out, "⇅") {
		t.Errorf("synced workspace rendered push_state glyph:\n%s", out)
	}
}

// TestRender_StuckStatePreemptsGitStats locks in the v0.14 closeout
// precedence rule: when a row carries a stuck_state hint, the
// git_stats badge is suppressed even if the detector emitted one.
// During a rebase / merge / cherry-pick the ahead/behind/dirty
// counts reflect git's transient internal state and are not
// actionable; the only thing the user can usefully do is finish
// the in-flight op. Other badges (rename, mergeability, push_state,
// pr_status, shipped) keep rendering because they describe distinct
// facts that don't move during a rebase.
func TestRender_StuckStatePreemptsGitStats(t *testing.T) {
	t.Run("stuck + git_stats → only stuck", func(t *testing.T) {
		m := New(Options{})
		m.SetRows([]state.GlobalRow{{
			Project: "p", Name: "ws", Branch: "b",
			Hints: []state.Hint{
				{Kind: "stuck_state", Message: "⚠ rebasing"},
				{Kind: "git_stats", Message: "↑3 ↓1 *5"},
			},
		}})
		out := m.View()
		if !strings.Contains(out, "⚠ rebasing") {
			t.Errorf("stuck_state badge missing:\n%s", out)
		}
		if strings.Contains(out, "↑3") || strings.Contains(out, "↓1") || strings.Contains(out, "*5") {
			t.Errorf("git_stats glyphs leaked through stuck_state preempt:\n%s", out)
		}
	})

	t.Run("stuck + rename + mergeability + git_stats → all but stats", func(t *testing.T) {
		m := New(Options{})
		m.SetRows([]state.GlobalRow{{
			Project: "p", Name: "ws", Branch: "b",
			Hints: []state.Hint{
				{Kind: "stuck_state", Message: "⚠ merging"},
				{Kind: "rename_suggested", Message: "branch needs rename"},
				{Kind: "mergeability", Message: "⚠ conflict"},
				{Kind: "git_stats", Message: "↑2 *4"},
			},
		}})
		out := m.View()
		for _, want := range []string{"⚠ merging", "↻ rename", "⚠ conflict"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q under stuck_state precedence:\n%s", want, out)
			}
		}
		if strings.Contains(out, "↑2") || strings.Contains(out, "*4") {
			t.Errorf("git_stats survived stuck_state preempt:\n%s", out)
		}
	})

	t.Run("git_stats alone (no stuck) → unchanged", func(t *testing.T) {
		// Regression guard: the precedence rule must NOT fire when
		// stuck_state is absent. git_stats is a primary surface in the
		// common case and must keep rendering.
		m := New(Options{})
		m.SetRows([]state.GlobalRow{{
			Project: "p", Name: "ws", Branch: "b",
			Hints: []state.Hint{{Kind: "git_stats", Message: "↑3 *2"}},
		}})
		out := m.View()
		if !strings.Contains(out, "↑3 *2") {
			t.Errorf("git_stats should render when stuck_state absent:\n%s", out)
		}
	})

	t.Run("stuck with empty message does NOT preempt", func(t *testing.T) {
		// Defensive: an empty stuck_state.Message is the detector's
		// "no signal" contract. It should NOT trigger the preempt
		// because the badge column will be empty for that hint and
		// the user is owed the git_stats info.
		m := New(Options{})
		m.SetRows([]state.GlobalRow{{
			Project: "p", Name: "ws", Branch: "b",
			Hints: []state.Hint{
				{Kind: "stuck_state", Message: ""},
				{Kind: "git_stats", Message: "↑1"},
			},
		}})
		out := m.View()
		if !strings.Contains(out, "↑1") {
			t.Errorf("empty stuck_state should not preempt git_stats:\n%s", out)
		}
	})
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
		// Lone non-running main rows are now classified idle and collapsed
		// by default; expand the local host so the row's status text renders.
		m.idleExpanded = map[string]bool{"": true}
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
	// Lone non-running main rows are idle by default — expand to render.
	m.idleExpanded = map[string]bool{"": true}
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

// TestRender_MergeabilityBadge: a row carrying a mergeability hint
// renders "⚠ conflict" verbatim. Plural form rendered as well so the
// count survives lipgloss styling intact.
func TestRender_MergeabilityBadge(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"singular", "⚠ conflict", "⚠ conflict"},
		{"plural", "⚠ 3 conflicts", "⚠ 3 conflicts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			m.SetRows([]state.GlobalRow{{
				Project: "p", Name: "ws", Branch: "b",
				Hints: []state.Hint{{Kind: "mergeability", Message: tc.msg}},
			}})
			out := m.View()
			if !strings.Contains(out, tc.want) {
				t.Errorf("mergeability badge %q missing from render:\n%s", tc.want, out)
			}
		})
	}
}

// TestRender_MergeabilityBadge_AlongsideGitStats: the mergeability badge
// is complementary to git_stats (the warning AND the divergence count
// should be visible together — knowing you'll conflict means more in
// context with how-far-diverged-am-i). Mergeability does NOT preempt
// git_stats.
func TestRender_MergeabilityBadge_AlongsideGitStats(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "p", Name: "ws", Branch: "b",
		Hints: []state.Hint{
			{Kind: "mergeability", Message: "⚠ conflict"},
			{Kind: "git_stats", Message: "↑3 ↓2 *1"},
		},
	}})
	out := m.View()
	if !strings.Contains(out, "⚠ conflict") {
		t.Errorf("mergeability badge missing:\n%s", out)
	}
	if !strings.Contains(out, "↑3 ↓2 *1") {
		t.Errorf("git_stats badge missing (should not be preempted):\n%s", out)
	}
	// And mergeability should appear LEFT of git_stats so the warning
	// reads first when scanning the row.
	mergeIdx := strings.Index(out, "⚠ conflict")
	statsIdx := strings.Index(out, "↑3 ↓2 *1")
	if mergeIdx == -1 || statsIdx == -1 {
		t.Fatalf("badge index lookup failed: merge=%d stats=%d", mergeIdx, statsIdx)
	}
	if mergeIdx > statsIdx {
		t.Errorf("expected mergeability LEFT of git_stats (merge@%d, stats@%d):\n%s",
			mergeIdx, statsIdx, out)
	}
}

// TestRender_MergeabilityBadge_Suppressed: an empty message must not
// render badge chrome. Mirrors the contract for git_stats — empty
// message = no signal.
func TestRender_MergeabilityBadge_Suppressed(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "p", Name: "ws", Branch: "b",
		Hints: []state.Hint{{Kind: "mergeability", Message: ""}},
	}})
	out := m.View()
	if strings.Contains(out, "⚠") {
		t.Errorf("empty mergeability message rendered ⚠ glyph:\n%s", out)
	}
}

// TestRender_MergeabilityBadge_OrderRelativeToRename: when both rename
// and mergeability are active, rename sits left of mergeability.
// stuck_state would preempt both, but absent that, rename is the
// "name your branch" prerequisite badge.
func TestRender_MergeabilityBadge_OrderRelativeToRename(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "p", Name: "ws", Branch: "b",
		Hints: []state.Hint{
			{Kind: "mergeability", Message: "⚠ conflict"},
			{Kind: "rename_suggested", Message: "rename me"},
		},
	}})
	out := m.View()
	renameIdx := strings.Index(out, "↻ rename")
	mergeIdx := strings.Index(out, "⚠ conflict")
	if renameIdx == -1 || mergeIdx == -1 {
		t.Fatalf("badge missing: rename=%d merge=%d\n%s", renameIdx, mergeIdx, out)
	}
	if renameIdx > mergeIdx {
		t.Errorf("expected rename LEFT of mergeability (rename@%d, merge@%d):\n%s",
			renameIdx, mergeIdx, out)
	}
}

// TestRender_StuckStateBadge_AllStates: each of the four stuck_state
// messages renders verbatim through the row pipeline. Lipgloss styling
// must not corrupt the glyphs or text.
func TestRender_StuckStateBadge_AllStates(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"rebasing", "⚠ rebasing"},
		{"merging", "⚠ merging"},
		{"pick", "⚠ pick"},
		{"detached", "⚠ detached"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			m.SetRows([]state.GlobalRow{{
				Project: "p", Name: "ws", Branch: "b",
				Hints: []state.Hint{{Kind: "stuck_state", Message: tc.msg}},
			}})
			out := m.View()
			if !strings.Contains(out, tc.msg) {
				t.Errorf("stuck_state badge %q missing from render:\n%s", tc.msg, out)
			}
		})
	}
}

// TestRender_StuckStateBadge_Suppressed: an empty message suppresses
// the badge. Mirrors git_stats / mergeability — empty = no signal,
// don't render orphan chrome.
func TestRender_StuckStateBadge_Suppressed(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "p", Name: "ws", Branch: "b",
		Hints: []state.Hint{{Kind: "stuck_state", Message: ""}},
	}})
	out := m.View()
	if strings.Contains(out, "⚠") {
		t.Errorf("empty stuck_state message rendered ⚠ glyph:\n%s", out)
	}
}

// TestRender_StuckStateBadge_LeftmostOfRename: stuck_state preempts
// attention because the user can't act on rename / git_stats while
// mid-rebase. Verify it sits left of the rename badge in the row.
func TestRender_StuckStateBadge_LeftmostOfRename(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{
		Project: "p", Name: "ws", Branch: "b",
		Hints: []state.Hint{
			{Kind: "rename_suggested", Message: "rename me"},
			{Kind: "stuck_state", Message: "⚠ rebasing"},
		},
	}})
	out := m.View()
	stuckIdx := strings.Index(out, "⚠ rebasing")
	renameIdx := strings.Index(out, "↻ rename")
	if stuckIdx == -1 || renameIdx == -1 {
		t.Fatalf("badge missing: stuck=%d rename=%d\n%s", stuckIdx, renameIdx, out)
	}
	if stuckIdx > renameIdx {
		t.Errorf("expected stuck_state LEFT of rename (stuck@%d, rename@%d):\n%s",
			stuckIdx, renameIdx, out)
	}
}

// TestAgentBadge_RemoteRowSkipsNoAIFallback: regression for the
// user-reported "remote workspace has agent pane but TUI doesn't
// show it." The laptop's agent poll only probes local tmux sessions,
// so remote sessions are absent from the agentStates map. Pre-fix,
// agentBadge fell through to the No-AI fallback `·` which falsely
// signaled "this workspace has no agent." Now: blank slot for
// remote rows until Phase 1d.2 propagates remote agent state.
func TestAgentBadge_RemoteRowSkipsNoAIFallback(t *testing.T) {
	r := state.GlobalRow{
		Name: "remote-foo", Status: state.StatusReady,
		Alive: true, TmuxSession: "cravd/remote-foo",
		Host: "tower",
	}
	got := agentBadge(r, nil, true /* poll has landed */)
	if got != "  " {
		t.Errorf("agentBadge(remote row) = %q; want two-space blank", got)
	}
}

// TestAgentBadge_LocalRowStillShowsNoAIAfterPoll: belt-and-suspenders
// for the host-row short-circuit — local rows must keep the No-AI
// fallback so local workspaces without an agent pane (e.g. an aider
// session that quit) still surface the gray dot.
func TestAgentBadge_LocalRowStillShowsNoAIAfterPoll(t *testing.T) {
	r := state.GlobalRow{
		Name: "local-foo", Status: state.StatusReady,
		Alive: true, TmuxSession: "p/local-foo",
		// Host empty → local row.
	}
	got := agentBadge(r, nil, true)
	if got == "  " {
		t.Errorf("agentBadge(local row, post-poll, no state) = blank; want No-AI dot")
	}
}

// TestAgentBadge_RemoteRowReadsAgentStateField: v0.17 Phase 1d.2 —
// remote rows carry their agent state on r.AgentState (wired from
// the canopy ls --json `agent_state` field). agentBadge must read
// from that field, not from the local agentStates map.
func TestAgentBadge_RemoteRowReadsAgentStateField(t *testing.T) {
	cases := []struct {
		state    string
		wantBare string // visible glyph (after ansi stripping)
	}{
		{"awaiting_input", "✋"},
		{"thinking", "⚡"},
		{"idle", "💤"},
		{"", ""}, // unknown → blank
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			r := state.GlobalRow{
				Name: "remote-foo", Status: state.StatusReady,
				Alive: true, TmuxSession: "cravd/remote-foo",
				Host:       "tower",
				AgentState: tc.state,
			}
			got := agentBadge(r, nil, true)
			if tc.wantBare == "" {
				if got != "  " {
					t.Errorf("AgentState=%q: got %q; want blank", tc.state, got)
				}
				return
			}
			if !strings.Contains(got, tc.wantBare) {
				t.Errorf("AgentState=%q: got %q; want to contain %q", tc.state, got, tc.wantBare)
			}
		})
	}
}

// TestIsStale covers the v0.19 staleness decision tree: only fires for
// remote rows (Host!=""), only after a successful refresh (LastSeen
// non-zero), only when time.Since(LastSeen) > staleThreshold. The
// renderer relies on this to gate the dim style + section banner —
// false positives would dim healthy data, false negatives would hide
// SSH outages.
func TestIsStale(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		row  state.GlobalRow
		want bool
	}{
		{
			name: "local row never stale",
			row:  state.GlobalRow{Host: "", LastSeen: now.Add(-24 * time.Hour)},
			want: false,
		},
		{
			name: "remote row, never refreshed (zero LastSeen)",
			row:  state.GlobalRow{Host: "tower"},
			want: false,
		},
		{
			name: "remote row, refreshed 1s ago",
			row:  state.GlobalRow{Host: "tower", LastSeen: now.Add(-1 * time.Second)},
			want: false,
		},
		{
			name: "remote row, refreshed right at threshold (not yet stale)",
			row:  state.GlobalRow{Host: "tower", LastSeen: now.Add(-staleThreshold + time.Second)},
			want: false,
		},
		{
			name: "remote row, refreshed past threshold (stale)",
			row:  state.GlobalRow{Host: "tower", LastSeen: now.Add(-staleThreshold - time.Second)},
			want: true,
		},
		{
			name: "remote row, refreshed long ago (definitely stale)",
			row:  state.GlobalRow{Host: "tower", LastSeen: now.Add(-1 * time.Hour)},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStale(tc.row); got != tc.want {
				t.Errorf("isStale = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRender_StaleHostHeaderHasBanner: when a remote host's LastSeen is
// older than staleThreshold, the host section header gets a "⚠ stale Ns"
// pill. Catches drift if the dim styling is added but the banner copy
// is forgotten (the banner is the load-bearing signal for users
// scanning the list — dim alone is easy to miss on the cursor row).
func TestRender_StaleHostHeaderHasBanner(t *testing.T) {
	m := New(Options{})
	staleTime := time.Now().Add(-30 * time.Second) // 3x threshold
	m.SetRows([]state.GlobalRow{
		// Local row first so we exercise the "any remote present"
		// branch that triggers the host headers at all.
		{Project: "p", Name: "local-ws", Branch: "b", Status: state.StatusReady, Alive: true, TmuxSession: "p/local"},
		{Project: "p", Name: "remote-ws", Branch: "b", Status: state.StatusReady, Alive: true, TmuxSession: "p/remote", Host: "tower", LastSeen: staleTime},
	})
	m.cursor = -1
	out := m.View()
	if !strings.Contains(out, "⚠ stale") {
		t.Errorf("expected '⚠ stale' banner on tower host header for LastSeen 30s ago, got:\n%s", out)
	}
}

// TestRender_FreshHostHeaderNoBanner: a remote host refreshed within
// the freshness window must NOT show a stale banner. Without this
// guard the banner could appear permanently if the threshold logic
// inverts somewhere.
func TestRender_FreshHostHeaderNoBanner(t *testing.T) {
	m := New(Options{})
	freshTime := time.Now().Add(-1 * time.Second)
	m.SetRows([]state.GlobalRow{
		{Project: "p", Name: "local-ws", Branch: "b", Status: state.StatusReady, Alive: true, TmuxSession: "p/local"},
		{Project: "p", Name: "remote-ws", Branch: "b", Status: state.StatusReady, Alive: true, TmuxSession: "p/remote", Host: "tower", LastSeen: freshTime},
	})
	m.cursor = -1
	out := m.View()
	if strings.Contains(out, "⚠ stale") {
		t.Errorf("did NOT expect '⚠ stale' banner for fresh remote row (LastSeen 1s ago), got:\n%s", out)
	}
}

// TestRender_LocalHostHeaderNeverStale: local rows must never trigger
// the stale UX even if LastSeen is somehow set on them. Defensive guard
// against future refactors that might leak per-host LastSeen onto local
// rows.
func TestRender_LocalHostHeaderNeverStale(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		// Even if a local row's LastSeen is way old, it must not stale.
		{Project: "p", Name: "local-ws", Branch: "b", Status: state.StatusReady, Alive: true, TmuxSession: "p/local", LastSeen: time.Now().Add(-1 * time.Hour)},
		{Project: "p", Name: "remote-ws", Branch: "b", Status: state.StatusReady, Alive: true, TmuxSession: "p/remote", Host: "tower", LastSeen: time.Now()},
	})
	m.cursor = -1
	out := m.View()
	if strings.Contains(out, "⚠ stale") {
		t.Errorf("local row with old LastSeen should not trigger stale UX, got:\n%s", out)
	}
}

// TestRender_LoadingRowShowsSpinnerUnderHostHeader: synthetic Loading
// rows must surface the host section header AND a spinner+"loading…"
// placeholder line, so a registered host appears immediately on first
// launch instead of being hidden until SSH returns.
func TestRender_LoadingRowShowsSpinnerUnderHostHeader(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "p", Name: "local-ws", Branch: "b", Status: state.StatusReady, Alive: true, TmuxSession: "p/local"},
		{Host: "tower", Loading: true},
	})
	m.cursor = -1
	out := m.View()
	// Host names render uppercased inside the pill (v0.22 redesign).
	if !strings.Contains(out, "TOWER") {
		t.Errorf("expected host header 'TOWER' in output; got:\n%s", out)
	}
	if !strings.Contains(out, "loading…") {
		t.Errorf("expected loading placeholder line; got:\n%s", out)
	}
}

// TestRender_LoadingRowAnimatesWithSpinnerFrame: bumping the spinner
// frame must cycle through the Braille rotation so the placeholder
// reads as live progress instead of a stuck glyph. Drives the
// SetSpinnerFrame plumbing from the parent's hostsSpinnerTickMsg.
func TestRender_LoadingRowAnimatesWithSpinnerFrame(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{{Host: "tower", Loading: true}})
	m.cursor = -1
	m.SetSpinnerFrame(0)
	frame0 := m.View()
	m.SetSpinnerFrame(1)
	frame1 := m.View()
	if frame0 == frame1 {
		t.Errorf("expected spinner glyph to change between frames 0 and 1; got identical output:\n%s", frame0)
	}
	if !strings.Contains(frame0, spinnerFrames[0]) {
		t.Errorf("expected frame 0 to contain glyph %q; got:\n%s", spinnerFrames[0], frame0)
	}
	if !strings.Contains(frame1, spinnerFrames[1]) {
		t.Errorf("expected frame 1 to contain glyph %q; got:\n%s", spinnerFrames[1], frame1)
	}
}

// TestUpdate_EnterOnLoadingRowIsNoop: pressing enter on a synthetic
// loading placeholder must NOT invoke onActivate (the row has no
// workspace to attach to). Same posture for `o`.
func TestUpdate_EnterOnLoadingRowIsNoop(t *testing.T) {
	activateCalls := 0
	goToCalls := 0
	m := New(Options{
		OnActivate:    func(state.GlobalRow) tea.Cmd { activateCalls++; return nil },
		OnGoToProject: func(state.GlobalRow) tea.Cmd { goToCalls++; return nil },
	})
	m.SetRows([]state.GlobalRow{{Host: "tower", Loading: true}})
	m.cursor = 0
	if _, cmd := m.Update(key("enter")); cmd != nil {
		t.Errorf("enter on Loading row should return nil cmd; got %v", cmd)
	}
	if activateCalls != 0 {
		t.Errorf("enter on Loading row triggered onActivate %d times; want 0", activateCalls)
	}
	if _, cmd := m.Update(key("o")); cmd != nil {
		t.Errorf("o on Loading row should return nil cmd; got %v", cmd)
	}
	if goToCalls != 0 {
		t.Errorf("o on Loading row triggered onGoToProject %d times; want 0", goToCalls)
	}
}

// TestSetLoadingHosts_HeaderRendersSpinner: when a host is in the
// loadingHosts set, its section header must include a spinner glyph
// alongside the host name so the user can see "we're checking" even
// when stale rows from a previous refresh are still visible on
// screen. v0.22.
func TestSetLoadingHosts_HeaderRendersSpinner(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Host: "tower", Project: "canopy", Name: "(main)", IsMain: true, Branch: "main"},
	})
	m.SetLoadingHosts(map[string]bool{"tower": true})
	m.SetSpinnerFrame(0)
	out := m.View()
	if !strings.Contains(out, spinnerFrames[0]) {
		t.Errorf("expected spinner glyph %q in header when host is loading; got:\n%s", spinnerFrames[0], out)
	}
	// Host names render uppercased inside the pill (v0.22 redesign).
	if !strings.Contains(out, "TOWER") {
		t.Errorf("expected host name 'TOWER' in header; got:\n%s", out)
	}
}

// TestSetLoadingHosts_EmptyOmitsSpinner: with no hosts marked loading,
// host headers render plain (no spinner glyph). Regression guard
// against the spinner latching on after a refresh completes.
func TestSetLoadingHosts_EmptyOmitsSpinner(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Host: "tower", Project: "canopy", Name: "(main)", IsMain: true, Branch: "main"},
	})
	m.SetLoadingHosts(nil)
	m.SetSpinnerFrame(0)
	out := m.View()
	for _, glyph := range spinnerFrames {
		if strings.Contains(out, glyph) {
			t.Errorf("expected no spinner glyph in header when loadingHosts empty; found %q in:\n%s", glyph, out)
		}
	}
}

// TestSetLoadingHosts_LocalSectionUnaffected: loading-hosts decorates
// only the remote-host headers. The local section header (Host=="",
// labelled "local") must render plain even when the map is non-empty —
// local rows aren't part of the SSH fan-out and shouldn't borrow its
// spinner.
func TestSetLoadingHosts_LocalSectionUnaffected(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		// At least one remote row so hasRemote=true (local header only
		// appears when the listing includes a non-local row).
		{Host: "", Project: "canopy", Name: "(main)", IsMain: true, Branch: "main"},
		{Host: "tower", Project: "cravd", Name: "(main)", IsMain: true, Branch: "main"},
	})
	m.SetLoadingHosts(map[string]bool{"tower": true})
	m.SetSpinnerFrame(0)
	out := m.View()
	// The "local" header sits before the "tower" header. The spinner
	// should appear only on the tower side — assert by counting glyph
	// occurrences (one per loading host).
	glyph := spinnerFrames[0]
	count := strings.Count(out, glyph)
	if count != 1 {
		t.Errorf("expected exactly one spinner in output (for tower); got %d occurrences of %q in:\n%s", count, glyph, out)
	}
}

// TestSetLoadingHosts_HeaderSpinnerAnimatesWithFrame: bumping the
// spinner frame must roll the header glyph through the rotation in
// lockstep with the placeholder row spinner, so a host that has
// cached rows shows the same animation as one with no rows yet.
func TestSetLoadingHosts_HeaderSpinnerAnimatesWithFrame(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Host: "tower", Project: "canopy", Name: "(main)", IsMain: true, Branch: "main"},
	})
	m.SetLoadingHosts(map[string]bool{"tower": true})

	m.SetSpinnerFrame(0)
	frame0 := m.View()
	m.SetSpinnerFrame(1)
	frame1 := m.View()
	if frame0 == frame1 {
		t.Errorf("header spinner did not change between frames 0 and 1; output:\n%s", frame0)
	}
	if !strings.Contains(frame0, spinnerFrames[0]) {
		t.Errorf("frame 0 missing glyph %q; got:\n%s", spinnerFrames[0], frame0)
	}
	if !strings.Contains(frame1, spinnerFrames[1]) {
		t.Errorf("frame 1 missing glyph %q; got:\n%s", spinnerFrames[1], frame1)
	}
}
