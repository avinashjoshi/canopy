package lifecycle

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/oncactus/canopy/internal/state"
)

// detectGitStats returns a Hint with the workspace's "what's in flight"
// numbers: how far ahead / behind the default branch the workspace is,
// and how many files have uncommitted changes.
//
// Format: `↑3 ↓1 *5`
//
//	↑N  commits on this branch that are NOT in the default branch yet
//	    (your unpushed work)
//	↓N  commits on the default branch that are NOT on this branch
//	    (you're behind; consider rebase/merge)
//	*N  files with uncommitted changes (modified + staged + untracked)
//
// Zero counts are omitted to keep clean workspaces visually quiet:
// a workspace with no work and clean tree shows no badge at all (this
// detector returns nil), while one that's purely behind shows just
// `↓1`. The signal is "how much divergence from main, in three
// dimensions."
//
// Returns nil when:
//   - all three counts are zero (nothing to surface)
//   - we can't resolve the default branch (rare; no main, no master, no remote HEAD)
//   - any git command fails (defer to the next refresh)
//
// Cost: three short git invocations per workspace per refresh.
// `rev-list --count` is essentially-free; `status --porcelain` walks
// the index but caps at the workspace's tree size. <30ms total on
// typical worktrees.
func detectGitStats(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" {
		return nil
	}
	defaultBranch := gitDefaultBranch(ctx, ws.Path)
	if defaultBranch == "" {
		return nil
	}

	// Prefer origin/<default> when present (matches "real" main); fall
	// back to local <default> for purely-local repos.
	target := "origin/" + defaultBranch
	if !gitRefExists(ctx, ws.Path, target) {
		target = defaultBranch
		if !gitRefExists(ctx, ws.Path, target) {
			return nil
		}
	}

	ahead := gitRevListCount(ctx, ws.Path, target, "HEAD")    // commits in HEAD not in target
	behind := gitRevListCount(ctx, ws.Path, "HEAD", target)   // commits in target not in HEAD
	dirty := gitDirtyFileCount(ctx, ws.Path)

	if ahead == 0 && behind == 0 && dirty == 0 {
		return nil
	}

	return &state.Hint{
		Kind:       "git_stats",
		Message:    formatGitStats(ahead, behind, dirty),
		DetectedAt: time.Now(),
	}
}

// formatGitStats renders the three counts with leading glyphs, hiding
// any zero counts. Keeps clean workspaces' badges short:
//
//	(3, 0, 0) → "↑3"
//	(3, 1, 5) → "↑3 ↓1 *5"
//	(0, 0, 5) → "*5"
//	(0, 0, 0) → caller returns nil instead
func formatGitStats(ahead, behind, dirty int) string {
	parts := make([]string, 0, 3)
	if ahead > 0 {
		parts = append(parts, fmt.Sprintf("↑%d", ahead))
	}
	if behind > 0 {
		parts = append(parts, fmt.Sprintf("↓%d", behind))
	}
	if dirty > 0 {
		parts = append(parts, fmt.Sprintf("*%d", dirty))
	}
	return strings.Join(parts, " ")
}

// gitRevListCount returns the number of commits reachable from `to` but
// not from `from`. Wraps `git rev-list --count from..to`. Errors map to
// 0 (silent) so the detector falls back to "no signal" rather than
// hard-failing on a transient git issue.
func gitRevListCount(ctx context.Context, path, from, to string) int {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-list",
		"--count", from+".."+to)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return count
}

// gitDirtyFileCount returns the count of files with uncommitted
// changes — staged + unstaged + untracked. Implemented as one
// `git status --porcelain` line count so we don't double-count files
// that are both staged AND unstaged (porcelain emits one line per
// path with two-character status code).
func gitDirtyFileCount(ctx context.Context, path string) int {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}
