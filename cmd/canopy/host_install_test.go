package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/avinashjoshi/canopy/internal/host"
)

// TestRunHostInstallHappyPath stubs the SSH runner + probe so we can
// verify runHostInstall's success path: prints progress, calls runner
// with the right args, re-probes, surfaces the new version.
func TestRunHostInstallHappyPath(t *testing.T) {
	defer restoreHostInstallStubs(stubHostInstallRunRemote(t, func(sshTarget string, reinstall bool, out, errOut io.Writer) error {
		if sshTarget != "avi@tower.tail.ts.net" {
			t.Errorf("ssh target = %q, want %q", sshTarget, "avi@tower.tail.ts.net")
		}
		if reinstall {
			t.Errorf("reinstall = true, want false (test passes reinstall=false)")
		}
		// Simulate install.sh output streaming through.
		out.Write([]byte("canopy install\n  cloning...\n"))
		return nil
	}))()
	defer restoreHostInstallStubs(stubHostInstallProbe(t, func(target string) probeOutcome {
		return probeOutcome{kind: probeOK, canopyVersion: "canopy v0.17.1.0", rtt: 50 * time.Millisecond}
	}))()

	var out, errOut bytes.Buffer
	h := host.Host{Name: "tower", SSHTarget: "avi@tower.tail.ts.net"}
	err := runHostInstall(context.Background(), &out, &errOut, strings.NewReader(""), h, false, true /* yes, skip prompt */)
	if err != nil {
		t.Fatalf("runHostInstall: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Installing canopy on tower") {
		t.Errorf("output should mention which host; got:\n%s", got)
	}
	if !strings.Contains(got, "canopy v0.17.1.0") {
		t.Errorf("output should surface re-probed version; got:\n%s", got)
	}
	if strings.Contains(got, "mode: --reinstall") {
		t.Errorf("default install should NOT print reinstall banner; got:\n%s", got)
	}
}

// TestRunHostInstallReinstallBanner: --reinstall propagates BOTH to
// the runner call AND to the user-facing output (so the user knows
// the existing clone is about to be wiped).
func TestRunHostInstallReinstallBanner(t *testing.T) {
	var sawReinstall bool
	defer restoreHostInstallStubs(stubHostInstallRunRemote(t, func(_ string, reinstall bool, _, _ io.Writer) error {
		sawReinstall = reinstall
		return nil
	}))()
	defer restoreHostInstallStubs(stubHostInstallProbe(t, func(string) probeOutcome {
		return probeOutcome{kind: probeOK, canopyVersion: "canopy v0.17.1.0"}
	}))()

	var out bytes.Buffer
	h := host.Host{Name: "tower", SSHTarget: "avi@tower.tail.ts.net"}
	_ = runHostInstall(context.Background(), &out, io.Discard, strings.NewReader(""), h, true, true)
	if !sawReinstall {
		t.Errorf("runner should have received reinstall=true")
	}
	if !strings.Contains(out.String(), "mode: --reinstall") {
		t.Errorf("output should warn about reinstall mode; got:\n%s", out.String())
	}
}

// TestRunHostInstallProbeBrokenWarns: install ran successfully but
// canopy is still not on the remote's login PATH. We must surface a
// trailing warning (with the PATH hint) instead of silently claiming
// success.
func TestRunHostInstallProbeBrokenWarns(t *testing.T) {
	defer restoreHostInstallStubs(stubHostInstallRunRemote(t, func(_ string, _ bool, _, _ io.Writer) error {
		return nil
	}))()
	defer restoreHostInstallStubs(stubHostInstallProbe(t, func(string) probeOutcome {
		return probeOutcome{kind: probeBroken, detail: "canopy not installed on remote"}
	}))()

	var out, errOut bytes.Buffer
	h := host.Host{Name: "tower", SSHTarget: "avi@tower.tail.ts.net"}
	err := runHostInstall(context.Background(), &out, &errOut, strings.NewReader(""), h, false, true)
	if err != nil {
		t.Fatalf("runHostInstall: %v", err)
	}
	if !strings.Contains(errOut.String(), "still fails") {
		t.Errorf("errOut should warn that probe still fails; got:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "~/.local/bin") {
		t.Errorf("errOut should hint about PATH / ~/.local/bin; got:\n%s", errOut.String())
	}
}

// TestRunHostInstallRunnerError: install.sh on the remote bails out.
// runHostInstall must return a wrapped error so callers (wizard, CLI)
// can surface it without losing the underlying cause.
func TestRunHostInstallRunnerError(t *testing.T) {
	sentinel := errors.New("simulated install.sh failure")
	defer restoreHostInstallStubs(stubHostInstallRunRemote(t, func(_ string, _ bool, _, _ io.Writer) error {
		return sentinel
	}))()
	defer restoreHostInstallStubs(stubHostInstallProbe(t, func(string) probeOutcome {
		t.Errorf("probe should NOT run when install fails")
		return probeOutcome{}
	}))()

	h := host.Host{Name: "tower", SSHTarget: "avi@tower.tail.ts.net"}
	err := runHostInstall(context.Background(), io.Discard, io.Discard, strings.NewReader(""), h, false, true)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel; got %v", err)
	}
	if !strings.Contains(err.Error(), "canopy host install tower") {
		t.Errorf("error must mention command + host name for breadcrumbs; got %v", err)
	}
}

// TestRunHostInstallLocalPromptCancels: when yes=false AND stdin is
// a tty (stubbed) AND the user types n, we must NOT call the SSH
// runner — the prompt is the gate.
func TestRunHostInstallLocalPromptCancels(t *testing.T) {
	defer restoreHostInstallStubs(stubHostInstallIsTerminal(t, true))()
	defer restoreHostInstallStubs(stubHostInstallPromptYesNo(t, false))() // user says no
	defer restoreHostInstallStubs(stubHostInstallRunRemote(t, func(_ string, _ bool, _, _ io.Writer) error {
		t.Errorf("runner should NOT run when user cancels at prompt")
		return nil
	}))()

	var out bytes.Buffer
	h := host.Host{Name: "tower", SSHTarget: "avi@tower.tail.ts.net"}
	err := runHostInstall(context.Background(), &out, io.Discard, strings.NewReader(""), h, false, false /* yes=false → prompt */)
	if err != nil {
		t.Errorf("cancel should be clean (nil error); got %v", err)
	}
	if !strings.Contains(out.String(), "Cancelled") {
		t.Errorf("output should report cancellation; got:\n%s", out.String())
	}
}

// TestRunHostInstallLocalPromptApproves: when yes=false AND stdin is
// a tty AND the user types y, we proceed to the runner.
func TestRunHostInstallLocalPromptApproves(t *testing.T) {
	defer restoreHostInstallStubs(stubHostInstallIsTerminal(t, true))()
	defer restoreHostInstallStubs(stubHostInstallPromptYesNo(t, true))()
	ranInstall := false
	defer restoreHostInstallStubs(stubHostInstallRunRemote(t, func(_ string, _ bool, _, _ io.Writer) error {
		ranInstall = true
		return nil
	}))()
	defer restoreHostInstallStubs(stubHostInstallProbe(t, func(string) probeOutcome {
		return probeOutcome{kind: probeOK, canopyVersion: "canopy v0.17.1.0"}
	}))()

	h := host.Host{Name: "tower", SSHTarget: "avi@tower.tail.ts.net"}
	_ = runHostInstall(context.Background(), io.Discard, io.Discard, strings.NewReader(""), h, false, false)
	if !ranInstall {
		t.Errorf("runner should have run after user approved the prompt")
	}
}

// TestHostInstallPromptYesNo covers both yes-paths (y / Y) and the
// implicit no path (empty input, n, other char). The Y-default
// convention from `canopy upgrade` does NOT apply here — empty input
// is no, because this prompt can fire over a TTY-less pipe and we
// don't want to misread a stray byte as consent for sudo.
func TestHostInstallPromptYesNo(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"n\n", false},
		{"\n", false}, // empty → no (different from upgrade's Y-default)
		{"", false},   // EOF → no
		{"x\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var out bytes.Buffer
			got := hostInstallPromptYesNo(strings.NewReader(tc.in), &out)
			if got != tc.want {
				t.Errorf("input=%q got=%v want=%v", tc.in, got, tc.want)
			}
			if !strings.Contains(out.String(), "Continue?") {
				t.Errorf("prompt should ask 'Continue?'; got %q", out.String())
			}
		})
	}
}

