package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/state"
)

// names extracts row names in order — a compact way to assert ordering.
func names(rows []state.GlobalRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSortRowsMineFirst: within a section, mine floats above reviewing,
// the main row stays pinned to the top, and the sort is stable (relative
// order within an ownership band is preserved).
func TestSortRowsMineFirst(t *testing.T) {
	rows := []state.GlobalRow{
		{Project: "p", Name: "review-a", SourceKind: "pr"},          // reviewing
		{Project: "p", IsMain: true, Name: "(main)"},                // main, pinned top
		{Project: "p", Name: "mine-a", SourceKind: "fresh"},         // mine
		{Project: "p", Name: "review-b", Owner: "octocat"},          // reviewing
		{Project: "p", Name: "mine-b", SourceKind: "issue"},         // mine
	}
	sortRowsMineFirst(rows)
	got := names(rows)
	want := []string{"(main)", "mine-a", "mine-b", "review-a", "review-b"}
	if !eq(got, want) {
		t.Errorf("order = %v; want %v", got, want)
	}
}

// TestSortRowsMineFirst_KeepsSectionsSeparate: two different sections
// (a local project and a remote host) must not interleave even if a
// later section's rows would sort ahead — grouping is preserved.
func TestSortRowsMineFirst_KeepsSectionsSeparate(t *testing.T) {
	rows := []state.GlobalRow{
		{Project: "p", ProjectRoot: "/p", Name: "p-review", SourceKind: "pr"},
		{Project: "p", ProjectRoot: "/p", Name: "p-mine", SourceKind: "fresh"},
		{Host: "tower", Project: "q", Name: "q-review", SourceKind: "pr"},
		{Host: "tower", Project: "q", Name: "q-mine", SourceKind: "fresh"},
	}
	sortRowsMineFirst(rows)
	got := names(rows)
	want := []string{"p-mine", "p-review", "q-mine", "q-review"}
	if !eq(got, want) {
		t.Errorf("order = %v; want %v (sections must stay grouped)", got, want)
	}
}

// TestSortRowsMineFirst_RemoteProjectsStayGrouped is the regression for
// the remote-host header-split bug. Remote rows carry an EMPTY ProjectRoot
// (the field is omitempty and absent on older hosts' `canopy ls --json`),
// so when the sort grouped runs on ProjectRoot every project on a host
// collapsed into one run — pinning all (main) rows to the top and splitting
// each project into a "(main)-only" header plus a separate workspaces
// header. Grouping on Project (the renderer's key, always populated) keeps
// each project its own run with its (main) pinned inside it. The rows below
// deliberately leave ProjectRoot empty: this test fails under the old
// ProjectRoot key and passes under the Project key.
func TestSortRowsMineFirst_RemoteProjectsStayGrouped(t *testing.T) {
	rows := []state.GlobalRow{
		{Host: "tower", Project: "canopy", IsMain: true, Name: "(main)"},
		{Host: "tower", Project: "canopy", Name: "free-robin", SourceKind: "fresh"},
		{Host: "tower", Project: "cravd", IsMain: true, Name: "(main)"},
		{Host: "tower", Project: "cravd", Name: "kind-maple", SourceKind: "fresh"},
	}
	sortRowsMineFirst(rows)
	// Each project's (main) must stay adjacent to its own workspaces; the
	// two mains must NOT both float to the top of the host.
	got := names(rows)
	want := []string{"(main)", "free-robin", "(main)", "kind-maple"}
	if !eq(got, want) {
		t.Errorf("order = %v; want %v (per-project grouping must hold)", got, want)
	}
	// Belt-and-suspenders: the project column must not flip-flop, i.e. all
	// rows of a project are contiguous (no "canopy, cravd, canopy" headers).
	seen := map[string]bool{}
	prev := ""
	for _, r := range rows {
		if r.Project != prev {
			if seen[r.Project] {
				t.Errorf("project %q reappears after another project; rows not grouped", r.Project)
			}
			seen[r.Project] = true
			prev = r.Project
		}
	}
}

// TestFilteredRows_HideReviewing covers both states of the `m` toggle:
// off shows every row, on drops the reviewing rows (but keeps mine and
// the main row).
func TestFilteredRows_HideReviewing(t *testing.T) {
	mk := func(hide bool) *Model {
		return &Model{
			tab: tabGlobal,
			allRows: []state.GlobalRow{
				{ProjectRoot: "/p", IsMain: true, Name: "(main)"},
				{ProjectRoot: "/p", Name: "mine", SourceKind: "fresh"},
				{ProjectRoot: "/p", Name: "review", SourceKind: "pr"},
			},
			hideReviewing: hide,
		}
	}

	all := names(mk(false).filteredRows())
	if len(all) != 3 {
		t.Errorf("filter off: got %v; want all 3 rows", all)
	}

	on := mk(true)
	got := names(on.filteredRows())
	want := []string{"(main)", "mine"}
	if !eq(got, want) {
		t.Errorf("filter on: got %v; want %v (review row hidden)", got, want)
	}
	// The hidden-count must be tracked so the view can show the banner —
	// a hidden row should read as "filtered", not "missing data".
	if on.reviewHiddenCount != 1 {
		t.Errorf("reviewHiddenCount = %d; want 1", on.reviewHiddenCount)
	}

	// Filter off: nothing hidden, count resets to 0.
	off := mk(false)
	off.filteredRows()
	if off.reviewHiddenCount != 0 {
		t.Errorf("reviewHiddenCount with filter off = %d; want 0", off.reviewHiddenCount)
	}
}

// TestActionSetOwner_GuardsNonWorkspaceRows: pressing `o` on the
// synthetic main row (or a loading placeholder) must not open the modal —
// those aren't real workspaces and can't carry an owner.
func TestActionSetOwner_GuardsMainRow(t *testing.T) {
	m := &Model{mode: listMode}
	m.list.SetRows([]state.GlobalRow{{IsMain: true, Name: "(main)"}})
	m2, _ := actionSetOwner(m, tea.KeyMsg{})
	if mm := m2.(*Model); mm.mode != listMode {
		t.Errorf("mode = %v after `o` on main row; want listMode (no modal)", mm.mode)
	}
}

// TestSubmitOwner_RejectsEmpty: an empty textinput submit is rejected
// with an inline error, NOT treated as a clear — clearing is the
// explicit ctrl+d path. Guards against the fat-finger wipe.
func TestSubmitOwner_RejectsEmpty(t *testing.T) {
	m := &Model{mode: ownerFormMode}
	m.ownerTarget = Row{Name: "ws", SourceKind: "fresh"}
	m.ownerInput.SetValue("")
	m2, _ := m.submitOwner(false)
	mm := m2.(*Model)
	if mm.mode != ownerFormMode {
		t.Errorf("mode = %v; want ownerFormMode (stay open on empty submit)", mm.mode)
	}
	if mm.ownerError == "" {
		t.Errorf("ownerError empty; want an inline rejection message")
	}
}

// TestToggleReviewFilter flips the flag both ways.
func TestToggleReviewFilter(t *testing.T) {
	m := &Model{}
	m2, _ := actionToggleReviewFilter(m, tea.KeyMsg{})
	if !m2.(*Model).hideReviewing {
		t.Errorf("first toggle: hideReviewing = false; want true")
	}
	m3, _ := actionToggleReviewFilter(m2.(*Model), tea.KeyMsg{})
	if m3.(*Model).hideReviewing {
		t.Errorf("second toggle: hideReviewing = true; want false")
	}
}
