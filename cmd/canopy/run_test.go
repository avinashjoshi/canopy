package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunRunCmd_noCanopyJSON: invoking canopy run from a directory with
// no canopy.json above it returns ErrNotFound from the config layer with
// a hint pointing at the surface.
func TestRunRunCmd_noCanopyJSON(t *testing.T) {
	// Create a tmpdir that's outside any real canopy project (Chdir into
	// it so the discover walk-up hits filesystem root).
	tmpRoot := t.TempDir()
	origCwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	if err := os.Chdir(tmpRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Use a HOME outside the project tree so any /home/avi/canopy.json
	// in the walk-up doesn't confuse the test.
	t.Setenv("HOME", tmpRoot)

	// Walk up from a temp dir that has no parent canopy.json. We rely on
	// /tmp not having one above it, which is reliable in CI.
	cmd := newRunCmd()
	cmd.SetArgs([]string{})
	cmd.SetContext(context.Background())
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from canopy.json discovery, got nil")
	}
	if !strings.Contains(err.Error(), "canopy.json") {
		t.Errorf("error should mention canopy.json: %v", err)
	}
}

// TestRunRunCmd_emptyScriptsRun: a canopy.json with empty scripts.run
// should error with a message pointing at the field.
func TestRunRunCmd_emptyScriptsRun(t *testing.T) {
	tmpRoot := t.TempDir()
	origCwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	confPath := filepath.Join(tmpRoot, "canopy.json")
	// Empty scripts.run but non-empty setup/archive (they're required for
	// validate to pass — at least, setup and archive must be set).
	conf := `{
		"scripts": {
			"setup": "bin/canopy-setup",
			"run": "",
			"archive": "bin/canopy-archive"
		}
	}`
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}

	if err := os.Chdir(tmpRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cmd := newRunCmd()
	cmd.SetArgs([]string{})
	cmd.SetContext(context.Background())
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		// Either validate refused (also acceptable — empty Run is a
		// load-time concern in some validation policies) or our own
		// check fired.
		t.Fatal("expected error for empty scripts.run, got nil")
	}
	// Check the message points at scripts.run (whether the error came
	// from validate or our explicit check).
	if !strings.Contains(err.Error(), "run") {
		t.Errorf("error should mention 'run' field: %v", err)
	}
}

// TestRunRunCmd_validProjectChangesDir: a canopy.json with a real
// scripts.run path verifies that we'd actually try to exec it. We can't
// let syscall.Exec succeed (it'd replace the test binary with the run
// script — boom). Instead we set scripts.run to a non-existent path so
// syscall.Exec errors before exec'ing, but AFTER we've validated and
// chdir'd. Smoke check: error message contains the expected exec target.
func TestRunRunCmd_validButExecMissing(t *testing.T) {
	tmpRoot := t.TempDir()
	origCwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	conf := `{
		"scripts": {
			"setup": "bin/canopy-setup",
			"run": "bin/no-such-binary",
			"archive": "bin/canopy-archive"
		}
	}`
	if err := os.WriteFile(filepath.Join(tmpRoot, "canopy.json"), []byte(conf), 0o644); err != nil {
		t.Fatalf("write canopy.json: %v", err)
	}

	if err := os.Chdir(tmpRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cmd := newRunCmd()
	cmd.SetArgs([]string{})
	cmd.SetContext(context.Background())
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing run binary, got nil")
	}
	// Error chain should include the exec attempt at the resolved path.
	wantSubstr := "no-such-binary"
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error missing %q: %v", wantSubstr, err)
	}
}

// TestNewRunCmd_carriesAllowInTmuxAnnotation: canopy run needs to work
// inside the workspace's tmux session — that's the whole point of the
// shortcut. Without the annotation, the nested-tmux guard refuses.
func TestNewRunCmd_carriesAllowInTmuxAnnotation(t *testing.T) {
	cmd := newRunCmd()
	got, ok := cmd.Annotations[allowInTmuxAnnotation]
	if !ok {
		t.Fatalf("canopy run: missing allow-in-tmux annotation; nested-tmux guard will refuse")
	}
	if got != "true" {
		t.Errorf("canopy run: allow-in-tmux=%q; want \"true\"", got)
	}
}
