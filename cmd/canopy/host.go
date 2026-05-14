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
// installation, list registered hosts, remove one, and manage the
// projects-on-host map managed via the top-level `canopy project` namespace.
//
// Schema (v0.17.0 Phase 1a, registry version 2):
//
//	{
//	  "hosts": {
//	    "tower": {
//	      "type": "ssh",
//	      "ssh_target": "avi@tower.tail.ts.net",
//	      "projects": {
//	        "canopy": "/home/avi/Work/canopy",
//	        "cravd":  "/home/avi/Work/cravd"
//	      }
//	    }
//	  }
//	}
//
// Host registration is metadata-only — `canopy host add` does NOT
// verify the SSH target is reachable. Bare add lets users seed
// registrations against hosts that aren't online yet (a Fly Machine
// that's stopped, a home server that's asleep), which matches the
// "remote workspaces survive disconnects" thesis. Phase 1d's huh
// wizard `canopy project init --on <host>` adds connectivity-probing UX.
//
// See docs/design/v0.17-remote-workspaces.md.
func hostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Manage registered remote canopy hosts (v0.17.0)",
		Long: "Hosts let you dispatch `canopy new --on <name>` and `canopy switch --on <name>` " +
			"to a remote canopy installation reachable over SSH. The registry lives at " +
			"~/.canopy/hosts.json. A host can serve multiple projects — each with its own " +
			"remote path — managed via `canopy project add/ls/rm --on <host>`.",
	}
	cmd.AddCommand(hostAddCmd())
	cmd.AddCommand(hostLsCmd())
	cmd.AddCommand(hostShowCmd())
	cmd.AddCommand(hostRmCmd())
	cmd.AddCommand(hostInstallCmd())
	cmd.AddCommand(hostClipboardCmd())
	return cmd
}

// hostShowCmd renders the detailed view of one host: SSH target, type,
// added timestamp, and the full list of registered projects. Replaces
// the awkward-reading `canopy host project ls <host>` (which was just
// a project list with no host context). Now project-related output
// has a single natural home.
func hostShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show details for one host (SSH target, projects, etc.)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			h, err := reg.Resolve(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "HOST\t%s\n", h.Name)
			fmt.Fprintf(tw, "SSH TARGET\t%s\n", h.SSHTarget)
			fmt.Fprintf(tw, "TYPE\t%s\n", h.Type)
			fmt.Fprintf(tw, "ADDED\t%s\n", h.AddedAt.Local().Format("2006-01-02 15:04"))
			tw.Flush()

			projs, err := reg.ListProjects(args[0])
			if err != nil {
				return err
			}
			if len(projs) == 0 {
				fmt.Fprintf(out, "\nPROJECTS (0)\n  — none yet. Run: canopy project add <project-name> <remote-path> --on %s\n", h.Name)
				return nil
			}
			fmt.Fprintf(out, "\nPROJECTS (%d)\n", len(projs))
			ptw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, p := range projs {
				fmt.Fprintf(ptw, "  %s\t%s\n", p.Name, p.Path)
			}
			return ptw.Flush()
		},
	}
}

func hostAddCmd() *cobra.Command {
	var interactive bool
	c := &cobra.Command{
		Use:   "add [<name> <ssh-target>]",
		Short: "Register a remote canopy host (positional args, or --interactive for a guided form)",
		Long: "Registers a name → SSH-target mapping in ~/.canopy/hosts.json.\n\n" +
			"Two modes:\n" +
			"  canopy host add tower avi@tower.tail.ts.net    # positional, no probe\n" +
			"  canopy host add --interactive                  # huh form + connectivity probe + ssh-copy-id offer\n\n" +
			"Bare add registers no projects; use `canopy project add <name> <path> --on <host>` " +
			"to tell canopy where each project lives on this host. Name must not look like an " +
			"SSH target (no @ or :).",
		Args: func(cmd *cobra.Command, args []string) error {
			if interactive {
				if len(args) != 0 {
					return fmt.Errorf("--interactive takes no positional args (use the form)")
				}
				return nil
			}
			if len(args) != 2 {
				return fmt.Errorf("accepts 2 positional args (name, ssh-target) OR --interactive")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if interactive {
				return runHostAddWizard(cmd.Context(), os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			if err := reg.Add(args[0], host.Host{SSHTarget: args[1]}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Registered host %q → %s\n", args[0], args[1])
			fmt.Fprintf(cmd.OutOrStdout(),
				"\nNext: register one or more projects on %s:\n  canopy project add <project-name> <remote-path> --on %s\n",
				args[0], args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&interactive, "interactive", false,
		"open a guided form (huh) that prompts for name + ssh-target, probes connectivity, and offers ssh-copy-id if key auth isn't set up")
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
					"No hosts registered. Try: canopy host add tower avi@tower.tail.ts.net")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSSH TARGET\tPROJECTS")
			for _, h := range list {
				projects := fmt.Sprintf("%d", len(h.Projects))
				if len(h.Projects) == 0 {
					projects = "— (run `canopy project add ... --on " + h.Name + "`)"
				} else {
					names := make([]string, 0, len(h.Projects))
					for n := range h.Projects {
						names = append(names, n)
					}
					projects = fmt.Sprintf("%d (%s)", len(h.Projects), joinNamesShort(names))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", h.Name, h.SSHTarget, projects)
			}
			return tw.Flush()
		},
	}
}

func hostRmCmd() *cobra.Command {
	// --force reserved for Phase 1b when remotes-cache.json exists and
	// we can refuse rm if the host has cached workspaces.
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

// joinNamesShort formats a sorted list of project names for the ls
// output, truncating long lists with an ellipsis so the table stays
// readable when a host has 10+ projects.
func joinNamesShort(names []string) string {
	const max = 3
	if len(names) <= max {
		return joinComma(names)
	}
	return joinComma(names[:max]) + ", …"
}

func joinComma(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
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
