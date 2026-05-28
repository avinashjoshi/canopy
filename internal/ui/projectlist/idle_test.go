package projectlist

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/avinashjoshi/canopy/internal/state"
)

// TestClassifyIdle is the table-driven contract test for the idle
// classifier. Idle = a project whose only row is its synthetic (main)
// row, with main not running and not in a transitional state. The
// renderer hides idle rows by default and emits a "+N idle projects"
// roll-up per host. The classifier needs to be correct because both
// the renderer and the cursor navigation rely on its hidden[] slice.
func TestClassifyIdle(t *testing.T) {
	cases := []struct {
		name       string
		rows       []state.GlobalRow
		expanded   map[string]bool
		wantHidden []bool
		wantIdle   map[string]int
	}{
		{
			name: "lone main row, stopped → idle and hidden",
			rows: []state.GlobalRow{
				{Project: "brain", Name: "(main)", IsMain: true, Status: state.StatusStopped},
			},
			wantHidden: []bool{true},
			wantIdle:   map[string]int{"": 1},
		},
		{
			name: "lone main row, empty status → idle and hidden",
			rows: []state.GlobalRow{
				{Project: "brain", Name: "(main)", IsMain: true},
			},
			wantHidden: []bool{true},
			wantIdle:   map[string]int{"": 1},
		},
		{
			// Regression: state.BuildGlobalRows stamps Status:"main" on
			// every synthetic main row (listing.go:268). Pre-fix, the
			// classifier's filter rejected anything that wasn't ""/stopped,
			// so production rows never matched and the collapse feature
			// was inert. This case locks the production row shape.
			name: "lone main row, status=\"main\" (production shape) → idle and hidden",
			rows: []state.GlobalRow{
				{Project: "chrome-tab-close-guard", Name: "(main)", IsMain: true, Status: "main"},
			},
			wantHidden: []bool{true},
			wantIdle:   map[string]int{"": 1},
		},
		{
			name: "main row with workspaces → not idle",
			rows: []state.GlobalRow{
				{Project: "cravd", Name: "(main)", IsMain: true},
				{Project: "cravd", Name: "fair-comet", Status: state.StatusReady, Alive: true},
			},
			wantHidden: []bool{false, false},
			wantIdle:   map[string]int{},
		},
		{
			name: "main row, alive (server running) → not idle",
			rows: []state.GlobalRow{
				{Project: "guard", Name: "(main)", IsMain: true, Alive: true},
			},
			wantHidden: []bool{false},
			wantIdle:   map[string]int{},
		},
		{
			name: "main row, broken status → not idle (attention state)",
			rows: []state.GlobalRow{
				{Project: "guard", Name: "(main)", IsMain: true, Status: state.StatusBroken},
			},
			wantHidden: []bool{false},
			wantIdle:   map[string]int{},
		},
		{
			name: "expanded host shows idle counted but not hidden",
			rows: []state.GlobalRow{
				{Host: "tower", Project: "brain", Name: "(main)", IsMain: true},
			},
			expanded:   map[string]bool{"tower": true},
			wantHidden: []bool{false},
			wantIdle:   map[string]int{"tower": 1},
		},
		{
			name: "loading placeholder never idle",
			rows: []state.GlobalRow{
				{Host: "pi", Loading: true},
			},
			wantHidden: []bool{false},
			wantIdle:   map[string]int{},
		},
		{
			name: "per-host independence: local expanded, tower collapsed",
			rows: []state.GlobalRow{
				{Project: "brain", Name: "(main)", IsMain: true},
				{Host: "tower", Project: "canopy", Name: "(main)", IsMain: true},
			},
			expanded:   map[string]bool{"": true},
			wantHidden: []bool{false, true},
			wantIdle:   map[string]int{"": 1, "tower": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotHidden, gotIdle := ClassifyIdle(tc.rows, tc.expanded)
			if len(gotHidden) != len(tc.wantHidden) {
				t.Fatalf("hidden length: got %d, want %d", len(gotHidden), len(tc.wantHidden))
			}
			for i := range gotHidden {
				if gotHidden[i] != tc.wantHidden[i] {
					t.Errorf("hidden[%d]: got %v, want %v", i, gotHidden[i], tc.wantHidden[i])
				}
			}
			if len(gotIdle) != len(tc.wantIdle) {
				t.Errorf("idleByHost size: got %v, want %v", gotIdle, tc.wantIdle)
			}
			for k, v := range tc.wantIdle {
				if gotIdle[k] != v {
					t.Errorf("idleByHost[%q]: got %d, want %d", k, gotIdle[k], v)
				}
			}
		})
	}
}

