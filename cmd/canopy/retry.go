package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// retryCmd returns the `canopy retry <name>` cobra subcommand.
//
// Re-runs scripts.setup against an existing workspace without destroying
// the worktree, branch, or claude history. Used to recover from a
// fixable scripts.setup failure (the original use case) — the user
// resolves whatever broke setup and runs `canopy retry <name>`.
//
// Default-allowed on `broken` only. `ready` and `stopped` require
// --force because re-running setup on a healthy workspace can be
// destructive depending on what scripts.setup does (DB drops, port
// reservations, agent briefing files, etc.). Always refuses on
// `setting_up` (concurrent setup hazard) and `orphaned` (no dir).
func retryCmd() *cobra.Command {
	var (
		force     bool
		onHost    string
		remoteCwd string
	)
	cmd := &cobra.Command{
		Use:   "retry <name>",
		Short: "Re-run scripts.setup on a workspace (preserves worktree)",
		Long: "Use this when scripts.setup failed and you've fixed the underlying\n" +
			"issue. Workspace stays in place — same dir, same branch, same port,\n" +
			"same claude history — only scripts.setup re-runs. On success the\n" +
			"workspace flips to ready and the tmux session is built (if it\n" +
			"wasn't already). On failure the workspace stays broken with the\n" +
			"new error captured in last_error.\n\n" +
			"Default: only valid on `broken` workspaces. Pass --force to retry\n" +
			"on `ready` or `stopped` (e.g., to refresh a DB schema scripts.setup\n" +
			"creates). Always refuses on `setting_up` (another setup is running)\n" +
			"and `orphaned` (no on-disk dir to set up against).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

			// v0.17.0 Phase 1: --on dispatches to remote canopy.
			if onHost != "" {
				cwd, _ := os.Getwd()
				resolved, err := resolveOnForSwitch(onHost, localProjectBasename(cwd), remoteCwd)
				if err != nil {
					return err
				}
				remoteArgs := []string{name}
				if force {
					remoteArgs = append(remoteArgs, "--force")
				}
				return dispatchVerbToRemote(ctx, resolved, "retry", remoteArgs, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}

			mgr, err := loadManager()
			if err != nil {
				return err
			}

			ws, err := mgr.RetrySetup(ctx, name, force, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				if ws != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"\nworkspace %q is in status %q.\n",
						ws.Name, ws.Status)
					if ws.LastErrorHint != "" {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"hint: %s\n", ws.LastErrorHint)
					}
					fmt.Fprintf(cmd.ErrOrStderr(),
						"See ~/.canopy/log/canopy.log for full details.\n")
				}
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"\nWorkspace ready: %s\n  branch:  %s\n  path:    %s\n  port:    %d\n  session: %s\n",
				ws.Name, ws.Branch, ws.Path, ws.Port, ws.TmuxSessionName())
			fmt.Fprintf(cmd.OutOrStdout(),
				"\nRun `canopy switch %s` to attach.\n", ws.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"allow retry on ready/stopped workspaces (re-runs scripts.setup on a healthy workspace; can be destructive)")
	cmd.Flags().StringVar(&onHost, "on", "", "dispatch to remote canopy at <host or ssh-target> (v0.17.0)")
	cmd.Flags().StringVar(&remoteCwd, "remote-cwd", "", "with --on: cd to <path> on the remote before invoking canopy")
	return cmd
}
