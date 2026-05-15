// userconfig.go — user-level canopy preferences at ~/.canopy/config.json.
//
// Distinct from the per-project canopy.json (handled by Load / Discover in
// config.go): the user config holds settings the user wants persistent
// across projects, like source-root for `canopy init <git-url>` clones.
//
// Schema (v1):
//
//	{
//	  "version": 1,
//	  "source-root": "/home/avi/Work"
//	}
//
// version is a forward-compat marker. Load() warns if it sees a higher
// version than UserConfigSchemaVersion — migrating up is the new code's
// job; refusing to load down-revs the user out of their settings.
//
// Concurrency: mutations go through UserStore.WithLock which holds an
// advisory flock on config.json.lock for the read-modify-write window.
// Mirrors internal/state.Store's pattern so the locking semantics are
// uniform across canopy's persistent state.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// UserConfigFileName is the basename canopy looks for under ~/.canopy.
const UserConfigFileName = "config.json"

// UserConfigSchemaVersion is the version of the on-disk JSON schema.
// Cheap forward-compat insurance: writing it from day one means a future
// canopy that bumps the schema can detect old files and migrate them
// without guessing.
const UserConfigSchemaVersion = 1

// UserConfig holds user-level preferences persisted at
// ~/.canopy/config.json. New settings get a JSON tag here; the matching
// CLI key in cmd/canopy/config.go uses the JSON tag verbatim (e.g.
// `canopy config set source-root <path>` writes to SourceRoot).
type UserConfig struct {
	// Version is the schema version. Set on Save if zero. Load surfaces
	// a warning (not an error) if it reads a value > UserConfigSchemaVersion
	// so old binaries don't silently mishandle a newer file.
	Version int `json:"version"`

	// SourceRoot is the directory where `canopy init <git-url>` clones
	// repos by default. Empty means "use the default" (~/.canopy/sources).
	// Set via `canopy config set source-root <path>`.
	SourceRoot string `json:"source-root,omitempty"`
}

// UserStore is the read/write handle for ~/.canopy/config.json. Mirrors
// internal/state.Store's shape so callers learn one locking idiom.
type UserStore struct {
	path     string
	lockPath string
}

// NewUserStore returns a UserStore rooted at canopyHome (typically
// ~/.canopy). Does NOT create the directory — caller's responsibility,
// same as state.NewStore.
func NewUserStore(canopyHome string) (*UserStore, error) {
	if canopyHome == "" {
		return nil, errors.New("config.NewUserStore: empty canopyHome")
	}
	return &UserStore{
		path:     filepath.Join(canopyHome, UserConfigFileName),
		lockPath: filepath.Join(canopyHome, UserConfigFileName+".lock"),
	}, nil
}

// Path returns the absolute path to config.json. Useful for error
// messages so users know which file to inspect.
func (s *UserStore) Path() string { return s.path }

// Load reads config.json. A missing file returns a zero-value
// UserConfig and no error — that's the default state for a fresh user.
// A malformed file returns a wrapped error pointing at the path so the
// user can edit/repair it.
//
// Reads outside WithLock are tolerated: callers that only need a snapshot
// (`canopy config get`, init-time source-root lookup) accept that a
// concurrent set may have just landed and they'll pick it up next call.
func (s *UserStore) Load() (*UserConfig, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return &UserConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config.Load: read %s: %w", s.path, err)
	}
	var c UserConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config.Load: parse %s: %w", s.path, err)
	}
	if c.Version > UserConfigSchemaVersion {
		log.Warn("config.user.load.version_ahead",
			"file_version", c.Version,
			"binary_version", UserConfigSchemaVersion,
			"path", s.path,
			"action", "loading anyway; upgrade canopy if fields appear missing")
	}
	return &c, nil
}

// Save writes config to config.json atomically via tmpfile + rename.
// POSIX guarantees rename is atomic within a filesystem, so readers
// see either the old file or the new one — never a half-written doc.
//
// Callers that aren't holding WithLock race against concurrent writers
// and may clobber. Almost nobody should call Save directly; use
// WithLock from set-style commands instead.
func (s *UserStore) Save(c *UserConfig) error {
	if c.Version == 0 {
		c.Version = UserConfigSchemaVersion
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config.Save: marshal: %w", err)
	}
	data = append(data, '\n')

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("config.Save: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config.Save: rename: %w", err)
	}
	return nil
}

