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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
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
//
// As a side effect, fires a best-effort SyncBranch call before rendering
// so that `git branch -m` performed inside the workspace shows up on the
// next tick. The sync is bounded by a 500ms timeout so a hung tmux or
// flock contention can't hang the user's status bar (tmux would render
// stale output for one tick, no worse).
func renderCurrentLine(ctx context.Context) string {
	t := tmux.New()
	sessionName, err := t.CurrentSession(ctx)
	if err != nil {
		statuslineLog.Warn("statusline.current_session", "err", err.Error())
		return ""
	}

	store, err := newStoreForStatusline()
	if err != nil {
		statuslineLog.Warn("statusline.load_state", "err", err.Error())
		return ""
	}
	st, err := store.Load()
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

	// Best-effort branch sync. Bounded ctx so worst-case behavior is one
	// tick of stale label, never a hung status bar. We re-load from state
	// after the sync so the render reflects any change we just applied.
	syncCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if res, err := workspace.SyncWorkspaceBranch(syncCtx, store, t, ws.ProjectRoot, ws.Name); err != nil {
		statuslineLog.Debug("statusline.sync_branch", "name", ws.Name, "err", err.Error())
	} else if res.Changed {
		// State just changed under us. Re-read so we render the new label,
		// not the pre-sync stale one. Cheap (one file read).
		if st2, err := store.Load(); err == nil {
			if ws2 := findWorkspaceBySession(st2, res.NewSession); ws2 != nil {
				ws = ws2
			}
		}
	}

	// Resolve "is the running canopy a dev build?" once per invocation.
	// Statusline is invoked every status-interval seconds, so we keep
	// this cheap: VersionDetails does at most one git fork (dev only,
	// path heuristic miss only). The DEV suffix is the user's
	// at-a-glance reminder that they swapped binaries via `canopy use`
	// or `make dev` and forgot to swap back.
	d := versionDetails()
	cols := paneColsForStatusline()
	remoteHost := readRemoteMarker()
	return formatCurrent(ws.ProjectBasename(), ws.Branch, ws.Name, ws.Status, ws.Port, d.IsDev, d.DevWorkspace, remoteHost, cols)
}

// readRemoteMarker returns the host nickname to display in the remote
// statusline pill, or "" when the current tmux session is local.
//
// Sole signal: CANOPY_REMOTE_HOST. The laptop sets this in the bash
// one-liner that mosh runs on the remote, and canopy switch propagates
// it to the tmux session env via SetSessionEnv so statusline subprocesses
// inherit it across re-attaches. The value is the registered host
// nickname from hosts.json (e.g., "tower"), not os.Hostname().
//
// Why no SSH_CONNECTION fallback: users typically run their own tmux
// status-right hostname segment (#H or similar), which already shows
// "where am I" for any sshd-launched shell — manual ssh, dogfood-into-
// own-box, mosh-from-anywhere. A canopy pill repeating that data
// renders the same hostname twice with no incremental signal. The pill's
// unique value is the *registered nickname* for canopy-driven attaches,
// which only CANOPY_REMOTE_HOST carries. Restricting the trigger keeps
// the pill meaningful instead of noisy.
//
// The returned value is the raw nickname (no styling). formatCurrent
// wraps it in the yellow pill #[bg=yellow,fg=black] segment and runs the
// nickname through escapeForTmux to neutralize style injection.
func readRemoteMarker() string {
	return strings.TrimSpace(os.Getenv("CANOPY_REMOTE_HOST"))
}

// newStoreForStatusline returns a state.Store handle for the statusline
// path. Returning the Store (rather than the loaded State) lets the
// caller both read state AND hand the same Store to SyncWorkspaceBranch
// when it needs to mutate under flock.
//
// Cost: one os.UserHomeDir() + one stat to verify the canopy home dir.
// Sub-millisecond on warm caches; same as the old loadStateForStatusline.
func newStoreForStatusline() (*state.Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("statusline: home dir: %w", err)
	}
	return state.NewStore(filepath.Join(home, ".canopy"))
}

// findWorkspaceBySession matches both the current `<project>/<branch>` format
// AND the legacy `<project>-<branch>` format. Legacy match is what lets the
// statusline find a workspace whose live tmux session hasn't migrated yet
// — without it, `findWorkspaceBySession` would return nil for every legacy
// session, the statusline would render empty, and SyncBranch's migration
// shim would never run from the statusline path.
func findWorkspaceBySession(st *state.State, sessionName string) *state.Workspace {
	if sessionName == "" {
		return nil
	}
	for i := range st.Workspaces {
		if st.Workspaces[i].TmuxSessionName() == sessionName {
			return &st.Workspaces[i]
		}
	}
	for i := range st.Workspaces {
		if st.Workspaces[i].LegacyTmuxSessionName() == sessionName {
			return &st.Workspaces[i]
		}
	}
	return nil
}

