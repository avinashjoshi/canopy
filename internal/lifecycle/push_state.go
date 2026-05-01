package lifecycle

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/avinashjoshi/canopy/internal/state"
)

// detectPushState returns a Hint when the workspace's local branch has
// commits that are NOT yet on its upstream tracking ref (typically
// origin/<branch>). This is the signal users actually ask for 20× a
// day — "is my work backed up on origin?" — which the existing
// git_stats `↑N` badge does NOT answer (git_stats compares HEAD to the
// default branch, not to the branch's own upstream).
//
// Two distinct shapes:
//
//	⇡N           local has N commits the upstream doesn't (push needed)
//	⇅            local and upstream have both diverged (force-push or
//	             pull-rebase needed)
//
// "Behind upstream but not ahead" is intentionally NOT surfaced here —
// that's a pull-state signal, not push-state. Conflating the two is
// what motivated splitting this from git_stats in the first place.
//
// Edge case — branch never pushed: when no upstream is configured,
// fall back to counting commits since the merge-base with the default
// branch. Same `⇡N` glyph; the Action string clarifies the user needs
// `git push -u origin <branch>` rather than a plain `git push`. The
// badge stays the same shape so the eye doesn't have to learn a new
// symbol for the no-upstream case.
//
// Returns nil when:
//   - workspace path empty
//   - HEAD detached (no branch to compare upstream against)
//   - branch is fully synced with upstream (or upstream-less + no
//     commits past default branch)
//   - any underlying git command fails (defer to next refresh)
//
// Cost: 2-3 short git invocations per workspace per refresh
// (rev-parse for upstream, two rev-list --count). All under 30ms;
// runs inside RunFast's goroutine pool.
func detectPushState(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" {
		return nil
	}
	branch := gitCurrentBranch(ctx, ws.Path)
	if branch == "" {
		// Detached HEAD or git error — stuck_state surfaces the detached
		// signal; no separate push_state hint here.
		return nil
	}

	upstream, ok := gitUpstream(ctx, ws.Path)
	if !ok {
		// No upstream configured. Fall back to "commits past default
		// branch" — that's the count the user would push if they ran
		// `git push -u origin <branch>` right now.
		return detectPushStateNoUpstream(ctx, ws.Path, branch)
	}

	ahead := gitRevListCount(ctx, ws.Path, upstream, "HEAD")  // local has, upstream doesn't
	behind := gitRevListCount(ctx, ws.Path, "HEAD", upstream) // upstream has, local doesn't

	switch {
	case ahead > 0 && behind > 0:
		return &state.Hint{
			Kind:       "push_state",
			Message:    "⇅",
			Action:     "git fetch && git push --force-with-lease",
			DetectedAt: time.Now(),
		}
	case ahead > 0:
		return &state.Hint{
			Kind:       "push_state",
			Message:    fmt.Sprintf("⇡%d", ahead),
			Action:     "git push",
			DetectedAt: time.Now(),
		}
	}
	// ahead == 0 — either fully synced (behind == 0) or strictly behind.
	// Pull-state is out of scope; return nil in both cases.
	return nil
}

// detectPushStateNoUpstream covers the "branch has never been pushed"
// case. Counts commits between the default branch's merge-base and
// HEAD; if any exist, surfaces ⇡N with an action that wires upstream
// on first push. If no commits past the default branch (a brand-new
// workspace), returns nil so the badge column stays clean for fresh
// rows.
func detectPushStateNoUpstream(ctx context.Context, path, branch string) *state.Hint {
	defaultBranch := gitDefaultBranch(ctx, path)
	if defaultBranch == "" {
		return nil
	}
	// Prefer origin/<default>; fall back to local <default> for purely-
	// local repos. Same shape as detectGitStats.
	target := "origin/" + defaultBranch
	if !gitRefExists(ctx, path, target) {
		target = defaultBranch
		if !gitRefExists(ctx, path, target) {
			return nil
		}
	}
	n := gitRevListCount(ctx, path, target, "HEAD")
	if n == 0 {
		return nil
	}
	return &state.Hint{
		Kind:       "push_state",
		Message:    fmt.Sprintf("⇡%d", n),
		Action:     fmt.Sprintf("git push -u origin %s", branch),
		DetectedAt: time.Now(),
	}
}

// gitUpstream returns the abbreviated name of the current branch's
// upstream tracking ref (e.g., "origin/feature"), reported by
// `git rev-parse --abbrev-ref @{upstream}`. The second return value
// is false when no upstream is configured or any git error occurs —
// caller treats both as "no upstream."
//
// Why abbreviated: the abbrev-ref form is what `git rev-list` accepts
// directly (as a ref name) without surrounding quotes. The absolute
// form `refs/remotes/origin/feature` would also work but the abbrev
// form keeps log lines and error messages compact.
func gitUpstream(ctx context.Context, path string) (string, bool) {
	cmd := exec.CommandContext(ctx, "git", "-C", path,
		"rev-parse", "--abbrev-ref", "@{upstream}")
	out, err := cmd.Output()
	if err != nil {
		// Most common: branch.<name>.remote / branch.<name>.merge unset
		// (i.e., never pushed). git exits non-zero with a stderr message
		// we don't need to parse.
		return "", false
	}
	upstream := strings.TrimSpace(string(out))
	if upstream == "" {
		return "", false
	}
	return upstream, true
}
