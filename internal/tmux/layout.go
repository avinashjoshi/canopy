package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CaptureWindowLayout returns tmux's opaque window-layout descriptor for
// the active window of the given session. The string is what tmux's
// `select-layout` accepts: a checksumed, byte-precise serialization of
// every pane's pixel geometry within the window. Round-tripping it via
// SelectLayout restores the EXACT layout, not just the proportions.
//
// Used by canopy agent swap: capture before kill-pane → respawn the
// new agent pane → SelectLayout to restore. tmux's `split-window
// -l <N>%` takes a PERCENTAGE of the target pane, which means a naive
// kill-and-resplit drifts the IDE/terminal/agent geometry slightly each
// swap (the remaining panes redistribute when one is killed, so a
// 30% split off the IDE post-kill is not the same geometry as the
// original 30% split). Capturing and restoring the layout sidesteps
// that drift entirely.
//
// Returns the layout string for the session's ACTIVE window. Multi-
// window sessions are out of scope: canopy currently lays out one
// window per workspace.
func (c *Client) CaptureWindowLayout(ctx context.Context, session string) (string, error) {
	args := c.args("display-message", "-t", session, "-p", "#{window_layout}")
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux.CaptureWindowLayout(%s): %w (stderr: %s)",
			session, err, strings.TrimSpace(stderr.String()))
	}
	layout := strings.TrimSpace(stdout.String())
	if layout == "" {
		return "", fmt.Errorf("tmux.CaptureWindowLayout(%s): empty layout (session may not exist)", session)
	}
	return layout, nil
}

// SelectLayout for restoring a captured window_layout string lives in
// session.go alongside the other layout primitives. Callers that pass
// the output of CaptureWindowLayout straight to SelectLayout restore
// byte-precise geometry. Empty strings come from a swallowed
// CaptureWindowLayout error; SelectLayout treats them as a tmux error
// rather than a no-op (consistent with session.go's existing contract),
// so the caller should drop empty layouts before calling.

// KillPane terminates the pane with the given pane ID. Used by canopy
// agent swap to remove the existing agent pane before respawning the
// new agent in its place. Idempotent at the canopy semantics layer:
// if the pane is already gone (e.g., user manually killed it before
// the swap), tmux returns a "can't find pane" error which the swap
// path can choose to treat as fine.
//
// paneID must be the `%<digits>` form from a prior Create/SplitPane
// or LookupPane call. Invalid pane IDs are rejected by tmux with a
// clear error.
func (c *Client) KillPane(ctx context.Context, paneID string) error {
	if paneID == "" {
		return fmt.Errorf("tmux.KillPane: empty pane ID")
	}
	args := c.args("kill-pane", "-t", paneID)
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.KillPane(%s): %w (stderr: %s)",
			paneID, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
