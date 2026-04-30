package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oncactus/canopy/internal/config"
	"github.com/oncactus/canopy/internal/state"
	"github.com/oncactus/canopy/internal/workspace"
)

// loadManager is the shared entry point for every subcommand that needs
// the workspace lifecycle. Resolves the canonical project root via
// ResolveCurrentProject (workspace-path-prefix match wins over canopy.json
// walk-up) before constructing the Manager — without this, running a
// CLI verb from inside a workspace dir would resolve cfg.ProjectRoot to
// the worktree path (the worktree contains the project's checked-in
// canopy.json), and Reconcile / Find / Remove etc. would filter every
// row out by ProjectRoot mismatch. That's the same bug we fixed for
// the unified TUI's route in cmd/canopy/route.go; the CLI needs the
// equivalent treatment.
//
// All errors are user-facing — the caller can return them straight from
// a cobra RunE and cobra will print a clean error before exiting non-zero.
func loadManager() (*workspace.Manager, error) {
	cwd, err := getCwd()
	if err != nil {
		return nil, err
	}
	cfg, err := resolveCfgForCwd(cwd)
	if err != nil {
		return nil, err
	}
	return workspace.New(cfg)
}

// resolveCfgForCwd picks the right canopy.json for a CLI invocation:
// when cwd is inside a registered workspace, load the source-repo's
// canopy.json (so cfg.ProjectRoot matches the canonical key in
// state.Workspaces); otherwise walk up from cwd. Extracted from
// loadManager so the resolution logic is testable without going
// through workspace.New (which touches ~/.canopy/state.json).
func resolveCfgForCwd(cwd string) (*config.Config, error) {
	home, hErr := os.UserHomeDir()
	if hErr == nil {
		store, sErr := state.NewStore(filepath.Join(home, ".canopy"))
		if sErr == nil {
			st, _ := store.Load()
			if root := workspace.ResolveCurrentProject(cwd, st); root != "" {
				if pcfg, perr := config.LoadFrom(root); perr == nil {
					return pcfg, nil
				}
				// LoadFrom failed at the canonical root (canopy.json
				// missing, parse error). Fall through to walk-up so
				// the user gets the same error path they'd see today.
			}
		}
	}
	return loadConfig(cwd)
}

// getCwd is a thin wrapper around os.Getwd that wraps the error with a
// consistent prefix. Used by subcommands that don't need the full Manager
// (canopy main, canopy ls --all) but still need to discover canopy.json.
func getCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	return cwd, nil
}

// loadConfig walks up from startDir to find canopy.json and parses it.
// Used by subcommands that need config but not the full Manager.
//
// ErrNotFound is wrapped with a friendly, action-oriented message so
// commands like `canopy main` and `canopy new` give the user a usable
// next step ("run `canopy init`") instead of leaking the internal
// walk-up error verbatim.
func loadConfig(startDir string) (*config.Config, error) {
	cfg, err := config.DiscoverAndLoad(startDir)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return nil, fmt.Errorf(
				"this directory isn't a canopy project yet — run `canopy init` here (or cd into one of your existing canopy projects). Looked from: %s",
				startDir)
		}
		return nil, err
	}
	return cfg, nil
}
