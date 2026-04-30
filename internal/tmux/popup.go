package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrNoCurrentClient is returned by CurrentSession when tmux reports no
// client is attached to the server (e.g. running outside any tmux client,
// or the server has no sessions).
var ErrNoCurrentClient = errors.New("tmux: no current client")

// DisplayPopup runs `tmux display-popup -E -w 80% -h 80% [-d cwd] <command>`
// and blocks until the popup exits. The popup runs <command> as a
// shell-style argument; tmux invokes it via the user's shell.
//
// cwd, when non-empty, becomes the popup body's working directory via
// `-d`. Empty cwd lets tmux pick its default (the tmux server's cwd,
// which is rarely what callers want). Pass the calling pane's cwd
// (`#{pane_current_path}`) so subcommands like canopy popup-inner can
// walk up from there to find canopy.json.
//
// The -E flag means the popup closes when the command exits (zero or
// non-zero). The -w/-h flags size the popup to 80% of the host pane.
//
// Returns the underlying *exec.ExitError verbatim if tmux exits non-zero,
// so callers can distinguish "user closed the popup" from "tmux refused
// to run."
func (c *Client) DisplayPopup(ctx context.Context, command, cwd string) error {
	log.Info("tmux.display-popup", "command", command, "cwd", cwd)

	args := []string{"display-popup", "-E", "-w", "80%", "-h", "80%"}
	if cwd != "" {
		args = append(args, "-d", cwd)
	}
	args = append(args, command)
	cmd := exec.CommandContext(ctx, "tmux", c.args(args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.DisplayPopup: %w (stderr: %s)", err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// PaneCwd returns the working directory of the named pane, queried via
// `tmux display-message -p -t <pane> '#{pane_current_path}'`. Empty
// pane name targets the calling client's active pane. Use to resolve
// the cwd of a key-bind caller (TMUX_PANE env) before launching a popup
// that needs that cwd.
func (c *Client) PaneCwd(ctx context.Context, pane string) (string, error) {
	args := []string{"display-message", "-p"}
	if pane != "" {
		args = append(args, "-t", pane)
	}
	args = append(args, "#{pane_current_path}")
	cmd := exec.CommandContext(ctx, "tmux", c.args(args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux.PaneCwd(%q): %w (stderr: %s)", pane, err,
			strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// DetachClient detaches the current tmux client (the one identified by
// $TMUX in the calling environment). The popup uses this when the user
// deletes the workspace they're sitting in: instead of letting tmux
// auto-switch the client to some other session — or worse, auto-build
// the project main session as a parking spot — we just kick the client
// out of tmux entirely. The user lands back at whatever shell started
// tmux. Sessions other than the killed one are unaffected.
//
// Errors are surfaced verbatim; the typical caller wraps them as
// best-effort logs (the popup is closing anyway, no UI to surface to).
func (c *Client) DetachClient(ctx context.Context) error {
	log.Info("tmux.detach-client")

	args := c.args("detach-client")
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.DetachClient: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SwitchClient switches the current tmux client to the named target
// session, the equivalent of `<prefix>s`-then-pick. Used by canopy popup
// to flip from the popup's host client into a workspace's session.
//
// Returns ErrSessionNotFound (the existing sentinel from session.go) if
// tmux can't find a session by that name. The mapping: tmux exit code 1
// with "can't find session" stderr → ErrSessionNotFound.
func (c *Client) SwitchClient(ctx context.Context, target string) error {
	log.Info("tmux.switch-client", "target", target)

	// Detach any existing clients on the target session before our
	// client switches in. Solo-dev default — see detachOtherClients
	// in session.go for full rationale + opt-out env var.
	c.detachOtherClients(ctx, target, "switch-client")

	args := c.args("switch-client", "-t", target)
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		// tmux says "can't find session" when the target is missing.
		// Map to the sentinel so callers can refresh state and re-render.
		if strings.Contains(stderrStr, "can't find session") {
			return fmt.Errorf("tmux.SwitchClient(%s): %w", target, ErrSessionNotFound)
		}
		return fmt.Errorf("tmux.SwitchClient(%s): %w (stderr: %s)", target, err, stderrStr)
	}
	return nil
}

// CurrentSession returns the name of the session the *calling* tmux client
// is currently attached to. Implemented via `tmux display-message -p '#S'`.
//
// Returns ErrNoCurrentClient if tmux reports no client is attached. That
// happens in two cases: there is no tmux server running, or the caller is
// running outside any tmux client (e.g., a one-shot statusline invocation
// from a non-tmux context — which canopy guards against, but the wrapper
// still maps the error cleanly).
func (c *Client) CurrentSession(ctx context.Context) (string, error) {
	args := c.args("display-message", "-p", "#S")
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		// tmux signals "no client/server" three ways depending on state:
		//   - "no current client"      : server up but no attached client
		//   - "no server running"      : tmux subcommand explicitly checked
		//   - "error connecting to ..." : socket file missing (server never started)
		// All three map to ErrNoCurrentClient from canopy's POV.
		if strings.Contains(stderrStr, "no current client") ||
			strings.Contains(stderrStr, "no server running") ||
			strings.Contains(stderrStr, "error connecting to") {
			return "", ErrNoCurrentClient
		}
		return "", fmt.Errorf("tmux.CurrentSession: %w (stderr: %s)", err, stderrStr)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Version returns tmux's version string, e.g. "3.4" or "3.5a". Used by
// canopy popup to refuse pre-3.2 tmux (display-popup support landed in
// 3.2, October 2021).
//
// `tmux -V` prints "tmux 3.4\n" on stdout. We strip the "tmux " prefix
// and trim whitespace; the rest is opaque to callers (parse with
// CompareVersions if you need ordering).
func (c *Client) Version(ctx context.Context) (string, error) {
	// Note: -V is a tmux global flag, not a subcommand. It does not
	// participate in the -L socket scoping (no server contacted) so we
	// don't go through c.args() here.
	cmd := exec.CommandContext(ctx, "tmux", "-V")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux.Version: %w (stderr: %s)", err,
			strings.TrimSpace(stderr.String()))
	}
	out := strings.TrimSpace(stdout.String())
	// Expected shape: "tmux 3.4". Be defensive about future format drift.
	if !strings.HasPrefix(out, "tmux ") {
		return "", fmt.Errorf("tmux.Version: unexpected output %q", out)
	}
	return strings.TrimSpace(strings.TrimPrefix(out, "tmux ")), nil
}

// CompareVersions reports whether got is at least want (e.g., "3.4" >=
// "3.2"). Both inputs are tmux version strings as returned by Version().
//
// Compares major.minor numerically. Suffix letters (e.g. "3.5a") are
// treated as patch and ignored — "3.5" and "3.5a" compare equal. Unknown
// formats return an error so the caller can decide whether to fail open
// or closed.
func CompareVersions(got, want string) (bool, error) {
	gMaj, gMin, err := parseTmuxVersion(got)
	if err != nil {
		return false, fmt.Errorf("CompareVersions(got %q): %w", got, err)
	}
	wMaj, wMin, err := parseTmuxVersion(want)
	if err != nil {
		return false, fmt.Errorf("CompareVersions(want %q): %w", want, err)
	}
	if gMaj != wMaj {
		return gMaj > wMaj, nil
	}
	return gMin >= wMin, nil
}

// parseTmuxVersion splits "3.4" or "3.5a" into (3, 4) or (3, 5).
func parseTmuxVersion(s string) (major, minor int, err error) {
	// Strip any trailing letter (3.5a → 3.5).
	cut := len(s)
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'z' {
			cut = i
			break
		}
	}
	parts := strings.SplitN(s[:cut], ".", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("not in MAJOR.MINOR form: %q", s)
	}
	if _, err := fmt.Sscanf(parts[0], "%d", &major); err != nil {
		return 0, 0, fmt.Errorf("major: %w", err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &minor); err != nil {
		return 0, 0, fmt.Errorf("minor: %w", err)
	}
	return major, minor, nil
}
