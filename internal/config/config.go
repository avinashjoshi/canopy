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

	// Agent is the LEGACY singular agent block. Empty block (no
	// agent.type set) is treated as Type="claude" by validate(). When
	// the newer Agents array is also present, Agents wins and Agent is
	// silently ignored — see validate() for the precedence dance.
	Agent Agent `json:"agent,omitempty"`

	// Agents is the v0.22 plural allowlist of launchers this project
	// supports. canopy new --agent <type> and canopy agent swap <type>
	// both validate against this list via AllowsAgent. The first entry
	// is the project's DEFAULT for new workspaces.
	//
	// Precedence rules (validate()):
	//   - Agents non-empty:    use as-is; Agent legacy block ignored
	//   - Agents empty, Agent.Type set: promote to Agents=[Agent.Type]
	//   - Both empty:          Agents=["claude"] (matches legacy default)
	//
	// The legacy Agent.Briefing/BriefingFile stays a separate concern —
	// even when Agents is present, the briefing-text plumbing keeps
	// reading from Agent. Per-launcher briefings are a follow-up.
	Agents []string `json:"agents,omitempty"`

	// ProjectRoot is set by Load (not from the JSON). Absolute path to the
	// directory containing canopy.json.
	ProjectRoot string `json:"-"`

	// Project is the basename of ProjectRoot. canopy uses this as the project
	// identifier in state.json and as the prefix for tmux session names
	// (e.g. "cravd-bold-falcon").
	Project string `json:"-"`
}

// DefaultAgent returns the canonical default agent for a new workspace
// created from this project. Always returns the first entry in Agents
// (which validate() guarantees is non-empty). Use this instead of
// reading Cfg.Agent.Type directly — Agent.Type is the legacy field that
// validate() may not have populated when canopy.json declared Agents
// only.
func (c *Config) DefaultAgent() string {
	if len(c.Agents) > 0 {
		return c.Agents[0]
	}
	// validate() should have populated Agents; this is a defensive
	// fallback for callers that bypass Load (test fixtures, mostly).
	if c.Agent.Type != "" {
		return c.Agent.Type
	}
	return "claude"
}

// AllowsAgent reports whether the project's canopy.json declares the
// given agent type as runnable. Empty target → false. Used by the
// --agent CLI flag and the canopy agent swap verb to gate against
// ErrAgentNotAllowed at command time, before any side effects fire.
func (c *Config) AllowsAgent(t string) bool {
	if t == "" {
		return false
	}
	for _, a := range c.Agents {
		if a == t {
			return true
		}
	}
	return false
}

