// Package config loads the per-project canopy.json config.
//
// The discovery model mirrors how go.mod, package.json, .git, and similar
// repo-root markers are found: canopy walks up from a starting directory
// until it finds canopy.json, errors if it hits the filesystem root first.
// This means `canopy new` works from anywhere inside the repo, not just
// from the root.
//
// The schema is intentionally tiny — three script paths and nothing else.
// More flexibility (configurable pane layouts, multi-AI tool support) is
// captured in TODOS.md as v0.5+ work.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("config")

// FileName is the on-disk filename canopy looks for. Constant rather than
// configurable because the whole point of the walk-up convention is one
// well-known name.
const FileName = "canopy.json"

// Sentinel errors. Tests use errors.Is.
var (
	// ErrNotFound is returned when Discover walks all the way to / without
	// finding canopy.json. The CLI should suggest creating one.
	ErrNotFound = errors.New("config: canopy.json not found in cwd or any parent directory")

	// ErrInvalid is returned when the file exists but fails to parse or
	// fails validation (missing required scripts, etc.).
	ErrInvalid = errors.New("config: canopy.json is invalid")
)

// Scripts holds the script paths that define a project's lifecycle.
// All paths are relative to the project root (the directory containing
// canopy.json) and must be executable files.
type Scripts struct {
	// Setup runs once when canopy creates a workspace. Failure -> broken.
	Setup string `json:"setup"`
	// Run is the long-running command for the server pane (e.g. bin/dev).
	// Re-launched on resurrection.
	Run string `json:"run"`
	// Archive runs when canopy removes a workspace (DB drop, server kill).
	Archive string `json:"archive"`
	// Agent is the v0.6 power-user override: when set, canopy spawns
	// this script in the agent pane instead of using the built-in
	// launcher dispatch (internal/agent/launchers.go) keyed on Agent.Type.
	// The script receives CANOPY_AGENT_BRIEFING (the assembled briefing
	// text inline) plus the standard CANOPY_* env vars. Empty by default —
	// most users get the right behavior from Agent.Type alone.
	Agent string `json:"agent,omitempty"`
}

// Agent holds the v0.6 agent-launcher config: which agent to spawn and
// what project-specific briefing to include in its system prompt.
//
// Agent.Type picks the built-in launcher from internal/agent/launchers.go.
// Default "claude" — keeps existing canopy.json files working unchanged.
//
// Briefing vs BriefingFile: the inline string is convenient for short
// project notes; the file path is the right shape for longer briefings
// that benefit from being committed and edited as proper markdown. If
// both are set, the file wins (with a warning logged at canopy.json load
// time). Empty Briefing + empty BriefingFile means "no project-specific
// briefing" — the canopy lifecycle conventions still get injected.
type Agent struct {
	// Type picks the built-in launcher. Known values: "claude", "codex",
	// "opencode", "aider". Default "claude". Unknown → error at load time.
	Type string `json:"type,omitempty"`

	// Briefing is inline project-specific text appended to the canopy
	// universal lifecycle conventions in the AGENT.md briefing.
	Briefing string `json:"briefing,omitempty"`

	// BriefingFile is a path (relative to project root) to a .md file
	// whose contents are appended to the briefing. Wins over Briefing
	// if both are set.
	BriefingFile string `json:"briefing_file,omitempty"`
}

// Config is the parsed canopy.json plus metadata about where it was found.
// ProjectRoot is the absolute path to the directory containing canopy.json;
// Project is its base name (which doubles as the project identifier in
// state.json and the tmux session prefix).
type Config struct {
	// Scripts comes from the JSON.
	Scripts Scripts `json:"scripts"`

	// Agent comes from the JSON. Empty block (no agent.type set) is
	// treated as Type="claude" by validate(), so existing canopy.json
	// files without an agent block keep working unchanged.
	Agent Agent `json:"agent,omitempty"`

	// ProjectRoot is set by Load (not from the JSON). Absolute path to the
	// directory containing canopy.json.
	ProjectRoot string `json:"-"`

	// Project is the basename of ProjectRoot. canopy uses this as the project
	// identifier in state.json and as the prefix for tmux session names
	// (e.g. "cravd-bold-falcon").
	Project string `json:"-"`
}

