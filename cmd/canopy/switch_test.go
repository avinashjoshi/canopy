package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/tmux"
)

// TestBuildRemoteSwitchCmd_MainDispatchesToCanopyMain: the TUI synthesizes
// "(main)" as the workspace name for project-main rows, but the remote
// has no workspace by that literal name — `canopy switch (main)` would
// hit ErrNotFound. Bridge that gap by routing (main) through `canopy
// main` instead (which uses EnsureMainSession + the project's port_base).
func TestBuildRemoteSwitchCmd_MainDispatchesToCanopyMain(t *testing.T) {
	got := buildRemoteSwitchCmd("/home/me/repo", "tower", "", false, true)
	if !strings.Contains(got, "exec canopy main") {
		t.Errorf("remote cmd missing `exec canopy main`; got: %q", got)
	}
	if strings.Contains(got, "exec canopy switch") {
		t.Errorf("remote cmd should not dispatch via canopy switch for (main); got: %q", got)
	}
	if !strings.Contains(got, "cd /home/me/repo") {
		t.Errorf("remote cmd missing cd to project root; got: %q", got)
	}
}

// TestBuildRemoteSwitchCmd_WorkspaceDispatchesToCanopySwitch covers the
// regular (non-main) path: a named workspace dispatches via `canopy
// switch <name>` on the remote, with the name shell-quoted.
func TestBuildRemoteSwitchCmd_WorkspaceDispatchesToCanopySwitch(t *testing.T) {
	got := buildRemoteSwitchCmd("/repo", "tower", "bold-tiger", false, false)
	if !strings.Contains(got, "exec canopy switch bold-tiger") {
		t.Errorf("expected `exec canopy switch bold-tiger`; got: %q", got)
	}
	if strings.Contains(got, "exec canopy main") {
		t.Errorf("named workspace should not route to canopy main; got: %q", got)
	}
}

// TestBuildRemoteSwitchCmd_LiteralMainWorkspaceNotRedirected: git accepts
// "(main)" as a branch name (`git check-ref-format --branch '(main)'`
// returns 0), so a workspace could legitimately be named "(main)". The
// dispatch must NOT silently redirect such a workspace to `canopy main`
// — the main flag, not the string match, carries the laptop-side intent.
func TestBuildRemoteSwitchCmd_LiteralMainWorkspaceNotRedirected(t *testing.T) {
	got := buildRemoteSwitchCmd("/repo", "tower", "(main)", false, false)
	if !strings.Contains(got, "exec canopy switch") {
		t.Errorf("workspace literally named (main) should dispatch via canopy switch when main=false; got: %q", got)
	}
	if strings.Contains(got, "exec canopy main") {
		t.Errorf("workspace literally named (main) was silently redirected to canopy main; got: %q", got)
	}
}

// TestBuildRemoteSwitchCmd_ShareExportsCanopyNoDetach: --share propagates
// to the remote shell via CANOPY_NO_DETACH=1, since mosh doesn't carry
// arbitrary env across the connection boundary. Without --share, the
// export line is absent.
func TestBuildRemoteSwitchCmd_ShareExportsCanopyNoDetach(t *testing.T) {
	with := buildRemoteSwitchCmd("/repo", "tower", "x", true, false)
	if !strings.Contains(with, "export CANOPY_NO_DETACH=1") {
		t.Errorf("share=true missing CANOPY_NO_DETACH export; got: %q", with)
	}
	without := buildRemoteSwitchCmd("/repo", "tower", "x", false, false)
	if strings.Contains(without, "CANOPY_NO_DETACH") {
		t.Errorf("share=false should not export CANOPY_NO_DETACH; got: %q", without)
	}
}

// TestBuildRemoteSwitchCmd_HostNameExportsCanopyRemoteHost: the registered
// host nickname rides along as CANOPY_REMOTE_HOST so the remote canopy's
// statusline pill renders "@<nickname>" instead of the fallback hostname.
// Empty hostName (raw target spec, e.g. `--on user@1.2.3.4`) skips the
// export so the remote statusline falls back to SSH_CONNECTION + Hostname.
func TestBuildRemoteSwitchCmd_HostNameExportsCanopyRemoteHost(t *testing.T) {
	with := buildRemoteSwitchCmd("/repo", "tower", "x", false, false)
	if !strings.Contains(with, "export CANOPY_REMOTE_HOST=tower") {
		t.Errorf("hostName=tower missing CANOPY_REMOTE_HOST export; got: %q", with)
	}
	without := buildRemoteSwitchCmd("/repo", "", "x", false, false)
	if strings.Contains(without, "CANOPY_REMOTE_HOST") {
		t.Errorf("hostName='' should not export CANOPY_REMOTE_HOST (raw target); got: %q", without)
	}
}

// TestBuildRemoteSwitchCmd_HostNameShellQuoted: a hostile nickname must
// be shell-quoted so it can't escape the export. shellQuote single-quote-
// wraps anything containing shell metacharacters.
func TestBuildRemoteSwitchCmd_HostNameShellQuoted(t *testing.T) {
	got := buildRemoteSwitchCmd("/repo", "evil; rm -rf /", "x", false, false)
	if !strings.Contains(got, "export CANOPY_REMOTE_HOST='evil; rm -rf /'") {
		t.Errorf("hostile hostName not shell-quoted; got: %q", got)
	}
}

