// Package ghx wraps the gh CLI for the small set of canopy operations
// that need GitHub data (PR + issue lookups for `canopy new --pr` /
// `--issue`). gh is an optional dependency: when absent or unauthed,
// the helpers return ErrUnavailable so callers can degrade gracefully
// rather than crash.
//
// Why a separate package: keeps gh-shellout knowledge in one place
// and out of cmd/canopy/new.go. Lifecycle's pr_status detector still
// shells gh directly because it has its own caching layer, but new
// operations should funnel through here.
package ghx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("ghx")

// ErrUnavailable means gh is missing from PATH or is unauthenticated.
// Callers should treat this as "feature unavailable" rather than fatal.
var ErrUnavailable = errors.New("gh CLI unavailable (missing on PATH or unauthed)")

// ErrNotFound means gh ran successfully but the requested PR/issue
// doesn't exist (or isn't visible to the authed user).
var ErrNotFound = errors.New("not found")

// PR is the subset of pull-request fields canopy uses. Mirrors gh's
// JSON output shape; fields not listed here are silently dropped on
// unmarshal.
type PR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	HeadRefName string `json:"headRefName"` // source branch name
	BaseRefName string `json:"baseRefName"` // PR's target branch (usually "main")
	// IsCrossRepository is true when the PR comes from a fork. Affects
	// how canopy fetches the branch — same-repo PRs already have their
	// branch on origin; forks need a refs/pull/<num>/head fetch.
	IsCrossRepository bool `json:"isCrossRepository"`
	// HeadRepositoryOwner is the fork's owner login for cross-repo PRs.
	// Empty for same-repo PRs. Useful for the briefing's PR header.
	HeadRepositoryOwner struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
}

// Issue is the subset of issue fields canopy uses. Same shape rules
// as PR — gh's JSON has more fields but we only ask for these.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"` // "OPEN" | "CLOSED"
}

// Available reports whether gh is installed and (best-effort) authed.
// Cheap; just LookPath. Authentication is checked by gh itself when
// individual commands run; we don't probe `gh auth status` here
// because that's another shellout per call.
func Available() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

// FetchPR reads PR metadata via `gh pr view <num>`. cwd is set to
// projectRoot so gh resolves the right repo automatically (no need
// to pass --repo). Returns ErrUnavailable if gh isn't installed,
// ErrNotFound if gh exits non-zero (PR doesn't exist or is hidden).
func FetchPR(ctx context.Context, projectRoot string, num int) (*PR, error) {
	if !Available() {
		return nil, ErrUnavailable
	}
	out, err := runGH(ctx, projectRoot, "pr", "view", strconv.Itoa(num),
		"--json", "number,title,body,headRefName,baseRefName,isCrossRepository,headRepositoryOwner")
	if err != nil {
		log.Debug("ghx.FetchPR.failed", "num", num, "err", err)
		return nil, fmt.Errorf("FetchPR(%d): %w", num, ErrNotFound)
	}
	var pr PR
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, fmt.Errorf("FetchPR(%d): parse: %w", num, err)
	}
	return &pr, nil
}

// FetchIssue reads issue metadata via `gh issue view <num>`. Same
// shape as FetchPR.
func FetchIssue(ctx context.Context, projectRoot string, num int) (*Issue, error) {
	if !Available() {
		return nil, ErrUnavailable
	}
	out, err := runGH(ctx, projectRoot, "issue", "view", strconv.Itoa(num),
		"--json", "number,title,body,state")
	if err != nil {
		log.Debug("ghx.FetchIssue.failed", "num", num, "err", err)
		return nil, fmt.Errorf("FetchIssue(%d): %w", num, ErrNotFound)
	}
	var iss Issue
	if err := json.Unmarshal(out, &iss); err != nil {
		return nil, fmt.Errorf("FetchIssue(%d): parse: %w", num, err)
	}
	return &iss, nil
}

// runGH is the shared shellout helper. cwd ensures gh discovers the
// right repository. Output goes back as bytes for the caller to
// json.Unmarshal; stderr is captured into the error so the caller
// can surface gh's own message ("could not find pull request" etc.)
// instead of a generic "exit 1".
func runGH(ctx context.Context, cwd string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		// Capture stderr from ExitError so the caller's error message
		// reflects gh's actual failure cause.
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}
