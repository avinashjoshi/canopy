package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/avinashjoshi/canopy/internal/tmux"
)

// initpromptTestSocket is a distinct tmux -L socket name so these tests
// don't collide with the user's real tmux server or with other packages'
// test sockets (workspace_test's tests use "canopy-test-workspace").
const initpromptTestSocket = "canopy-test-initprompt"

func TestErrPromptFailed_ErrorsAs(t *testing.T) {
	// Verifies the sentinel works with errors.As — cmd/canopy/main.go's
	// exit-code-2 branch and TUI dispatch both depend on this.
	var inner error = &ErrPromptFailed{Reason: "test reason"}
	wrapped := errors.New("not a prompt failure")

	var pf *ErrPromptFailed
	if !errors.As(inner, &pf) {
		t.Error("errors.As on direct *ErrPromptFailed = false, want true")
	}
	if pf.Reason != "test reason" {
		t.Errorf("Reason after As = %q, want %q", pf.Reason, "test reason")
	}

	// Negative: a different error should NOT match.
	pf = nil
	if errors.As(wrapped, &pf) {
		t.Error("errors.As on plain error = true, want false")
	}
}

func TestErrPromptFailed_ErrorMessageFormat(t *testing.T) {
	e := &ErrPromptFailed{Reason: "test reason here"}
	want := "prompt not sent: test reason here"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestIsPromptFailed_DirectAndWrapped(t *testing.T) {
	// Direct match.
	direct := &ErrPromptFailed{Reason: "direct"}
	pf, ok := IsPromptFailed(direct)
	if !ok {
		t.Error("IsPromptFailed(direct) = false, want true")
	}
	if pf == nil || pf.Reason != "direct" {
		t.Errorf("IsPromptFailed(direct) returned %+v, want Reason=direct", pf)
	}

	// Wrapped via fmt.Errorf %w — errors.As traverses the chain.
	wrapped := fmt.Errorf("outer: %w", &ErrPromptFailed{Reason: "inner"})
	pf, ok = IsPromptFailed(wrapped)
	if !ok {
		t.Error("IsPromptFailed(wrapped) = false, want true (errors.As should traverse %w)")
	}
	if pf == nil || pf.Reason != "inner" {
		t.Errorf("IsPromptFailed(wrapped) returned %+v, want Reason=inner", pf)
	}

	// Negative: a plain error is NOT a prompt failure.
	plain := errors.New("nope")
	if pf, ok := IsPromptFailed(plain); ok {
		t.Errorf("IsPromptFailed(plain) = true (%+v), want false", pf)
	}

	// nil: defensively, IsPromptFailed(nil) should be (nil, false).
	if pf, ok := IsPromptFailed(nil); ok || pf != nil {
		t.Errorf("IsPromptFailed(nil) = (%+v, %v), want (nil, false)", pf, ok)
	}
}

// TestPromptPhaseBudget covers the 3-tier resolution order: explicit
// override > remote-dispatch default > local default. Pure function, no
// tmux needed.
func TestPromptPhaseBudget(t *testing.T) {
	tests := []struct {
		name          string
		overrideEnv   string // CANOPY_PROMPT_PHASE_BUDGET
		remoteDispEnv string // CANOPY_REMOTE_DISPATCH
		want          time.Duration
	}{
		{
			name:        "explicit override wins",
			overrideEnv: "15s",
			want:        15 * time.Second,
		},
		{
			name:          "explicit override wins even when remote-dispatch is also set",
			overrideEnv:   "1s",
			remoteDispEnv: "1",
			want:          1 * time.Second,
		},
		{
			name:        "malformed override falls through to next tier",
			overrideEnv: "not-a-duration",
			want:        5 * time.Second,
		},
		{
			// Distinguishes "malformed falls through to the NEXT tier"
			// from "malformed always jumps straight to the local
			// default" — both existing malformed-override cases happen
			// to land on 5s either way, so neither proves the actual
			// tier-2-before-tier-3 fallthrough order on its own.
			name:          "malformed override falls through to remote-dispatch tier, not past it",
			overrideEnv:   "garbage",
			remoteDispEnv: "1",
			want:          15 * time.Second,
		},
		{
			// time.ParseDuration happily accepts "0s" — it's not
			// "malformed" to it — but a zero budget would skip
			// awaitPaneOutput's poll loop entirely. Zero must fall
			// through like a malformed value, not be honored.
			name:        "zero override falls through to next tier",
			overrideEnv: "0s",
			want:        5 * time.Second,
		},
		{
			name:        "negative override falls through to next tier",
			overrideEnv: "-1s",
			want:        5 * time.Second,
		},
		{
			name:          "remote-dispatch set, no override, uses 15s default",
			remoteDispEnv: "1",
			want:          15 * time.Second,
		},
		{
			name: "neither set, uses 5s local default",
			want: 5 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CANOPY_PROMPT_PHASE_BUDGET", tt.overrideEnv)
			t.Setenv("CANOPY_REMOTE_DISPATCH", tt.remoteDispEnv)
			if got := promptPhaseBudget(); got != tt.want {
				t.Errorf("promptPhaseBudget() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestAwaitPaneOutput_MatchBeforeBudget: the match condition appears
// partway through the budget (pane runs `sleep 0.3; echo READY; sleep 5`)
// — awaitPaneOutput should return the captured text containing it well
// before the budget expires.
func TestAwaitPaneOutput_MatchBeforeBudget(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()

	paneID, err := c.Create(ctx, "await-match", t.TempDir(), "sleep 0.3; echo READY; sleep 5")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	start := time.Now()
	captured, err := awaitPaneOutput(ctx, c, paneID, 3*time.Second, 100*time.Millisecond, io.Discard,
		"waiting", "timed out", func(s string) bool { return strings.Contains(s, "READY") })
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("awaitPaneOutput: unexpected error: %v", err)
	}
	if !strings.Contains(captured, "READY") {
		t.Errorf("captured text %q does not contain READY", captured)
	}
	if elapsed > 2*time.Second {
		t.Errorf("awaitPaneOutput took %s; expected to return shortly after the 0.3s sleep, well under the 3s budget", elapsed)
	}
}

// TestAwaitPaneOutput_TimesOutWhenMatchNeverAppears: the match condition
// never becomes true within a short budget — awaitPaneOutput must give up
// and return *ErrPromptFailed with the supplied timeoutMsg.
func TestAwaitPaneOutput_TimesOutWhenMatchNeverAppears(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()

	paneID, err := c.Create(ctx, "await-timeout", t.TempDir(), "sleep 5")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = awaitPaneOutput(ctx, c, paneID, 1*time.Second, 100*time.Millisecond, io.Discard,
		"waiting", "custom timeout reason", func(s string) bool { return strings.Contains(s, "NEVER") })

	pf, ok := IsPromptFailed(err)
	if !ok {
		t.Fatalf("awaitPaneOutput: err = %v, want *ErrPromptFailed", err)
	}
	if pf.Reason != "custom timeout reason" {
		t.Errorf("Reason = %q, want %q", pf.Reason, "custom timeout reason")
	}
}

// TestAwaitPaneOutput_ToleratesTransientCaptureFailures: once the pane's
// sole process exits, tmux tears down the pane (and, since it's the
// only pane in its session, the whole server) out from under the poll
// loop, so capturePaneTimeout starts erroring mid-poll. awaitPaneOutput
// must tolerate that (keep retrying, not panic or return early) and
// still reach the timeout path on budget exhaustion.
func TestAwaitPaneOutput_ToleratesTransientCaptureFailures(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()

	// Sole pane in its session; process exits at ~200ms, tearing the
	// session (and this socket's server) down under the poll loop.
	paneID, err := c.Create(ctx, "await-transient-failure", t.TempDir(), "sleep 0.2; exit")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	start := time.Now()
	_, err = awaitPaneOutput(ctx, c, paneID, 800*time.Millisecond, 150*time.Millisecond, io.Discard,
		"waiting", "never matches, pane died mid-poll", func(s string) bool { return false })
	elapsed := time.Since(start)

	pf, ok := IsPromptFailed(err)
	if !ok {
		t.Fatalf("awaitPaneOutput: err = %v (elapsed %s), want *ErrPromptFailed — capture failures after pane death must not panic or return early", err, elapsed)
	}
	if pf.Reason != "never matches, pane died mid-poll" {
		t.Errorf("Reason = %q, want %q", pf.Reason, "never matches, pane died mid-poll")
	}
	// Must have kept polling through the capture failures out to
	// roughly the full budget, not bailed out early the moment capture
	// started erroring.
	if elapsed < 600*time.Millisecond {
		t.Errorf("awaitPaneOutput returned after only %s; expected it to keep retrying through capture failures out to ~800ms budget", elapsed)
	}
}

// TestAwaitPaneOutput_ContextCancelledReturnsPromptly: cancelling ctx
// mid-poll must return promptly rather than waiting out the full budget.
func TestAwaitPaneOutput_ContextCancelledReturnsPromptly(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	baseCtx := context.Background()

	paneID, err := c.Create(baseCtx, "await-cancel", t.TempDir(), "sleep 5")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithCancel(baseCtx)
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = awaitPaneOutput(ctx, c, paneID, 5*time.Second, 100*time.Millisecond, io.Discard,
		"waiting", "should not reach timeout", func(s string) bool { return false })
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("awaitPaneOutput took %s after cancel at ~200ms; expected a prompt return well under the 5s budget", elapsed)
	}
}

// TestAwaitClaudeReady_ImmediatelyReady covers Phase 1's "ready, skip
// trust entirely" branch: the pane already shows a claude-idle marker
// on the very first capture.
func TestAwaitClaudeReady_ImmediatelyReady(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	t.Setenv("CANOPY_PROMPT_PHASE_BUDGET", "3s")
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()

	// IsClaudeRendering only looks at the bottom 12 lines of the capture
	// (by design — see its doc comment), so the marker needs enough
	// preceding filler to scroll past the pane's default height and
	// actually land at the bottom of the viewport, matching how a real,
	// screen-filling claude UI behaves.
	paneID, err := c.Create(ctx, "ready-immediately", t.TempDir(),
		`for i in $(seq 1 60); do echo line$i; done; echo 'Welcome back'; sleep 5`)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := awaitClaudeReady(ctx, c, paneID, io.Discard); err != nil {
		t.Fatalf("awaitClaudeReady: unexpected error: %v", err)
	}
}

// TestAwaitClaudeReady_TrustDialogThenReady covers Phase 1's "trust
// dialog matched, dismiss with Enter, advance to Phase 2" branch, and
// Phase 2 itself: the pane shows the trust dialog, blocks on `read`
// until awaitClaudeReady's SendKeyName(Enter) dismisses it, then prints
// a claude-idle marker.
func TestAwaitClaudeReady_TrustDialogThenReady(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	t.Setenv("CANOPY_PROMPT_PHASE_BUDGET", "3s")
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()

	// IsTrustDialog matches anywhere in the capture, but the post-dismiss
	// IsClaudeRendering check only looks at the bottom 12 lines (see the
	// comment on the sibling test above) — pad with filler so "Welcome
	// back" actually lands at the bottom of the viewport.
	paneID, err := c.Create(ctx, "trust-then-ready", t.TempDir(),
		`echo 'Yes, I trust this folder'; read _; sleep 0.2; for i in $(seq 1 60); do echo line$i; done; echo 'Welcome back'; sleep 5`)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := awaitClaudeReady(ctx, c, paneID, io.Discard); err != nil {
		t.Fatalf("awaitClaudeReady: unexpected error: %v", err)
	}
}

// TestAwaitPaneOutput_ContextAlreadyDoneAtStart covers the ctx.Err()
// check at the TOP of the loop (distinct from the mid-loop select's
// ctx.Done() case, which TestAwaitPaneOutput_ContextCancelledReturnsPromptly
// covers): ctx is already cancelled before awaitPaneOutput is even
// called, so the very first loop iteration must return immediately
// without attempting a capture.
func TestAwaitPaneOutput_ContextAlreadyDoneAtStart(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	baseCtx := context.Background()

	paneID, err := c.Create(baseCtx, "await-already-cancelled", t.TempDir(), "sleep 5")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithCancel(baseCtx)
	cancel() // already done before awaitPaneOutput is ever called

	start := time.Now()
	_, err = awaitPaneOutput(ctx, c, paneID, 5*time.Second, 100*time.Millisecond, io.Discard,
		"waiting", "should not reach timeout", func(s string) bool { return false })
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("awaitPaneOutput took %s with an already-cancelled ctx; expected an immediate return", elapsed)
	}
}

// TestAwaitPaneOutput_GracePeriodCatchesLateMatch is the direct
// regression test for the "one more capture right at the deadline"
// guard: the match condition renders AFTER the last in-loop capture
// but BEFORE the post-loop grace-period capture, so only the grace
// check — not the main loop — observes it.
//
// Timing: budget=600ms, pollInterval=200ms. In-loop captures land at
// approximately t=0, 200, 400ms; the loop then sleeps to t=600ms,
// finds the deadline passed, and exits WITHOUT capturing again. The
// pane prints its marker at t=500ms — after the t=400ms capture missed
// it, before the ~t=600ms+ grace capture sees it.
func TestAwaitPaneOutput_GracePeriodCatchesLateMatch(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()

	paneID, err := c.Create(ctx, "await-grace-period", t.TempDir(), "sleep 0.5; echo GRACEMATCH; sleep 5")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	captured, err := awaitPaneOutput(ctx, c, paneID, 600*time.Millisecond, 200*time.Millisecond, io.Discard,
		"waiting", "should have been caught by the grace check", func(s string) bool { return strings.Contains(s, "GRACEMATCH") })

	if err != nil {
		t.Fatalf("awaitPaneOutput: unexpected error (grace-period check should have caught the late match): %v", err)
	}
	if !strings.Contains(captured, "GRACEMATCH") {
		t.Errorf("captured text %q does not contain GRACEMATCH", captured)
	}
}

// TestAwaitClaudeReady_Phase1TimesOut: the pane never shows the trust
// dialog or a claude-ready marker within the budget — awaitClaudeReady
// must propagate Phase 1's timeout error, not silently succeed or hang.
func TestAwaitClaudeReady_Phase1TimesOut(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	t.Setenv("CANOPY_PROMPT_PHASE_BUDGET", "500ms")
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()

	paneID, err := c.Create(ctx, "phase1-timeout", t.TempDir(), "sleep 5")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = awaitClaudeReady(ctx, c, paneID, io.Discard)
	pf, ok := IsPromptFailed(err)
	if !ok {
		t.Fatalf("awaitClaudeReady: err = %v, want *ErrPromptFailed", err)
	}
	if !strings.HasPrefix(pf.Reason, "Phase 1 timeout") {
		t.Errorf("Reason = %q, want prefix %q", pf.Reason, "Phase 1 timeout")
	}
}

// TestAwaitClaudeReady_Phase2TimesOut: the trust dialog appears and
// gets dismissed successfully (Phase 1 succeeds), but no claude-ready
// marker ever appears afterward — awaitClaudeReady must propagate
// Phase 2's timeout error specifically, not Phase 1's.
func TestAwaitClaudeReady_Phase2TimesOut(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping integration test")
	}
	t.Setenv("CANOPY_PROMPT_PHASE_BUDGET", "500ms")
	c := tmux.WithSocket(initpromptTestSocket)
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })
	ctx := context.Background()

	// Dismiss succeeds (the `read` consumes awaitClaudeReady's Enter),
	// but nothing claude-shaped ever renders afterward.
	paneID, err := c.Create(ctx, "phase2-timeout", t.TempDir(),
		`echo 'Yes, I trust this folder'; read _; sleep 5`)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = awaitClaudeReady(ctx, c, paneID, io.Discard)
	pf, ok := IsPromptFailed(err)
	if !ok {
		t.Fatalf("awaitClaudeReady: err = %v, want *ErrPromptFailed", err)
	}
	if !strings.HasPrefix(pf.Reason, "Phase 2 timeout") {
		t.Errorf("Reason = %q, want prefix %q", pf.Reason, "Phase 2 timeout")
	}
}
