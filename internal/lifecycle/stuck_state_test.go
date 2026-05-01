package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// resolveGitDir asks git for the per-worktree gitdir of `path`. Test
// helper: detector code uses gitResolveGitDir under the hood; tests
// shell out so a bug in the helper can't mask a bug in the detector.
func resolveGitDir(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", path, "rev-parse", "--git-dir").Output()
	if err != nil {
		t.Fatalf("rev-parse --git-dir: %v", err)
	}
	gd := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(path, gd)
	}
	return gd
}

// TestDetectStuckState_EmptyPath: ws.Path == "" short-circuits to nil.
// Mirrors the contract of the other detectors.
func TestDetectStuckState_EmptyPath(t *testing.T) {
	got := detectStuckState(context.Background(), makeWorkspace("x", "x", "", ""))
	if got != nil {
		t.Errorf("empty path = %+v; want nil", got)
	}
}

// TestDetectStuckState_None: a clean worktree on an attached branch
// with no rebase/merge/pick state in flight returns nil.
func TestDetectStuckState_None(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "clean")
	ws := makeWorkspace("clean", "clean", wt, source)

	if got := detectStuckState(context.Background(), ws); got != nil {
		t.Errorf("clean worktree = %+v; want nil", got)
	}
}

// TestDetectStuckState_Rebase: forging a rebase-merge directory in the
// per-worktree gitdir surfaces "⚠ rebasing". This is the shape git
// leaves behind during `git rebase -i` etc.
func TestDetectStuckState_Rebase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "rebasing")
	gitDir := resolveGitDir(t, wt)

	// Forge the rebase-merge dir. Git only checks for its existence, not
	// its contents, when reporting in-progress state.
	if err := os.MkdirAll(filepath.Join(gitDir, "rebase-merge"), 0o755); err != nil {
		t.Fatalf("mkdir rebase-merge: %v", err)
	}

	ws := makeWorkspace("rebasing", "rebasing", wt, source)
	got := detectStuckState(context.Background(), ws)
	if got == nil {
		t.Fatal("expected stuck_state hint; got nil")
	}
	if got.Kind != "stuck_state" {
		t.Errorf("Kind = %q; want stuck_state", got.Kind)
	}
	if got.Message != "⚠ rebasing" {
		t.Errorf("Message = %q; want ⚠ rebasing", got.Message)
	}
	if !strings.Contains(got.Action, "rebase --continue") {
		t.Errorf("Action %q missing 'rebase --continue'", got.Action)
	}
}

// TestDetectStuckState_RebaseApply: the alternate rebase-apply dir
// (used by `git rebase` without `-i` on older codepaths) also fires
// the rebasing badge. Both shapes should be treated identically.
func TestDetectStuckState_RebaseApply(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "rebase-apply")
	gitDir := resolveGitDir(t, wt)

	if err := os.MkdirAll(filepath.Join(gitDir, "rebase-apply"), 0o755); err != nil {
		t.Fatalf("mkdir rebase-apply: %v", err)
	}

	ws := makeWorkspace("rebase-apply", "rebase-apply", wt, source)
	got := detectStuckState(context.Background(), ws)
	if got == nil || got.Message != "⚠ rebasing" {
		t.Errorf("got %+v; want ⚠ rebasing", got)
	}
}

// TestDetectStuckState_Merge: a MERGE_HEAD file in the gitdir surfaces
// "⚠ merging". Git writes this when a `git merge` stops on conflicts;
// the file's presence (not contents) is the in-progress signal.
func TestDetectStuckState_Merge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "merging")
	gitDir := resolveGitDir(t, wt)

	if err := os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"),
		[]byte("deadbeef\n"), 0o644); err != nil {
		t.Fatalf("write MERGE_HEAD: %v", err)
	}

	ws := makeWorkspace("merging", "merging", wt, source)
	got := detectStuckState(context.Background(), ws)
	if got == nil {
		t.Fatal("expected stuck_state hint; got nil")
	}
	if got.Message != "⚠ merging" {
		t.Errorf("Message = %q; want ⚠ merging", got.Message)
	}
	if !strings.Contains(got.Action, "merge --continue") {
		t.Errorf("Action %q missing 'merge --continue'", got.Action)
	}
}

