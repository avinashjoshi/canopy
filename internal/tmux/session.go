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
	"os/exec"
	"strings"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("tmux")

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

// Create starts a new detached tmux session named name with the working
// directory cwd. The session has one default pane running the user's shell;
// callers that want a richer pane layout (canopy's standard 4-pane workspace)
// should call this then issue follow-up tmux commands. That orchestration
// lives in internal/workspace, not here.
//
// Returns ErrSessionExists if a session with that name is already alive.
func (c *Client) Create(ctx context.Context, name, cwd string) error {
	log.Info("tmux.create", "name", name, "cwd", cwd)

	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return fmt.Errorf("tmux.Create(%s): %w", name, err)
	}
	if exists {
		return fmt.Errorf("tmux.Create(%s): %w", name, ErrSessionExists)
	}

	cmd := exec.CommandContext(ctx, "tmux", c.args("new-session", "-d", "-s", name, "-c", cwd)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.Create(%s): %w (stderr: %s)", name, err, strings.TrimSpace(stderr.String()))
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

// Attach hands the current terminal off to a tmux attach. In v0 step 2 this
// is a stub that logs and returns nil — the real handoff (tea.ExecProcess
// from inside the Bubbletea program) lands in step 6b.
//
// TODO(step 6b): wire tea.ExecProcess. This stub exists so callers can
// already depend on the package shape during steps 3-5.
func (c *Client) Attach(ctx context.Context, name string) error {
	log.Info("tmux.attach (stub)", "name", name)
	return nil
}

// KillServer shuts down the tmux server bound to this client's socket.
// Used by tests to clean up in t.Cleanup; rarely useful in production.
func (c *Client) KillServer(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "tmux", c.args("kill-server")...)
	// kill-server exits non-zero if no server was running; treat that as success.
	_ = cmd.Run()
	return nil
}
