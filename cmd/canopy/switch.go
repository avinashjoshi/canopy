package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// switchFlags holds the parsed --on flag for v0.17.0 Phase 0.
var switchFlags struct {
	onHost    string
	remoteCwd string
	share     bool // --share: don't detach other clients (multi-attach)
	main      bool // --main: attach to the project's main session instead of a named workspace
}

// switchCmd returns the `canopy switch <name>` cobra subcommand.
//
// Before dispatching, switch runs a lazy reconcile: if the recorded
// status disagrees with reality (state says ready but tmux session is
// gone, or the dir has been hand-deleted), the row's status is updated
// in place. That way `canopy switch` always operates on the truth, not
// on whatever state.json said the last time canopy was used.
//
// Behavior by status (after reconcile):
//
//	ready      -> attach (syscall.Exec into tmux)
//	stopped    -> resurrect (rebuild tmux session, claude --continue),
//	              then attach
//	broken     -> print error log path, suggest canopy rm
//	orphaned   -> print warning, suggest canopy rm
//	setting_up -> print "still setting up", exit non-zero
func switchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "switch <name>",
		Short: "Attach to a workspace's tmux session (resurrect if stopped)",
		// 0 args when --main is set (project main session, no workspace name);
		// 1 arg otherwise (workspace name).
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if !switchFlags.main && len(args) == 0 {
				return fmt.Errorf("canopy switch: workspace name required (or pass --main for project main session)")
			}
			var name string
			if len(args) > 0 {
				name = args[0]
			}

			// v0.17.0 Phase 0: --on <ssh-target> attaches via mosh to a
			// workspace that lives on a remote canopy. The remote canopy
			// is the canonical state-owner; we just exec `mosh <target>
			// -- canopy switch <name>` and let its existing reconcile +
			// resurrect + attach flow run there. Mosh handles the wire
			// (UDP, state-sync, roaming, laptop-suspend tolerance).
			//
			// This pattern is intentionally dumb: laptop doesn't know
			// the workspace's tmux session name (depends on tower's
			// canopy version and project layout). Tower's canopy figures
			// it out. Laptop is the renderer; tower is the brain.
			if switchFlags.onHost != "" {
				cwd, _ := os.Getwd()
				preferred := localProjectBasename(cwd)
				resolved, err := resolveOnForSwitch(switchFlags.onHost, preferred, switchFlags.remoteCwd)
				if err != nil {
					return err
				}
				return dispatchSwitchToRemote(ctx, resolved, name, switchFlags.share, switchFlags.main)
			}

			// Local --main: this branch only handles --on dispatch; local
			// main attach is `canopy main`, not `canopy switch --main`.
			if switchFlags.main {
				return fmt.Errorf("canopy switch --main only valid with --on; use `canopy main` locally")
			}

			// Local --share: set CANOPY_NO_DETACH=1 in the process env
			// so subsequent tmux Attach calls skip the detach-others +
			// -d behavior. shouldDetachOthers() reads from the env, so
			// this propagates through Manager.Attach without a wider
			// refactor. v0.17 Phase 1j.
			if switchFlags.share {
				_ = os.Setenv("CANOPY_NO_DETACH", "1")
			}

			mgr, err := loadManager()
			if err != nil {
				return err
			}

			// Lazy reconcile: ensure status reflects reality before we act on it.
			// Errors here are non-fatal; if reconcile fails we proceed with the
			// stale status (and the user can re-run `canopy reconcile` directly).
			if _, err := mgr.Reconcile(ctx); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: reconcile failed: %v\n", err)
			}

			ws, err := mgr.Find(ctx, name)
			if err != nil {
				return err
			}

			switch ws.Status {
			case state.StatusReady:
				// Backfill @canopy-role tags for v0.15-style sessions that
				// never went through the v0.16+ buildSession (which tags at
				// creation). Best-effort: errors logged, never block attach.
				workspace.BackfillRoles(ctx, mgr.Tmux, ws.TmuxSessionName(), mgr.Cfg.Agent.Type)
				fmt.Fprintf(cmd.OutOrStdout(), "Attaching tmux session %s...\n", ws.TmuxSessionName())
				return mgr.Tmux.Attach(ctx, ws.TmuxSessionName())

			case state.StatusStopped:
				fmt.Fprintf(cmd.OutOrStdout(), "Resurrecting workspace %s...\n", name)
				revived, err := mgr.Resurrect(ctx, name)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Attaching tmux session %s...\n", revived.TmuxSessionName())
				return mgr.Tmux.Attach(ctx, revived.TmuxSessionName())

			case state.StatusBroken:
				fmt.Fprintf(cmd.ErrOrStderr(),
					"workspace %q is in status %q.\nLast error: %s\nSee ~/.canopy/log/canopy.log for details.\nRun `canopy rm %s` to clean up.\n",
					name, ws.Status, ws.LastError, name)
				return fmt.Errorf("workspace %q is broken", name)

			case state.StatusOrphaned:
				fmt.Fprintf(cmd.ErrOrStderr(),
					"workspace %q has no on-disk dir at %s.\nRun `canopy rm %s` to drop the state row.\n",
					name, ws.Path, name)
				return fmt.Errorf("workspace %q is orphaned", name)

			case state.StatusSettingUp:
				fmt.Fprintf(cmd.ErrOrStderr(),
					"workspace %q is still setting up. Try again in a moment.\n", name)
				return fmt.Errorf("workspace %q is still setting up", name)

			default:
				return fmt.Errorf("workspace %q has unknown status %q", name, ws.Status)
			}
		},
	}
	c.Flags().StringVar(&switchFlags.onHost, "on", "",
		"attach to a workspace on remote canopy at <ssh-target> via mosh+tmux (v0.17.0 Phase 0)")
	c.Flags().StringVar(&switchFlags.remoteCwd, "remote-cwd", "",
		"with --on: cd to <path> on the remote before invoking canopy (Phase 0; Phase 1 absorbs into hosts.json)")
	c.Flags().BoolVar(&switchFlags.main, "main", false,
		"with --on: attach to the project's main session (canopy main on the remote) instead of a named workspace")
	c.Flags().BoolVar(&switchFlags.share, "share", false,
		"don't detach existing tmux clients on the target session (multi-attach / parallel mosh)")
	return c
}

