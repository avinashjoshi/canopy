package lifecycle

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/state"
)

// detectRenameSuggested returns a Hint when:
//
//  1. The workspace has at least one commit past its base branch (i.e.,
//     the user has made progress).
//  2. The current branch name still matches the workspace name (i.e.,
//     it's still the auto-generated namegen pattern, e.g. "ancient-hornet"
//     and the user hasn't done `git branch -m feat/oauth` yet).
//
// Both conditions together signal "you've made meaningful work; the
// branch should reflect what it's for." The agent's AGENT.md briefing
// instructs it to act on this hint by running `git branch -m <name>`.
//
// Returns nil when:
//   - branch was already renamed (current branch != ws.Name)
//   - no commits past main yet (no work done)
//   - git command fails (treat as "no hint" — never block UI on a
//     diagnostic failure)
func detectRenameSuggested(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" {
		return nil
	}

	// Step 1: get the current branch name in this worktree.
	//
	// Historical note: this used to read state.Branch directly, which is
	// set at workspace-create time. With Manager.SyncBranch (v0.15+) the
	// statusline tick keeps state.Branch synced live, so reading it would
	// be cheaper. We still git rev-parse here to keep the detector
	// independent of the sync timing — if the user just ran `git
	// branch -m` and the next sync hasn't fired, the detector still sees
	// the new name and the rename-suggested hint correctly disappears.
	currentBranch, err := git.CurrentBranch(ctx, ws.Path)
	if err != nil || currentBranch == "" {
		// Detached HEAD, mid-rebase, or git error — no hint either way.
		return nil
	}

	// Step 2: only fire if the branch still matches the auto-generated
	// workspace name. If the user already renamed (currentBranch !=
	// ws.Name), the hint is satisfied — don't surface it.
	if currentBranch != ws.Name {
		return nil
	}

	// Step 3: count commits past the source repo's default branch. If
	// zero, no progress has been made yet — don't suggest renaming
	// until there's something to name.
	commitCount := gitCommitsPastDefault(ctx, ws.Path)
	if commitCount == 0 {
		return nil
	}

	return &state.Hint{
		Kind:       "rename_suggested",
		Message:    fmt.Sprintf("branch '%s' has %d commit(s) past main; rename to reflect intent", ws.Name, commitCount),
		Action:     "git branch -m <intent-name>",
		DetectedAt: time.Now(),
	}
}

// gitCommitsPastDefault returns the number of commits on HEAD that
// aren't reachable from the source repo's default branch. We use the
// remote-tracking branch (origin/<default>) as the reference, which
// requires a prior fetch — we accept that staleness rather than running
// fetch on every detector invocation (network + auth).
//
// Returns 0 on:
//   - any git error (treat as "no commits" rather than fabricate a
//     count we can't trust)
//   - the worktree is up-to-date with origin/<default>
//
// Implementation: `git rev-list --count <default>..HEAD` where <default>
// is origin/HEAD's ref (resolved per-call). Caches nothing — this runs
// on every detector tick and the rev-list is cheap (~5ms for typical
// repos).
func gitCommitsPastDefault(ctx context.Context, path string) int {
	// Resolve the default branch via origin/HEAD. Returns "main" or
	// "master" or whatever the user's repo uses.
	defaultBranch := gitDefaultBranch(ctx, path)
	if defaultBranch == "" {
		return 0
	}
	ref := "origin/" + defaultBranch

	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-list", "--count", ref+"..HEAD")
	out, err := cmd.Output()
	if err != nil {
		log.Debug("lifecycle.rename.rev-list", "path", path, "ref", ref, "err", err)
		return 0
	}
	count := 0
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &count); scanErr != nil {
		return 0
	}
	return count
}

// gitDefaultBranch returns the source repo's default branch name (e.g.,
// "main"). Resolved via `git symbolic-ref refs/remotes/origin/HEAD`,
// which is set when the worktree's source repo was cloned. Returns ""
// only when nothing matches — rare; only happens for repos with no
// main, no master, and no symbolic-ref.
//
// Probe order:
//
//  1. refs/remotes/origin/HEAD (the canonical "what does origin think
//     the default is" pointer; set on clone)
//  2. origin/main → origin/master (remote-tracked candidates if 1 isn't set)
//  3. local main → local master (purely-local repos with no remote)
//
// Each probe is a cheap local ref lookup (rev-parse). The 3rd tier is
// what makes the local-only "shipped" detection work without a remote.
func gitDefaultBranch(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "symbolic-ref",
		"--short", "refs/remotes/origin/HEAD")
	out, err := cmd.Output()
	if err == nil {
		// Output is "origin/main" — strip the "origin/" prefix.
		ref := strings.TrimSpace(string(out))
		if strings.HasPrefix(ref, "origin/") {
			return strings.TrimPrefix(ref, "origin/")
		}
		return ref
	}
	// Probe remote-tracked candidates first.
	for _, candidate := range []string{"main", "master"} {
		probe := exec.CommandContext(ctx, "git", "-C", path, "rev-parse",
			"--verify", "origin/"+candidate)
		if err := probe.Run(); err == nil {
			return candidate
		}
	}
	// Fall back to local branches for purely-local repos.
	for _, candidate := range []string{"main", "master"} {
		probe := exec.CommandContext(ctx, "git", "-C", path, "rev-parse",
			"--verify", "refs/heads/"+candidate)
		if err := probe.Run(); err == nil {
			return candidate
		}
	}
	return ""
}
