package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
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
				propagateRemoteHostEnv(ctx, mgr.Tmux, ws.TmuxSessionName())
				fmt.Fprintf(cmd.OutOrStdout(), "Attaching tmux session %s...\n", ws.TmuxSessionName())
				return mgr.Tmux.Attach(ctx, ws.TmuxSessionName())

			case state.StatusStopped:
				fmt.Fprintf(cmd.OutOrStdout(), "Resurrecting workspace %s...\n", name)
				revived, err := mgr.Resurrect(ctx, name)
				if err != nil {
					return err
				}
				propagateRemoteHostEnv(ctx, mgr.Tmux, revived.TmuxSessionName())
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

	// Pre-probe the remote project path via SSH before exec'ing mosh.
	// dispatchSwitchToRemote does a syscall.Exec into mosh, which can't
	// surface errors back to the TUI — any cd failure inside the mosh
	// child shell tears down with no visible message. A 1-roundtrip SSH
	// check (reuses the ControlMaster socket) keeps the error in the
	// terminal the TUI is still drawing in. Skip when RemoteCwd is empty
	// (raw ssh-target with no path; remote canopy walks cwd from $HOME).
	if resolved.RemoteCwd != "" {
		if probeErr := probeRemoteCwd(ctx, target, resolved.RemoteCwd); probeErr != nil {
			spec := resolved.HostName
			if spec == "" {
				spec = target
			}
			// We deliberately don't unwrap probeErr — distinguishing
			// "host offline" from "path missing" requires re-running the
			// probe with a different command, and the user can tell from
			// context (the TUI shows host status separately). Wrap the
			// most likely cause: a path-registration mismatch.
			return remotePathMissingErr(spec, resolved.RemoteCwd, resolved.HostName)
		}
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
	remoteCmd := buildRemoteSwitchCmd(resolved.RemoteCwd, resolved.HostName, wsName, share, main)
	argv := moshExecArgv(target, remoteCmd)
	// syscall.Exec replaces this process with mosh. On success, this
	// call does not return; on failure we fall through to the error.
	if err := syscall.Exec(moshBin, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec mosh: %w", err)
	}
	// Unreachable.
	return nil
}

// moshExecArgv builds the argv for the syscall.Exec into mosh: mosh
// [--] target command... . Extracted as a pure function so the argv
// shape is independently testable without exec'ing a real process.
//
// "--" before target: without it, a target string shaped like a mosh
// option (e.g. "--server=malicious-command") is parsed by mosh as a
// FLAG, not a hostname — confirmed by PoC. target traces back to
// resolved.SSHTarget, which a raw --on/--remote spec sets directly
// from user input with no validation. See internal/host/ssh.go's
// MoshCmd for the same fix applied to the non-exec code path.
//
// Exactly ONE "--" belongs here. mosh's own usage is
// "[options] [--] [user@]host [command...]" — no second "--" between
// host and command. A second one (this used to have one, inherited
// from before the security fix added the first) becomes the FIRST
// ELEMENT of the forwarded command itself: mosh forwards everything
// after host verbatim as [command...], so mosh-server receives
// "-- bash -lc <remoteCmd>" and tries to execvp a program literally
// named "--" — confirmed live: "mosh-server: execvp: --: No such file
// or directory".
func moshExecArgv(target, remoteCmd string) []string {
	return []string{"mosh", "--", target, "bash", "-lc", remoteCmd}
}

// propagateRemoteHostEnv synchronizes the CANOPY_REMOTE_HOST tag on the
// named tmux session with this canopy process's env, so statusline
// subprocesses spawned by the tmux server render the correct pill state
// across re-attaches.
//
// Two paths:
//
//   - Remote attach: this process inherits CANOPY_REMOTE_HOST=tower from
//     the mosh remote one-liner (see buildRemoteSwitchCmd). Set the
//     session env to the same nickname.
//
//   - Local attach to a session that was PREVIOUSLY remote-attached: the
//     prior remote attach left CANOPY_REMOTE_HOST=tower in the session
//     env. This process has no CANOPY_REMOTE_HOST set. Without explicit
//     cleanup, the stale tag persists and the yellow pill keeps
//     rendering — falsely signaling "you are still attached to tower"
//     when the user is now physically on the box. Unset it.
//
// Why this is needed on top of the bash export in buildRemoteSwitchCmd:
// the export reaches `canopy switch` itself, but an already-running tmux
// server has its env frozen at startup time. Statusline ticks fork off
// the server, not the bash that ran canopy switch — so without
// per-session set-environment, the pill state would drift from reality.
//
// Per-session scope (not -g) is deliberate: a remote host where the
// user also has local-only sessions shouldn't mark those with the
// laptop's nickname. Local sessions on the remote stay unmarked.
//
// Best-effort: tmux errors are logged and swallowed; the pill state
// just doesn't update that tick.
func propagateRemoteHostEnv(ctx context.Context, t *tmux.Client, session string) {
	if session == "" {
		return
	}
	host := strings.TrimSpace(os.Getenv("CANOPY_REMOTE_HOST"))
	if host == "" {
		// Local-attach path: clear any stale tag from a prior remote attach.
		if err := t.UnsetSessionEnv(ctx, session, "CANOPY_REMOTE_HOST"); err != nil {
			clog.Pkg("switch").Warn("switch.unset_remote_host", "session", session, "err", err.Error())
		}
		return
	}
	if err := t.SetSessionEnv(ctx, session, "CANOPY_REMOTE_HOST", host); err != nil {
		clog.Pkg("switch").Warn("switch.propagate_remote_host", "session", session, "err", err.Error())
	}
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
func buildRemoteSwitchCmd(remoteCwd, hostName, wsName string, share, main bool) string {
	out := `export PATH="$HOME/.local/bin:$PATH"; `
	if share {
		// Propagate --share over the mosh dispatch as a remote env var
		// so the remote canopy switch skips detach-other-clients. mosh
		// doesn't forward arbitrary env vars across the SSH-style
		// boundary; setting it explicitly in the remote shell does.
		out += `export CANOPY_NO_DETACH=1; `
	}
	if hostName != "" {
		// Tag the remote shell with the host's registered nickname so the
		// remote canopy's statusline can render a yellow pill identifying
		// "you are attached to <hostName>, not local." The remote-side
		// canopy switch propagates this to the tmux session env (via
		// propagateRemoteHostEnv → tmux set-environment -t <session>) so
		// statusline subprocesses inherit it across re-attaches. Without
		// hostName (raw target spec), no pill renders — the statusline
		// pill is a precise "canopy drove this attach" signal, not a
		// general "this is an ssh session" indicator (the user's own
		// tmux status-right hostname segment handles that).
		out += "export CANOPY_REMOTE_HOST=" + shellQuote(hostName) + "; "
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
