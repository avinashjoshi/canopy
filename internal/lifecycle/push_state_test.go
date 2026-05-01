package lifecycle

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// setupWorkspaceWithUpstream extends setupWorkspace by registering a
// fake "origin" remote, forging refs/remotes/origin/<branchName> at
// the new branch's HEAD, and wiring branch.<name>.{remote,merge} so
// `git rev-parse @{upstream}` resolves cleanly. The remote config
// (URL + fetch refspec) is required for git to map "origin/<branch>"
// → refs/remotes/origin/<branch> when resolving @{upstream}; the
// forged ref alone is not enough.
//
// This is the test analog of "branch was pushed once and is currently
// in sync with origin."
func setupWorkspaceWithUpstream(t *testing.T, source, branchName string) string {
	t.Helper()
	wt := setupWorkspace(t, source, branchName)

	// Register a fake remote URL — never fetched/pushed against, but
	// its presence (plus the standard fetch refspec) is what teaches
	// `rev-parse @{upstream}` how to resolve "origin/<branch>".
	if out, err := exec.Command("git", "-C", source, "remote", "add",
		"origin", source).CombinedOutput(); err != nil {
		// Already added — git remote add is idempotent across multiple
		// test workspaces sharing one source repo. Ignore "already
		// exists" by string match; fail otherwise.
		if !strings.Contains(string(out), "already exists") {
			t.Fatalf("git remote add origin: %v\n%s", err, out)
		}
	}

	// Forge refs/remotes/origin/<branch> at the worktree's HEAD.
	headSha, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if out, err := exec.Command("git", "-C", source, "update-ref",
		"refs/remotes/origin/"+branchName,
		strings.TrimSpace(string(headSha))).CombinedOutput(); err != nil {
		t.Fatalf("update-ref origin/%s: %v\n%s", branchName, err, out)
	}

	// Wire upstream tracking config so @{upstream} resolves. We write
	// the config directly rather than via `git branch --set-upstream-to`
	// because the latter requires the upstream to exist as a known
	// branch, and our forged refs/remotes/origin/<branch> doesn't show
	// up in `git branch -r` reliably across git versions.
	for _, kv := range [][]string{
		{"branch." + branchName + ".remote", "origin"},
		{"branch." + branchName + ".merge", "refs/heads/" + branchName},
	} {
		if out, err := exec.Command("git", "-C", wt, "config",
			kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s=%s: %v\n%s", kv[0], kv[1], err, out)
		}
	}
	return wt
}

// TestDetectPushState_Synced: branch is in sync with origin/<branch>,
// no push needed → nil. The clean default state for any pushed branch.
func TestDetectPushState_Synced(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspaceWithUpstream(t, source, "synced-feature")

	ws := makeWorkspace("synced-feature", "synced-feature", wt, source)
	got := detectPushState(context.Background(), ws)
	if got != nil {
		t.Errorf("synced workspace should not fire push_state; got %+v", got)
	}
}

// TestDetectPushState_Unpushed: branch has commits past its upstream →
// ⇡N badge with the count of unpushed commits.
func TestDetectPushState_Unpushed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspaceWithUpstream(t, source, "unpushed-feature")

	// Make two local commits past the upstream (which sits at the
	// branch's create-time SHA per setupWorkspaceWithUpstream).
	for i := 0; i < 2; i++ {
		if out, err := exec.Command("git", "-C", wt,
			"commit", "--allow-empty", "-m", "wip").CombinedOutput(); err != nil {
			t.Fatalf("wip commit: %v\n%s", err, out)
		}
	}

	ws := makeWorkspace("unpushed-feature", "unpushed-feature", wt, source)
	got := detectPushState(context.Background(), ws)
	if got == nil {
		t.Fatal("expected push_state hint for 2 unpushed commits; got nil")
	}
	if got.Kind != "push_state" {
		t.Errorf("Kind = %q; want push_state", got.Kind)
	}
	if got.Message != "⇡2" {
		t.Errorf("Message = %q; want ⇡2", got.Message)
	}
	if !strings.Contains(got.Action, "git push") {
		t.Errorf("Action missing 'git push': %q", got.Action)
	}
	if strings.Contains(got.Action, "force-with-lease") {
		t.Errorf("Action should be plain push for ahead-only, not force-with-lease: %q", got.Action)
	}
}

// TestDetectPushState_Diverged: local has commits the upstream doesn't,
// AND upstream has commits the local doesn't (e.g., after a local
// rebase that rewrote already-pushed history) → ⇅ badge with a
// force-push action.
func TestDetectPushState_Diverged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspaceWithUpstream(t, source, "diverged-feature")

	// Local: one commit past upstream.
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "local-only").CombinedOutput(); err != nil {
		t.Fatalf("local commit: %v\n%s", err, out)
	}

	// Upstream: one DIFFERENT commit past the original branch base.
	// Build it by committing on a temp branch in the source repo, then
	// pointing refs/remotes/origin/diverged-feature at that commit.
	if out, err := exec.Command("git", "-C", source,
		"branch", "tmp-upstream", "main").CombinedOutput(); err != nil {
		t.Fatalf("branch tmp-upstream: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", source, "checkout",
		"tmp-upstream").CombinedOutput(); err != nil {
		t.Fatalf("checkout tmp-upstream: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", source,
		"commit", "--allow-empty", "-m", "upstream-only").CombinedOutput(); err != nil {
		t.Fatalf("upstream commit: %v\n%s", err, out)
	}
	upstreamSha, err := exec.Command("git", "-C", source,
		"rev-parse", "tmp-upstream").Output()
	if err != nil {
		t.Fatalf("rev-parse tmp-upstream: %v", err)
	}
	if out, err := exec.Command("git", "-C", source, "update-ref",
		"refs/remotes/origin/diverged-feature",
		strings.TrimSpace(string(upstreamSha))).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}

	ws := makeWorkspace("diverged-feature", "diverged-feature", wt, source)
	got := detectPushState(context.Background(), ws)
	if got == nil {
		t.Fatal("expected push_state hint for divergence; got nil")
	}
	if got.Kind != "push_state" {
		t.Errorf("Kind = %q; want push_state", got.Kind)
	}
	if got.Message != "⇅" {
		t.Errorf("Message = %q; want ⇅", got.Message)
	}
	if !strings.Contains(got.Action, "force-with-lease") {
		t.Errorf("Action missing 'force-with-lease' for diverged state: %q", got.Action)
	}
}

