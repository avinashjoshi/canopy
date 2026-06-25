package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
)

// TestOwnerWireDrift is the drift guard for the owner feature. The owner
// + source_kind fields are hand-duplicated across four wire structs:
// LsJSONWorkspace (emit), host.RemoteWorkspace (parse), and
// state.RemoteWorkspaceRow (cache). They all share the json tags
// "owner"/"source_kind". If someone adds the field to one struct but
// forgets another, the value silently drops on the wire — this test
// round-trips a value through all three to catch that at compile+test
// time rather than as a missing pill in production.
func TestOwnerWireDrift(t *testing.T) {
	const wantOwner = "octocat"
	const wantKind = "pr"

	// 1. Emit shape (what `canopy ls --json` writes).
	emit := LsJSONWorkspace{
		Name: "pr-1", Project: "p", Branch: "b", Status: "ready",
		TmuxSession: "p/pr-1", Owner: wantOwner, SourceKind: wantKind,
	}
	wire, err := json.Marshal(emit)
	if err != nil {
		t.Fatalf("marshal LsJSONWorkspace: %v", err)
	}
	if !strings.Contains(string(wire), `"owner":"octocat"`) ||
		!strings.Contains(string(wire), `"source_kind":"pr"`) {
		t.Fatalf("emit JSON missing owner/source_kind: %s", wire)
	}

	// 2. Parse shape (what the laptop's host refresher unmarshals).
	var rw host.RemoteWorkspace
	if err := json.Unmarshal(wire, &rw); err != nil {
		t.Fatalf("unmarshal into host.RemoteWorkspace: %v", err)
	}
	if rw.Owner != wantOwner || rw.SourceKind != wantKind {
		t.Errorf("host.RemoteWorkspace dropped fields: owner=%q source_kind=%q", rw.Owner, rw.SourceKind)
	}

	// 3. Cache shape (what persists to remotes-cache.json).
	var row state.RemoteWorkspaceRow
	if err := json.Unmarshal(wire, &row); err != nil {
		t.Fatalf("unmarshal into state.RemoteWorkspaceRow: %v", err)
	}
	if row.Owner != wantOwner || row.SourceKind != wantKind {
		t.Errorf("state.RemoteWorkspaceRow dropped fields: owner=%q source_kind=%q", row.Owner, row.SourceKind)
	}
}
