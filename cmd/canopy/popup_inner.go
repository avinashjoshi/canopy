// Command canopy popup-inner is the body of the canopy popup, hosted
// inside `tmux display-popup -E` by the launcher (popup.go). It runs
// the existing global TUI (internal/ui.GlobalModel) configured for popup
// mode: pressing Enter on a ready/alive row fires `tmux switch-client`
// instead of `tmux attach`, then the model quits — which closes the
// popup. The user lands in the selected workspace's tmux session via
// the parent tmux client, no detach round-trip needed.
//
// This subcommand is hidden from `canopy --help` because nobody invokes
// it directly — it's a delegation target for `canopy popup`.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/oncactus/canopy/internal/clog"
	"github.com/oncactus/canopy/internal/config"
	"github.com/oncactus/canopy/internal/state"
	"github.com/oncactus/canopy/internal/tmux"
	"github.com/oncactus/canopy/internal/ui"
)

var popupInnerLog = clog.Pkg("popup-inner")

func newPopupInnerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "popup-inner",
		Short: "Internal: TUI body for canopy popup. Not for direct invocation.",
		Long: `This subcommand is invoked by tmux display-popup from canopy popup. It
renders the global TUI inside the popup and uses tmux switch-client (not
attach) on Enter so the parent client transitions to the selected
workspace cleanly.

Run 'canopy popup' instead — that's the user-facing entry point.
`,
		Hidden: true,
		// MUST run inside tmux: we're hosted by display-popup which only
		// fires inside an attached tmux client.
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE:        runPopupInner,
	}
	return cmd
}

func runPopupInner(cmd *cobra.Command, _ []string) error {
	// Open state read-only. Same path used by `canopy ls` and friends.
	store, err := openStateForPopupInner()
	if err != nil {
		return fmt.Errorf("popup-inner: open state: %w", err)
	}

	tc := tmux.New()

	// Resolve the "current project" so the Local tab can filter to its
	// workspaces. The tricky bit: when the popup is invoked from inside
	// a workspace, the workspace dir itself contains canopy.json (it's
	// a git worktree of the project), so a naive config.DiscoverAndLoad
	// returns the workspace dir as "project root" — which doesn't match
	// any state.json ProjectRoot, and the Local tab shows nothing.
	//
	// Right resolution: load state.json first, find a registered
	// workspace whose Path is a prefix of cwd. That workspace's
	// ProjectRoot is the canonical project root, used by every other
	// row. THEN fall back to canopy.json walk-up + state Project
	// registry (covers "user is in the main project repo, not a
	// workspace"). Else empty.
	st, _ := store.Load() // best-effort; empty state means no Local matches
	currentProject := ""
	if cwd, err := os.Getwd(); err == nil {
		currentProject = resolveCurrentProject(cwd, st)
	}

	// Construct the global TUI configured for popup mode. The closure
	// fires switch-client to the chosen workspace's session and lets
	// AsPopup quit the model immediately after, which collapses the
	// display-popup window.
	model := ui.NewGlobal(store, tc).AsPopup(func(session string) error {
		ctx := cmd.Context()
		if err := tc.SwitchClient(ctx, session); err != nil {
			// Log so tmux's error in the parent bar isn't the only
			// trace we have. Don't propagate to the user — popup is
			// closing anyway.
			popupInnerLog.Warn("popup-inner.switch_client_failed",
				"session", session, "err", err.Error())
			return err
		}
		popupInnerLog.Info("popup-inner.switched", "session", session)
		return nil
	}, currentProject)

	// Run with AltScreen so the popup gets a clean canvas. tea.WithMouseCellMotion
	// is intentionally omitted — popup mode keystrokes only.
	prog := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(cmd.Context()))
	if _, err := prog.Run(); err != nil {
		return fmt.Errorf("popup-inner: tea program: %w", err)
	}
	return nil
}

func openStateForPopupInner() (*state.Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("popup-inner: home dir: %w", err)
	}
	return state.NewStore(filepath.Join(home, ".canopy"))
}

// resolveCurrentProject maps a cwd to a canonical project root for the
// Local tab filter. Strategy (in order):
//
//  1. Workspace match: if cwd is under any state.Workspaces[*].Path,
//     return that workspace's ProjectRoot. Workspaces are git worktrees
//     of the project; their ProjectRoot is the canonical original repo
//     root that every row uses as a key.
//
//  2. Project main-repo match: if config.DiscoverAndLoad walks up from
//     cwd and finds canopy.json AT a path that's registered in
//     state.Projects, return that path. Covers "I cd'd into the main
//     repo and pressed the popup keybind."
//
//  3. Else empty string. Local tab shows no rows; Global tab still works.
//
// nil state is tolerated — yields empty (best-effort fallback).
func resolveCurrentProject(cwd string, st *state.State) string {
	if st == nil {
		return ""
	}
	// Step 1: workspace-path prefix match.
	cwdWithSlash := strings.TrimRight(cwd, "/") + "/"
	for _, ws := range st.Workspaces {
		if ws.Path == "" {
			continue
		}
		wsPathWithSlash := strings.TrimRight(ws.Path, "/") + "/"
		if cwdWithSlash == wsPathWithSlash || strings.HasPrefix(cwdWithSlash, wsPathWithSlash) {
			if ws.ProjectRoot != "" {
				return ws.ProjectRoot
			}
		}
	}

	// Step 2: registered-project canopy.json walk-up.
	cfg, err := config.DiscoverAndLoad(cwd)
	if err != nil {
		// Not in a canopy project at all (or unreadable) — empty.
		if !errors.Is(err, config.ErrNotFound) {
			popupInnerLog.Warn("popup-inner.config_discover_failed", "err", err.Error())
		}
		return ""
	}
	if _, registered := st.Projects[cfg.ProjectRoot]; registered {
		return cfg.ProjectRoot
	}
	// canopy.json found but project isn't in state.json — could be a
	// stale config from a different machine, or canopy init was never
	// run for this project. Don't filter by it; the Local tab would
	// show zero rows anyway.
	return ""
}
