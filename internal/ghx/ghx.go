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
	"sync"
	"time"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/host"
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
	// Author is the PR author's login. Present for both same-repo and
	// cross-repo PRs (unlike HeadRepositoryOwner, which is fork-only).
	// Canopy stamps this onto Workspace.Owner at creation so a workspace
	// spun up to review someone's PR is visibly marked as theirs.
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
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
		"--json", "number,title,body,headRefName,baseRefName,isCrossRepository,headRepositoryOwner,author")
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

// PRSummary is the subset of fields the new-workspace picker needs
// per PR. Smaller than PR (no body, no fork-detection fields) so
// `gh pr list` returns quickly even on big repos.
type PRSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	HeadRefName string `json:"headRefName"`
}

// IssueSummary mirrors PRSummary for issues.
type IssueSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

// listCache memoizes ListPRs / ListIssues for 60s per (projectRoot,
// kind). `gh pr list` typically runs ~500ms on a warm cache, ~1s
// cold; caching for 60s makes the picker feel instant on re-open
// without significantly staling the data.
//
// Mutex-protected map; sync.Map would be overkill at this size
// (one entry per project per kind).
var (
	listCacheMu  sync.Mutex
	listCacheMap = map[string]listCacheEntry{}
)

type listCacheEntry struct {
	prs    []PRSummary
	issues []IssueSummary
	at     time.Time
}

const listCacheTTL = 60 * time.Second

// ListPRs returns up to `limit` open PRs in the repo at projectRoot,
// most-recent first. Results are cached 60s per project so the TUI
// picker feels instant on repeated opens.
//
// Returns ErrUnavailable if gh is missing. Returns an empty slice
// (not an error) when the repo has no open PRs — callers render an
// empty-state hint rather than treating it as failure.
func ListPRs(ctx context.Context, projectRoot string, limit int) ([]PRSummary, error) {
	if !Available() {
		return nil, ErrUnavailable
	}
	if limit <= 0 {
		limit = 20
	}

	cacheKey := "pr|" + projectRoot
	listCacheMu.Lock()
	if e, ok := listCacheMap[cacheKey]; ok && time.Since(e.at) < listCacheTTL {
		out := append([]PRSummary(nil), e.prs...)
		listCacheMu.Unlock()
		return out, nil
	}
	listCacheMu.Unlock()

	out, err := runGH(ctx, projectRoot, "pr", "list",
		"--state", "open",
		"--limit", strconv.Itoa(limit),
		"--json", "number,title,author,headRefName")
	if err != nil {
		log.Debug("ghx.ListPRs.failed", "err", err)
		return nil, fmt.Errorf("ListPRs: %w", err)
	}
	var prs []PRSummary
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("ListPRs: parse: %w", err)
	}

	listCacheMu.Lock()
	listCacheMap[cacheKey] = listCacheEntry{prs: prs, at: time.Now()}
	listCacheMu.Unlock()

	return prs, nil
}

// RemoteListPRs is the SSH analog of ListPRs: runs `gh pr list` on a
// remote host inside `remoteCwd` and parses the same JSON. Used by the
// new-workspace TUI picker against a remote-row target where the local
// gh client has no knowledge of the remote project's repo.
//
// sshTarget is the literal ssh argument (e.g. "user@host" or a Host
// alias); remoteCwd is the absolute project path on the remote.
// Returns an empty slice (not an error) when the remote repo has no
// open PRs. Errors include the remote stderr when available so the
// picker can surface "gh not installed on tower" rather than a
// generic "exit 1".
//
// Cache key includes sshTarget + remoteCwd so two hosts (or two
// projects on the same host) don't share a list.
func RemoteListPRs(ctx context.Context, sshTarget, remoteCwd string, limit int) ([]PRSummary, error) {
	if sshTarget == "" || remoteCwd == "" {
		return nil, fmt.Errorf("RemoteListPRs: sshTarget and remoteCwd required")
	}
	if limit <= 0 {
		limit = 20
	}

	cacheKey := "pr|remote|" + sshTarget + "|" + remoteCwd
	listCacheMu.Lock()
	if e, ok := listCacheMap[cacheKey]; ok && time.Since(e.at) < listCacheTTL {
		out := append([]PRSummary(nil), e.prs...)
		listCacheMu.Unlock()
		return out, nil
	}
	listCacheMu.Unlock()

	remoteCmd := fmt.Sprintf("cd %s && gh pr list --state open --limit %d --json number,title,author,headRefName",
		host.ShellSingleQuote(remoteCwd), limit)
	out, err := runSSHCapture(ctx, sshTarget, remoteCmd)
	if err != nil {
		log.Debug("ghx.RemoteListPRs.failed", "target", sshTarget, "cwd", remoteCwd, "err", err)
		return nil, fmt.Errorf("RemoteListPRs: %w", err)
	}
	var prs []PRSummary
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("RemoteListPRs: parse: %w", err)
	}

	listCacheMu.Lock()
	listCacheMap[cacheKey] = listCacheEntry{prs: prs, at: time.Now()}
	listCacheMu.Unlock()

	return prs, nil
}