// TestRender_IdleRollupCollapsedByDefault: idle projects should not
// appear in the rendered output by default; a roll-up line with the
// count should appear in their place.
func TestRender_IdleRollupCollapsedByDefault(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "cravd", Name: "fair-comet", Branch: "fair-comet", Status: state.StatusReady, Alive: true},
		{Project: "brain", Name: "(main)", IsMain: true, Branch: "master"},
		{Project: "fizzy", Name: "(main)", IsMain: true, Branch: "main"},
	})
	out := stripStyle(m.View())
	if strings.Contains(out, "brain") {
		t.Errorf("collapsed view should not contain idle project 'brain'; got:\n%s", out)
	}
	if strings.Contains(out, "fizzy") {
		t.Errorf("collapsed view should not contain idle project 'fizzy'; got:\n%s", out)
	}
	if !strings.Contains(out, "2 idle projects") {
		t.Errorf("expected roll-up line '2 idle projects'; got:\n%s", out)
	}
	if !strings.Contains(out, "fair-comet") {
		t.Errorf("non-idle workspace 'fair-comet' should still render; got:\n%s", out)
	}
}

// TestRender_IdleRollupHasBlankLineAbove: the roll-up line must have a
// blank line above it so it doesn't visually glue to the last project's
// last workspace row. Regression for the "too close" complaint when the
// fix that finally classified production main rows as idle landed.
func TestRender_IdleRollupHasBlankLineAbove(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "cravd", Name: "fair-comet", Status: state.StatusReady, Alive: true},
		{Project: "brain", Name: "(main)", IsMain: true, Status: "main"},
	})
	out := stripStyle(m.View())
	lines := strings.Split(out, "\n")
	var rollupIdx = -1
	for i, ln := range lines {
		if strings.Contains(ln, "idle project") {
			rollupIdx = i
			break
		}
	}
	if rollupIdx <= 0 {
		t.Fatalf("roll-up line not found or at top; got:\n%s", out)
	}
	if strings.TrimSpace(lines[rollupIdx-1]) != "" {
		t.Errorf("expected blank line immediately above roll-up; got %q (full:\n%s)", lines[rollupIdx-1], out)
	}
}

// TestRender_IdleRollupExpandsOnE: pressing `e` flips the cursor host's
// idle state from collapsed to expanded, surfacing the previously-hidden
// rows. The roll-up line stays but flips its hint to "e collapse" so
// the user can fold them back.
func TestRender_IdleRollupExpandsOnE(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "cravd", Name: "fair-comet", Status: state.StatusReady, Alive: true},
		{Project: "brain", Name: "(main)", IsMain: true, Branch: "master"},
	})
	// Press `e` while cursor is on the visible 'fair-comet' (cursor=0
	// after SetRows). Both rows share local host so the toggle affects
	// the local idle group.
	next, _ := m.Update(key("e"))
	out := stripStyle(next.View())
	if !strings.Contains(out, "brain") {
		t.Errorf("after `e`, idle project 'brain' should be visible; got:\n%s", out)
	}
	if !strings.Contains(out, "e collapse") {
		t.Errorf("after expanding, roll-up hint should say 'e collapse'; got:\n%s", out)
	}
}

