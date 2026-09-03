package ui

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewHostUpgradeSSHCmd_RunsLoginShell pins the load-bearing invariant
// that the remote shell is `bash -l`. Login shells source ~/.bash_profile
// / ~/.profile, which is one of two places version managers inject the
// toolchain PATH (the other is ~/.bashrc, handled separately by
// remoteEnvPrep's direct mise/asdf activation). Without -l, non-
// interactive SSH inherits a bare default PATH and `make install` can't
// find `go` — the regression that first motivated this helper (reported
// as `canopy upgrade on tower: make: go: No such file or directory`).
func TestNewHostUpgradeSSHCmd_RunsLoginShell(t *testing.T) {
	cmd := newHostUpgradeSSHCmd(context.Background(), "avi@tower", "exec canopy upgrade --yes")
	if len(cmd.Args) < 3 {
		t.Fatalf("expected at least 3 args, got %v", cmd.Args)
	}
	last2 := cmd.Args[len(cmd.Args)-2:]
	if last2[0] != "bash" || last2[1] != "-l" {
		t.Errorf("ssh argv must end with `bash -l` to source login profile on the remote; got %v", last2)
	}
	// bash -l must immediately follow the ssh target slot (ssh's argv
	// convention: target THEN remote command tokens).
	target := cmd.Args[len(cmd.Args)-3]
	if target != "avi@tower" {
		t.Errorf("ssh target must immediately precede `bash -l`; got %q at args[-3]", target)
	}
}

// TestNewHostUpgradeSSHCmd_PipesScriptViaStdin verifies the remote
// script travels via stdin, NOT as an SSH argv. SSH would otherwise
// word-split anything past the target through the remote shell, which
// mangles multi-token scripts like the install.sh curl|wget fallback
// and the remoteEnvPrep chain. This is the same pattern
// internal/host/refresh.go uses.
func TestNewHostUpgradeSSHCmd_PipesScriptViaStdin(t *testing.T) {
	script := `export PATH="$HOME/.local/bin:$PATH"; exec canopy upgrade --yes`
	cmd := newHostUpgradeSSHCmd(context.Background(), "avi@tower", script)
	if cmd.Stdin == nil {
		t.Fatal("Stdin must carry the remote script (got nil)")
	}
	got, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if !strings.Contains(string(got), script) {
		t.Errorf("stdin should contain remote script %q; got %q", script, string(got))
	}
	// Stdin convention: trailing newline so the remote shell treats
	// the script as a complete line. Missing it can leave a heredoc-
	// flavored shell waiting for more input on an open stdin.
	if !strings.HasSuffix(string(got), "\n") {
		t.Errorf("stdin must end with a newline; got %q", string(got))
	}
	// The script must NOT appear in argv — that would re-expose the
	// word-splitting hazard the stdin path fixes.
	for _, a := range cmd.Args {
		if strings.Contains(a, "canopy upgrade") {
			t.Errorf("remote script leaked into argv at %q; must travel via stdin only", a)
		}
	}
}

// TestNewHostUpgradeSSHCmd_PreservesSSHControlOpts pins the ssh -o
// flags that make the in-TUI flow non-interactive: ControlMaster /
// ControlPath / ControlPersist for multiplexing, BatchMode=yes +
// NumberOfPasswordPrompts=0 to prevent a password prompt from hanging
// the goroutine or corrupting the Bubbletea render (ssh writes prompts
// to /dev/tty directly, bypassing our captured stdout/stderr).
//
// Regression target: a future refactor that drops one of these would
// re-introduce the "host without key auth hangs the TUI" bug.
func TestNewHostUpgradeSSHCmd_PreservesSSHControlOpts(t *testing.T) {
	cmd := newHostUpgradeSSHCmd(context.Background(), "avi@tower", "echo")
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"ControlMaster=auto",
		"ControlPersist=300",
		"BatchMode=yes",
		"NumberOfPasswordPrompts=0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh argv missing %q; got %s", want, joined)
		}
	}
}

// TestRemoteEnvPrep_Fragments pins the load-bearing strings in the
// shell snippet we prepend to every remote canopy command. Each
// fragment maps to a documented layer of remoteEnvPrep's strategy; a
// regression here would silently undo the fix for hosts whose Go
// toolchain lives behind a non-interactive-bailing ~/.bashrc.
func TestRemoteEnvPrep_Fragments(t *testing.T) {
	for _, want := range []string{
		// Layer 1: mise direct activation. `command -v` finds mise
		// regardless of bashrc, then `eval $(mise activate bash)`
		// produces the same PATH/shim exports omarchy/default/bash/init
		// emits when sourced interactively.
		`command -v mise`,
		`mise activate bash`,
		// Layer 2: asdf direct activation via its canonical hook.
		`"$HOME/.asdf/asdf.sh"`,
		// Layer 3: static toolchain spots as belt-and-suspenders.
		`"$HOME/.local/bin"`,
		`"/usr/local/go/bin"`,
		`"$HOME/go/bin"`,
		// Dedupe pattern (case glob over a colon-padded $PATH) so a
		// path already on PATH isn't re-prepended on every invocation.
		`case ":$PATH:"`,
		// Errors are swallowed so a broken version-manager install
		// doesn't turn a recoverable upgrade into a hard failure.
		`|| true`,
	} {
		if !strings.Contains(remoteEnvPrep, want) {
			t.Errorf("remoteEnvPrep missing fragment %q\n  got: %s", want, remoteEnvPrep)
		}
	}
}

