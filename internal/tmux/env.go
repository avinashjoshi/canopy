package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// SetSessionEnv sets a per-session environment variable on the named tmux
// session via `tmux set-environment -t <session> KEY VALUE`. The variable
// is scoped to this session only (not the whole server), so other
// sessions on the same tmux server stay unmarked.
//
// Why per-session and not server-global (-g): canopy's mosh-attach flow
// sets CANOPY_REMOTE_HOST=tower so the remote canopy's statusline can
// render a "you are attached to tower" pill. But the same remote host
// might run local-only canopy sessions (a user physically logged in)
// that should NOT be marked. Per-session scope keeps local sessions
// unmarked while remote-attached sessions inherit the marker.
//
// Without this call, setting CANOPY_REMOTE_HOST via `export` in the
// mosh remote bash one-liner is insufficient: tmux's server env is
// frozen at server-startup time. An already-running tmux server on
// the remote won't pick up the export, so statusline subprocesses
// the server spawns won't see the var. Set-environment overrides the
// stored env for the named session regardless of when the server
// started.
//
// Returns nil when session doesn't exist (tmux errors with code 1)
// since the caller's intent is "if this session is around, tag it" —
// missing session means nothing to tag, not a failure mode worth
// propagating. All other tmux errors propagate so the caller can log.
func (c *Client) SetSessionEnv(ctx context.Context, session, key, value string) error {
	if session == "" {
		return fmt.Errorf("tmux.SetSessionEnv: empty session")
	}
	if key == "" {
		return fmt.Errorf("tmux.SetSessionEnv: empty key")
	}
	cmd := exec.CommandContext(ctx, "tmux", c.args("set-environment", "-t", session, key, value)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// Session doesn't exist (or server not running). Caller's
			// intent is conditional tagging, not a hard requirement.
			return nil
		}
		return fmt.Errorf("tmux.SetSessionEnv(%s, %s): %w (stderr: %s)",
			session, key, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// UnsetSessionEnv removes a per-session environment variable from the
// named tmux session via `tmux set-environment -u -t <session> KEY`.
// Subprocesses spawned by that session after this call no longer see
// the key in their environment.
//
// Why we need this distinct from `SetSessionEnv(session, key, "")`:
// setting a var to empty string is observably different from unsetting
// it (one is "set to empty"; the other removes the key entirely).
// Statusline subprocesses test `os.Getenv("CANOPY_REMOTE_HOST") == ""`
// which behaves the same for both, but `tmux show-environment` differs:
// set-to-empty shows the key with empty value; unset shows
// `-CANOPY_REMOTE_HOST` (the leading `-` is tmux's convention for "this
// is explicitly cleared"). The unset path keeps the session env clean
// instead of accumulating cleared-but-still-present keys across attaches.
//
// Use case: a remote attach tagged the session with `CANOPY_REMOTE_HOST=tower`.
// Later, a local attach to the same session must clear the tag so the
// statusline stops rendering a stale "@tower" pill that misrepresents
// the connection. Without this call, the previous session-env override
// would persist for the life of the tmux server.
//
// Same error contract as SetSessionEnv: missing session swallows
// (returns nil), all other tmux errors propagate.
func (c *Client) UnsetSessionEnv(ctx context.Context, session, key string) error {
	if session == "" {
		return fmt.Errorf("tmux.UnsetSessionEnv: empty session")
	}
	if key == "" {
		return fmt.Errorf("tmux.UnsetSessionEnv: empty key")
	}
	cmd := exec.CommandContext(ctx, "tmux", c.args("set-environment", "-u", "-t", session, key)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("tmux.UnsetSessionEnv(%s, %s): %w (stderr: %s)",
			session, key, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