// TestPropagateRemoteHostEnv exercises the three guard branches of the
// wrapper: env-unset early-return, empty-session early-return, and the
// happy path that delegates to tmux.SetSessionEnv. The integration
// (real tmux server, show-environment round-trip) is covered by
// internal/tmux/env_test.go; this test just locks in the wrapper's
// own conditional logic.
func TestPropagateRemoteHostEnv(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	const sock = "canopy-test-prop"
	c := tmux.WithSocket(sock)
	ctx := context.Background()
	t.Cleanup(func() { _ = c.KillServerAndReap(context.Background()) })

	t.Run("env_unset_clears_stale_tag", func(t *testing.T) {
		// Simulate the stale-tag scenario the adversarial review found:
		// a prior remote attach set CANOPY_REMOTE_HOST on the session.
		// A later local attach (no CANOPY_REMOTE_HOST in this process's
		// env) must clear the session tag so the statusline pill stops
		// rendering a falsely-remote signal.
		const session = "stale-tag-clear"
		if _, err := c.Create(ctx, session, t.TempDir(), ""); err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Pre-tag the session with a stale remote nickname.
		if err := c.SetSessionEnv(ctx, session, "CANOPY_REMOTE_HOST", "tower"); err != nil {
			t.Fatalf("pre-tag: %v", err)
		}
		// Now propagate with empty env (local-attach path).
		t.Setenv("CANOPY_REMOTE_HOST", "")
		propagateRemoteHostEnv(ctx, c, session)

		// After clearing, `tmux show-environment -t S CANOPY_REMOTE_HOST`
		// exits non-zero with "unknown variable" — the stale tag is gone.
		cmd := exec.Command("tmux", "-L", sock, "show-environment", "-t", session, "CANOPY_REMOTE_HOST")
		combined, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("env unset on local attach: tag NOT cleared, show-environment returned %q", string(combined))
		}
		if !strings.Contains(string(combined), "unknown variable") {
			t.Errorf("env unset on local attach: expected 'unknown variable', got %q", string(combined))
		}
	})

	t.Run("empty_session_is_noop", func(t *testing.T) {
		t.Setenv("CANOPY_REMOTE_HOST", "tower")
		// No session created. Empty session string must early-return
		// before reaching tmux. We can't easily observe the no-op; the
		// assertion is "doesn't panic and returns quickly."
		propagateRemoteHostEnv(ctx, c, "")
	})

	t.Run("happy_path_sets_env", func(t *testing.T) {
		t.Setenv("CANOPY_REMOTE_HOST", "tower")
		const session = "happy-session"
		if _, err := c.Create(ctx, session, t.TempDir(), ""); err != nil {
			t.Fatalf("Create: %v", err)
		}
		propagateRemoteHostEnv(ctx, c, session)
		out, err := exec.Command("tmux", "-L", sock, "show-environment", "-t", session, "CANOPY_REMOTE_HOST").Output()
		if err != nil {
			t.Fatalf("show-environment: %v", err)
		}
		want := "CANOPY_REMOTE_HOST=tower"
		if strings.TrimSpace(string(out)) != want {
			t.Errorf("happy path: got %q; want %q", strings.TrimSpace(string(out)), want)
		}
	})
}

func indexOf(s []string, want string) int {
	for i, v := range s {
		if v == want {
			return i
		}
	}
	return -1
}

// TestMoshExecArgv_TargetSeparator is the regression test for the live
// hand-tested bug: dispatchSwitchToRemote's actual syscall.Exec argv
// (built by moshExecArgv) previously had a redundant second "--" between
// target and the wrapped command, which mosh forwards verbatim as
// [command...] — mosh-server then tries to execvp a program literally
// named "--" ("mosh-server: execvp: --: No such file or directory").
// internal/host/ssh.go's MoshCmd got this exact regression test when the
// bug was fixed there; this is its counterpart for the real exec path
// real users hit, which previously had no test at all.
func TestMoshExecArgv_TargetSeparator(t *testing.T) {
	argv := moshExecArgv("avi@tower", "exec canopy switch oauth-fix")
	if argv[0] != "mosh" {
		t.Fatalf("argv[0] = %q, want \"mosh\"", argv[0])
	}

	targetIdx := indexOf(argv, "avi@tower")
	if targetIdx < 0 {
		t.Fatalf("target not found in argv: %v", argv)
	}
	if targetIdx == 0 || argv[targetIdx-1] != "--" {
		t.Fatalf("target at idx %d must be immediately preceded by \"--\"; argv: %v", targetIdx, argv)
	}

	// Command must follow target DIRECTLY — no second "--".
	want := []string{"bash", "-lc", "exec canopy switch oauth-fix"}
	got := argv[targetIdx+1:]
	if len(got) != len(want) {
		t.Fatalf("post-target argv: got %v, want %v (no separator between target and command)", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("post-target argv[%d] = %q, want %q", i, got[i], w)
		}
	}

	dashCount := 0
	for _, a := range argv {
		if a == "--" {
			dashCount++
		}
	}
	if dashCount != 1 {
		t.Errorf("found %d \"--\" tokens in argv; want exactly 1: %v", dashCount, argv)
	}
}

// TestMoshExecArgv_DashPrefixedTargetIsProtected mirrors
// TestMoshCmd_DashPrefixedTargetIsProtected: a target shaped like a mosh
// option must land strictly after the leading "--" so mosh's own parser
// can't mistake it for a flag.
func TestMoshExecArgv_DashPrefixedTargetIsProtected(t *testing.T) {
	evil := "--server=malicious-command"
	argv := moshExecArgv(evil, "exec canopy switch foo")

	leadingDashIdx := indexOf(argv, "--")
	if leadingDashIdx < 0 {
		t.Fatalf("no leading \"--\" found; dash-prefixed target would be parsed as a mosh option: %v", argv)
	}
	targetIdx := indexOf(argv, evil)
	if targetIdx < 0 {
		t.Fatalf("target %q not found verbatim in argv: %v", evil, argv)
	}
	if targetIdx != leadingDashIdx+1 {
		t.Errorf("target at idx %d must immediately follow the leading \"--\" at idx %d; argv: %v", targetIdx, leadingDashIdx, argv)
	}
}
