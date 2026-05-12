package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/avinashjoshi/canopy/internal/host"
)

// runHostAddWizard is `canopy host add --interactive`. Used by the
// Hosts TUI tab's `n` keybind (via tea.ExecProcess) AND by power
// users who want the guided flow from the bare CLI.
//
// Flow:
//
//  1. huh form: prompts for host name + SSH target.
//  2. validate the name (no @ : /), check no duplicate in registry.
//  3. probe connectivity with BatchMode SSH (5s deadline).
//  4. on success: register host, print confirmation.
//  5. on "Permission denied": offer to run `ssh-copy-id <target>` —
//     this hands the terminal to ssh-copy-id which uses the standard
//     interactive password prompt. After ssh-copy-id succeeds, re-probe
//     to confirm, then register.
//  6. on network failure (offline / no route): warn, ask whether to
//     register anyway. Most users say yes (host might just be asleep).
//
// All output goes to stdout/stderr — this is a foreground UX, not a
// TUI sub-component, so terminal manipulation is fine.
func runHostAddWizard(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	var (
		name      string
		sshTarget string
	)

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Host name").
				Description("Short identifier you'll use with --on. Must not contain @ or : or whitespace.").
				Placeholder("tower").
				Value(&name).
				Validate(func(s string) error {
					s = strings.TrimSpace(s)
					if s == "" {
						return errors.New("name is required")
					}
					if strings.ContainsAny(s, "@:/ \t\n") {
						return errors.New("name must not contain @, :, /, or whitespace")
					}
					if s == "local" {
						return errors.New("\"local\" is reserved")
					}
					reg, err := loadHostRegistry()
					if err != nil {
						return nil // skip duplicate check if registry can't load
					}
					if _, err := reg.Resolve(s); err == nil {
						return fmt.Errorf("host %q is already registered (use a different name or `canopy host rm %s` first)", s, s)
					}
					return nil
				}),

			huh.NewInput().
				Title("SSH target").
				Description("user@hostname or hostname:port — what you'd type after `ssh`.").
				Placeholder("avi@tower.tail-xxxx.ts.net").
				Value(&sshTarget).
				Validate(func(s string) error {
					s = strings.TrimSpace(s)
					if s == "" {
						return errors.New("SSH target is required")
					}
					return nil
				}),
		),
	).WithProgramOptions()

	if err := form.Run(); err != nil {
		// huh.ErrUserAborted is the "user pressed Ctrl+C" signal.
		// Return cleanly — no host registered, no error to surface.
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Fprintln(errOut, "Cancelled.")
			return nil
		}
		return fmt.Errorf("wizard: %w", err)
	}
	name = strings.TrimSpace(name)
	sshTarget = strings.TrimSpace(sshTarget)

	// Probe SSH connectivity with BatchMode (no password prompts).
	fmt.Fprintf(out, "\nProbing SSH connection to %s ...\n", sshTarget)
	probeResult := probeSSH(ctx, sshTarget, 5*time.Second)
	switch probeResult.kind {
	case probeOK:
		fmt.Fprintf(out, "✓ Connection confirmed (%s, %.1fs RTT)\n", probeResult.canopyVersion, probeResult.rtt.Seconds())

	case probePermissionDenied:
		fmt.Fprintln(out, "✗ SSH key auth failed: Permission denied.")
		var setupKeys bool
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Set up passwordless SSH now?").
					Description("Runs `ssh-copy-id "+sshTarget+"` — you'll be asked for the SSH password once, then key auth works from then on.").
					Affirmative("Yes, run ssh-copy-id").
					Negative("No, I'll set it up later").
					Value(&setupKeys),
			),
		).Run()
		if err != nil && !errors.Is(err, huh.ErrUserAborted) {
			return fmt.Errorf("wizard: %w", err)
		}
		if setupKeys {
			if err := runSSHCopyID(sshTarget, in, out, errOut); err != nil {
				fmt.Fprintf(errOut, "ssh-copy-id failed: %v\n", err)
				fmt.Fprintln(errOut, "You can re-run `ssh-copy-id "+sshTarget+"` manually later. Registering host anyway — refreshes will fail until keys are set up.")
			} else {
				// Re-probe to confirm keys work.
				fmt.Fprintf(out, "\nRe-probing %s with new keys ...\n", sshTarget)
				if r := probeSSH(ctx, sshTarget, 5*time.Second); r.kind == probeOK {
					fmt.Fprintf(out, "✓ Key auth now works (%s, %.1fs RTT)\n", r.canopyVersion, r.rtt.Seconds())
				} else {
					fmt.Fprintf(errOut, "⚠ Re-probe failed: %s\nRegistering host anyway.\n", r.detail)
				}
			}
		} else {
			fmt.Fprintln(errOut, "OK; refreshes will fail until you run `ssh-copy-id "+sshTarget+"`.")
		}

	case probeOffline:
		fmt.Fprintf(errOut, "✗ Could not reach %s: %s\n", sshTarget, probeResult.detail)
		var registerAnyway bool
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Register anyway?").
					Description("The host might be asleep / network might be down. Refreshes will retry; if the host comes online it'll show up automatically.").
					Affirmative("Yes, register").
					Negative("No, cancel").
					Value(&registerAnyway),
			),
		).Run()
		if err != nil || !registerAnyway {
			fmt.Fprintln(errOut, "Cancelled.")
			return nil
		}

	case probeBroken:
		fmt.Fprintf(errOut, "⚠ Connected to %s but canopy isn't installed there (or isn't on PATH): %s\n", sshTarget, probeResult.detail)
		fmt.Fprintln(out, "Install canopy on the remote first:")
		fmt.Fprintln(out, "  ssh "+sshTarget+" 'curl -fsSL https://canopy.dev/install.sh | sh'")
		fmt.Fprintln(out, "Or scp a built binary to ~/.local/bin/ on that host.")
		var registerAnyway bool
		err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Register anyway?").
					Description("Refreshes will fail until canopy is installed on the remote.").
					Affirmative("Yes, register").
					Negative("No, cancel").
					Value(&registerAnyway),
			),
		).Run()
		if err != nil || !registerAnyway {
			fmt.Fprintln(errOut, "Cancelled.")
			return nil
		}
	}

	// Register the host.
	reg, err := loadHostRegistry()
	if err != nil {
		return err
	}
	if err := reg.Add(name, host.Host{SSHTarget: sshTarget}); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n✓ Registered host %q → %s\n", name, sshTarget)
	fmt.Fprintf(out, "\nNext: register a project on this host with\n  canopy project add <name> <remote-path> --on %s\n", name)
	return nil
}

