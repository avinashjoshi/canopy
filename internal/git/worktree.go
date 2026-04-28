// Package git wraps the subset of `git worktree` commands that canopy needs.
//
// All exported functions return errors wrapped with %w so callers can use
// errors.Is against the sentinels in this package for known cases (branch
// already exists, path collision, etc.). Stderr from git is captured and
// included in non-sentinel error messages so the user can see what went
// wrong without grepping the log.
//
// This package does not understand canopy's "workspace" abstraction. It only
// knows about git worktrees on disk. The internal/workspace package composes
// these functions with internal/tmux and internal/state to build the full
// workspace lifecycle.
package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("git")

// Sentinel errors. Tests and callers use errors.Is against these to handle
// known cases distinctly from unexpected git failures.
var (
	// ErrBranchExists is returned when `git worktree add -b <branch>` fails
	// because <branch> is already a ref (any local or remote branch by that
	// name). Callers should suggest a different name or `canopy switch`.
	ErrBranchExists = errors.New("git: branch already exists")

	// ErrPathExists is returned when the target directory already exists on
	// disk. Distinct from ErrBranchExists because a stale worktree directory
	// outside canopy's state.json can produce this without a branch conflict.
	ErrPathExists = errors.New("git: target path already exists")

	// ErrPathNotFound is returned by Remove when the worktree path is not a
	// known git worktree (already removed, never created, or hand-deleted).
	ErrPathNotFound = errors.New("git: worktree path not found")
)

// Add creates a new git worktree at path with a new branch named branch.
// The new branch is created from the current HEAD of repoRoot.
//
// Returns ErrBranchExists or ErrPathExists for the corresponding conflicts.
// These cases are checked via pre-flight (git rev-parse for the branch,
// os.Stat for the path) so we don't depend on git's English error strings
// to recognize them — the substring fallback is only for unexpected stderr.
func Add(ctx context.Context, repoRoot, branch, path string) error {
	log.Info("git.add", "repo", repoRoot, "branch", branch, "path", path)

	// Pre-flight: branch must not already exist.
	if exists, err := branchExists(ctx, repoRoot, branch); err != nil {
		return fmt.Errorf("git.Add(%s): pre-flight branch check: %w", branch, err)
	} else if exists {
		return fmt.Errorf("git.Add(%s): %w", branch, ErrBranchExists)
	}

	// Pre-flight: target dir must not already exist.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("git.Add(%s): %w", path, ErrPathExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("git.Add(%s): pre-flight path check: %w", path, err)
	}

	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "add", "-b", branch, path)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Pre-flight should have caught the common conflicts. Anything that
		// fails here is something we didn't anticipate (permissions, locked
		// worktree, weird git config). Surface stderr so the user can see it.
		return fmt.Errorf("git.Add(%s -> %s): %w (stderr: %s)", branch, path, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// branchExists returns true if a local branch by that name exists in the
// repo at repoRoot. Uses git's exit code (0 = exists, 1 = not), not stderr
// parsing.
func branchExists(ctx context.Context, repoRoot, branch string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot,
		"rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Remove removes the git worktree at path. By default Remove refuses if the
// worktree has uncommitted changes (mimicking `git worktree remove` without
// --force); callers can pass force=true to bypass.
//
// Returns ErrPathNotFound if path is not a known worktree (checked via
// `git worktree list --porcelain`, not stderr parsing).
func Remove(ctx context.Context, repoRoot, path string, force bool) error {
	log.Info("git.remove", "repo", repoRoot, "path", path, "force", force)

	// Pre-flight: is path actually a registered worktree of repoRoot?
	if known, err := isWorktree(ctx, repoRoot, path); err != nil {
		return fmt.Errorf("git.Remove(%s): pre-flight check: %w", path, err)
	} else if !known {
		return fmt.Errorf("git.Remove(%s): %w", path, ErrPathNotFound)
	}

	args := []string{"-C", repoRoot, "worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)

	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Pre-flight should have caught the missing-worktree case. Anything
		// here is unexpected (locked, dirty without --force, permissions).
		return fmt.Errorf("git.Remove(%s): %w (stderr: %s)", path, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// DeleteBranch removes a local branch from repoRoot. Used by workspace
// removal so canopy doesn't leave dead branches behind after every
// `canopy rm` — workspaces are ephemeral by design and their branches
// should follow them out.
//
// Pass force=true to delete branches that haven't been merged. canopy's
// remove flow always uses force=true because the user explicitly asked
// for removal and may be removing a feature branch with unmerged work.
//
// Returns nil if the branch doesn't exist (idempotent — Remove can call
// this even after the branch has already been cleaned up by something else).
func DeleteBranch(ctx context.Context, repoRoot, branch string, force bool) error {
	log.Info("git.delete-branch", "repo", repoRoot, "branch", branch, "force", force)

	exists, err := branchExists(ctx, repoRoot, branch)
	if err != nil {
		return fmt.Errorf("git.DeleteBranch(%s): pre-flight: %w", branch, err)
	}
	if !exists {
		return nil // idempotent no-op
	}

	flag := "-d"
	if force {
		flag = "-D"
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "branch", flag, branch)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git.DeleteBranch(%s): %w (stderr: %s)", branch, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// isWorktree returns true if path is a registered worktree of the repo at
// repoRoot. Uses `git worktree list --porcelain`, which prints a line like
// "worktree /abs/path/to/wt" for each entry — stable, machine-readable
// format guaranteed by git's --porcelain contract.
func isWorktree(ctx context.Context, repoRoot, path string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("abs path: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git worktree list: %w", err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") &&
			strings.TrimPrefix(line, "worktree ") == abs {
			return true, nil
		}
	}
	return false, nil
}

// sanitizeRe matches runs of characters that are unsafe in tmux session
// names AND in filesystem path segments. Slashes, spaces, colons, and other
// punctuation become a single hyphen. Underscores, dots, alphanumerics,
// and existing hyphens are kept.
var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Sanitize returns a safe identifier derived from a git branch name. The
// result is suitable for use as both a tmux session name (Conductor-style:
// "<project>-<sanitized>") and as the basename of a filesystem path.
//
// Rules:
//   - Trim leading/trailing whitespace.
//   - Collapse runs of unsafe characters to a single hyphen.
//   - Trim leading/trailing hyphens.
//   - Case is preserved (JIRA-1234 stays JIRA-1234).
//
// Examples:
//
//	feature/oauth      -> feature-oauth
//	feature/sub/x      -> feature-sub-x
//	feat: bug          -> feat-bug
//	JIRA-1234          -> JIRA-1234
//	  spaced           -> spaced
//	//                 -> ""        (callers must check for empty)
//
// Sanitize never collapses to a name that would conflict with the original
// in canopy's state — the git branch keeps its original (possibly slashed)
// name; only the filesystem and tmux derivatives get sanitized.
func Sanitize(branch string) string {
	s := strings.TrimSpace(branch)
	s = sanitizeRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}
