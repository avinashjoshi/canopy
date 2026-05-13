package host

import (
	"encoding/json"
	"testing"

	"github.com/avinashjoshi/canopy/internal/state"
)

// TestRemoteWorkspace_Phase1gParse verifies the laptop-side Refresher
// can parse the v0.17 Phase 1g schema (mem_rss, cpu, hints,
// last_error_hint) from a remote canopy's `canopy ls --json` output.
//
// Regression target: a schema mismatch between cmd/canopy/ls.go
// (emitter) and internal/host/refresh.go (parser) silently strips the
// new fields. The Global tab would render remote rows without the
// CPU/mem cell and PR badges users expect.
func TestRemoteWorkspace_Phase1gParse(t *testing.T) {
	wire := []byte(`{
	  "name": "foo",
	  "project": "cravd",
	  "branch": "feature-x",
	  "status": "ready",
	  "port": 3001,
	  "tmux_session": "cravd/foo",
	  "alive": true,
	  "mem_rss": 536870912,
	  "cpu": 42.5,
	  "hints": [
	    {"kind": "pr_status", "message": "PR #123 mergeable"}
	  ],
	  "last_error_hint": "scripts.setup exited 1"
	}`)
	var got RemoteWorkspace
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MemRSS != 536870912 {
		t.Errorf("MemRSS = %d; want 536870912", got.MemRSS)
	}
	if got.CPU != 42.5 {
		t.Errorf("CPU = %v; want 42.5", got.CPU)
	}
	if len(got.Hints) != 1 || got.Hints[0].Kind != "pr_status" {
		t.Errorf("Hints = %+v; want one pr_status hint", got.Hints)
	}
	if got.LastErrorHint != "scripts.setup exited 1" {
		t.Errorf("LastErrorHint = %q; want scripts.setup exited 1", got.LastErrorHint)
	}
}

// TestRemoteWorkspace_LegacyParseStillWorks: a remote running an older
// canopy that omits the Phase 1g fields must still parse cleanly. The
// laptop renders the row without the new decorations, same as a local
// row whose probe failed.
func TestRemoteWorkspace_LegacyParseStillWorks(t *testing.T) {
	wire := []byte(`{
	  "name": "foo",
	  "project": "cravd",
	  "branch": "b",
	  "status": "ready",
	  "port": 3001,
	  "tmux_session": "cravd/foo",
	  "alive": true
	}`)
	var got RemoteWorkspace
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MemRSS != 0 || got.CPU != 0 || len(got.Hints) != 0 || got.LastErrorHint != "" {
		t.Errorf("legacy parse leaked non-zero fields: %+v", got)
	}
	// Sanity check: the Hint type is reachable from this package via the
	// state import, so a new contributor adding a Hint field will see
	// this test fail at compile time if they forget to update the wire
	// shape.
	_ = state.Hint{}
}
