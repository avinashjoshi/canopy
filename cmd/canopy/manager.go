package main

import (
	"fmt"
	"os"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// loadManager is the shared entry point for every subcommand that needs
// the workspace lifecycle. It walks up from cwd to find canopy.json,
// loads config, then constructs a Manager pointing at ~/.canopy for state.
//
// All errors are user-facing — the caller can return them straight from
// a cobra RunE and cobra will print a clean error before exiting non-zero.
func loadManager() (*workspace.Manager, error) {
	cwd, err := getCwd()
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(cwd)
	if err != nil {
		return nil, err
	}
	return workspace.New(cfg)
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
func loadConfig(startDir string) (*config.Config, error) {
	return config.DiscoverAndLoad(startDir)
}
