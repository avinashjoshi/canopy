package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestInstallShell_BashSnippet: explicit `bash` arg produces a wrapper
// with the lazygit-style env-var protocol, mktemp temp file, and the
// cd-then-rm cleanup. These are load-bearing for the protocol — if
// any of them drop, the wrapper silently breaks.
func TestInstallShell_BashSnippet(t *testing.T) {
	cmd := newInstallShellCmd()
	cmd.SetArgs([]string{"bash"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute bash: %v", err)
	}

	body := out.String()

	// Protocol invariants the canopy-side actionFocusProject reads:
	if !strings.Contains(body, "CANOPY_NEW_DIR_FILE") {
		t.Errorf("bash wrapper missing CANOPY_NEW_DIR_FILE env var:\n%s", body)
	}
	// mktemp ensures concurrent canopy invocations don't clash.
	if !strings.Contains(body, "mktemp") {
		t.Errorf("bash wrapper missing mktemp (concurrent-invocation safety):\n%s", body)
	}
	// `command canopy` so the wrapper doesn't recurse into itself.
	if !strings.Contains(body, "command canopy") {
		t.Errorf("bash wrapper missing 'command canopy' (would recurse):\n%s", body)
	}
	// Cleanup: rm -f the temp file.
	if !strings.Contains(body, "rm -f") {
		t.Errorf("bash wrapper missing temp-file cleanup:\n%s", body)
	}
}

// TestInstallShell_FishSnippet: fish syntax differs enough from POSIX
// that a separate template is needed — but the protocol invariants
// are the same.
func TestInstallShell_FishSnippet(t *testing.T) {
	cmd := newInstallShellCmd()
	cmd.SetArgs([]string{"fish"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute fish: %v", err)
	}

	body := out.String()
	if !strings.Contains(body, "CANOPY_NEW_DIR_FILE") {
		t.Errorf("fish wrapper missing CANOPY_NEW_DIR_FILE:\n%s", body)
	}
	if !strings.Contains(body, "function canopy") {
		t.Errorf("fish wrapper missing 'function canopy':\n%s", body)
	}
	if !strings.Contains(body, "set -l") {
		t.Errorf("fish wrapper missing local-var declaration:\n%s", body)
	}
}

// TestInstallShell_UnknownShellErrors: an unrecognized shell name
// returns a helpful error rather than silently producing the wrong
// wrapper.
func TestInstallShell_UnknownShellErrors(t *testing.T) {
	cmd := newInstallShellCmd()
	cmd.SetArgs([]string{"tcsh"}) // unsupported
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("Execute tcsh: want error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown shell") {
		t.Errorf("error should mention 'unknown shell': %v", err)
	}
}

// TestInstallShell_ZshSameAsBash: zsh and bash share the POSIX wrapper
// (the syntax is compatible). Verify both produce identical output —
// catches a future divergence that would silently break one.
func TestInstallShell_ZshSameAsBash(t *testing.T) {
	bashOut := runInstallShellCapture(t, "bash")
	zshOut := runInstallShellCapture(t, "zsh")
	if bashOut != zshOut {
		t.Errorf("bash and zsh wrappers diverged:\nbash:\n%s\n\nzsh:\n%s", bashOut, zshOut)
	}
}

func runInstallShellCapture(t *testing.T, shell string) string {
	t.Helper()
	cmd := newInstallShellCmd()
	cmd.SetArgs([]string{shell})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute %s: %v", shell, err)
	}
	return out.String()
}
