package main

import (
	"strings"
	"testing"
)

// Source-spec validation + parsing now live on workspace.SourceSpec.
// See internal/workspace/source_test.go for the parser/validator
// coverage. CLI-only tests live here.

// TestBuildRemoteScript_PathPrecheck_AbsentWhenNoRemoteCwd: when no
// remoteCwd is set (raw ssh-target with no project path), the script
// must skip the dir-existence pre-check entirely — there's nothing to
// check, and emitting an empty `[ ! -d "" ]` test would always fire
// the missing-path branch incorrectly.
func TestBuildRemoteScript_PathPrecheck_AbsentWhenNoRemoteCwd(t *testing.T) {
	got := buildRemoteScript("", []string{"canopy", "new", "--no-attach"}, "")
	if strings.Contains(got, "exit 7") {
		t.Errorf("empty remoteCwd should not emit the missing-path pre-check; got:\n%s", got)
	}
	if strings.Contains(got, "[ ! -d ") {
		t.Errorf("empty remoteCwd should not emit a `test -d` check; got:\n%s", got)
	}
}

// TestBuildRemoteScript_PathPrecheck_PresentWhenRemoteCwd: when a
// remoteCwd is set (the common --on <registered-host> path), the
// script must guard the cd with a `test -d` that exits 7 on miss.
// Exit 7 is the sentinel dispatchNewToRemote keys off for the
// rewrapped "remote project path does not exist" error.
func TestBuildRemoteScript_PathPrecheck_PresentWhenRemoteCwd(t *testing.T) {
	got := buildRemoteScript("/home/jarvis/Work/brain", []string{"canopy", "new", "--no-attach"}, "")
	if !strings.Contains(got, "exit 7") {
		t.Errorf("non-empty remoteCwd must emit exit-7 sentinel on missing path; got:\n%s", got)
	}
	if !strings.Contains(got, "/home/jarvis/Work/brain") {
		t.Errorf("script must reference the remoteCwd in the pre-check; got:\n%s", got)
	}
	// Pre-check must precede the cd, not follow it — otherwise cd
	// fails first under `set -e` and the sentinel never runs.
	checkIdx := strings.Index(got, "exit 7")
	cdIdx := strings.Index(got, "cd /home/jarvis/Work/brain")
	if checkIdx == -1 || cdIdx == -1 || checkIdx > cdIdx {
		t.Errorf("dir-existence check must precede cd; checkIdx=%d cdIdx=%d:\n%s",
			checkIdx, cdIdx, got)
	}
}

// TestRemotePathMissingErr_IncludesRemediationWhenHostRegistered:
// when the host was reached via a registry name, the formatted error
// must include a copy-pasteable `canopy project add ... --on <host>`
// hint so the user can fix the registration without digging through
// docs. Without a registered host name (raw ssh-target dispatch),
// the hint is omitted — there's no canonical name to use in the
// remediation command.
func TestRemotePathMissingErr_IncludesRemediationWhenHostRegistered(t *testing.T) {
	err := remotePathMissingErr("pi", "/home/avi/Work/brain", "pi")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "/home/avi/Work/brain") {
		t.Errorf("error must name the offending path: %q", msg)
	}
	if !strings.Contains(msg, "canopy project add") {
		t.Errorf("error with hostName must include remediation hint: %q", msg)
	}
	if !strings.Contains(msg, "--on pi") {
		t.Errorf("remediation must reference the host name: %q", msg)
	}
}

// TestRemotePathMissingErr_NoRemediationForRawTarget: a raw ssh-target
// dispatch (no registry name) has no canonical "name" to put in the
// `--on` flag of the remediation hint, so we omit it rather than
// fabricate something the user has to substitute. The path is still
// surfaced.
func TestRemotePathMissingErr_NoRemediationForRawTarget(t *testing.T) {
	err := remotePathMissingErr("jarvis@pi.ts.net", "/nope", "")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "/nope") {
		t.Errorf("error must name the path: %q", msg)
	}
	if strings.Contains(msg, "canopy project add") {
		t.Errorf("raw ssh-target should not emit project-add hint (no canonical host name): %q", msg)
	}
}
