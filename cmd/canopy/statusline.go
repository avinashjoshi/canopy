// Command canopy statusline renders a single-line tmux status-bar widget
// showing the active workspace's name, status glyph, and port. Designed
// to be invoked from tmux's `status-right` via #(canopy statusline).
//
// Hard correctness rules:
//
//  1. NEVER print errors to stdout. tmux substitutes #(...) output verbatim
//     into the status bar — anything we print becomes visible to the user
//     in their tmux prompt. All errors go to stderr/canopy.log; stdout
//     is always a valid status string (often empty).
//
//  2. NEVER panic out. A defer-recover wraps the entire main flow so a
//     canopy bug can't garbage-fill the user's tmux status bar across
//     every session they have open.
//
//  3. ALWAYS escape `#` to `##`. tmux interprets `#[fg=red]` and friends
//     as style sequences inside #(...) output. A workspace name like
//     `feat#[bg=red]gotcha` would inject styles otherwise. The escape is
//     tmux's documented way to emit a literal `#`.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/oncactus/canopy/internal/clog"
	"github.com/oncactus/canopy/internal/state"
	"github.com/oncactus/canopy/internal/tmux"
)

const (
	// statuslineFormatCurrent is the v0.7 default and only supported
	// format. Future formats (fleet, alerts) defer to v0.8.
	statuslineFormatCurrent = "current"
)

func newStatuslineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "statusline",
		Short: "Render a tmux status-bar widget for the current workspace.",
		Long: `Designed for tmux's status-right via #(canopy statusline).

Prints a single line to stdout. Errors go to stderr/canopy.log; stdout is
always a valid tmux status string (possibly empty). Never panics.

Add to your tmux config:

  set -ag status-right " #(canopy statusline --format=current) "
`,
		// Allow running inside tmux: tmux re-invokes us every status-interval
		// from inside its own session. Without this annotation, the nested-
		// tmux guard would refuse and the status bar would error every 15s.
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE:        runStatusline,
	}
	cmd.Flags().String("format", statuslineFormatCurrent,
		"format: current (only one supported in v0.7)")
	return cmd
}

// statuslineLog is the package-private logger for this subcommand. We
// don't reuse the cmd-level "main" log because statusline runs every
// status-interval seconds — having a dedicated logger key makes filtering
// canopy.log noise easy: `grep '"pkg":"statusline"'`.
var statuslineLog = clog.Pkg("statusline")

func runStatusline(cmd *cobra.Command, _ []string) error {
	// IRON RULE: defer-recover at the entry point. Any panic in the
	// statusline path becomes an empty stdout, exit 0. The user's tmux
	// status bar must not become a debugging surface.
	defer func() {
		if r := recover(); r != nil {
			statuslineLog.Warn("statusline.panic", "recovered", fmt.Sprintf("%v", r))
			// stdout is whatever it already was (possibly empty); we don't
			// rewrite it because we may have partially written before the
			// panic. tmux will tolerate a partial line for one tick.
		}
	}()

	format, _ := cmd.Flags().GetString("format")
	if format != statuslineFormatCurrent {
		// Unknown format: silent empty. Don't error to stderr because
		// that just spams the user's tmux config every 15s if they typo.
		statuslineLog.Warn("statusline.unknown_format", "format", format)
		return nil
	}

	out := renderCurrentLine(cmd.Context())
	if out != "" {
		fmt.Fprintln(cmd.OutOrStdout(), out)
	}
	return nil
}

// renderCurrentLine produces the line for `--format=current`. ALWAYS
// returns a tmux-safe string: either a fully-escaped status line, or "".
// Never propagates errors — internal failures log + return "".
func renderCurrentLine(ctx context.Context) string {
	t := tmux.New()
	sessionName, err := t.CurrentSession(ctx)
	if err != nil {
		statuslineLog.Warn("statusline.current_session", "err", err.Error())
		return ""
	}

	st, err := loadStateForStatusline()
	if err != nil {
		statuslineLog.Warn("statusline.load_state", "err", err.Error())
		return ""
	}

	ws := findWorkspaceBySession(st, sessionName)
	if ws == nil {
		// User is in a non-canopy tmux session, or the session was renamed
		// out from under canopy. Silent empty — no status to show.
		return ""
	}

	return formatCurrent(ws.Name, ws.Status, ws.Port)
}

// loadStateForStatusline opens state.json read-only. No flock — statusline
// tolerates a stale snapshot up to status-interval seconds (15s default),
// which is well within tmux's refresh window. Locking would block tmux
// across canopy mutations and is a perf footgun.
func loadStateForStatusline() (*state.State, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("statusline: home dir: %w", err)
	}
	store, err := state.NewStore(filepath.Join(home, ".canopy"))
	if err != nil {
		return nil, err
	}
	return store.Load()
}

func findWorkspaceBySession(st *state.State, sessionName string) *state.Workspace {
	if sessionName == "" {
		return nil
	}
	for i := range st.Workspaces {
		if st.Workspaces[i].TmuxSession == sessionName {
			return &st.Workspaces[i]
		}
	}
	return nil
}

// formatCurrent renders one line: "canopy: <name> <glyph> :<port>".
// Always passes through escapeForTmux so `#` in a workspace name can't
// inject style sequences.
func formatCurrent(name string, status state.Status, port int) string {
	return escapeForTmux(fmt.Sprintf("canopy: %s %s :%d", name, statuslineGlyph(status), port))
}

// statuslineGlyph mirrors the protanopia-friendly glyph set used in the
// global TUI (internal/ui/projectlist/projectlist.go:statusGlyphFor).
// Duplicated because statusline output is plain text — no lipgloss styles
// survive tmux's #(...) interpolation.
func statuslineGlyph(s state.Status) string {
	switch s {
	case state.StatusReady:
		return "●"
	case state.StatusSettingUp:
		return "…"
	case state.StatusStopped:
		return "⏸"
	case state.StatusBroken:
		return "✗"
	case state.StatusOrphaned:
		return "!"
	}
	return ""
}

// escapeForTmux replaces `#` with `##` so a workspace name containing
// `#[fg=red]gotcha` cannot inject style sequences into tmux's status
// bar. tmux's documented escape: `#` → `##` for a literal pound sign in
// `status-right` interpolation.
//
// Codex caught this missing in the v0.7 plan. Without it, hostile branch
// names from forks (`canopy new --pr` flow) could mess with the user's
// status bar styling. Cosmetic, not RCE, but trivially fixable.
func escapeForTmux(s string) string {
	return strings.ReplaceAll(s, "#", "##")
}
