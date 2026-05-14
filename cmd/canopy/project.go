package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/state"
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
			projectName, remotePath := args[0], args[1]
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			// Pre-flight: probe the remote path so an obvious mismatch
			// (e.g. local user "avi" path registered for a host whose
			// user is "jarvis") surfaces at register-time, not three
			// commands later inside a busy popup. Best-effort: if the
			// probe fails for transport reasons (host asleep, key not
			// set up yet) we still register — the user might be
			// pre-configuring. Only the path-missing path warns.
			h, hostErr := reg.Resolve(onHost)
			if hostErr == nil && h.SSHTarget != "" {
				ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
				if probeErr := probeRemoteCwd(ctx, h.SSHTarget, remotePath); probeErr != nil {
					if ee, ok := probeErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
						// `test -d` exit 1 = path doesn't exist (vs. ssh
						// transport errors which give different codes).
						// Warn loudly but register anyway — the user
						// might be registering ahead of a `git clone` on
						// the remote, and refusing here would break that
						// workflow.
						fmt.Fprintf(cmd.ErrOrStderr(),
							"warning: path %q does not exist on host %q.\n"+
								"  The path is still being registered, but `canopy new --on %s` and\n"+
								"  `canopy switch --on %s` will fail with `cd: No such file or directory`\n"+
								"  until the path exists on the remote (clone the repo or fix the path).\n",
							remotePath, onHost, onHost, onHost)
					}
				}
				cancel()
			}
			if err := reg.AddProject(onHost, projectName, remotePath); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Registered project %q on host %q → %s\n", projectName, onHost, remotePath)
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
		Short: "List project registrations across all hosts (local + remote), or filter with --on",
		Long: "Without --on: shows every project canopy knows about — local projects from state.json " +
			"(initialized via `canopy init`) AND remote-host project registrations from hosts.json. " +
			"Local projects appear with HOST=\"local\". With --on <host>: filters to one host's projects; " +
			"--on local shows just the local projects.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			// Build the combined list. Local projects come from
			// state.json (Projects map keyed by canonical path);
			// remote projects come from the host registry. Sort by
			// host then by name for stable output.
			var entries []projectListEntry
			if onHost == "" || onHost == "local" {
				localEntries, err := loadLocalProjects()
				if err != nil {
					return err
				}
				entries = append(entries, localEntries...)
			}
			if onHost != "local" {
				reg, err := loadHostRegistry()
				if err != nil {
					return err
				}
				if onHost != "" {
					projs, err := reg.ListProjects(onHost)
					if err != nil {
						return err
					}
					for _, p := range projs {
						entries = append(entries, projectListEntry{Host: onHost, Name: p.Name, Path: p.Path})
					}
				} else {
					all, err := reg.ListAllProjects()
					if err != nil {
						return err
					}
					for _, e := range all {
						entries = append(entries, projectListEntry{Host: e.HostName, Name: e.Name, Path: e.Path})
					}
				}
			}

			if len(entries) == 0 {
				switch onHost {
				case "local":
					fmt.Fprintln(out, "No local projects. Run `canopy init` from a project directory.")
				case "":
					fmt.Fprintln(out, "No projects anywhere. Run `canopy init` locally or `canopy project add <name> <path> --on <host>` for a remote.")
				default:
					fmt.Fprintf(out, "No projects on %q. Run: canopy project add <name> <path> --on %s\n", onHost, onHost)
				}
				return nil
			}

			// When filtered to a single host, drop the HOST column —
			// it's redundant and the table reads cleaner without it.
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			if onHost != "" {
				fmt.Fprintln(tw, "NAME\tPATH")
				for _, e := range entries {
					fmt.Fprintf(tw, "%s\t%s\n", e.Name, e.Path)
				}
			} else {
				fmt.Fprintln(tw, "HOST\tPROJECT\tPATH")
				for _, e := range entries {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Host, e.Name, e.Path)
				}
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&onHost, "on", "",
		"filter: <hostname> for one remote, 'local' for laptop-only, omitted for everything")
	return c
}

// projectListEntry is the unified shape used by `canopy project ls`.
// Combines local projects (from state.json) and remote registrations
// (from hosts.json) into one cross-source view. Host="local" for
// laptop projects; otherwise the registered host name.
type projectListEntry struct {
	Host string
	Name string
	Path string
}

// loadLocalProjects reads state.json's Projects map and converts each
// entry to a projectListEntry with Host="local". Sorted by basename.
//
// Empty state.json (first-time-canopy laptop) returns an empty slice,
// not an error — local-projects-empty is a normal first-run state.
func loadLocalProjects() ([]projectListEntry, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("loadLocalProjects: $HOME not set: %w", err)
	}
	store, err := state.NewStore(filepath.Join(home, ".canopy"))
	if err != nil {
		return nil, fmt.Errorf("loadLocalProjects: open state.Store: %w", err)
	}
	st, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("loadLocalProjects: read state.json: %w", err)
	}
	out := make([]projectListEntry, 0, len(st.Projects))
	for root := range st.Projects {
		out = append(out, projectListEntry{
			Host: "local",
			Name: filepath.Base(root),
			Path: root,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
