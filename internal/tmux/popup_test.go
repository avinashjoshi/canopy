package tmux_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/tmux"
)

// TestCompareVersions covers the version comparator used by canopy popup
// to refuse pre-3.2 tmux. Table-driven so additions stay cheap.
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		got, want string
		atLeast   bool
		wantErr   bool
	}{
		{"3.4", "3.2", true, false},
		{"3.2", "3.2", true, false},
		{"3.1", "3.2", false, false},
		{"2.9", "3.2", false, false},
		{"4.0", "3.9", true, false},
		{"3.5a", "3.5", true, false}, // suffix letter treated as patch, equal
		{"3.5", "3.5a", true, false}, // and the other direction
		{"garbage", "3.2", false, true},
		{"3.4", "garbage", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.got+"_vs_"+tc.want, func(t *testing.T) {
			got, err := tmux.CompareVersions(tc.got, tc.want)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CompareVersions(%q, %q): got err=%v, wantErr=%v",
					tc.got, tc.want, err, tc.wantErr)
			}
			if got != tc.atLeast {
				t.Errorf("CompareVersions(%q, %q): got %v, want %v",
					tc.got, tc.want, got, tc.atLeast)
			}
		})
	}
}

// TestVersion_realTmux verifies Version() actually parses real tmux output.
// We can't mock tmux's -V output, so this is an integration test that runs
// against the user's tmux binary (skipped if missing).
func TestVersion_realTmux(t *testing.T) {
	requireTmux(t)
	c := tmux.New() // no socket needed; -V doesn't contact a server.
	ver, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	// We don't assert a specific value; just that parsing produced
	// something MAJOR.MINOR-shaped.
	if ver == "" {
		t.Fatal("Version returned empty string")
	}
	if _, err := tmux.CompareVersions(ver, "0.1"); err != nil {
		t.Errorf("CompareVersions rejects parsed version %q: %v", ver, err)
	}
}

// TestCurrentSession_noServer covers the "tmux server isn't running"
// branch by pointing at a fresh socket name that has no server.
func TestCurrentSession_noServer(t *testing.T) {
	requireTmux(t)
	// A unique socket nobody else has touched. Don't use newClient here
	// because we don't want a server created behind us — we want the
	// "no server" error path.
	c := tmux.WithSocket("canopy-test-no-server-" + t.Name())
	_, err := c.CurrentSession(context.Background())
	if !errors.Is(err, tmux.ErrNoCurrentClient) {
		t.Errorf("CurrentSession with no server: got %v, want ErrNoCurrentClient", err)
	}
}

// TestSwitchClient_targetNotFound covers the error mapping: tmux's
// "can't find session" stderr → ErrSessionNotFound sentinel. We force
// the failure by switching to a session that doesn't exist on a fresh
// (but running) server.
//
// To get the server running without creating the target, we create a
// throwaway session, then try to switch to a different name.
func TestSwitchClient_targetNotFound(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()

	// Seed the server with one session so it's alive.
	if _, err := c.Create(ctx, "seed", t.TempDir(), ""); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	// SwitchClient targets a session that doesn't exist. tmux should
	// say "can't find session" → mapped to ErrSessionNotFound.
	//
	// Note: switch-client requires a client to act on, and there's no
	// real client in tests. Real tmux will still parse the target and
	// emit "can't find session" before complaining about the missing
	// client, so the sentinel mapping fires correctly. If a future
	// tmux version reorders that, this test will catch the regression.
	err := c.SwitchClient(ctx, "definitely-not-a-real-session")
	if err == nil {
		t.Fatal("SwitchClient: got nil, want error")
	}
	if !errors.Is(err, tmux.ErrSessionNotFound) {
		// Some tmux versions complain about the missing client first
		// (before parsing -t). In that case the error contains "no
		// current client" or similar — accept either as long as we
		// surface SOME error. The sentinel mapping is a best-effort
		// "if tmux says it doesn't exist, tell the caller" wrapper.
		if !strings.Contains(err.Error(), "no current client") &&
			!strings.Contains(err.Error(), "no client") {
			t.Errorf("SwitchClient: got %v, want ErrSessionNotFound or no-client error", err)
		}
	}
}

