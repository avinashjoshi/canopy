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

// TestLsJSONOutput_ClipboardBridgeField asserts the v0.18 Lane C.3
// schema bump (clipboard_bridge) round-trips through the wire format.
// Drift between LsJSONOutput and host.remoteLsResponse would silently
// lose the field on the laptop side, leaving the Hosts-tab pill
// permanently neutral even on bridged hosts.
func TestLsJSONOutput_ClipboardBridgeField(t *testing.T) {
	in := LsJSONOutput{
		SchemaVersion:   lsJSONSchemaVersion,
		CanopyVersion:   "0.18.0+test",
		GeneratedAt:     time.Unix(1700000000, 0).UTC().Format(time.RFC3339),
		ClipboardBridge: "bridged",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"clipboard_bridge":"bridged"`) {
		t.Errorf("emitted JSON missing clipboard_bridge: %s", data)
	}
	var out LsJSONOutput
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ClipboardBridge != "bridged" {
		t.Errorf("clipboard_bridge round-trip failed: got %q", out.ClipboardBridge)
	}
}

func TestLsJSONOutput_ClipboardBridgeOmitWhenEmpty(t *testing.T) {
	// An old canopy (pre-v0.18) reports an empty string; we must omit
	// the field from JSON so older laptops don't get confused.
	in := LsJSONOutput{
		SchemaVersion: lsJSONSchemaVersion,
		CanopyVersion: "0.17.5",
	}
	data, _ := json.Marshal(in)
	if strings.Contains(string(data), "clipboard_bridge") {
		t.Errorf("empty ClipboardBridge leaked into JSON: %s", data)
	}
}

func TestLsJSONSchemaVersion_BumpedForClipboardBridge(t *testing.T) {
	// Schema version must bump when the wire format changes. If someone
	// reverts a field without reverting the schema bump (or vice versa),
	// this test catches the drift. v0.22 added owner + source_kind → 6.
	if lsJSONSchemaVersion != 6 {
		t.Errorf("lsJSONSchemaVersion = %d, want 6 (v0.20 added clipboard_bridge; v0.22 added owner + source_kind)", lsJSONSchemaVersion)
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
	for _, key := range []string{`"mem_rss"`, `"cpu"`, `"hints"`, `"last_error_hint"`, `"port"`, `"attached"`} {
		if strings.Contains(s, key) {
			t.Errorf("empty workspace emitted %s: %s", key, s)
		}
	}
}

// TestLsJSONWorkspace_AttachedRoundTrip verifies the v0.19
// remote-status-observability wire-format addition: Attached round-trips
// through JSON so the laptop's host.RemoteWorkspace decode picks it up
// and the cache merge stamps it onto GlobalRow.Attached. Without this,
// remote rows always rendered as detached and the confirm-attach modal
// never fired for them.
func TestLsJSONWorkspace_AttachedRoundTrip(t *testing.T) {
	in := LsJSONWorkspace{
		Name: "foo", Project: "p", Branch: "b",
		Status: "ready", TmuxSession: "p/foo", Alive: true,
		Attached: true,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"attached":true`) {
		t.Errorf("emitted JSON missing attached=true: %s", s)
	}
	var out LsJSONWorkspace
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.Attached {
		t.Errorf("Attached lost in round-trip: in=true out=false")
	}
}

// TestLsJSONSchemaVersion_BumpedForV019 guards against a future PR
// adding a field but forgetting to bump the schema version. Older
// laptops parse additive fields fine (json tag `omitempty` + unknown
// fields ignored), but the version is a coordination signal for
// breaking changes — we still bump it on every additive change so a
// `--require-schema=N` flag stays meaningful.
func TestLsJSONSchemaVersion_BumpedForV019(t *testing.T) {
	if lsJSONSchemaVersion < 4 {
		t.Errorf("lsJSONSchemaVersion = %d, want >= 4 (v0.19 added attached + motion-aware agent_state)", lsJSONSchemaVersion)
	}
}
