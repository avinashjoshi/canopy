package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadAskQuestion_ExactlyOneMode covers the input-mode contract:
// positional, --file, or --stdin — exactly one. Zero modes errors;
// two or more modes errors. The "exactly one" guard prevents silent
// intent loss when a user accidentally combines flags.
func TestLoadAskQuestion_ExactlyOneMode(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "q.md")
	if err := os.WriteFile(file, []byte("from a file"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cases := []struct {
		name       string
		positional []string
		file       string
		stdin      bool
		stdinBody  string
		wantQ      string
		wantErr    bool
	}{
		{name: "no modes errors", wantErr: true},
		{name: "positional alone", positional: []string{"hello", "world"}, wantQ: "hello world"},
		{name: "file alone", file: file, wantQ: "from a file"},
		{name: "stdin alone", stdin: true, stdinBody: "from stdin", wantQ: "from stdin"},
		{name: "positional+file errors", positional: []string{"a"}, file: file, wantErr: true},
		{name: "positional+stdin errors", positional: []string{"a"}, stdin: true, wantErr: true},
		{name: "file+stdin errors", file: file, stdin: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadAskQuestion(tc.positional, tc.file, tc.stdin, strings.NewReader(tc.stdinBody))
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error; got nil (q=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantQ {
				t.Errorf("got %q; want %q", got, tc.wantQ)
			}
		})
	}
}

// TestSweepAskTempFiles_DeletesOldKeepsRecent: the startup sweep must
// remove stale leak files (>1h) while leaving recent ones alone. Pins
// the contract used by `cmd/canopy/main.go init()` and the popup's
// defer cleanup story.
func TestSweepAskTempFiles_DeletesOldKeepsRecent(t *testing.T) {
	// Stash $HOME so the sweep operates on a test tmpdir, not the
	// real ~/.canopy.
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpDir := filepath.Join(home, ".canopy", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatalf("mkdir tmpDir: %v", err)
	}

	old := filepath.Join(tmpDir, "ask-old.md")
	recent := filepath.Join(tmpDir, "ask-recent.md")
	other := filepath.Join(tmpDir, "agent-briefing-xyz.md") // not ask-*
	for _, p := range []string{old, recent, other} {
		if err := os.WriteFile(p, []byte("body"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	// Backdate old to 2h ago.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}

	sweepAskTempFiles()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old file still exists (err=%v); want removed", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent file got removed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("non-ask file got removed: %v (sweep should only touch ask-*.md)", err)
	}
}

// TestBranchHint_NonGitDirReturnsEmpty: branchHint should not surface
// git errors — it's a soft-fall that yields "" when git can't read
// the branch (detached HEAD, no git, not a repo).
func TestBranchHint_NonGitDirReturnsEmpty(t *testing.T) {
	tmp := t.TempDir() // bare temp dir, no .git
	got := branchHint(tmp)
	if got != "" {
		t.Errorf("branchHint(non-git dir) = %q; want \"\"", got)
	}
}