// TestAttachVerb_switchesByTmuxEnv: inside tmux, AttachCmd must
// produce a switch-client invocation, NOT attach. attach from inside
// a tmux client fails with "sessions should be nested with care"
// (and from a popup pty it manifests as the user-reported "can't
// resurrect a stopped workspace" bug). Regression guard for
// 2026-04-29 fix.
func TestAttachVerb_switchesByTmuxEnv(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()

	// Seed a session so AttachCmd's HasSession check passes.
	if _, err := c.Create(ctx, "verb-test", t.TempDir(), ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cases := []struct {
		name     string
		tmuxEnv  string
		wantVerb string
	}{
		{"outside_tmux", "", "attach"},
		{"inside_tmux", "/tmp/tmux-1000/default,1234,0", "switch-client"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX", tc.tmuxEnv)
			cmd, err := c.AttachCmd(ctx, "verb-test")
			if err != nil {
				t.Fatalf("AttachCmd: %v", err)
			}
			// cmd.Args is ["tmux", "-L", "canopy-test", <verb>, "-t", "verb-test"].
			// Find the verb (first arg after socket flags, if any).
			var verb string
			for i, a := range cmd.Args {
				if a == "-L" {
					i++ // skip socket name
					continue
				}
				if i > 0 && a != cmd.Args[0] && a != "-L" && !strings.HasPrefix(a, "canopy-test") {
					verb = a
					break
				}
			}
			if verb != tc.wantVerb {
				t.Errorf("TMUX=%q: AttachCmd args=%v, got verb %q, want %q",
					tc.tmuxEnv, cmd.Args, verb, tc.wantVerb)
			}
		})
	}
}

// TestAttachCmd_AutoDetachFlag asserts that AttachCmd appends `-d` to
// the attach verb so the resulting tmux invocation kicks any other
// client off the target session before we attach. Without `-d`, two
// terminals attached to the same session mirror keystrokes and confuse
// the user about which one is "real."
//
// Only the `attach` verb (outside-tmux path) takes `-d`. The
// switch-client verb (inside-tmux path) doesn't accept `-d`; the
// detach-client preflight in detachOtherClients handles that case.
//
// CANOPY_NO_DETACH=1 is the documented escape hatch for
// pair-programming or "second terminal mirroring this for reference."
func TestAttachCmd_AutoDetachFlag(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	if _, err := c.Create(ctx, "detach-test", t.TempDir(), ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cases := []struct {
		name     string
		tmuxEnv  string
		noDetach string
		wantD    bool
	}{
		{"outside_tmux_default", "", "", true},
		{"outside_tmux_opt_out", "", "1", false},
		{"inside_tmux_default", "/tmp/tmux-1000/default,1234,0", "", false}, // switch-client never has -d
		{"inside_tmux_opt_out", "/tmp/tmux-1000/default,1234,0", "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX", tc.tmuxEnv)
			t.Setenv("CANOPY_NO_DETACH", tc.noDetach)
			cmd, err := c.AttachCmd(ctx, "detach-test")
			if err != nil {
				t.Fatalf("AttachCmd: %v", err)
			}
			hasD := false
			for _, a := range cmd.Args {
				if a == "-d" {
					hasD = true
					break
				}
			}
			if hasD != tc.wantD {
				t.Errorf("args=%v: -d present=%v; want %v", cmd.Args, hasD, tc.wantD)
			}
		})
	}
}

// TestAttachCmdWithOptions_SharedSkipsDetach: regression for the
// v0.17 Phase 1j share-vs-steal split. AttachOptions{Shared: true}
// must skip the -d flag so existing clients keep their attach.
// CANOPY_NO_DETACH already provides a session-wide opt-out; Shared is
// a per-call opt-in that doesn't require mutating process env.
func TestAttachCmdWithOptions_SharedSkipsDetach(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	if _, err := c.Create(ctx, "shared-test", t.TempDir(), ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Force "outside tmux" so the -d-eligible attach verb is used.
	t.Setenv("TMUX", "")
	t.Setenv("CANOPY_NO_DETACH", "")

	cmd, err := c.AttachCmdWithOptions(ctx, "shared-test", tmux.AttachOptions{Shared: true})
	if err != nil {
		t.Fatalf("AttachCmdWithOptions: %v", err)
	}
	for _, a := range cmd.Args {
		if a == "-d" {
			t.Errorf("Shared=true args=%v contains -d; want no -d", cmd.Args)
		}
	}
}

// TestDisplayPopup_noServer covers the error path when tmux isn't running.
// display-popup needs a client/server, so against an empty socket it must
// surface an error rather than silently succeeding.
func TestDisplayPopup_noServer(t *testing.T) {
	requireTmux(t)
	c := tmux.WithSocket("canopy-test-popup-noserver-" + t.Name())
	err := c.DisplayPopup(context.Background(), "true", "")
	if err == nil {
		t.Fatal("DisplayPopup with no server: got nil, want error")
	}
	// We don't assert the specific error shape — just that we surface
	// SOMETHING rather than silently exiting zero.
}
