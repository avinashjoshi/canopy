package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/host"
)

// hostCmd returns the `canopy host` parent command. Subcommands manage
// the ~/.canopy/hosts.json registry: add a remote SSH-reachable canopy
// installation, list registered hosts, remove one.
//
// v0.17.0 Phase 1a: registry is metadata-only — `canopy host add` does
// NOT verify the SSH target is reachable or that canopy is installed
// on the remote. That's a Phase 1d concern (the huh-wizard
// `canopy host project init` flow ping-probes before registering).
// Bare add lets users seed registrations against hosts that aren't
// online yet (a Fly Machine that's stopped, a home server that's
// asleep), which matches "remote workspaces survive disconnects" thesis.
//
// See docs/design/v0.17-remote-workspaces.md.
func hostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Manage registered remote canopy hosts (v0.17.0)",
		Long: "Hosts let you dispatch `canopy new --on <name>` and `canopy switch --on <name>` " +
			"to a remote canopy installation reachable over SSH. The registry lives at " +
			"~/.canopy/hosts.json. Project paths registered alongside the SSH target mean " +
			"you don't have to pass --remote-cwd every command.",
	}
	cmd.AddCommand(hostAddCmd())
	cmd.AddCommand(hostLsCmd())
	cmd.AddCommand(hostRmCmd())
	return cmd
}

func hostAddCmd() *cobra.Command {
	var projectPath string
	c := &cobra.Command{
		Use:   "add <name> <ssh-target>",
		Short: "Register a remote canopy host (e.g. `canopy host add tower avi@tower.tail.ts.net`)",
		Long: "Registers a name → SSH-target mapping in ~/.canopy/hosts.json. " +
			"--project-path is the remote directory where canopy runs (where the host's " +
			"canopy.json walk-up succeeds); future `canopy new --on <name>` invocations " +
			"cd there before dispatching. Name must not look like an SSH target (no @ or :).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			h := host.Host{
				SSHTarget:   args[1],
				ProjectPath: projectPath,
			}
			if err := reg.Add(args[0], h); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Registered host %q → %s\n", args[0], args[1])
			if projectPath != "" {
				fmt.Fprintf(cmd.OutOrStdout(),
					"  project_path: %s\n", projectPath)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(),
					"  Tip: pass --project-path /path/to/project so you don't need --remote-cwd on every command.\n")
			}
			return nil
		},
	}
	c.Flags().StringVar(&projectPath, "project-path", "",
		"absolute path to the project directory on the remote (where canopy.json walk-up succeeds)")
	return c
}

func hostLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List registered remote hosts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			list, err := reg.List()
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(),
					"No hosts registered. Try: canopy host add tower avi@tower.tail.ts.net --project-path /home/avi/Work/yourproject")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSSH TARGET\tPROJECT PATH")
			for _, h := range list {
				project := h.ProjectPath
				if project == "" {
					project = "—"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", h.Name, h.SSHTarget, project)
			}
			return tw.Flush()
		},
	}
}

func hostRmCmd() *cobra.Command {
	// --force reserved for Phase 1b when remotes-cache.json exists and
	// we can refuse rm if the host has cached workspaces. Phase 1a
	// just removes from the registry; the flag is here so we don't
	// have to change the CLI surface later.
	var force bool
	c := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a registered host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			if err := reg.Remove(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed host %q.\n", args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false,
		"reserved for Phase 1b: force removal even if the host has cached workspaces")
	return c
}

// loadHostRegistry opens ~/.canopy/hosts.json. Used by all `canopy host`
// subcommands AND by the --on resolver in new.go/switch.go.
func loadHostRegistry() (*host.Registry, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("loadHostRegistry: $HOME not set: %w", err)
	}
	return host.NewRegistry(filepath.Join(home, ".canopy"))
}