// probeSSH result kinds — keyed off the error string for now (good
// enough for v0.17; can move to typed errors when SSH client wrappers
// expose them).
type probeKind int

const (
	probeOK probeKind = iota
	probePermissionDenied
	probeOffline
	probeBroken
)

type probeOutcome struct {
	kind          probeKind
	canopyVersion string // populated when kind == probeOK
	rtt           time.Duration
	detail        string // user-facing error detail, empty on success
}

// probeSSH runs a lightweight version probe on the remote, with detailed
// error classification. Returns quickly (≤5s) thanks to BatchMode +
// ConnectTimeout.
//
// Probe shape:
//
//	export PATH=$HOME/.local/bin:$PATH
//	canopy version 2>&1
//
// `canopy version` is the actual subcommand (not --version). On
// success it prints the version. On missing-binary the shell prints
// "canopy: command not found" and exits non-zero. We distinguish:
//
//   - SSH-level errors (Permission denied, network) → probePermissionDenied
//     or probeOffline, NO canopy on the remote was reached.
//   - SSH OK + bash "command not found" → probeBroken, SSH reaches but
//     canopy isn't installed.
//   - SSH OK + canopy ran but exited non-zero (e.g., old canopy that
//     doesn't have the version subcommand) → probeBroken with the
//     output as the detail. User sees "canopy too old" guidance.
//   - SSH OK + canopy printed version → probeOK.
func probeSSH(ctx context.Context, sshTarget string, timeout time.Duration) probeOutcome {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := host.SSHCmdBatch(ctx, sshTarget, "bash", "-l")
	cmd.Stdin = strings.NewReader(`export PATH="$HOME/.local/bin:$PATH"
canopy version 2>&1
`)
	out, err := cmd.CombinedOutput()
	rtt := time.Since(start)
	outStr := string(out)

	// SSH-level failures: detect from the stderr/output content.
	// These mean we never reached canopy on the remote at all.
	if strings.Contains(outStr, "Permission denied") {
		return probeOutcome{kind: probePermissionDenied, rtt: rtt, detail: "Permission denied"}
	}
	if strings.Contains(outStr, "Connection refused") ||
		strings.Contains(outStr, "Could not resolve hostname") ||
		strings.Contains(outStr, "no route to host") ||
		strings.Contains(outStr, "timed out") ||
		strings.Contains(outStr, "Connection timed out") {
		return probeOutcome{kind: probeOffline, rtt: rtt, detail: truncateProbe(outStr)}
	}
	// SSH reached, but canopy not installed.
	if strings.Contains(outStr, "command not found") ||
		strings.Contains(outStr, "canopy: not found") {
		return probeOutcome{kind: probeBroken, rtt: rtt, detail: "canopy not installed on remote"}
	}
	// SSH reached, canopy found but exited non-zero (likely an older
	// canopy that doesn't know the version subcommand, or some other
	// wire-format mismatch).
	if err != nil {
		return probeOutcome{kind: probeBroken, rtt: rtt, detail: truncateProbe(outStr)}
	}
	// Success: canopy ran and printed version info.
	return probeOutcome{kind: probeOK, canopyVersion: firstLine(outStr), rtt: rtt}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func truncateProbe(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// runSSHCopyID execs ssh-copy-id with the standard terminal so the
// user gets the normal password prompt. Returns whatever ssh-copy-id
// returns. exec.Cmd not host.SSHCmd because ssh-copy-id is its own
// binary, not raw ssh.
func runSSHCopyID(target string, in io.Reader, out, errOut io.Writer) error {
	binPath, err := exec.LookPath("ssh-copy-id")
	if err != nil {
		return fmt.Errorf("ssh-copy-id not found in PATH — install openssh-copy-id (Debian) / openssh-clients (Arch)")
	}
	fmt.Fprintf(out, "\nRunning: ssh-copy-id %s\n(You'll be asked for the SSH password once.)\n\n", target)
	cmd := exec.Command(binPath, target)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errOut
	return cmd.Run()
}
