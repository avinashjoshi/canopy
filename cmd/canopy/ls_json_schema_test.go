package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/avinashjoshi/canopy/internal/state"
)

// TestLsJSONWorkspace_Phase1gFields verifies the v0.17 Phase 1g
// additions (mem_rss, cpu, hints, last_error_hint) round-trip through
// the JSON wire format the laptop refresher parses.
//
// Regression target: the laptop's Refresher unmarshals into
// host.RemoteWorkspace which mirrors this schema. Drift between the two
// shapes silently strips fields the Global tab needs for parity.
func TestLsJSONWorkspace_Phase1gFields(t *testing.T) {
	in := LsJSONWorkspace{
		Name:        "foo",
		Project:     "cravd",
		Branch:      "feature-x",
		Status:      string(state.StatusReady),
		Port:        3001,
		TmuxSession: "cravd/foo",
		Alive:       true,
		MemRSS:      512 * 1024 * 1024,
		CPU:         42.5,
		Hints: []state.Hint{
			{Kind: "pr_status", Message: "PR #123 mergeable", DetectedAt: time.Unix(1700000000, 0).UTC()},
		},
		LastErrorHint: "scripts.setup exited 1: bundle install failed",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out LsJSONWorkspace
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.MemRSS != in.MemRSS {
		t.Errorf("MemRSS lost: in=%d out=%d", in.MemRSS, out.MemRSS)
	}
	if out.CPU != in.CPU {
		t.Errorf("CPU lost: in=%v out=%v", in.CPU, out.CPU)
	}
	if len(out.Hints) != 1 || out.Hints[0].Kind != "pr_status" {
		t.Errorf("Hints lost: %+v", out.Hints)
	}
	if out.LastErrorHint != in.LastErrorHint {
		t.Errorf("LastErrorHint lost: in=%q out=%q", in.LastErrorHint, out.LastErrorHint)
	}
	// Wire-format spot check: the JSON keys must be the snake_case names
	// the refresher expects.
	s := string(data)
	for _, want := range []string{`"mem_rss"`, `"cpu"`, `"hints"`, `"last_error_hint"`, `"kind"`} {
		if !strings.Contains(s, want) {
			t.Errorf("emitted JSON missing key %s: %s", want, s)
		}
	}
}

// TestCanopyVersionInfo_TracksPackageVersion verifies the wire format
// reports the package-level `version` var (stripped of the "v" prefix)
// so the laptop Hosts tab can render `v` + canopy_version without
// double-prefixing, and so a remote canopy never reports as
// "(unknown)" the way it did before init() was wired up.
//
// Regression target: canopyVersionInfo used to default to "(unknown)"
// and was never assigned, so every remote reported itself as unknown
// in the Hosts tab.
func TestCanopyVersionInfo_TracksPackageVersion(t *testing.T) {
	if version == "" {
		t.Fatal("package version is empty — bare build should still default to \"dev\"")
	}
	// canopyVersionInfo is init()-set; in the test binary `version`
	// is "dev" (no ldflags), so canopyVersionInfo should be "dev" too.
	if canopyVersionInfo == "(unknown)" {
		t.Errorf("canopyVersionInfo still \"(unknown)\" — init() didn't wire it from `version`")
	}
	want := strings.TrimPrefix(version, "v")
	if canopyVersionInfo != want {
		t.Errorf("canopyVersionInfo = %q, want %q (version=%q stripped)", canopyVersionInfo, want, version)
	}
	// And it must not start with "v" — display layer adds that.
	if strings.HasPrefix(canopyVersionInfo, "v") {
		t.Errorf("canopyVersionInfo leaked the leading \"v\": %q", canopyVersionInfo)
	}
}

// TestLsJSONWorkspace_OmitEmptyKeepsPayloadLean: a fully-empty workspace
// (no port, no load, no hints, no diagnosis) should not emit those keys
// at all. Keeps the payload from ballooning across 50-workspace hosts.
func TestLsJSONWorkspace_OmitEmptyKeepsPayloadLean(t *testing.T) {
	in := LsJSONWorkspace{
		Name: "bare", Project: "p", Branch: "b",
		Status: "ready", TmuxSession: "p/bare", Alive: false,
	}
	data, _ := json.Marshal(in)
	s := string(data)
	for _, key := range []string{`"mem_rss"`, `"cpu"`, `"hints"`, `"last_error_hint"`, `"port"`} {
		if strings.Contains(s, key) {
			t.Errorf("empty workspace emitted %s: %s", key, s)
		}
	}
}
