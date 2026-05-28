package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/avinashjoshi/canopy/internal/workspace"
	"github.com/spf13/cobra"
)

// rmFlags holds parsed --yes (skip confirmation prompt), --force
// (bypass v0.6 safety pre-flight checks), --on / --remote-cwd
// (v0.17.0 Phase 1 — dispatch to a remote canopy host).
var rmFlags struct {
	yes       bool
	force     bool
	onHost    string
	remoteCwd string
}

// rmCmd returns the `canopy rm <name>` cobra subcommand.
//
// Removal runs scripts.archive (DB drop, server kill), kills the tmux
// session, removes the git worktree, deletes the branch, and drops the
// state row.
//
// v0.6 adds a smart pre-flight safety check: refuses to proceed when
// the workspace has uncommitted changes, unpushed commits, or an open
// PR. The check protects against the "I just rm'd uncommitted work"
// moment. --force bypasses the check entirely (CI / scripted use);
// --yes only skips the confirmation prompt and DOES run the safety
// check (so scripts that pipe `yes` still get protection).
//
// --force also mirrors `rm -f`: a missing workspace is treated as
// idempotent success ("already removed") rather than a hard error.
// Strict mode (no --force) still errors out so typos surface.
//
// Edge: workspace in `orphaned` status (worktree dir gone) gracefully
// degrades — the safety check warns it can't verify uncommitted state
// and proceeds to confirm prompt, never blocks rm because the diagnostic
// itself failed.
func rmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Tear down a workspace (scripts.archive + git worktree remove + state cleanup)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

			// v0.17.0 Phase 1: --on dispatches to remote canopy. The
			// remote's `canopy rm` runs its full safety check +
			// scripts.archive against the workspace there. Pass-through
			// flags (--yes, --force, --remote-cwd) are forwarded so the
			// remote does the right thing.
			if rmFlags.onHost != "" {
				cwd, _ := os.Getwd()
				resolved, err := resolveOnForSwitch(rmFlags.onHost, localProjectBasename(cwd), rmFlags.remoteCwd)
				if err != nil {
					return err
				}
				remoteArgs := []string{name}
				if rmFlags.yes {
					remoteArgs = append(remoteArgs, "--yes")
				}
				if rmFlags.force {
					remoteArgs = append(remoteArgs, "--force")
				}
				return dispatchVerbToRemote(ctx, resolved, "rm", remoteArgs, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}

			mgr, err := loadManager()
			if err != nil {
				return err
			}

			ws, err := mgr.Find(ctx, name)
			if err != nil {
				if handled, herr := rmHandleFindErr(err, rmFlags.force, name, cmd.OutOrStdout()); handled {
					return herr
				}
				if errors.Is(err, workspace.ErrWorkspaceNotFound) {
					return rmEnrichNotFound(ctx, mgr, name)
				}
				return err
			}

			// v0.6 safety pre-flight: refuse on hanging work unless --force.
			//
			// Delegates to mgr.SafetyPreflight which lives in the workspace
			// package — same code path as the TUI's 'd' delete flow, so
			// both surfaces refuse on the same conditions.
			//
			// On orphaned workspaces (worktree dir missing), the check
			// returns nil (no hangs detected) plus a debug-log line; we
			// don't block rm just because the diagnostic itself failed.
			if !rmFlags.force {
				hangs, err := mgr.SafetyPreflight(ctx, name)
				if err != nil {
					return err
				}
				if len(hangs) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(),
						"Refusing to remove %q — hanging work detected:\n", name)
					for _, h := range hangs {
						fmt.Fprintf(cmd.OutOrStdout(), "  • %s\n", h)
					}
					fmt.Fprintf(cmd.OutOrStdout(),
						"\nResolve the issues above, or pass --force to bypass.\n")
					return fmt.Errorf("workspace %q has hanging work; use --force to bypass", name)
				}
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
	cmd.Flags().BoolVarP(&rmFlags.yes, "yes", "y", false, "skip confirmation prompt (does NOT skip safety check)")
	cmd.Flags().BoolVar(&rmFlags.force, "force", false, "bypass v0.6 safety check (uncommitted/unpushed/open-PR)")
	cmd.Flags().StringVar(&rmFlags.onHost, "on", "", "dispatch to remote canopy at <host or ssh-target> (v0.17.0)")
	cmd.Flags().StringVar(&rmFlags.remoteCwd, "remote-cwd", "", "with --on: cd to <path> on the remote before invoking canopy")
	return cmd
}

