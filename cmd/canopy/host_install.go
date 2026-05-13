// Command canopy host install installs (or reinstalls) canopy on a
// registered remote host. It SSHes to the host, pipes install.sh from
// the main branch to bash with --yes (and optionally --reinstall), and
// streams output back to the laptop. On success, it re-probes the host
// to confirm canopy is now reachable and reports the new version.
//
// Why this lives next to host.go: it's a host-management verb that
// reads the hosts.json registry, not a workspace operation. The
// matching wizard-side path (`canopy host add --interactive` offering
// install when probeBroken hits) calls the same runHostInstall function
// — single execution path, single set of error messages, single test
// surface.
//
// Distribution shape: source-based, same as install.sh expects. We
// pipe install.sh to bash via curl OR wget (one-liner fallback so
// Alpine boxes without curl pre-installed still work). install.sh
// itself does dep-detection + auto-install (under --yes) on the
// remote, so the laptop never has to know which distro the remote is.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/host"
)

// hostInstallProbeTimeout is the deadline for the post-install
// reachability probe. The install itself can take minutes (clone
// + build + make install), but the probe is just `canopy version`
// over SSH — should be sub-second after a fresh install.
const hostInstallProbeTimeout = 8 * time.Second

func hostInstallCmd() *cobra.Command {
	var (
		reinstall bool
		yes       bool
	)
	c := &cobra.Command{
		Use:   "install <name>",
		Short: "Install (or reinstall) canopy on a registered remote host",
		Long: "Installs canopy on the named host via SSH. Pipes install.sh from main to\n" +
			"bash on the remote with --yes (so the install runs unattended). Missing OS\n" +
			"deps (git, tmux 3.2+, Go 1.22+, make) get auto-installed via the remote's\n" +
			"package manager; this assumes passwordless sudo on the remote when deps\n" +
			"are missing — otherwise sudo will fail and you'll need to install deps\n" +
			"manually first.\n\n" +
			"--reinstall wipes ~/.canopy/src on the remote and re-clones fresh. Use\n" +
			"this for recovery after a broken clone. State (workspaces, hosts.json,\n" +
			"state.json) is preserved.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			h, err := reg.Resolve(args[0])
			if err != nil {
				return err
			}
			return runHostInstall(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				cmd.InOrStdin(), h, reinstall, yes)
		},
	}
	c.Flags().BoolVar(&reinstall, "reinstall", false,
		"wipe ~/.canopy/src on the remote and re-clone fresh (state preserved)")
	c.Flags().BoolVarP(&yes, "yes", "y", false,
		"skip the local confirmation prompt (the remote install always runs --yes)")
	return c
}

// runHostInstall is the shared install path used by `canopy host install`
// AND by host_wizard.go's probeBroken auto-offer. The local prompt only
// fires for the CLI surface; the wizard already showed its own huh
// confirm before calling in, so it passes yes=true to skip the double-
// confirm.
//
// Why exposed at package level: host_wizard.go calls it directly so the
// install path is tested once and the wizard branch shares all error
// handling — no parallel implementations to keep in sync.
func runHostInstall(ctx context.Context, out, errOut io.Writer, in io.Reader, h host.Host, reinstall, yes bool) error {
	fmt.Fprintf(out, "Installing canopy on %s (%s)\n", h.Name, h.SSHTarget)
	if reinstall {
		fmt.Fprintln(out, "  mode: --reinstall (will wipe ~/.canopy/src on remote)")
	}

	if !yes && hostInstallIsTerminal(os.Stdin) {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "This will SSH to the host and run install.sh, which may invoke")
		fmt.Fprintln(out, "sudo on the remote to install missing OS dependencies.")
		if !hostInstallPromptYesNo(in, out) {
			fmt.Fprintln(out, "Cancelled.")
			return nil
		}
	}

	fmt.Fprintln(out, "")
	if err := hostInstallRunRemote(ctx, h.SSHTarget, reinstall, out, errOut); err != nil {
		return fmt.Errorf("canopy host install %s: %w", h.Name, err)
	}

	// Re-probe to confirm canopy now answers. The install can succeed
	// from a shell perspective but leave canopy off the user's PATH
	// (the `~/.local/bin` hint print in install.sh exists precisely
	// for this case). Re-probe catches that and surfaces it as a
	// trailing warning instead of letting the user discover it via a
	// future failed refresh.
	res := hostInstallProbe(ctx, h.SSHTarget, hostInstallProbeTimeout)
	switch res.kind {
	case probeOK:
		fmt.Fprintf(out, "\n✓ canopy installed on %s (%s)\n", h.Name, res.canopyVersion)
	case probeBroken:
		fmt.Fprintf(errOut, "\n⚠ Install ran but `canopy version` still fails on %s: %s\n", h.Name, res.detail)
		fmt.Fprintln(errOut, "  Likely cause: ~/.local/bin isn't on the remote's login PATH.")
		fmt.Fprintln(errOut, "  Add `export PATH=\"$HOME/.local/bin:$PATH\"` to ~/.bashrc on the remote and re-probe.")
	default:
		fmt.Fprintf(errOut, "\n⚠ Install ran but probe failed: %s\n", res.detail)
	}
	return nil
}

