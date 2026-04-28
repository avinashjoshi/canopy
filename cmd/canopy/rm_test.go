package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupCleanWorktree creates a fresh git repo with one commit and a
// worktree on a feature branch. Returns the worktree path.
func setupCleanWorktree(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	source := t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=main", source},
		{"-C", source, "config", "user.email", "t@e"},
		{"-C", source, "config", "user.name", "t"},
		{"-C", source, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Forge origin/main + origin/HEAD so default-branch resolution works
	// without a real remote.
	if out, err := exec.Command("git", "-C", source, "update-ref",
		"refs/remotes/origin/main", "main").CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", source, "symbolic-ref",
		"refs/remotes/origin/HEAD", "refs/remotes/origin/main").CombinedOutput(); err != nil {
		t.Fatalf("symbolic-ref: %v\n%s", err, out)
	}

	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", source, "worktree", "add",
		"-b", branch, wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	return wt
}

// TestSafetyPreflight_Clean: a freshly-created worktree with no
// uncommitted changes and no commits past origin/main → no hangs.
func TestSafetyPreflight_Clean(t *testing.T) {
	wt := setupCleanWorktree(t, "ancient-hornet")

	hangs := safetyPreflight(context.Background(), wt, "ancient-hornet")
	if len(hangs) != 0 {
		t.Errorf("clean worktree should have no hangs; got %v", hangs)
	}
}

// TestSafetyPreflight_Uncommitted: an uncommitted file in the worktree
// triggers the uncommitted hang.
func TestSafetyPreflight_Uncommitted(t *testing.T) {
	wt := setupCleanWorktree(t, "ancient-hornet")
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hangs := safetyPreflight(context.Background(), wt, "ancient-hornet")
	found := false
	for _, h := range hangs {
		if strings.Contains(h, "uncommitted") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected uncommitted hang; got %v", hangs)
	}
}

// TestSafetyPreflight_UnpushedCommits: commits past origin/main with
// no upstream tracked → unpushed hang.
func TestSafetyPreflight_UnpushedCommits(t *testing.T) {
	wt := setupCleanWorktree(t, "feature")
	// Commit on the feature branch — diverges from origin/main.
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "feature").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}

	hangs := safetyPreflight(context.Background(), wt, "feature")
	found := false
	for _, h := range hangs {
		if strings.Contains(h, "unpushed") || strings.Contains(h, "no upstream") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected unpushed hang; got %v", hangs)
	}
}

// TestSafetyPreflight_OrphanWorktree: worktree dir gone → safety
// pre-flight returns no hangs (degrades gracefully — never blocks rm).
//
// IRON RULE: regression test for the v0.5 "rm always proceeds" behavior
// when the worktree has been hand-deleted. The new safety checks must
// not regress this.
func TestSafetyPreflight_OrphanWorktree(t *testing.T) {
	hangs := safetyPreflight(context.Background(), "/nonexistent/worktree/path", "feature")
	if len(hangs) != 0 {
		t.Errorf("orphan worktree must not block rm; got %v", hangs)
	}
}

// TestSafetyPreflight_EmptyPath: empty worktree path (defensive) returns
// no hangs.
func TestSafetyPreflight_EmptyPath(t *testing.T) {
	hangs := safetyPreflight(context.Background(), "", "feature")
	if len(hangs) != 0 {
		t.Errorf("empty path should be no-op; got %v", hangs)
	}
}

// TestExtractPRNumber_HappyPath: parse a typical gh JSON snippet.
func TestExtractPRNumber_HappyPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`{"state":"OPEN","number":42}`, "42"},
		{`{"number":42,"state":"OPEN"}`, "42"},
		{`{"number": 142}`, "142"},
		{``, ""},
		{`{"state":"OPEN"}`, ""},
	}
	for _, c := range cases {
		if got := extractPRNumber(c.in); got != c.want {
			t.Errorf("extractPRNumber(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
