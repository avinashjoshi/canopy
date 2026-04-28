package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// reconcileCmd returns the `canopy reconcile` cobra subcommand.
//
// Reconcile walks the workspaces in state.json, compares each to actual
// disk + tmux state, and updates the status field where they disagree.
// It NEVER deletes a row — orphaned workspaces (whose dir has been
// hand-removed) get marked orphaned and stay in state.json until the
// user runs `canopy rm` explicitly.
//
// The same logic runs implicitly inside `canopy switch` (lazy reconcile);
// this subcommand is the explicit handle for "I think state.json is out
// of sync, fix it without doing anything else."
func reconcileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Update workspace statuses to match disk + tmux reality",
		Long: "Walks state.json and corrects each workspace's status based on\n" +
			"whether its dir is still on disk and whether its tmux session is\n" +
			"alive. Stale ready -> stopped, missing dir -> orphaned, crashed\n" +
			"setup -> broken. Never deletes rows; use `canopy rm` for that.",
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := loadManager()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			// 1. Update statuses for known workspaces.
			changes, err := mgr.Reconcile(ctx)
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to reconcile — every workspace already matches reality.")
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Updated %d workspace(s):\n", len(changes))
				for _, c := range changes {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s -> %s\n", c.Name, c.From, c.To)
				}
			}

			// 2. Discover orphan dirs (workspace dirs on disk but not in
			// state.json). Read-only — we don't auto-prune, the user might
			// want them. Print suggested commands so they can clean up
			// without thinking too hard.
			orphans, err := mgr.DiscoverOrphans(ctx)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: orphan scan failed: %v\n", err)
				return nil
			}
			if len(orphans) == 0 {
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nFound %d orphan workspace dir(s) not in state.json:\n", len(orphans))
			for _, o := range orphans {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", o.Path)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nTo remove orphans (NOT reversible — uncommitted work is lost):")
			for _, o := range orphans {
				fmt.Fprintf(cmd.OutOrStdout(), "  git -C %s worktree remove --force %s && git -C %s branch -D %s\n",
					mgr.Cfg.ProjectRoot, o.Path, mgr.Cfg.ProjectRoot, o.Name)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\n(`canopy adopt <name>` to take ownership without removing — coming in v0.5.)")
			return nil
		},
	}
}
