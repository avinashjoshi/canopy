package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// lsCmd returns the `canopy ls` cobra subcommand. Output is one line per
// workspace, machine-friendly (whitespace-aligned, no decorations), so
// scripts can pipe through cut/awk.
//
// Output columns:
//
//	NAME            BRANCH          STATUS          PORT  TMUX_SESSION
//	bold-falcon     feature-x       ready           3001  cravd-bold-falcon
func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List workspaces for the current project",
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := loadManager()
			if err != nil {
				return err
			}
			workspaces, err := mgr.List(cmd.Context())
			if err != nil {
				return err
			}
			if len(workspaces) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No workspaces. Run `canopy new` to create one.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tBRANCH\tSTATUS\tPORT\tTMUX_SESSION")
			for _, w := range workspaces {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", w.Name, w.Branch, w.Status, w.Port, w.TmuxSession)
			}
			return tw.Flush()
		},
	}
}
