package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/state"
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

// newTestManager builds a Manager backed by a fresh state.Store in a
// temp dir, suitable for read-side tests like rmEnrichNotFound. It
// intentionally skips workspace.New (which runs migration + tmux init)
// so tests stay hermetic and fast.
func newTestManager(t *testing.T, project, projectRoot string) *workspace.Manager {
	t.Helper()
	home := t.TempDir()
	store, err := state.NewStore(filepath.Join(home, ".canopy"))
	if err != nil {
		t.Fatalf("state.NewStore: %v", err)
	}
	return &workspace.Manager{
		Cfg:        &config.Config{Project: project, ProjectRoot: projectRoot},
		Store:      store,
		CanopyHome: filepath.Join(home, ".canopy"),
	}
}

// addWorkspace seeds a workspace row into the store. Tests use it to
// arrange the state.json side of the world before calling rmEnrichNotFound.
func addWorkspace(t *testing.T, mgr *workspace.Manager, projectRoot, name string) {
	t.Helper()
	err := mgr.Store.WithLock(func(s *state.State) error {
		return s.Add(state.Workspace{ProjectRoot: projectRoot, Name: name, Status: state.StatusReady})
	})
	if err != nil {
		t.Fatalf("seed workspace %s/%s: %v", projectRoot, name, err)
	}
}

// TestRmEnrichNotFound_ListsSiblings is the happy path: the named
// workspace doesn't exist, but two sibling workspaces do. The error
// should name them so the user spots a typo (or notices their TUI
// list was stale).
func TestRmEnrichNotFound_ListsSiblings(t *testing.T) {
	mgr := newTestManager(t, "canopy", "/home/u/Work/canopy")
	addWorkspace(t, mgr, "/home/u/Work/canopy", "alpha-fox")
	addWorkspace(t, mgr, "/home/u/Work/canopy", "bravo-jay")

	err := rmEnrichNotFound(context.Background(), mgr, "noble-lichen")
	msg := err.Error()

	if !strings.Contains(msg, `workspace "noble-lichen" not found in project "canopy"`) {
		t.Errorf("missing project-qualified target name in %q", msg)
	}
	if !strings.Contains(msg, "/home/u/Work/canopy") {
		t.Errorf("missing project root in %q", msg)
	}
	if !strings.Contains(msg, "alpha-fox") || !strings.Contains(msg, "bravo-jay") {
		t.Errorf("missing sibling workspace names in %q", msg)
	}
	if !strings.Contains(msg, "--force") {
		t.Errorf("missing --force hint in %q", msg)
	}
	if !strings.Contains(msg, "stale") {
		t.Errorf("missing stale-list hint in %q", msg)
	}
}

// TestRmEnrichNotFound_EmptyProject covers the "fresh project, nothing
// here to delete" case — the user should see an explicit "no
// workspaces are registered" line instead of a confusing "Workspaces
// here: " with nothing after the colon.
func TestRmEnrichNotFound_EmptyProject(t *testing.T) {
	mgr := newTestManager(t, "canopy", "/home/u/Work/canopy")

	err := rmEnrichNotFound(context.Background(), mgr, "noble-lichen")
	msg := err.Error()

	if !strings.Contains(msg, "No workspaces are registered") {
		t.Errorf("missing empty-project sentence in %q", msg)
	}
	if strings.Contains(msg, "Workspaces here:") {
		t.Errorf("should not list workspaces when there are none: %q", msg)
	}
}

// TestRmEnrichNotFound_CrossProjectHint covers the "user is in the
// wrong project root" case. A workspace with the requested name lives
// under a different project on this host. The error should surface
// the other project's root so the user can cd there.
func TestRmEnrichNotFound_CrossProjectHint(t *testing.T) {
	mgr := newTestManager(t, "canopy", "/home/u/Work/canopy")
	addWorkspace(t, mgr, "/home/u/Work/other", "noble-lichen")

	err := rmEnrichNotFound(context.Background(), mgr, "noble-lichen")
	msg := err.Error()

	if !strings.Contains(msg, "/home/u/Work/other") {
		t.Errorf("missing other-project root in %q", msg)
	}
	if !strings.Contains(msg, "registered under") {
		t.Errorf("missing cross-project hint phrasing in %q", msg)
	}
}

// TestRmEnrichNotFound_NoCrossProjectFalsePositive: when no other
// project has a workspace by this name, we must NOT emit the
// "registered under" line. Otherwise the user chases a phantom.
func TestRmEnrichNotFound_NoCrossProjectFalsePositive(t *testing.T) {
	mgr := newTestManager(t, "canopy", "/home/u/Work/canopy")
	addWorkspace(t, mgr, "/home/u/Work/canopy", "alpha-fox")
	addWorkspace(t, mgr, "/home/u/Work/other", "totally-different")

	err := rmEnrichNotFound(context.Background(), mgr, "noble-lichen")
	msg := err.Error()

	if strings.Contains(msg, "registered under") {
		t.Errorf("false-positive cross-project hint in %q", msg)
	}
}