// AddAgentToCanopyJSON appends agentName to <projectRoot>/canopy.json's
// `agents` array if it's not already present, and writes the file
// back atomically. Unknown top-level keys are preserved via raw-map
// round-trip (same pattern as userconfig.go) so user-added fields
// don't get clobbered. v0.22.
//
// Used by the in-TUI swap + ask pickers' D6=A "auto-add on pick"
// path: when a user picks an agent that's installed but not in the
// project's allowlist, this writes the config update silently.
// Idempotent — no-op + nil return if the agent is already listed.
//
// The function does NOT mutate any in-memory Config; the caller is
// expected to re-Load if they need the updated Cfg.Agents. (canopy
// agent swap's success path doesn't need it — the verb has already
// resolved the launcher by name; canopy.json is updated for the next
// invocation.)
//
// Errors propagate: file-missing, parse failure, malformed agents
// field (e.g., `"agents": "claude"` instead of an array), or write
// failure. Each is wrapped with the canopy.json path for diagnosis.
func AddAgentToCanopyJSON(projectRoot, agentName string) error {
	if agentName == "" {
		return fmt.Errorf("config.AddAgentToCanopyJSON: empty agent name")
	}
	path := filepath.Join(projectRoot, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config.AddAgentToCanopyJSON: read %s: %w", path, err)
	}

	// Two-pass decode: raw map preserves unknown keys; typed agents
	// extraction modifies just the field we care about.
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("config.AddAgentToCanopyJSON: parse %s: %w", path, err)
	}
	var agents []string
	if existing, ok := raw["agents"]; ok && len(existing) > 0 {
		if err := json.Unmarshal(existing, &agents); err != nil {
			return fmt.Errorf("config.AddAgentToCanopyJSON: parse agents in %s: %w", path, err)
		}
	}
	// Legacy promotion: if `agents` is missing but the legacy `agent: {type}`
	// block is present, seed the new array from that type. Without this
	// seed, auto-adding a NEW agent to a project that implicitly declared
	// claude via `agent.type` would write only the new agent and silently
	// drop claude from the allowlist. (codex review P1, 2026-06-25.)
	if len(agents) == 0 {
		if rawAgent, ok := raw["agent"]; ok && len(rawAgent) > 0 {
			var legacy struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(rawAgent, &legacy); err == nil && legacy.Type != "" {
				agents = []string{legacy.Type}
			}
		}
	}
	// Last-resort default: a project with neither `agents` nor `agent.type`
	// implicitly defaulted to claude (validate() does this on Load).
	// Adding a new agent shouldn't drop the implicit claude default.
	if len(agents) == 0 {
		agents = []string{"claude"}
	}
	for _, a := range agents {
		if a == agentName {
			return nil // idempotent — already there
		}
	}
	agents = append(agents, agentName)

	updated, err := json.Marshal(agents)
	if err != nil {
		return fmt.Errorf("config.AddAgentToCanopyJSON: marshal agents: %w", err)
	}
	raw["agents"] = updated

	// Re-marshal the whole doc with indent so the file stays
	// human-readable. We can't use json.Marshal on the raw map and
	// get stable key order; for canopy.json's small fixed surface
	// (scripts + agent + agents) the indented output is acceptable
	// even with map-iteration-order keys. If key order becomes
	// load-bearing, swap to a custom encoder; for now boring wins.
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("config.AddAgentToCanopyJSON: marshal doc: %w", err)
	}
	out = append(out, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("config.AddAgentToCanopyJSON: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config.AddAgentToCanopyJSON: rename: %w", err)
	}
	log.Info("config.agent.added", "project_root", projectRoot, "agent", agentName)
	return nil
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

// LoadFrom reads canopy.json directly from <root>/canopy.json without the
// walk-up that DiscoverAndLoad does. Used when the caller already knows the
// canonical project root (e.g. cross-project destructive ops in the unified
// TUI: state.GlobalRow.ProjectRoot is authoritative — no need to walk up).
//
// Returns ErrNotFound if the file is missing, ErrInvalid for parse failures.
func LoadFrom(root string) (*Config, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("config.LoadFrom: abs %s: %w", root, err)
	}
	path := filepath.Join(abs, FileName)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config.LoadFrom(%s): %w", root, ErrNotFound)
		}
		return nil, fmt.Errorf("config.LoadFrom: stat %s: %w", path, err)
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
	// Agents/Agent precedence (v0.22 schema dance):
	//   - Agents non-empty → use as-is; the legacy Agent block (if any)
	//     is silently ignored. Agent.Type is forced to Agents[0] so any
	//     code still reading the legacy field stays consistent.
	//   - Agents empty, Agent.Type set → promote to Agents=[Agent.Type].
	//     This is the existing-canopy.json upgrade path: a project that
	//     only declares `agent.type` gains Agents=[that-type] without
	//     the user editing anything.
	//   - Both empty → default Agents=["claude"], matching legacy.
	//
	// The precedence is silent (no log noise on collision) per eng-review
	// D6. The legacy `agent` block keeps working indefinitely; users
	// migrate by editing canopy.json on their own schedule.
	switch {
	case len(c.Agents) > 0:
		// Keep Agent.Type aligned with Agents[0] for any holdout
		// readers; the canonical source going forward is Agents.
		c.Agent.Type = c.Agents[0]
	case c.Agent.Type != "":
		c.Agents = []string{c.Agent.Type}
	default:
		c.Agents = []string{"claude"}
		c.Agent.Type = "claude"
	}
	if c.Agent.Briefing != "" && c.Agent.BriefingFile != "" {
		log.Warn("config.agent: both briefing and briefing_file set; file wins",
			"briefing_file", c.Agent.BriefingFile)
	}
	return nil
}
