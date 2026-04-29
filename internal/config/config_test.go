package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oncactus/canopy/internal/clog"
	"github.com/oncactus/canopy/internal/config"
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
		`{"scripts": {"setup": "s"}}`,        // partial: only setup
		`{"scripts": {"run": "bin/dev"}}`,    // partial: only run
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
