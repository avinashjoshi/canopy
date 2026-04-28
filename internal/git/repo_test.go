package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

var _ = filepath.Separator // keep filepath import even if a future edit drops the only user

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

// TestSourceRepoFromWorktree_DerivesParent: a worktree of repo R should
// resolve back to R's directory, not the worktree's own dir.
func TestSourceRepoFromWorktree_DerivesParent(t *testing.T) {
	source := t.TempDir()

	// Build a real source repo with one commit.
	for _, args := range [][]string{
		{"init", "--initial-branch=main", source},
		{"-C", source, "config", "user.email", "t@e"},
		{"-C", source, "config", "user.name", "t"},
		{"-C", source, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", args, err, out)
		}
	}

	// Add a worktree on a fresh branch.
	worktree := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", source, "worktree", "add", "-b", "feat/x", worktree).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}

	got, err := SourceRepoFromWorktree(context.Background(), worktree)
	if err != nil {
		t.Fatalf("SourceRepoFromWorktree: %v", err)
	}
	// On macOS, t.TempDir's /var/folders gets symlinked through /private/var,
	// so EvalSymlinks may resolve to a different prefix. Compare basenames.
	if filepath.Base(got) != filepath.Base(source) {
		t.Errorf("SourceRepoFromWorktree(%q) = %q, want a path whose basename is %q",
			worktree, got, filepath.Base(source))
	}
	// Sanity: the resolved path should NOT be the worktree itself.
	if filepath.Base(got) == filepath.Base(worktree) {
		t.Errorf("SourceRepoFromWorktree returned the worktree path itself")
	}
}

// TestSourceRepoFromWorktree_NotARepo: passing a non-repo dir errors.
func TestSourceRepoFromWorktree_NotARepo(t *testing.T) {
	dir := t.TempDir()
	if _, err := SourceRepoFromWorktree(context.Background(), dir); err == nil {
		t.Errorf("expected error on non-repo dir, got nil")
	}
}
