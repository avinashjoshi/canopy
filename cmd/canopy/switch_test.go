package main

import (
	"strings"
	"testing"
)

// TestBuildRemoteSwitchCmd_MainDispatchesToCanopyMain: the TUI synthesizes
// "(main)" as the workspace name for project-main rows, but the remote
// has no workspace by that literal name — `canopy switch (main)` would
// hit ErrNotFound. Bridge that gap by routing (main) through `canopy
// main` instead (which uses EnsureMainSession + the project's port_base).
func TestBuildRemoteSwitchCmd_MainDispatchesToCanopyMain(t *testing.T) {
	got := buildRemoteSwitchCmd("/home/me/repo", "", false, true)
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
	got := buildRemoteSwitchCmd("/repo", "bold-tiger", false, false)
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
	got := buildRemoteSwitchCmd("/repo", "(main)", false, false)
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
	with := buildRemoteSwitchCmd("/repo", "x", true, false)
	if !strings.Contains(with, "export CANOPY_NO_DETACH=1") {
		t.Errorf("share=true missing CANOPY_NO_DETACH export; got: %q", with)
	}
	without := buildRemoteSwitchCmd("/repo", "x", false, false)
	if strings.Contains(without, "CANOPY_NO_DETACH") {
		t.Errorf("share=false should not export CANOPY_NO_DETACH; got: %q", without)
	}
}
