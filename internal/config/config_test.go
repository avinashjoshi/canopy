package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/config"
)

func TestMain(m *testing.M) {
	teardown, _ := clog.Init(false)
	defer teardown()
	m.Run()
}

// validJSON is a minimal-but-complete canopy.json shared by happy-path tests.
const validJSON = `{
  "scripts": {
    "setup": "bin/canopy-setup",
    "run": "bin/dev",
    "archive": "bin/canopy-archive"
  }
}`

// TestDiscover_HappyPath: canopy.json at cwd, Discover finds it without
// walking.
func TestDiscover_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "canopy.json"), validJSON)

	got, err := config.Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != filepath.Join(dir, "canopy.json") {
		t.Errorf("Discover = %q; want %q", got, filepath.Join(dir, "canopy.json"))
	}
}

// TestDiscover_WalksUp: canopy.json at the project root, Discover starts
// from a deep subdirectory and walks up to find it.
func TestDiscover_WalksUp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(root, "canopy.json"), validJSON)

	got, err := config.Discover(deep)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != filepath.Join(root, "canopy.json") {
		t.Errorf("Discover = %q; want %q", got, filepath.Join(root, "canopy.json"))
	}
}

// TestDiscover_NotFound: no canopy.json anywhere up to the filesystem root,
// Discover returns ErrNotFound (not a panic, not a hang).
func TestDiscover_NotFound(t *testing.T) {
	t.Parallel()
	// t.TempDir is normally under /tmp which has no canopy.json. If for some
	// reason a sibling test or a hostile environment created one above, the
	// test would be wrong — but in practice this is fine.
	_, err := config.Discover(t.TempDir())
	if !errors.Is(err, config.ErrNotFound) {
		t.Errorf("Discover(no-canopy-anywhere): got %v; want errors.Is(... ErrNotFound)", err)
	}
}

// TestLoad_HappyPath verifies the full parse + derived-fields flow.
func TestLoad_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "canopy.json")
	writeFile(t, path, validJSON)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scripts.Setup != "bin/canopy-setup" {
		t.Errorf("Setup = %q", cfg.Scripts.Setup)
	}
	if cfg.Scripts.Run != "bin/dev" {
		t.Errorf("Run = %q", cfg.Scripts.Run)
	}
	if cfg.Scripts.Archive != "bin/canopy-archive" {
		t.Errorf("Archive = %q", cfg.Scripts.Archive)
	}
	if cfg.Project != filepath.Base(dir) {
		t.Errorf("Project = %q; want %q", cfg.Project, filepath.Base(dir))
	}
	if cfg.ProjectRoot == "" {
		t.Error("ProjectRoot is empty after Load")
	}
}

// TestLoad_BadJSON covers parse failure: the file exists but isn't valid JSON.
func TestLoad_BadJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "canopy.json")
	writeFile(t, path, `{"scripts": {`) // truncated

	_, err := config.Load(path)
	if !errors.Is(err, config.ErrInvalid) {
		t.Errorf("Load(bad-json): got %v; want errors.Is(... ErrInvalid)", err)
	}
}

// TestLoad_OptionalScripts confirms that scripts fields are optional —
// canopy.json with `{}` or `{"scripts": {}}` parses fine. canopy creates
// workspaces with no setup/run/archive hooks in that case.
func TestLoad_OptionalScripts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "canopy.json")

	cases := []string{
		`{}`,
		`{"scripts": {}}`,
		`{"scripts": {"setup": "s"}}`,     // partial: only setup
		`{"scripts": {"run": "bin/dev"}}`, // partial: only run
		`{"scripts": {"setup": "s", "run": "r"}}`, // partial: no archive
	}
	for _, body := range cases {
		writeFile(t, path, body)
		if _, err := config.Load(path); err != nil {
			t.Errorf("Load(%q): %v; want no error", body, err)
		}
	}
}

