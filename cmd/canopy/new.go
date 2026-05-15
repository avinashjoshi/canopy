package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

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
	remoteCwd  string // --remote-cwd <path>: cwd on the remote before running canopy (Phase 0)
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
			// Pre-validate the prompt BEFORE any dispatch (local or remote).
			// Bad flag combos / unreadable / oversized files exit 1 with
			// no workspace created — same for `canopy new` and `canopy new
			// --on tower`. v0.17.0 Phase 1f.
			promptText, err := loadPrompt(newWorkspaceFlags.prompt, newWorkspaceFlags.promptFile)
			if err != nil {
				return err
			}

			if newWorkspaceFlags.onHost != "" {
				cwd, _ := os.Getwd()
				localProject := localProjectBasename(cwd)
				resolved, err := resolveOnForNew(newWorkspaceFlags.onHost, localProject, newWorkspaceFlags.remoteCwd)
				if err != nil {
					return err
				}
				return dispatchNewToRemote(ctx, resolved, args, promptText, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}

			mgr, err := loadManager()
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
	cmd.Flags().StringVar(&newWorkspaceFlags.remoteCwd, "remote-cwd", "",
		"with --on: cd to <path> on the remote before invoking canopy (Phase 0; Phase 1 absorbs into hosts.json project registry)")
	return cmd
}

// dispatchNewToRemote SSHes the `canopy new` invocation to a remote host
// and streams output back. Builds the argv by hand from the parsed
// flags rather than re-serializing os.Args, so this stays robust
// against future flag additions on the local side that aren't yet on
// the remote.
//
// promptText (v0.17.0 Phase 1f): when non-empty, it's base64-encoded
// and embedded in the bash script via a heredoc; the remote shell
// decodes it into a umask-077 temp file, then invokes canopy with
// --prompt-file <temp>. The trap line cleans up the temp file
// regardless of exit code. Base64 + heredoc means the prompt can
// contain ANY characters (quotes, backticks, $vars, even the heredoc
// marker) without escaping concerns. The prompt text never appears
// in `ps aux` on either machine — it lives only in the script (sent
// via SSH stdin) and the temp file (umask 077).
func dispatchNewToRemote(ctx context.Context, resolved resolvedHost, posArgs []string, promptText string, stdout, stderr io.Writer) error {
	target := resolved.SSHTarget

	var canopyArgs []string
	canopyArgs = append(canopyArgs, "canopy", "new")
	if newWorkspaceFlags.name != "" {
		canopyArgs = append(canopyArgs, "--name", newWorkspaceFlags.name)
	}
	// Phase 0 default: --no-attach on remote. The local user can't be
	// attached to a tmux session that lives on tower from inside an
	// SSH-only invocation; they need to run `canopy switch --on <target>`
	// separately to mosh-attach.
	canopyArgs = append(canopyArgs, "--no-attach")
	if newWorkspaceFlags.pr != 0 {
		canopyArgs = append(canopyArgs, "--pr", fmt.Sprintf("%d", newWorkspaceFlags.pr))
	}
	if newWorkspaceFlags.issue != 0 {
		canopyArgs = append(canopyArgs, "--issue", fmt.Sprintf("%d", newWorkspaceFlags.issue))
	}
	if newWorkspaceFlags.branch != "" {
		canopyArgs = append(canopyArgs, "--branch", newWorkspaceFlags.branch)
	}
	if newWorkspaceFlags.allowLoc {
		canopyArgs = append(canopyArgs, "--allow-local")
	}
	// Pass through any positional args (cobra collects unparsed; in
	// practice `canopy new` takes none today but future-proof the call).
	canopyArgs = append(canopyArgs, posArgs...)

	// resolved.RemoteCwd already factored in --remote-cwd (per-command
	// override) and the registry's host.Projects[<local-project>] lookup.
	// Build a small shell script and pipe it to `bash -l` on the remote
	// via stdin. The login shell sources ~/.bash_profile / ~/.profile,
	// which is where most install scripts add ~/.local/bin to PATH.
	// `set -e` halts on cd failure so we don't accidentally run canopy
	// in the wrong directory.
	script := buildRemoteScript(resolved.RemoteCwd, canopyArgs, promptText)
	fmt.Fprintf(stderr, "Dispatching to %s (%s):\n%s", target, resolved.Source, indent(script, "  "))

	c := host.SSHCmd(ctx, target, "bash", "-l")
	c.Stdout = stdout
	c.Stderr = stderr
	c.Stdin = strings.NewReader(script)
	if err := c.Run(); err != nil {
		// Exit code 2 from the remote = "workspace OK, prompt failed"
		// (cmd/canopy/main.go maps workspace.IsPromptFailed → exit 2).
		// Preserve that distinction by returning ErrPromptFailed locally
		// so main.go propagates it as exit 2, not exit 1. Without this,
		// callers that branch on "workspace created" vs "create failed"
		// — including the TUI's createDoneMsg auto-attach path — can't
		// tell the difference.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
			fmt.Fprintf(stdout, "\nRemote workspace created (initial prompt failed). Re-send the prompt manually after attaching.\n")
			return &workspace.ErrPromptFailed{Reason: "remote prompt delivery failed (exit 2)"}
		}
		// Exit 7 = the dir-existence pre-check in buildRemoteScript fired.
		// Surface a clear, actionable remediation instead of bash's terse
		// "No such file or directory" — the registered remote path is the
		// real bug and the user needs to know which command to run.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 7 {
			return remotePathMissingErr(newWorkspaceFlags.onHost, resolved.RemoteCwd, resolved.HostName)
		}
		return fmt.Errorf("remote canopy new failed: %w", err)
	}

	fmt.Fprintf(stdout, "\nRemote workspace created. Attach with:\n  canopy switch --on %s <name>\n", newWorkspaceFlags.onHost)
	return nil
}

