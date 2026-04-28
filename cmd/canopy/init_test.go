package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/state"
)

// TestRunInit_Fresh: cwd has no canopy.json. runInit writes it, registers
// the project in state.Projects, and prints the next-steps block.
func TestRunInit_Fresh(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	cwd := t.TempDir()

	var buf bytes.Buffer
	if err := runInit(cwd, initOptions{}, &buf); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	// canopy.json written.
	if _, err := os.Stat(filepath.Join(cwd, "canopy.json")); err != nil {
		t.Errorf("canopy.json not written: %v", err)
	}
	// State entry registered.
	store, _ := state.NewStore(filepath.Join(fakeHome, ".canopy"))
	st, err := store.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	canonical, _ := canonicalize(cwd)
	if _, ok := st.Projects[canonical]; !ok {
		t.Errorf("project not registered in state.Projects (key=%q); have %v", canonical, st.Projects)
	}
	// Output makes sense.
	if !strings.Contains(buf.String(), "Wrote") {
		t.Errorf("expected 'Wrote' in output: %q", buf.String())
	}
}

// TestRunInit_Idempotent: re-running runInit on an already-initialized
// dir without --force prints the friendly message and exits 0. State is
// unchanged.
func TestRunInit_Idempotent(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	cwd := t.TempDir()

	// First init.
	var b1 bytes.Buffer
	if err := runInit(cwd, initOptions{}, &b1); err != nil {
		t.Fatalf("first runInit: %v", err)
	}

	// Second init — should print friendly message, not error.
	var b2 bytes.Buffer
	if err := runInit(cwd, initOptions{}, &b2); err != nil {
		t.Fatalf("second runInit: %v", err)
	}
	if !strings.Contains(b2.String(), "already initialized") {
		t.Errorf("expected 'already initialized' in second output: %q", b2.String())
	}
}

// TestRunInit_Force: --force overwrites an existing canopy.json.
func TestRunInit_Force(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	cwd := t.TempDir()

	// Pre-existing canopy.json with custom content.
	customJSON := `{"scripts": {"setup": "weird-path"}}`
	if err := os.WriteFile(filepath.Join(cwd, "canopy.json"), []byte(customJSON), 0o644); err != nil {
		t.Fatalf("write existing canopy.json: %v", err)
	}

	var buf bytes.Buffer
	if err := runInit(cwd, initOptions{Force: true}, &buf); err != nil {
		t.Fatalf("runInit force: %v", err)
	}

	// canopy.json was overwritten — should no longer contain "weird-path".
	written, err := os.ReadFile(filepath.Join(cwd, "canopy.json"))
	if err != nil {
		t.Fatalf("read canopy.json: %v", err)
	}
	if strings.Contains(string(written), "weird-path") {
		t.Errorf("canopy.json not overwritten under --force: %s", written)
	}
}

// TestRunInit_RefusesBasenameCollision: state.json has a project at /a/foo,
// running canopy init in /b/foo (different root, same basename) refuses
// with the user-facing error and DOES NOT write canopy.json.
//
// IRON-RULE regression: refused init must leave disk unchanged.
func TestRunInit_RefusesBasenameCollision(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	canopyHome := filepath.Join(fakeHome, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir canopyHome: %v", err)
	}

	// Pre-load state.json with a project at /a/cravd registered.
	preexisting := `{
		"schema_version": 2,
		"projects": {"/a/cravd": {"root": "/a/cravd", "port_base": 7000}},
		"workspaces": []
	}`
	statePath := filepath.Join(canopyHome, "state.json")
	if err := os.WriteFile(statePath, []byte(preexisting), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	stateBefore, _ := os.ReadFile(statePath)

	// Build a colliding cwd: different absolute path, same basename.
	parent := t.TempDir()
	collidingDir := filepath.Join(parent, "cravd")
	if err := os.MkdirAll(collidingDir, 0o755); err != nil {
		t.Fatalf("mkdir colliding: %v", err)
	}

	var buf bytes.Buffer
	err := runInit(collidingDir, initOptions{}, &buf)
	if err == nil {
		t.Fatalf("runInit on colliding basename: got nil err, want collision refusal")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("error message should mention 'already registered': %v", err)
	}

	// canopy.json must NOT have been written.
	if _, err := os.Stat(filepath.Join(collidingDir, "canopy.json")); !os.IsNotExist(err) {
		t.Errorf("canopy.json was written despite collision refusal")
	}

	// state.json must be byte-identical.
	stateAfter, _ := os.ReadFile(statePath)
	if string(stateBefore) != string(stateAfter) {
		t.Errorf("state.json mutated by refused init\nbefore: %s\nafter:  %s", stateBefore, stateAfter)
	}
}

// TestRunInit_ConductorMirror: a conductor.json next to cwd has its
// scripts mirrored verbatim into the new canopy.json. Existing behavior;
// the refactor must not have broken it.
func TestRunInit_ConductorMirror(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	cwd := t.TempDir()

	conductorJSON := `{"scripts": {"setup": "bin/conductor-setup", "run": "bin/conductor-run", "archive": "bin/conductor-archive"}}`
	if err := os.WriteFile(filepath.Join(cwd, "conductor.json"), []byte(conductorJSON), 0o644); err != nil {
		t.Fatalf("write conductor.json: %v", err)
	}

	var buf bytes.Buffer
	if err := runInit(cwd, initOptions{}, &buf); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if !strings.Contains(buf.String(), "mirrored scripts from") {
		t.Errorf("expected mirror notice in output: %q", buf.String())
	}

	written, _ := os.ReadFile(filepath.Join(cwd, "canopy.json"))
	if !strings.Contains(string(written), "bin/conductor-setup") {
		t.Errorf("canopy.json missing mirrored conductor scripts: %s", written)
	}
}

// TestRunInit_RegisterIsIdempotent: running runInit twice with --force
// does not duplicate the project entry or change PortBase if already set.
func TestRunInit_RegisterIsIdempotent(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	cwd := t.TempDir()
	canonical, _ := canonicalize(cwd)

	// Pre-write a state entry with PortBase already allocated.
	canopyHome := filepath.Join(fakeHome, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	statePath := filepath.Join(canopyHome, "state.json")
	preexisting := fmt.Sprintf(`{
		"schema_version": 2,
		"projects": {%q: {"root": %q, "port_base": 8500}},
		"workspaces": []
	}`, canonical, canonical)
	if err := os.WriteFile(statePath, []byte(preexisting), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var buf bytes.Buffer
	if err := runInit(cwd, initOptions{Force: true}, &buf); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	store, _ := state.NewStore(canopyHome)
	st, _ := store.Load()
	if got := st.Projects[canonical].PortBase; got != 8500 {
		t.Errorf("PortBase clobbered by re-init: got %d, want 8500", got)
	}
	// Sanity: still exactly one entry.
	if len(st.Projects) != 1 {
		t.Errorf("Projects count = %d, want 1", len(st.Projects))
	}
}

// noopErrorIs is a placeholder using errors.Is to keep the import alive
// in case future tests need it without re-imports.
var _ = errors.Is
