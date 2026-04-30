package lifecycle

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/oncactus/canopy/internal/state"
)

// detectShipped returns a Hint when the workspace's branch has been
// merged into the default branch via squash-merge — the branch had
// distinct commits whose patches now live in main as a single squash
// commit.
//
// Detection is squash-merge ONLY (the most common gh PR workflow).
// We deliberately don't try to detect merge-commit-style merges
// because the signal is ambiguous from current git state alone:
// "HEAD is reachable from main" is true both when the branch was
// merge-commit-merged AND when the branch was forked from main and
// main fast-forwarded past it without any work on the branch (the
// "fresh fork that fell behind" case). We used to do the merge-commit
// check, and it false-positived on every fresh workspace whose main
// advanced past it — leading to a misleading "✓ shipped" badge on
// branches the user was actively working on.
//
// Future: store the branch's base commit at workspace creation time
// in state.Workspace, then merge-commit detection becomes safe (we
// can require base != HEAD before claiming shipped). For now, squash-
// merge-only is the conservative default — false negative on
// merge-commit workflows beats false positive on every fresh branch.
//
// Target ref selection:
//   - If `origin/<default>` exists, use that. Team-collaboration case.
//   - Else fall back to local `<default>`. Purely-local-repo case.
//
// `git cherry <target> HEAD` outputs one line per commit on HEAD past
// <target>:
//
//	"+ <sha> <subject>" → patch NOT in target (in-flight)
//	"- <sha> <subject>" → patch IS in target (squash-merged)
//
// If cherry output is non-empty AND every line starts with "-", the
// branch's commits have all been squash-merged into target.
//
// Caveat: this is the "fallback" signal — pr_status (when present) is
// the authoritative shipping signal. The projectlist badge renderer
// hides this hint when pr_status is also active for the same workspace.
//
// Returns nil when:
//   - branch has zero commits past target (fresh / never worked / merge-commit-merged)
//   - branch has commits past target that aren't in target yet (in-flight)
//   - any "+" line in cherry (still some unmerged work)
//   - we can't resolve <default> at all
//   - any git command fails (defer to next refresh)
func detectShipped(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" {
		return nil
	}

	defaultBranch := gitDefaultBranch(ctx, ws.Path)
	if defaultBranch == "" {
		return nil
	}

	target := "origin/" + defaultBranch
	hasRemote := gitRefExists(ctx, ws.Path, target)
	if !hasRemote {
		target = defaultBranch
		if !gitRefExists(ctx, ws.Path, target) {
			return nil
		}
	}

	cherryOut, err := exec.CommandContext(ctx, "git", "-C", ws.Path,
		"cherry", target, "HEAD").Output()
	if err != nil {
		log.Debug("lifecycle.shipped.cherry-failed",
			"path", ws.Path, "target", target, "err", err)
		return nil
	}
	cherryStr := strings.TrimSpace(string(cherryOut))
	if cherryStr == "" {
		// Either fresh fork (never had distinct commits) or
		// merge-commit-merged. Ambiguous; don't claim shipped.
		return nil
	}
	for _, line := range strings.Split(cherryStr, "\n") {
		if !strings.HasPrefix(line, "- ") {
			// Any "+ " line means there's still work past target
			// that hasn't been squashed in. Branch is in-flight.
			return nil
		}
	}
	return &state.Hint{
		Kind:       "shipped",
		Message:    fmt.Sprintf("branch squash-merged into %s", target),
		Action:     fmt.Sprintf("canopy rm %s", ws.Name),
		DetectedAt: time.Now(),
	}
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

