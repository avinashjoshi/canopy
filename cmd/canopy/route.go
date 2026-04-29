// Routing for the no-subcommand `canopy` invocation.
//
// v0.8 unification: there is now ONE TUI program for every canopy
// invocation. The routing decision is much simpler than v0.5-v0.7:
//
//	1. Fresh git repo with no canopy.json → InitSplashModel
//	   ("press i to canopy init"). On 'i', runInit fires synchronously.
//
//	2. Everything else (in-project, outside-any-project, popup) →
//	   the unified TUI via ui.RunUnified. Pre-selected tab + Local-row
//	   filter is computed from cwd via workspace.ResolveCurrentProject.
//
// The popup keybind in tmux invokes `canopy` directly (no popup-inner
// subcommand). CANOPY_IN_POPUP=1 set by `display-popup -E` flips
// rendering to popup mode.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oncactus/canopy/internal/config"
	"github.com/oncactus/canopy/internal/git"
	"github.com/oncactus/canopy/internal/state"
	"github.com/oncactus/canopy/internal/tmux"
	"github.com/oncactus/canopy/internal/ui"
	"github.com/oncactus/canopy/internal/workspace"
)

// routeRoot picks between the init splash (fresh canopy install, no
// known projects) and the unified TUI. Called from main.go's RunE when
// the user invokes `canopy` with no subcommand.
//
// Routing precedence (most specific first):
//
//  1. cwd has canopy.json → unified TUI with Manager scoped to that
//     project. If Manager construction fails (e.g. v1/v2 state collision),
//     fall back to global mode (mgr=nil) rather than refusing to launch —
//     the user can still see workspaces and act on cross-project rows.
//
//  2. cwd resolves to a registered workspace (state.json knows about it)
//     → unified TUI with mgr=nil but currentProject=resolved. Covers
//     workspace dirs, where canopy.json isn't checked into the worktree.
//
//  3. cwd is in a git repo with no canopy.json AND state.json has no
//     projects yet → init splash. The "first canopy install" path.
//
//  4. Anything else → unified TUI in global mode. Covers /tmp, $HOME,
//     ambient dirs.
func routeRoot(ctx context.Context, cwd string, stdout io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("unified TUI: home dir: %w", err)
	}
	store, err := state.NewStore(filepath.Join(home, ".canopy"))
	if err != nil {
		return err
	}
	tc := tmux.New()

	// Best-effort state load up front so the workspace-cwd resolver and
	// the "any registered projects?" check below can use it. nil state
	// is tolerated — both paths fall through cleanly.
	st, _ := store.Load()

	cfg, cfgErr := config.DiscoverAndLoad(cwd)

	// Real walk-up error (permission denied, malformed mid-tree
	// canopy.json) — surface as-is. ErrNotFound falls through.
	if cfgErr != nil && !errors.Is(cfgErr, config.ErrNotFound) {
		return cfgErr
	}

	var mgr *workspace.Manager
	currentProject := ""

	if cfg != nil {
		// Path 1: cwd has canopy.json. Try to construct a Manager;
		// if it fails (v1/v2 state collision is the common cause),
		// log a warning and fall back to global mode so the unified
		// TUI still launches. The user can fix state.json from
		// outside and re-run, or use the Global tab in the meantime.
		currentProject = cfg.ProjectRoot
		m, err := workspace.New(cfg)
		if err != nil {
			fmt.Fprintf(stdout,
				"warning: couldn't construct Manager for %s (%v). "+
					"Launching unified TUI in global mode — destructive verbs on this project's "+
					"rows won't work until the underlying state issue is resolved.\n",
				cfg.ProjectRoot, err)
		} else {
			mgr = m
		}
	} else {
		// Path 2: cwd is a known workspace? Use the workspace-cwd
		// resolver. ResolveCurrentProject also walks up canopy.json
		// as a fallback, but cwd already failed that — so if it
		// returns non-empty here, the match came from the workspace
		// path-prefix lookup.
		currentProject = workspace.ResolveCurrentProject(cwd, st)
	}

	// Path 3: init splash gate. Only fire when the user has NO known
	// projects AND cwd is a git repo. This prevents the splash from
	// firing inside workspace dirs (which are git worktrees of repos
	// canopy DOES know about) and inside fresh repos for users who
	// already have other projects registered (where the right action
	// is "see your other projects" not "init this empty one").
	if cfg == nil && currentProject == "" {
		hasProjects := st != nil && len(st.Projects) > 0
		if !hasProjects {
			isRepo, _ := git.IsRepo(ctx, cwd)
			if isRepo {
				return runInitSplashFlow(cwd, stdout)
			}
		}
	}

	return ui.RunUnified(mgr, store, tc, currentProject)
}

// runInitSplashFlow shows the init prompt. If the user opts in, this
// function runs `canopy init` against cwd synchronously (same code path
// the standalone `canopy init` command uses).
func runInitSplashFlow(cwd string, stdout io.Writer) error {
	didInit, err := ui.RunInitSplash(cwd)
	if err != nil {
		return fmt.Errorf("init splash: %w", err)
	}
	if !didInit {
		return nil
	}
	return runInit(cwd, initOptions{}, stdout)
}