// RemoteListIssues is the SSH analog of ListIssues. Same shape and
// caching policy as RemoteListPRs.
func RemoteListIssues(ctx context.Context, sshTarget, remoteCwd string, limit int) ([]IssueSummary, error) {
	if sshTarget == "" || remoteCwd == "" {
		return nil, fmt.Errorf("RemoteListIssues: sshTarget and remoteCwd required")
	}
	if limit <= 0 {
		limit = 20
	}

	cacheKey := "issue|remote|" + sshTarget + "|" + remoteCwd
	listCacheMu.Lock()
	if e, ok := listCacheMap[cacheKey]; ok && time.Since(e.at) < listCacheTTL {
		out := append([]IssueSummary(nil), e.issues...)
		listCacheMu.Unlock()
		return out, nil
	}
	listCacheMu.Unlock()

	remoteCmd := fmt.Sprintf("cd %s && gh issue list --state open --limit %d --json number,title,author",
		host.ShellSingleQuote(remoteCwd), limit)
	out, err := runSSHCapture(ctx, sshTarget, remoteCmd)
	if err != nil {
		log.Debug("ghx.RemoteListIssues.failed", "target", sshTarget, "cwd", remoteCwd, "err", err)
		return nil, fmt.Errorf("RemoteListIssues: %w", err)
	}
	var issues []IssueSummary
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("RemoteListIssues: parse: %w", err)
	}

	listCacheMu.Lock()
	listCacheMap[cacheKey] = listCacheEntry{issues: issues, at: time.Now()}
	listCacheMu.Unlock()

	return issues, nil
}

// runSSHCapture runs a remote command via host.SSHRunUserBatch and
// returns stdout bytes. Stderr is captured into the error so the
// caller surfaces remote gh's own message ("gh: command not found",
// "could not find pull request") instead of a generic exit.
func runSSHCapture(ctx context.Context, sshTarget, remoteCmd string) ([]byte, error) {
	cmd := host.SSHRunUserBatch(ctx, sshTarget, remoteCmd)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("ssh %s: %s", sshTarget, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("ssh %s: %w", sshTarget, err)
	}
	return out, nil
}

// ListIssues returns up to `limit` open issues. Same shape and
// caching policy as ListPRs.
func ListIssues(ctx context.Context, projectRoot string, limit int) ([]IssueSummary, error) {
	if !Available() {
		return nil, ErrUnavailable
	}
	if limit <= 0 {
		limit = 20
	}

	cacheKey := "issue|" + projectRoot
	listCacheMu.Lock()
	if e, ok := listCacheMap[cacheKey]; ok && time.Since(e.at) < listCacheTTL {
		out := append([]IssueSummary(nil), e.issues...)
		listCacheMu.Unlock()
		return out, nil
	}
	listCacheMu.Unlock()

	out, err := runGH(ctx, projectRoot, "issue", "list",
		"--state", "open",
		"--limit", strconv.Itoa(limit),
		"--json", "number,title,author")
	if err != nil {
		log.Debug("ghx.ListIssues.failed", "err", err)
		return nil, fmt.Errorf("ListIssues: %w", err)
	}
	var issues []IssueSummary
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("ListIssues: parse: %w", err)
	}

	listCacheMu.Lock()
	listCacheMap[cacheKey] = listCacheEntry{issues: issues, at: time.Now()}
	listCacheMu.Unlock()

	return issues, nil
}

// ResetListCache wipes the PR/Issue list cache. Exposed for tests
// and for a future "force refresh" key in the picker.
func ResetListCache() {
	listCacheMu.Lock()
	listCacheMap = map[string]listCacheEntry{}
	listCacheMu.Unlock()
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