// TestRender_IdleRollupExpandsRemoteEvenFromLocalRow pins the v0.21.11+
// lockstep `e` semantics: pressing `e` while the cursor is on a local
// row must also expand a remote host whose only rows are idle (and
// therefore hidden, so the cursor cannot land on them). Pre-fix this
// was the user-visible "idle projects only expands local, not remote"
// bug — per-host independence had no way to toggle a host that lacked
// a visible cursor target.
func TestRender_IdleRollupExpandsRemoteEvenFromLocalRow(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		// Local: a visible workspace so the cursor has somewhere to
		// land, plus an idle main so local has its own roll-up.
		{Project: "cravd", Name: "fair-comet", Status: state.StatusReady, Alive: true},
		{Project: "brain", Name: "(main)", IsMain: true, Branch: "master"},
		// Tower: an idle-only host. With per-host independence the
		// cursor could never reach this row to toggle it.
		{Host: "tower", Project: "canopy", Name: "(main)", IsMain: true, Status: "main"},
	})
	next, _ := m.Update(key("e"))
	out := stripStyle(next.View())
	if !strings.Contains(out, "brain") {
		t.Errorf("after `e`, local idle project 'brain' should be visible; got:\n%s", out)
	}
	if !strings.Contains(out, "canopy") {
		t.Errorf("after `e`, tower idle project 'canopy' should be visible; got:\n%s", out)
	}
	// Press again: both should collapse together. Lockstep means the
	// second `e` collapses whatever the first `e` expanded.
	next2, _ := next.Update(key("e"))
	out2 := stripStyle(next2.View())
	if strings.Contains(out2, "brain") {
		t.Errorf("after second `e`, local 'brain' should be hidden; got:\n%s", out2)
	}
	if strings.Contains(out2, "canopy") {
		t.Errorf("after second `e`, tower 'canopy' should be hidden; got:\n%s", out2)
	}
}

// TestRender_NoIdleNoRollupLine: when no project meets the idle
// criteria, the renderer must not emit a roll-up line at all.
// Regression guard against accidentally emitting "+ 0 idle projects".
func TestRender_NoIdleNoRollupLine(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "cravd", Name: "fair-comet", Status: state.StatusReady, Alive: true},
	})
	out := stripStyle(m.View())
	if strings.Contains(out, "idle project") {
		t.Errorf("no idle projects, but roll-up line appeared; got:\n%s", out)
	}
}

// TestNav_SkipsHiddenIdleRows: pressing `j` from the last visible row
// must NOT advance onto a hidden idle row — the cursor should stay
// put. Confirms the nextVisible-based skip logic in the navigation
// handlers.
func TestNav_SkipsHiddenIdleRows(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "cravd", Name: "fair-comet", Status: state.StatusReady, Alive: true},
		{Project: "brain", Name: "(main)", IsMain: true, Status: state.StatusStopped}, // hidden idle
	})
	if m.cursor != 0 {
		t.Fatalf("initial cursor: got %d, want 0", m.cursor)
	}
	next, _ := m.Update(key("j"))
	if next.cursor != 0 {
		t.Errorf("`j` should not advance onto hidden idle row; cursor went to %d", next.cursor)
	}
}

// TestNav_EReexpandsForJumpToHidden: after pressing `e` on a host with
// idle projects, the previously-hidden rows become navigable. Direct
// confirmation that the expanded state participates in nextVisible.
func TestNav_EReexpandsForJumpToHidden(t *testing.T) {
	m := New(Options{})
	m.SetRows([]state.GlobalRow{
		{Project: "cravd", Name: "fair-comet", Status: state.StatusReady, Alive: true},
		{Project: "brain", Name: "(main)", IsMain: true, Status: state.StatusStopped},
	})
	next, _ := m.Update(key("e"))
	// Now `j` should advance to the previously-hidden brain row.
	next2, _ := next.Update(key("j"))
	if next2.cursor != 1 {
		t.Errorf("after `e`, `j` should reach idle row at index 1; cursor went to %d", next2.cursor)
	}
}

// TestSetRows_CursorOffIdleRow: SetRows should never leave the cursor
// on a hidden idle row. The 'attach to this row' affordance would point
// at nothing if it did.
func TestSetRows_CursorOffIdleRow(t *testing.T) {
	m := New(Options{})
	// Cursor would naturally land at 0 — make sure the cursor advances
	// off an idle row at position 0.
	m.SetRows([]state.GlobalRow{
		{Project: "brain", Name: "(main)", IsMain: true, Status: state.StatusStopped}, // idle
		{Project: "cravd", Name: "fair-comet", Status: state.StatusReady, Alive: true},
	})
	if m.cursor != 1 {
		t.Errorf("cursor should auto-advance off hidden idle row; landed at %d (expected 1)", m.cursor)
	}
}

