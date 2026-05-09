package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestRenameCmd_PinUnpinMutuallyExclusive: cobra rejects --pin and
// --unpin together. We exercise the flag-binding (not just the
// `MarkFlagsMutuallyExclusive` line) so a future refactor that
// accidentally drops the constraint surfaces immediately.
func TestRenameCmd_PinUnpinMutuallyExclusive(t *testing.T) {
	cmd := newRenameCmd()
	cmd.SetArgs([]string{"some-ws", "--pin", "--unpin"})

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("want error for --pin --unpin; got nil")
	}
	// Cobra's wording: "if any flags in the group [pin unpin] are set
	// none of the others can be". The flag group is the load-bearing
	// signal; assert on it rather than on a specific phrasing so a cobra
	// upgrade that polishes the message doesn't break this test.
	if !strings.Contains(err.Error(), "[pin unpin]") {
		t.Errorf("error did not name the [pin unpin] flag group: %v", err)
	}
}

// TestRenameCmd_PinFlagBinds: --pin is a recognized flag with a
// boolean default. A regression where the flag binding gets renamed or
// dropped would otherwise surface as a cryptic "unknown flag" error in
// production. Same shape for --unpin.
func TestRenameCmd_PinFlagBinds(t *testing.T) {
	for _, flag := range []string{"pin", "unpin"} {
		cmd := newRenameCmd()
		f := cmd.Flags().Lookup(flag)
		if f == nil {
			t.Fatalf("flag %q not registered", flag)
		}
		if f.DefValue != "false" {
			t.Errorf("flag %q default = %q; want false", flag, f.DefValue)
		}
	}
}

// TestResolveTargetWorkspace_ExplicitArg: with a positional arg, the
// resolver returns it directly and never consults tmux. Works equally
// from inside or outside a session — the simplest and most-used path.
func TestResolveTargetWorkspace_ExplicitArg(t *testing.T) {
	got, err := resolveTargetWorkspace(context.Background(), nil, []string{"my-ws"})
	if err != nil {
		t.Fatalf("explicit arg should not error: %v", err)
	}
	if got != "my-ws" {
		t.Errorf("got %q; want my-ws", got)
	}
}

// TestResolveTargetWorkspace_NoArgsOutsideTmux: no positional arg + nil
// manager produces an actionable error message that names the fix
// (`pass the workspace name explicitly`). The nil-manager path is also
// the first guard in resolveTargetWorkspace so this test doubles as a
// nil-safety regression check — it caught a panic in CI when the
// function refactor moved mgr.List above the tmux check.
func TestResolveTargetWorkspace_NoArgsOutsideTmux(t *testing.T) {
	_, err := resolveTargetWorkspace(context.Background(), nil, nil)
	if err == nil {
		t.Fatalf("want error for no-args-nil-manager; got nil")
	}
	if !strings.Contains(err.Error(), "pass the workspace name explicitly") {
		t.Errorf("error lost the friendly direction: %v", err)
	}
}
