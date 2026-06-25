package state

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOwnerIsReviewing covers every branch of the three-state owner
// model: explicit self-marker (mine), a foreign login (reviewing), and
// the empty fallback that derives from SourceKind.
func TestOwnerIsReviewing(t *testing.T) {
	cases := []struct {
		name       string
		owner      string
		sourceKind string
		want       bool
	}{
		{"empty + fresh → mine", "", "fresh", false},
		{"empty + issue → mine", "", "issue", false},
		{"empty + branch → mine", "", "branch", false},
		{"empty + pr → legacy review", "", "pr", true},
		{"login → reviewing", "octocat", "fresh", true},
		{"login + pr → reviewing", "octocat", "pr", true},
		{"self-marker → mine", OwnerSelfMarker, "pr", false},
		{"self-marker + fresh → mine", OwnerSelfMarker, "fresh", false},
	}
	for _, tc := range cases {
		if got := OwnerIsReviewing(tc.owner, tc.sourceKind); got != tc.want {
			t.Errorf("%s: OwnerIsReviewing(%q,%q) = %v; want %v",
				tc.name, tc.owner, tc.sourceKind, got, tc.want)
		}
	}
}

// TestOwnerPillLabel covers the rendered pill text for every state.
func TestOwnerPillLabel(t *testing.T) {
	cases := []struct {
		owner, sourceKind, want string
	}{
		{"", "fresh", ""},
		{"", "issue", ""},
		{"", "branch", ""},
		{"", "pr", "REVIEW"},
		{"octocat", "fresh", "@octocat"},
		{"octocat", "pr", "@octocat"},
		{OwnerSelfMarker, "pr", ""},
		{OwnerSelfMarker, "fresh", ""},
	}
	for _, tc := range cases {
		if got := OwnerPillLabel(tc.owner, tc.sourceKind); got != tc.want {
			t.Errorf("OwnerPillLabel(%q,%q) = %q; want %q",
				tc.owner, tc.sourceKind, got, tc.want)
		}
	}
}

// TestNormalizeOwner covers trimming, @-stripping, length cap, and every
// rejection path (empty, control char, the reserved marker).
func TestNormalizeOwner(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantOK  bool
	}{
		{"plain login", "octocat", "octocat", true},
		{"strip leading @", "@octocat", "octocat", true},
		{"trim space", "  octocat  ", "octocat", true},
		{"trim then strip @", "  @octocat ", "octocat", true},
		{"free-form name kept", "Jane Doe", "Jane Doe", true},
		{"empty rejected", "", "", false},
		{"whitespace-only rejected", "   ", "", false},
		{"just @ rejected", "@", "", false},
		{"control char rejected", "oct\x07cat", "", false},
		{"reserved marker rejected", OwnerSelfMarker, "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeOwner(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("%s: NormalizeOwner(%q) = (%q,%v); want (%q,%v)",
				tc.name, tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestNormalizeOwner_LengthCap: an over-long value is truncated, not
// rejected, so a pasted blob still produces a usable (if clipped) owner
// rather than failing the edit outright.
func TestNormalizeOwner_LengthCap(t *testing.T) {
	long := strings.Repeat("a", ownerMaxLen+20)
	got, ok := NormalizeOwner(long)
	if !ok {
		t.Fatalf("NormalizeOwner(long) ok = false; want true")
	}
	if len(got) != ownerMaxLen {
		t.Errorf("len = %d; want %d (capped)", len(got), ownerMaxLen)
	}
}

// TestWorkspaceOwner_LegacyRoundTrip: a state.json row written before
// this feature (no `owner` key) must unmarshal with Owner=="" so it
// reads back as mine (or a legacy review row if pr-sourced) — proving
// the omitempty field needs no migration.
func TestWorkspaceOwner_LegacyRoundTrip(t *testing.T) {
	legacy := `{"name":"old","branch":"old","path":"/x","port":3000,"status":"ready","created_at":"2025-01-01T00:00:00Z","source_kind":"pr"}`
	var ws Workspace
	if err := json.Unmarshal([]byte(legacy), &ws); err != nil {
		t.Fatalf("unmarshal legacy row: %v", err)
	}
	if ws.Owner != "" {
		t.Errorf("legacy Owner = %q; want \"\"", ws.Owner)
	}
	// pr-sourced legacy row with no owner → reads as a review row.
	if !OwnerIsReviewing(ws.Owner, ws.SourceKind) {
		t.Errorf("legacy pr row should read as reviewing")
	}
}

// TestWorkspaceOwner_OmitEmpty: an empty Owner must not emit the key, so
// existing state.json files don't grow a noise field for the common
// (mine) case.
func TestWorkspaceOwner_OmitEmpty(t *testing.T) {
	ws := Workspace{Name: "w", Branch: "b", Path: "/p", Port: 3000, Status: StatusReady}
	data, err := json.Marshal(ws)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "owner") {
		t.Errorf("empty Owner leaked into JSON: %s", data)
	}
}

// TestGlobalRowOwnerMethods: the row-level wrappers agree with the
// free functions they delegate to (so renderers/sorters can call
// row.IsReviewing() / row.OwnerPill()).
func TestGlobalRowOwnerMethods(t *testing.T) {
	r := GlobalRow{Owner: "octocat", SourceKind: "pr"}
	if !r.IsReviewing() || r.OwnerPill() != "@octocat" {
		t.Errorf("review row: IsReviewing=%v pill=%q", r.IsReviewing(), r.OwnerPill())
	}
	mine := GlobalRow{Owner: "", SourceKind: "fresh"}
	if mine.IsReviewing() || mine.OwnerPill() != "" {
		t.Errorf("mine row: IsReviewing=%v pill=%q", mine.IsReviewing(), mine.OwnerPill())
	}
}
