package lifecycle

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/avinashjoshi/canopy/internal/state"
)

// detectShipped returns a Hint when the workspace's branch has shipped
// — its commits are now part of origin/<default>'s history.
//
// We try TWO checks in order, because real-world git workflows include
// both merge-commit and squash-merge styles:
//
//  1. Merge-commit style: HEAD is reachable from origin/<default>. After
//     a merge commit, the branch's tip is itself on main's history.
//     `git merge-base --is-ancestor HEAD origin/<default>` exits 0.
//
//  2. Squash-merge style: the branch's commits are NOT individually on
//     main, but their CHANGES are (collapsed into one squash commit).
//     `git cherry origin/<default> HEAD` returns lines starting with
//     `-` for each branch commit whose patch is already in main. If
//     all of HEAD's commits past main are `-` lines, it's squashed.
//
// Both checks have a precondition: there must be at least ONE commit on
// the branch past origin/<default>. Without that, we'd false-positive on
// every fresh workspace (HEAD == origin/main vacuously satisfies
// is-ancestor). The "had work, now merged" signal is exactly: there were
// commits past main; either they're now reachable (merge-commit) or
// their patches are now in main (squash). No commits = no shipped, ever.
//
// Returns nil when:
//   - branch has zero commits past origin/<default> (fresh / never worked)
//   - branch has commits past main that aren't in main yet (in-flight)
//   - we can't resolve origin/<default> (no remote, fresh clone)
//   - any git command fails (defer to the next refresh)
//
// Caveat: requires an up-to-date origin/<default>. We don't fetch here.
// A merged PR surfaces only after the user's next `git fetch` (which
// canopy does on workspace creation, but not periodically).
func detectShipped(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" {
		return nil
	}

	defaultBranch := gitDefaultBranch(ctx, ws.Path)
	if defaultBranch == "" {
		return nil
	}
	target := "origin/" + defaultBranch

	// Precondition: main must have ADVANCED PAST our HEAD. On a fresh
	// workspace, HEAD == origin/<default>, so main hasn't moved past us
	// yet — nothing to ship. After any real merge (commit or squash),
	// main has at least one commit (the merge / squash) that's not on
	// our HEAD's reachable set, so the count is > 0.
	//
	// This is the v0.6-first-cut bug we're fixing: is-ancestor returns
	// true vacuously when HEAD == origin/<default>, which is the fresh
	// workspace case. The "main advanced" check filters that out.
	mainAdvanced := gitCommitsMainAheadOfHead(ctx, ws.Path, defaultBranch)
	if mainAdvanced == 0 {
		return nil
	}

	// Check 1: merge-commit style. HEAD reachable from origin/<default>.
	cmd := exec.CommandContext(ctx, "git", "-C", ws.Path,
		"merge-base", "--is-ancestor", "HEAD", target)
	if err := cmd.Run(); err == nil {
		return &state.Hint{
			Kind:       "shipped",
			Message:    fmt.Sprintf("branch reachable from %s (merge-commit style); ready to close out", target),
			Action:     fmt.Sprintf("canopy rm %s", ws.Name),
			DetectedAt: time.Now(),
		}
	}

	// Check 2: squash-merge style. `git cherry origin/<default> HEAD`
	// outputs one line per commit on HEAD past <default>:
	//   "+ <sha> <subject>" → patch NOT in main (in-flight)
	//   "- <sha> <subject>" → patch IS in main (squash-merged)
	//
	// If every line starts with "-", the squash absorbed all branch
	// commits → shipped. If any "+" exists, there's still work that
	// hasn't been squashed in.
	cherryOut, err := exec.CommandContext(ctx, "git", "-C", ws.Path,
		"cherry", target, "HEAD").Output()
	if err != nil {
		log.Debug("lifecycle.shipped.cherry-failed",
			"path", ws.Path, "target", target, "err", err)
		return nil
	}
	cherryStr := string(cherryOut)
	if cherryStr == "" {
		return nil
	}
	allSquashed := true
	for _, line := range strings.Split(cherryStr, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			allSquashed = false
			break
		}
	}
	if allSquashed {
		return &state.Hint{
			Kind:       "shipped",
			Message:    fmt.Sprintf("branch squash-merged into %s; ready to close out", target),
			Action:     fmt.Sprintf("canopy rm %s", ws.Name),
			DetectedAt: time.Now(),
		}
	}
	return nil
}
