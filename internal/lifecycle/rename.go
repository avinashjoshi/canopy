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
//  1. The workspace has at least one commit past its base branch OR
//     uncommitted changes in the worktree (i.e., the agent has made
//     progress — uncommitted work counts because the user-visible
//     complaint is "I gathered intent, started working, but the branch
//     name is still namegen because nothing got committed yet").
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
//   - no commits AND no uncommitted changes yet (truly fresh — the
//     fresh-launch briefing already nudged rename; nothing more to add)
//   - git command fails (treat as "no hint" — never block UI on a
//     diagnostic failure)
//
// The uncommitted-only branch matters because it lets the resume-launch
// delta briefing re-nudge the agent that never renamed on the first
// turn. Without it, a workspace that picked up work without a commit
// past main would silently stay on its namegen name forever — the
// resume briefing only re-emits hints that fire, so the rename
// directive only ever showed up in the FRESH briefing once.
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

	// Step 3: detect progress. Either commits past main OR tracked-file
	// modifications count — see the function doc for why
	// uncommitted-only is in scope.
	//
	// Untracked files are EXCLUDED on purpose: build artifacts, log
	// files, IDE caches, and `scripts.setup` byproducts routinely
	// appear as untracked noise the agent has no intention of
	// committing. Counting them would spam the rename hint forever for
	// workspaces with chatty tooling, defeating the resume-briefing
	// re-nudge this loosening was meant to enable. "Intent gathered"
	// signals as edits to tracked files (modified/added/renamed/deleted
	// against HEAD), not whatever a tool drops into the worktree.
	commitCount := gitCommitsPastDefault(ctx, ws.Path)
	dirtyCount := gitTrackedDirtyCount(ctx, ws.Path)
	if commitCount == 0 && dirtyCount == 0 {
		return nil
	}

	// Message phrasing reflects what kind of progress was found.
	// "N commit(s)" reads as concrete work-units; "N uncommitted
	// file(s)" reads as in-flight work. Mixed cases prefer the
	// commit framing because commits are more durable signal than
	// dirty files.
	var msg string
	switch {
	case commitCount > 0:
		msg = fmt.Sprintf("branch '%s' has %d commit(s) past main; rename to reflect intent", ws.Name, commitCount)
	default:
		msg = fmt.Sprintf("branch '%s' has %d uncommitted file(s); rename to reflect intent", ws.Name, dirtyCount)
	}

	return &state.Hint{
		Kind:       "rename_suggested",
		Message:    msg,
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
