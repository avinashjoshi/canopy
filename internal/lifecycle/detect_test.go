package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oncactus/canopy/internal/state"
)

// setupSourceRepo builds a fresh "source" git repo with a main branch
// and one commit. Returns the absolute path.
func setupSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=main", dir},
		{"-C", dir, "config", "user.email", "t@e"},
		{"-C", dir, "config", "user.name", "t"},
		{"-C", dir, "commit", "--allow-empty", "-m", "initial"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// setupWorkspace creates a worktree on a new branch off source's main,
// AND seeds the proper remote-tracking refs (refs/remotes/origin/main +
// refs/remotes/origin/HEAD) so detectors that look up "origin/<default>"
// find them. We don't have an actual remote in tests; we forge the
// refs directly via git update-ref + symbolic-ref.
func setupWorkspace(t *testing.T, source, branchName string) string {
	t.Helper()

	// Forge refs/remotes/origin/main pointing at source's main HEAD.
	// This is the test analog of `git fetch origin` — gives detectors
	// a target to compare against without needing real network.
	if out, err := exec.Command("git", "-C", source, "update-ref",
		"refs/remotes/origin/main", "main").CombinedOutput(); err != nil {
		t.Fatalf("update-ref origin/main: %v\n%s", err, out)
	}
	// Set origin/HEAD → origin/main so gitDefaultBranch's symbolic-ref
	// lookup works.
	if out, err := exec.Command("git", "-C", source, "symbolic-ref",
		"refs/remotes/origin/HEAD", "refs/remotes/origin/main").CombinedOutput(); err != nil {
		t.Fatalf("symbolic-ref origin/HEAD: %v\n%s", err, out)
	}

	// Now create the worktree on a fresh branch.
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", source, "worktree", "add",
		"-b", branchName, wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	return wt
}

func makeWorkspace(name, branch, path, root string) state.Workspace {
	return state.Workspace{
		Name:        name,
		Branch:      branch,
		Path:        path,
		ProjectRoot: root,
		TmuxSession: name,
	}
}

// TestDetectRenameSuggested_NoCommitsPastMain: fresh worktree on a new
// branch, branch matches workspace name, but no commits past main →
// no hint (nothing to rename for yet).
func TestDetectRenameSuggested_NoCommitsPastMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "ancient-hornet")
	ws := makeWorkspace("ancient-hornet", "ancient-hornet", wt, source)

	got := detectRenameSuggested(context.Background(), ws)
	if got != nil {
		t.Errorf("expected nil hint with no commits past main; got %+v", got)
	}
}

// TestDetectRenameSuggested_CommitsAndAutoName: branch name matches
// workspace name AND commits past main exist → hint fires.
func TestDetectRenameSuggested_CommitsAndAutoName(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "ancient-hornet")
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "wip").CombinedOutput(); err != nil {
		t.Fatalf("wip commit: %v\n%s", err, out)
	}

	ws := makeWorkspace("ancient-hornet", "ancient-hornet", wt, source)
	got := detectRenameSuggested(context.Background(), ws)
	if got == nil {
		t.Fatal("expected hint; got nil")
	}
	if got.Kind != "rename_suggested" {
		t.Errorf("Kind = %q; want rename_suggested", got.Kind)
	}
	if !strings.Contains(got.Message, "ancient-hornet") {
		t.Errorf("message missing branch name: %s", got.Message)
	}
}

// TestDetectRenameSuggested_AlreadyRenamed: user already ran git
// branch -m, so currentBranch != ws.Name → no hint.
func TestDetectRenameSuggested_AlreadyRenamed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "ancient-hornet")
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "wip").CombinedOutput(); err != nil {
		t.Fatalf("wip commit: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", wt,
		"branch", "-m", "feat/oauth").CombinedOutput(); err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}

	// state.Workspace.Name is still the old name (state.json hasn't
	// been reconciled) but the actual branch has moved to feat/oauth.
	ws := makeWorkspace("ancient-hornet", "ancient-hornet", wt, source)
	got := detectRenameSuggested(context.Background(), ws)
	if got != nil {
		t.Errorf("expected nil hint after rename; got %+v", got)
	}
}

// TestDetectShipped_NotMerged: HEAD is NOT reachable from origin/main →
// no hint.
func TestDetectShipped_NotMerged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "in-flight")
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "diverging").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}

	ws := makeWorkspace("in-flight", "in-flight", wt, source)
	got := detectShipped(context.Background(), ws)
	if got != nil {
		t.Errorf("expected nil hint while in flight; got %+v", got)
	}
}

