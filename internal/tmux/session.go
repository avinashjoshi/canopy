// Package tmux wraps the subset of tmux commands canopy needs to manage
// per-workspace sessions.
//
// All operations go through a Client which optionally holds a named tmux
// socket (`tmux -L <name>`). Production code uses Client.New(), which talks
// to the user's default tmux server. Tests use Client.WithSocket("name") to
// scope to an isolated socket so they don't pollute the user's tmux state.
//
// This package is a leaf primitive: it knows how to start/stop/check tmux
// sessions, but not how canopy composes them into a 4-pane workspace
// layout. That orchestration lives in internal/workspace.
package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("tmux")

// SafeName turns an arbitrary identifier into one safe for use as a tmux
// session or window name. tmux's target syntax uses `:` and `.` as
// separators (`session:window.pane`), so neither character can appear
// unescaped in a name without breaking every subsequent target lookup.
//
// This is stricter than git.Sanitize — git allows dots in branch names
// (v1.2.3 stays v1.2.3) but tmux can't have them. Other unsafe characters
// collapse to a single `-`; alphanumerics, underscore, and hyphen pass
// through. Leading/trailing hyphens are trimmed.
//
//	SafeName("v1.2.3")            -> "v1-2-3"
//	SafeName("avi.tools")         -> "avi-tools"
//	SafeName("feature/oauth")     -> "feature-oauth"
//	SafeName("tmp.X-feat")        -> "tmp-X-feat"
func SafeName(s string) string {
	var b []byte
	prevDash := false
	for _, r := range s {
		safe := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-'
		if safe {
			b = append(b, byte(r))
			prevDash = (r == '-')
			continue
		}
		// Collapse runs of unsafe chars into a single hyphen.
		if !prevDash {
			b = append(b, '-')
			prevDash = true
		}
	}
	// Trim leading/trailing hyphens.
	start, end := 0, len(b)
	for start < end && b[start] == '-' {
		start++
	}
	for end > start && b[end-1] == '-' {
		end--
	}
	return string(b[start:end])
}

// Sentinel errors. Callers use errors.Is to distinguish "this is the
// 'doesn't exist' case" from genuine failures.
var (
	// ErrSessionExists is returned when Create is called for a session name
	// that's already alive on the server.
	ErrSessionExists = errors.New("tmux: session already exists")

	// ErrSessionNotFound is returned by Kill when the session isn't on the
	// server. Reconciliation treats this as a no-op.
	ErrSessionNotFound = errors.New("tmux: session not found")
)

// Client is a thin wrapper around the tmux CLI. The zero value is invalid;
// use New() or WithSocket() to construct one.
type Client struct {
	// socket is the tmux socket name passed via `tmux -L`. Empty means use
	// the user's default tmux server (no -L flag).
	socket string
}

// New returns a client that talks to the user's default tmux server.
func New() *Client { return &Client{} }

// WithSocket returns a client scoped to the named tmux socket. The socket
// is created on first use; tmux's own server-per-socket model means
// canopy-test sessions can't collide with the user's running tmux.
//
// Test code should always use WithSocket, never New.
func WithSocket(name string) *Client { return &Client{socket: name} }

// args prepends the -L flag if the client has a custom socket, then appends
// rest. Internal helper to keep the per-method exec.Command call sites short.
func (c *Client) args(rest ...string) []string {
	if c.socket == "" {
		return rest
	}
	out := make([]string, 0, len(rest)+2)
	out = append(out, "-L", c.socket)
	out = append(out, rest...)
	return out
}

// HasSession returns true if a session named name is alive on the server.
//
// tmux's exit codes here: 0 when the session exists, 1 when it doesn't (or
// when the server isn't running yet — both states map to "no session"
// from canopy's point of view).
func (c *Client) HasSession(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, "tmux", c.args("has-session", "-t", name)...)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("tmux.HasSession(%s): %w", name, err)
}

// Create starts a new detached tmux session named name with cwd as the
// initial working directory. If shellCmd is non-empty, the first pane runs
// "sh -c <shellCmd>" so multi-arg shell expressions work (e.g.
// "rm -rf .overmind.sock && bin/dev"); otherwise the pane runs the user's
// default shell.
//
// Returns ErrSessionExists if a session with that name is already alive.
//
// Callers that want canopy's standard 4-pane workspace layout call this
// to seed the session, then SplitPane for each additional pane. That
// orchestration lives in internal/workspace, not here.
//
// env contains "KEY=VALUE" entries that set session-level environment
// variables (via tmux's -e flag), inherited by every pane in the session,
// including future panes the user creates with prefix-c. Use this for
// CANOPY_PORT and friends so commands typed in the shell pane (like
// `bin/dev`) can read them.
func (c *Client) Create(ctx context.Context, name, cwd, shellCmd string, env ...string) error {
	log.Info("tmux.create", "name", name, "cwd", cwd, "cmd", shellCmd, "env_count", len(env))

	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return fmt.Errorf("tmux.Create(%s): %w", name, err)
	}
	if exists {
		return fmt.Errorf("tmux.Create(%s): %w", name, ErrSessionExists)
	}

	args := c.args("new-session", "-d", "-s", name, "-c", cwd)
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	if shellCmd != "" {
		// `sh -c "<expr>"` so any shell metachars (&&, |, $VAR) work.
		// Single-command cases (just "nvim") run via sh too; the extra
		// process is microseconds and not worth the API split.
		args = append(args, "sh", "-c", shellCmd)
	}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.Create(%s): %w (stderr: %s)", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SplitDirection picks how SplitPane carves up the target pane. tmux uses
// "-h" for a side-by-side (horizontal split = vertical divider line) and
// "-v" for stacked (vertical split = horizontal divider line). The naming
// has historically tripped people up; the constants below match tmux.
type SplitDirection string

