package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/workspace"
)

// TestRmHandleFindErr_ForceSwallowsNotFound is the regression test for
// the "TUI force-delete on a stale remote row prints a scary error"
// bug. The local TUI dispatches `canopy rm <name> --yes --force` for
// remote rows; when the remote canopy has already lost the workspace
// (manual rm on the remote since the last host refresh), the remote's
// `mgr.Find` returns ErrWorkspaceNotFound. Pre-fix, that bubbled up
// through the SSH dispatch as `remote canopy rm failed: exit status 1`,
// even though the user's intent ("make it gone") was already satisfied.
//
// With --force, mirror `rm -f` semantics: not-found is success.
func TestRmHandleFindErr_ForceSwallowsNotFound(t *testing.T) {
	var out bytes.Buffer
	notFound := fmt.Errorf("workspace.Find(early-badger): %w", workspace.ErrWorkspaceNotFound)
	handled, err := rmHandleFindErr(notFound, true, "early-badger", &out)
	if !handled {
		t.Fatal("handled = false; want true (force + not-found should short-circuit)")
	}
	if err != nil {
		t.Errorf("err = %v; want nil (force + not-found = success)", err)
	}
	if !strings.Contains(out.String(), "already removed") {
		t.Errorf("out = %q; want informational message containing \"already removed\"", out.String())
	}
	if !strings.Contains(out.String(), "early-badger") {
		t.Errorf("out = %q; want workspace name in message", out.String())
	}
}

// TestRmHandleFindErr_StrictReturnsNotFound: without --force, not-found
// still surfaces. This is the safety net for typos in interactive CLI
// use ("canopy rm fixx" instead of "fix") — the user wants to know
// they didn't actually delete anything.
func TestRmHandleFindErr_StrictReturnsNotFound(t *testing.T) {
	var out bytes.Buffer
	notFound := fmt.Errorf("workspace.Find(fixx): %w", workspace.ErrWorkspaceNotFound)
	handled, _ := rmHandleFindErr(notFound, false, "fixx", &out)
	if handled {
		t.Error("handled = true; want false (no --force should bubble the error)")
	}
	if out.Len() != 0 {
		t.Errorf("out = %q; want empty (strict mode prints nothing)", out.String())
	}
}

// TestRmHandleFindErr_ForceOnOtherErrors: --force only special-cases
// not-found. Other errors from Find (e.g. state-store I/O failure)
// still bubble up so the user sees the real problem instead of a
// misleading "already removed" message.
func TestRmHandleFindErr_ForceOnOtherErrors(t *testing.T) {
	var out bytes.Buffer
	otherErr := errors.New("state.json: permission denied")
	handled, _ := rmHandleFindErr(otherErr, true, "anything", &out)
	if handled {
		t.Error("handled = true; want false (force should NOT swallow non-not-found errors)")
	}
	if out.Len() != 0 {
		t.Errorf("out = %q; want empty (no message for non-not-found)", out.String())
	}
}