// hostInstallRunRemote is the SSH-side execution. Pipes install.sh
// to bash with --yes (and --reinstall if requested), streaming stdout
// + stderr back to the laptop in real time. We use the interactive
// SSHCmd (not SSHCmdBatch) because the user kicked this off
// foreground — if SSH needs to surface a key-prompt or sudo-prompt
// it should reach the user's terminal.
//
// Script construction lives in internal/host.InstallScript so the
// in-TUI Hosts-tab flow (update_host_upgrade.go) shares the exact
// same payload — single seam, single test surface.
//
// Exposed as a package-level var so tests can stub the SSH path
// without forking real ssh. Matches the upgradeRunShell seam.
var hostInstallRunRemote = func(ctx context.Context, sshTarget string, reinstall bool, out, errOut io.Writer) error {
	// Single bash -c invocation. Login shell (-l) so the remote's
	// PATH includes the user's profile additions (~/.local/bin, brew,
	// asdf, etc.) — install.sh's PATH hint depends on this resolution.
	cmd := host.SSHCmd(ctx, sshTarget, "bash", "-lc", host.InstallScript(reinstall))
	cmd.Stdout = out
	cmd.Stderr = errOut
	return cmd.Run()
}

// hostInstallProbe wraps probeSSH so tests can stub the post-install
// reachability check independent of the SSH path. Default forwards to
// the real probeSSH used by the wizard.
var hostInstallProbe = func(ctx context.Context, sshTarget string, timeout time.Duration) probeOutcome {
	return probeSSH(ctx, sshTarget, timeout)
}

// hostInstallIsTerminal mirrors upgradeIsTerminal — same single-syscall
// tty check, exposed as a package-level var so tests can stub the
// "tty? yes/no" decision deterministically without setting up real
// ptys.
var hostInstallIsTerminal = func(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// hostInstallPromptYesNo is the local confirmation gate for the CLI
// surface (NOT the wizard, which has its own huh confirm before
// calling runHostInstall with yes=true).
//
// Same Y-default contract as upgradePromptYesNo: empty/Enter is yes,
// any explicit "n" is no, garbage re-prompts up to 3 times. Exposed
// as a package-level var so tests can stub a canned input source.
var hostInstallPromptYesNo = func(in io.Reader, out io.Writer) bool {
	// Bare minimum prompt — three-strikes input loop is over-engineering
	// for a single y/N gate. Anything that isn't a recognized yes is no.
	fmt.Fprint(out, "Continue? [y/N]: ")
	buf := make([]byte, 16)
	n, _ := in.Read(buf)
	if n == 0 {
		return false
	}
	switch buf[0] {
	case 'y', 'Y':
		return true
	default:
		return false
	}
}
