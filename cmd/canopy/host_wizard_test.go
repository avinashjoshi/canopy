package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunSSHCopyID_RejectsDashPrefixedTarget is the regression test for
// the security gap found in runHostAddWizard's ssh-copy-id offer: the
// huh form only validated the SSH-target field as non-empty, so an
// option-shaped target (e.g. "--server=malicious-command") reached
// ssh-copy-id unguarded. Confirmed by PoC that ssh-copy-id forwards
// such a target to its OWN internal ssh invocation unprotected — a "--"
// separator on canopy's own call doesn't help the way it does for
// direct ssh/mosh calls, so validation has to happen here at the exec
// sink. Mirrors internal/ui/update_host.go's
// TestHandleConfirmSSHCopyIDKey_RejectsDashPrefixedTarget.
//
// Validation happens before exec.LookPath, so this doesn't require
// ssh-copy-id to be installed in the test environment.
func TestRunSSHCopyID_RejectsDashPrefixedTarget(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runSSHCopyID("--server=malicious-command", strings.NewReader(""), &out, &errOut)
	if err == nil {
		t.Fatal("runSSHCopyID: got nil error for dash-prefixed target; want rejection")
	}
	if !strings.Contains(err.Error(), "refusing to run ssh-copy-id") {
		t.Errorf("runSSHCopyID error = %q; want it to mention refusing to run ssh-copy-id", err.Error())
	}
}

// TestRunSSHCopyID_RejectsEmptyTarget covers the other ValidateSSHTarget
// branch reachable here (empty string), even though the wizard's own
// form already rejects an empty SSH-target field — defense in depth for
// any future caller of runSSHCopyID that skips the form.
func TestRunSSHCopyID_RejectsEmptyTarget(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runSSHCopyID("", strings.NewReader(""), &out, &errOut)
	if err == nil {
		t.Fatal("runSSHCopyID: got nil error for empty target; want rejection")
	}
}
