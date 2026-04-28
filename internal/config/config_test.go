package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/config"
)

func TestMain(m *testing.M) {
	teardown, _ := clog.Init(false)
	defer teardown()
	m.Run()
}

// validJSON is a minimal-but-complete canopy.json shared by happy-path tests.
const validJSON = `{
  "scripts": {
    "setup": "bin/canopy-setup",
    "run": "bin/dev",
    "archive": "bin/canopy-archive"
  }
}`

// TestDiscover_HappyPath: canopy.json at cwd, Discover finds it without
// walking.
func TestDiscover_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "canopy.json"), validJSON)

	got, err := config.Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != filepath.Join(dir, "canopy.json") {
		t.Errorf("Discover = %q; want %q", got, filepath.Join(dir, "canopy.json"))
	}
}

// TestDiscover_WalksUp: canopy.json at the project root, Discover starts
// from a deep subdirectory and walks up to find it.
func TestDiscover_WalksUp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(root, "canopy.json"), validJSON)

	got, err := config.Discover(deep)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != filepath.Join(root, "canopy.json") {
		t.Errorf("Discover = %q; want %q", got, filepath.Join(root, "canopy.json"))
	}
}

// TestDiscover_NotFound: no canopy.json anywhere up to the filesystem root,
// Discover returns ErrNotFound (not a panic, not a hang).
func TestDiscover_NotFound(t *testing.T) {
	t.Parallel()
	// t.TempDir is normally under /tmp which has no canopy.json. If for some
	// reason a sibling test or a hostile environment created one above, the
	// test would be wrong — but in practice this is fine.
	_, err := config.Discover(t.TempDir())
	if !errors.Is(err, config.ErrNotFound) {
		t.Errorf("Discover(no-canopy-anywhere): got %v; want errors.Is(... ErrNotFound)", err)
	}
}

// TestLoad_HappyPath verifies the full parse + derived-fields flow.
func TestLoad_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "canopy.json")
	writeFile(t, path, validJSON)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scripts.Setup != "bin/canopy-setup" {
		t.Errorf("Setup = %q", cfg.Scripts.Setup)
	}
	if cfg.Scripts.Run != "bin/dev" {
		t.Errorf("Run = %q", cfg.Scripts.Run)
	}
	if cfg.Scripts.Archive != "bin/canopy-archive" {
		t.Errorf("Archive = %q", cfg.Scripts.Archive)
	}
	if cfg.Project != filepath.Base(dir) {
		t.Errorf("Project = %q; want %q", cfg.Project, filepath.Base(dir))
	}
	if cfg.ProjectRoot == "" {
		t.Error("ProjectRoot is empty after Load")
	}
}

// TestLoad_BadJSON covers parse failure: the file exists but isn't valid JSON.
func TestLoad_BadJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "canopy.json")
	writeFile(t, path, `{"scripts": {`) // truncated

	_, err := config.Load(path)
	if !errors.Is(err, config.ErrInvalid) {
		t.Errorf("Load(bad-json): got %v; want errors.Is(... ErrInvalid)", err)
	}
}

// TestLoad_MissingRequiredFields covers validation: file parses but a
// required script is missing.
func TestLoad_MissingRequiredFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "canopy.json")
	// Missing scripts.archive.
	writeFile(t, path, `{"scripts": {"setup": "s", "run": "r"}}`)

	_, err := config.Load(path)
	if !errors.Is(err, config.ErrInvalid) {
		t.Errorf("Load(missing-archive): got %v; want errors.Is(... ErrInvalid)", err)
	}
}

// TestLoad_FileMissing covers the case where Discover succeeded but Load
// is given a bad path. Should be a plain os error, not a sentinel.
func TestLoad_FileMissing(t *testing.T) {
	t.Parallel()
	_, err := config.Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Errorf("Load(missing-file): want error; got nil")
	}
	if errors.Is(err, config.ErrInvalid) {
		t.Errorf("Load(missing-file) returned ErrInvalid; should be a plain io error")
	}
}

// TestDiscoverAndLoad: convenience wrapper that's the most common entry
// point for the CLI.
func TestDiscoverAndLoad(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deep := filepath.Join(root, "sub", "dir")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(root, "canopy.json"), validJSON)

	cfg, err := config.DiscoverAndLoad(deep)
	if err != nil {
		t.Fatalf("DiscoverAndLoad: %v", err)
	}
	if cfg.Project != filepath.Base(root) {
		t.Errorf("Project = %q; want %q", cfg.Project, filepath.Base(root))
	}
}

// writeFile is a tiny test helper for setting up canopy.json fixtures.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}
