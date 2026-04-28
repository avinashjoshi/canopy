// Command canopy is a TUI for managing git worktrees with paired tmux
// sessions. See docs/design/v0-canopy.md for the full design.
//
// Running `canopy` with no arguments opens the workspace TUI (once it's
// implemented). Until then, only `canopy version` and the standard cobra
// help output are wired up.
package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clog"
)

// debugFlag is the --debug switch on the root command. When true, the log
// level is bumped from INFO to DEBUG before any other canopy package runs.
var debugFlag bool

// version is set via -ldflags at release time by goreleaser. When canopy is
// built via `go install`, version stays "dev" and the real version comes
// from runtime/debug.ReadBuildInfo (the module's vcs info).
var version = "dev"

func main() {
	root := &cobra.Command{
		Use:   "canopy",
		Short: "TUI for managing git worktrees with paired tmux sessions and per-project setup hooks.",
		Long: "   _____\n" +
			"  / ____|\n" +
			" | |     __ _ _ __   ___  _ __  _   _\n" +
			" | |    / _` | '_ \\ / _ \\| '_ \\| | | |\n" +
			" | |___| (_| | | | | (_) | |_) | |_| |\n" +
			"  \\_____\\__,_|_| |_|\\___/| .__/ \\__, |\n" +
			"                         | |     __/ |\n" +
			"                         |_|    |___/\n" +
			"\n" +
			"Canopy creates per-branch git worktrees, runs configurable setup\n" +
			"and teardown scripts, and pairs each workspace with a 4-pane tmux\n" +
			"session (nvim / claude / shell / server). One TUI lets you switch\n" +
			"between workspaces and resurrect them after reboots.\n",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Guard first: if canopy is running inside an existing tmux
			// session (and isn't an explicitly-allowed subcommand like
			// `version`), refuse before doing any other work. Surfaces
			// to the user as a cobra error, which prints to stderr and
			// exits non-zero — clean and visible.
			if err := enforceNoNestedTmux(cmd); err != nil {
				return err
			}
			teardown, err := clog.Init(debugFlag)
			if err != nil {
				return err
			}
			cobra.OnFinalize(teardown)
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// `canopy` with no subcommand is context-sensitive (see
			// route.go for the full dispatch table):
			//   - inside a project   → project TUI
			//   - in a fresh git repo → init splash
			//   - anywhere else      → global TUI
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("canopy: getwd: %w", err)
			}
			return routeRoot(cmd.Context(), cwd, cmd.OutOrStdout())
		},
	}
	root.PersistentFlags().BoolVar(&debugFlag, "debug", false, "enable DEBUG-level logging to ~/.canopy/log/canopy.log")

	root.AddCommand(versionCmd())
	root.AddCommand(initCmd())
	root.AddCommand(newCmd())
	root.AddCommand(lsCmd())
	root.AddCommand(switchCmd())
	root.AddCommand(rmCmd())
	root.AddCommand(reconcileCmd())
	root.AddCommand(mainCmd())
	root.AddCommand(retryCmd())

	if err := root.Execute(); err != nil {
		// cobra has already printed the error; just exit non-zero.
		os.Exit(1)
	}
}

// versionCmd resolves canopy's version using runtime/debug.ReadBuildInfo so
// that both `goreleaser`-built binaries (which inject `version` via -ldflags)
// and `go install`-built binaries (which read the module's vcs revision)
// produce a useful string with no extra build machinery.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print canopy's version, commit, and build date.",
		// version is the one subcommand we let users run from inside an
		// existing tmux session — it's the canonical "is canopy
		// installed?" probe and must answer regardless of context. See
		// enforceNoNestedTmux + allowInTmuxAnnotation in guard.go.
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		Run: func(cmd *cobra.Command, args []string) {
			v, commit, date := versionInfo()
			fmt.Fprintf(cmd.OutOrStdout(), "canopy %s (commit %s, built %s)\n", v, commit, date)
		},
	}
}

// versionInfo returns (version, commit, date), filling in the gaps from
// runtime/debug.BuildInfo when the ldflags-injected `version` is still "dev".
func versionInfo() (string, string, string) {
	v := version
	commit := "unknown"
	date := "unknown"

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v, commit, date
	}

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				commit = s.Value[:7]
			} else {
				commit = s.Value
			}
		case "vcs.time":
			date = s.Value
		}
	}

	// `go install` doesn't set ldflags, so version stays "dev". Fall back
	// to the module version (e.g. "v0.1.0") if the build info has it.
	if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}

	return v, commit, date
}
