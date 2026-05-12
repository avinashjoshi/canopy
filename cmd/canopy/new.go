package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// newWorkspaceFlags holds parsed CLI flags. Package-level so they're
// easy to test/inspect. v0.6 added --pr / --issue / --branch /
// --allow-local for the source-variant flows; the original --name
// and --no-attach still work as before. v0.17.0 Phase 0 adds --on
// for remote dispatch (see docs/design/v0.17-remote-workspaces.md).
var newWorkspaceFlags struct {
	name       string
	noAttach   bool
	pr         int    // --pr <num>: check out this PR's branch into a workspace
	issue      int    // --issue <num>: create workspace, briefing references this issue
	branch     string // --branch <name>: check out an existing branch
	allowLoc   bool   // --allow-local: with --branch, allow non-existent on origin
	prompt     string // --prompt: initial agent message; sent after creation
	promptFile string // --prompt-file: read --prompt content from file (multi-line)
	onHost     string // --on <ssh-target>: dispatch to remote canopy (v0.17.0 Phase 0)
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
			"creates a git worktree, runs scripts.setup, builds the standard 3-pane\n" +
			"tmux session (nvim / claude / shell), and attaches.\n\n" +
			"Source variants (mutually exclusive):\n" +
			"  --pr <num>     check out PR <num>'s branch (briefing includes PR body)\n" +
			"  --issue <num>  fresh branch off main; briefing seeded with issue body\n" +
			"  --branch <n>   check out existing branch <n> from origin\n" +
			"  --allow-local  with --branch, allow checkout of a local-only branch",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// v0.17.0 Phase 0: --on <ssh-target> dispatches the rest of
			// the invocation to a remote canopy. Phase 0 is intentionally
			// minimal — the remote owns the workspace lifecycle entirely;
			// laptop's role here is just "run canopy new over there with
			// these args, stream the output back."
			//
			// Phase 1 will add: hosts.json registry (so users say
			// `--on tower` instead of full ssh-target), --prompt support
			// over SSH (via stdin-pipe + remote temp-file dance), and
			// laptop-side state-cache integration so the new workspace
			// shows up in the local TUI without manual refresh.
			if newWorkspaceFlags.onHost != "" {
				return dispatchNewToRemote(ctx, newWorkspaceFlags.onHost, args, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}

			mgr, err := loadManager()
			if err != nil {
				return err
			}

			// Pre-validate the prompt BEFORE workspace creation. Bad
			// flag combos / unreadable / oversized files exit 1 with
			// no workspace created (per v3 failure-modes table).
			promptText, err := loadPrompt(newWorkspaceFlags.prompt, newWorkspaceFlags.promptFile)
			if err != nil {
				return err
			}

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
				ws.Name, ws.Branch, ws.Path, ws.Port, ws.TmuxSessionName())

			// Send the initial prompt if requested. The trust-dialog
			// state machine + claude-rendering verify all live in
			// sendInitialPrompt; we just decide what to do with the
			// outcome here.
			var promptErr error
			if promptText != "" {
				promptErr = workspace.SendInitialPrompt(
					ctx,
					mgr.Tmux,
					ws.TmuxSessionName(),
					ws.Name,
					promptText,
					cmd.ErrOrStderr(),
				)
				if promptErr == nil {
					fmt.Fprintf(cmd.OutOrStdout(),
						"Sent initial prompt to agent (%d chars).\n", len(promptText))
				} else {
					if pf, ok := workspace.IsPromptFailed(promptErr); ok {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"WARN: workspace created, %s\n", pf.Error())
					} else {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"ERROR while sending prompt: %v\n", promptErr)
					}
				}
			}

			if newWorkspaceFlags.noAttach {
				fmt.Fprintf(cmd.OutOrStdout(),
					"\nSkipping attach (--no-attach). Run `canopy switch %s` to attach later.\n", ws.Name)
				// Return promptErr (if any) so main.go can pick the right
				// exit code (2 for *errPromptFailed, 1 for other errors).
				return promptErr
			}

			fmt.Fprintf(cmd.OutOrStdout(), "\nAttaching tmux session %s...\n", ws.TmuxSessionName())
			// Attach replaces the canopy process via syscall.Exec on success.
			// If we return from Attach, it failed. Note: a non-nil promptErr
			// is dropped here — once we exec into tmux the user sees the
			// state directly and the exit code becomes whatever tmux returns.
			return mgr.Tmux.Attach(ctx, ws.TmuxSessionName())
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
	cmd.Flags().StringVar(&newWorkspaceFlags.prompt, "prompt", "",
		"initial agent message; sent to the agent pane after creation (single-line)")
	cmd.Flags().StringVar(&newWorkspaceFlags.promptFile, "prompt-file", "",
		"read --prompt content from file (multi-line; max 32KB)")
	cmd.Flags().StringVar(&newWorkspaceFlags.onHost, "on", "",
		"dispatch to remote canopy at <ssh-target> instead of running locally (v0.17.0 Phase 0)")
	return cmd
}