// formatCurrent renders one tmux status-right line:
//
//	[#[bg=yellow,fg=black] @<host> #[default] ]<project> / <wsName> / <branch> <glyph> :<port> [DEV:<x>]
//
// The leading yellow-pill prefix only renders when remoteHost is non-empty
// (the user is attached to this tmux session via mosh/ssh — see readRemoteMarker).
// Pill style codes are assembled OUTSIDE the escapeForTmux boundary so the
// `#[...]` sequences reach tmux unescaped; the user-controlled host nickname
// inside the pill IS escaped, so a hostile `CANOPY_REMOTE_HOST=tower#[bg=red]`
// can't inject extra styles.
//
// The workspace segment shows both wsName and branch when they differ
// (the post-`git branch -m` case where the folder name and branch
// diverged). When wsName == branch (the common auto-slug case) the
// segment renders as a single identifier, matching today's behavior.
// Width-aware: under pressure, wsName and branch share the truncation
// budget proportionally so neither piece vanishes entirely until the
// budget falls below dropThreshold. Project survives last.
//
// branch falls back to wsName when empty (legacy state.json rows that
// pre-date the live-sync pipeline). project falls back to "canopy" when
// empty (defensive: a corrupted state row shouldn't blank the line).
//
// The DEV suffix is independent of which workspace the user is in: it
// reflects the running canopy binary, not the active session. Collapses
// to bare [DEV] when devWorkspace == active branch.
//
// Per /plan-eng-review + /plan-design-review:
//   - D1 + D3: remote host marker sourced from CANOPY_REMOTE_HOST (canopy-
//     driven attaches) or SSH_CONNECTION/MOSH_TOKEN fallback (manual ssh).
//   - C1-revised: inner separator is "/" not ":" so it doesn't collide
//     with the port marker ":40010" in the tail.
//   - C2-revised: proportional truncation, not "drop wsName first."
//   - Color: yellow background pill, not foreground magenta — matches the
//     TUI DEV-pill convention and guarantees contrast across themes.
func formatCurrent(project, branch, wsName string, status state.Status, port int, isDev bool, devWorkspace, remoteHost string, cols int) string {
	if project == "" {
		project = "canopy"
	}
	displayBranch := branch
	if displayBranch == "" {
		displayBranch = wsName
	}
	// leftName is the workspace-folder name shown to the left of the slash
	// separator, only when it differs from the branch. When they match
	// (auto-slug case) we keep the single-identifier render for symmetry
	// with today's statusline.
	leftName := ""
	if wsName != "" && wsName != displayBranch {
		leftName = wsName
	}

	devSuffix := ""
	if isDev {
		// DEV-suffix-when-redundant drop: if the running dev binary
		// belongs to this same branch, the [DEV:x] part is just noise.
		if devWorkspace != "" && devWorkspace != displayBranch {
			devSuffix = fmt.Sprintf(" [DEV:%s]", devWorkspace)
		} else {
			devSuffix = " [DEV]"
		}
	}

	glyph := statuslineGlyph(status)
	tail := fmt.Sprintf(" %s :%d%s", glyph, port, devSuffix)

	// Workspace segment ("/wsName/branch", or "/branch") fitted to the
	// remaining budget. cols<=0 means "no width info from tmux" — render
	// unbounded and let tmux deal with overflow.
	var wsSeg string
	if cols <= 0 {
		if leftName != "" && displayBranch != "" {
			wsSeg = " / " + leftName + " / " + displayBranch
		} else if displayBranch != "" {
			wsSeg = " / " + displayBranch
		}
	} else {
		// Reserve visible-width budget for the marker pill so the workspace
		// segment isn't overcounted when we're attached remote. The pill
		// occupies " @<host> " visible cols (3 fixed + len(host)); the
		// #[...] style codes don't render on screen so don't count.
		markerVisibleW := 0
		if remoteHost != "" {
			markerVisibleW = 3 + runewidth.StringWidth(remoteHost)
		}
		fixedCols := markerVisibleW + runewidth.StringWidth(project) + runewidth.StringWidth(tail)
		wsSeg = renderWorkspaceSegment(leftName, displayBranch, cols-fixedCols)
	}

	// Body (everything except the pill) gets escaped as one unit, matching
	// the original escape boundary. The pill is assembled OUTSIDE this
	// boundary; only the host nickname inside it is user-controlled and
	// therefore individually escaped.
	body := escapeForTmux(project + wsSeg + tail)
	if remoteHost == "" {
		return body
	}
	pill := "#[bg=yellow,fg=black] @" + escapeForTmux(remoteHost) + " #[default] "
	return pill + body
}

// paneColsForStatusline reads tmux's current client width so the collapse
// algorithm has a real budget to work with. Returns 0 ("no width info,
// render full") on any error — formatCurrent falls back to the
// unbounded layout when cols<=0, which matches today's behavior.
//
// Uses `tmux display-message -p '#{client_width}'` because status-right
// is rendered per-client, not per-pane. Each statusline tick is a fresh
// process so there's no cross-tick caching to worry about.
//
// Heuristic clamp: client_width is the entire terminal width, but tmux's
// status-right shares cols with status-left and other status-right
// segments. We reserve roughly half the width for canopy's segment, which
// matches typical tmux configs where canopy is one of several segments.
// Users with custom-heavy status bars get less budget; users with bare
// status bars get more. A future refinement could parse the actual
// status-right format.
func paneColsForStatusline() int {
	out, err := exec.Command("tmux", "display-message", "-p", "#{client_width}").Output()
	if err != nil {
		return 0
	}
	cols := 0
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &cols); scanErr != nil {
		return 0
	}
	if cols <= 0 {
		return 0
	}
	// Reserve ~half for the rest of the status bar. Floor at 30 cols so
	// we don't auto-drop the branch on perfectly normal terminal widths
	// just because some user runs their status bar packed.
	budget := cols / 2
	if budget < 30 {
		budget = cols // tiny terminal, give canopy whatever's there
	}
	return budget
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
