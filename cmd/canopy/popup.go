// Command canopy popup is the tmux display-popup launcher for canopy's
// global TUI. Bound to a tmux key (default <prefix>g), tapping it spawns
// a floating popup that hosts the global workspace picker. Picking a
// workspace fires `tmux switch-client` to that workspace's session and
// closes the popup.
//
// Architecture: two cobra commands.
//
//   - `canopy popup`        — the LAUNCHER. Runs OUTSIDE the popup,
//                             from a tmux key bind. Validates the
//                             environment (TMUX set, tmux >= 3.2)
//                             and invokes `tmux display-popup -E
//                             <self> popup-inner`. Blocks until the
//                             popup closes.
//   - `canopy popup-inner`  — the BODY (hidden). Runs INSIDE the popup.
//                             Renders the global TUI with a substituted
//                             activate callback that fires switch-client
//                             instead of attach. See popup_inner.go.
//
// This split keeps each function tightly scoped (per the user's
// "explicit > clever" preference and first-time-Go pacing) and makes
// the launcher and body testable independently.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/oncactus/canopy/internal/clog"
	"github.com/oncactus/canopy/internal/tmux"
)

// popupMinTmuxVersion is the minimum tmux version that supports
// display-popup. It landed in tmux 3.2 (October 2021). Older tmux
// returns "unknown command" for display-popup, which would surface
// as a confusing failure deep inside this code path. We check up
// front and refuse with a clear message.
const popupMinTmuxVersion = "3.2"

var popupLog = clog.Pkg("popup")

func newPopupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "popup",
		Short: "Open the canopy global TUI as a tmux floating popup.",
		Long: `Spawns a tmux display-popup hosting the canopy global TUI.

Bind in your tmux config (default <prefix>g):

  bind g run-shell "canopy popup"

Selecting a workspace switches your current tmux client to it and closes
the popup. Requires tmux 3.2+ (display-popup support).
`,
		// MUST run inside tmux: tmux is what's invoking us, and we need
		// $TMUX set for display-popup to know which client to render
		// the popup against.
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE:        runPopup,
	}
	return cmd
}

// runPopup is the launcher. Validates environment, then invokes
// `tmux display-popup -E <self> popup-inner`, which blocks until the
// popup exits (user picked a workspace, hit q, or pressed Esc).
func runPopup(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	// Outside-tmux is a hard error. The popup ONLY makes sense as a
	// floating window over an existing tmux client. Tell the user
	// what to do instead.
	if os.Getenv("TMUX") == "" {
		return fmt.Errorf(
			"canopy popup requires tmux. You're running outside any tmux session.\n\n" +
				"  Run `canopy` directly for the standalone TUI, or start tmux first.")
	}

	t := tmux.New()

	// Version preflight: refuse pre-3.2 tmux with a clear message
	// pointing at the actual version they have.
	ver, err := t.Version(ctx)
	if err != nil {
		return fmt.Errorf("canopy popup: tmux version check failed: %w", err)
	}
	ok, err := tmux.CompareVersions(ver, popupMinTmuxVersion)
	if err != nil {
		// Couldn't parse version. Fail closed: don't pretend it's fine.
		return fmt.Errorf("canopy popup: cannot parse tmux version %q: %w", ver, err)
	}
	if !ok {
		return fmt.Errorf(
			"canopy popup requires tmux %s+ (display-popup support).\n\n"+
				"  You have tmux %s. Upgrade tmux, or use `canopy session` once it ships.",
			popupMinTmuxVersion, ver)
	}

	// Resolve the canopy binary path so the popup invokes THIS binary
	// (not whatever's first on PATH inside the popup's shell). This
	// matters for development: when running canopy from a workspace
	// build, the popup must invoke the same binary, not a stale install.
	self, err := selfBinaryPath()
	if err != nil {
		return fmt.Errorf("canopy popup: locate self: %w", err)
	}

	// Resolve the calling pane's cwd. tmux's display-popup defaults to
	// the tmux server's cwd, which is rarely useful — the popup body
	// (popup-inner) walks up from cwd to find canopy.json for the Local
	// tab. Without the right cwd, every popup-from-inside-a-workspace
	// shows "no project" even though canopy.json sits one dir up.
	//
	// TMUX_PANE is set by tmux on processes spawned from key-binds.
	// If unset (e.g., user invoked `canopy popup` from a non-keybind
	// shell), fall back to os.Getwd — that's already the right answer
	// for that flow.
	popupCwd := resolvePopupCwd(ctx, t)

	// Run the display-popup. Blocks until the popup closes. The popup
	// runs `<self> popup-inner` which renders the TUI and either
	// returns 0 (user picked or quit) or non-zero on error.
	popupCmd := fmt.Sprintf("%s popup-inner", shellQuote(self))
	popupLog.Info("popup.spawn", "self", self, "tmux_version", ver, "cwd", popupCwd)
	if err := t.DisplayPopup(ctx, popupCmd, popupCwd); err != nil {
		// tmux returns non-zero if the inner process exits non-zero
		// OR if the user closes the popup with C-c (some tmux versions).
		// Treat both as "the popup closed somehow" — not an error to
		// surface, since the user-visible action already happened.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			popupLog.Info("popup.exit_nonzero", "exit_code", exitErr.ExitCode())
			return nil
		}
		return fmt.Errorf("canopy popup: display-popup failed: %w", err)
	}
	return nil
}

// selfBinaryPath returns the absolute path to the currently-executing
// canopy binary via os.Executable(). Resolved at runtime so dev builds
// (./canopy) and installed builds (~/go/bin/canopy or /usr/bin/canopy)
// both work without hardcoding.
func selfBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// os.Executable can return a symlink path on some platforms. Resolve
	// once so the popup invokes the real binary (not a stale symlink
	// from a previous install). On resolution failure, fall back to the
	// symlink path — worst case the popup invokes via symlink, which
	// is fine.
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, nil
	}
	return resolved, nil
}

// resolvePopupCwd returns the directory the popup body should start
// in. Strategy:
//   1. If $TMUX_PANE is set (the canonical key-bind invocation path),
//      ask tmux for that pane's cwd. This is what the user is actually
//      working in.
//   2. Otherwise, fall back to os.Getwd. Covers `canopy popup` invoked
//      directly from a shell prompt — that shell's cwd is the right
//      starting point.
//   3. On any error, return "" — DisplayPopup tolerates empty cwd
//      (lets tmux pick its default) instead of failing the popup.
func resolvePopupCwd(ctx context.Context, t *tmux.Client) string {
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		if cwd, err := t.PaneCwd(ctx, pane); err == nil && cwd != "" {
			return cwd
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// shellQuote wraps a path in single quotes if it contains characters
// the shell would interpret (spaces, $, etc). tmux's display-popup
// command argument is parsed by the shell, so paths with spaces would
// break without quoting.
//
// Single quotes preserve everything except a literal single quote;
// since canopy's binary path comes from os.Executable() (resolved
// from /proc/self/exe on Linux), it cannot contain a single quote
// in any realistic install. Worst case for an exotic path: we'd need
// $'...' or escaped quoting. v0.7 keeps it simple.
func shellQuote(s string) string {
	// Fast path: alphanumerics, slash, dot, underscore, hyphen are all
	// shell-safe. Skip quoting if every character is in that set.
	for i := 0; i < len(s); i++ {
		c := s[i]
		safe := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '/' || c == '.' || c == '_' || c == '-'
		if !safe {
			return "'" + s + "'"
		}
	}
	return s
}