// TestLoad_FileMissing covers the case where Discover succeeded but Load
// is given a bad path. Should be a plain os error, not a sentinel.
func TestLoad_FileMissing(t *testing.T) {
	t.Parallel()
	_, err := config.Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Errorf("Load(missing-file): want error; got nil")
	}
	if errors.Is(err, config.ErrInvalid) {
		t.Errorf("Load(missing-file) returned ErrInvalid; should be a plain io error")
	}
}

// TestDiscoverAndLoad: convenience wrapper that's the most common entry
// point for the CLI.
func TestDiscoverAndLoad(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deep := filepath.Join(root, "sub", "dir")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(root, "canopy.json"), validJSON)

	cfg, err := config.DiscoverAndLoad(deep)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}
	if cfg.Project != filepath.Base(root) {
		t.Errorf("Project = %q; want %q", cfg.Project, filepath.Base(root))
	}
}

// writeFile is a tiny test helper for setting up canopy.json fixtures.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// TestLoad_AgentDefaults: a canopy.json without an `agent` block must
// load cleanly and Agent.Type must default to "claude". This is the
// backwards-compat path: every existing canopy.json from v0.5 and earlier
// has no agent block, and they all need to keep working.
func TestLoad_AgentDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "canopy.json"), validJSON)

	cfg, err := config.DiscoverAndLoad(dir)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}
	if cfg.Agent.Type != "claude" {
		t.Errorf("Agent.Type default = %q; want %q (backwards-compat)", cfg.Agent.Type, "claude")
	}
	if cfg.Agent.Briefing != "" {
		t.Errorf("Agent.Briefing should be empty by default; got %q", cfg.Agent.Briefing)
	}
	if cfg.Agent.BriefingFile != "" {
		t.Errorf("Agent.BriefingFile should be empty by default; got %q", cfg.Agent.BriefingFile)
	}
}

// TestLoad_AgentExplicit: a canopy.json that DOES set the agent block
// must round-trip its values (type / briefing / briefing_file).
func TestLoad_AgentExplicit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configJSON := `{
  "scripts": {
    "setup": "bin/canopy-setup",
    "run": "bin/dev",
    "archive": "bin/canopy-archive"
  },
  "agent": {
    "type": "opencode",
    "briefing": "Project uses Rails 7. Run RSpec for tests.",
    "briefing_file": "docs/AGENT_BRIEFING.md"
  }
}`
	writeFile(t, filepath.Join(dir, "canopy.json"), configJSON)

	cfg, err := config.DiscoverAndLoad(dir)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}
	if cfg.Agent.Type != "opencode" {
		t.Errorf("Agent.Type = %q; want %q", cfg.Agent.Type, "opencode")
	}
	if cfg.Agent.Briefing == "" {
		t.Errorf("Agent.Briefing should be populated")
	}
	if cfg.Agent.BriefingFile != "docs/AGENT_BRIEFING.md" {
		t.Errorf("Agent.BriefingFile = %q; want %q", cfg.Agent.BriefingFile, "docs/AGENT_BRIEFING.md")
	}
	// Both Briefing AND BriefingFile set: validate() warns at log level
	// but doesn't error. Test only verifies no error here; the warning
	// goes to slog and is hard to assert in a unit test.
}

