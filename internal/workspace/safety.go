package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// SafetyPreflight runs the v0.6 hanging-work checks against a workspace
// and returns a list of human-readable messages for each one that
// triggered. Empty slice means "clean to remove."
//
// Lives in package workspace (not cmd/canopy) so both the CLI's
// `canopy rm` AND the TUI's 'd' delete flow share the same checks.
// v0.6 first cut put this in cmd/canopy/rm.go and the TUI's delete
// path silently bypassed it — bug surfaced on first real test.
//
// The three checks (in order):
//
//  1. uncommitted changes (`git status --porcelain`)
//  2. unpushed commits (HEAD ahead of upstream OR ahead of origin/<default>
//     when no upstream is tracked)
//  3. open PR for the branch (gh pr view)
//
// Best-effort: each check is independent and a failure of one (e.g.,
// git status erroring on an orphaned worktree) doesn't block the others.
// Returning an empty slice from a check failure means "no hang detected"
// — we never block rm because the diagnostic itself failed.
//
// Order of returned messages matters for UX: uncommitted comes first
// (most actionable; user likely needs to commit/stash), then unpushed
// (less common but data-loss-risk), then open-PR (informational; user
// might still want to rm and let the PR close naturally).
func (m *Manager) SafetyPreflight(ctx context.Context, name string) ([]string, error) {
	ws, err := m.Find(ctx, name)
	if err != nil {
		return nil, err
	}
	return safetyChecks(ctx, ws.Path, ws.Branch), nil
}

// safetyChecks is the package-private worker. Takes path + branch
// directly so callers can run the checks without first calling Find
// (e.g., when they already have ws in hand from a prior load).
//
// Two signals gate the unpushed and open-PR checks:
//
//  1. PR state (gh): if `gh pr view` reports MERGED, the local
//     "ahead of origin/main" commits are work that's already landed
//     in a squashed/rebased shape — flagging them as data-loss risk
//     is wrong and was the original "PR says merged but rm still
//     warns about losing work" confusion.
//
//  2. Upstream deleted (git): the branch had a configured upstream
//     and the remote ref is now gone. This is the canonical signal
//     of GitHub's auto-delete-branch flow on PR merge: the PR landed,
//     the branch was retired remote-side, and the local commits are
//     not "work in danger of being lost" — they were pushed and
//     accepted (squash-merged) before the remote branch went away.
//     Treating "upstream deleted" as merged also covers the case
//     where gh isn't on PATH, isn't authenticated, or rate-limits —
//     so the user doesn't get a force-screen just because gh is
//     unavailable.
func safetyChecks(ctx context.Context, worktreePath, branch string) []string {
	if worktreePath == "" {
		return nil // no path to check; orphaned-row safe path
	}
	prState := readPRState(ctx, worktreePath, branch)
	merged := prState == "MERGED" || wasUpstreamDeleted(ctx, worktreePath)

	var hangs []string
	if msg := checkUncommitted(ctx, worktreePath); msg != "" {
		hangs = append(hangs, msg)
	}
	if !merged {
		if msg := checkUnpushed(ctx, worktreePath); msg != "" {
			hangs = append(hangs, msg)
		}
	}
	if !merged && prState == "OPEN" {
		if msg := checkOpenPR(ctx, worktreePath, branch); msg != "" {
			hangs = append(hangs, msg)
		}
	}
	return hangs
}

