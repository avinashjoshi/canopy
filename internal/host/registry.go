package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Host is a single registered remote canopy installation. v0.17.0 only
// stores SSH-reachable hosts; future phases (canopy.cloud thesis, Fly
// Machines) add other Type values without changing the registry shape.
//
// ProjectPath is the host's canonical project directory: where
// `canopy new --on <name>` cd's before invoking canopy on the remote.
// Phase 0 required this as a per-command --remote-cwd flag; Phase 1a
// moves it into the registry so the CLI surface becomes `--on tower`
// with everything else inferred.
type Host struct {
	Name        string    `json:"-"` // map key in registryFile.Hosts
	Type        string    `json:"type"`
	SSHTarget   string    `json:"ssh_target,omitempty"`
	ProjectPath string    `json:"project_path,omitempty"`
	AddedAt     time.Time `json:"added_at"`
}

// Registry is the on-disk hosts.json read/write surface.
//
// Pattern matches internal/state.Store: load → mutate via callbacks →
// save under flock. Future Phase 1b adds Refresher (laptop-side
// per-host goroutine polling) which is a *consumer* of the registry,
// not a layer above it.
type Registry struct {
	canopyHome string
	path       string
	lockPath   string
}

// registryFile is the on-disk JSON shape. Version-bump when the shape
// changes; older versions get migrated forward in NewRegistry().
type registryFile struct {
	Version int              `json:"version"`
	Hosts   map[string]*Host `json:"hosts"`
}

const registryVersion = 1

// Sentinels for the cmd/canopy/host.go CLI to detect specific cases.
var (
	ErrHostExists   = errors.New("host already exists in registry")
	ErrHostNotFound = errors.New("host not found in registry")
	ErrHostInvalid  = errors.New("host name is invalid")
)

// NewRegistry returns a Registry rooted at canopyHome (typically
// ~/.canopy). Creates the dir if missing. Does not load yet — callers
// pass through Add/Remove/List which open the file under flock.
func NewRegistry(canopyHome string) (*Registry, error) {
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		return nil, fmt.Errorf("host.NewRegistry: mkdir %s: %w", canopyHome, err)
	}
	return &Registry{
		canopyHome: canopyHome,
		path:       filepath.Join(canopyHome, "hosts.json"),
		lockPath:   filepath.Join(canopyHome, ".hosts.lock"),
	}, nil
}

// Add registers a new host. Errors with ErrHostExists if name is
// already taken — callers can `canopy host rm` first or pick a
// different name. Idempotent re-add is intentionally rejected because
// silent overwrite of SSHTarget would be confusing for users with
// muscle-memory on the name.
func (r *Registry) Add(name string, h Host) error {
	if err := validateName(name); err != nil {
		return err
	}
	return r.withLock(func(rf *registryFile) error {
		if _, exists := rf.Hosts[name]; exists {
			return fmt.Errorf("host.Add(%q): %w", name, ErrHostExists)
		}
		h.Name = name
		if h.Type == "" {
			h.Type = "ssh"
		}
		if h.AddedAt.IsZero() {
			h.AddedAt = time.Now().UTC()
		}
		rf.Hosts[name] = &h
		return nil
	})
}

// Remove drops a host from the registry. Errors with ErrHostNotFound
// if the name isn't registered. Phase 1b will add a workspace-presence
// check (refuse rm if the host has cached workspaces, --force overrides)
// but the registry layer stays simple: it just persists.
func (r *Registry) Remove(name string) error {
	return r.withLock(func(rf *registryFile) error {
		if _, exists := rf.Hosts[name]; !exists {
			return fmt.Errorf("host.Remove(%q): %w", name, ErrHostNotFound)
		}
		delete(rf.Hosts, name)
		return nil
	})
}

// Resolve returns a copy of the named host. ErrHostNotFound if missing.
// Callers should treat the returned Host as read-only; mutations must
// go through Add (re-add after Remove) so they're persisted under the
// flock contract.
func (r *Registry) Resolve(name string) (Host, error) {
	rf, err := r.load()
	if err != nil {
		return Host{}, err
	}
	h, ok := rf.Hosts[name]
	if !ok {
		return Host{}, fmt.Errorf("host.Resolve(%q): %w", name, ErrHostNotFound)
	}
	out := *h
	out.Name = name
	return out, nil
}

// List returns all registered hosts in deterministic name-sorted order.
// Empty slice (not nil) when the registry has no entries.
func (r *Registry) List() ([]Host, error) {
	rf, err := r.load()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rf.Hosts))
	for name := range rf.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Host, 0, len(names))
	for _, name := range names {
		h := *rf.Hosts[name]
		h.Name = name
		out = append(out, h)
	}
	return out, nil
}

// --- internals ---

// load reads hosts.json. Missing file is treated as an empty registry
// (first-time use). Malformed JSON returns a wrapped error.
func (r *Registry) load() (*registryFile, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &registryFile{Version: registryVersion, Hosts: map[string]*Host{}}, nil
		}
		return nil, fmt.Errorf("host.load: read %s: %w", r.path, err)
	}
	var rf registryFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("host.load: parse %s: %w", r.path, err)
	}
	if rf.Hosts == nil {
		rf.Hosts = map[string]*Host{}
	}
	if rf.Version == 0 {
		rf.Version = registryVersion
	}
	return &rf, nil
}

// save writes hosts.json atomically. Tmp-file + rename so partial
// writes don't corrupt the registry if the laptop is power-yanked
// mid-write. Caller must hold the flock.
func (r *Registry) save(rf *registryFile) error {
	rf.Version = registryVersion
	data, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return fmt.Errorf("host.save: marshal: %w", err)
	}
	data = append(data, '\n')
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("host.save: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("host.save: rename %s -> %s: %w", tmp, r.path, err)
	}
	return nil
}

// withLock load+mutate+save under an exclusive flock. Matches the
// state.Store.WithLock shape exactly so a future merged registry (if
// we ever unify state + hosts into one config file) is a structural
// refactor not a semantic one.
func (r *Registry) withLock(fn func(*registryFile) error) (err error) {
	lockFile, err := os.OpenFile(r.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("host.withLock: open lock: %w", err)
	}
	defer lockFile.Close()

	fd := int(lockFile.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return fmt.Errorf("host.withLock: acquire flock: %w", err)
	}
	defer func() {
		if uerr := unix.Flock(fd, unix.LOCK_UN); uerr != nil && err == nil {
			err = fmt.Errorf("host.withLock: release flock: %w", uerr)
		}
	}()

	rf, err := r.load()
	if err != nil {
		return err
	}
	if err := fn(rf); err != nil {
		return err
	}
	return r.save(rf)
}

// validateName rejects names that would confuse the --on flag parser
// or shell quoting on the remote. The same heuristic as resolveOn
// (cmd/canopy): if a name contains @ or :, it's an SSH target, not a
// registry name. Also reject empty, whitespace, and the literal
// "local" (reserved for future loopback transport).
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrHostInvalid)
	}
	if strings.ContainsAny(name, "@:/ \t\n") {
		return fmt.Errorf("%w: name %q contains @, :, /, or whitespace", ErrHostInvalid, name)
	}
	if name == "local" {
		return fmt.Errorf("%w: %q is reserved for future use", ErrHostInvalid, name)
	}
	return nil
}
