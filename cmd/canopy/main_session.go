package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/tmux"
)

// mainCmd returns the `canopy main` cobra subcommand.
//
// `canopy main` opens (or attaches to) a tmux session anchored at the
// project root — the source repo itself, where the main/default branch
// lives. Useful for working on the main branch without first creating
// a worktree.
//
// This is intentionally lighter than `canopy new`:
//   - No git worktree is created (the source repo is already a "worktree"
//     in git's sense).
//   - No port is allocated (the main repo doesn't need an isolated port).
//   - No setup or archive scripts run (the source repo is already set up).
//   - No state.json row is written (the main session is ephemeral; if
//     the user kills it, `canopy main` just creates a fresh one).
//
// The session uses canopy's standard 3-pane layout (nvim top-left,
// claude top-right, shell full-width bottom). Claude on resurrection
// uses --continue automatically because it's per-directory.
func mainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "main",
		Short: "Open or attach to a tmux session in the project root (the main repo)",
		Long: "Anchors a tmux session at the project root — the source repo where\n" +
			"the main branch lives — without creating a new worktree. The session\n" +
			"name is `<project>-main` and uses the same 3-pane layout as workspace\n" +
			"sessions (nvim / claude / shell).\n\n" +
			"If a session already exists (e.g. you ran `canopy main` earlier and\n" +
			"detached), this just attaches to it. If not, canopy builds the panes\n" +
			"and attaches.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := getCwd()
			if err != nil {
				return err
			}

			cfg, err := loadConfig(cwd)
			if err != nil {
				return err
			}

			tc := tmux.New()
			session := tmux.SafeName(cfg.Project) + "-main"
			ctx := cmd.Context()

			alive, err := tc.HasSession(ctx, session)
			if err != nil {
				return err
			}
			if !alive {
				fmt.Fprintf(cmd.OutOrStdout(), "Building main session for %s at %s...\n",
					cfg.Project, cfg.ProjectRoot)
				if err := buildMainSession(ctx, tc, session, cfg.ProjectRoot); err != nil {
					return err
				}
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Attaching tmux session %s...\n", session)
			return tc.Attach(ctx, session)
		},
	}
}

// buildMainSession creates the 3-pane layout for `canopy main`. Identical
// shape to workspace.buildSession but without the workspace-specific env
// or scripts; this session lives at the project root, not a worktree.
func buildMainSession(ctx context.Context, tc *tmux.Client, session, projectRoot string) error {
	if err := tc.Create(ctx, session, projectRoot, `nvim; exec "$SHELL"`); err != nil {
		return err
	}
	// Shell, full-width bottom.
	if err := tc.SplitPane(ctx, session, projectRoot, "", tmux.SplitVertical); err != nil {
		return err
	}
	// Claude top-right (--continue so prior conversation in this dir resumes).
	if err := tc.SplitPane(ctx, session, projectRoot, `claude --continue; exec "$SHELL"`, tmux.SplitHorizontal); err != nil {
		return err
	}
	return nil
}
