package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/avinashjoshi/canopy/internal/state"
)

// detectStuckState returns a Hint when the workspace's git repo is in a
// transient mid-operation state — mid-rebase, mid-merge, mid-cherry-pick,
// or detached HEAD. These are easy to forget when juggling multiple
// canopy workspaces in parallel; the row badge keeps the surprise from
// landing days later when the user context-switches back.
//
// The state is derived from marker files/dirs that git writes into the
// per-worktree gitdir while an operation is in progress:
//
//	rebase-merge/ or rebase-apply/   → mid-rebase
//	MERGE_HEAD                       → mid-merge
//	CHERRY_PICK_HEAD                 → mid-cherry-pick
//	HEAD points to a SHA, not a ref  → detached HEAD
//
// Critical detail: in a canopy worktree, <ws.Path>/.git is a *file* (a
// `gitdir:` pointer), not a directory. The marker files live under
// <main-repo>/.git/worktrees/<name>/, NOT under <ws.Path>/.git/. We
// resolve the per-worktree gitdir via `git rev-parse --git-dir` rather
// than poking at <ws.Path>/.git directly.
//
// Returns nil when:
//   - workspace path empty
//   - gitdir resolution fails (not a git repo, command unavailable)
//   - none of the four stuck states apply
//
// Cost: a few stat calls plus one `git rev-parse` invocation per
// workspace per refresh. Well under 10ms; runs inside RunFast's
// goroutine pool.
func detectStuckState(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" {
		return nil
	}
	gitDir := gitResolveGitDir(ctx, ws.Path)
	if gitDir == "" {
		return nil
	}

	// Order matters — first match wins. A workspace can technically be
	// both detached AND mid-rebase (interactive rebase parks HEAD on the
	// commit being edited), but the rebase signal is the more actionable
	// one to surface. Same logic applies to merge / cherry-pick: the
	// in-progress operation is what the user needs to finish first.
	if dirExists(filepath.Join(gitDir, "rebase-merge")) ||
		dirExists(filepath.Join(gitDir, "rebase-apply")) {
		return &state.Hint{
			Kind:       "stuck_state",
			Message:    "⚠ rebasing",
			Action:     "git rebase --continue",
			DetectedAt: time.Now(),
		}
	}
	if fileExists(filepath.Join(gitDir, "MERGE_HEAD")) {
		return &state.Hint{
			Kind:       "stuck_state",
			Message:    "⚠ merging",
			Action:     "git merge --continue",
			DetectedAt: time.Now(),
		}
	}
	if fileExists(filepath.Join(gitDir, "CHERRY_PICK_HEAD")) {
		return &state.Hint{
			Kind:       "stuck_state",
			Message:    "⚠ pick",
			Action:     "git cherry-pick --continue",
			DetectedAt: time.Now(),
		}
	}
	if gitHeadDetached(ctx, ws.Path) {
		// We don't suggest a specific branch because we don't know which
		// one the user meant to be on — surface the shape of the fix and
		// let the user/agent fill in the name.
		return &state.Hint{
			Kind:       "stuck_state",
			Message:    "⚠ detached",
			Action:     "git switch <branch>",
			DetectedAt: time.Now(),
		}
	}
	return nil
}

// gitResolveGitDir returns the per-worktree gitdir's absolute path by
// asking git itself. In a canopy worktree, this is
// <source>/.git/worktrees/<name>, not <ws.Path>/.git (which is a file).
// Empty string on any failure (not a repo, git missing, ctx canceled).
func gitResolveGitDir(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--git-dir")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	gitDir := strings.TrimSpace(string(out))
	if gitDir == "" {
		return ""
	}
	// `--git-dir` may return a path relative to ws.Path; resolve so
	// callers can stat absolute paths regardless.
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	return gitDir
}

// gitHeadDetached reports whether HEAD is pointing at a raw commit
// rather than a branch ref. `git rev-parse --symbolic-full-name HEAD`
// returns "HEAD" when detached; otherwise it returns "refs/heads/<name>".
// Errors map to false so a transient git failure doesn't fire a noisy
// false-positive badge.
func gitHeadDetached(ctx context.Context, path string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", path,
		"rev-parse", "--symbolic-full-name", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "HEAD"
}

// dirExists is a small stat helper that returns true only when the path
// names an existing directory. Used for rebase-merge/ and rebase-apply/
// which git creates as directories, not files.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// fileExists is the symmetric helper for MERGE_HEAD / CHERRY_PICK_HEAD,
// which git writes as plain files inside the gitdir.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
