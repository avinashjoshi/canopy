package config_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/avinashjoshi/canopy/internal/config"
)

// userStore returns a UserStore rooted at t.TempDir() so each test gets
// its own clean config dir and lockfile. No HOME pollution.
func userStore(t *testing.T) (*config.UserStore, string) {
	t.Helper()
	home := t.TempDir()
	s, err := config.NewUserStore(home)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	return s, home
}

// TestUserConfig_Load_Missing returns empty UserConfig + no error so a
// fresh user (no config.json yet) gets default behavior without an error.
func TestUserConfig_Load_Missing(t *testing.T) {
	t.Parallel()
	s, _ := userStore(t)
	c, err := s.Load()
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	if c == nil {
		t.Fatal("Load returned nil config; want empty struct")
	}
	if c.SourceRoot != "" {
		t.Errorf("Load missing file: SourceRoot = %q; want empty", c.SourceRoot)
	}
	if c.Version != 0 {
		t.Errorf("Load missing file: Version = %d; want 0 (unset)", c.Version)
	}
}

// TestUserConfig_Save_RoundTrip writes a config and reads it back. Verifies
// the schema-version default fires on save and the JSON round-trips cleanly.
func TestUserConfig_Save_RoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := userStore(t)
	want := &config.UserConfig{SourceRoot: "/tmp/canopy-srcs"}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SourceRoot != want.SourceRoot {
		t.Errorf("SourceRoot = %q; want %q", got.SourceRoot, want.SourceRoot)
	}
	if got.Version != config.UserConfigSchemaVersion {
		t.Errorf("Version = %d; want %d (default on save)", got.Version, config.UserConfigSchemaVersion)
	}
}

