package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/state"
)

// prStatusCacheTTL is how long a pr_status detector result stays cached
// before we re-shell-out to gh. 10 minutes balances "fresh enough that
// the user sees PR state changes within a reasonable window" against
// "doesn't burn the GitHub API budget."
//
// At 10min TTL with 10 active workspaces, the worst-case rate is
// 60 detector calls/hour = ~1.2% of an authenticated GitHub user's
// 5000/hr budget. Acceptable headroom.
const prStatusCacheTTL = 10 * time.Minute

// ghMissingLogged tracks whether we've already log-warned about gh being
// missing this session. We log once and stay silent thereafter — the
// detector itself returns nil (no hint) on every call, so the warning
// is informational only.
var (
	ghMissingMu     sync.Mutex
	ghMissingLogged bool
)

// prStatusCache holds per-workspace pr_status results, keyed by
// "<projectRoot>|<branch>". A small map; ~10 entries for a typical
// power user. Mutex-protected; sync.Map would be overkill.
var (
	prStatusMu    sync.Mutex
	prStatusCache = map[string]prStatusEntry{}
)

type prStatusEntry struct {
	hint     *state.Hint // nil if "no PR for this branch"
	cachedAt time.Time
}

// detectPRStatus returns a Hint when an open or merged PR exists for
// the workspace's branch. Cached 10 minutes per (project, branch). On
// cache hit, returns the cached value instantly without shelling out.
//
// Returns nil when:
//   - gh is missing on PATH (logs once per session, then silently nil)
//   - gh is unauthenticated (gh's own error message lands in stderr;
//     we silently nil and rely on the user noticing eventually)
//   - no PR exists for the branch
//   - cached miss (we cache "no PR" too, with the same TTL, to avoid
//     re-querying for branches that genuinely have no PR yet)
//
// pr_status hints persist as "PR #142 open: 2 reviews pending" or
// "PR #142 merged 2 hours ago" — formatted for human glance, not
// machine parse.
func detectPRStatus(ctx context.Context, ws state.Workspace) *state.Hint {
	if ws.Path == "" || ws.ProjectRoot == "" {
		return nil
	}

	// Cache key: project + current branch. We prefer git's live view but
	// fall back to ws.Branch (which Manager.SyncBranch keeps live now,
	// so the fallback is also fresh — historical comments about staleness
	// no longer apply). git.CurrentBranch returns "" on detached HEAD or
	// any error; either way, ws.Branch is the right backstop.
	currentBranch, _ := git.CurrentBranch(ctx, ws.Path)
	if currentBranch == "" {
		currentBranch = ws.Branch
	}
	if currentBranch == "" {
		return nil
	}
	cacheKey := ws.ProjectRoot + "|" + currentBranch

	prStatusMu.Lock()
	if entry, ok := prStatusCache[cacheKey]; ok {
		if time.Since(entry.cachedAt) < prStatusCacheTTL {
			prStatusMu.Unlock()
			// Cache hit. Return a copy of the cached hint (or nil if
			// "no PR" was cached).
			if entry.hint == nil {
				return nil
			}
			h := *entry.hint
			return &h
		}
	}
	prStatusMu.Unlock()

	// Cache miss or stale — query gh.
	hint := queryPRStatus(ctx, ws.ProjectRoot, currentBranch)

	// Cache the result (including nil — "no PR" caches with the same
	// TTL to avoid hammering gh for branches that won't get a PR).
	prStatusMu.Lock()
	prStatusCache[cacheKey] = prStatusEntry{hint: hint, cachedAt: time.Now()}
	prStatusMu.Unlock()

	return hint
}