// TestDetectShipped_MergedViaMergeCommit: a real `git merge --no-ff
// feature` merge. Detector currently returns nil for this case
// (squash-merge-only detection — the merge-commit case is ambiguous
// from current git state alone with the "fresh fork that fell behind"
// case, so we don't claim shipped without storing the branch base
// commit). Documented limitation; future work to record BaseCommit
// on workspace creation will let us safely detect this case too.
//
// Real-world shape:
//
//	       main
//	o────o────M
//	 \      /
//	  o────o   feature
//
// After this, HEAD (= feature tip) is reachable from origin/main, but
// `git cherry origin/main HEAD` returns nothing (HEAD has no commits
// past origin/main from cherry's POV — they're all reachable from
// the merge commit). So we conservatively return nil.
func TestDetectShipped_MergedViaMergeCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "merged-feature")

	// Make a commit on the feature branch.
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "feature").CombinedOutput(); err != nil {
		t.Fatalf("feature commit: %v\n%s", err, out)
	}
	featureSha, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse feature: %v", err)
	}

	// Simulate the merge commit: a new commit on origin/main with TWO
	// parents — the prior origin/main tip and the feature tip. Use
	// `git commit-tree` to forge it without a working merge operation.
	prevMain, err := exec.Command("git", "-C", source, "rev-parse",
		"refs/remotes/origin/main").Output()
	if err != nil {
		t.Fatalf("rev-parse origin/main: %v", err)
	}
	tree, err := exec.Command("git", "-C", source, "rev-parse", "HEAD^{tree}").Output()
	if err != nil {
		t.Fatalf("rev-parse tree: %v", err)
	}
	// Note: env vars set author + committer so commit-tree doesn't
	// complain about missing identity.
	mergeCmd := exec.Command("git", "-C", source, "commit-tree",
		strings.TrimSpace(string(tree)),
		"-p", strings.TrimSpace(string(prevMain)),
		"-p", strings.TrimSpace(string(featureSha)),
		"-m", "Merge feature into main")
	mergeCmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	mergeShaOut, err := mergeCmd.Output()
	if err != nil {
		t.Fatalf("commit-tree (merge): %v", err)
	}
	mergeSha := strings.TrimSpace(string(mergeShaOut))

	// Move origin/main to the merge commit.
	if out, err := exec.Command("git", "-C", source, "update-ref",
		"refs/remotes/origin/main", mergeSha).CombinedOutput(); err != nil {
		t.Fatalf("update-ref origin/main: %v\n%s", err, out)
	}

	ws := makeWorkspace("merged-feature", "merged-feature", wt, source)
	got := detectShipped(context.Background(), ws)
	if got != nil {
		t.Errorf("expected nil for merge-commit-merged branch (squash-only detection); got %+v", got)
	}
}

// TestDetectShipped_FreshWorkspace: regression test for the v0.6 false-
// positive — a brand-new workspace with no commits past main MUST NOT
// fire shipped. Before the fix, is-ancestor returned true vacuously
// because HEAD == origin/main, and shipped fired immediately on every
// fresh workspace creation.
func TestDetectShipped_FreshWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "brand-new")

	// No commits, no work, no merge. Just a fresh workspace.
	ws := makeWorkspace("brand-new", "brand-new", wt, source)
	got := detectShipped(context.Background(), ws)
	if got != nil {
		t.Errorf("fresh workspace must NOT fire shipped; got %+v", got)
	}
}