// buildRemoteScript assembles the shell script piped into the remote
// login shell. `set -e` ensures cd failure short-circuits the canopy
// invocation. `exec` replaces bash with canopy so the exit code is
// canopy's, not bash's.
//
// When promptText is non-empty (v0.17.0 Phase 1f), the script:
//  1. Creates an umask-077 temp file via mktemp.
//  2. Registers a trap that removes the temp file on shell exit
//     (success, failure, signal — all paths).
//  3. base64-decodes the prompt text into the temp file from an
//     inline heredoc. Base64 + heredoc avoids ALL escaping concerns —
//     the prompt can contain backticks, $vars, the literal string
//     "EOF", anything; base64 maps it to a single-character alphabet
//     that never collides with shell metachars.
//  4. Appends --prompt-file <temp> to the canopy invocation.
//
// The prompt text never lands in `ps aux` on either machine (only the
// canopy process reads the temp file; the file is deleted before any
// other process can list it). Security decision 3A from
// /plan-ceo-review.
func buildRemoteScript(remoteCwd string, canopyArgs []string, promptText string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	// Ensure ~/.local/bin is on PATH. The canopy curl-installer puts the
	// binary there, but many distros (including omarchy) have an
	// interactive-only .bashrc guard that doesn't fire for non-
	// interactive SSH-command shells.
	b.WriteString(`export PATH="$HOME/.local/bin:$PATH"` + "\n")
	if remoteCwd != "" {
		// Pre-check the cwd with a distinct exit code (7) so the caller can
		// distinguish "remote project path not registered correctly" from
		// other failures. Without this, a typo or local-vs-remote user
		// mismatch (e.g. /home/avi/Work/brain registered for a pi host
		// whose user is jarvis) bubbles up as a generic `cd: No such file`
		// — opaque to anyone who didn't set up the host. exitRemotePathMissing
		// is the matching constant on the laptop side.
		fmt.Fprintf(&b, "if [ ! -d %s ]; then echo \"canopy: remote project path %s does not exist on this host\" >&2; exit 7; fi\n",
			shellQuote(remoteCwd), shellQuote(remoteCwd))
		fmt.Fprintf(&b, "cd %s\n", shellQuote(remoteCwd))
	}

	// Prompt path: write the prompt to a temp file, run canopy as a
	// CHILD (not exec), then unlink the temp file after canopy exits.
	// Crucial: NOT `exec canopy` because exec replaces the shell, so
	// any cleanup line after exec never runs AND a trap registered
	// on the bash process is gone with the shell. Run-then-rm keeps
	// the shell alive long enough to delete the file.
	if promptText != "" {
		b.WriteString(`__CANOPY_PROMPT_FILE=$(umask 077 && mktemp /tmp/canopy-prompt-XXXXXX)` + "\n")
		// Defensive trap covers signal-kill / power-cut paths where
		// the explicit rm below never runs. EXIT fires on any bash
		// exit including signals.
		b.WriteString(`trap 'rm -f "$__CANOPY_PROMPT_FILE"' EXIT INT TERM` + "\n")
		b.WriteString(`base64 -d > "$__CANOPY_PROMPT_FILE" <<'__CANOPY_PROMPT_B64__'` + "\n")
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(promptText)))
		b.WriteString("\n__CANOPY_PROMPT_B64__\n")
		canopyArgs = append(canopyArgs, "--prompt-file", `"$__CANOPY_PROMPT_FILE"`)
		// No `exec` prefix — we need to outlive canopy so the trap
		// fires + the explicit rm runs. canopy's exit code propagates
		// via the script's last command (this canopy invocation).
		writeCanopyArgs(&b, canopyArgs)
		b.WriteByte('\n')
		return b.String()
	}

	// No prompt: exec replacement is fine — canopy becomes the shell.
	b.WriteString("exec ")
	writeCanopyArgs(&b, canopyArgs)
	b.WriteByte('\n')
	return b.String()
}

// writeCanopyArgs is the shared per-arg writer. The prompt-file
// argument is pre-tagged as `"$__CANOPY_PROMPT_FILE"` so bash expands
// the variable; shellQuote would wrap that in single quotes and break
// the expansion.
func writeCanopyArgs(b *strings.Builder, args []string) {
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		if strings.HasPrefix(a, `"$`) {
			b.WriteString(a)
		} else {
			b.WriteString(shellQuote(a))
		}
	}
}

// indent prepends `prefix` to each line of `s`. Used to display the
// remote script in stderr so it's clear what was dispatched.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	out := ""
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			continue
		}
		out += prefix + line + "\n"
	}
	return out
}

// remotePathMissingErr formats the exit-7 ("remote project path doesn't
// exist") response from buildRemoteScript's pre-check into a clear,
// remediable error. hostName is the registry name (empty for raw
// ssh-targets); when set, it's used to build a copy-pasteable
// `canopy project add` command.
func remotePathMissingErr(onHostSpec, remotePath, hostName string) error {
	hint := ""
	if hostName != "" {
		// The project name component matches the local project basename
		// resolution path that wrote this registration in the first
		// place. Quoting the path is fine — shells handle it.
		hint = fmt.Sprintf("\n  Update the registered remote path with:\n    canopy project add %s <correct-remote-path> --on %s\n  (current registered path: %s)",
			localProjectBasename(currentCwd()), hostName, remotePath)
	}
	return fmt.Errorf("remote project path %q does not exist on host %q.%s",
		remotePath, onHostSpec, hint)
}

// currentCwd is os.Getwd with the error swallowed — used only for the
// hint string in remotePathMissingErr where an empty value just yields
// a less-specific (but still correct) hint.
func currentCwd() string {
	cwd, _ := os.Getwd()
	return cwd
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
