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
// Projects is the host's per-project path map. Key is the project name
// (matches local project basename for cwd-driven lookup); value is the
// absolute path on the remote where that project lives. A host can
// host any number of projects — laptop has `canopy` and `cravd`, tower
// might have `canopy` only, fly-iad might have `canopy` AND `cravd` AND
// a private fork. The matching key is convention, not enforcement:
// `tower.Projects["canopy"]` is "what tower knows about a project named
// canopy", which is what the local `canopy` project gets dispatched into.
//
// Empty Projects map = host registered but no projects yet. Dispatch
// still works if --remote-cwd is passed explicitly, but the daily verb
// `canopy new --on tower` needs the project resolved.
type Host struct {
	Name      string            `json:"-"` // map key in registryFile.Hosts
	Type      string            `json:"type"`
	SSHTarget string            `json:"ssh_target,omitempty"`
	Projects  map[string]string `json:"projects,omitempty"`
	AddedAt   time.Time         `json:"added_at"`

	// LegacyProjectPath is preserved here ONLY for one-shot migration
	// from registry version 1 (single project_path per host). On load,
	// it gets moved into Projects under a "default" key with a warning
	// logged. New writes never include this field.
	LegacyProjectPath string `json:"project_path,omitempty"`
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

const registryVersion = 2

// Sentinels for the cmd/canopy/host.go CLI to detect specific cases.
var (
	ErrHostExists       = errors.New("host already exists in registry")
	ErrHostNotFound     = errors.New("host not found in registry")
	ErrHostInvalid      = errors.New("host name is invalid")
	ErrProjectExists    = errors.New("project already registered on this host")
	ErrProjectNotFound  = errors.New("project not registered on this host")
	ErrProjectInvalid   = errors.New("project name is invalid")
	ErrSSHTargetInvalid = errors.New("ssh target is invalid")
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

// AddProject registers a project on an existing host. Errors:
//   - ErrHostNotFound: host not registered
//   - ErrProjectExists: project already registered on this host (use
//     RemoveProject first, or use a different name)
//   - ErrProjectInvalid: name contains forbidden chars
//
// remotePath should be the absolute path on the remote where the
// project's canopy.json lives. v0.17.0 doesn't ping-probe the path
// (network might be down, host might be asleep) — Phase 1d's `host
// project init` wizard adds a connectivity check.
func (r *Registry) AddProject(hostName, projectName, remotePath string) error {
	if err := validateProjectName(projectName); err != nil {
		return err
	}
	return r.withLock(func(rf *registryFile) error {
		h, exists := rf.Hosts[hostName]
		if !exists {
			return fmt.Errorf("host.AddProject(%q, %q): %w", hostName, projectName, ErrHostNotFound)
		}
		if h.Projects == nil {
			h.Projects = map[string]string{}
		}
		if _, dup := h.Projects[projectName]; dup {
			return fmt.Errorf("host.AddProject(%q, %q): %w", hostName, projectName, ErrProjectExists)
		}
		h.Projects[projectName] = remotePath
		return nil
	})
}

// RemoveProject drops a project from a host's project map. Errors with
// ErrHostNotFound or ErrProjectNotFound; the host itself stays.
func (r *Registry) RemoveProject(hostName, projectName string) error {
	return r.withLock(func(rf *registryFile) error {
		h, exists := rf.Hosts[hostName]
		if !exists {
			return fmt.Errorf("host.RemoveProject(%q, %q): %w", hostName, projectName, ErrHostNotFound)
		}
		if _, found := h.Projects[projectName]; !found {
			return fmt.Errorf("host.RemoveProject(%q, %q): %w", hostName, projectName, ErrProjectNotFound)
		}
		delete(h.Projects, projectName)
		return nil
	})
}

// GetProject returns the remote path for a (host, project) pair.
// Distinguishes "host unknown" from "host known but project not
// registered on it" so the dispatcher can print the right error
// (the latter is recoverable with `canopy project add ... --on <host>`; the
// former requires `canopy host add` first).
func (r *Registry) GetProject(hostName, projectName string) (string, error) {
	rf, err := r.load()
	if err != nil {
		return "", err
	}
	h, exists := rf.Hosts[hostName]
	if !exists {
		return "", fmt.Errorf("host.GetProject(%q, %q): %w", hostName, projectName, ErrHostNotFound)
	}
	path, found := h.Projects[projectName]
	if !found {
		return "", fmt.Errorf("host.GetProject(%q, %q): %w", hostName, projectName, ErrProjectNotFound)
	}
	return path, nil
}

// ListProjects returns a host's projects as a name-sorted slice of
// (name, path) pairs. Empty slice (not nil) for hosts with no projects.
func (r *Registry) ListProjects(hostName string) ([]ProjectEntry, error) {
	rf, err := r.load()
	if err != nil {
		return nil, err
	}
	h, exists := rf.Hosts[hostName]
	if !exists {
		return nil, fmt.Errorf("host.ListProjects(%q): %w", hostName, ErrHostNotFound)
	}
	names := make([]string, 0, len(h.Projects))
	for name := range h.Projects {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ProjectEntry, 0, len(names))
	for _, name := range names {
		out = append(out, ProjectEntry{Name: name, Path: h.Projects[name]})
	}
	return out, nil
}

// ProjectEntry is the listing-friendly shape returned by ListProjects.
type ProjectEntry struct {
	Name string
	Path string
}

// AllProjectEntry is the cross-host listing shape returned by
// ListAllProjects. Each entry pairs the host name with the project
// name and remote path, enabling the `canopy project ls` view that
// shows everything you've registered anywhere.
type AllProjectEntry struct {
	HostName string
	Name     string
	Path     string
}

// ListAllProjects returns every (host, project) pair in the registry,
// sorted by host name then project name. Used by `canopy project ls`
// without --on filter ("show me everywhere I have stuff registered").
func (r *Registry) ListAllProjects() ([]AllProjectEntry, error) {
	rf, err := r.loadAndMaybePersistMigration()
	if err != nil {
		return nil, err
	}
	hostNames := make([]string, 0, len(rf.Hosts))
	for name := range rf.Hosts {
		hostNames = append(hostNames, name)
	}
	sort.Strings(hostNames)
	var out []AllProjectEntry
	for _, hostName := range hostNames {
		h := rf.Hosts[hostName]
		projNames := make([]string, 0, len(h.Projects))
		for name := range h.Projects {
			projNames = append(projNames, name)
		}
		sort.Strings(projNames)
		for _, projName := range projNames {
			out = append(out, AllProjectEntry{
				HostName: hostName,
				Name:     projName,
				Path:     h.Projects[projName],
			})
		}
	}
	return out, nil
}

// Resolve returns a copy of the named host. ErrHostNotFound if missing.
// Callers should treat the returned Host as read-only; mutations must
// go through Add (re-add after Remove) so they're persisted under the
// flock contract.
//
// Side effect: if the on-disk hosts.json is v1, this call persists the
// v2-migrated form to disk under the flock. That way the user sees a
// "freshly normalized" file after any read-only Resolve/List call,
// not just after a write-like Add/Remove.
func (r *Registry) Resolve(name string) (Host, error) {
	rf, err := r.loadAndMaybePersistMigration()
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
// Empty slice (not nil) when the registry has no entries. Like Resolve,
// triggers a persisted v1→v2 migration if needed.
func (r *Registry) List() ([]Host, error) {
	rf, err := r.loadAndMaybePersistMigration()
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
//
// Migrations: registry v1 stored a single `project_path` per host; v2
// stores a `projects: {name: path}` map. On load, any v1 host with a
// non-empty LegacyProjectPath has it folded into Projects under a
// "default" key and a warning is logged. New writes never serialize
// LegacyProjectPath (omitempty).
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
		rf.Version = 1
	}
	// v1 → v2 migration.
	if rf.Version < 2 {
		for name, h := range rf.Hosts {
			if h.LegacyProjectPath != "" {
				if h.Projects == nil {
					h.Projects = map[string]string{}
				}
				if _, exists := h.Projects["default"]; !exists {
					h.Projects["default"] = h.LegacyProjectPath
					log.Warn("host.registry.migrated", "host", name,
						"from", "v1 project_path",
						"to", "v2 projects.default",
						"path", h.LegacyProjectPath,
						"hint", "rename with `canopy project rm default --on "+name+"; canopy project add <name> <path> --on "+name+"`")
				}
				h.LegacyProjectPath = ""
			}
		}
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

// loadAndMaybePersistMigration loads + saves under the flock IF the
// on-disk file needed migration. Used by read-only methods (Resolve,
// List, GetProject) so the migrated shape lands on disk without
// requiring a write-like Add/Remove first. Reads on already-v2 files
// skip the flock entirely.
func (r *Registry) loadAndMaybePersistMigration() (*registryFile, error) {
	// Quick check: peek at the on-disk version. If it's already
	// current, skip the flock dance.
	data, rerr := os.ReadFile(r.path)
	if rerr == nil {
		var probe struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(data, &probe) == nil && probe.Version >= registryVersion {
			return r.load() // fast path: no migration needed
		}
	}
	// Either missing-file, malformed, or out-of-date version. Run
	// load (which handles the missing-file case + does the v1→v2
	// transform in-memory). If migration ran, persist under the flock.
	var rf *registryFile
	err := r.withLock(func(loaded *registryFile) error {
		rf = loaded
		return nil
	})
	return rf, err
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

// ValidateSSHTarget rejects an SSH target that could be misinterpreted
// as a command-line option by ssh, mosh, or a tool one of those wraps
// internally. canopy's own ssh/mosh call sites (internal/host/ssh.go)
// defend against this with an explicit "--" separator before the
// target — but that fix doesn't universally hold: confirmed by PoC
// that ssh-copy-id (invoked from internal/ui/update_host.go for the
// Hosts-tab "set up auth" flow) still forwards an option-shaped target
// to ITS OWN internal ssh invocation unprotected, even when canopy's
// own call to ssh-copy-id puts "--" before it. Rejecting at the point
// of use is the guarantee that holds regardless of how a given
// downstream tool behaves, or where the bad value originated (a
// tampered hosts.json bypasses any registration-time check entirely).
//
// Empty and leading-dash strings are rejected; anything else is a
// plausible hostname/IP/SSH alias and is accepted as-is — canopy
// doesn't validate DNS shape, only the option-injection shape.
func ValidateSSHTarget(target string) error {
	if target == "" {
		return fmt.Errorf("%w: empty SSH target", ErrSSHTargetInvalid)
	}
	if strings.HasPrefix(target, "-") {
		return fmt.Errorf("%w: %q starts with \"-\", which ssh/mosh/ssh-copy-id could parse as an option instead of a hostname", ErrSSHTargetInvalid, target)
	}
	return nil
}

// validateProjectName mirrors the host-name rules. Project names must
// match the local project basename (canopy walks up cwd, takes the
// final path segment as the project name), so the same character
// restrictions apply. Slashes and whitespace would break that.
func validateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrProjectInvalid)
	}
	if strings.ContainsAny(name, "@:/ \t\n") {
		return fmt.Errorf("%w: name %q contains @, :, /, or whitespace", ErrProjectInvalid, name)
	}
	return nil
}
