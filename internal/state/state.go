// Package state owns canopy's persistent registry of workspaces.
//
// The on-disk format is a single JSON file at ~/.canopy/state.json. Every
// mutation goes through Store.WithLock, which holds an advisory flock on a
// sibling lockfile (state.json.lock) for the duration of the read-modify-
// write window. Multiple canopy processes (two terminals running
// `canopy new` simultaneously) serialize cleanly; the second process waits,
// then sees the first's writes when the lock releases.
//
// Reads through Store.Load are unlocked. Callers that only need a snapshot
// (TUI refresh, `canopy ls`) tolerate stale data; the cost of locking every
// read isn't worth it.
//
// Atomicity: Store.Save writes to state.json.tmp first and rename(2)s into
// place. POSIX guarantees rename is atomic within the same filesystem, so
// readers either see the old file or the new one — never a half-written
// JSON document.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("state")

// SchemaVersion is the version of the on-disk JSON schema. Bumping it
// signals that future canopy versions need to migrate the file.
//
//	v1 (canopy <= v0.4): Projects map keyed by project basename; Workspace
//	  rows use Project (basename) as the project key.
//	v2 (canopy >= v0.5): Projects map keyed by canonical absolute project
//	  root path; Workspace rows have ProjectRoot (canonical path) as the
//	  authoritative key. Workspace.Project is kept as a legacy-read field
//	  for one release so v0.5 can still parse v1 files; never written by
//	  v0.5+.
//
// State files are migrated lazily on the first project-scoped command via
// State.MigrateLegacyProject — there's no big-bang migration script and a
// v1 file Loads cleanly into v2 structs (legacy fields populate, new
// fields stay zero until migration runs).
const SchemaVersion = 2

// Hint is a v0.6 lifecycle hint surfaced by a detector
// (internal/lifecycle/). Hints are NOT persisted in state.json — they're
// recomputed on every TUI refresh / canopy reconcile run because
// persisting risks staleness after a manual git operation outside canopy.
//
// The Hint type lives in package state (not internal/lifecycle) because
// many packages consume it (UI for badges, agent for briefings, cmd for
// rm safety checks). Putting it in the package that all consumers
// already depend on avoids a circular-dep dance.
//
// Detectors return zero or one Hint per kind per workspace. Multiple
// hint kinds can be active at once (e.g., rename_suggested AND pr_status
// firing on the same workspace mid-flight).
type Hint struct {
	// Kind is one of: "rename_suggested", "shipped", "pr_status".
	// Adding a new kind: register the detector in internal/lifecycle/
	// and document the kind in docs/design/v0.6-agent-lifecycle.md.
	Kind string

	// Message is human-readable, one line, present tense. E.g.:
	// "branch 'ancient-hornet' has 3 commits past main; rename to reflect intent"
	Message string

	// Action is an optional suggested command for the user OR the
	// agent to run. E.g. "git branch -m <name>". Surfaced in TUI
	// hover (eventually) and in the AGENT.md briefing's hint section.
	// Empty when no specific action applies.
	Action string

	// DetectedAt is when this hint was last refreshed. Used by stale-
	// data indicators (e.g., pr_status hint from a 1h-old cache shows
	// "stale" tag). Not persisted; reset on every detector run.
	DetectedAt time.Time
}

// Status enumerates the five workspace states from the design doc.
//
//	setting_up -> ready  (scripts.setup succeeded)
//	setting_up -> broken (scripts.setup failed)
//	ready -> stopped     (tmux session died, dir alive — resurrectable)
//	ready -> orphaned    (workspace dir gone from disk)
type Status string

const (
	StatusSettingUp Status = "setting_up"
	StatusReady     Status = "ready"
	StatusStopped   Status = "stopped"
	StatusBroken    Status = "broken"
	StatusOrphaned  Status = "orphaned"
)