// --- stub helpers ---

// Stub functions return a closure that restores the previous value
// when called. Pattern matches upgrade_test.go's stub-and-restore
// usage; defer-call-the-result-of-stub is idiomatic for that style.

func stubHostInstallRunRemote(t *testing.T, fn func(sshTarget string, reinstall bool, out, errOut io.Writer) error) func() {
	t.Helper()
	prev := hostInstallRunRemote
	hostInstallRunRemote = func(_ context.Context, sshTarget string, reinstall bool, out, errOut io.Writer) error {
		return fn(sshTarget, reinstall, out, errOut)
	}
	return func() { hostInstallRunRemote = prev }
}

func stubHostInstallProbe(t *testing.T, fn func(target string) probeOutcome) func() {
	t.Helper()
	prev := hostInstallProbe
	hostInstallProbe = func(_ context.Context, target string, _ time.Duration) probeOutcome {
		return fn(target)
	}
	return func() { hostInstallProbe = prev }
}

func stubHostInstallIsTerminal(t *testing.T, isTTY bool) func() {
	t.Helper()
	prev := hostInstallIsTerminal
	hostInstallIsTerminal = func(*os.File) bool { return isTTY }
	return func() { hostInstallIsTerminal = prev }
}

func stubHostInstallPromptYesNo(t *testing.T, approve bool) func() {
	t.Helper()
	prev := hostInstallPromptYesNo
	hostInstallPromptYesNo = func(io.Reader, io.Writer) bool { return approve }
	return func() { hostInstallPromptYesNo = prev }
}

// restoreHostInstallStubs is a no-op wrapper that just returns the
// restore closure — exists so test bodies read `defer restoreHostInstallStubs(stub(...))()` and
// the parens-after-defer convention stays consistent.
func restoreHostInstallStubs(restore func()) func() { return restore }
