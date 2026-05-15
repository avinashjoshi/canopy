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
	"strings"

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

	// Build the remote command as a single string for `bash -lc`.
	// Shell-quote each user-provided value so URLs containing
	// metacharacters (`;`, `&`, `$`, etc.) don't get reinterpreted on
	// the remote side. The remote shell sees: `bash -lc 'canopy init
	// <quoted-url>'` — login shell sources the user's profile (so
	// canopy on PATH works) and pty is allocated by SSHRunUser (so
	// git auth prompts can read from /dev/tty).
	//
	// Pre-pend CANOPY_INIT_RESULT_FILE=<remote-temp> so the remote
	// canopy writes its canonical project root to a file we can fetch
	// after the SSH dispatch. We then call reg.AddProject locally
	// with that path — without it, `canopy new --on tower` fails
	// because the laptop's hosts.json doesn't know cravd's path on
	// tower (resolveOnForNew can't auto-resolve --remote-cwd).
	resultFile := fmt.Sprintf("/tmp/canopy-init-result-%d.txt", os.Getpid())
	remote := fmt.Sprintf("%s=%s canopy init %s", initResultEnvVar, shellQuote(resultFile), shellQuote(arg))
	if opts.DestOverride != "" {
		remote += " " + shellQuote(opts.DestOverride)
	}
	if opts.Force {
		remote += " --force"
	}
	if opts.WithScripts {
		remote += " --with-scripts"
	}

	fmt.Fprintf(stdout, "Dispatching to %s (%s)...\n", h.Name, h.SSHTarget)

	cmd := host.SSHRunUser(ctx, h.SSHTarget, remote)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("canopy init --on %s: %w", hostName, err)
	}

	// Init succeeded on the remote. Fetch the result file (canonical
	// project root path) so we can register the project locally in
	// hosts.json. Uses the same ControlMaster socket the first
	// dispatch opened — handshake is instant.
	if err := registerRemoteProjectLocally(ctx, hostName, h.SSHTarget, resultFile, stdout); err != nil {
		// Don't fail the init — the project IS initialized on the
		// remote. Surface the warning so the user knows they may
		// need to register manually before `canopy new --on <host>`.
		fmt.Fprintf(stdout, "warning: %v\n", err)
		fmt.Fprintf(stdout, "  Workaround: canopy project add <name> <remote-path> --on %s\n", hostName)
	}
	return nil
}

// registerRemoteProjectLocally fetches the result file from the
// remote and writes the (project-name, project-path) pair into the
// laptop's hosts.json Projects map for hostName. Idempotent — if the
// project is already registered, AddProject returns an error we
// downgrade to a no-op.
func registerRemoteProjectLocally(ctx context.Context, hostName, sshTarget, resultFile string, stdout io.Writer) error {
	// Read + clean up the result file in one ssh round-trip. Use
	// SSHCmdBatch (no -t) since we just want the cat output.
	fetch := host.SSHCmdBatch(ctx, sshTarget, "bash", "-c",
		fmt.Sprintf("cat %s 2>/dev/null && rm -f %s", shellQuote(resultFile), shellQuote(resultFile)))
	out, err := fetch.Output()
	if err != nil {
		return fmt.Errorf("fetch remote init result: %w", err)
	}
	canonicalRoot := strings.TrimSpace(string(out))
	if canonicalRoot == "" {
		return fmt.Errorf("remote init result is empty (remote canopy may be pre-v0.20 — won't auto-register)")
	}

	projectName := filepath.Base(canonicalRoot)
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	reg, err := host.NewRegistry(filepath.Join(home, ".canopy"))
	if err != nil {
		return fmt.Errorf("open host registry: %w", err)
	}
	if err := reg.AddProject(hostName, projectName, canonicalRoot); err != nil {
		// ErrProjectExists is fine — idempotent re-run.
		if errors.Is(err, host.ErrProjectExists) {
			fmt.Fprintf(stdout, "Project %q on %s already registered locally.\n", projectName, hostName)
			return nil
		}
		return fmt.Errorf("register %q on %s in laptop's hosts.json: %w", projectName, hostName, err)
	}
	fmt.Fprintf(stdout, "Registered %q on %s → %s\n", projectName, hostName, canonicalRoot)
	return nil
}

// shellQuote is provided by cmd/canopy/install_tmux.go — it wraps
// strings in single quotes for safe shell interpolation. Used here
// to harden the URL/dest values against shell metacharacters when
// joined into the remote command line.

// initResultEnvVar names the env var canopy init checks for. When
// non-empty, runInit writes one line to that path on successful
// init: `<canonical-project-root>\n`.
//
// Used by `canopy init --on <host>` to round-trip the remote's
// canonical project path back to the laptop, so the laptop can
// register cravd in its hosts.json (Projects map). Without that
// registration, follow-up `canopy new --on tower` fails with
// "host has no projects registered" (resolveOnForNew can't find a
// remote-cwd for the project).
//
// File path is owned by the caller (the SSH-dispatch side picks a
// temp path on the remote and cats it back after init returns).
const initResultEnvVar = "CANOPY_INIT_RESULT_FILE"

// writeInitResultFile writes canonicalRoot to the path named in the
// CANOPY_INIT_RESULT_FILE env var, when set. No-op when unset (the
// common local-CLI case).
//
// Failures are logged but not fatal — init's main outcome already
// succeeded, and a missing result file just means the laptop's
// hosts.json doesn't get auto-registered. The user can register
// manually via `canopy project add <name> <path> --on <host>`.
func writeInitResultFile(canonicalRoot string) {
	path := os.Getenv(initResultEnvVar)
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(canonicalRoot+"\n"), 0o600); err != nil {
		// Surface to debug log via slog (init.go's logger), but
		// don't fail the init or scream at the user — this is a
		// remote-dispatch helper, not core init behavior.
		fmt.Fprintf(os.Stderr, "warning: write %s: %v\n", initResultEnvVar, err)
	}
}
