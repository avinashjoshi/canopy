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
	return formatCurrent(ws.ProjectBasename(), ws.Branch, ws.Name, ws.Status, ws.Port, d.IsDev, d.DevWorkspace, cols)
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
//	<project> / <branch> <glyph> :<port> [DEV:<x>]
//
// Width-aware: when cols is positive, the branch segment collapses
// (full -> right-ellipsis -> initials -> dropped) per the design D2
// algorithm so narrow tmux panes don't smear the line.
//
// branch falls back to wsName when empty (legacy state.json rows that
// pre-date the live-sync pipeline). project falls back to "canopy" when
// empty (defensive: a corrupted state row shouldn't blank the line).
//
// The DEV suffix is independent of which workspace the user is in: it
// reflects the running canopy binary, not the active session. So a
// user inside workspace B's tmux who has `canopy use feature-A` flipped
// will see `canopy / B ● :40010 [DEV:feature-A]` — exactly the
// confusion-buster the design calls for.
//
// devWorkspace may be empty even when isDev is true (binary lives
// outside any known worktree); we fall back to bare "[DEV]" then so
// the user still sees the dev marker.
//
// Drops the [DEV:<x>] suffix down to bare [DEV] when devWorkspace ==
// the workspace's branch — saves ~24 cols on the screenshot's exact
// pain point (where the workspace name and the active dev binary
// reference the same thing, so the suffix is just noise).
//
// Always passes through escapeForTmux so `#` in a name can't inject
// style sequences.
func formatCurrent(project, branch, wsName string, status state.Status, port int, isDev bool, devWorkspace string, cols int) string {
	if project == "" {
		project = "canopy"
	}
	displayBranch := branch
	if displayBranch == "" {
		displayBranch = wsName
	}

	devSuffix := ""
	if isDev {
		// DEV-suffix-when-redundant drop (collapse step 1): if the
		// running dev binary belongs to this same branch, the [DEV:x]
		// part is just noise — collapse to bare [DEV].
		if devWorkspace != "" && devWorkspace != displayBranch {
			devSuffix = fmt.Sprintf(" [DEV:%s]", devWorkspace)
		} else {
			devSuffix = " [DEV]"
		}
	}

	glyph := statuslineGlyph(status)
	tail := fmt.Sprintf(" %s :%d%s", glyph, port, devSuffix)

	// Width budget for the branch segment = total cols minus the fixed
	// pieces (project name + tail). When cols <= 0 (no width info from
	// tmux), render at full and let tmux do whatever it does.
	var branchSeg string
	if cols <= 0 {
		if displayBranch != "" {
			branchSeg = " / " + displayBranch
		}
	} else {
		fixedCols := runewidth.StringWidth(project) + runewidth.StringWidth(tail)
		branchSeg = renderBranchSegment(displayBranch, cols-fixedCols)
	}

	return escapeForTmux(project + branchSeg + tail)
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
