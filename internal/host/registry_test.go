package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRegistry_AddListResolveRemove covers the happy path round-trip:
// add → list → resolve → remove. Verifies persistence across registry
// instances (simulating the laptop being killed mid-session and a
// fresh canopy process picking up the same hosts.json).
func TestRegistry_AddListResolveRemove(t *testing.T) {
	home := t.TempDir()
	reg, err := NewRegistry(home)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if err := reg.Add("tower", Host{SSHTarget: "avi@tower.tail.ts.net"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := reg.AddProject("tower", "canopy", "/home/avi/Work/canopy"); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	if err := reg.Add("fly-iad", Host{SSHTarget: "fly@iad.fly.dev"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Reopen — simulates a fresh canopy process.
	reg2, err := NewRegistry(home)
	if err != nil {
		t.Fatalf("NewRegistry (reopen): %v", err)
	}

	list, err := reg2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2; got %+v", len(list), list)
	}
	// Sorted: fly-iad < tower
	if list[0].Name != "fly-iad" || list[1].Name != "tower" {
		t.Errorf("list order: got %v %v, want fly-iad then tower", list[0].Name, list[1].Name)
	}

	h, err := reg2.Resolve("tower")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if h.SSHTarget != "avi@tower.tail.ts.net" {
		t.Errorf("ssh_target: got %q, want avi@tower.tail.ts.net", h.SSHTarget)
	}
	if h.Projects["canopy"] != "/home/avi/Work/canopy" {
		t.Errorf("projects[canopy]: got %q, want /home/avi/Work/canopy", h.Projects["canopy"])
	}
	if h.Type != "ssh" {
		t.Errorf("type: got %q, want ssh (default)", h.Type)
	}
	if h.AddedAt.IsZero() {
		t.Error("added_at should be auto-populated")
	}

	if err := reg2.Remove("tower"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Verify removal persists.
	reg3, _ := NewRegistry(home)
	if _, err := reg3.Resolve("tower"); !errors.Is(err, ErrHostNotFound) {
		t.Errorf("expected ErrHostNotFound after Remove, got %v", err)
	}
}

// TestRegistry_ProjectLifecycle covers AddProject / GetProject /
// ListProjects / RemoveProject in one round-trip. Verifies the
// multi-project shape that v0.17.0 Phase 1a's revised schema enables:
// one host can serve multiple projects, each with its own remote path.
func TestRegistry_ProjectLifecycle(t *testing.T) {
	home := t.TempDir()
	reg, _ := NewRegistry(home)
	if err := reg.Add("tower", Host{SSHTarget: "a@t"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := reg.AddProject("tower", "canopy", "/home/cassy/Work/canopy"); err != nil {
		t.Fatalf("AddProject canopy: %v", err)
	}
	if err := reg.AddProject("tower", "cravd", "/home/cassy/Work/cravd"); err != nil {
		t.Fatalf("AddProject cravd: %v", err)
	}

	// Lookup the path back.
	path, err := reg.GetProject("tower", "canopy")
	if err != nil || path != "/home/cassy/Work/canopy" {
		t.Errorf("GetProject(tower,canopy): got %q,%v want /home/cassy/Work/canopy,nil", path, err)
	}

	// List sorted.
	projs, err := reg.ListProjects("tower")
	if err != nil || len(projs) != 2 {
		t.Fatalf("ListProjects: got %v,%v want 2 entries,nil", projs, err)
	}
	if projs[0].Name != "canopy" || projs[1].Name != "cravd" {
		t.Errorf("ListProjects order: got %v,%v want canopy,cravd", projs[0].Name, projs[1].Name)
	}

	// Duplicate add rejected.
	if err := reg.AddProject("tower", "canopy", "/another/path"); !errors.Is(err, ErrProjectExists) {
		t.Errorf("duplicate AddProject: got %v want ErrProjectExists", err)
	}

	// Remove + verify lookup fails.
	if err := reg.RemoveProject("tower", "canopy"); err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}
	if _, err := reg.GetProject("tower", "canopy"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("GetProject after Remove: got %v want ErrProjectNotFound", err)
	}

	// Removing a project that doesn't exist.
	if err := reg.RemoveProject("tower", "missing"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("RemoveProject missing: got %v want ErrProjectNotFound", err)
	}

	// Operations on unknown host.
	if err := reg.AddProject("unknown", "x", "/p"); !errors.Is(err, ErrHostNotFound) {
		t.Errorf("AddProject on unknown host: got %v want ErrHostNotFound", err)
	}
}

// TestRegistry_V1Migration verifies that hand-edited v1 hosts.json
// files (with project_path on the host) get migrated to v2's projects
// map on load, with the original path stored under the "default" key.
func TestRegistry_V1Migration(t *testing.T) {
	home := t.TempDir()
	v1json := `{
  "version": 1,
  "hosts": {
    "tower": {
      "type": "ssh",
      "ssh_target": "avi@tower",
      "project_path": "/home/avi/Work/canopy",
      "added_at": "2026-05-12T00:00:00Z"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(home, "hosts.json"), []byte(v1json), 0o644); err != nil {
		t.Fatalf("seed v1 json: %v", err)
	}
	reg, _ := NewRegistry(home)
	h, err := reg.Resolve("tower")
	if err != nil {
		t.Fatalf("Resolve after v1 load: %v", err)
	}
	if got := h.Projects["default"]; got != "/home/avi/Work/canopy" {
		t.Errorf("v1 project_path → projects.default: got %q want /home/avi/Work/canopy", got)
	}
	if h.LegacyProjectPath != "" {
		t.Errorf("LegacyProjectPath should be cleared after migration; got %q", h.LegacyProjectPath)
	}

	// Trigger a Save by adding another host. Verify the on-disk file
	// is now v2 (no project_path key on the migrated host).
	if err := reg.Add("other", Host{SSHTarget: "x@y"}); err != nil {
		t.Fatalf("Add to trigger save: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(home, "hosts.json"))
	if strings.Contains(string(data), `"project_path":`) {
		t.Errorf("v2 hosts.json should not contain project_path key; got:\n%s", data)
	}
	if !strings.Contains(string(data), `"version": 2`) {
		t.Errorf("hosts.json should declare version=2; got:\n%s", data)
	}
}

// TestRegistry_AddDuplicateRejected ensures `canopy host add tower x`
// followed by a second call with a different target doesn't silently
// overwrite. User must Remove first.
func TestRegistry_AddDuplicateRejected(t *testing.T) {
	home := t.TempDir()
	reg, _ := NewRegistry(home)
	if err := reg.Add("tower", Host{SSHTarget: "a@x"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := reg.Add("tower", Host{SSHTarget: "different@y"})
	if !errors.Is(err, ErrHostExists) {
		t.Errorf("second Add should return ErrHostExists, got %v", err)
	}
	// Verify the original wasn't overwritten.
	h, _ := reg.Resolve("tower")
	if h.SSHTarget != "a@x" {
		t.Errorf("original ssh_target should be preserved, got %q", h.SSHTarget)
	}
}

// TestRegistry_RemoveUnknownErrors covers `canopy host rm <unknown>`.
func TestRegistry_RemoveUnknownErrors(t *testing.T) {
	home := t.TempDir()
	reg, _ := NewRegistry(home)
	err := reg.Remove("nonexistent")
	if !errors.Is(err, ErrHostNotFound) {
		t.Errorf("Remove unknown: got %v, want ErrHostNotFound", err)
	}
}

// TestRegistry_ResolveUnknown covers the lookup path that the --on
// flag resolver calls. Specific error type lets the CLI print a
// targeted message ("did you forget to run `canopy host add`?").
func TestRegistry_ResolveUnknown(t *testing.T) {
	home := t.TempDir()
	reg, _ := NewRegistry(home)
	_, err := reg.Resolve("tower")
	if !errors.Is(err, ErrHostNotFound) {
		t.Errorf("Resolve unknown: got %v, want ErrHostNotFound", err)
	}
}

// TestRegistry_EmptyList ensures Resolve/List on a fresh registry
// returns sensible zero-values (not nil-deref panics).
func TestRegistry_EmptyList(t *testing.T) {
	home := t.TempDir()
	reg, _ := NewRegistry(home)
	list, err := reg.List()
	if err != nil {
		t.Fatalf("List on empty registry: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List on empty: got %d entries, want 0", len(list))
	}
}

// TestValidateName covers the name-format gate that prevents users
// from registering a name that would later be misinterpreted as a raw
// SSH target by the --on resolver (anything containing @ or :).
func TestValidateName(t *testing.T) {
	cases := []struct {
		name      string
		wantValid bool
	}{
		{"tower", true},
		{"fly-iad", true},
		{"home-server", true},
		{"box1", true},
		{"", false},
		{"avi@tower", false},   // looks like an SSH target
		{"tower:22", false},    // looks like an SSH target with port
		{"local", false},       // reserved
		{"with space", false},  // whitespace
		{"a/b", false},         // slash
		{"\tname", false},      // leading tab
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateName(c.name)
			if c.wantValid && err != nil {
				t.Errorf("validateName(%q) = %v, want nil", c.name, err)
			}
			if !c.wantValid && err == nil {
				t.Errorf("validateName(%q) = nil, want error", c.name)
			}
			if err != nil && !errors.Is(err, ErrHostInvalid) {
				t.Errorf("validateName(%q): error type = %v, want ErrHostInvalid", c.name, err)
			}
		})
	}
}

// TestRegistry_AtomicSave verifies that the hosts.json file lands
// atomically — no .tmp leftover, no partial write visible to a reader
// mid-flight. We can't reliably trigger a power-cut in a unit test;
// we just verify the .tmp file doesn't exist after Add returns.
func TestRegistry_AtomicSave(t *testing.T) {
	home := t.TempDir()
	reg, _ := NewRegistry(home)
	if err := reg.Add("tower", Host{SSHTarget: "a@b"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "hosts.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("hosts.json.tmp should not exist after Add; got %v", err)
	}
}

// TestRegistry_MalformedJSON_Errors covers the case where someone
// hand-edits hosts.json into a broken state. We surface a useful
// error rather than corrupting on save.
func TestRegistry_MalformedJSON_Errors(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "hosts.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed bad json: %v", err)
	}
	reg, _ := NewRegistry(home)
	_, err := reg.List()
	if err == nil {
		t.Errorf("List on malformed hosts.json should error")
	}
}

// TestRegistry_ConcurrentAddsSerializeUnderFlock spawns N goroutines
// that each call Add with a unique name. With the flock contract
// holding, all of them should succeed and the final list should
// contain all N hosts in deterministic order. Smoke test for the
// "two canopy processes running simultaneously" case.
func TestRegistry_ConcurrentAddsSerializeUnderFlock(t *testing.T) {
	const n = 8
	home := t.TempDir()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reg, err := NewRegistry(home)
			if err != nil {
				errs <- err
				return
			}
			name := fmt.Sprintf("h%d", i)
			if err := reg.Add(name, Host{SSHTarget: name + "@example", AddedAt: time.Now().UTC()}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Add: %v", err)
		}
	}

	reg, _ := NewRegistry(home)
	list, err := reg.List()
	if err != nil {
		t.Fatalf("List after concurrent adds: %v", err)
	}
	if len(list) != n {
		t.Errorf("concurrent adds: got %d hosts, want %d (lost an update under contention)", len(list), n)
	}
}
