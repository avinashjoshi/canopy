package settings_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avinashjoshi/canopy/internal/settings"
)

// TestLoad_Missing covers the no-config-file case: must return defaults
// without erroring. Most users won't have a config.json and that's fine.
func TestLoad_Missing(t *testing.T) {
	t.Parallel()
	s, err := settings.Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := settings.Default()
	if s != want {
		t.Errorf("Load(missing) = %+v; want %+v", s, want)
	}
}

// TestLoad_FullConfig: all fields specified, no defaults needed.
func TestLoad_FullConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"ports": {"base": 4000, "project_stride": 500, "workspace_stride": 5}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := settings.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Ports.Base != 4000 || s.Ports.ProjectStride != 500 || s.Ports.WorkspaceStride != 5 {
		t.Errorf("Load = %+v", s)
	}
}

// TestLoad_PartialConfig: only one field specified — others must keep
// their defaults via the unmarshal-over-default merge.
func TestLoad_PartialConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"ports": {"base": 5000}}` // only base; rest should default
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := settings.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Ports.Base != 5000 {
		t.Errorf("Base = %d; want 5000", s.Ports.Base)
	}
	if s.Ports.ProjectStride != 1000 {
		t.Errorf("ProjectStride = %d; want default 1000", s.Ports.ProjectStride)
	}
	if s.Ports.WorkspaceStride != 10 {
		t.Errorf("WorkspaceStride = %d; want default 10", s.Ports.WorkspaceStride)
	}
}

// TestLoad_Invalid_BadJSON: malformed config -> error, return defaults.
func TestLoad_Invalid_BadJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{invalid`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := settings.Load(dir)
	if err == nil {
		t.Error("Load(bad json): want error; got nil")
	}
}

// TestLoad_Invalid_OutOfRange: base port outside 1024-65535 -> validation error.
func TestLoad_Invalid_OutOfRange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"ports": {"base": 100}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := settings.Load(dir)
	if err == nil {
		t.Error("Load(base=100): want validation error; got nil")
	}
}

// TestLoad_Invalid_StrideExceedsProject: workspace_stride > project_stride
// is nonsensical (a workspace would land in another project's range).
func TestLoad_Invalid_StrideExceedsProject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"ports": {"workspace_stride": 5000}}` // larger than default project_stride 1000
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := settings.Load(dir)
	if err == nil {
		t.Error("Load(stride > project_stride): want validation error; got nil")
	}
}
