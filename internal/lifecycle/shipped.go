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
// — its commits are now part of the default branch's history.
//
// Target ref selection:
//   - If `origin/<default>` exists (the workspace's source repo has a
//     remote), use that. This is the team-collaboration case — "shipped"
//     means "merged into the remote's main."
//   - Else fall back to local `<default>` (e.g. `main`). This is the
//     purely-local-repo case — "shipped" means "merged into local main."
//
// We try TWO checks in order, because real-world git workflows include
// both merge-commit and squash-merge styles:
//
//  1. Merge-commit style: HEAD is reachable from <target>. After
//     a merge commit, the branch's tip is itself on main's history.
//     `git merge-base --is-ancestor HEAD <target>` exits 0.
//
//  2. Squash-merge style: the branch's commits are NOT individually on
//     main, but their CHANGES are (collapsed into one squash commit).
//     `git cherry <target> HEAD` returns lines starting with `-` for
//     each branch commit whose patch is already in main. If all of
//     HEAD's commits past main are `-` lines, it's squashed.
//
// Both checks have a precondition: there must be at least ONE commit on
// the branch past <target>. Without that, we'd false-positive on
// every fresh workspace (HEAD == <target> vacuously satisfies
// is-ancestor). The "had work, now merged" signal is exactly: there were
// commits past main; either they're now reachable (merge-commit) or
// their patches are now in main (squash). No commits = no shipped, ever.
//
// Caveat: this is the "fallback" signal — pr_status (when present) is
// the authoritative shipping signal because a "merged into main" branch
// without a corresponding merged-or-closed PR is a rare/odd state. The
// projectlist badge renderer hides this hint when pr_status is also
// active for the same workspace.
//
// Returns nil when:
//   - branch has zero commits past target (fresh / never worked)
//   - branch has commits past main that aren't in main yet (in-flight)
//   - we can't resolve <default> at all (rare; no main, no master, no remote HEAD)
//   - any git command fails (defer to the next refresh)
//
// Caveat for the remote case: requires an up-to-date origin/<default>.
// We don't fetch here. A merged PR surfaces only after the user's next
// `git fetch` (which canopy does on workspace creation, but not
// periodically).
func detectShipped(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" {
		return nil
	}

	defaultBranch := gitDefaultBranch(ctx, ws.Path)
	if defaultBranch == "" {
		return nil
	}

	// Prefer origin/<default> when the source repo has a remote; fall
	// back to local <default> for purely-local repos. We probe by
	// checking whether origin/<default> resolves to a commit; on local
	// repos that ref simply doesn't exist and rev-parse returns non-zero.
	target := "origin/" + defaultBranch
	hasRemote := gitRefExists(ctx, ws.Path, target)
	if !hasRemote {
		target = defaultBranch
		if !gitRefExists(ctx, ws.Path, target) {
			// No usable target ref. Defer to the next refresh.
			return nil
		}
	}

	// Precondition: target must have ADVANCED PAST our HEAD. On a fresh
	// workspace, HEAD == <target>, so the target hasn't moved past us
	// yet — nothing to ship. After any real merge (commit or squash),
	// target has at least one commit (the merge / squash) that's not on
	// our HEAD's reachable set, so the count is > 0.
	//
	// This is the v0.6-first-cut bug we're fixing: is-ancestor returns
	// true vacuously when HEAD == <target>, which is the fresh
	// workspace case. The "target advanced" check filters that out.
	if gitCommitsAheadOf(ctx, ws.Path, "HEAD", target) == 0 {
		return nil
	}

	// Locality qualifier in the message: helps the user disambiguate
	// "in sync with origin/main" (remote-tracked) from "in sync with
	// main" (purely-local repo).
	locality := "remote"
	if !hasRemote {
		locality = "local"
	}

	// Check 1: merge-commit style. HEAD reachable from <target>.
	cmd := exec.CommandContext(ctx, "git", "-C", ws.Path,
		"merge-base", "--is-ancestor", "HEAD", target)
	if err := cmd.Run(); err == nil {
		return &state.Hint{
			Kind:       "shipped",
			Message:    fmt.Sprintf("branch reachable from %s (%s, merge-commit style)", target, locality),
			Action:     fmt.Sprintf("canopy rm %s", ws.Name),
			DetectedAt: time.Now(),
		}
	}

	// Check 2: squash-merge style. `git cherry <target> HEAD` outputs
	// one line per commit on HEAD past <target>:
	//   "+ <sha> <subject>" → patch NOT in target (in-flight)
	//   "- <sha> <subject>" → patch IS in target (squash-merged)
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
			Message:    fmt.Sprintf("branch squash-merged into %s (%s)", target, locality),
			Action:     fmt.Sprintf("canopy rm %s", ws.Name),
			DetectedAt: time.Now(),
		}
	}
	return nil
}

// gitRefExists returns true when ref resolves to a commit in the repo
// at path. Used by detectShipped to choose between origin/<default> and
// local <default> as the merge target. Cheap; just `git rev-parse
// --verify <ref>^{commit}` which fails silently when the ref doesn't
// exist.
func gitRefExists(ctx context.Context, path, ref string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-parse",
		"--verify", "--quiet", ref+"^{commit}")
	return cmd.Run() == nil
}

// gitCommitsAheadOf returns the number of commits reachable from `to`
// but not from `from` — the "to is ahead of from" count.
//
// Wraps the "main advanced past HEAD" check from rename.go so detectShipped
// can reuse it against either origin/<default> or local <default>.
func gitCommitsAheadOf(ctx context.Context, path, from, to string) int {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-list",
		"--count", from+".."+to)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	count := 0
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &count); scanErr != nil {
		return 0
	}
	return count
}