// TestLoad_Agents_Precedence covers the v0.22 schema rules: the new
// `agents` plural array wins when both forms are present, the legacy
// `agent` form is promoted to a single-element list when alone, and
// neither falls back to ["claude"]. Each case asserts both Agents[]
// AND Agent.Type so the legacy field stays consistent with the
// canonical Agents source.
func TestLoad_Agents_Precedence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		json        string
		wantAgents  []string
		wantPrimary string
	}{
		{
			name: "neither: default to claude",
			json: `{"scripts":{"setup":"","run":"","archive":""}}`,
			wantAgents:  []string{"claude"},
			wantPrimary: "claude",
		},
		{
			name: "legacy agent.type alone is promoted to agents=[type]",
			json: `{"scripts":{"setup":"","run":"","archive":""},"agent":{"type":"codex"}}`,
			wantAgents:  []string{"codex"},
			wantPrimary: "codex",
		},
		{
			name: "agents alone: round-trip + Agent.Type tracks agents[0]",
			json: `{"scripts":{"setup":"","run":"","archive":""},"agents":["codex","claude"]}`,
			wantAgents:  []string{"codex", "claude"},
			wantPrimary: "codex",
		},
		{
			name: "both present: agents silently wins; Agent.Type aligns to agents[0]",
			json: `{"scripts":{"setup":"","run":"","archive":""},"agent":{"type":"claude"},"agents":["aider","codex"]}`,
			wantAgents:  []string{"aider", "codex"},
			wantPrimary: "aider",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "canopy.json"), tc.json)
			cfg, err := config.DiscoverAndLoad(dir)
			if err != nil {
				t.Fatalf("DiscoverAndLoad: %v", err)
			}
			if !equalStrings(cfg.Agents, tc.wantAgents) {
				t.Errorf("Agents = %v; want %v", cfg.Agents, tc.wantAgents)
			}
			if cfg.Agent.Type != tc.wantPrimary {
				t.Errorf("Agent.Type = %q; want %q", cfg.Agent.Type, tc.wantPrimary)
			}
			if got := cfg.DefaultAgent(); got != tc.wantPrimary {
				t.Errorf("DefaultAgent() = %q; want %q", got, tc.wantPrimary)
			}
		})
	}
}

// TestAddAgentToCanopyJSON_AppendsNewAgent: the auto-add path on a
// project whose canopy.json had no `agents` field AND no legacy
// `agent: {type}` block writes ["claude", "codex"] — the implicit
// "claude" default is preserved, and the new agent is appended.
// Unknown top-level keys must round-trip untouched.
func TestAddAgentToCanopyJSON_AppendsNewAgent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initial := `{
  "scripts": {"setup": "x", "run": "x", "archive": "x"},
  "custom_user_field": "preserve me"
}`
	writeFile(t, filepath.Join(dir, "canopy.json"), initial)

	if err := config.AddAgentToCanopyJSON(dir, "codex"); err != nil {
		t.Fatalf("AddAgentToCanopyJSON: %v", err)
	}

	cfg, err := config.LoadFrom(dir)
	if err != nil {
		t.Fatalf("LoadFrom post-add: %v", err)
	}
	if !equalStrings(cfg.Agents, []string{"claude", "codex"}) {
		t.Errorf("Agents = %v; want [claude codex] (implicit claude preserved + codex appended)", cfg.Agents)
	}
	// Unknown key survives.
	raw, _ := os.ReadFile(filepath.Join(dir, "canopy.json"))
	if !strings.Contains(string(raw), `"preserve me"`) {
		t.Errorf("custom_user_field clobbered; full file:\n%s", string(raw))
	}
}

// TestAddAgentToCanopyJSON_AppendsToExistingList: project already
// has an agents list; new entry gets appended (not replacing).
func TestAddAgentToCanopyJSON_AppendsToExistingList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initial := `{
  "scripts": {"setup": "x", "run": "x", "archive": "x"},
  "agents": ["claude"]
}`
	writeFile(t, filepath.Join(dir, "canopy.json"), initial)

	if err := config.AddAgentToCanopyJSON(dir, "codex"); err != nil {
		t.Fatalf("AddAgentToCanopyJSON: %v", err)
	}
	cfg, _ := config.LoadFrom(dir)
	if !equalStrings(cfg.Agents, []string{"claude", "codex"}) {
		t.Errorf("Agents = %v; want [claude codex]", cfg.Agents)
	}
}

