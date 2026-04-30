package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupCleanWorktree creates a fresh git repo with one commit and a
// worktree on a feature branch. Returns the worktree path. Mirrors
// the helper in lifecycle's tests; kept package-local because both
// packages need their own (workspace doesn't depend on lifecycle).
func setupCleanWorktreeForSafety(t *testing.T, branch string) string {
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

// TestSafetyChecks_Clean: a freshly-created worktree with no
// uncommitted changes and no commits past origin/main → no hangs.
func TestSafetyChecks_Clean(t *testing.T) {
	wt := setupCleanWorktreeForSafety(t, "ancient-hornet")

	hangs := safetyChecks(context.Background(), wt, "ancient-hornet")
	if len(hangs) != 0 {
		t.Errorf("clean worktree should have no hangs; got %v", hangs)
	}
}

// TestSafetyChecks_Uncommitted: an uncommitted file in the worktree
// triggers the uncommitted hang.
func TestSafetyChecks_Uncommitted(t *testing.T) {
	wt := setupCleanWorktreeForSafety(t, "ancient-hornet")
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hangs := safetyChecks(context.Background(), wt, "ancient-hornet")
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

// TestSafetyChecks_UnpushedCommits: commits past origin/main with no
// upstream tracked → unpushed hang.
func TestSafetyChecks_UnpushedCommits(t *testing.T) {
	wt := setupCleanWorktreeForSafety(t, "feature")
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "feature").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}

	hangs := safetyChecks(context.Background(), wt, "feature")
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

// TestSafetyChecks_UpstreamDeleted_NoUnpushedHang: simulates the
// post-merge auto-delete-branch flow. The local feature branch was
// pushed (has upstream config + a remote-tracking ref), then the
// remote-tracking ref disappears (e.g. after `git fetch --prune`
// following GitHub's auto-delete on PR merge). safetyChecks should
// NOT flag the unpushed commits as hanging work — they were pushed,
// landed, and the branch was retired.
func TestSafetyChecks_UpstreamDeleted_NoUnpushedHang(t *testing.T) {
	wt := setupCleanWorktreeForSafety(t, "feature")

	// Make a commit on the feature branch.
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "feature work").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}

	// Wire upstream tracking: configure remote.origin.fetch (so git
	// can map refs/heads/* on origin to refs/remotes/origin/*),
	// branch.feature.{remote,merge}, and create an actual
	// remote-tracking ref. Then delete the remote-tracking ref to
	// simulate GitHub's post-merge auto-delete + a local prune.
	for _, args := range [][]string{
		{"-C", wt, "config", "remote.origin.url", "."},
		{"-C", wt, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
		{"-C", wt, "config", "branch.feature.remote", "origin"},
		{"-C", wt, "config", "branch.feature.merge", "refs/heads/feature"},
		{"-C", wt, "update-ref", "refs/remotes/origin/feature", "HEAD"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Sanity-check upstream resolves.
	if out, err := exec.Command("git", "-C", wt,
		"rev-parse", "--abbrev-ref", "@{u}").CombinedOutput(); err != nil {
		t.Fatalf("upstream not configured: %v\n%s", err, out)
	}
	// Now delete the remote-tracking ref → upstream config still
	// points at origin/feature, but the ref itself is gone.
	if out, err := exec.Command("git", "-C", wt,
		"update-ref", "-d", "refs/remotes/origin/feature").CombinedOutput(); err != nil {
		t.Fatalf("delete ref: %v\n%s", err, out)
	}

	hangs := safetyChecks(context.Background(), wt, "feature")
	for _, h := range hangs {
		if strings.Contains(h, "unpushed") || strings.Contains(h, "no upstream") {
			t.Errorf("unpushed/no-upstream hang must NOT fire after remote branch was auto-deleted; got %v", hangs)
		}
	}
}

// TestWasUpstreamDeleted: helper-level coverage for the three cases
// (no upstream configured, upstream alive, upstream deleted).
func TestWasUpstreamDeleted(t *testing.T) {
	// Case 1: no upstream configured → false.
	wt := setupCleanWorktreeForSafety(t, "no-upstream")
	if wasUpstreamDeleted(context.Background(), wt) {
		t.Errorf("branch with no upstream configured should report false")
	}

	// Case 2: upstream configured AND alive → false.
	wt2 := setupCleanWorktreeForSafety(t, "alive")
	for _, args := range [][]string{
		{"-C", wt2, "config", "remote.origin.url", "."},
		{"-C", wt2, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
		{"-C", wt2, "config", "branch.alive.remote", "origin"},
		{"-C", wt2, "config", "branch.alive.merge", "refs/heads/alive"},
		{"-C", wt2, "update-ref", "refs/remotes/origin/alive", "HEAD"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if wasUpstreamDeleted(context.Background(), wt2) {
		t.Errorf("branch with live upstream should report false")
	}

	// Case 3: upstream configured but ref gone → true.
	if out, err := exec.Command("git", "-C", wt2,
		"update-ref", "-d", "refs/remotes/origin/alive").CombinedOutput(); err != nil {
		t.Fatalf("delete ref: %v\n%s", err, out)
	}
	if !wasUpstreamDeleted(context.Background(), wt2) {
		t.Errorf("branch with deleted upstream ref should report true")
	}
}

// TestSafetyChecks_OrphanWorktree: worktree dir gone → safetyChecks
// returns no hangs (degrades gracefully — never blocks rm).
//
// IRON RULE: regression test for the v0.5 "rm always proceeds" behavior
// when the worktree has been hand-deleted. The new safety checks must
// not regress this.
func TestSafetyChecks_OrphanWorktree(t *testing.T) {
	hangs := safetyChecks(context.Background(), "/nonexistent/worktree/path", "feature")
	if len(hangs) != 0 {
		t.Errorf("orphan worktree must not block rm; got %v", hangs)
	}
}

// TestSafetyChecks_EmptyPath: empty worktree path (defensive) returns
// no hangs.
func TestSafetyChecks_EmptyPath(t *testing.T) {
	hangs := safetyChecks(context.Background(), "", "feature")
	if len(hangs) != 0 {
		t.Errorf("empty path should be no-op; got %v", hangs)
	}
}

// TestExtractPRNumber: parse typical gh JSON output.
func TestExtractPRNumber(t *testing.T) {
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