// wasUpstreamDeleted returns true when the branch had an upstream
// configured (i.e., was pushed at some point) but the remote-tracking
// ref is no longer present locally. After a `git fetch --prune` (or
// any fetch with default `remote.<name>.prune` semantics), this state
// is the canonical signature of "GitHub auto-deleted the branch on PR
// merge."
//
// Why both conditions matter:
//   - Upstream configured: distinguishes "this branch was pushed and
//     tracked" from "freshly created local-only branch" (the latter
//     never had a remote in the first place; absence isn't a signal).
//   - Remote ref absent: the remote-side branch the upstream pointed
//     at is gone.
//
// Without an explicit prune the stale ref can linger in
// refs/remotes/origin/<branch> for a while; in that window we fall
// back to the gh-based MERGED signal in safetyChecks. Either signal
// alone is sufficient.
func wasUpstreamDeleted(ctx context.Context, path string) bool {
	// Resolve the current branch name from HEAD. Detached HEAD or
	// non-branch state → no signal.
	branchOut, err := exec.CommandContext(ctx, "git", "-C", path,
		"symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return false
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "" {
		return false
	}

	// Read upstream configuration directly from .git/config rather
	// than via `@{u}` — once the remote-tracking ref is gone, git's
	// `@{u}` resolution itself fails (which is exactly the state
	// we're trying to detect, not bail out on).
	remoteOut, err := exec.CommandContext(ctx, "git", "-C", path,
		"config", "--get", "branch."+branch+".remote").Output()
	if err != nil {
		return false // no upstream configured (never pushed)
	}
	remote := strings.TrimSpace(string(remoteOut))
	if remote == "" || remote == "." {
		// remote == "." means upstream is in this same repo (no
		// remote tracking ref to delete). Skip.
		return false
	}
	mergeOut, err := exec.CommandContext(ctx, "git", "-C", path,
		"config", "--get", "branch."+branch+".merge").Output()
	if err != nil {
		return false
	}
	merge := strings.TrimSpace(string(mergeOut))
	if !strings.HasPrefix(merge, "refs/heads/") {
		return false
	}
	upstreamBranch := strings.TrimPrefix(merge, "refs/heads/")

	// Probe whether the remote-tracking ref still exists. Absent
	// ref + present upstream config = "branch was pushed, then the
	// remote-side branch was deleted."
	trackingRef := "refs/remotes/" + remote + "/" + upstreamBranch
	probe := exec.CommandContext(ctx, "git", "-C", path, "rev-parse",
		"--verify", "--quiet", trackingRef)
	return probe.Run() != nil
}

// readPRState returns the gh-reported PR state for the branch, or empty
// when gh is missing / no PR exists / lookup fails. Used by safetyChecks
// to gate which downstream checks fire. Single gh call shared across
// the unpushed and open-PR gates.
func readPRState(ctx context.Context, worktreePath, branch string) string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", branch,
		"--json", "state,number")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	const tag = `"state":"`
	i := strings.Index(string(out), tag)
	if i < 0 {
		return ""
	}
	rest := string(out)[i+len(tag):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// checkUncommitted: empty git status --porcelain output = clean tree.
// Any output (or git error) is treated conservatively: empty output
// means clean, errors mean "can't tell, don't block."
func checkUncommitted(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return "" // diagnostic failed; orphan-friendly path
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return fmt.Sprintf("uncommitted changes (%d file(s)) — commit, stash, or discard first", len(lines))
}

// checkUnpushed: HEAD has commits not on the upstream branch (or, when
// no upstream is tracked, on origin/<default>).
func checkUnpushed(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "log",
		"--oneline", "@{u}..HEAD")
	out, err := cmd.Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 1 && lines[0] == "" {
			return ""
		}
		return fmt.Sprintf("%d unpushed commit(s) — push first or accept the loss", len(lines))
	}
	// No upstream tracked; fall back to origin/<default>..HEAD.
	defaultBranch := defaultBranchSafe(ctx, path)
	if defaultBranch == "" {
		return ""
	}
	cmd = exec.CommandContext(ctx, "git", "-C", path, "log",
		"--oneline", "origin/"+defaultBranch+"..HEAD")
	out, err = cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	return fmt.Sprintf("%d commit(s) on this branch with no upstream — push or accept the loss", len(lines))
}

// checkOpenPR: gh pr view for an open PR on the branch.
func checkOpenPR(ctx context.Context, worktreePath, branch string) string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", branch,
		"--json", "state,number")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	if strings.Contains(string(out), `"state":"OPEN"`) {
		num := extractPRNumber(string(out))
		if num != "" {
			return fmt.Sprintf("PR #%s is open — let it merge or close it first", num)
		}
		return "an open PR exists for this branch — let it merge or close it first"
	}
	return ""
}

// defaultBranchSafe: same shape as the lifecycle package's helper
// but kept local to avoid a back-import (lifecycle imports state, and
// we don't want lifecycle importing workspace either).
func defaultBranchSafe(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "symbolic-ref",
		"--short", "refs/remotes/origin/HEAD")
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		return strings.TrimPrefix(ref, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		probe := exec.CommandContext(ctx, "git", "-C", path, "rev-parse",
			"--verify", "origin/"+b)
		if err := probe.Run(); err == nil {
			return b
		}
	}
	return ""
}

// extractPRNumber pulls the PR number out of gh's JSON output via a
// small string scan — full json.Unmarshal would be heavy for one field.
func extractPRNumber(s string) string {
	const tag = `"number":`
	i := strings.Index(s, tag)
	if i < 0 {
		return ""
	}
	rest := s[i+len(tag):]
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}