// TestRemoteCmds_ChainEnvPrep pins that both remote call sites prefix
// the prep snippet. Without it, the remote `canopy upgrade` inherits
// a bare PATH and fails at `make install` with "go: No such file or
// directory" — the bug this fix exists to prevent.
func TestRemoteCmds_ChainEnvPrep(t *testing.T) {
	for name, cmd := range map[string]string{
		"remoteUpgradeCmd":    remoteUpgradeCmd,
		"remoteUseReleaseCmd": remoteUseReleaseCmd,
	} {
		if !strings.HasPrefix(cmd, remoteEnvPrep+";") {
			t.Errorf("%s must start with remoteEnvPrep followed by `;`; got %q", name, cmd)
		}
	}
}

// TestRemoteEnvPrep_RunsUnderBash exercises the prep snippet end-to-end
// against a real bash subprocess so the shell syntax (case-glob dedupe,
// `||` error swallowing, mise/asdf conditionals) doesn't silently rot.
// HOME is overridden to a temp dir so the test never reads the
// developer's real ~/.asdf/asdf.sh; PATH starts bare so PATH-prepending
// can be observed.
//
// We can't write to /usr/local/go/bin from a unit test, so the dedupe
// loop's positive branch is exercised via $HOME/go/bin (materialized
// below) — that asserts the loop runs end-to-end on at least one
// canonical spot without simulating an absolute path we don't own.
func TestRemoteEnvPrep_RunsUnderBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on PATH; remoteEnvPrep is shell-targeted")
	}

	home := t.TempDir()
	goBin := filepath.Join(home, "go", "bin")
	if err := os.MkdirAll(goBin, 0o755); err != nil {
		t.Fatalf("mkdir go/bin: %v", err)
	}

	// Append a probe that echoes the augmented PATH AFTER the prep
	// finishes. The prep ships bit-for-bit identical; we only suffix
	// the probe.
	script := remoteEnvPrep + `; echo "PROBE_PATH=$PATH"`
	// --noprofile: hermetic against the host machine's own
	// /etc/profile[.d] (see TestRemoteEnvPrep_ActivatesMiseInUserLocalBin's
	// doc comment for why this is load-bearing, not cosmetic).
	cmd := exec.Command("bash", "--noprofile", "-l")
	cmd.Stdin = strings.NewReader(script + "\n")
	cmd.Env = []string{
		"HOME=" + home,
		// Bare PATH that doesn't contain any of the prep's target dirs
		// — so any prepend can be observed in PROBE_PATH.
		"PATH=/usr/bin:/bin",
		"TERM=dumb",
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash run failed: %v\noutput:\n%s", err, out)
	}
	probe := extractLineForTest(string(out), "PROBE_PATH=")
	if probe == "" {
		t.Fatalf("PROBE_PATH line missing from output:\n%s", string(out))
	}
	if !strings.Contains(probe, goBin) {
		t.Errorf("PROBE_PATH missing %q\n  probe: %s", goBin, probe)
	}
}

// TestRemoteEnvPrep_DedupesPath verifies the case-glob dedupe loop
// doesn't accumulate duplicate entries when a target path is already
// on PATH. Without dedupe, every invocation would push another copy
// onto PATH — slow growth, but real on long-lived SSH multiplex
// sessions hitting upgrade repeatedly.
func TestRemoteEnvPrep_DedupesPath(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on PATH; remoteEnvPrep is shell-targeted")
	}

	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	script := remoteEnvPrep + `; echo "PROBE_PATH=$PATH"`
	// --noprofile: hermetic against the host machine's own
	// /etc/profile[.d] (see TestRemoteEnvPrep_ActivatesMiseInUserLocalBin's
	// doc comment for why this is load-bearing, not cosmetic).
	cmd := exec.Command("bash", "--noprofile", "-l")
	cmd.Stdin = strings.NewReader(script + "\n")
	cmd.Env = []string{
		"HOME=" + home,
		// Pre-populate PATH with ~/.local/bin — the dedupe should
		// notice it's already present and not prepend a second copy.
		"PATH=" + localBin + ":/usr/bin:/bin",
		"TERM=dumb",
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash run failed: %v\noutput:\n%s", err, out)
	}
	probe := extractLineForTest(string(out), "PROBE_PATH=")
	count := strings.Count(probe, localBin)
	if count != 1 {
		t.Errorf("expected %q to appear exactly once in PROBE_PATH; got %d\n  probe: %s", localBin, count, probe)
	}
}