// WithLock acquires an exclusive advisory flock on config.json.lock,
// loads the current UserConfig, runs fn, saves, and releases. Mirrors
// internal/state.Store.WithLock.
//
// Two `canopy config set ...` calls in parallel terminals serialize:
// the second blocks on Flock(LOCK_EX) until the first releases. No data
// loss from interleaved read-modify-write windows.
func (s *UserStore) WithLock(fn func(*UserConfig) error) (err error) {
	lockFile, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("config.WithLock: open lock: %w", err)
	}
	defer lockFile.Close()

	fd := int(lockFile.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return fmt.Errorf("config.WithLock: acquire flock: %w", err)
	}
	defer func() {
		if uerr := unix.Flock(fd, unix.LOCK_UN); uerr != nil && err == nil {
			err = fmt.Errorf("config.WithLock: release flock: %w", uerr)
		}
	}()

	c, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(c); err != nil {
		return err
	}
	return s.Save(c)
}

// SourceRootSource names where an effective source-root value came from.
// Used by `canopy config get source-root` to annotate the printed value
// with its origin so users know whether to edit the env, the config, or
// rely on the default.
type SourceRootSource string

const (
	SourceRootFromEnv     SourceRootSource = "env"     // $CANOPY_SOURCE_ROOT
	SourceRootFromConfig  SourceRootSource = "config"  // ~/.canopy/config.json
	SourceRootFromDefault SourceRootSource = "default" // ~/.canopy/sources
)

// SourceRootEnvVar is the env var that overrides the configured source-root.
// Exported so tests and `canopy config list` can name it without hardcoding.
const SourceRootEnvVar = "CANOPY_SOURCE_ROOT"

// ResolveSourceRoot returns the effective source-root with its origin.
// Precedence (highest wins):
//  1. $CANOPY_SOURCE_ROOT (env)
//  2. config.SourceRoot from ~/.canopy/config.json
//  3. Default: <canopyHome>/sources (typically ~/.canopy/sources)
//
// The returned path is not validated and not created on disk — callers
// that need the dir to exist call ensureSourceRoot just before clone.
// Setting a config value should not mutate the filesystem; setting then
// changing your mind shouldn't leave empty dirs behind.
//
// Tilde expansion: `~/foo` and `~` are expanded to $HOME-relative paths
// when reading. The shell normally does this before canopy sees an
// argument, but values coming from non-shell paths (TUI text input,
// hand-edited config.json) keep their literal `~`. Without expansion,
// filepath.Abs later produces nonsense like `/cwd/~/foo`. Expanding on
// READ keeps the JSON friendly to humans (config.json shows `~/Work`,
// not `/home/cassy/Work`) and bullet-proofs every caller.
//
// canopyHome is passed in (not derived from os.UserHomeDir) so tests
// can inject a tempdir without monkey-patching HOME.
func ResolveSourceRoot(c *UserConfig, canopyHome string) (path string, source SourceRootSource) {
	if env := os.Getenv(SourceRootEnvVar); env != "" {
		return ExpandTilde(env), SourceRootFromEnv
	}
	if c != nil && c.SourceRoot != "" {
		return ExpandTilde(c.SourceRoot), SourceRootFromConfig
	}
	return filepath.Join(canopyHome, "sources"), SourceRootFromDefault
}

// ExpandTilde returns p with a leading `~` or `~/` expanded to the
// current user's home dir. Other forms (`~user/...`) are left unchanged
// because we'd need /etc/passwd parsing for the general case and that's
// rare enough to not be worth the complexity.
//
// Used by ResolveSourceRoot AND by callers that resolve user-typed
// destinations (e.g. the 2nd positional of `canopy init <url> <dest>`).
//
// Public because cmd/canopy and internal/ui both need it.
func ExpandTilde(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Best-effort: if we can't resolve $HOME, return the literal
		// path. The caller's filepath.Abs may produce something
		// surprising, but failing loudly elsewhere beats silently
		// substituting an empty string here.
		return p
	}
	if p == "~" {
		return home
	}
	if len(p) >= 2 && p[1] == '/' {
		return filepath.Join(home, p[2:])
	}
	// `~name` form — leave alone. We don't /etc/passwd lookup.
	return p
}
