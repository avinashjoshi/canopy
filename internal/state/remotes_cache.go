package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// RemotesCache is the laptop-side read aggregate of remote-host
// `canopy ls --json` results. v0.17.0 Phase 1b. Persisted at
// ~/.canopy/remotes-cache.json so the TUI can show last-known rows
// for hosts that are currently offline.
//
// Ownership: laptop is the canonical writer for this file (it's its
// view of every remote host's state). Remote canopies don't read or
// write it. The map keyed by host name has one entry per registered
// host that has been seen at least once.
//
// Atomic write: tmp-file + rename under flock matches the existing
// state.Store pattern. Concurrent canopy processes (the rare "two
// canopy TUIs at the same time" case) won't corrupt the file.
type RemotesCache struct {
	canopyHome string
	path       string
	lockPath   string
}

type remotesCacheFile struct {
	Version int                            `json:"version"`
	Hosts   map[string]*RemoteHostSnapshot `json:"hosts"`
}

// RemoteHostSnapshot is the persisted-then-loaded shape per host. The
// LastSeen / LastError pair distinguishes "haven't fetched recently"
// from "fetched and got an error" so the TUI renders the right pill.
type RemoteHostSnapshot struct {
	// Workspaces is whatever the host returned on its most recent
	// successful refresh. Persists across TUI restarts so the laptop
	// has "last-known" rows to render when the host is offline.
	Workspaces []RemoteWorkspaceRow `json:"workspaces"`

	// CanopyVersion is what the remote reported. Empty for hosts
	// never successfully contacted.
	CanopyVersion string `json:"canopy_version,omitempty"`

	// LastSeen is the timestamp of the last SUCCESSFUL refresh. Zero
	// if the host has never been reached.
	LastSeen time.Time `json:"last_seen,omitempty"`

	// LastError describes the most recent FAILED refresh (auth fail,
	// timeout, malformed JSON, etc). Empty after a subsequent success.
	LastError string `json:"last_error,omitempty"`

	// LastRefreshAttempt is the timestamp of the most recent refresh
	// attempt, success or failure. Used to render "tower (last seen
	// 47s ago)" on the section header.
	LastRefreshAttempt time.Time `json:"last_refresh_attempt"`

	// ClipboardBridge is the v0.18 bridge status reported by the
	// remote's `canopy ls --json`. One of "off", "bridged", "broken",
	// or "" when the remote canopy predates v0.18 (the JSON field is
	// absent and the decoder leaves it empty). Drives the `📋` pill
	// on the Hosts tab (Lane C.4).
	ClipboardBridge string `json:"clipboard_bridge,omitempty"`
}

// RemoteWorkspaceRow is the on-disk + in-memory cached shape of one
// remote workspace, mirroring host.RemoteWorkspace (we can't import
// internal/host here without a cycle, so this is the duplicated leaf).
type RemoteWorkspaceRow struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	Branch      string `json:"branch"`
	Status      string `json:"status"`
	Port        int    `json:"port,omitempty"`
	TmuxSession string `json:"tmux_session"`
	Alive       bool   `json:"alive"`

	// v0.17 Phase 1g: cached load + hints + diagnosis. Persisted so
	// last-known values render when the host is offline.
	MemRSS        int64  `json:"mem_rss,omitempty"`
	CPU           float64 `json:"cpu,omitempty"`
	Hints         []Hint  `json:"hints,omitempty"`
	LastErrorHint string  `json:"last_error_hint,omitempty"`

	// AgentState mirrors LsJSONWorkspace.AgentState (v0.17 Phase 1d.2).
	AgentState string `json:"agent_state,omitempty"`
}

const remotesCacheVersion = 1

// NewRemotesCache returns a RemotesCache rooted at canopyHome. Creates
// the dir if missing. Doesn't load on construction — call Load() or
// use WithLock for mutations.
func NewRemotesCache(canopyHome string) (*RemotesCache, error) {
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		return nil, fmt.Errorf("state.NewRemotesCache: mkdir %s: %w", canopyHome, err)
	}
	return &RemotesCache{
		canopyHome: canopyHome,
		path:       filepath.Join(canopyHome, "remotes-cache.json"),
		lockPath:   filepath.Join(canopyHome, ".remotes-cache.lock"),
	}, nil
}

// Path returns the on-disk location of the cache file. Useful for tests
// + the Hosts tab's "cache: <path>" diagnostic.
func (r *RemotesCache) Path() string { return r.path }

// Load reads the cache. Missing file is treated as an empty cache (the
// first TUI session after canopy install never has cached rows). The
// returned map is keyed by host name and is a copy of the file — caller
// can mutate freely; persistence is explicit via Save().
func (r *RemotesCache) Load() (map[string]*RemoteHostSnapshot, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*RemoteHostSnapshot{}, nil
		}
		return nil, fmt.Errorf("state.RemotesCache.Load: read %s: %w", r.path, err)
	}
	var rf remotesCacheFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("state.RemotesCache.Load: parse %s: %w", r.path, err)
	}
	if rf.Hosts == nil {
		rf.Hosts = map[string]*RemoteHostSnapshot{}
	}
	return rf.Hosts, nil
}

// Save writes the cache atomically. Tmp-file + rename so partial writes
// don't leave a corrupt file on power-cut.
func (r *RemotesCache) Save(hosts map[string]*RemoteHostSnapshot) error {
	rf := remotesCacheFile{Version: remotesCacheVersion, Hosts: hosts}
	data, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return fmt.Errorf("state.RemotesCache.Save: marshal: %w", err)
	}
	data = append(data, '\n')
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("state.RemotesCache.Save: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state.RemotesCache.Save: rename: %w", err)
	}
	return nil
}

// WithLock load → mutate → save under an exclusive flock. Matches
// state.Store.WithLock shape for consistency.
func (r *RemotesCache) WithLock(fn func(map[string]*RemoteHostSnapshot) error) (err error) {
	lockFile, err := os.OpenFile(r.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("state.RemotesCache.WithLock: open lock: %w", err)
	}
	defer lockFile.Close()

	fd := int(lockFile.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return fmt.Errorf("state.RemotesCache.WithLock: acquire flock: %w", err)
	}
	defer func() {
		if uerr := unix.Flock(fd, unix.LOCK_UN); uerr != nil && err == nil {
			err = fmt.Errorf("state.RemotesCache.WithLock: release flock: %w", uerr)
		}
	}()

	hosts, err := r.Load()
	if err != nil {
		return err
	}
	if err := fn(hosts); err != nil {
		return err
	}
	return r.Save(hosts)
}

// SortedHostNames returns the keys of the cache in deterministic order.
// Used by the TUI's render path so section headers don't shuffle on
// refresh.
func SortedHostNames(hosts map[string]*RemoteHostSnapshot) []string {
	names := make([]string, 0, len(hosts))
	for n := range hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