// TestRemoteEnvPrep_ActivatesMiseInUserLocalBin is the regression
// test for an ordering bug an adversarial review caught pre-ship.
//
// The canonical mise installer (`curl https://mise.run | sh`) drops
// the mise binary at $HOME/.local/bin/mise. On a non-interactive
// `bash -l` (what we get over SSH), ~/.bashrc bails before adding
// ~/.local/bin to PATH — so if `command -v mise` ran *before* the
// static path enumeration, it would not find mise and silently skip
// activation. The shim dir would never be prepended and `make
// install` would die with "go: No such file or directory" — exactly
// reproducing the v0.21.4.0 regression remoteEnvPrep exists to fix.
//
// The fix is the ordering: static-paths loop runs first (which puts
// ~/.local/bin on PATH), THEN `command -v mise` runs (now finds it),
// THEN `mise activate bash` exports the shim path. This test pins
// that contract by:
//
//  1. Stubbing a `mise` binary at $HOME/.local/bin/mise that, when
//     called as `mise activate bash`, emits an export referencing a
//     uniquely-named fake shim dir.
//  2. Starting bash with PATH=/usr/bin:/bin (no ~/.local/bin).
//  3. Asserting the fake shim dir lands in PROBE_PATH — which can
//     only happen if mise activation actually ran.
//
// If a future refactor reorders the snippet to put `command -v mise`
// before the static loop, this test fails because the stub mise
// binary is unreachable at activation time.
//
// Hermeticity note (found 2026-09-02): `-l` alone isn't enough — a
// login shell also sources the HOST MACHINE's own /etc/profile and
// /etc/profile.d/*, which is outside this test's control and varies
// per developer machine. On a box whose system-wide profile already
// wires up mise (e.g. Omarchy Linux's env-bootstrap script, which
// unconditionally appends ~/.local/share/mise/shims and ~/.local/bin
// to PATH for every login shell — even one whose $HOME is this test's
// fake tempdir), that append lands the REAL system `mise` on PATH
// too, and — since it's appended, not prepended, and /usr/bin/mise
// sits ahead of it either way — `command -v mise` can resolve to the
// real system binary instead of this test's stub, independent of
// whatever order remoteEnvPrep's own snippet runs in. The result: the
// test starts asserting something about the *host machine's* shell
// config instead of remoteEnvPrep's ordering contract, and fails (or
// passes) based on which machine runs `go test`, not the code under
// test. `--noprofile` skips /etc/profile and the personal profile
// files entirely so only remoteEnvPrep's own PATH manipulation is in
// play — restoring the isolation the fake $HOME was already trying to
// provide.
func TestRemoteEnvPrep_ActivatesMiseInUserLocalBin(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on PATH; remoteEnvPrep is shell-targeted")
	}

	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatalf("mkdir .local/bin: %v", err)
	}
	// Unique per-test sentinel so a stale fixture from another test
	// can't make this one pass spuriously.
	shimDir := filepath.Join(home, "fake-mise-shims-regression")
	miseStub := "#!/bin/bash\n" +
		`[ "$1" = "activate" ] && printf 'export PATH="%s:$PATH"\n' "` + shimDir + `"` + "\n"
	if err := os.WriteFile(filepath.Join(localBin, "mise"), []byte(miseStub), 0o755); err != nil {
		t.Fatalf("write mise stub: %v", err)
	}

	script := remoteEnvPrep + `; echo "PROBE_PATH=$PATH"`
	// --noprofile: hermetic against the host machine's own
	// /etc/profile[.d] (see TestRemoteEnvPrep_ActivatesMiseInUserLocalBin's
	// doc comment for why this is load-bearing, not cosmetic).
	cmd := exec.Command("bash", "--noprofile", "-l")
	cmd.Stdin = strings.NewReader(script + "\n")
	cmd.Env = []string{
		"HOME=" + home,
		// Critical: ~/.local/bin is NOT on initial PATH. The static
		// loop must add it before `command -v mise` runs.
		"PATH=/usr/bin:/bin",
		"TERM=dumb",
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash run failed: %v\noutput:\n%s", err, out)
	}
	probe := extractLineForTest(string(out), "PROBE_PATH=")
	if probe == "" {
		t.Fatalf("PROBE_PATH line missing from output:\n%s", string(out))
	}
	if !strings.Contains(probe, shimDir) {
		t.Errorf("mise activation didn't run — shim dir %q missing from PROBE_PATH\n"+
			"  probe: %s\n"+
			"  This means the snippet's `command -v mise` ran before the static\n"+
			"  ~/.local/bin prepend, reproducing the ordering bug. Check the order\n"+
			"  of statements in remoteEnvPrep.", shimDir, probe)
	}
}

// extractLineForTest returns the first line in `text` that starts with
// `prefix` (or empty if none). Used to scope substring assertions to
// the probe line, keeping bashrc body output and shell noise out of the
// match window.
func extractLineForTest(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
