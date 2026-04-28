package lifecycle

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/avinashjoshi/canopy/internal/state"
)

// detectShipped returns a Hint when the workspace's HEAD is reachable
// from the source repo's default branch (origin/<default>). That's the
// canonical "this branch has been merged and shipped" signal — the
// commits on HEAD are now part of main's history.
//
// We use reachability rather than "branch is gone from origin" because:
//
//   - Reachability works for both squash-merge and merge-commit PR styles
//     (the SHA changes on squash, but HEAD's tree is reachable from main).
//   - "Branch deleted from origin" alone is a weak signal — a force-push
//     deletion or rename also clears the upstream branch without merging.
//
// Returns nil when:
//   - HEAD is NOT reachable from origin/<default> (still in flight)
//   - we can't resolve the default branch (no remote, fresh clone, etc.)
//   - any git command fails (defer staleness to the next refresh rather
//     than fabricate a hint)
//
// Caveat: requires an up-to-date origin/<default>. We don't fetch here
// (network + auth); the user's most recent `git fetch` (or canopy's
// fetch in workspace.Create) is what we work against. A merged PR
// surfaces only after the next fetch — acceptable lag for v0.6.
func detectShipped(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" {
		return nil
	}

	defaultBranch := gitDefaultBranch(ctx, ws.Path)
	if defaultBranch == "" {
		return nil
	}
	target := "origin/" + defaultBranch

	// `git merge-base --is-ancestor HEAD <target>` exits 0 when HEAD
	// is reachable from target, 1 when not. Any other exit is an error.
	cmd := exec.CommandContext(ctx, "git", "-C", ws.Path,
		"merge-base", "--is-ancestor", "HEAD", target)
	err := cmd.Run()
	if err == nil {
		// HEAD is reachable from origin/<default> — shipped.
		return &state.Hint{
			Kind:       "shipped",
			Message:    fmt.Sprintf("branch reachable from %s; ready to close out", target),
			Action:     fmt.Sprintf("canopy rm %s", ws.Name),
			DetectedAt: time.Now(),
		}
	}

	// is-ancestor exit 1 = "not reachable" = not shipped (silent skip).
	// Any other error = "couldn't tell" = also silent skip; log at debug.
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return nil
	}
	log.Debug("lifecycle.shipped.merge-base-failed",
		"path", ws.Path, "target", target, "err", err)
	return nil
}
