package workspace_test

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// TestSyncBranch_NoOp covers the common path: branch hasn't changed since
// the last sync. Should return Changed=false with zero side effects.
func TestSyncBranch_NoOp(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "noop-ws", workspace.CreateOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Branch matches what's recorded — sync should be a no-op.
	res, err := mgr.SyncBranch(context.Background(), ws.Name)
	if err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}
	if res.Changed {
		t.Errorf("Changed = true on no-op; want false. Result: %+v", res)
	}
}

// TestSyncBranch_RenamePropagates covers the load-bearing path: agent
// runs `git branch -m`, statusline calls SyncBranch, both state.json and
// the live tmux session pick up the new name. Asserts every observable.
func TestSyncBranch_RenamePropagates(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "rename-ws", workspace.CreateOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	originalSession := ws.TmuxSessionName()

	// Agent / user does the rename.
	out, err := exec.Command("git", "-C", ws.Path, "branch", "-m", "feat-cool-stuff").CombinedOutput()
	if err != nil {
		t.Fatalf("git branch -m: %v\n%s", err, out)
	}

	res, err := mgr.SyncBranch(context.Background(), ws.Name)
	if err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}
	if !res.Changed {
		t.Fatalf("Changed = false; want true. Result: %+v", res)
	}
	if res.NewBranch != "feat-cool-stuff" {
		t.Errorf("NewBranch = %q; want feat-cool-stuff", res.NewBranch)
	}

	// state.json reflects the new branch + new session name.
	got, err := mgr.Find(context.Background(), ws.Name)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Branch != "feat-cool-stuff" {
		t.Errorf("state Branch = %q; want feat-cool-stuff", got.Branch)
	}
	if got.TmuxSessionName() == originalSession {
		t.Errorf("state TmuxSession unchanged after rename: still %q", got.TmuxSessionName())
	}

	// Tmux server agrees: new session alive, old session gone.
	if has, _ := mgr.Tmux.HasSession(context.Background(), got.TmuxSessionName()); !has {
		t.Errorf("new tmux session %q not found post-sync", got.TmuxSessionName())
	}
	if has, _ := mgr.Tmux.HasSession(context.Background(), originalSession); has {
		t.Errorf("old tmux session %q still alive post-sync", originalSession)
	}
}

// TestSyncBranch_DetachedHEAD: mid-rebase the worktree has detached HEAD
// (`git rev-parse --abbrev-ref HEAD` returns "HEAD"). Sync must NOT
// blank out the recorded branch — preserves last-known label so the
// statusline doesn't flicker to empty during a rebase.
func TestSyncBranch_DetachedHEAD(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var stdout, stderr bytes.Buffer
	ws, err := mgr.Create(context.Background(), "rebase-ws", workspace.CreateOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	originalBranch := ws.Branch

	// Detach HEAD by checking out the current commit directly.
	rev, err := exec.Command("git", "-C", ws.Path, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	out, err := exec.Command("git", "-C", ws.Path, "checkout", "--detach", string(bytes.TrimSpace(rev))).CombinedOutput()
	if err != nil {
		t.Fatalf("git checkout --detach: %v\n%s", err, out)
	}

	res, err := mgr.SyncBranch(context.Background(), ws.Name)
	if err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}
	if res.Changed {
		t.Errorf("Changed = true on detached HEAD; want false (must preserve label)")
	}

	got, err := mgr.Find(context.Background(), ws.Name)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Branch != originalBranch {
		t.Errorf("Branch overwritten during detached HEAD: got %q, want %q (preserved)", got.Branch, originalBranch)
	}
}

// TestSyncBranch_NotFound: nonexistent workspace returns a wrapped
// ErrWorkspaceNotFound so callers (e.g. statusline) can distinguish
// "not a canopy session" from genuine errors.
func TestSyncBranch_NotFound(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	_, err := mgr.SyncBranch(context.Background(), "no-such-ws")
	if !errors.Is(err, workspace.ErrWorkspaceNotFound) {
		t.Errorf("err = %v; want errors.Is(... ErrWorkspaceNotFound)", err)
	}
}

// TestSyncBranch_Collision: two workspaces with the same branch name
// after rename — the second SyncBranch should return ErrSessionNameInUse
// without writing state.json (preserving the stale value is preferable
// to a half-applied rename).
func TestSyncBranch_Collision(t *testing.T) {
	requireGitAndTmux(t)
	mgr, _ := fixture(t)

	var stdout, stderr bytes.Buffer
	wsA, err := mgr.Create(context.Background(), "ws-alpha", workspace.CreateOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	wsB, err := mgr.Create(context.Background(), "ws-beta", workspace.CreateOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}

	// Rename both to branch names that DIFFER in git (dot vs hyphen) but
	// COLLIDE under tmux.SafeName (both produce "<project>-shared-name").
	// Git allows dots in branch names; tmux strips them. The collision
	// only surfaces at the tmux layer.
	if out, err := exec.Command("git", "-C", wsA.Path, "branch", "-m", "shared-name").CombinedOutput(); err != nil {
		t.Fatalf("git branch -m A: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", wsB.Path, "branch", "-m", "shared.name").CombinedOutput(); err != nil {
		t.Fatalf("git branch -m B: %v\n%s", err, out)
	}
	if _, err := mgr.SyncBranch(context.Background(), wsA.Name); err != nil {
		t.Fatalf("SyncBranch A: %v", err)
	}
	_, err = mgr.SyncBranch(context.Background(), wsB.Name)
	if !errors.Is(err, tmux.ErrSessionNameInUse) {
		t.Fatalf("SyncBranch B: got %v; want ErrSessionNameInUse", err)
	}

	// wsB's state.json must be unchanged (no half-rename).
	got, err := mgr.Find(context.Background(), wsB.Name)
	if err != nil {
		t.Fatalf("Find B: %v", err)
	}
	if got.TmuxSessionName() != wsB.TmuxSessionName() {
		t.Errorf("wsB TmuxSession changed despite collision: was %q, now %q",
			wsB.TmuxSessionName(), got.TmuxSessionName())
	}
}