// queryPRStatus shells out to gh and returns the parsed hint. Used only
// on cache miss; the caller (detectPRStatus) handles caching.
//
// Implementation: `gh pr view <branch> --json number,state,mergedAt,reviewDecision`
// returns enough to render the hint. Exit code 0 = PR found; non-zero =
// no PR (gh exits 1 with a message like "no pull requests found" — we
// don't parse the message, just treat any error as "no PR").
func queryPRStatus(ctx context.Context, projectRoot, branch string) *state.Hint {
	if !ghAvailable() {
		return nil
	}

	// gh pr view with --json returns structured JSON we can parse.
	// --jq doesn't help here; we want all four fields and Go's json
	// unmarshal is the cleanest path.
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", branch,
		"--repo", repoFromProjectRoot(projectRoot),
		"--json", "number,state,mergedAt,reviewDecision,statusCheckRollup")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		// gh exits non-zero when no PR exists for the branch. Treat
		// silently.
		log.Debug("lifecycle.pr_status.gh-failed",
			"branch", branch, "err", err)
		return nil
	}

	var prData struct {
		Number         int    `json:"number"`
		State          string `json:"state"` // "OPEN" | "CLOSED" | "MERGED"
		MergedAt       string `json:"mergedAt,omitempty"`
		ReviewDecision string `json:"reviewDecision,omitempty"`
		// statusCheckRollup is an array of check entries; we collapse to
		// a single conclusion (passing / failing / pending) for the badge.
		StatusCheckRollup []struct {
			// CI checks use Conclusion ("SUCCESS"/"FAILURE"/"") + Status
			// ("COMPLETED"/"IN_PROGRESS"/"QUEUED"). Status checks (older
			// commit-status API) use only State ("SUCCESS"/"FAILURE"/
			// "PENDING"). We read both shapes and normalize.
			Conclusion string `json:"conclusion,omitempty"`
			Status     string `json:"status,omitempty"`
			State      string `json:"state,omitempty"`
		} `json:"statusCheckRollup,omitempty"`
	}
	if jsonErr := json.Unmarshal(out, &prData); jsonErr != nil {
		log.Warn("lifecycle.pr_status.parse-failed",
			"branch", branch, "err", jsonErr)
		return nil
	}

	// Render a one-line message based on state.
	var msg string
	switch prData.State {
	case "OPEN":
		switch prData.ReviewDecision {
		case "APPROVED":
			msg = fmt.Sprintf("PR #%d open, approved; awaiting merge", prData.Number)
		case "REVIEW_REQUIRED", "":
			msg = fmt.Sprintf("PR #%d open; awaiting reviews", prData.Number)
		case "CHANGES_REQUESTED":
			msg = fmt.Sprintf("PR #%d open; changes requested", prData.Number)
		default:
			msg = fmt.Sprintf("PR #%d open (%s)", prData.Number, prData.ReviewDecision)
		}
	case "MERGED":
		msg = fmt.Sprintf("PR #%d merged; ready to close workspace", prData.Number)
	case "CLOSED":
		msg = fmt.Sprintf("PR #%d closed without merging", prData.Number)
	default:
		msg = fmt.Sprintf("PR #%d (%s)", prData.Number, prData.State)
	}

	// Append checks rollup so the badge renderer can show a separate
	// "checks: passing/failing/pending" pill alongside the PR-state
	// badge. Only meaningful for OPEN PRs — merged/closed PRs land in
	// a terminal state and check status is moot.
	if prData.State == "OPEN" && len(prData.StatusCheckRollup) > 0 {
		if rollup := summarizeChecks(prData.StatusCheckRollup); rollup != "" {
			msg = msg + " · " + rollup
		}
	}

	return &state.Hint{
		Kind:       "pr_status",
		Message:    msg,
		DetectedAt: time.Now(),
	}
}

// summarizeChecks collapses a per-check array into a single rollup
// label for badge rendering. Precedence: any failing check wins,
// then any pending/running check, else passing. Empty when no check
// has a recognizable state (e.g., very early in CI before any check
// has reported).
func summarizeChecks(checks []struct {
	Conclusion string `json:"conclusion,omitempty"`
	Status     string `json:"status,omitempty"`
	State      string `json:"state,omitempty"`
}) string {
	hasFailing, hasPending, hasPassing := false, false, false
	for _, c := range checks {
		// Normalize across both check shapes (CI vs commit-status).
		switch {
		case c.Conclusion == "FAILURE", c.Conclusion == "TIMED_OUT", c.Conclusion == "CANCELLED",
			c.State == "FAILURE", c.State == "ERROR":
			hasFailing = true
		case c.Conclusion == "SUCCESS", c.State == "SUCCESS":
			hasPassing = true
		case c.Status == "IN_PROGRESS", c.Status == "QUEUED", c.Status == "PENDING",
			c.State == "PENDING":
			hasPending = true
		}
	}
	switch {
	case hasFailing:
		return "checks failing"
	case hasPending:
		return "checks running"
	case hasPassing:
		return "checks passing"
	}
	return ""
}

// ghAvailable returns true when the gh binary is on PATH. Logs the
// "gh missing" warning once per session, then silently returns false
// on every subsequent call.
func ghAvailable() bool {
	if _, err := exec.LookPath("gh"); err != nil {
		ghMissingMu.Lock()
		if !ghMissingLogged {
			log.Info("lifecycle.pr_status: gh CLI not on PATH; pr_status detector disabled this session")
			ghMissingLogged = true
		}
		ghMissingMu.Unlock()
		return false
	}
	return true
}

// repoFromProjectRoot returns "<owner>/<repo>" for use with `gh pr view
// --repo`. We could parse `git remote get-url origin` ourselves, but gh
// already does this if we omit --repo (it walks up from cwd). Returning
// "" here lets gh handle it. Kept as a function for future expansion
// (e.g., respecting a CANOPY_GH_REPO env var override).
func repoFromProjectRoot(projectRoot string) string {
	_ = projectRoot
	return "" // empty → gh resolves from cwd (set via cmd.Dir)
}

// PRStatusCacheSize returns the number of entries currently in the
// pr_status cache. Exposed for tests + diagnostics; production code
// should not branch on this.
func PRStatusCacheSize() int {
	prStatusMu.Lock()
	defer prStatusMu.Unlock()
	return len(prStatusCache)
}

// ResetPRStatusCache wipes the cache. Called by the TUI's `r` (refresh)
// action — pressing `r` is the user saying "I want truth now," not
// "respect the 10-minute TTL." Without this, a user who pushed a PR
// review or merged a PR would see stale "awaiting review" / "open"
// hints for up to 10 minutes after refreshing. Same intent as
// memCache.InvalidateAll() in the same `r` handler. Mutex-protected.
//
// Background ticks and reconcile still hit the cache (they're not the
// user explicitly demanding fresh data), so the GitHub API budget
// argument that motivated the 10-min TTL still holds for ambient
// loads — only deliberate user action busts.
func ResetPRStatusCache() {
	prStatusMu.Lock()
	prStatusCache = map[string]prStatusEntry{}
	prStatusMu.Unlock()

	ghMissingMu.Lock()
	ghMissingLogged = false
	ghMissingMu.Unlock()
}