// TestDetectPushState_BehindOnly: local is behind upstream but has no
// commits ahead. This is "needs git pull" — pull-state, not push-
// state — so we deliberately return nil and let other surfaces (or a
// future pull_state detector) own it.
func TestDetectPushState_BehindOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspaceWithUpstream(t, source, "behind-feature")

	// Advance origin/behind-feature by one commit on a temp branch
	// while leaving the local worktree's HEAD where it was.
	if out, err := exec.Command("git", "-C", source,
		"branch", "tmp-up", "main").CombinedOutput(); err != nil {
		t.Fatalf("branch: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", source, "checkout",
		"tmp-up").CombinedOutput(); err != nil {
		t.Fatalf("checkout: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", source,
		"commit", "--allow-empty", "-m", "upstream-advance").CombinedOutput(); err != nil {
		t.Fatalf("upstream advance: %v\n%s", err, out)
	}
	upstreamSha, _ := exec.Command("git", "-C", source, "rev-parse", "tmp-up").Output()
	if out, err := exec.Command("git", "-C", source, "update-ref",
		"refs/remotes/origin/behind-feature",
		strings.TrimSpace(string(upstreamSha))).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}

	ws := makeWorkspace("behind-feature", "behind-feature", wt, source)
	got := detectPushState(context.Background(), ws)
	if got != nil {
		t.Errorf("behind-only is pull-state, not push-state; want nil, got %+v", got)
	}
}

// TestDetectPushState_NoUpstream: brand-new branch that's never been
// pushed (no branch.<name>.{remote,merge} configured). With commits
// past the default branch, surfaces ⇡N with a `git push -u origin
// <branch>` action that wires upstream on first push.
func TestDetectPushState_NoUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	// setupWorkspace seeds origin/main but does NOT seed
	// origin/<branch> or wire upstream tracking — exactly the
	// "never pushed" shape we want to test.
	wt := setupWorkspace(t, source, "fresh-branch")

	// Two commits past the default branch.
	for i := 0; i < 2; i++ {
		if out, err := exec.Command("git", "-C", wt,
			"commit", "--allow-empty", "-m", "wip").CombinedOutput(); err != nil {
			t.Fatalf("wip commit: %v\n%s", err, out)
		}
	}

	ws := makeWorkspace("fresh-branch", "fresh-branch", wt, source)
	got := detectPushState(context.Background(), ws)
	if got == nil {
		t.Fatal("expected push_state hint for never-pushed branch with commits; got nil")
	}
	if got.Message != "⇡2" {
		t.Errorf("Message = %q; want ⇡2", got.Message)
	}
	if !strings.Contains(got.Action, "git push -u origin fresh-branch") {
		t.Errorf("Action should wire upstream on first push: %q", got.Action)
	}
}

// TestDetectPushState_NoUpstream_FreshWorkspace: brand-new branch
// with no commits past the default branch (the moment after `canopy
// new` finishes setup). Nothing to push yet → nil. Keeps the badge
// column clean for fresh rows.
func TestDetectPushState_NoUpstream_FreshWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "brand-new")

	ws := makeWorkspace("brand-new", "brand-new", wt, source)
	got := detectPushState(context.Background(), ws)
	if got != nil {
		t.Errorf("fresh workspace should not fire push_state; got %+v", got)
	}
}

// TestDetectPushState_DetachedHead: detached HEAD has no branch to
// reason about an upstream for. stuck_state owns the detached signal;
// push_state stays quiet → nil.
func TestDetectPushState_DetachedHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "before-detach")

	// Make a commit so HEAD has somewhere to detach to.
	if out, err := exec.Command("git", "-C", wt,
		"commit", "--allow-empty", "-m", "wip").CombinedOutput(); err != nil {
		t.Fatalf("wip commit: %v\n%s", err, out)
	}
	headSha, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	// Detach: checkout the SHA directly.
	if out, err := exec.Command("git", "-C", wt, "checkout",
		"--detach", strings.TrimSpace(string(headSha))).CombinedOutput(); err != nil {
		t.Fatalf("checkout --detach: %v\n%s", err, out)
	}

	ws := makeWorkspace("before-detach", "before-detach", wt, source)
	got := detectPushState(context.Background(), ws)
	if got != nil {
		t.Errorf("detached HEAD should not fire push_state; got %+v", got)
	}
}

// TestDetectPushState_EmptyPath: defensive — workspace with no Path
// returns nil without panicking. Mirrors the early-return contract
// every other detector follows.
func TestDetectPushState_EmptyPath(t *testing.T) {
	ws := makeWorkspace("ghost", "ghost", "", "")
	got := detectPushState(context.Background(), ws)
	if got != nil {
		t.Errorf("empty path should return nil; got %+v", got)
	}
}