// Workspace is one row in state.json. Field tags match the design doc's
// JSON schema exactly so the on-disk file stays human-readable.
//
// Project IDs migrated in v2: ProjectRoot (canonical absolute path) is the
// authoritative key. Project (basename) is kept for one release as a
// legacy-read field so v0.5 can parse v1 state.json files; v0.5+ writes
// both for backward compat. v0.6 will drop the legacy Project field.
type Workspace struct {
	// Project is the legacy basename-keyed project name. v1 state files
	// only have this. v2+ also writes it for backward compat with tools
	// that grep state.json. Lookups should prefer ProjectRoot.
	Project string `json:"project,omitempty"`

	// ProjectRoot is the canonical absolute path to the project's repo
	// root (the directory containing canopy.json), as resolved by
	// filepath.EvalSymlinks. Authoritative key in v2+; empty in v1 rows
	// until MigrateLegacyProject runs.
	ProjectRoot string `json:"project_root,omitempty"`

	Name        string    `json:"name"`
	Branch      string    `json:"branch"`
	Path        string    `json:"path"`
	TmuxSession string    `json:"tmux_session"`
	Port        int       `json:"port"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	LastError   string    `json:"last_error,omitempty"`
	// LastErrorHint is a one-line user-facing diagnosis of LastError.
	// Populated by workspace.Diagnose() when scripts.setup fails with
	// a stderr signature canopy recognizes (missing Rails master key,
	// network blip, db already exists, etc.). Empty when no pattern
	// matched — the caller falls back to the raw error chain.
	LastErrorHint string `json:"last_error_hint,omitempty"`

	// AgentLaunchCount tracks how many times canopy has spawned the
	// agent process for this workspace. Incremented on every successful
	// workspace.Create completion + workspace.Resurrect completion.
	// Used by the v0.6 hybrid briefing strategy: count==0 means fresh
	// launch (full briefing); count>0 means resume launch (hints-only
	// delta, or skip --append-system-prompt entirely if no hints).
	//
	// Why a counter and not a bool: we want to know "first launch" but
	// also leave the door open for future heuristics (e.g., "agent
	// re-launched 5 times in a day → maybe it's stuck"). Cheap to
	// store; same shape as Port.
	AgentLaunchCount int `json:"agent_launch_count,omitempty"`

	// SourceKind is set once at workspace creation and never changes.
	// Drives which AGENT.md briefing variant gets assembled and the
	// agent's framing for the work. Values:
	//   - ""        legacy / pre-v0.6 row (treat as "fresh")
	//   - "fresh"   canopy new with no source flags
	//   - "pr"      canopy new --pr <num>: review-mode briefing
	//   - "issue"   canopy new --issue <num>: implementation-mode briefing
	//   - "branch"  canopy new --branch <name>: pickup-mode briefing
	//
	// Stored as a string rather than an enum to keep the JSON
	// human-readable and forward-compatible (new SourceKinds can land
	// without a schema migration; readers tolerant of unknown values
	// fall back to "fresh"-style briefing).
	SourceKind string `json:"source_kind,omitempty"`
}

// ProjectMeta tracks per-project metadata that lives outside any single
// workspace row. Holds the project's allocated port base (deterministic
// per-project block, default 1000 wide) plus the canonical root path.
//
// PortBase is first-come-first-served at canopy's first sight of a project.
// Stable across reboots and `canopy rm`s; only nuking ~/.canopy/state.json
// would re-shuffle the assignments.
//
// Root mirrors the State.Projects map key for self-describing meta — when
// you look up s.Projects["/home/avi/Work/canopy"], the meta value's Root
// field is also "/home/avi/Work/canopy". Redundant on purpose: it lets
// tooling read meta without also tracking which key it came from.
type ProjectMeta struct {
	// Root is the canonical absolute path to the project's repo root
	// (the directory containing canopy.json). Same value as the map key.
	// Empty in v1 entries until MigrateLegacyProject runs.
	Root string `json:"root,omitempty"`

	PortBase int `json:"port_base"`
}

// State is the root document. We always write at least an empty workspaces
// array (never null) so external tools doing `jq '.workspaces[]'` don't trip.
type State struct {
	SchemaVersion int                    `json:"schema_version"`
	Projects      map[string]ProjectMeta `json:"projects,omitempty"`
	Workspaces    []Workspace            `json:"workspaces"`
}

// Sentinel errors. Tests use errors.Is.
var (
	// ErrNotFound is returned by State.Find when no workspace matches.
	ErrNotFound = errors.New("state: workspace not found")

	// ErrAlreadyExists is returned by State.Add when a workspace with the
	// same (project, name) is already present.
	ErrAlreadyExists = errors.New("state: workspace already exists")
)

// Find returns a pointer to the workspace with the given (projectRoot, name)
// tuple, or nil + ErrNotFound. projectRoot is the canonical absolute path
// to the project's repo root (the v2 authoritative key); callers typically
// pass m.Cfg.ProjectRoot.
//
// The returned pointer is into the caller's State slice; mutations are
// visible after Save. After MigrateLegacyProject has run on the relevant
// project, every Workspace row has ProjectRoot populated; rows that haven't
// been migrated yet are invisible to Find — that's a feature, not a bug,
// because the lifecycle code that calls Find always runs migration first.
func (s *State) Find(projectRoot, name string) (*Workspace, error) {
	for i := range s.Workspaces {
		w := &s.Workspaces[i]
		if w.ProjectRoot == projectRoot && w.Name == name {
			return w, nil
		}
	}
	return nil, ErrNotFound
}

// Add appends w to State.Workspaces if no row already exists for the same
// (ProjectRoot, Name). Returns ErrAlreadyExists otherwise. Callers must
// populate w.ProjectRoot — Add does not infer it from cfg.
func (s *State) Add(w Workspace) error {
	if _, err := s.Find(w.ProjectRoot, w.Name); err == nil {
		return fmt.Errorf("state.Add(%s/%s): %w", w.ProjectRoot, w.Name, ErrAlreadyExists)
	}
	s.Workspaces = append(s.Workspaces, w)
	return nil
}

// EnsureProjectBase returns the port base assigned to a project, identified
// by its canonical absolute root path. If the project hasn't been seen
// before, allocates the next free base (firstBase, firstBase+stride,
// firstBase+2×stride, ...) and persists it in s.Projects. Caller must be
// holding the state lock — this mutates s in place.
//
// Returns the base + a boolean indicating whether a new assignment was
// made (callers may want to log the first-time event).
//
// Errors only when the search exceeds maxProjects iterations, which
// would mean canopy is being asked to track more concurrent projects
// than the port plan accommodates (default 1000 / 1000 = 1).
func (s *State) EnsureProjectBase(projectRoot string, firstBase, stride, maxProjects int) (int, bool, error) {
	if s.Projects == nil {
		s.Projects = map[string]ProjectMeta{}
	}
	if meta, ok := s.Projects[projectRoot]; ok {
		// Self-heal: an old v1 entry that got migrated may have left the
		// Root field empty. Backfill on next access.
		if meta.Root == "" {
			meta.Root = projectRoot
			s.Projects[projectRoot] = meta
		}
		return meta.PortBase, false, nil
	}
	used := make(map[int]struct{}, len(s.Projects))
	for _, m := range s.Projects {
		used[m.PortBase] = struct{}{}
	}
	for i := 0; i < maxProjects; i++ {
		candidate := firstBase + i*stride
		if _, taken := used[candidate]; !taken {
			s.Projects[projectRoot] = ProjectMeta{Root: projectRoot, PortBase: candidate}
			return candidate, true, nil
		}
	}
	return 0, false, fmt.Errorf("state.EnsureProjectBase: ran out of project slots after %d", maxProjects)
}

// Remove deletes the workspace with the given (projectRoot, name) from the
// slice. Returns ErrNotFound if no match. projectRoot is the canonical
// absolute path to the project's repo root (the v2 authoritative key).
func (s *State) Remove(projectRoot, name string) error {
	for i := range s.Workspaces {
		w := s.Workspaces[i]
		if w.ProjectRoot == projectRoot && w.Name == name {
			s.Workspaces = append(s.Workspaces[:i], s.Workspaces[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("state.Remove(%s/%s): %w", projectRoot, name, ErrNotFound)
}

// Store is the on-disk handle for state.json. The zero value is invalid;
// use NewStore to construct.
type Store struct {
	path     string // <home>/state.json
	lockPath string // <home>/state.json.lock
}

// NewStore returns a Store rooted at canopyHome (typically ~/.canopy). It
// creates canopyHome on first call so subsequent Load/Save don't need to
// re-check.
func NewStore(canopyHome string) (*Store, error) {
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		return nil, fmt.Errorf("state.NewStore: mkdir %s: %w", canopyHome, err)
	}
	return &Store{
		path:     filepath.Join(canopyHome, "state.json"),
		lockPath: filepath.Join(canopyHome, "state.json.lock"),
	}, nil
}

// Path returns the location of state.json. Useful for log messages and
// tests; callers should not write to this path directly (use Save).
func (s *Store) Path() string { return s.path }

// Load reads state.json from disk. A missing file returns an empty State
// with SchemaVersion set, which is the correct first-run behavior. A
// malformed file returns an error; we never silently overwrite a corrupt
// state.json since it might contain data the user wants to recover.
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{SchemaVersion: SchemaVersion, Workspaces: []Workspace{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state.Load: read %s: %w", s.path, err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("state.Load: parse %s: %w", s.path, err)
	}
	if st.Workspaces == nil {
		st.Workspaces = []Workspace{}
	}
	return &st, nil
}

// Save writes state to state.json atomically (tmpfile + rename). Callers
// that hold the WithLock lock are guaranteed serialization; callers that
// call Save outside the lock race against other writers and may overwrite
// concurrent changes — almost nobody should do that.
func (s *Store) Save(state *State) error {
	if state.SchemaVersion == 0 {
		state.SchemaVersion = SchemaVersion
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("state.Save: marshal: %w", err)
	}
	// Append trailing newline so the file is friendly to text editors.
	data = append(data, '\n')

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("state.Save: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state.Save: rename: %w", err)
	}
	return nil
}

// WithLock is the safe way to mutate state. It acquires an exclusive
// advisory flock on state.json.lock, loads the current state, runs fn,
// then saves and releases. fn may modify the *State in place.
//
// Two callers running WithLock simultaneously serialize: the second call
// blocks on Flock(LOCK_EX) until the first releases. This prevents the
// classic read-modify-write race where two `canopy new` invocations both
// load (port=3000), both append, both save, and one overwrites the other.
//
// fn returning an error skips the Save and propagates the error. Successful
// fn always saves, even if fn made no visible changes — that's a tiny cost
// (the file already matches what we'd write) and keeps the contract simple.
func (s *Store) WithLock(fn func(*State) error) (err error) {
	lockFile, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("state.WithLock: open lock: %w", err)
	}
	defer lockFile.Close()

	fd := int(lockFile.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return fmt.Errorf("state.WithLock: acquire flock: %w", err)
	}
	defer func() {
		if uerr := unix.Flock(fd, unix.LOCK_UN); uerr != nil && err == nil {
			err = fmt.Errorf("state.WithLock: release flock: %w", uerr)
		}
	}()

	st, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	if err := s.Save(st); err != nil {
		return err
	}
	log.Debug("state.with-lock: saved", "workspaces", len(st.Workspaces))
	return nil
}
