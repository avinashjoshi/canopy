// Command canopy set-owner marks a workspace as belonging to someone
// else (work you're reviewing) or clears it back to yourself. It backs
// the TUI's `o` keybind and is independently usable from the CLI.
//
//	canopy set-owner <workspace> <login>    # mark as @login's (reviewing)
//	canopy set-owner <workspace> --clear    # reset to yours (no pill)
//	canopy set-owner <ws> <login> --on <host>   # same, on a remote canopy
//
// "Owner" is a single field with three render states (see
// state.Workspace.Owner): a foreign login renders an "@login" pill, the
// reserved self-marker (written by --clear) renders nothing and overrides
// the legacy "pr-sourced row → REVIEW pill" fallback, and empty derives
// from SourceKind. There is intentionally no way to pass an empty owner
// as a "clear" — that's what --clear is for, so a fat-fingered empty arg
// can't silently wipe ownership.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

func newSetOwnerCmd() *cobra.Command {
	var (
		clear     bool
		onHost    string
		remoteCwd string
	)
	cmd := &cobra.Command{
		Use:   "set-owner <workspace> [<login>]",
		Short: "Mark a workspace as someone else's to review, or clear it back to yours.",
		Long: `Set or clear a workspace's owner. A workspace with a foreign owner
shows an "@login" pill in the Workspaces tab so you can tell at a glance
which rows are your own work and which are PRs/branches you're reviewing.

  canopy set-owner pr-1234 octocat    # this is octocat's PR — I'm reviewing
  canopy set-owner pr-1234 --clear    # actually it's mine now; drop the pill

Workspaces created from a PR (canopy new --pr) are stamped with the PR
author automatically; this verb is for fixing that up or for branch /
local workspaces that had no owner to begin with.

With --on <host>, the change is dispatched to a remote canopy over SSH,
the same way canopy rm/retry --on work.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

			// Argument shape: exactly one of "<login>" or --clear.
			if clear && len(args) == 2 {
				return fmt.Errorf("set-owner: pass either a login or --clear, not both")
			}
			if !clear && len(args) == 1 {
				return fmt.Errorf("set-owner: give a login (e.g. `canopy set-owner %s octocat`) or use --clear to reset to yourself", name)
			}

			// v0.17-style --on dispatch: run the verb on a remote canopy.
			// Forward the user's raw login / --clear; the remote
			// re-normalizes so validation lives in one place.
			if onHost != "" {
				cwd, _ := os.Getwd()
				resolved, err := resolveOnForSwitch(onHost, localProjectBasename(cwd), remoteCwd)
				if err != nil {
					return err
				}
				remoteArgs := []string{name}
				if clear {
					remoteArgs = append(remoteArgs, "--clear")
				} else {
					remoteArgs = append(remoteArgs, args[1])
				}
				return dispatchVerbToRemote(ctx, resolved, "set-owner", remoteArgs, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}

			ownerVal := state.OwnerSelfMarker
			if !clear {
				norm, ok := state.NormalizeOwner(args[1])
				if !ok {
					return fmt.Errorf("set-owner: owner must be a non-empty login or name; use --clear to reset to yourself")
				}
				ownerVal = norm
			}

			mgr, err := loadManager()
			if err != nil {
				return err
			}
			if err := mgr.SetOwner(ctx, name, ownerVal); err != nil {
				if errors.Is(err, workspace.ErrWorkspaceNotFound) {
					return fmt.Errorf("set-owner: no workspace named %q in this project", name)
				}
				return fmt.Errorf("set-owner: %w", err)
			}

			if clear {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: owner cleared — this workspace is yours\n", name)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: owner set to @%s — marked as reviewing\n", name, ownerVal)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "reset the workspace back to yours (removes the owner pill)")
	cmd.Flags().StringVar(&onHost, "on", "", "dispatch to remote canopy at <host or ssh-target>")
	cmd.Flags().StringVar(&remoteCwd, "remote-cwd", "", "with --on: cd to <path> on the remote before invoking canopy")
	return cmd
}
