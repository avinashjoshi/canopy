package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/workspace"
)

// newWorkspaceFlags holds parsed CLI flags. Package-level so they're
// easy to test/inspect. v0.6 added --pr / --issue / --branch /
// --allow-local for the source-variant flows; the original --name
// and --no-attach still work as before.
var newWorkspaceFlags struct {
	name     string
	noAttach bool
	pr       int    // --pr <num>: check out this PR's branch into a workspace
	issue    int    // --issue <num>: create workspace, briefing references this issue
	branch   string // --branch <name>: check out an existing branch
	allowLoc bool   // --allow-local: with --branch, allow non-existent on origin
}

// newCmd returns the `canopy new` cobra subcommand.
//
// Source variants (mutually exclusive):
//
//	canopy new                       # fresh workspace, random name
//	canopy new --name fix-bug        # fresh, explicit name
//	canopy new --pr 42               # check out PR #42 into a new workspace
//	canopy new --issue 17            # implementation workspace seeded with issue body
//	canopy new --branch feat/x       # check out existing branch from origin
//	canopy new --branch feat/x --allow-local
//	                                 # check out existing local-only branch
func newCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new workspace and attach to its tmux session",
		Long: "Generates a random adjective-noun workspace name (or uses --name),\n" +
			"creates a git worktree, runs scripts.setup, builds the standard 4-pane\n" +
			"tmux session (nvim / claude / shell / scripts.run), and attaches.\n\n" +
			"Source variants (mutually exclusive):\n" +
			"  --pr <num>     check out PR <num>'s branch (briefing includes PR body)\n" +
			"  --issue <num>  fresh branch off main; briefing seeded with issue body\n" +
			"  --branch <n>   check out existing branch <n> from origin\n" +
			"  --allow-local  with --branch, allow checkout of a local-only branch",
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := loadManager()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			spec := workspace.SourceSpec{
				PR:         newWorkspaceFlags.pr,
				Issue:      newWorkspaceFlags.issue,
				Branch:     newWorkspaceFlags.branch,
				AllowLocal: newWorkspaceFlags.allowLoc,
			}
			opts, suggestedName, err := mgr.ResolveSource(ctx, spec)
			if err != nil {
				return err
			}
			// Pick the workspace name. Explicit --name beats the
			// source-derived suggestion, which beats namegen (the
			// empty string case, handled inside Manager.Create).
			name := newWorkspaceFlags.name
			if name == "" {
				name = suggestedName
			}

			ws, err := mgr.Create(ctx, name, opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
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
	cmd.Flags().IntVar(&newWorkspaceFlags.pr, "pr", 0,
		"check out the given PR number into a new workspace (uses gh)")
	cmd.Flags().IntVar(&newWorkspaceFlags.issue, "issue", 0,
		"seed the briefing with the given issue's body (uses gh)")
	cmd.Flags().StringVar(&newWorkspaceFlags.branch, "branch", "",
		"check out an existing branch (must exist on origin unless --allow-local)")
	cmd.Flags().BoolVar(&newWorkspaceFlags.allowLoc, "allow-local", false,
		"with --branch, allow a branch that exists only locally (no origin/<name>)")
	return cmd
}
