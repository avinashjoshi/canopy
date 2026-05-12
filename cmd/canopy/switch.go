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
	onHost string
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
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

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
				return dispatchSwitchToRemote(ctx, switchFlags.onHost, name)
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
func dispatchSwitchToRemote(ctx context.Context, target, wsName string) error {
	if err := host.CheckMoshAvailable(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Attaching to %s via mosh+tmux...\n", target)

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
	remoteCmd := `export PATH="$HOME/.local/bin:$PATH"; exec canopy switch ` + shellQuote(wsName)
	argv := []string{"mosh", target, "--", "bash", "-lc", remoteCmd}
	// syscall.Exec replaces this process with mosh. On success, this
	// call does not return; on failure we fall through to the error.
	if err := syscall.Exec(moshBin, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec mosh: %w", err)
	}
	// Unreachable.
	return nil
}
