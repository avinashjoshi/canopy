package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// rmFlags holds parsed --yes (skip confirmation prompt).
var rmFlags struct {
	yes bool
}

// rmCmd returns the `canopy rm <name>` cobra subcommand.
//
// Removal runs scripts.archive (DB drop, server kill), kills the tmux
// session, removes the git worktree (with --force; we accept that the
// user explicitly asked for removal), and drops the state row.
//
// By default rmCmd prompts for confirmation. --yes (or -y) skips it for
// scripting.
func rmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Tear down a workspace (scripts.archive + git worktree remove + state cleanup)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := loadManager()
			if err != nil {
				return err
			}
			name := args[0]
			ctx := cmd.Context()

			ws, err := mgr.Find(ctx, name)
			if err != nil {
				return err
			}

			if !rmFlags.yes {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Remove workspace %q?\n  branch:  %s\n  path:    %s\n  port:    %d\n  status:  %s\n\nThis runs scripts.archive then deletes the git worktree.\nProceed? [y/N] ",
					name, ws.Branch, ws.Path, ws.Port, ws.Status)
				ok, err := readYesNo(cmd.InOrStdin())
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}

			if err := mgr.Remove(ctx, name, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed workspace %q.\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&rmFlags.yes, "yes", "y", false, "skip confirmation prompt")
	return cmd
}

// readYesNo reads one line from r (typically stdin) and reports whether
// the user typed something that means yes. Anything else (including EOF)
// is no.
func readYesNo(r io.Reader) (bool, error) {
	reader := bufio.NewReader(r)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}
