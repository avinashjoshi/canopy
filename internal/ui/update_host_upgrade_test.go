package ui

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestNewHostUpgradeSSHCmd_RunsLoginShell pins the load-bearing invariant
// that the remote shell is `bash -l`. Login shells source ~/.bash_profile
// / ~/.profile, which is where mise/asdf/etc inject the toolchain PATH.
// Without -l, non-interactive SSH inherits a bare default PATH and
// `make install` can't find `go` — the regression that motivated this
// helper (reported as `canopy upgrade on tower: make: go: No such file
// or directory`).
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
// and the `export PATH=…; exec canopy upgrade --yes` chain. This is
// the same pattern internal/host/refresh.go uses.
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
