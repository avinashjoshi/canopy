package lifecycle

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/avinashjoshi/canopy/internal/state"
)

// detectMergeability returns a Hint when merging the default branch
// (origin/<default> preferred, local fallback) into HEAD would produce
// conflicts. Wraps `git merge-tree --write-tree --name-only` (git 2.38+),
// which simulates the merge inside the object store without touching the
// working tree or the index.
//
// Why this hint exists: with multiple parallel canopy workspaces, main
// moves under each branch constantly. The first time a user discovers a
// conflict is usually at /ship time, after they've already context-
// switched into the workspace expecting a clean merge. Surfacing the
// would-conflict state in the row turns the surprise into a
// before-you-start signal.
//
// Returns nil when:
//   - workspace path empty
//   - default branch can't be resolved
//   - both origin/<default> and local <default> refs are missing
//   - target is an ancestor of HEAD (no merge needed; can't conflict)
//   - the simulated merge is clean (exit 0)
//   - merge-tree is unavailable or fails (exit ≥2; e.g. git <2.38)
//
// Cost: ~30-80ms cold (one merge-tree call). The ancestor short-circuit
// drops common synced workspaces to ~10ms. Runs inside RunFast's
// goroutine pool so wall-clock stays parallel-bounded with the other
// detectors.
func detectMergeability(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" {
		return nil
	}
	defaultBranch := gitDefaultBranch(ctx, ws.Path)
	if defaultBranch == "" {
		return nil
	}
	target := "origin/" + defaultBranch
	if !gitRefExists(ctx, ws.Path, target) {
		target = defaultBranch
		if !gitRefExists(ctx, ws.Path, target) {
			return nil
		}
	}

	// Short-circuit: if target is fully merged into HEAD already, the
	// merge is a no-op and there's nothing to conflict over. Saves the
	// merge-tree call in the common synced case.
	if gitIsAncestor(ctx, ws.Path, target, "HEAD") {
		return nil
	}

	// `git merge-tree --write-tree --name-only HEAD <target>` simulates
	// the merge in the object store. Output line 1 is the merged tree's
	// OID; lines 2+ (only present when conflicted) are conflicting paths.
	// Exit codes:
	//   0   clean merge
	//   1   merge with conflicts
	//   2+  error (including "unknown flag" on git <2.38)
	cmd := exec.CommandContext(ctx, "git", "-C", ws.Path,
		"merge-tree", "--write-tree", "--name-only", "HEAD", target)
	out, err := cmd.Output()
	if err == nil {
		// Exit 0 — clean merge.
		return nil
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		// Non-exit error (context canceled, fork failed). Defer; we'll
		// retry on the next refresh.
		return nil
	}
	if ee.ExitCode() != 1 {
		// Anything other than "exit 1 = conflicts" is treated as
		// "merge-tree refused to answer." Includes git-too-old, repo
		// corruption, ref disappeared mid-call. Silent fallback.
		return nil
	}

	// Parse conflicting paths. First line is the merged tree's OID;
	// subsequent non-empty lines are paths. If for some reason there
	// are no path lines (shouldn't happen with exit 1 + --name-only),
	// be conservative and return nil rather than an unsupported "0
	// conflicts" badge.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	conflictCount := 0
	for i, line := range lines {
		if i == 0 {
			continue // tree OID
		}
		if strings.TrimSpace(line) != "" {
			conflictCount++
		}
	}
	if conflictCount == 0 {
		return nil
	}

	return &state.Hint{
		Kind:       "mergeability",
		Message:    formatMergeability(conflictCount),
		Action:     "git fetch origin && git rebase origin/" + defaultBranch,
		DetectedAt: time.Now(),
	}
}

// formatMergeability renders the conflict-count message.
//
//	1 conflict  -> "⚠ conflict"     (singular, tightest)
//	N conflicts -> "⚠ N conflicts"  (with count)
//
// Singular form drops the number because the badge column is tight; the
// glyph + word is enough signal at the row level. Multi-conflict gets
// the count because "how big a mess am I in" is the next question.
func formatMergeability(n int) string {
	if n == 1 {
		return "⚠ conflict"
	}
	return fmt.Sprintf("⚠ %d conflicts", n)
}

// gitIsAncestor returns true when ancestor's history is fully contained
// in descendant's history. Wraps `git merge-base --is-ancestor`, which
// exits 0 = is-ancestor, 1 = not-ancestor, ≥2 = error. Errors map to
// false (defer); this is fine because the caller's only use is a
// short-circuit optimization — a false negative just costs one
// merge-tree call.
func gitIsAncestor(ctx context.Context, path, ancestor, descendant string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", path,
		"merge-base", "--is-ancestor", ancestor, descendant)
	return cmd.Run() == nil
}
