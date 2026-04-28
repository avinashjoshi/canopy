// Routing for the no-subcommand `canopy` invocation.
//
// `canopy` (no args) is context-sensitive in v0.5+:
//
//	1. Inside a canopy project (canopy.json discoverable up the tree)
//	   → today's project-scoped Bubbletea TUI (workspace.Manager + ui.Run).
//
//	2. Inside a git repo with no canopy.json
//	   → InitSplashModel ("press i to canopy init"). On 'i', the splash
//	     exits and we run runInit synchronously so its output prints to
//	     the user's normal terminal post-altscreen.
//
//	3. Anywhere else (home dir, /tmp, etc.)
//	   → GlobalModel: read-only cross-project view. Lists every workspace
//	     and every alive `<project>-main` session canopy knows about.
//
// The dispatch function below stays small on purpose. Each branch hands
// off to a self-contained Run* in the appropriate package; this file is
// the routing table.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/settings"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/ui"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// routeRoot picks which Bubbletea Model to run based on the cwd's
// project + git state. Called from main.go's RunE when the user invokes
// `canopy` with no subcommand.
//
// Errors from config.DiscoverAndLoad that aren't ErrNotFound (permission
// denied, parse failure) bubble up unchanged so the user sees the real
// problem. Errors from git.IsRepo are demoted to "not a repo" because
// the routing concern is binary: if we can't confirm a repo, treat as
// "no fresh-repo splash" and fall through to global mode.
func routeRoot(ctx context.Context, cwd string, stdout io.Writer) error {
	cfg, cfgErr := config.DiscoverAndLoad(cwd)

	switch {
	case cfgErr == nil:
		// Inside a canopy project. Build the Manager (which runs the
		// v1→v2 migration + basename-collision gate inside) and hand
		// off to today's project TUI.
		mgr, err := workspace.New(cfg)
		if err != nil {
			return err
		}
		return ui.Run(mgr)

	case errors.Is(cfgErr, config.ErrNotFound):
		// No canopy.json found up the tree. Decide: fresh git repo
		// (init splash) or somewhere else (global TUI).
		isRepo, _ := git.IsRepo(ctx, cwd)
		if isRepo {
			return runInitSplashFlow(cwd, stdout)
		}
		return runGlobalFlow()

	default:
		// Real error walking up (permission denied, malformed canopy.json
		// somewhere mid-tree). Surface as-is.
		return cfgErr
	}
}

// runInitSplashFlow shows the init prompt. If the user opts in, this
// function runs `canopy init` against cwd synchronously (same code path
// the standalone `canopy init` command uses). Output prints to stdout
// after the Bubbletea program exits altscreen, so it looks identical to
// running `canopy init` directly.
func runInitSplashFlow(cwd string, stdout io.Writer) error {
	didInit, err := ui.RunInitSplash(cwd)
	if err != nil {
		return fmt.Errorf("init splash: %w", err)
	}
	if !didInit {
		return nil // user dismissed
	}
	// User pressed 'i' — run init with the same defaults as the standalone
	// command. We deliberately don't auto-launch the project TUI after
	// init succeeds; the next-steps block tells the user what to do, and
	// they re-run `canopy` to enter the project view. One re-keystroke
	// is fine, and it keeps this routing function simple.
	return runInit(cwd, initOptions{}, stdout)
}

// runGlobalFlow opens GlobalModel against state.json. No canopy.json,
// no Manager — global mode reads state directly. Loads ~/.canopy/config.json
// so the auto_close_shipped lifecycle toggle reaches the model. A missing
// or partial settings file is fine: settings.Load returns Default() with
// nil error in that case, so RunGlobal always gets sane defaults.
func runGlobalFlow() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("global: home dir: %w", err)
	}
	canopyHome := filepath.Join(home, ".canopy")
	store, err := state.NewStore(canopyHome)
	if err != nil {
		return err
	}
	s, err := settings.Load(canopyHome)
	if err != nil {
		return fmt.Errorf("global: load settings: %w", err)
	}
	return ui.RunGlobal(store, tmux.New(), s)
}
