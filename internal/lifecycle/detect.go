// Package lifecycle owns canopy's v0.6 lifecycle detectors. Detectors
// are pure functions that read a workspace's git/gh state and return at
// most one Hint per kind. They're called from the TUI refresh loop and
// from canopy reconcile to surface "what's the next action" for each
// workspace (rename your branch, the PR is open, you can close out, ...).
//
// Hints are NOT persisted in state.json — they're recomputed on every
// run because persisting risks staleness after a manual git operation
// outside canopy. This is the deliberate design call from the v0.6 CEO
// review: derived data doesn't get persisted; only canonical state does.
//
// Detector cost classes:
//
//   - local (RunFast): rename_suggested, shipped, pr_status. The first
//     two are pure-local git reads (rev-list, merge-base). pr_status
//     shells out to gh but is cached 10min in-memory per (project, branch),
//     so the worst case is one gh call per workspace per 10min — well
//     inside the GitHub API budget. Cache key includes the current branch
//     so a `git branch -m` invalidates cleanly via mismatched key.
//
// All three run on every TUI refresh + every reconcile. Earlier versions
// gated pr_status behind a manual `r` keystroke to "save the API budget,"
// but the cache makes that safety unnecessary and the gating produced a
// confusing UX where local "shipped" appeared before authoritative PR
// state. The PR state is the better signal for any branch with a PR;
// the local "shipped" detector is a fallback for purely-local work.
package lifecycle

import (
	"context"
	"sync"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/state"
)

var log = clog.Pkg("lifecycle")

// RunFast runs all detectors in parallel and returns their aggregated
// hints. Safe to call on every TUI refresh — pr_status is cached 10min
// so the worst-case wall time is dominated by the slowest local git
// command (typically <50ms). On a cache miss for pr_status, the gh
// call adds ~200-500ms to the slowest goroutine, but that only happens
// once per branch per 10min.
//
// Returned slice is unordered; callers that need stable ordering should
// sort by Kind. Hints with Kind="" are treated as "no hint" (filtered).
func RunFast(ctx context.Context, ws state.Workspace) []state.Hint {
	const detectorCount = 4
	type result struct{ h *state.Hint }
	results := make(chan result, detectorCount)

	var wg sync.WaitGroup
	wg.Add(detectorCount)

	// rename_suggested: the agent should rename the branch once it
	// understands the feature's intent.
	go func() {
		defer wg.Done()
		results <- result{h: detectRenameSuggested(ctx, ws)}
	}()

	// shipped: branch squash-merged into the default branch. Surfaces
	// the "you can rm this" signal. PR status supersedes this when
	// present (the badge renderer hides shipped under PR).
	go func() {
		defer wg.Done()
		results <- result{h: detectShipped(ctx, ws)}
	}()

	// pr_status: PR state from gh. Cached 10min per branch so the gh
	// call is amortized.
	go func() {
		defer wg.Done()
		results <- result{h: detectPRStatus(ctx, ws)}
	}()

	// git_stats: ahead/behind/dirty counts. Surfaces every refresh so
	// the user sees in-flight work at a glance regardless of PR state.
	go func() {
		defer wg.Done()
		results <- result{h: detectGitStats(ctx, ws)}
	}()

	wg.Wait()
	close(results)

	out := make([]state.Hint, 0, detectorCount)
	for r := range results {
		if r.h != nil {
			out = append(out, *r.h)
		}
	}
	return out
}

// All is RunFast. Kept as a separate name so canopy reconcile callers
// stay readable ("run all detectors"); both surfaces share the same
// detector set now that pr_status is part of RunFast.
func All(ctx context.Context, ws state.Workspace) []state.Hint {
	return RunFast(ctx, ws)
}