// TestHostPill_Uppercases: the host pill renderer uppercases its label
// (DESIGN: ALL-CAPS reads as section heading without competing with
// the brand pill). Direct test on the helper because the rendering
// path is shared by every host section.
func TestHostPill_Uppercases(t *testing.T) {
	got := hostPill("tower")
	plain := stripStyle(got)
	if !strings.Contains(plain, "TOWER") {
		t.Errorf("hostPill should uppercase 'tower' → 'TOWER'; got %q", plain)
	}
	if strings.Contains(plain, "tower") && !strings.Contains(plain, "TOWER") {
		t.Errorf("hostPill leaked lowercase 'tower' in output: %q", plain)
	}
}

// TestSelectionStyle_UsesTeal: selection bg must be color 38 (teal),
// not 237 (the old dark grey that collided with scopePillStyle and
// inactiveTabStyle). lipgloss.Color is `type Color string` so the
// type assertion + string compare works regardless of TTY detection.
func TestSelectionStyle_UsesTeal(t *testing.T) {
	bg, ok := selectionStyle().GetBackground().(lipgloss.Color)
	if !ok {
		t.Fatalf("selectionStyle bg is not a lipgloss.Color; got %T", selectionStyle().GetBackground())
	}
	if string(bg) != "38" {
		t.Errorf("selectionStyle bg: got %q, want \"38\" (teal)", string(bg))
	}
}

// TestProjectHeaderStyle_NotViolet: project header must be bold-white
// (231), not violet (99). Violet now belongs to the brand pill alone;
// project headers got the bold-white slot in the v0.22 palette work.
func TestProjectHeaderStyle_NotViolet(t *testing.T) {
	fg, ok := projectHeaderStyle().GetForeground().(lipgloss.Color)
	if !ok {
		t.Fatalf("projectHeaderStyle fg is not a lipgloss.Color; got %T", projectHeaderStyle().GetForeground())
	}
	if string(fg) == "99" {
		t.Errorf("projectHeaderStyle fg should NOT be violet (99); palette demotion regressed")
	}
	if string(fg) != "231" {
		t.Errorf("projectHeaderStyle fg: got %q, want \"231\" (bold-white)", string(fg))
	}
}

// TestRender_NarrowWidthDropsMemColumn: at < 100 cols, the mem cell
// (e.g. "530M 1%") should not appear so the columns fit gracefully.
// Mirrors hosts.Render's tiered drop policy.
func TestRender_NarrowWidthDropsMemColumn(t *testing.T) {
	row := state.GlobalRow{
		Project: "cravd", Name: "fair-comet", Branch: "fair-comet",
		Status: state.StatusReady, Alive: true, Port: 41040,
		MemRSS: 530 * 1024 * 1024, CPU: 1.0,
	}
	m := New(Options{})
	m.SetRows([]state.GlobalRow{row})
	m.SetSize(80, 24)
	narrow := stripStyle(m.View())
	if strings.Contains(narrow, "530M") {
		t.Errorf("at width=80 the mem cell should drop; got '530M' in output:\n%s", narrow)
	}
	m.SetSize(120, 24)
	wide := stripStyle(m.View())
	if !strings.Contains(wide, "530M") {
		t.Errorf("at width=120 the mem cell should render; got no '530M' in output:\n%s", wide)
	}
}

// TestNextVisible: the cursor walker is small enough to unit-test in
// isolation. Confirms direction handling and the "fell off the end"
// fallback (returns from unchanged).
func TestNextVisible(t *testing.T) {
	cases := []struct {
		name   string
		hidden []bool
		from   int
		delta  int
		want   int
	}{
		{"start visible, no move needed", []bool{false, true, false}, 0, 1, 0},
		{"step forward past hidden", []bool{false, true, false}, 1, 1, 2},
		{"step backward past hidden", []bool{false, true, false}, 1, -1, 0},
		{"all hidden, return from", []bool{true, true, true}, 0, 1, 0},
		{"empty slice", []bool{}, 0, 1, 0},
		{"delta zero is a no-op", []bool{false}, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextVisible(tc.hidden, tc.from, tc.delta)
			if got != tc.want {
				t.Errorf("nextVisible(%v, %d, %d) = %d; want %d", tc.hidden, tc.from, tc.delta, got, tc.want)
			}
		})
	}
}
