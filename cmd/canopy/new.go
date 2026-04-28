package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newWorkspaceFlags holds the parsed --name and --no-attach values.
// Package-level rather than per-Cmd so they're easy to test/inspect.
var newWorkspaceFlags struct {
	name     string
	noAttach bool
}

// newCmd returns the `canopy new` cobra subcommand.
//
// Usage:
//
//	canopy new                  # generates a random name (e.g. bold-falcon)
//	canopy new --name fix-bug   # explicit name
//	canopy new --no-attach      # create but don't auto-attach to tmux
func newCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new workspace and attach to its tmux session",
		Long: "Generates a random adjective-noun workspace name (or uses --name),\n" +
			"creates a git worktree, runs scripts.setup, builds the standard 4-pane\n" +
			"tmux session (nvim / claude / shell / scripts.run), and attaches.",
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := loadManager()
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			ws, err := mgr.Create(ctx, newWorkspaceFlags.name, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				// Even on failure, print the workspace summary if we have one
				// so the user knows where to find logs / what to clean up.
				if ws != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"\nworkspace %q is in status %q.\n",
						ws.Name, ws.Status)
					if ws.LastErrorHint != "" {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"hint: %s\n", ws.LastErrorHint)
					}
					fmt.Fprintf(cmd.ErrOrStderr(),
						"See ~/.canopy/log/canopy.log for full details.\n"+
							"Once you've fixed the issue, `canopy retry %s` re-runs scripts.setup\n"+
							"against the existing worktree (preserves branch, port, claude history).\n"+
							"Or `canopy rm %s` to drop it entirely.\n",
						ws.Name, ws.Name)
				}
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"\nWorkspace ready: %s\n  branch:  %s\n  path:    %s\n  port:    %d\n  session: %s\n",
				ws.Name, ws.Branch, ws.Path, ws.Port, ws.TmuxSession)

			if newWorkspaceFlags.noAttach {
				fmt.Fprintf(cmd.OutOrStdout(),
					"\nSkipping attach (--no-attach). Run `canopy switch %s` to attach later.\n", ws.Name)
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "\nAttaching tmux session %s...\n", ws.TmuxSession)
			// Attach replaces the canopy process via syscall.Exec on success.
			// If we return from Attach, it failed.
			return mgr.Tmux.Attach(ctx, ws.TmuxSession)
		},
	}
	cmd.Flags().StringVar(&newWorkspaceFlags.name, "name", "",
		"explicit workspace name (default: random adjective-noun)")
	cmd.Flags().BoolVar(&newWorkspaceFlags.noAttach, "no-attach", false,
		"don't auto-attach to the tmux session after creation")
	return cmd
}
