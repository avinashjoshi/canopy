// Command canopy run executes scripts.run from the nearest canopy.json,
// replacing the canopy process with the run script via syscall.Exec.
//
// Designed as a one-keystroke replacement for typing "bin/dev" (or
// whatever the project's run script is). Bind it to a tmux key via
// `canopy install tmux` for a workflow like:
//
//	<prefix>r → server starts in current pane, owns the tty until Ctrl-C
//
// Inherits the current shell's stdin/stdout/stderr (via syscall.Exec —
// canopy's process image is replaced, no fork, no stdio piping). Inherits
// every CANOPY_* env var the workspace tmux session set, so scripts.run
// sees CANOPY_PORT etc. without canopy having to thread them through.
//
// This subcommand intentionally does NOT spawn the run script in the
// background or in a different pane. The user wanted "type fewer
// characters to start the server" — not "auto-orchestrate a pane
// layout." Pane orchestration belongs in v0.6's lifecycle layer.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/config"
)

var runLog = clog.Pkg("run")

func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute scripts.run from canopy.json (the project's dev server).",
		Long: `Reads scripts.run from the nearest canopy.json (walked up from cwd) and
execs it in place. Inherits the calling shell's stdin/stdout/stderr and
any CANOPY_* env vars set by the workspace tmux session.

Designed as a one-keystroke replacement for typing the project's dev
server command. Bind to a tmux key via 'canopy install tmux' (default:
<prefix>r) for one-stroke server start.

Refuses if there's no canopy.json above cwd, or if scripts.run is empty.
Use 'canopy main' or attach to a workspace first to land in a directory
that has canopy.json.
`,
		// Allow inside tmux: this command is meant for workspace shell
		// panes — that's the entire point. The nested-tmux concern that
		// motivated the guard ("nesting tmux confuses attach plumbing")
		// doesn't apply: canopy run never touches tmux state.
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE:        runRunCmd,
	}
	return cmd
}

func runRunCmd(_ *cobra.Command, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("canopy run: getwd: %w", err)
	}

	cfg, err := config.DiscoverAndLoad(cwd)
	if err != nil {
		return fmt.Errorf("canopy run: %w", err)
	}

	if cfg.Scripts.Run == "" {
		return fmt.Errorf(
			"canopy run: scripts.run is empty in %s/canopy.json — nothing to run.\n"+
				"  Edit canopy.json and set \"scripts\": {\"run\": \"bin/dev\"} or similar.",
			cfg.ProjectRoot)
	}

	// Resolve scripts.run relative to project root. Absolute paths pass
	// through. The hooks package follows the same convention for setup/
	// archive scripts; canopy run reuses the convention for symmetry.
	runPath := cfg.Scripts.Run
	if !filepath.IsAbs(runPath) {
		runPath = filepath.Join(cfg.ProjectRoot, runPath)
	}

	// chdir to project root before exec so the run script sees a stable
	// cwd regardless of where the user invoked from. Most dev servers
	// expect this (look up package.json, Gemfile, etc. at cwd).
	if err := os.Chdir(cfg.ProjectRoot); err != nil {
		return fmt.Errorf("canopy run: chdir %s: %w", cfg.ProjectRoot, err)
	}

	runLog.Info("run.exec", "path", runPath, "project", cfg.Project)

	// syscall.Exec replaces canopy's process image with the run script.
	// stdin/stdout/stderr stay attached to the calling tty; signals
	// (Ctrl-C, SIGTERM) go directly to the script. This is the same
	// shape internal/tmux/session.go:Attach uses for `tmux attach`.
	//
	// argv[0] is the script path, NOT "canopy" — the run script
	// inspecting os.Args[0] (or its equivalent) sees its own name.
	if err := syscall.Exec(runPath, []string{runPath}, os.Environ()); err != nil {
		return fmt.Errorf("canopy run: exec %s: %w", runPath, err)
	}
	// syscall.Exec returns only on failure; the success path never reaches
	// here (the process image is replaced before return).
	return nil
}