// TestDetectStuckState_CherryPick: a CHERRY_PICK_HEAD file surfaces the
// shorter "⚠ pick" badge — the column is tight and the verb is enough
// to communicate the state at a glance.
func TestDetectStuckState_CherryPick(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "picking")
	gitDir := resolveGitDir(t, wt)

	if err := os.WriteFile(filepath.Join(gitDir, "CHERRY_PICK_HEAD"),
		[]byte("deadbeef\n"), 0o644); err != nil {
		t.Fatalf("write CHERRY_PICK_HEAD: %v", err)
	}

	ws := makeWorkspace("picking", "picking", wt, source)
	got := detectStuckState(context.Background(), ws)
	if got == nil {
		t.Fatal("expected stuck_state hint; got nil")
	}
	if got.Message != "⚠ pick" {
		t.Errorf("Message = %q; want ⚠ pick", got.Message)
	}
	if !strings.Contains(got.Action, "cherry-pick --continue") {
		t.Errorf("Action %q missing 'cherry-pick --continue'", got.Action)
	}
}

// TestDetectStuckState_Detached: checking out a raw commit SHA puts the
// worktree in detached HEAD. The detector reports "⚠ detached" with a
// generic switch action (we don't know which branch the user meant).
func TestDetectStuckState_Detached(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "detached-ws")

	// Make a commit so we have a SHA to detach onto, then check it out
	// by SHA. `git checkout <sha>` is the conventional way to put a
	// worktree in detached state.
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "head").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	shaOut, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	sha := strings.TrimSpace(string(shaOut))
	if out, err := exec.Command("git", "-C", wt, "checkout", sha).CombinedOutput(); err != nil {
		t.Fatalf("checkout %s: %v\n%s", sha, err, out)
	}

	ws := makeWorkspace("detached-ws", "detached-ws", wt, source)
	got := detectStuckState(context.Background(), ws)
	if got == nil {
		t.Fatal("expected stuck_state hint; got nil")
	}
	if got.Message != "⚠ detached" {
		t.Errorf("Message = %q; want ⚠ detached", got.Message)
	}
	if !strings.Contains(got.Action, "git switch") {
		t.Errorf("Action %q missing 'git switch'", got.Action)
	}
}

// TestDetectStuckState_RebasePreemptsDetached: an interactive rebase
// parks HEAD detached AND drops a rebase-merge dir. The rebase signal
// is the more actionable one — surface it. Locks in the precedence
// order documented in the detector.
func TestDetectStuckState_RebasePreemptsDetached(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "rebase-detached")
	gitDir := resolveGitDir(t, wt)

	// Detach HEAD by checking out the initial commit's SHA.
	shaOut, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	sha := strings.TrimSpace(string(shaOut))
	if out, err := exec.Command("git", "-C", wt, "checkout", sha).CombinedOutput(); err != nil {
		t.Fatalf("checkout: %v\n%s", err, out)
	}
	// And forge a rebase-merge dir on top.
	if err := os.MkdirAll(filepath.Join(gitDir, "rebase-merge"), 0o755); err != nil {
		t.Fatalf("mkdir rebase-merge: %v", err)
	}

	ws := makeWorkspace("rebase-detached", "rebase-detached", wt, source)
	got := detectStuckState(context.Background(), ws)
	if got == nil || got.Message != "⚠ rebasing" {
		t.Errorf("got %+v; want ⚠ rebasing (must preempt detached)", got)
	}
}

// TestGitHeadDetached_AttachedReturnsFalse: locks in the helper's
// "no false positive on a normal branch" contract — without it, the
// detached badge would fire on every clean workspace.
func TestGitHeadDetached_AttachedReturnsFalse(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "attached")
	if got := gitHeadDetached(context.Background(), wt); got {
		t.Errorf("attached worktree gitHeadDetached = true; want false")
	}
}

// TestGitResolveGitDir_NotARepo: for a non-repo path, resolution must
// return empty so the detector early-exits instead of stat-ing
// nonexistent gitdir paths.
func TestGitResolveGitDir_NotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if got := gitResolveGitDir(context.Background(), dir); got != "" {
		t.Errorf("non-repo gitResolveGitDir = %q; want empty", got)
	}
}
