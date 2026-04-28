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
	"os/exec"
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
// Returns ErrBranchExists or ErrPathExists for the corresponding conflicts;
// any other failure (network, permissions, missing git binary) comes back
// wrapped with the captured stderr in the message.
func Add(ctx context.Context, repoRoot, branch, path string) error {
	log.Info("git.add", "repo", repoRoot, "branch", branch, "path", path)

	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "add", "-b", branch, path)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		s := strings.TrimSpace(stderr.String())
		// git's stderr distinguishes "a branch named X already exists" from
		// "<path> already exists". Match on substrings; the wording is stable
		// across git 2.30+ which the design doc requires.
		switch {
		case strings.Contains(s, "branch named") && strings.Contains(s, "already exists"):
			return fmt.Errorf("git.Add(%s): %w", branch, ErrBranchExists)
		case strings.Contains(s, "already exists"):
			return fmt.Errorf("git.Add(%s): %w", path, ErrPathExists)
		default:
			return fmt.Errorf("git.Add(%s -> %s): %w (stderr: %s)", branch, path, err, s)
		}
	}
	return nil
}

// Remove removes the git worktree at path. By default Remove refuses if the
// worktree has uncommitted changes (mimicking `git worktree remove` without
// --force); callers can pass force=true to bypass.
//
// Returns ErrPathNotFound if path is not a known worktree.
func Remove(ctx context.Context, repoRoot, path string, force bool) error {
	log.Info("git.remove", "repo", repoRoot, "path", path, "force", force)

	args := []string{"-C", repoRoot, "worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)

	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		s := strings.TrimSpace(stderr.String())
		switch {
		case strings.Contains(s, "is not a working tree"),
			strings.Contains(s, "No such file"):
			return fmt.Errorf("git.Remove(%s): %w", path, ErrPathNotFound)
		default:
			return fmt.Errorf("git.Remove(%s): %w (stderr: %s)", path, err, s)
		}
	}
	return nil
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
