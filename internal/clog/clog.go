// Package clog configures structured logging for canopy.
//
// All canopy packages use slog with a "pkg" attribute set at import time.
// Logs are written as JSON to ~/.canopy/log/canopy.log (append mode).
// The --debug flag passed to the root command bumps the level from INFO to
// DEBUG; otherwise INFO is the default.
//
// Idiomatic use at the top of each canopy package:
//
//	var log = clog.Pkg("git")
//
//	func Add(branch, path string) error {
//	    log.Info("worktree.add", "branch", branch, "path", path)
//	    ...
//	}
package clog

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// level is mutable so the --debug flag can bump it after Init runs.
// slog.LevelVar is safe for concurrent use.
var level = new(slog.LevelVar)

// Init opens ~/.canopy/log/canopy.log and installs a JSON slog handler as
// the global default. It must be called once at startup before any other
// package logs anything.
//
// Returns a teardown closure that closes the underlying log file. Callers
// should defer it from main.
func Init(debug bool) (func(), error) {
	if debug {
		level.Set(slog.LevelDebug)
	} else {
		level.Set(slog.LevelInfo)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return func() {}, fmt.Errorf("clog.Init: home dir: %w", err)
	}

	dir := filepath.Join(home, ".canopy", "log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}, fmt.Errorf("clog.Init: mkdir %s: %w", dir, err)
	}

	path := filepath.Join(dir, "canopy.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return func() {}, fmt.Errorf("clog.Init: open %s: %w", path, err)
	}

	handler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))

	return func() { _ = f.Close() }, nil
}

// Pkg returns a logger pre-tagged with the given package name. This is the
// only function most callers need; one var declaration at the top of each
// package gives every log line a consistent "pkg" attribute for grep-ability.
func Pkg(name string) *slog.Logger {
	return slog.With("pkg", name)
}

// SetDebug toggles the log level at runtime. Useful for tests and for code
// paths that want to bump verbosity without restarting.
func SetDebug(debug bool) {
	if debug {
		level.Set(slog.LevelDebug)
	} else {
		level.Set(slog.LevelInfo)
	}
}
