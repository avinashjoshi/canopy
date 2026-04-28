// Package clog configures structured logging for canopy.
//
// All canopy packages use slog with a "pkg" attribute set at import time.
// Logs are written as JSON to ~/.canopy/log/canopy.log (append mode), with
// automatic rotation: 10 MB max per file, 3 backups kept, 28 days max age,
// rotated files compressed.
//
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

	"gopkg.in/natefinch/lumberjack.v2"
)

// level is mutable so the --debug flag can bump it after Init runs.
// slog.LevelVar is safe for concurrent use.
var level = new(slog.LevelVar)

// rotation policy. Tuned for a developer-tool workload (a few hundred
// INFO-level lines per workspace operation). 10 MB at INFO covers roughly
// a month of active use; DEBUG fills it faster but is rarely on for long.
const (
	maxSizeMB  = 10 // rotate when canopy.log exceeds this
	maxBackups = 3  // keep this many compressed backups (canopy.log.1.gz, ...)
	maxAgeDays = 28 // delete backups older than this
)

// Init opens ~/.canopy/log/canopy.log behind a lumberjack rotating writer
// and installs a JSON slog handler as the global default. It must be called
// once at startup before any other package logs anything.
//
// Returns a teardown closure that closes the underlying writer. Callers
// should defer it from main (cobra.OnFinalize is the easy spot).
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

	writer := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "canopy.log"),
		MaxSize:    maxSizeMB,
		MaxBackups: maxBackups,
		MaxAge:     maxAgeDays,
		Compress:   true,
	}

	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))

	return func() { _ = writer.Close() }, nil
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