// TestAddAgentToCanopyJSON_IdempotentNoOp: adding an agent that's
// already in the list is a no-op (returns nil, file unchanged).
// Catches the dumb regression where idempotency gets dropped during
// a refactor and we start duplicating entries.
func TestAddAgentToCanopyJSON_IdempotentNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initial := `{
  "scripts": {"setup": "x", "run": "x", "archive": "x"},
  "agents": ["claude", "codex"]
}`
	path := filepath.Join(dir, "canopy.json")
	writeFile(t, path, initial)

	statBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	// Sleep slightly so a mtime change would be detectable.
	time.Sleep(20 * time.Millisecond)

	if err := config.AddAgentToCanopyJSON(dir, "codex"); err != nil {
		t.Fatalf("AddAgentToCanopyJSON: %v", err)
	}
	statAfter, _ := os.Stat(path)
	if !statBefore.ModTime().Equal(statAfter.ModTime()) {
		t.Errorf("file mtime changed on idempotent add (before=%v after=%v); want no write",
			statBefore.ModTime(), statAfter.ModTime())
	}
}

// TestAddAgentToCanopyJSON_PreservesLegacyAgentType pins the codex
// review fix (P1 #5, 2026-06-25): a project whose canopy.json declared
// `agent: {type: "claude"}` and NO `agents:` array must end up with
// `agents: ["claude", "codex"]` after auto-adding codex — NOT
// `agents: ["codex"]` (which would silently drop claude).
func TestAddAgentToCanopyJSON_PreservesLegacyAgentType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initial := `{
  "scripts": {"setup": "x", "run": "x", "archive": "x"},
  "agent": {"type": "claude"}
}`
	writeFile(t, filepath.Join(dir, "canopy.json"), initial)

	if err := config.AddAgentToCanopyJSON(dir, "codex"); err != nil {
		t.Fatalf("AddAgentToCanopyJSON: %v", err)
	}
	cfg, err := config.LoadFrom(dir)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !equalStrings(cfg.Agents, []string{"claude", "codex"}) {
		t.Errorf("Agents = %v; want [claude codex] (claude must NOT be dropped)", cfg.Agents)
	}
}

// TestAddAgentToCanopyJSON_PreservesImplicitClaudeDefault: a project
// with NEITHER agents nor agent.type implicitly defaults to claude
// (validate() does this on Load). Auto-adding codex must produce
// ["claude", "codex"], not ["codex"]. Sister test to the legacy
// agent.type case above.
func TestAddAgentToCanopyJSON_PreservesImplicitClaudeDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initial := `{
  "scripts": {"setup": "x", "run": "x", "archive": "x"}
}`
	writeFile(t, filepath.Join(dir, "canopy.json"), initial)

	if err := config.AddAgentToCanopyJSON(dir, "codex"); err != nil {
		t.Fatalf("AddAgentToCanopyJSON: %v", err)
	}
	cfg, err := config.LoadFrom(dir)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !equalStrings(cfg.Agents, []string{"claude", "codex"}) {
		t.Errorf("Agents = %v; want [claude codex] (implicit claude must NOT be dropped)", cfg.Agents)
	}
}

// TestAddAgentToCanopyJSON_EmptyAgentRejected: empty agent name is
// an error (defensive — no way to add "" usefully).
func TestAddAgentToCanopyJSON_EmptyAgentRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "canopy.json"), `{"scripts":{"setup":"x","run":"x","archive":"x"}}`)
	if err := config.AddAgentToCanopyJSON(dir, ""); err == nil {
		t.Error("AddAgentToCanopyJSON(\"\") = nil; want error")
	}
}

