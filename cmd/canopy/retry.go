package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// retryCmd returns the `canopy retry <name>` cobra subcommand.
//
// Re-runs scripts.setup against an existing broken workspace without
// destroying the worktree, branch, or claude history. Used to recover
// from a fixable scripts.setup failure (missing config, network blip,
// fixed dependency conflict) — the user resolves whatever broke setup
// and runs `canopy retry <name>`.
//
// Only allowed on workspaces in `broken` status. Other statuses return
// a clear error pointing at the right verb (canopy switch for stopped,
// canopy rm for orphaned).
func retryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <name>",
		Short: "Re-run scripts.setup on a broken workspace (preserves worktree)",
		Long: "Use this when scripts.setup failed and you've fixed the underlying\n" +
			"issue. Workspace stays in place — same dir, same branch, same port,\n" +
			"same claude history — only scripts.setup re-runs. On success the\n" +
			"workspace flips to ready and the tmux session is built (if it\n" +
			"wasn't already). On failure the workspace stays broken with the\n" +
			"new error captured in last_error.\n\n" +
			"Only valid on workspaces in `broken` status. Other statuses point\n" +
			"at the right verb to use instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := loadManager()
			if err != nil {
				return err
			}
			name := args[0]
			ctx := cmd.Context()

			ws, err := mgr.RetrySetup(ctx, name, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				if ws != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"\nworkspace %q is in status %q.\nSee ~/.canopy/log/canopy.log for details.\n",
						ws.Name, ws.Status)
				}
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"\nWorkspace ready: %s\n  branch:  %s\n  path:    %s\n  port:    %d\n  session: %s\n",
				ws.Name, ws.Branch, ws.Path, ws.Port, ws.TmuxSession)
			fmt.Fprintf(cmd.OutOrStdout(),
				"\nRun `canopy switch %s` to attach.\n", ws.Name)
			return nil
		},
	}
}