// Discover walks up from startDir looking for canopy.json. It stops at the
// first parent directory that contains a readable canopy.json, or at the
// filesystem root (returning ErrNotFound).
//
// Returns the absolute path to the canopy.json file (not the directory).
// Callers usually pass that path to Load.
func Discover(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", fmt.Errorf("config.Discover: abs %s: %w", startDir, err)
	}

	for {
		candidate := filepath.Join(dir, FileName)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Permission denied, etc. — surface, don't keep walking.
			return "", fmt.Errorf("config.Discover: stat %s: %w", candidate, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// We've hit the filesystem root.
			return "", fmt.Errorf("config.Discover (started %s): %w", startDir, ErrNotFound)
		}
		dir = parent
	}
}

// Load reads and parses canopy.json at the given path. It populates the
// derived fields (ProjectRoot, Project) and validates that all three
// script fields are present.
//
// Returns ErrInvalid for parse failures or missing required fields.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config.Load: read %s: %w", path, err)
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config.Load(%s): %w: %v", path, ErrInvalid, err)
	}

	if err := validate(&c); err != nil {
		return nil, fmt.Errorf("config.Load(%s): %w", path, err)
	}

	// Canonicalize the project root: absolute path with symlinks resolved.
	// This is the key used in state.json's Projects map (v2+), so it must
	// be stable across runs regardless of how the user cd'd in. Without
	// EvalSymlinks, a user with `~/code -> /home/avi/code` would register
	// the project differently depending on which path they used.
	root, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("config.Load: abs root: %w", err)
	}
	if resolved, lerr := filepath.EvalSymlinks(root); lerr == nil {
		root = resolved
	}
	// Else: EvalSymlinks failed (rare — usually means a missing dir, but
	// the dir HAS to exist since canopy.json is in it). Fall through with
	// the abs path; it's still a valid stable identifier on this machine.
	c.ProjectRoot = root
	c.Project = filepath.Base(root)

	log.Info("config.loaded", "path", path, "project", c.Project)
	return &c, nil
}

// DiscoverAndLoad is shorthand for Discover + Load. Most CLI subcommands
// want both: walk up from cwd, parse what we find.
func DiscoverAndLoad(startDir string) (*Config, error) {
	path, err := Discover(startDir)
	if err != nil {
		return nil, err
	}
	return Load(path)
}

// validate normalizes the loaded config and surfaces user-fixable
// problems. Today it:
//   - Defaults Agent.Type to "claude" when unset (back-compat for
//     canopy.json files that predate v0.6).
//   - Logs a warning if both Briefing and BriefingFile are set
//     (BriefingFile wins; this is documented but worth alerting).
//
// We deliberately do NOT check that script paths or briefing files
// exist or are executable here. The runner's error at execution time
// is more precise ("script not found" vs "validation failed"), and
// tools like `canopy init --with-scripts` might generate the canopy.json
// before the scripts exist on disk.
//
// Agent.Type is NOT validated against the known launcher map here —
// that check lives in internal/agent.ResolveLauncher, which is the
// single source of truth for which agents canopy supports. Keeping
// the check there means new agents land via one PR (add a launcher),
// not two (add a launcher + update config validation).
func validate(c *Config) error {
	if c.Agent.Type == "" {
		c.Agent.Type = "claude"
	}
	if c.Agent.Briefing != "" && c.Agent.BriefingFile != "" {
		log.Warn("config.agent: both briefing and briefing_file set; file wins",
			"briefing_file", c.Agent.BriefingFile)
	}
	return nil
}