// TestConfig_AllowsAgent covers the gate for --agent / canopy agent swap.
// Empty argument is never allowed; declared types are; undeclared types
// are rejected so the caller can surface ErrAgentNotAllowed.
func TestConfig_AllowsAgent(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Agents: []string{"claude", "codex"}}
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"claude", true},
		{"codex", true},
		{"aider", false},
		{"future-thing", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := cfg.AllowsAgent(tc.in); got != tc.want {
				t.Errorf("AllowsAgent(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
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

// TestLoad_AgentEmptyBlock: an explicitly-empty agent block (`"agent": {}`)
// behaves the same as omitting the block — Type defaults to "claude".
func TestLoad_AgentEmptyBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configJSON := `{
  "scripts": {"setup": "", "run": "", "archive": ""},
  "agent": {}
}`
	writeFile(t, filepath.Join(dir, "canopy.json"), configJSON)

	cfg, err := config.DiscoverAndLoad(dir)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}
	if cfg.Agent.Type != "claude" {
		t.Errorf("Agent.Type for empty block = %q; want %q", cfg.Agent.Type, "claude")
	}
}

// TestLoadFrom_HappyPath: LoadFrom reads <root>/canopy.json directly
// without any walk-up.
func TestLoadFrom_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "canopy.json"), validJSON)

	cfg, err := config.LoadFrom(dir)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Project == "" {
		t.Errorf("LoadFrom returned empty Project")
	}
	if cfg.ProjectRoot == "" {
		t.Errorf("LoadFrom returned empty ProjectRoot")
	}
}

// TestLoadFrom_NotFound: missing canopy.json at the given root returns
// ErrNotFound (not a generic error). Lets callers distinguish "config
// gone" from "config corrupt" without parsing error strings.
func TestLoadFrom_NotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Note: do NOT write canopy.json
	_, err := config.LoadFrom(dir)
	if err == nil {
		t.Fatalf("LoadFrom missing-file: want error, got nil")
	}
	if !errors.Is(err, config.ErrNotFound) {
		t.Errorf("LoadFrom missing-file: want ErrNotFound, got %v", err)
	}
}

// TestLoadFrom_NoWalkUp: LoadFrom must NOT walk up the directory tree.
// Discover walks; LoadFrom is the targeted variant for callers who
// already know the canonical root (e.g. cross-project ops in the unified
// TUI). Verify by placing canopy.json at the parent and asking for the
// child — must error.
func TestLoadFrom_NoWalkUp(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, "canopy.json"), validJSON)
	child := filepath.Join(parent, "subdir")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := config.LoadFrom(child)
	if err == nil {
		t.Fatalf("LoadFrom child-dir: want error, got nil (LoadFrom must NOT walk up)")
	}
	if !errors.Is(err, config.ErrNotFound) {
		t.Errorf("LoadFrom child-dir: want ErrNotFound, got %v", err)
	}
}

// TestLoadFrom_BadJSON: parse failure surfaces ErrInvalid the same way
// Load does. LoadFrom is just Load with a different lookup, so it should
// inherit Load's error semantics.
func TestLoadFrom_BadJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "canopy.json"), `{not valid json`)

	_, err := config.LoadFrom(dir)
	if err == nil {
		t.Fatalf("LoadFrom bad-json: want error, got nil")
	}
	if !errors.Is(err, config.ErrInvalid) {
		t.Errorf("LoadFrom bad-json: want ErrInvalid, got %v", err)
	}
}

// TestLoad_ScriptsAgentOverride: scripts.agent (the power-user override
// path) round-trips alongside the agent block.
func TestLoad_ScriptsAgentOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configJSON := `{
  "scripts": {
    "setup": "bin/canopy-setup",
    "run": "bin/dev",
    "archive": "bin/canopy-archive",
    "agent": "bin/my-agent-launcher"
  },
  "agent": {"type": "claude"}
}`
	writeFile(t, filepath.Join(dir, "canopy.json"), configJSON)

	cfg, err := config.DiscoverAndLoad(dir)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}
	if cfg.Scripts.Agent != "bin/my-agent-launcher" {
		t.Errorf("Scripts.Agent = %q; want %q", cfg.Scripts.Agent, "bin/my-agent-launcher")
	}
	if cfg.Agent.Type != "claude" {
		t.Errorf("Agent.Type = %q; want %q", cfg.Agent.Type, "claude")
	}
}
