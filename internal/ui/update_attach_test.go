package ui

import (
	"strings"
	"testing"
)

// TestRemoteAttachArgs_Basics covers the ordinary named-workspace
// dispatch shape: `switch --on <host> <name>`, with no --remote-cwd,
// --main, --share, or --no-mosh when none of those apply.
func TestRemoteAttachArgs_Basics(t *testing.T) {
	row := Row{Host: "tower", Name: "oauth-fix"}
	got, err := remoteAttachArgs(row, "", false, false)
	if err != nil {
		t.Fatalf("remoteAttachArgs: %v", err)
	}
	want := []string{"switch", "--on", "tower", "oauth-fix"}
	if !equalStrSlices(got, want) {
		t.Errorf("args = %v; want %v", got, want)
	}
}

// TestRemoteAttachArgs_MainRowRequiresCwd: an IsMain row with no
// resolvable cwd must error rather than silently dispatch to the
// wrong project's main session — see remoteAttachArgs's doc comment.
func TestRemoteAttachArgs_MainRowRequiresCwd(t *testing.T) {
	row := Row{Host: "tower", Project: "myproj", IsMain: true}
	_, err := remoteAttachArgs(row, "", false, false)
	if err == nil {
		t.Fatal("remoteAttachArgs: nil error; want one for an IsMain row with no cwd")
	}
	if !strings.Contains(err.Error(), "--on tower") {
		t.Errorf("error missing actionable hint: %q", err.Error())
	}
}

// TestRemoteAttachArgs_MainRowWithCwd: an IsMain row WITH a resolved
// cwd dispatches via --main (not a workspace name), with --remote-cwd
// set.
func TestRemoteAttachArgs_MainRowWithCwd(t *testing.T) {
	row := Row{Host: "tower", Project: "myproj", IsMain: true}
	got, err := remoteAttachArgs(row, "/home/avi/myproj", false, false)
	if err != nil {
		t.Fatalf("remoteAttachArgs: %v", err)
	}
	want := []string{"switch", "--on", "tower", "--remote-cwd", "/home/avi/myproj", "--main"}
	if !equalStrSlices(got, want) {
		t.Errorf("args = %v; want %v", got, want)
	}
}

// TestRemoteAttachArgs_ShareAppendsFlag: shared=true (the user
// confirmed sharing an already-attached session) appends --share.
func TestRemoteAttachArgs_ShareAppendsFlag(t *testing.T) {
	row := Row{Host: "tower", Name: "oauth-fix"}
	got, err := remoteAttachArgs(row, "", true, false)
	if err != nil {
		t.Fatalf("remoteAttachArgs: %v", err)
	}
	if !containsStr(got, "--share") {
		t.Errorf("args = %v; want --share present", got)
	}
}

// TestRemoteAttachArgs_NoMoshAppendsFlag is the v0.22.x regression
// test: `canopy --remote <host> --no-mosh` (m.pinnedNoMosh) must
// propagate --no-mosh onto every attach dispatched from that pinned
// session, so the subprocess's chooseAttachMode (cmd/canopy/switch.go)
// picks the ssh-reconnect-loop instead of mosh.
func TestRemoteAttachArgs_NoMoshAppendsFlag(t *testing.T) {
	row := Row{Host: "tower", Name: "oauth-fix"}

	without, err := remoteAttachArgs(row, "", false, false)
	if err != nil {
		t.Fatalf("remoteAttachArgs: %v", err)
	}
	if containsStr(without, "--no-mosh") {
		t.Errorf("args = %v; --no-mosh must be absent when noMosh=false", without)
	}

	with, err := remoteAttachArgs(row, "", false, true)
	if err != nil {
		t.Fatalf("remoteAttachArgs: %v", err)
	}
	if !containsStr(with, "--no-mosh") {
		t.Errorf("args = %v; want --no-mosh present when noMosh=true", with)
	}
}

// TestRemoteAttachArgs_AllFlagsCombine: --remote-cwd, --share, and
// --no-mosh must all coexist correctly on a named (non-main) row.
func TestRemoteAttachArgs_AllFlagsCombine(t *testing.T) {
	row := Row{Host: "tower", Name: "oauth-fix"}
	got, err := remoteAttachArgs(row, "/home/avi/myproj", true, true)
	if err != nil {
		t.Fatalf("remoteAttachArgs: %v", err)
	}
	want := []string{"switch", "--on", "tower", "--remote-cwd", "/home/avi/myproj", "oauth-fix", "--share", "--no-mosh"}
	if !equalStrSlices(got, want) {
		t.Errorf("args = %v; want %v", got, want)
	}
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
