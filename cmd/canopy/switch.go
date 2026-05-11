package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// switchCmd returns the `canopy switch <name>` cobra subcommand.
//
// Before dispatching, switch runs a lazy reconcile: if the recorded
// status disagrees with reality (state says ready but tmux session is
// gone, or the dir has been hand-deleted), the row's status is updated
// in place. That way `canopy switch` always operates on the truth, not
// on whatever state.json said the last time canopy was used.
//
// Behavior by status (after reconcile):
//
//	ready      -> attach (syscall.Exec into tmux)
//	stopped    -> resurrect (rebuild tmux session, claude --continue),
//	              then attach
//	broken     -> print error log path, suggest canopy rm
//	orphaned   -> print warning, suggest canopy rm
//	setting_up -> print "still setting up", exit non-zero
func switchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "switch <name>",
		Short: "Attach to a workspace's tmux session (resurrect if stopped)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := loadManager()
			if err != nil {
				return err
			}
			name := args[0]
			ctx := cmd.Context()

			// Lazy reconcile: ensure status reflects reality before we act on it.
			// Errors here are non-fatal; if reconcile fails we proceed with the
			// stale status (and the user can re-run `canopy reconcile` directly).
			if _, err := mgr.Reconcile(ctx); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: reconcile failed: %v\n", err)
			}

			ws, err := mgr.Find(ctx, name)
			if err != nil {
				return err
			}

			switch ws.Status {
			case state.StatusReady:
				// Backfill @canopy-role tags for v0.15-style sessions that
				// never went through the v0.16+ buildSession (which tags at
				// creation). Best-effort: errors logged, never block attach.
				_ = workspace.BackfillRoles(ctx, mgr.Tmux, ws.TmuxSessionName(), mgr.Cfg.Agent.Type)
				fmt.Fprintf(cmd.OutOrStdout(), "Attaching tmux session %s...\n", ws.TmuxSessionName())
				return mgr.Tmux.Attach(ctx, ws.TmuxSessionName())

			case state.StatusStopped:
				fmt.Fprintf(cmd.OutOrStdout(), "Resurrecting workspace %s...\n", name)
				revived, err := mgr.Resurrect(ctx, name)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Attaching tmux session %s...\n", revived.TmuxSessionName())
				return mgr.Tmux.Attach(ctx, revived.TmuxSessionName())

			case state.StatusBroken:
				fmt.Fprintf(cmd.ErrOrStderr(),
					"workspace %q is in status %q.\nLast error: %s\nSee ~/.canopy/log/canopy.log for details.\nRun `canopy rm %s` to clean up.\n",
					name, ws.Status, ws.LastError, name)
				return fmt.Errorf("workspace %q is broken", name)

			case state.StatusOrphaned:
				fmt.Fprintf(cmd.ErrOrStderr(),
					"workspace %q has no on-disk dir at %s.\nRun `canopy rm %s` to drop the state row.\n",
					name, ws.Path, name)
				return fmt.Errorf("workspace %q is orphaned", name)

			case state.StatusSettingUp:
				fmt.Fprintf(cmd.ErrOrStderr(),
					"workspace %q is still setting up. Try again in a moment.\n", name)
				return fmt.Errorf("workspace %q is still setting up", name)

			default:
				return fmt.Errorf("workspace %q has unknown status %q", name, ws.Status)
			}
		},
	}
}
