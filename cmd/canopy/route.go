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

// routeRoot picks between the init splash (fresh git repo, no
// canopy.json) and the unified TUI. Called from main.go's RunE when the
// user invokes `canopy` with no subcommand.
func routeRoot(ctx context.Context, cwd string, stdout io.Writer) error {
	cfg, cfgErr := config.DiscoverAndLoad(cwd)

	// Fresh-git-repo splash: no canopy.json AND we're inside a git repo.
	// Pre-empts the unified TUI because the user has nothing to list yet —
	// the right action is to `canopy init`.
	if errors.Is(cfgErr, config.ErrNotFound) {
		isRepo, _ := git.IsRepo(ctx, cwd)
		if isRepo {
			return runInitSplashFlow(cwd, stdout)
		}
	} else if cfgErr != nil && !errors.Is(cfgErr, config.ErrNotFound) {
		// Real error walking up (permission denied, malformed canopy.json
		// somewhere mid-tree). Surface as-is.
		return cfgErr
	}

	// Build store + tmux client; both work whether or not we're in a
	// project.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("unified TUI: home dir: %w", err)
	}
	store, err := state.NewStore(filepath.Join(home, ".canopy"))
	if err != nil {
		return err
	}
	tc := tmux.New()

	// Optional Manager: present when cwd has a canopy.json walk-up.
	var mgr *workspace.Manager
	if cfg != nil {
		mgr, err = workspace.New(cfg)
		if err != nil {
			return err
		}
	}

	// Resolve "current project" for the Local tab filter. When mgr is
	// non-nil this is just cfg.ProjectRoot. When mgr is nil (popup
	// outside any project, or canopy from a non-project dir), use the
	// workspace-cwd resolver so popup-from-inside-a-workspace correctly
	// pre-selects the workspace's project.
	currentProject := ""
	if cfg != nil {
		currentProject = cfg.ProjectRoot
	} else {
		// Best-effort: state load errors are non-fatal here, the
		// resolver tolerates nil state and returns "" (Global tab
		// pre-selected).
		st, _ := store.Load()
		currentProject = workspace.ResolveCurrentProject(cwd, st)
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
