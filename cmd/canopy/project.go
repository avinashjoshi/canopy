package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// projectCmd returns the `canopy project` parent command. Manages
// project-on-host registrations (which project on which host, at
// which remote path). This is the top-level surface; the older
// `canopy host project add/rm` namespace is removed in this commit
// because the flat form reads better aloud:
//
//	canopy project add canopy /home/cassy/Work/canopy --on tower
//
// vs. the nested form's
//
//	canopy host project add tower canopy /home/cassy/Work/canopy
//
// Also unlocks the cross-host `canopy project ls` view: "show me
// everywhere I have stuff registered."
//
// --on flag is shared with `canopy new --on` and `canopy switch --on`
// for verb consistency: it always means "which host should this verb
// act on." For project subcommands it's a registry name only (raw
// SSH targets aren't valid here — the host has to be registered first
// via `canopy host add`).
func projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage project-on-host registrations (add/ls/rm)",
		Long: "A project-on-host registration tells canopy where a project lives on a remote host. " +
			"Once registered, `canopy new --on tower` (from inside the local project) auto-resolves " +
			"to the right remote directory. Project names should match the local project's directory " +
			"basename so cwd-driven dispatch works without typing paths.",
	}
	cmd.AddCommand(projectAddCmd())
	cmd.AddCommand(projectLsCmd())
	cmd.AddCommand(projectRmCmd())
	return cmd
}

func projectAddCmd() *cobra.Command {
	var onHost string
	c := &cobra.Command{
		Use:   "add <project-name> <remote-path> --on <host>",
		Short: "Register a project on a host (e.g. `canopy project add canopy /home/cassy/Work/canopy --on tower`)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if onHost == "" {
				return fmt.Errorf("--on <host> is required (which host should this project be registered on?)")
			}
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			if err := reg.AddProject(onHost, args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Registered project %q on host %q → %s\n", args[0], onHost, args[1])
			return nil
		},
	}
	c.Flags().StringVar(&onHost, "on", "", "host to register this project on (required; must be in `canopy host ls`)")
	_ = c.MarkFlagRequired("on")
	return c
}

func projectLsCmd() *cobra.Command {
	var onHost string
	c := &cobra.Command{
		Use:   "ls",
		Short: "List project registrations across all hosts, or filter with --on",
		Long: "Without --on: shows every (host, project) pair in the registry. With --on, filters " +
			"to a single host's projects. Sorted by host then by name.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			if onHost != "" {
				projs, err := reg.ListProjects(onHost)
				if err != nil {
					return err
				}
				if len(projs) == 0 {
					fmt.Fprintf(out,
						"No projects on %q. Run: canopy project add <name> <path> --on %s\n",
						onHost, onHost)
					return nil
				}
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tREMOTE PATH")
				for _, p := range projs {
					fmt.Fprintf(tw, "%s\t%s\n", p.Name, p.Path)
				}
				return tw.Flush()
			}

			// Cross-host listing.
			all, err := reg.ListAllProjects()
			if err != nil {
				return err
			}
			if len(all) == 0 {
				fmt.Fprintln(out, "No projects registered on any host. Try: canopy project add <name> <path> --on <host>")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "HOST\tPROJECT\tREMOTE PATH")
			for _, e := range all {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", e.HostName, e.Name, e.Path)
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&onHost, "on", "", "filter to one host's projects (omit for all-hosts view)")
	return c
}

func projectRmCmd() *cobra.Command {
	var onHost string
	c := &cobra.Command{
		Use:   "rm <project-name> --on <host>",
		Short: "Remove a project registration from a host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if onHost == "" {
				return fmt.Errorf("--on <host> is required (which host should this project be removed from?)")
			}
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			if err := reg.RemoveProject(onHost, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed project %q from host %q.\n", args[0], onHost)
			return nil
		},
	}
	c.Flags().StringVar(&onHost, "on", "", "host to remove this project from (required)")
	_ = c.MarkFlagRequired("on")
	return c
}
