package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// mainCmd returns the `canopy main` cobra subcommand.
//
// `canopy main` opens (or attaches to) a tmux session anchored at the
// project root — the source repo itself, where the main/default branch
// lives. Useful for working on the main branch without first creating
// a worktree.
//
// Lighter than `canopy new`:
//   - No git worktree is created (the source repo is already a "worktree"
//     in git's sense).
//   - No setup or archive scripts run (the source repo is already set up).
//   - No state.json workspace row is written (the main session is
//     ephemeral; if the user kills it, `canopy main` just creates a fresh
//     one).
//
// What it DOES share with workspaces: the project's port base. Each
// project's port_base is reserved for `canopy main` (workspaces start
// at base + workspace_stride). canopy main exports CANOPY_PORT=<base>
// in the session env, so `bin/dev` typed in the shell pane binds to
// the project's main port, parallel to how workspaces bind to their
// allocated ports.
//
// Layout matches workspace sessions: nvim top-left, claude top-right
// (with --continue || claude fallback), shell full-width bottom.
func mainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "main",
		Short: "Open or attach to a tmux session in the project root (the main repo)",
		Long: "Anchors a tmux session at the project root with CANOPY_PORT=<base>\n" +
			"so `bin/dev` in the shell pane binds to your project's main port\n" +
			"(40000 by default for the first project, 41000 for the next, etc.).\n\n" +
			"The session name is `<project>-main` and uses the standard 3-pane\n" +
			"layout. If a session already exists, this just attaches to it.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := getCwd()
			if err != nil {
				return err
			}

			cfg, err := loadConfig(cwd)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			mgr, err := workspace.New(cfg)
			if err != nil {
				return err
			}

			session, err := mgr.EnsureMainSession(ctx)
			if err != nil {
				return err
			}

			t := tmux.New()
			propagateRemoteHostEnv(ctx, t, session)
			fmt.Fprintf(cmd.OutOrStdout(), "Attaching tmux session %s...\n", session)
			return t.Attach(ctx, session)
		},
	}
}
