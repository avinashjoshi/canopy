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
const SchemaVersion = 1

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
type Workspace struct {
	Project     string    `json:"project"`
	Name        string    `json:"name"`
	Branch      string    `json:"branch"`
	Path        string    `json:"path"`
	TmuxSession string    `json:"tmux_session"`
	Port        int       `json:"port"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	LastError   string    `json:"last_error,omitempty"`
}

// State is the root document. We always write at least an empty workspaces
// array (never null) so external tools doing `jq '.workspaces[]'` don't trip.
type State struct {
	SchemaVersion int         `json:"schema_version"`
	Workspaces    []Workspace `json:"workspaces"`
}

// Sentinel errors. Tests use errors.Is.
var (
	// ErrNotFound is returned by State.Find when no workspace matches.
	ErrNotFound = errors.New("state: workspace not found")

	// ErrAlreadyExists is returned by State.Add when a workspace with the
	// same (project, name) is already present.
	ErrAlreadyExists = errors.New("state: workspace already exists")
)

// Find returns a pointer to the workspace with the given (project, name)
// tuple, or nil + ErrNotFound. The returned pointer is into the caller's
// State slice; mutations are visible after Save.
func (s *State) Find(project, name string) (*Workspace, error) {
	for i := range s.Workspaces {
		w := &s.Workspaces[i]
		if w.Project == project && w.Name == name {
			return w, nil
		}
	}
	return nil, ErrNotFound
}

// Add appends w to State.Workspaces if no row already exists for the same
// (project, name). Returns ErrAlreadyExists otherwise.
func (s *State) Add(w Workspace) error {
	if _, err := s.Find(w.Project, w.Name); err == nil {
		return fmt.Errorf("state.Add(%s/%s): %w", w.Project, w.Name, ErrAlreadyExists)
	}
	s.Workspaces = append(s.Workspaces, w)
	return nil
}

// Remove deletes the workspace with the given (project, name) from the
// slice. Returns ErrNotFound if no match.
func (s *State) Remove(project, name string) error {
	for i := range s.Workspaces {
		w := s.Workspaces[i]
		if w.Project == project && w.Name == name {
			s.Workspaces = append(s.Workspaces[:i], s.Workspaces[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("state.Remove(%s/%s): %w", project, name, ErrNotFound)
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
