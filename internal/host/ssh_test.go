package host

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSSHCmd_ControlMasterFlags verifies that every ssh invocation
// carries the ControlMaster multiplex flags. The first call to a fresh
// host pays the handshake; every subsequent call within ControlPersist
// reuses the socket. Without these flags every refresh tick burns
// 50-300ms on handshakes — the TUI feels sluggish over tailscale-WAN.
func TestSSHCmd_ControlMasterFlags(t *testing.T) {
	cmd := SSHCmd(context.Background(), "avi@tower", "canopy", "ls")
	args := cmd.Args
	mustContainPair(t, args, "-o", "ControlMaster=auto")
	mustContainPrefix(t, args, "-o", "ControlPath=")
	mustContainPair(t, args, "-o", "ControlPersist=300")
	mustContainPair(t, args, "-o", "ConnectTimeout=5")
}

// TestSSHCmd_TargetAndArgsOrder verifies the SSH-target appears before
// the remote command, and that the remote command args are passed
// through in order. ssh's CLI requires this order; getting it wrong
// silently treats the command as an additional ssh flag.
func TestSSHCmd_TargetAndArgsOrder(t *testing.T) {
	cmd := SSHCmd(context.Background(), "avi@tower", "canopy", "new", "--name", "oauth-fix")
	args := cmd.Args
	// args[0] is "ssh" itself
	if args[0] != "ssh" {
		t.Fatalf("args[0] = %q, want \"ssh\"", args[0])
	}

	targetIdx := indexOf(args, "avi@tower")
	if targetIdx < 0 {
		t.Fatalf("target not found in args: %v", args)
	}

	// Remote command args must come AFTER the target.
	for _, want := range []string{"canopy", "new", "--name", "oauth-fix"} {
		idx := indexOf(args, want)
		if idx < 0 {
			t.Errorf("expected remote arg %q in args: %v", want, args)
			continue
		}
		if idx <= targetIdx {
			t.Errorf("remote arg %q appears at idx %d, before target at idx %d", want, idx, targetIdx)
		}
	}
}

// TestSSHCmd_PreservesArgvOrder is the regression test for the most
// likely future bug — someone rearranges sshArgs slice and the remote
// arg order silently breaks. canopy new --branch foo --name bar must
// arrive at the remote as `canopy new --branch foo --name bar`, not
// reshuffled, because cobra's parser is order-sensitive in some flag
// combinations.
func TestSSHCmd_PreservesArgvOrder(t *testing.T) {
	remoteArgs := []string{"canopy", "new", "--branch", "feat/x", "--name", "bar", "--no-attach"}
	cmd := SSHCmd(context.Background(), "tower", remoteArgs...)

	// Find the start of the remote command (first occurrence of "canopy" after the ssh flags).
	args := cmd.Args
	start := indexOf(args, "canopy")
	if start < 0 {
		t.Fatalf("canopy not found in args: %v", args)
	}
	got := args[start:]
	if len(got) != len(remoteArgs) {
		t.Fatalf("remote args length: got %d (%v), want %d (%v)", len(got), got, len(remoteArgs), remoteArgs)
	}
	for i, want := range remoteArgs {
		if got[i] != want {
			t.Errorf("remote args[%d] = %q, want %q", i, got[i], want)
		}
	}
}

// TestSSHCmdBatch_BatchModeFlags: SSHCmdBatch (unlike SSHCmd) must set
// BatchMode=yes and NumberOfPasswordPrompts=0 so a host with no cached
// key auth fails fast with "Permission denied" instead of opening
// /dev/tty for a password prompt. Regression test for the v0.22.x
// clipboard-bridge auto-setup bug: internal/clipboard.defaultSSHExec
// used to build on the non-batch SSHCmd, which is safe when a real
// terminal is attached (canopy host clipboard <name> run by hand, or a
// tea.ExecProcess handoff) but hangs and corrupts the render when the
// SSH call runs unattended inside a live Bubbletea alt-screen — exactly
// what the auto-setup path does. This test pins the primitive
// defaultSSHExec now depends on; see also
// internal/clipboard/host_install.go's defaultSSHExec doc comment.
func TestSSHCmdBatch_BatchModeFlags(t *testing.T) {
	cmd := SSHCmdBatch(context.Background(), "avi@tower", "canopy", "ls", "--json")
	args := cmd.Args
	mustContainPair(t, args, "-o", "BatchMode=yes")
	mustContainPair(t, args, "-o", "NumberOfPasswordPrompts=0")
	mustContainPair(t, args, "-o", "ControlMaster=auto")
	targetIdx := indexOf(args, "avi@tower")
	if targetIdx < 0 {
		t.Fatalf("target not found in args: %v", args)
	}
	if args[targetIdx-1] != "--" {
		t.Errorf("target at idx %d must be immediately preceded by \"--\"; args: %v", targetIdx, args)
	}
}