// dispatchNewToRemote SSHes the `canopy new` invocation to a remote host
// and streams output back. Phase 0 builds the argv by hand from the
// parsed flags rather than re-serializing os.Args, so this stays robust
// against future flag additions on the local side that aren't yet on
// the remote.
//
// Phase 0 limitation: --prompt / --prompt-file are not supported with
// --on (would leak prompt text via remote process listing). User flow
// for prompted remote workspaces in Phase 0:
//  1. canopy new --on tower --name oauth-fix --branch oauth-fix
//  2. canopy switch --on tower oauth-fix  (mosh-attach + type prompt manually)
//
// Phase 1 will pipe prompt text via SSH stdin into a remote temp file
// so it never appears in `ps aux` on either side.
func dispatchNewToRemote(ctx context.Context, target string, posArgs []string, stdout, stderr io.Writer) error {
	if newWorkspaceFlags.prompt != "" || newWorkspaceFlags.promptFile != "" {
		return fmt.Errorf(
			"--on does not support --prompt / --prompt-file in v0.17.0 Phase 0.\n"+
				"Create the remote workspace first, then attach with `canopy switch --on %s <name>` and type the prompt.\n"+
				"Phase 1 will pipe prompt text via SSH stdin (see TODOS.md).",
			target)
	}

	remoteArgs := []string{"canopy", "new"}
	if newWorkspaceFlags.name != "" {
		remoteArgs = append(remoteArgs, "--name", newWorkspaceFlags.name)
	}
	// Phase 0 default: --no-attach on remote. The local user can't be
	// attached to a tmux session that lives on tower from inside an
	// SSH-only invocation; they need to run `canopy switch --on <target>`
	// separately to mosh-attach.
	remoteArgs = append(remoteArgs, "--no-attach")
	if newWorkspaceFlags.pr != 0 {
		remoteArgs = append(remoteArgs, "--pr", fmt.Sprintf("%d", newWorkspaceFlags.pr))
	}
	if newWorkspaceFlags.issue != 0 {
		remoteArgs = append(remoteArgs, "--issue", fmt.Sprintf("%d", newWorkspaceFlags.issue))
	}
	if newWorkspaceFlags.branch != "" {
		remoteArgs = append(remoteArgs, "--branch", newWorkspaceFlags.branch)
	}
	if newWorkspaceFlags.allowLoc {
		remoteArgs = append(remoteArgs, "--allow-local")
	}
	// Pass through any positional args (cobra collects unparsed; in
	// practice `canopy new` takes none today but future-proof the call).
	remoteArgs = append(remoteArgs, posArgs...)

	fmt.Fprintf(stderr, "Dispatching to %s: %s\n", target, joinArgs(remoteArgs))

	c := host.SSHCmd(ctx, target, remoteArgs...)
	c.Stdout = stdout
	c.Stderr = stderr
	c.Stdin = os.Stdin // pass through in case remote canopy prompts (shouldn't, but harmless)
	if err := c.Run(); err != nil {
		return fmt.Errorf("remote canopy new failed: %w", err)
	}

	fmt.Fprintf(stdout, "\nRemote workspace created. Attach with:\n  canopy switch --on %s <name>\n", target)
	return nil
}

// joinArgs is a tiny display helper for the "Dispatching to X: canopy
// new ..." log line. Not for safe shell quoting (we never pass through
// /bin/sh in Phase 0 — ssh argv flows direct to argv on the other side).
func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
