package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// IsRepo reports whether dir is inside a git working tree.
//
// Implemented via `git -C <dir> rev-parse --is-inside-work-tree`. git prints
// "true" on success; on a non-repo dir it exits non-zero with a message on
// stderr. We treat any non-zero exit as "not a repo" rather than a failure
// condition, because the caller (cmd/canopy/route.go) wants a binary
// signal: route to init splash if this is a fresh repo, route to global
// mode otherwise. A missing git binary, a permissions error, or a dir that
// doesn't exist all collapse to "not a repo" via the returned error.
//
// The (bool, error) shape is preserved so callers that want to distinguish
// "definitely not a repo" from "couldn't even run git" can: a true return
// always means a repo was confirmed; a false return with a nil error means
// confirmed-not-a-repo; a false return with a non-nil error means we
// couldn't tell (git missing, exec failed, etc.) and the caller can decide.
func IsRepo(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		// ExitError with stderr "not a git repository" is the common
		// not-a-repo case; we collapse it to (false, nil) so callers
		// don't have to introspect the exit code. Any other error (git
		// binary missing, dir doesn't exist) we surface as (false, err).
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// CurrentBranch returns the current branch name of the worktree at dir.
// Used by Reconcile to detect post-create renames (`git branch -m newname`)
// so the TUI's branch column reflects the live name instead of the
// branch recorded at workspace-create time.
//
// Detached HEAD returns ("", nil) — no branch is checked out, but it's
// not an error condition (canopy never creates detached worktrees, but
// a user might detach manually). Callers treat empty as "leave the
// recorded branch alone."
//
// Errors (git missing, dir gone) return ("", err); callers should log
// and skip rather than fail the whole reconcile pass over one bad row.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Clone runs `git clone <url> <dest>` via exec.CommandContext.
//
// Captures stderr and embeds it in the returned error so auth failures,
// invalid URLs, and unreachable hosts surface readably to the caller —
// e.g. `canopy init <url>` can render the git error directly to the
// user instead of a generic "git failed."
//
// stdout is passed through to the provided writer when non-nil (so CLI
// callers can show git's "Cloning into 'foo'..." progress); pass nil to
// discard. stderr is always captured for the error message, regardless
// of the writer. (Bytes printed by git on a "fatal" exit go to stderr.)
//
// Cancellation: on ctx.Done, exec.CommandContext sends SIGKILL to git.
// Git's partial-clone dir is left behind — canopy doesn't rm -rf the
// partial because the dest path comes from user input and a buggy
// resolver could otherwise destroy real data. (Cleanup-on-cancel is
// captured in TODOS.md for a future PR with strict safety bounds.)
//
// Auth: this function does NOT capture stdin. SSH agent / HTTPS
// credential helpers / host-key prompts only work when the caller
// inherits stdin (CLI: just leave it; TUI: use tea.ExecProcess so git
// gets the real tty). Passing a tty-less context here silently breaks
// auth — the design doc records this constraint.
//
// Does NOT mkdir dest's parent. Caller must ensure the parent exists
// (cmd/canopy/init_source.ensureSourceRoot does this for the URL flow).
func Clone(ctx context.Context, url, dest string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, "git", "clone", url, dest)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Cancellation wins as the surfaced error so callers can
			// distinguish "user hit ctrl+c" from "git rejected creds."
			return fmt.Errorf("git clone: cancelled: %w", ctxErr)
		}
		if stderr != "" {
			return fmt.Errorf("git clone %s: %w (%s)", url, err, stderr)
		}
		return fmt.Errorf("git clone %s: %w", url, err)
	}
	return nil
}

// SourceRepoFromWorktree returns the absolute path of the source repo
// that owns the given worktree directory. Used by the global TUI's
// "open project" keybind to find the project's root even when the
// state.Workspace's ProjectRoot is an unmigrated v1 basename.
//
// Mechanism: `git -C <worktree> rev-parse --git-common-dir` returns the
// path to the .git directory shared across all worktrees of a repo —
// for a worktree, that's the SOURCE repo's .git, not the worktree's
// .git file. Strip the trailing `/.git` and you have the source repo
// path. EvalSymlinks canonicalizes the result so it matches what
// config.Load would produce.
//
// Returns ("", err) on any failure: dir isn't a worktree, git missing,
// path resolution fails. Caller treats as "can't derive."
func SourceRepoFromWorktree(ctx context.Context, worktree string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "--git-common-dir")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(string(out))
	if commonDir == "" {
		return "", exec.ErrNotFound
	}
	// commonDir may be relative (typical: ".git" when run from the repo
	// root) or absolute. Resolve against the worktree path.
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktree, commonDir)
	}
	// Strip the trailing .git (or /.git/worktrees/<name> for non-main
	// worktrees of the source repo's worktree machinery — those still
	// have the source repo as the parent of the .git dir).
	repoRoot := strings.TrimSuffix(commonDir, "/.git")
	repoRoot = strings.TrimSuffix(repoRoot, "/.git/")
	if filepath.Base(repoRoot) == ".git" {
		repoRoot = filepath.Dir(repoRoot)
	}
	resolved, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		// Fall back to the abs path if EvalSymlinks failed (rare).
		return filepath.Abs(repoRoot)
	}
	return resolved, nil
}