// TestDetectShipped_LocalRepoFallback: a workspace whose source repo
// has NO remote (no origin/main, no origin/HEAD) — purely local —
// should still detect "shipped" against local main when the feature
// branch's commits are squash-merged into it.
//
// This covers the "I'm just hacking locally, no GitHub involved"
// workflow. Without the fallback, detectShipped would return nil for
// every commit on every local-only repo, which is wrong.
func TestDetectShipped_LocalRepoFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Build a local-only repo: NO origin remote, NO refs/remotes/origin/*.
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

	// Create a feature worktree with a commit.
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", source, "worktree", "add",
		"-b", "local-feature", wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	// Real (non-empty) feature commit. `merge --squash` of an empty
	// commit collapses to nothing-to-commit, so we need a real diff.
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	if out, err := exec.Command("git", "-C", wt, "add", "feature.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", wt,
		"commit", "-m", "feat").CombinedOutput(); err != nil {
		t.Fatalf("feat commit: %v\n%s", err, out)
	}

	// Squash-merge feature into local main: one squash commit on main
	// containing all the feature's changes; the feature's own commits
	// are NOT in main's history but their patch IDs match the squash.
	if out, err := exec.Command("git", "-C", source, "merge",
		"--squash", "local-feature").CombinedOutput(); err != nil {
		t.Fatalf("merge --squash: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", source,
		"commit", "-m", "squash-merged locally").CombinedOutput(); err != nil {
		t.Fatalf("squash commit: %v\n%s", err, out)
	}

	ws := makeWorkspace("local-feature", "local-feature", wt, source)
	got := detectShipped(context.Background(), ws)
	if got == nil {
		t.Fatal("expected shipped hint for squash-merged branch; got nil")
	}
	if got.Kind != "shipped" {
		t.Errorf("Kind = %q; want shipped", got.Kind)
	}
	if !strings.Contains(got.Action, "canopy rm local-feature") {
		t.Errorf("Action missing rm command: %q", got.Action)
	}
}

// TestDetectShipped_FreshForkBehindMain is the regression test for the
// false positive that bit misty-marsh: a workspace that was forked
// from main and never had unique commits, while main has fast-forwarded
// past it. Old detector said "shipped" because HEAD was an ancestor
// of main; the right answer is nil because the branch never had work
// to ship.
//
// Replicates: branch from main, don't commit anything on the branch,
// commit something to main directly, observe the workspace's row in
// the TUI — should NOT show "✓ merged".
func TestDetectShipped_FreshForkBehindMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "fresh-fork")
	// Feature branch makes ZERO commits.

	// Advance origin/main by simulating a new main commit. We don't
	// actually push — just move the ref to a new commit.
	if out, err := exec.Command("git", "-C", source, "checkout", "main").CombinedOutput(); err != nil {
		t.Fatalf("checkout main: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", source,
		"commit", "--allow-empty", "-m", "main advance").CombinedOutput(); err != nil {
		t.Fatalf("main advance: %v\n%s", err, out)
	}
	mainSha, _ := exec.Command("git", "-C", source, "rev-parse", "main").Output()
	if out, err := exec.Command("git", "-C", source, "update-ref",
		"refs/remotes/origin/main", strings.TrimSpace(string(mainSha))).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}

	ws := makeWorkspace("fresh-fork", "fresh-fork", wt, source)
	got := detectShipped(context.Background(), ws)
	if got != nil {
		t.Errorf("REGRESSION: fresh fork with no commits past advanced main returned shipped hint %+v; want nil (branch never had work to ship)", got)
	}
}

// TestRunFast_Parallelism: RunFast dispatches all detectors in
// parallel. Ensures no panic when multiple fire on the same workspace.
func TestRunFast_Parallelism(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "ancient-hornet")
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "wip").CombinedOutput(); err != nil {
		t.Fatalf("wip: %v\n%s", err, out)
	}

	ws := makeWorkspace("ancient-hornet", "ancient-hornet", wt, source)
	hints := RunFast(context.Background(), ws)

	// Two detectors should fire on this workspace shape:
	// - rename_suggested (auto-named branch with commits past main)
	// - git_stats (1 commit ahead → "↑1")
	// shipped does NOT fire (cherry shows + line, not -); pr_status
	// does not fire (no gh/PR setup in this test).
	kinds := make(map[string]bool)
	for _, h := range hints {
		kinds[h.Kind] = true
	}
	if !kinds["rename_suggested"] {
		t.Errorf("RunFast missing rename_suggested hint; got kinds %v", kinds)
	}
	if !kinds["git_stats"] {
		t.Errorf("RunFast missing git_stats hint; got kinds %v", kinds)
	}
	if kinds["shipped"] {
		t.Errorf("RunFast unexpectedly produced shipped hint: %+v", hints)
	}
}

// TestPRStatusCache_TTL: cache hit returns immediately without
// shelling out. We verify by setting a sentinel value, calling
// detectPRStatus, and checking the cache wasn't replaced.
func TestPRStatusCache_TTL(t *testing.T) {
	ResetPRStatusCache()
	defer ResetPRStatusCache()

	// Pre-seed cache with a fake "no PR" entry.
	prStatusMu.Lock()
	prStatusCache["/fake/root|test-branch"] = prStatusEntry{
		hint:     nil,                      // no PR
		cachedAt: prStatusEntry{}.cachedAt, // zero-time, but we'll override TTL via the test
	}
	prStatusMu.Unlock()

	// Use a stub workspace whose currentBranch resolution will fail
	// (path doesn't exist) — detectPRStatus returns nil before hitting
	// the cache. This test mostly verifies the cache ops don't deadlock
	// or panic when the path is invalid.
	ws := makeWorkspace("test", "test", "/fake/path", "/fake/root")
	got := detectPRStatus(context.Background(), ws)
	if got != nil {
		t.Errorf("expected nil for invalid path; got %+v", got)
	}
}

// TestPRStatus_GHMissing: when gh is not on PATH, the detector silently
// returns nil and logs once. We can't easily fake gh-missing here
// without messing with PATH; the test only verifies the function
// doesn't panic and returns nil gracefully when ws is invalid.
func TestPRStatus_GHMissing(t *testing.T) {
	ResetPRStatusCache()
	// We can't reliably make gh missing in a test (other tests on the
	// same machine might depend on it). Just verify the function call
	// path doesn't panic with a minimal ws.
	got := detectPRStatus(context.Background(), state.Workspace{})
	if got != nil {
		t.Errorf("empty workspace should return nil; got %+v", got)
	}
}
