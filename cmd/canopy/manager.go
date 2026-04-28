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
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}
	cfg, err := config.DiscoverAndLoad(cwd)
	if err != nil {
		return nil, err
	}
	return workspace.New(cfg)
}