// TestSSHCmd_NilArgs verifies that calling SSHCmd with no remote command
// (e.g., just to open a master connection) doesn't panic and produces a
// valid ssh-target-only invocation.
func TestSSHCmd_NilArgs(t *testing.T) {
	cmd := SSHCmd(context.Background(), "tower")
	if cmd == nil {
		t.Fatal("SSHCmd returned nil")
	}
	if indexOf(cmd.Args, "tower") < 0 {
		t.Errorf("target not in args: %v", cmd.Args)
	}
}

// TestMoshCmd_TargetSeparator verifies the mosh syntax
// `mosh -- <target> <cmd...>` is constructed correctly: exactly ONE
// leading `--` protects target from being parsed as a mosh option
// (mosh's own usage is "[options] [--] [user@]host [command...]" —
// confirmed by PoC that without it, an option-shaped target like
// "--server=..." is parsed as a real mosh flag, not the host), and NO
// second "--" between target and the command — mosh's usage has no
// separator there, and one used to be present here (inherited from
// before the security fix added the protective leading one) until it
// was confirmed live to break real attach: mosh forwards everything
// after target verbatim as [command...], so a stray "--" becomes the
// first element of the command mosh-server tries to execvp
// ("mosh-server: execvp: --: No such file or directory").
func TestMoshCmd_TargetSeparator(t *testing.T) {
	cmd := MoshCmd(context.Background(), "avi@tower", "canopy", "switch", "oauth-fix")
	args := cmd.Args
	if args[0] != "mosh" {
		t.Fatalf("args[0] = %q, want \"mosh\"", args[0])
	}

	targetIdx := indexOf(args, "avi@tower")
	if targetIdx < 0 {
		t.Fatalf("target not found in args: %v", args)
	}
	if targetIdx == 0 || args[targetIdx-1] != "--" {
		t.Fatalf("target at idx %d must be immediately preceded by \"--\" (protects an option-shaped target from mosh's own flag parser); args: %v", targetIdx, args)
	}

	// Command args must follow target DIRECTLY — no second "--".
	want := []string{"canopy", "switch", "oauth-fix"}
	got := args[targetIdx+1:]
	if len(got) != len(want) {
		t.Fatalf("post-target args: got %v, want %v (no separator between target and command)", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("post-target arg[%d] = %q, want %q", i, got[i], w)
		}
	}

	// Exactly one "--" in the whole argv — a second one anywhere would
	// reintroduce the live bug this test guards against.
	dashCount := 0
	for _, a := range args {
		if a == "--" {
			dashCount++
		}
	}
	if dashCount != 1 {
		t.Errorf("found %d \"--\" tokens in args; want exactly 1: %v", dashCount, args)
	}
}

// TestMoshCmd_DashPrefixedTargetIsProtected is the direct regression
// test for the security fix: a target string shaped like a mosh option
// must land in argv strictly AFTER the leading "--", never be
// reordered or dropped, so mosh's own parser can't mistake it for a
// flag (e.g. "--server=malicious-command" — confirmed by PoC to
// otherwise be interpreted as mosh's --server option, i.e. the command
// mosh runs to start the remote session).
func TestMoshCmd_DashPrefixedTargetIsProtected(t *testing.T) {
	evil := "--server=malicious-command"
	cmd := MoshCmd(context.Background(), evil, "canopy", "switch", "foo")
	args := cmd.Args

	leadingDashIdx := indexOf(args, "--")
	if leadingDashIdx < 0 {
		t.Fatalf("no leading \"--\" found; dash-prefixed target would be parsed as a mosh option: %v", args)
	}
	targetIdx := indexOf(args, evil)
	if targetIdx < 0 {
		t.Fatalf("target %q not found verbatim in args: %v", evil, args)
	}
	if targetIdx != leadingDashIdx+1 {
		t.Errorf("target at idx %d must immediately follow the leading \"--\" at idx %d; args: %v", targetIdx, leadingDashIdx, args)
	}
}

