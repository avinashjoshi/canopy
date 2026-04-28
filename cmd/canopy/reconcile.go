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
			changes, err := mgr.Reconcile(cmd.Context())
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to reconcile — every workspace already matches reality.")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated %d workspace(s):\n", len(changes))
			for _, c := range changes {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s -> %s\n", c.Name, c.From, c.To)
			}
			return nil
		},
	}
}