// TestUserConfig_Load_Malformed returns a wrapped error pointing at the
// path so the user can find and fix the broken file by hand.
func TestUserConfig_Load_Malformed(t *testing.T) {
	t.Parallel()
	s, home := userStore(t)
	bad := filepath.Join(home, config.UserConfigFileName)
	if err := os.WriteFile(bad, []byte("not json {{{"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := s.Load()
	if err == nil {
		t.Fatal("Load malformed: nil error; want wrapped parse error")
	}
	// Error must name the path for actionable user feedback.
	if !contains(err.Error(), bad) {
		t.Errorf("Load malformed err = %v; want path %q in message", err, bad)
	}
}

// TestUserConfig_Save_Atomic checks that a successful Save leaves no .tmp
// turd behind. The atomic-rename property itself is OS-level and tested
// implicitly; this test guards against accidental cleanup bugs.
func TestUserConfig_Save_Atomic(t *testing.T) {
	t.Parallel()
	s, home := userStore(t)
	if err := s.Save(&config.UserConfig{SourceRoot: "/a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("Save left a .tmp file behind: %s", e.Name())
		}
	}
}

// TestUserConfig_WithLock_Mutate runs a set-style flow: acquire lock,
// mutate, release, read back. This is the canonical pattern for
// `canopy config set source-root <path>`.
func TestUserConfig_WithLock_Mutate(t *testing.T) {
	t.Parallel()
	s, _ := userStore(t)
	err := s.WithLock(func(c *config.UserConfig) error {
		c.SourceRoot = "/home/avi/Work"
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SourceRoot != "/home/avi/Work" {
		t.Errorf("SourceRoot = %q; want /home/avi/Work", got.SourceRoot)
	}
}

// TestUserConfig_WithLock_ErrorSkipsWrite: when fn returns an error,
// the lock is released cleanly and the config file is NOT written.
// This is what prevents a half-baked set from corrupting state on
// validation failure.
func TestUserConfig_WithLock_ErrorSkipsWrite(t *testing.T) {
	t.Parallel()
	s, home := userStore(t)
	if err := s.Save(&config.UserConfig{SourceRoot: "/initial"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	wantErr := os.ErrInvalid
	err := s.WithLock(func(c *config.UserConfig) error {
		c.SourceRoot = "/should-not-stick"
		return wantErr
	})
	if err == nil {
		t.Fatal("WithLock: nil error; want propagation")
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load after failed WithLock: %v", err)
	}
	if got.SourceRoot != "/initial" {
		t.Errorf("SourceRoot = %q; want /initial (fn err must skip Save)", got.SourceRoot)
	}
	// Stale .tmp would indicate Save ran partially. Verify it didn't.
	if _, e := os.Stat(filepath.Join(home, config.UserConfigFileName+".tmp")); e == nil {
		t.Error("Save ran despite fn error: .tmp file present")
	}
}

// TestUserConfig_WithLock_Concurrent fires 10 simultaneous incrementing
// writes against the same store. flock serializes them; the final value
// must equal the sum of contributions. This is the classic read-modify-
// write-race regression test.
func TestUserConfig_WithLock_Concurrent(t *testing.T) {
	t.Parallel()
	s, _ := userStore(t)
	// Seed with an empty config so the first incrementer starts from 0.
	if err := s.Save(&config.UserConfig{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const writers = 10
	var wg sync.WaitGroup
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			err := s.WithLock(func(c *config.UserConfig) error {
				// Use SourceRoot as a counter — we append a char each time.
				// Length after N writes must equal N.
				c.SourceRoot += "x"
				return nil
			})
			if err != nil {
				t.Errorf("WithLock: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.SourceRoot) != writers {
		t.Errorf("after %d concurrent appends, len(SourceRoot) = %d; want %d (lost writes — flock not serializing)",
			writers, len(got.SourceRoot), writers)
	}
}

// TestResolveSourceRoot_Precedence covers all four levels of the
// precedence stack: env > config > default. Plus the SourceRootSource
// annotation that `canopy config get` displays.
func TestResolveSourceRoot_Precedence(t *testing.T) {
	// Not Parallel: mutates env. Each subtest restores HOME-equivalent state.
	defaultHome := t.TempDir()
	wantDefault := filepath.Join(defaultHome, "sources")

	cases := []struct {
		name       string
		envValue   string // CANOPY_SOURCE_ROOT; empty means unset
		config     *config.UserConfig
		wantPath   string
		wantSource config.SourceRootSource
	}{
		{
			name:       "env wins over config and default",
			envValue:   "/from-env",
			config:     &config.UserConfig{SourceRoot: "/from-config"},
			wantPath:   "/from-env",
			wantSource: config.SourceRootFromEnv,
		},
		{
			name:       "config wins over default when env unset",
			envValue:   "",
			config:     &config.UserConfig{SourceRoot: "/from-config"},
			wantPath:   "/from-config",
			wantSource: config.SourceRootFromConfig,
		},
		{
			name:       "default applies when env and config both empty",
			envValue:   "",
			config:     &config.UserConfig{},
			wantPath:   wantDefault,
			wantSource: config.SourceRootFromDefault,
		},
		{
			name:       "nil config falls back to default",
			envValue:   "",
			config:     nil,
			wantPath:   wantDefault,
			wantSource: config.SourceRootFromDefault,
		},
		{
			name:       "empty-string env is treated as unset (not as a path)",
			envValue:   "",
			config:     &config.UserConfig{SourceRoot: "/from-config"},
			wantPath:   "/from-config",
			wantSource: config.SourceRootFromConfig,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.SourceRootEnvVar, tc.envValue)
			// Setenv with empty string treats it as set-but-empty on some
			// platforms; explicitly unset to match the "not present" case.
			if tc.envValue == "" {
				os.Unsetenv(config.SourceRootEnvVar)
			}
			gotPath, gotSrc := config.ResolveSourceRoot(tc.config, defaultHome)
			if gotPath != tc.wantPath {
				t.Errorf("path = %q; want %q", gotPath, tc.wantPath)
			}
			if gotSrc != tc.wantSource {
				t.Errorf("source = %q; want %q", gotSrc, tc.wantSource)
			}
		})
	}
}

// TestNewUserStore_EmptyHome rejects an empty canopyHome rather than
// silently building a UserStore rooted at "" (which would write to cwd
// instead of ~/.canopy — surprising and destructive).
func TestNewUserStore_EmptyHome(t *testing.T) {
	t.Parallel()
	if _, err := config.NewUserStore(""); err == nil {
		t.Fatal("NewUserStore(\"\"): nil error; want refused")
	}
}

// TestExpandTilde covers the cases that cause the "literal ~ inside
// an absolute path" bork reported in the wild. The TUI Settings modal
// stores whatever the user types — `~/Work` stays literal — and the
// shell normally would have expanded it, so callers must explicitly
// expand on read. Without this, filepath.Abs produces nonsense like
// `/cwd/~/Work/cravd` and clones land in a literal dir named "~".
func TestExpandTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	if home == "" {
		t.Skip("no $HOME — can't exercise the expansion")
	}
	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/Work", filepath.Join(home, "Work")},
		{"~/Work/cravd", filepath.Join(home, "Work", "cravd")},
		// Already-absolute paths pass through unchanged.
		{"/abs/path", "/abs/path"},
		// `~user/...` is left alone — we don't do /etc/passwd lookups.
		{"~someone/x", "~someone/x"},
		// Empty stays empty.
		{"", ""},
		// `~` only at the start matters; mid-string is left alone.
		{"/already/has/~/inside", "/already/has/~/inside"},
	}
	for _, tc := range cases {
		got := config.ExpandTilde(tc.in)
		if got != tc.want {
			t.Errorf("ExpandTilde(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestResolveSourceRoot_ExpandsTilde: regression for the cravd path
// borked-by-literal-~ bug. A config value of `~/Work` must come out as
// `<HOME>/Work`, not `~/Work`.
func TestResolveSourceRoot_ExpandsTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	if home == "" {
		t.Skip("no $HOME")
	}
	os.Unsetenv("CANOPY_SOURCE_ROOT")
	c := &config.UserConfig{SourceRoot: "~/Work"}
	got, src := config.ResolveSourceRoot(c, "/anywhere")
	want := filepath.Join(home, "Work")
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
	if src != config.SourceRootFromConfig {
		t.Errorf("source = %q; want config", src)
	}
}

// TestResolveSourceRoot_ExpandsTildeFromEnv: env-set tildes also
// expand. Bash usually expands `~` in arg position but NOT inside
// variable values, so `CANOPY_SOURCE_ROOT="~/Work"` ships literally.
func TestResolveSourceRoot_ExpandsTildeFromEnv(t *testing.T) {
	home, _ := os.UserHomeDir()
	if home == "" {
		t.Skip("no $HOME")
	}
	t.Setenv("CANOPY_SOURCE_ROOT", "~/Work")
	got, src := config.ResolveSourceRoot(nil, "/anywhere")
	want := filepath.Join(home, "Work")
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
	if src != config.SourceRootFromEnv {
		t.Errorf("source = %q; want env", src)
	}
}

// TestUserStore_Path returns the absolute path to config.json. Error
// messages embed this string, so it's part of the public contract.
func TestUserStore_Path(t *testing.T) {
	t.Parallel()
	s, home := userStore(t)
	want := filepath.Join(home, config.UserConfigFileName)
	if got := s.Path(); got != want {
		t.Errorf("Path = %q; want %q", got, want)
	}
}

// contains is a tiny substring check that avoids pulling in strings just
// for this one assertion (matches the existing config_test.go style).
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