// TestCheckMoshAvailable_Errors verifies the error returned when mosh is
// missing carries useful installation instructions. We don't test the
// success path because that depends on the test runner's environment.
func TestCheckMoshAvailable_Errors(t *testing.T) {
	// We can't easily simulate mosh-missing without manipulating PATH,
	// so we just verify the error type is correctly shaped when we
	// construct one manually.
	original := errors.New("exec: \"mosh\": executable file not found in $PATH")
	err := &ErrMoshMissing{Inner: original}
	if !strings.Contains(err.Error(), "mosh is not installed") {
		t.Errorf("error message should mention mosh: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "pacman") {
		t.Errorf("error message should give an install hint: %s", err.Error())
	}
	if !errors.Is(err, original) {
		t.Errorf("errors.Is should unwrap to inner: %v", err)
	}
}

// TestSSHRunUser_LoginShellAndPTY verifies the v0.20 ad-hoc dispatch
// helper builds an ssh invocation that:
//   - allocates a remote pty (-t) so git auth prompts read from /dev/tty
//   - wraps the remote command in `bash -lc` so the user's login
//     profile sources PATH (fixing the exit-127 "canopy not found"
//     case the Add Project --on flow hit pre-fix)
func TestSSHRunUser_LoginShellAndPTY(t *testing.T) {
	cmd := SSHRunUser(context.Background(), "avi@tower", "canopy init 'https://x.git'")
	args := cmd.Args
	if args[0] != "ssh" {
		t.Fatalf("args[0] = %q, want ssh", args[0])
	}
	if indexOf(args, "-t") < 0 {
		t.Errorf("SSHRunUser missing -t (pty allocation): %v", args)
	}
	// bash -lc <cmd> must appear in order, AFTER the target.
	targetIdx := indexOf(args, "avi@tower")
	if targetIdx < 0 {
		t.Fatalf("target not in args: %v", args)
	}
	bashIdx := indexOf(args, "bash")
	if bashIdx < targetIdx {
		t.Errorf("bash must come after target; bashIdx=%d targetIdx=%d", bashIdx, targetIdx)
	}
	if args[bashIdx+1] != "-lc" {
		t.Errorf("expected -lc after bash, got %q", args[bashIdx+1])
	}
	// The remoteCmd is outer-shell-quoted before being passed as the
	// argv slot. SSH joins all post-target args with spaces and
	// transmits one string to the remote shell, so the wrapping
	// 'single quotes' must arrive intact for `bash -lc` to consume
	// the whole command as one token. (Without this the symptom is
	// `init: line 1: canopy: command not found` — bash splits the
	// command on spaces and runs only the first word.)
	//
	// SSHRunUser also prepends `export PATH=...` to ensure canopy is
	// found even when the remote login profile doesn't add
	// ~/.local/bin to PATH (which we observed in the wild on a fresh
	// Arch host: bash -lc PATH was /usr/local/sbin:/usr/local/bin:
	// /usr/bin:... only).
	got := args[bashIdx+2]
	if !strings.Contains(got, `canopy init '\''https://x.git'\''`) {
		t.Errorf("remote cmd missing properly-quoted user command; got %q", got)
	}
	if !strings.Contains(got, `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Errorf("remote cmd missing PATH prepend (canopy must be findable on hosts without profile setup); got %q", got)
	}
	if got[0] != '\'' || got[len(got)-1] != '\'' {
		t.Errorf("remote cmd not outer-quoted; got %q", got)
	}
	// ControlMaster reuse must still apply so a previously-opened
	// socket is shared (no re-handshake on the second dispatch).
	mustContainPair(t, args, "-o", "ControlMaster=auto")
}

// TestSSHRunUserBatch_BatchModeAndNoPTY: the non-interactive sibling
// of SSHRunUser. Must set BatchMode=yes (no password prompts) and
// NumberOfPasswordPrompts=0 (belt-and-suspenders), and must NOT
// allocate a pty (-t absent) because background TUI loaders have no
// real terminal to attach to. Otherwise mirrors SSHRunUser's
// login-shell + outer-quote behavior.
func TestSSHRunUserBatch_BatchModeAndNoPTY(t *testing.T) {
	cmd := SSHRunUserBatch(context.Background(), "avi@tower", "gh pr list --state open --limit 20")
	args := cmd.Args
	mustContainPair(t, args, "-o", "BatchMode=yes")
	mustContainPair(t, args, "-o", "NumberOfPasswordPrompts=0")
	mustContainPair(t, args, "-o", "ControlMaster=auto")
	if indexOf(args, "-t") >= 0 {
		t.Errorf("SSHRunUserBatch must NOT allocate a pty (no -t flag); got %v", args)
	}
	// bash -lc must still wrap the user command.
	bashIdx := indexOf(args, "bash")
	if bashIdx < 0 {
		t.Fatalf("bash not in args: %v", args)
	}
	if args[bashIdx+1] != "-lc" {
		t.Errorf("expected -lc after bash; got %q", args[bashIdx+1])
	}
	got := args[bashIdx+2]
	if got[0] != '\'' || got[len(got)-1] != '\'' {
		t.Errorf("remote cmd not outer-quoted; got %q", got)
	}
	if !strings.Contains(got, "gh pr list --state open --limit 20") {
		t.Errorf("remote cmd missing user command; got %q", got)
	}
}

// TestShellSingleQuote_EscapesInnerQuotes: paths and args with embedded
// single quotes still parse correctly after the standard close-quote/
// escape/re-open trick.
func TestShellSingleQuote_EscapesInnerQuotes(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"plain", "'plain'"},
		{"/home/avi/Work", "'/home/avi/Work'"},
		{"it's", `'it'\''s'`},
		{"", "''"},
	}
	for _, tc := range tests {
		if got := ShellSingleQuote(tc.in); got != tc.want {
			t.Errorf("ShellSingleQuote(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestCanopyHome_NonEmpty ensures the helper never returns an empty
// string (which would produce an invalid ControlPath like `ssh-%C.sock`
// in the cwd).
func TestCanopyHome_NonEmpty(t *testing.T) {
	got := canopyHome()
	if got == "" {
		t.Fatal("canopyHome() returned empty string")
	}
}

// exitCmd builds a *exec.Cmd that exits with the given code, via `sh -c
// exit N`. Used across the SSHAttachLoop tests below to simulate ssh's
// exit-255-means-transport-failure convention without a real network.
func exitCmd(code int) *exec.Cmd {
	if _, err := exec.LookPath("sh"); err != nil {
		return nil
	}
	return exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
}

// TestSSHAttachLoop_CleanExitStopsImmediately: a remote command that
// ends normally (exit 0 — e.g. the user detached from tmux
// deliberately) must stop the loop without retrying or sleeping.
func TestSSHAttachLoop_CleanExitStopsImmediately(t *testing.T) {
	if exitCmd(0) == nil {
		t.Skip("sh not on PATH")
	}
	slept := 0
	err := SSHAttachLoop(context.Background(), func() *exec.Cmd { return exitCmd(0) }, &bytes.Buffer{}, SSHAttachLoopOptions{
		Sleep: func(time.Duration) { slept++ },
	})
	if err != nil {
		t.Fatalf("SSHAttachLoop() = %v; want nil", err)
	}
	if slept != 0 {
		t.Errorf("slept %d times on a clean exit; want 0", slept)
	}
}

// TestSSHAttachLoop_RetriesOn255ThenSucceeds: exit 255 is ssh's own
// "transport failed" signal (ssh(1): "255 if an error occurred"). A
// single transport failure followed by a clean reconnect must succeed
// overall, having slept exactly once for the one retry.
func TestSSHAttachLoop_RetriesOn255ThenSucceeds(t *testing.T) {
	if exitCmd(0) == nil {
		t.Skip("sh not on PATH")
	}
	calls := 0
	slept := 0
	err := SSHAttachLoop(context.Background(), func() *exec.Cmd {
		calls++
		if calls == 1 {
			return exitCmd(255)
		}
		return exitCmd(0)
	}, &bytes.Buffer{}, SSHAttachLoopOptions{
		Sleep: func(time.Duration) { slept++ },
	})
	if err != nil {
		t.Fatalf("SSHAttachLoop() = %v; want nil", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d; want 2 (one failure + one success)", calls)
	}
	if slept != 1 {
		t.Errorf("slept %d times; want exactly 1 backoff between the two attempts", slept)
	}
}

// TestSSHAttachLoop_NonTransportErrorStopsWithoutRetry: an exit code
// other than 0 or 255 means the REMOTE command itself failed on its
// own terms (e.g. "workspace not found") — retrying would just repeat
// the same failure. Must stop immediately, no retry, no sleep.
func TestSSHAttachLoop_NonTransportErrorStopsWithoutRetry(t *testing.T) {
	if exitCmd(3) == nil {
		t.Skip("sh not on PATH")
	}
	calls := 0
	slept := 0
	err := SSHAttachLoop(context.Background(), func() *exec.Cmd {
		calls++
		return exitCmd(3)
	}, &bytes.Buffer{}, SSHAttachLoopOptions{
		Sleep: func(time.Duration) { slept++ },
	})
	if err == nil {
		t.Fatal("SSHAttachLoop() = nil; want the remote command's own failure surfaced")
	}
	if calls != 1 {
		t.Errorf("calls = %d; want exactly 1 (non-255 exit must not retry)", calls)
	}
	if slept != 0 {
		t.Errorf("slept %d times; want 0 (non-255 exit must not retry)", slept)
	}
}

// TestSSHAttachLoop_CommandFailsToStartStopsWithoutRetry: a command
// that never even starts (binary missing, permission denied) fails
// with an error that is NOT *exec.ExitError — errors.As must not match
// it, so this must be classified the same as a non-255 exit: stop
// immediately, no retry. Distinct from
// TestSSHAttachLoop_NonTransportErrorStopsWithoutRetry, which covers a
// command that DID start but exited with the wrong code.
func TestSSHAttachLoop_CommandFailsToStartStopsWithoutRetry(t *testing.T) {
	calls := 0
	slept := 0
	err := SSHAttachLoop(context.Background(), func() *exec.Cmd {
		calls++
		return exec.Command("/nonexistent/definitely-not-a-binary-xyz")
	}, &bytes.Buffer{}, SSHAttachLoopOptions{
		Sleep: func(time.Duration) { slept++ },
	})
	if err == nil {
		t.Fatal("SSHAttachLoop() = nil; want the start failure surfaced")
	}
	if calls != 1 {
		t.Errorf("calls = %d; want exactly 1 (a non-ExitError start failure must not retry)", calls)
	}
	if slept != 0 {
		t.Errorf("slept %d times; want 0 (a non-ExitError start failure must not retry)", slept)
	}
}

// TestSSHAttachLoop_GivesUpAfterMaxAttempts: a host that's genuinely
// unreachable (every attempt exits 255) must stop retrying at
// MaxAttempts rather than looping forever, and the returned error
// should say so.
func TestSSHAttachLoop_GivesUpAfterMaxAttempts(t *testing.T) {
	if exitCmd(255) == nil {
		t.Skip("sh not on PATH")
	}
	calls := 0
	slept := 0
	err := SSHAttachLoop(context.Background(), func() *exec.Cmd {
		calls++
		return exitCmd(255)
	}, &bytes.Buffer{}, SSHAttachLoopOptions{
		MaxAttempts: 3,
		Sleep:       func(time.Duration) { slept++ },
	})
	if err == nil {
		t.Fatal("SSHAttachLoop() = nil; want an error after exhausting MaxAttempts")
	}
	if !strings.Contains(err.Error(), "gave up after 3 attempts") {
		t.Errorf("error = %q; want it to mention giving up after 3 attempts", err.Error())
	}
	if calls != 3 {
		t.Errorf("calls = %d; want exactly MaxAttempts (3)", calls)
	}
	if slept != 2 {
		t.Errorf("slept %d times; want 2 (one backoff between each of the 3 attempts)", slept)
	}
}

// TestSSHAttachLoop_StatusLinesWrittenOnRetry: the reconnect status
// line is the only signal the user gets that the loop is retrying
// instead of hanging — it must actually be written to statusOut.
func TestSSHAttachLoop_StatusLinesWrittenOnRetry(t *testing.T) {
	if exitCmd(0) == nil {
		t.Skip("sh not on PATH")
	}
	calls := 0
	var out bytes.Buffer
	err := SSHAttachLoop(context.Background(), func() *exec.Cmd {
		calls++
		if calls == 1 {
			return exitCmd(255)
		}
		return exitCmd(0)
	}, &out, SSHAttachLoopOptions{
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("SSHAttachLoop() = %v; want nil", err)
	}
	if !strings.Contains(out.String(), "reconnecting") {
		t.Errorf("statusOut = %q; want a reconnecting message", out.String())
	}
}

// exitCmdWithStderr builds a *exec.Cmd that writes msg to stderr then
// exits with code. Used to simulate ssh's own diagnostic output for the
// isPermanentSSHFailure classification tests below.
func exitCmdWithStderr(code int, msg string) *exec.Cmd {
	if _, err := exec.LookPath("sh"); err != nil {
		return nil
	}
	return exec.Command("sh", "-c", "echo "+ShellSingleQuote(msg)+" >&2; exit "+strconv.Itoa(code))
}

// TestSSHAttachLoop_PermanentFailureStopsWithoutRetry: an exit-255
// whose stderr matches a known-permanent ssh failure (host key
// mismatch, auth rejected, DNS failure) must stop immediately — no
// retry, no sleep, no burying the diagnostic under "reconnecting..."
// noise. Regression test for the case a changed host key (a potential
// MITM warning) would otherwise get retried for up to ~7 minutes
// before finally surfacing.
func TestSSHAttachLoop_PermanentFailureStopsWithoutRetry(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
	}{
		{"permission_denied", "avi@tower: Permission denied (publickey)."},
		{"host_key_verification_failed", "Host key verification failed."},
		{"host_key_changed_warning", "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\nWARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!\n@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@"},
		{"dns_failure", "ssh: Could not resolve hostname totally-bogus-host.invalid: Name or service not known"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if exitCmdWithStderr(255, tc.stderr) == nil {
				t.Skip("sh not on PATH")
			}
			calls := 0
			slept := 0
			var out bytes.Buffer
			err := SSHAttachLoop(context.Background(), func() *exec.Cmd {
				calls++
				cmd := exitCmdWithStderr(255, tc.stderr)
				cmd.Stderr = &out // caller-wired passthrough, same as production
				return cmd
			}, &bytes.Buffer{}, SSHAttachLoopOptions{
				Sleep: func(time.Duration) { slept++ },
			})
			if err == nil {
				t.Fatal("SSHAttachLoop() = nil; want the permanent failure surfaced")
			}
			if calls != 1 {
				t.Errorf("calls = %d; want exactly 1 (a permanent failure must not retry)", calls)
			}
			if slept != 0 {
				t.Errorf("slept %d times; want 0 (a permanent failure must not retry)", slept)
			}
			if out.Len() == 0 {
				t.Error("caller-wired stderr writer got nothing; want the real ssh diagnostic still passed through live (SSHAttachLoop must tee, not replace, cmd.Stderr)")
			}
		})
	}
}

// TestSSHAttachLoop_TransientFailureStillRetries is the regression
// guard alongside the permanent-failure test above: a 255 exit whose
// stderr does NOT match a known-permanent pattern (e.g. a transient
// "Connection reset by peer") must still retry exactly as before this
// classification was added.
func TestSSHAttachLoop_TransientFailureStillRetries(t *testing.T) {
	if exitCmdWithStderr(255, "Connection reset by peer") == nil {
		t.Skip("sh not on PATH")
	}
	calls := 0
	slept := 0
	err := SSHAttachLoop(context.Background(), func() *exec.Cmd {
		calls++
		if calls == 1 {
			cmd := exitCmdWithStderr(255, "Connection reset by peer")
			cmd.Stderr = &bytes.Buffer{}
			return cmd
		}
		return exitCmd(0)
	}, &bytes.Buffer{}, SSHAttachLoopOptions{
		Sleep: func(time.Duration) { slept++ },
	})
	if err != nil {
		t.Fatalf("SSHAttachLoop() = %v; want nil (transient failure should have retried into success)", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d; want 2 (one transient failure + one success)", calls)
	}
	if slept != 1 {
		t.Errorf("slept %d times; want exactly 1", slept)
	}
}

// TestIsPermanentSSHFailure exercises the classifier directly.
func TestIsPermanentSSHFailure(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"permission_denied", "Permission denied (publickey).", true},
		{"host_key_verification_failed", "Host key verification failed.", true},
		{"host_identification_changed", "WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!", true},
		{"dns_failure", "Could not resolve hostname foo: Name or service not known", true},
		{"empty", "", false},
		{"transient_reset", "kex_exchange_identification: Connection reset by peer", false},
		{"generic_timeout", "ssh: connect to host tower port 22: Connection timed out", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermanentSSHFailure(tc.stderr); got != tc.want {
				t.Errorf("isPermanentSSHFailure(%q) = %v; want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

// TestSSHAttachLoop_CtxCancelledStopsBeforeSleeping: when the caller's
// context is already cancelled between a 255 (transport failure) and
// the next retry, the loop must return ctx.Err() immediately rather
// than sleeping and dialing again — otherwise a cancelled `canopy
// switch` (e.g. the TUI subprocess being torn down) would hang for a
// full backoff cycle before noticing.
func TestSSHAttachLoop_CtxCancelledStopsBeforeSleeping(t *testing.T) {
	if exitCmd(255) == nil {
		t.Skip("sh not on PATH")
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	slept := 0
	err := SSHAttachLoop(ctx, func() *exec.Cmd {
		calls++
		if calls == 1 {
			// Cancel right after the first (failing) attempt so the
			// ctx.Err() check — which runs after the max-attempts check
			// but before the sleep — is the branch that fires.
			cancel()
		}
		return exitCmd(255)
	}, &bytes.Buffer{}, SSHAttachLoopOptions{
		MaxAttempts: 5,
		Sleep:       func(time.Duration) { slept++ },
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("SSHAttachLoop() = %v; want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d; want exactly 1 (loop must stop on cancellation before redialing)", calls)
	}
	if slept != 0 {
		t.Errorf("slept %d times; want 0 (cancellation must be checked before the backoff sleep)", slept)
	}
}

// TestSSHAttachLoop_BackoffDoublesAndCaps verifies the actual backoff
// durations passed to Sleep grow exponentially from InitialBackoff and
// clamp at MaxBackoff — the retry-count assertions in the other tests
// only prove sleep was CALLED the right number of times, not that the
// delay itself follows the documented doubling/capping behavior.
func TestSSHAttachLoop_BackoffDoublesAndCaps(t *testing.T) {
	if exitCmd(255) == nil {
		t.Skip("sh not on PATH")
	}
	var got []time.Duration
	calls := 0
	err := SSHAttachLoop(context.Background(), func() *exec.Cmd {
		calls++
		return exitCmd(255)
	}, &bytes.Buffer{}, SSHAttachLoopOptions{
		MaxAttempts:    5,
		InitialBackoff: 1 * time.Second,
		MaxBackoff:     3 * time.Second,
		Sleep:          func(d time.Duration) { got = append(got, d) },
	})
	if err == nil {
		t.Fatal("SSHAttachLoop() = nil; want an error after exhausting MaxAttempts")
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("sleep durations = %v; want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("sleep[%d] = %v; want %v (1s -> 2s -> 4s capped to 3s -> stays 3s)", i, got[i], w)
		}
	}
}

// --- helpers ---

func indexOf(s []string, want string) int {
	for i, v := range s {
		if v == want {
			return i
		}
	}
	return -1
}

func mustContainPair(t *testing.T, args []string, key, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return
		}
	}
	t.Errorf("expected arg pair %q %q in args: %v", key, value, args)
}

func mustContainPrefix(t *testing.T, args []string, key, valuePrefix string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && strings.HasPrefix(args[i+1], valuePrefix) {
			return
		}
	}
	t.Errorf("expected arg pair %q (prefix %q) in args: %v", key, valuePrefix, args)
}
