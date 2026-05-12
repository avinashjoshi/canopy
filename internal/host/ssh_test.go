package host

import (
	"context"
	"errors"
	"strings"
	"testing"
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

// TestMoshCmd_TargetSeparator verifies the mosh syntax `mosh <target> --
// <cmd...>` is constructed correctly. The `--` separator is required by
// mosh to disambiguate ssh-target from the command to run.
func TestMoshCmd_TargetSeparator(t *testing.T) {
	cmd := MoshCmd(context.Background(), "avi@tower", "canopy", "switch", "oauth-fix")
	args := cmd.Args
	if args[0] != "mosh" {
		t.Fatalf("args[0] = %q, want \"mosh\"", args[0])
	}

	dashIdx := indexOf(args, "--")
	targetIdx := indexOf(args, "avi@tower")
	if targetIdx < 0 {
		t.Fatalf("target not found in args: %v", args)
	}
	if dashIdx < 0 {
		t.Fatalf("`--` separator not found in args: %v", args)
	}
	if dashIdx <= targetIdx {
		t.Errorf("`--` at idx %d should come after target at idx %d", dashIdx, targetIdx)
	}

	// Args after `--` should be the remote command, in order.
	want := []string{"canopy", "switch", "oauth-fix"}
	got := args[dashIdx+1:]
	if len(got) != len(want) {
		t.Fatalf("post-`--` args: got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("post-`--` arg[%d] = %q, want %q", i, got[i], w)
		}
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

// TestCanopyHome_NonEmpty ensures the helper never returns an empty
// string (which would produce an invalid ControlPath like `ssh-%C.sock`
// in the cwd).
func TestCanopyHome_NonEmpty(t *testing.T) {
	got := canopyHome()
	if got == "" {
		t.Fatal("canopyHome() returned empty string")
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
