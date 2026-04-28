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
//   - cheap (RunFast): rename_suggested, shipped. Pure local git reads
//     (rev-list, merge-base). ~10ms total per workspace. Run on every
//     TUI refresh + every reconcile.
//   - expensive (RunPRStatus): pr_status. Shells out to gh, hits the
//     GitHub API. Cached 10min in-memory. Run on canopy reconcile +
//     manual `r` keystroke. NOT on every TUI refresh — would burn ~24%
//     of the user's GitHub API budget for a workspace-heavy power user.
//
// The split between RunFast and RunPRStatus is intentional. The TUI
// dispatches them as separate tea.Cmds so a slow gh call never blocks
// the cheap ones (a workspace's row appears with rename/shipped hints
// immediately; pr_status arrives a beat later).
package lifecycle

import (
	"context"
	"sync"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/state"
)

var log = clog.Pkg("lifecycle")

// RunFast runs the cheap, local-only detectors (rename_suggested + shipped)
// in parallel and returns their aggregated hints. Safe to call on every
// TUI refresh — total wall time is dominated by the slowest local git
// command, typically <50ms.
//
// Returned slice is unordered; callers that need stable ordering should
// sort by Kind. Hints with Kind="" are treated as "no hint" (filtered).
func RunFast(ctx context.Context, ws state.Workspace) []state.Hint {
	type result struct{ h *state.Hint }
	results := make(chan result, 2)

	var wg sync.WaitGroup
	wg.Add(2)

	// rename_suggested: the agent should rename the branch once it
	// understands the feature's intent.
	go func() {
		defer wg.Done()
		results <- result{h: detectRenameSuggested(ctx, ws)}
	}()

	// shipped: the branch is reachable from origin/<default>, ready to close.
	go func() {
		defer wg.Done()
		results <- result{h: detectShipped(ctx, ws)}
	}()

	wg.Wait()
	close(results)

	out := make([]state.Hint, 0, 2)
	for r := range results {
		if r.h != nil {
			out = append(out, *r.h)
		}
	}
	return out
}

// RunPRStatus runs the expensive pr_status detector for a single
// workspace. Result is cached internally (10min TTL); a cached hit
// returns instantly. Called from canopy reconcile + the TUI's manual
// `r` refresh, NOT from every TUI tick.
//
// Returns nil when:
//   - gh is missing or unauthed (silent skip; logs once per session)
//   - no PR exists for the branch
//   - rate-limited (uses cached value if available; nil otherwise)
//
// Returns a Hint when a PR exists and we know its state.
func RunPRStatus(ctx context.Context, ws state.Workspace) *state.Hint {
	return detectPRStatus(ctx, ws)
}

// All combines RunFast + RunPRStatus into a single call. Used by canopy
// reconcile (which is the explicit "do all the work" entry point) and
// by the TUI's manual `r` keystroke. Not used by the TUI's auto-refresh
// (which calls RunFast only).
func All(ctx context.Context, ws state.Workspace) []state.Hint {
	hints := RunFast(ctx, ws)
	if h := RunPRStatus(ctx, ws); h != nil {
		hints = append(hints, *h)
	}
	return hints
}
