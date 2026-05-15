// init_remote.go — `canopy init <url> --on <host>` dispatch (v0.20 Phase D2).
//
// Re-uses the v0.17 SSH plumbing from internal/host to execute the
// add-project flow on a registered remote canopy. Single command from
// the laptop:
//
//	canopy init https://github.com/foo/bar.git --on tower
//
// Becomes, under the hood:
//
//	ssh -o ControlMaster=auto ... avi@tower 'canopy init <quoted-url>'
//
// Stdin/stdout/stderr are inherited so:
//
//   - git's auth prompts (SSH passphrase, HTTPS credential helpers,
//     host-key confirmations) render on the user's local tty — the
//     SSH session forwards the tty transparently.
//   - The remote canopy's "Cloning into 'X'..." and next-steps print
//     directly to the laptop's terminal.
//
// Requirements (documented in the error path):
//
//   - Host registered via `canopy host add <name> <ssh-target>` (v0.17).
//   - SSH key auth working (otherwise the user sees a Permission-denied;
//     `canopy host show <name>` shows the same).
//   - Remote canopy is v0.20+. Older versions don't accept a URL arg.
//
// Per-host source-root: the REMOTE canopy uses its own
// ~/.canopy/config.json + $CANOPY_SOURCE_ROOT. Configure source-root
// on each host (or rely on the default ~/.canopy/sources on the remote).

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/avinashjoshi/canopy/internal/host"
)

// runAddProjectRemote dispatches a `canopy init <arg> [<dest>]` call
// to a remote canopy installation via SSH. Returns once the remote
// process exits.
//
// hostName is the canopy host registry key (not the SSH target).
// resolveAndDispatch reads hosts.json, looks up the SSH target, and
// builds the SSHCmd. Errors at any step surface with the host name
// in context.
//
// stdout/stderr inheritance: we inherit the calling process's tty so
// the remote canopy's output (including git's progress + auth
// prompts) reaches the user directly. This makes the TUI flow
// require tea.ExecProcess; the CLI flow is plain exec.
func runAddProjectRemote(ctx context.Context, hostName, arg string, opts addProjectOptions, stdout io.Writer) error {
	if hostName == "" {
		return errors.New("canopy init --on: empty host name")
	}
	if arg == "" {
		return errors.New("canopy init --on: no <path-or-url> arg — remote-init requires a URL or absolute path")
	}

	canopyHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("canopy init --on: home dir: %w", err)
	}
	reg, err := host.NewRegistry(filepath.Join(canopyHome, ".canopy"))
	if err != nil {
		return fmt.Errorf("canopy init --on: open host registry: %w", err)
	}
	h, err := reg.Resolve(hostName)
	if err != nil {
		return fmt.Errorf("canopy init --on %s: %w. Register the host first: canopy host add %s <ssh-target>",
			hostName, err, hostName)
	}

	// Build the remote command. Shell-quote each user-provided value
	// so URLs containing shell metacharacters (`;`, `&`, `$`, etc.)
	// don't get reinterpreted on the remote side. SSH joins these
	// strings with spaces and passes them to the remote shell, so
	// each value must arrive as one shell word.
	remoteArgs := []string{"canopy", "init", shellQuote(arg)}
	if opts.DestOverride != "" {
		remoteArgs = append(remoteArgs, shellQuote(opts.DestOverride))
	}
	if opts.Force {
		remoteArgs = append(remoteArgs, "--force")
	}
	if opts.WithScripts {
		remoteArgs = append(remoteArgs, "--with-scripts")
	}

	fmt.Fprintf(stdout, "Dispatching to %s (%s)...\n", h.Name, h.SSHTarget)

	cmd := host.SSHCmd(ctx, h.SSHTarget, remoteArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("canopy init --on %s: %w", hostName, err)
	}
	return nil
}

// shellQuote is provided by cmd/canopy/install_tmux.go — it wraps
// strings in single quotes for safe shell interpolation. Used here
// to harden the URL/dest values against shell metacharacters when
// joined into the remote command line.