// dispatchSwitchToRemote execs mosh against the target host, where mosh
// in turn launches `canopy switch <name>` on the remote. The remote
// canopy's switch handles reconcile + resurrect + tmux attach exactly
// as it does locally; the result is your terminal becomes a mosh-
// rendered view of the remote tmux session.
//
// On success this function does not return — syscall.Exec replaces the
// current canopy process with mosh, so when mosh eventually exits the
// shell sees the mosh exit code, not canopy's. If exec FAILS (e.g.,
// mosh is missing) we return the wrapped error and the caller surfaces
// it normally.
//
// Why exec instead of cmd.Run(): mosh expects to own the terminal
// completely (raw mode, signal handling, alt-screen). Running it as a
// child of canopy with redirected stdio works but is fragile; SIGWINCH,
// ctrl-c, and terminal modes all need to flow cleanly. Exec replacement
// gives mosh a clean inheritance and avoids canopy sitting around as a
// zombie parent.
func dispatchSwitchToRemote(ctx context.Context, resolved resolvedHost, wsName string, share, main bool) error {
	target := resolved.SSHTarget
	if err := host.CheckMoshAvailable(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Attaching to %s (%s) via mosh+tmux...\n", target, resolved.Source)

	// Resolve mosh's absolute path for exec (syscall.Exec needs absolute path).
	moshBin, err := exec.LookPath("mosh")
	if err != nil {
		// Shouldn't happen — we just checked above — but defensive.
		return &host.ErrMoshMissing{Inner: err}
	}

	// Wrap remote command in `bash -lc '...'` and explicitly prepend
	// ~/.local/bin to PATH. Non-interactive SSH-command shells skip
	// .bashrc (interactive guard), so omarchy / Arch / similar setups
	// don't inherit the user's ~/.local/bin from their login profile.
	// Phase 1 absorbs the path into the hosts.json registry.
	//
	// resolved.RemoteCwd factored in --remote-cwd (per-command override),
	// preferred-project lookup, and first-registered-project fallback.
	// canopy switch on the remote walks up cwd looking for canopy.json,
	// and the global-workspace lookup finds the workspace by name across
	// projects — so ANY registered project works as the cd target.
	remoteCmd := buildRemoteSwitchCmd(resolved.RemoteCwd, wsName, share, main)
	argv := []string{"mosh", target, "--", "bash", "-lc", remoteCmd}
	// syscall.Exec replaces this process with mosh. On success, this
	// call does not return; on failure we fall through to the error.
	if err := syscall.Exec(moshBin, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec mosh: %w", err)
	}
	// Unreachable.
	return nil
}

// buildRemoteSwitchCmd assembles the bash one-liner that mosh runs on
// the remote. Factored out of dispatchSwitchToRemote so the main/named
// branching is unit-testable without the mosh exec.
//
// Wraps the remote in `bash -lc '...'` and explicitly prepends
// ~/.local/bin to PATH — non-interactive SSH-command shells skip
// .bashrc (interactive guard), so omarchy / Arch / similar setups
// don't inherit the user's ~/.local/bin from their login profile.
//
// `main` carries the laptop-side intent: the TUI synthesizes a "(main)"
// row for each project, and the user's intent on Enter is "attach to
// that project's main session." We dispatch `canopy main` for those.
// Keying off an explicit flag (not the literal string "(main)") means
// a real workspace happening to be named "(main)" — git accepts the
// branch name — still attaches via `canopy switch` instead of being
// silently redirected to the project main session.
func buildRemoteSwitchCmd(remoteCwd, wsName string, share, main bool) string {
	out := `export PATH="$HOME/.local/bin:$PATH"; `
	if share {
		// Propagate --share over the mosh dispatch as a remote env var
		// so the remote canopy switch skips detach-other-clients. mosh
		// doesn't forward arbitrary env vars across the SSH-style
		// boundary; setting it explicitly in the remote shell does.
		out += `export CANOPY_NO_DETACH=1; `
	}
	if remoteCwd != "" {
		out += "cd " + shellQuote(remoteCwd) + "; "
	}
	if main {
		out += "exec canopy main"
	} else {
		out += "exec canopy switch " + shellQuote(wsName)
	}
	return out
}
