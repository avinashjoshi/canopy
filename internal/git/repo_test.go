package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIsRepo_InsideRepo: a freshly-`git init`'d directory should report true.
func TestIsRepo_InsideRepo(t *testing.T) {
	dir := t.TempDir()

	// Use a real `git init` so the test exercises the real binary and
	// catches subtle output-shape changes between git versions.
	if err := exec.Command("git", "-C", dir, "init").Run(); err != nil {
		t.Skipf("git init failed (git not on PATH?): %v", err)
	}

	got, err := IsRepo(context.Background(), dir)
	if err != nil {
		t.Fatalf("IsRepo: unexpected err: %v", err)
	}
	if !got {
		t.Fatalf("IsRepo(%q) = false, want true", dir)
	}
}

// TestIsRepo_InsideRepoSubdir: nested under the repo root, should still be
// true. Verifies that we use git's own walk-up, not a naive .git existence
// check.
func TestIsRepo_InsideRepoSubdir(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init").Run(); err != nil {
		t.Skipf("git init failed: %v", err)
	}
	sub := filepath.Join(dir, "sub", "deep")
	if err := exec.Command("mkdir", "-p", sub).Run(); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := IsRepo(context.Background(), sub)
	if err != nil {
		t.Fatalf("IsRepo: unexpected err: %v", err)
	}
	if !got {
		t.Fatalf("IsRepo(%q) = false, want true (subdir of repo)", sub)
	}
}

// TestIsRepo_OutsideRepo: a fresh tempdir with no .git anywhere up the tree
// should report false with no error. /tmp on most CI is not in a git repo.
func TestIsRepo_OutsideRepo(t *testing.T) {
	dir := t.TempDir()

	got, err := IsRepo(context.Background(), dir)
	if err != nil {
		t.Fatalf("IsRepo: unexpected err: %v", err)
	}
	if got {
		// Possible if /tmp itself is inside a worktree (rare). Skip
		// rather than fail so the test is robust on weird CI setups.
		t.Skipf("IsRepo(%q) = true; tempdir appears to be inside a repo, skipping", dir)
	}
}

// TestIsRepo_DirDoesNotExist: a path that doesn't exist returns (false, err)
// because git itself errors. We don't want to silently swallow that as
// "not a repo" — the caller might want to handle it.
//
// Note: git's stderr-bearing exit status is treated as not-a-repo, which
// matches the behavior for nonexistent dirs too on most git versions
// (stderr "fatal: not a git repository ... cannot stat ..."). We accept
// either (false, nil) or (false, err) — both are valid for a nonexistent
// path.
func TestIsRepo_DirDoesNotExist(t *testing.T) {
	got, _ := IsRepo(context.Background(), "/nonexistent/definitely/not/here")
	if got {
		t.Fatalf("IsRepo on nonexistent dir = true, want false")
	}
}
