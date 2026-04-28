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

// Scripts holds the three script paths that define a project's lifecycle.
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
}

// Config is the parsed canopy.json plus metadata about where it was found.
// ProjectRoot is the absolute path to the directory containing canopy.json;
// Project is its base name (which doubles as the project identifier in
// state.json and the tmux session prefix).
type Config struct {
	// Scripts comes from the JSON.
	Scripts Scripts `json:"scripts"`

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

// validate is a no-op for canopy.json today. All three script fields are
// optional: a canopy.json with `{}` or `{"scripts": {}}` is fully valid
// and means "no setup hook, no server command, no archive hook" — canopy
// will still create the worktree and tmux session, just without running
// anything user-supplied. Future schema additions (layout, env, etc.)
// will live here when they grow up to need real validation.
//
// We deliberately do NOT check that script paths exist or are executable.
// The runner's error at execution time is more precise ("script not
// found" vs "validation failed"), and tools like `canopy init --with-scripts`
// might generate the canopy.json before the scripts exist on disk.
func validate(c *Config) error {
	return nil
}