const (
	// SplitHorizontal places the new pane to the RIGHT of target (vertical line).
	SplitHorizontal SplitDirection = "-h"

	// SplitVertical places the new pane BELOW target (horizontal line).
	SplitVertical SplitDirection = "-v"
)

// SplitPane creates a new pane by splitting the session's *active* pane.
// We target the session by name (`-t session`) rather than a specific
// pane index because window/pane base indices are user-configurable
// (many configs set `base-index 1`), and the orchestrator always wants
// to split the most recently created pane anyway — that's always the
// active one immediately after a previous split.
//
// cwd becomes the new pane's working directory; shellCmd is run via
// sh -c (or the default shell if empty), same semantics as Create.
//
// sizePercent is variadic for backward-compat — pass nothing for an
// even split (default 50/50), or pass a single integer 1-99 to size
// the NEW pane to that percentage of the parent. Used for the tdl-style
// layout where the bottom shell is 15% of the window and the right-side
// AI pane is 30% of the top.
//
// Layout note: chained splits produce a tree, not a balanced grid.
// SelectLayout can rearrange tiled grids, but for fixed proportional
// layouts (like tdl), use sizePercent on each split to set the geometry
// at creation time.
func (c *Client) SplitPane(ctx context.Context, session, cwd, shellCmd string, dir SplitDirection, sizePercent ...int) error {
	log.Info("tmux.split-pane", "session", session, "cwd", cwd, "cmd", shellCmd, "dir", dir)

	args := c.args("split-window", "-d", string(dir), "-t", session, "-c", cwd)
	if len(sizePercent) > 0 && sizePercent[0] > 0 && sizePercent[0] < 100 {
		// `-l <N>%` is the modern tmux size syntax (the deprecated form is
		// `-p <N>`). Sizes the NEW pane to N% of the parent pane.
		args = append(args, "-l", fmt.Sprintf("%d%%", sizePercent[0]))
	}
	if shellCmd != "" {
		args = append(args, "sh", "-c", shellCmd)
	}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.SplitPane(%s, %s): %w (stderr: %s)", session, dir, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SelectLayout applies a named tmux layout preset to the session's
// active window. The most useful preset for canopy's 4-pane workspace
// is "tiled", which arranges N panes in a clean grid regardless of
// split history. Other presets: "main-horizontal", "main-vertical",
// "even-horizontal", "even-vertical".
func (c *Client) SelectLayout(ctx context.Context, session, layout string) error {
	cmd := exec.CommandContext(ctx, "tmux", c.args("select-layout", "-t", session, layout)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.SelectLayout(%s, %s): %w (stderr: %s)", session, layout, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Kill terminates the named session. Returns ErrSessionNotFound if the
// session doesn't exist; reconciliation can ignore that.
func (c *Client) Kill(ctx context.Context, name string) error {
	log.Info("tmux.kill", "name", name)

	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return fmt.Errorf("tmux.Kill(%s): %w", name, err)
	}
	if !exists {
		return fmt.Errorf("tmux.Kill(%s): %w", name, ErrSessionNotFound)
	}

	cmd := exec.CommandContext(ctx, "tmux", c.args("kill-session", "-t", name)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.Kill(%s): %w (stderr: %s)", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Attach hands the current process off to `tmux attach -t <name>`. On
// success, syscall.Exec replaces the canopy process image with tmux —
// this function never returns. When the user detaches with prefix-d,
// they end up back at their original shell, not in canopy.
//
// This is the right shape for CLI subcommands (`canopy switch`,
// `canopy new` after setup completes). The Bubbletea TUI uses
// AttachCmd instead, which returns the prepared exec.Cmd so
// tea.ExecProcess can hand off + return control after detach.
//
// On failure (session doesn't exist, tmux missing), Attach returns an
// error and does NOT exec — the canopy process stays alive and can
// surface the error to the user.
func (c *Client) Attach(ctx context.Context, name string) error {
	log.Info("tmux.attach", "name", name)

	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return fmt.Errorf("tmux.Attach(%s): %w", name, err)
	}
	if !exists {
		return fmt.Errorf("tmux.Attach(%s): %w", name, ErrSessionNotFound)
	}

	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux.Attach: tmux not on PATH: %w", err)
	}

	args := []string{"tmux"}
	args = append(args, c.args("attach", "-t", name)...)
	return syscall.Exec(tmuxPath, args, os.Environ())
}

// AttachCmd returns a prepared exec.Cmd for `tmux attach -t <name>`
// without running it. The Bubbletea TUI passes this to tea.ExecProcess
// to hand the terminal to tmux temporarily; when the user detaches
// (prefix-d), tmux exits cleanly and Bubbletea reclaims the terminal
// to redraw the TUI.
//
// Pre-flight: returns ErrSessionNotFound if the session doesn't exist
// at call time. Caller should still handle exec errors from running
// the returned Cmd (terminal reset issues, tmux server crashed mid-attach).
func (c *Client) AttachCmd(ctx context.Context, name string) (*exec.Cmd, error) {
	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("tmux.AttachCmd(%s): %w", name, err)
	}
	if !exists {
		return nil, fmt.Errorf("tmux.AttachCmd(%s): %w", name, ErrSessionNotFound)
	}
	return exec.CommandContext(ctx, "tmux", c.args("attach", "-t", name)...), nil
}

// KillServer shuts down the tmux server bound to this client's socket.
// Used by tests to clean up in t.Cleanup; rarely useful in production.
func (c *Client) KillServer(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "tmux", c.args("kill-server")...)
	// kill-server exits non-zero if no server was running; treat that as success.
	_ = cmd.Run()
	return nil
}
