package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/hooks"
	"github.com/avinashjoshi/canopy/internal/settings"
	"github.com/avinashjoshi/canopy/internal/state"
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
			tc := tmux.New()
			session := tmux.SafeName(cfg.Project) + "-main"

			alive, err := tc.HasSession(ctx, session)
			if err != nil {
				return err
			}
			if !alive {
				// Determine project's main port via the same EnsureProjectBase
				// flow that `canopy new` uses, so canopy main and canopy new
				// agree on the project's base.
				port, err := mainPort(cfg.Project)
				if err != nil {
					return err
				}

				env := hooks.WorkspaceEnv(cfg.ProjectRoot, cfg.ProjectRoot, port)
				fmt.Fprintf(cmd.OutOrStdout(),
					"Building main session for %s at %s\n  port: %d (CANOPY_PORT)\n",
					cfg.Project, cfg.ProjectRoot, port)

				if err := buildMainSession(ctx, tc, session, cfg.ProjectRoot, env); err != nil {
					return err
				}
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Attaching tmux session %s...\n", session)
			return tc.Attach(ctx, session)
		},
	}
}

// mainPort returns the project's reserved base port. Loads settings and
// state; uses state.WithLock to atomically allocate-or-fetch the project's
// base, mirroring workspace.Manager.Create's usage so the two stay in sync.
func mainPort(project string) (int, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, fmt.Errorf("home dir: %w", err)
	}
	canopyHome := filepath.Join(home, ".canopy")

	store, err := state.NewStore(canopyHome)
	if err != nil {
		return 0, err
	}
	cfg, err := settings.Load(canopyHome)
	if err != nil {
		return 0, err
	}

	var base int
	err = store.WithLock(func(s *state.State) error {
		b, _, err := s.EnsureProjectBase(
			project,
			cfg.Ports.Base,
			cfg.Ports.ProjectStride,
			workspace.MaxProjects,
		)
		if err != nil {
			return err
		}
		base = b
		return nil
	})
	if err != nil {
		return 0, err
	}
	return base, nil
}

// buildMainSession creates the tdl-style 3-pane layout for `canopy main`.
// Same shape as workspace sessions (15% shell bottom, 30% claude top-right,
// ~70% nvim top-left); the env arg sets CANOPY_PORT etc. at the session
// level so commands typed in the shell pane (notably `bin/dev`) can read
// the project's main port.
func buildMainSession(ctx context.Context, tc *tmux.Client, session, projectRoot string, env []string) error {
	if err := tc.Create(ctx, session, projectRoot, `nvim; exec "$SHELL"`, env...); err != nil {
		return err
	}
	if err := tc.SplitPane(ctx, session, projectRoot, "", tmux.SplitVertical, 15); err != nil {
		return err
	}
	// `claude --continue || claude` falls back to a fresh session when
	// there's no prior conversation for this dir — without the ||
	// fallback, claude prints "no conversation found to continue" and
	// the keep-alive wrapper silently drops the user to a shell. The
	// fallback gives them a usable claude either way.
	if err := tc.SplitPane(ctx, session, projectRoot, `claude --continue || claude; exec "$SHELL"`, tmux.SplitHorizontal, 30); err != nil {
		return err
	}
	return nil
}