// rmHandleFindErr decides whether to swallow a Find error from `canopy rm`.
// Mirrors `rm -f`: with --force, a missing workspace is the desired end
// state (the user's intent is "make it gone"; it already is), so return
// nil after emitting an informational line. Without --force, or for any
// other error kind, the caller should bubble the original error up so
// typos and unrelated failures still surface.
//
// Returns (handled, err): handled=true means the caller should return
// err to short-circuit the remove flow; handled=false means the caller
// should bubble the original error up.
//
// Load-bearing for the TUI force-delete path: the local TUI dispatches
// `canopy rm <name> --yes --force` for remote rows, and when the remote
// canopy has already lost the workspace (rm-via-other-channel since the
// last host refresh), the user sees a scary "remote canopy rm failed:
// exit status 1" line in their scrollback even though the post-dispatch
// refresh would drop the stale row. With this helper the remote canopy
// exits 0 cleanly, and the row disappears on the next refresh tick with
// no noise.
func rmHandleFindErr(err error, force bool, name string, out io.Writer) (handled bool, _ error) {
	if force && errors.Is(err, workspace.ErrWorkspaceNotFound) {
		fmt.Fprintf(out, "Workspace %q not found — already removed.\n", name)
		return true, nil
	}
	return false, nil
}

// rmEnrichNotFound replaces the terse `workspace.Find(X): workspace: not
// found` with a diagnostic that answers the three questions a user staring
// at the bare error actually has:
//
//  1. What workspaces ARE in this project? (Surfaces typos and stale TUI
//     rows in the same line — the user sees the truth.)
//  2. Does a workspace by this name exist in another project on this host?
//     (Common when the TUI on the laptop dispatched into the wrong remote
//     project via cwd, or after a manual cd.)
//  3. How do I make this succeed when I know the workspace is already
//     gone? (Point at --force, which mirrors `rm -f` semantics.)
//
// The error is built best-effort: store/list failures degrade to the
// shorter form rather than masking the original not-found signal. This
// is a user-facing error, not a typed sentinel — callers that need to
// branch on "workspace not found" still have access via errors.Is on
// the underlying ErrWorkspaceNotFound at the Find site.
func rmEnrichNotFound(ctx context.Context, mgr *workspace.Manager, name string) error {
	var lines []string
	lines = append(lines, fmt.Sprintf("workspace %q not found in project %q (%s).",
		name, mgr.Cfg.Project, mgr.Cfg.ProjectRoot))

	if list, listErr := mgr.List(ctx); listErr == nil {
		if len(list) == 0 {
			lines = append(lines, "No workspaces are registered in this project.")
		} else {
			names := make([]string, 0, len(list))
			for _, w := range list {
				names = append(names, w.Name)
			}
			sort.Strings(names)
			lines = append(lines, "Workspaces here: "+strings.Join(names, ", ")+".")
		}
	}

	if st, sErr := mgr.Store.Load(); sErr == nil {
		var elsewhere []string
		for _, w := range st.Workspaces {
			if w.Name == name && w.ProjectRoot != mgr.Cfg.ProjectRoot {
				elsewhere = append(elsewhere, w.ProjectRoot)
			}
		}
		if len(elsewhere) > 0 {
			sort.Strings(elsewhere)
			lines = append(lines, fmt.Sprintf(
				"A workspace named %q is registered under: %s. Run `canopy rm` from that project's root (or pass --on with the right host/project).",
				name, strings.Join(elsewhere, ", ")))
		}
	}

	lines = append(lines, "If the TUI showed this workspace, its list may be stale — refresh the host and try again, or re-run with --force to make a missing workspace a silent success.")
	return errors.New(strings.Join(lines, "\n"))
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
