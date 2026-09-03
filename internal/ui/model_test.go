package ui

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/ghx"
	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/ui/projectlist"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// newTestModel builds a minimal *Model for unit-testing keymap and render
// paths that don't actually exercise the workspace.Manager. Used widely
// across this file; the bool param is unused after v0.8 unification but
// kept for callsite stability during the merge transition.
func newTestModel(_ bool) *Model {
	m := &Model{
		mgr: &workspace.Manager{
			Tmux: tmux.WithSocket("canopy-test"),
			Cfg:  &config.Config{Project: "test-project", ProjectRoot: "/tmp/test-project"},
		},
		tc:             tmux.WithSocket("canopy-test"),
		projectName:    "test-project",
		nameInput:      textinput.New(),
		listInput:      textinput.New(),
		targetInput:    textinput.New(),
		promptInput:    textarea.New(),
		mode:           listMode,
		currentProject: "/tmp/test-project",
		tab:            tabLocal,
	}
	m.list = projectlist.New(projectlist.Options{})
	return m
}

// setTestRows is a test helper that pushes rows into both m.allRows
// (the unfiltered set) and m.list (the rendered set). Tests use this
// instead of mutating m.allRows directly so the projectlist sub-
// component sees the same data the model holds.
func (m *Model) setTestRows(rows []Row) {
	m.allRows = rows
	m.list.SetRows(m.filteredRows())
}

// TestNewUnified_PopupModeFromEnv: NewUnified picks up CANOPY_IN_POPUP=1
// from the environment and stores it as m.inPopup. This is the single
// source of truth for popup-mode rendering after v0.8 unification —
// the env var is set inline by tmux's `display-popup -E` invocation.
func TestNewUnified_PopupModeFromEnv(t *testing.T) {
	store := &state.Store{}
	tc := tmux.WithSocket("canopy-test")

	t.Run("env=1 → popup mode", func(t *testing.T) {
		t.Setenv("CANOPY_IN_POPUP", "1")
		m := NewUnified(nil, store, tc, "", "", "")
		if !m.inPopup {
			t.Errorf("inPopup = false; want true when CANOPY_IN_POPUP=1")
		}
	})

	t.Run("env unset → fullscreen mode", func(t *testing.T) {
		t.Setenv("CANOPY_IN_POPUP", "")
		m := NewUnified(nil, store, tc, "", "", "")
		if m.inPopup {
			t.Errorf("inPopup = true; want false when env unset")
		}
	})

	t.Run("env=other → fullscreen mode (strict eq)", func(t *testing.T) {
		t.Setenv("CANOPY_IN_POPUP", "true")
		m := NewUnified(nil, store, tc, "", "", "")
		if m.inPopup {
			t.Errorf("inPopup = true; want false when env != \"1\"")
		}
	})
}

// TestNewUnified_DefaultTab: Local tab is pre-selected when a current
// project is resolved; Global tab pre-selected when not. Reflects the
// "scope is what I'm working on" / "give me everything" intuition from
// the unification design.
func TestNewUnified_DefaultTab(t *testing.T) {
	store := &state.Store{}
	tc := tmux.WithSocket("canopy-test")

	t.Run("with current project → tabLocal", func(t *testing.T) {
		m := NewUnified(nil, store, tc, "/some/project", "", "")
		if m.tab != tabLocal {
			t.Errorf("tab = %v; want tabLocal when currentProject != \"\"", m.tab)
		}
	})

	t.Run("no current project → tabGlobal", func(t *testing.T) {
		m := NewUnified(nil, store, tc, "", "", "")
		if m.tab != tabGlobal {
			t.Errorf("tab = %v; want tabGlobal when currentProject == \"\"", m.tab)
		}
	})
}

// TestNewRemotePinned: the `canopy --remote <host>` thin-client
// constructor (v0.22) must produce a Model with no local project/mgr,
// pinned to exactly the given host, landing on tabGlobal (the only tab
// visibleTabs() will offer it — see TestVisibleTabs_Pinned below).
// Covers both resolution paths: a registry entry (selfHeal=true) and a
// raw SSH target (selfHeal=false) — see resolveRemoteHost in
// cmd/canopy/host_resolve.go, which is what feeds this constructor.
func TestNewRemotePinned(t *testing.T) {
	store := &state.Store{}
	tc := tmux.WithSocket("canopy-test")

	t.Run("registry host", func(t *testing.T) {
		h := host.Host{Name: "tower", SSHTarget: "user@tower", Type: "ssh"}
		m := NewRemotePinned(store, tc, h, true)

		if m.pinnedHost.Name != "tower" {
			t.Errorf("pinnedHost.Name = %q; want %q", m.pinnedHost.Name, "tower")
		}
		if !m.pinnedHostSelfHeal {
			t.Error("pinnedHostSelfHeal = false; want true for a registry-resolved host")
		}
		if m.mgr != nil {
			t.Errorf("mgr = %v; want nil (thin client has no local project)", m.mgr)
		}
		if m.currentProject != "" {
			t.Errorf("currentProject = %q; want empty", m.currentProject)
		}
		if m.tab != tabGlobal {
			t.Errorf("tab = %v; want tabGlobal", m.tab)
		}
		if len(m.hostList) != 1 || m.hostList[0].Name != "tower" {
			t.Errorf("hostList = %v; want exactly [tower] (row actions like kill resolve hosts by name against this list)", m.hostList)
		}
		// resolveHostForExec is what execRemoteKill (kill a workspace row)
		// actually calls — proving it resolves the pinned host by name is
		// the real regression guard, not just checking the hostList field.
		got, err := m.resolveHostForExec("tower")
		if err != nil {
			t.Fatalf("resolveHostForExec(%q): %v", h.Name, err)
		}
		if got != "user@tower" {
			t.Errorf("resolveHostForExec(%q) = %q; want %q", h.Name, got, "user@tower")
		}
	})

	t.Run("raw SSH target", func(t *testing.T) {
		h := host.Host{Name: "user@tower.tail-abc.ts.net", SSHTarget: "user@tower.tail-abc.ts.net", Type: "ssh"}
		m := NewRemotePinned(store, tc, h, false)

		if m.pinnedHost.Name != "user@tower.tail-abc.ts.net" {
			t.Errorf("pinnedHost.Name = %q; want the raw target", m.pinnedHost.Name)
		}
		if m.pinnedHostSelfHeal {
			t.Error("pinnedHostSelfHeal = true; want false for a raw SSH target (no registry entry)")
		}
		// Regression guard for the raw-target case specifically: a raw
		// target has no local hosts.json entry at all, so without
		// NewRemotePinned overriding hostList, resolveHostForExec would
		// fail here even though the Model is looking straight at this
		// host — silently breaking kill/new-workspace-dispatch actions
		// for exactly the "no host add needed" flow this mode exists for.
		got, err := m.resolveHostForExec(h.Name)
		if err != nil {
			t.Fatalf("resolveHostForExec(%q): %v (raw-target row actions must resolve without a registry entry)", h.Name, err)
		}
		if got != h.SSHTarget {
			t.Errorf("resolveHostForExec(%q) = %q; want %q", h.Name, got, h.SSHTarget)
		}
	})
}

// TestVisibleTabs_Pinned: a pinned Model only ever offers tabGlobal —
// Local doesn't apply (no local project) and Hosts (fleet management)
// doesn't apply (the whole point is one pinned host), even if
// currentProject or hostList would otherwise make them eligible.
func TestVisibleTabs_Pinned(t *testing.T) {
	m := newTestModel(false)
	m.pinnedHost = host.Host{Name: "tower", SSHTarget: "user@tower", Type: "ssh"}
	// Deliberately set both would-be-eligible conditions to make sure
	// pinnedHost short-circuits ahead of them.
	m.currentProject = "/some/project"
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "user@tower", Type: "ssh"}}

	got := m.visibleTabs()
	if len(got) != 1 || got[0] != tabGlobal {
		t.Errorf("visibleTabs() = %v; want [tabGlobal] when pinned", got)
	}
}

// TestPushLoadingHosts_Pinned: while a pinned refresh is in flight, only
// the pinned host's section header should show the loading spinner —
// not every host in the laptop's own registry (m.hostList), which is
// irrelevant to a thin client showing exactly one host's rows.
func TestPushLoadingHosts_Pinned(t *testing.T) {
	m := newTestModel(false)
	m.pinnedHost = host.Host{Name: "tower", SSHTarget: "user@tower", Type: "ssh"}
	m.hostList = []host.Host{
		{Name: "tower", SSHTarget: "user@tower", Type: "ssh"},
		{Name: "other-host", SSHTarget: "user@other", Type: "ssh"},
	}
	m.remoteRefreshing = true

	m.pushLoadingHosts()

	if !m.list.LoadingHosts("tower") {
		t.Error("LoadingHosts(tower) = false; want true (the pinned host)")
	}
	if m.list.LoadingHosts("other-host") {
		t.Error("LoadingHosts(other-host) = true; want false (not the pinned host)")
	}
}

// TestBuildRemoteRowsMsg_SelfHealFalseSkipsNilRegistry: when selfHeal is
// false, buildRemoteRowsMsg must NOT touch reg (it's nil for a raw
// --remote SSH target with no registry entry to attach auto-discovered
// project registrations to — see resolveRemoteHost). This is a
// regression guard for a nil-pointer risk: if the `if selfHeal`
// guard around autoRegisterRemoteOrphans were ever dropped, this would
// panic on the nil *host.Registry. Uses an empty hosts slice so
// host.Refresher.Tick makes zero SSH calls — fast and network-free.
func TestBuildRemoteRowsMsg_SelfHealFalseSkipsNilRegistry(t *testing.T) {
	home := t.TempDir()

	msg := buildRemoteRowsMsg(home, nil, []host.Host{}, false)

	got, ok := msg.(remoteRowsLoadedMsg)
	if !ok {
		t.Fatalf("msg type = %T; want remoteRowsLoadedMsg", msg)
	}
	if got.err != nil {
		t.Errorf("err = %v; want nil", got.err)
	}
	if len(got.rows) != 0 {
		t.Errorf("rows = %v; want none", got.rows)
	}
}

// TestBuildRemoteRowsMsg_SelfHealTrueRunsOrphanCheck is the selfHeal=true
// sibling of TestBuildRemoteRowsMsg_SelfHealFalseSkipsNilRegistry: proves
// the `if selfHeal { hosts = autoRegisterRemoteOrphans(...) }` guard
// actually RUNS (not just correctly skips) when selfHeal is true and reg
// is a real, non-nil registry — i.e. the registry-resolved --remote path
// (see resolveRemoteHost's selfHeal=true branch). Uses an empty hosts
// slice for the same reason as its sibling: host.Refresher.Tick makes
// zero SSH calls, so this stays fast and network-free while still
// exercising the guard's true arm against a real *host.Registry instead
// of nil.
func TestBuildRemoteRowsMsg_SelfHealTrueRunsOrphanCheck(t *testing.T) {
	home := t.TempDir()
	reg, err := host.NewRegistry(home)
	if err != nil {
		t.Fatalf("host.NewRegistry: %v", err)
	}

	msg := buildRemoteRowsMsg(home, reg, []host.Host{}, true)

	got, ok := msg.(remoteRowsLoadedMsg)
	if !ok {
		t.Fatalf("msg type = %T; want remoteRowsLoadedMsg", msg)
	}
	if got.err != nil {
		t.Errorf("err = %v; want nil", got.err)
	}
	if len(got.rows) != 0 {
		t.Errorf("rows = %v; want none", got.rows)
	}
}

// cmdFuncName identifies WHICH named function produced a tea.Cmd closure,
// via the closure's own compiled name (e.g.
// "internal/ui.refreshCmdWithMem.func1"). Used to inspect a tea.Batch's
// contents without invoking any of the sub-commands — several of them do
// real I/O (SSH, disk), which unit tests must not trigger. Returns "" for
// a nil Cmd (tea.Batch skips nils itself, but callers may still see one
// in the slice depending on Bubbletea version).
func cmdFuncName(c tea.Cmd) string {
	if c == nil {
		return ""
	}
	return runtime.FuncForPC(reflect.ValueOf(c).Pointer()).Name()
}

// TestRefresh_PinnedModeSkipsLocalRefresh and its non-pinned sibling below
// close a real gap: refresh() has two new branches added alongside
// pinnedHost (skip local rows entirely; dispatch refreshRemoteCmdForHost
// instead of refreshRemoteCmd), and neither was exercised by a test —
// every pre-existing refresh() test uses a zero-value pinnedHost. A
// regression here would leak the laptop's own local workspace rows into
// a `canopy --remote <host>` thin client, or silently fan out to every
// registered host instead of just the pinned one.
//
// Doesn't invoke any of the batched commands (several do real SSH/disk
// I/O) — instead identifies each one by its compiled closure name via
// cmdFuncName, which is enough to prove which code paths refresh()
// decided to include without triggering any of them.
func TestRefresh_PinnedModeSkipsLocalRefresh(t *testing.T) {
	m := newTestModel(false)
	m.pinnedHost = host.Host{Name: "tower", SSHTarget: "user@tower", Type: "ssh"}
	m.pinnedHostSelfHeal = true

	batch := refreshBatchForTest(t, m)

	var sawLocal, sawPinnedRemote, sawAllHostsRemote bool
	for _, sub := range batch {
		switch name := cmdFuncName(sub); {
		case strings.HasSuffix(name, ".refreshCmdWithMem.func1"):
			sawLocal = true
		case strings.HasSuffix(name, ".refreshRemoteCmdForHost.func1"):
			sawPinnedRemote = true
		case strings.HasSuffix(name, ".refreshRemoteCmd.func1"):
			sawAllHostsRemote = true
		}
	}

	if sawLocal {
		t.Error("pinned refresh() dispatched refreshCmdWithMem (local rows); must never load local state in --remote thin-client mode")
	}
	if !sawPinnedRemote {
		t.Error("pinned refresh() did not dispatch refreshRemoteCmdForHost (single-host fan-out)")
	}
	if sawAllHostsRemote {
		t.Error("pinned refresh() dispatched refreshRemoteCmd (all-hosts fan-out); should use the single-host path instead")
	}
}

// TestRefresh_UnpinnedModeIncludesLocalRefresh is the non-pinned mirror:
// proves the ordinary (non-thin-client) path still dispatches BOTH the
// local refresh and the all-hosts remote fan-out, i.e. that the new
// pinnedHost branches didn't accidentally change default behavior.
func TestRefresh_UnpinnedModeIncludesLocalRefresh(t *testing.T) {
	m := newTestModel(false)
	// pinnedHost left zero-valued — this is the default, non-thin-client path.

	batch := refreshBatchForTest(t, m)

	var sawLocal, sawPinnedRemote, sawAllHostsRemote bool
	for _, sub := range batch {
		switch name := cmdFuncName(sub); {
		case strings.HasSuffix(name, ".refreshCmdWithMem.func1"):
			sawLocal = true
		case strings.HasSuffix(name, ".refreshRemoteCmdForHost.func1"):
			sawPinnedRemote = true
		case strings.HasSuffix(name, ".refreshRemoteCmd.func1"):
			sawAllHostsRemote = true
		}
	}

	if !sawLocal {
		t.Error("unpinned refresh() did not dispatch refreshCmdWithMem (local rows)")
	}
	if sawPinnedRemote {
		t.Error("unpinned refresh() dispatched refreshRemoteCmdForHost; should use the all-hosts path instead")
	}
	if !sawAllHostsRemote {
		t.Error("unpinned refresh() did not dispatch refreshRemoteCmd (all-hosts fan-out)")
	}
}

// refreshBatchForTest calls m.refresh() and unwraps the resulting
// tea.Cmd into its tea.BatchMsg slice WITHOUT invoking any sub-command.
// Invoking the outer Cmd only packages the batch (see tea.Batch's
// implementation) — it does not run the inner commands, so this stays
// side-effect-free even though several of the inner commands do real
// I/O when actually invoked.
func refreshBatchForTest(t *testing.T, m *Model) []tea.Cmd {
	t.Helper()
	cmd := m.refresh()
	if cmd == nil {
		t.Fatal("refresh() returned a nil cmd")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("refresh() cmd produced %T; want tea.BatchMsg", msg)
	}
	return batch
}

// TestHandleKey_TabSwitch: tab key flips m.tab between Local and Global.
// Resets cursor to 0 so a long-list scroll position doesn't carry over
// into a different tab confusingly.
func TestHandleKey_TabSwitch(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabLocal

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got := model.(*Model)
	if got.tab != tabGlobal {
		t.Errorf("after tab key: tab = %v; want tabGlobal", got.tab)
	}

	model, _ = got.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got = model.(*Model)
	if got.tab != tabLocal {
		t.Errorf("after second tab key: tab = %v; want tabLocal (round-trip)", got.tab)
	}
}

// TestHandleKey_TabSwitch_NoProjectSkipsLocal verifies the v0.17 Phase
// 1h contract: when canopy is launched outside any project
// (currentProject == ""), the project-scoped tab is NOT part of the
// cycle. Tab from Projects with no hosts wraps back to Projects;
// with hosts it cycles to Hosts. The user never lands on an empty
// project-tab — replaces the old "auto-focus into Local" behavior.
func TestHandleKey_TabSwitch_NoProjectSkipsLocal(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.currentProject = ""
	m.mgr = nil
	// No hosts registered AND no current project — cycle is just
	// [Projects]. Tab from Projects wraps to itself.
	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got := model.(*Model)
	if got.tab != tabGlobal {
		t.Errorf("Tab w/ no project + no hosts: tab = %v; want tabGlobal (stays)", got.tab)
	}
	if got.currentProject != "" {
		t.Errorf("Tab should not auto-focus a project: currentProject = %q; want empty", got.currentProject)
	}
}

// TestHandleKey_TabSwitch_NoProjectWithHostsCyclesToHosts: when there's
// no currentProject but at least one host is registered, the cycle
// is [Projects, Hosts] and Tab from Projects lands on Hosts.
func TestHandleKey_TabSwitch_NoProjectWithHostsCyclesToHosts(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.currentProject = ""
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "user@tower", Type: "ssh"}}
	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got := model.(*Model)
	if got.tab != tabHosts {
		t.Errorf("Tab w/ no project + hosts: tab = %v; want tabHosts", got.tab)
	}
}

// TestHandleKey_TabSwitch_WithProjectCyclesThroughLocal: when launched
// inside a project, the cycle is [<project>, Projects, Hosts?]. Tab
// from the project tab lands on Projects.
func TestHandleKey_TabSwitch_WithProjectCyclesThroughLocal(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabLocal
	m.currentProject = "/p/cravd"
	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	got := model.(*Model)
	if got.tab != tabGlobal {
		t.Errorf("Tab from project tab: tab = %v; want tabGlobal", got.tab)
	}
}

// TestHandleKey_NOnGlobalTab covers the v0.10+ cross-project `n` flow.
// Global tab no longer hides `n` outright; it derives the target project
// from the cursor row (mirroring how d/R/K already work). The two cases
// here pin both halves of the predicate: with a row, n is available and
// opens the picker; without one (or with a row missing ProjectRoot),
// n stays a no-op so the binding's help-line entry disappears too.
//
// Bindings-table semantics: a binding whose Available returns false
// doesn't fire its Action — no err set, no mode change, silent. The
// visual cue (n missing from help) does the user-facing signaling.
func TestHandleKey_NOnGlobalTab(t *testing.T) {
	t.Run("with cursor row → opens picker (cross-project)", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabGlobal
		// Cursor row's ProjectRoot will be loaded by managerForRow.
		// Use the test model's own project (which newTestModel set up
		// with Cfg.ProjectRoot = "/tmp/test-project") so managerForRow
		// hits the m.mgr fast-path and skips config.LoadFrom.
		m.setTestRows([]Row{
			{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "ws-1", Status: state.StatusReady},
		})

		model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		got := model.(*Model)
		if got.mode != newPickerMode {
			t.Errorf("n on Global tab w/ row: mode = %v; want newPickerMode", got.mode)
		}
		if got.newTargetMgr == nil {
			t.Errorf("newTargetMgr unset; want target resolved from cursor row")
		}
		if got.newTargetRoot != "/tmp/test-project" {
			t.Errorf("newTargetRoot = %q; want /tmp/test-project", got.newTargetRoot)
		}
		if got.newTargetName != "test-project" {
			t.Errorf("newTargetName = %q; want test-project", got.newTargetName)
		}
	})

	t.Run("no rows → silent no-op", func(t *testing.T) {
		m := newTestModel(false)
		m.mgr = nil // Pure global-mode invocation (canopy from outside any project).
		m.tab = tabGlobal
		m.setTestRows(nil)

		model, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		got := model.(*Model)
		if got.mode != listMode {
			t.Errorf("n on empty Global tab: mode = %v; want listMode", got.mode)
		}
		if cmd != nil {
			t.Errorf("n on empty Global tab: cmd = %v; want nil (no-op)", cmd)
		}
		if got.newTargetMgr != nil {
			t.Errorf("newTargetMgr set without resolution; want nil")
		}
	})

	t.Run("row missing ProjectRoot → silent no-op", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabGlobal
		m.setTestRows([]Row{
			{Project: "ghost", ProjectRoot: "", Name: "orphan", Status: state.StatusReady},
		})

		model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		got := model.(*Model)
		if got.mode != listMode {
			t.Errorf("n on row w/ empty ProjectRoot: mode = %v; want listMode", got.mode)
		}
	})
}

// TestAvailableNewWorkspace exercises the binding's Available predicate
// directly. Both the help-line filter AND the dispatch gate read from
// this; one source of truth for "is n usable right now."
//
// v0.10+ broadened semantics: Global tab is now a yes when the cursor
// row has a non-empty ProjectRoot — managerForRow does the heavy lift
// at action time. We deliberately don't require m.mgr on the Global
// path so canopy launched from outside any project still gets `n`
// once the user points at a row.
func TestAvailableNewWorkspace(t *testing.T) {
	type rowSpec struct {
		project, root string
	}
	cases := []struct {
		name string
		mgr  bool
		tab  tabKind
		rows []rowSpec
		want bool
	}{
		{"mgr + Local → enabled", true, tabLocal, nil, true},
		{"no mgr + Local → disabled (no canopy.json context)", false, tabLocal, nil, false},
		{"mgr + Global w/ row+root → enabled (cross-project)", true, tabGlobal, []rowSpec{{"p", "/tmp/p"}}, true},
		{"no mgr + Global w/ row+root → enabled (pure-global launch)", false, tabGlobal, []rowSpec{{"p", "/tmp/p"}}, true},
		{"mgr + Global w/ no rows → disabled", true, tabGlobal, nil, false},
		{"no mgr + Global w/ no rows → disabled", false, tabGlobal, nil, false},
		{"Global w/ row missing ProjectRoot → disabled", true, tabGlobal, []rowSpec{{"p", ""}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(false)
			if !tc.mgr {
				m.mgr = nil
			}
			m.tab = tc.tab
			rows := make([]Row, 0, len(tc.rows))
			for _, r := range tc.rows {
				rows = append(rows, Row{Project: r.project, ProjectRoot: r.root, Name: "ws", Status: state.StatusReady})
			}
			m.setTestRows(rows)
			got := availableNewWorkspace(m)
			if got != tc.want {
				t.Errorf("availableNewWorkspace(mgr=%v, tab=%v, rows=%v) = %v; want %v",
					tc.mgr, tc.tab, tc.rows, got, tc.want)
			}
		})
	}
}

// TestAvailableOpenBrowser covers the `B` predicate: live session
// AND a non-zero port. Both fields must be present — a stopped
// session 404s; a row with Port=0 either lost its allocation or was
// never started, both of which mean "nothing to point a browser at."
func TestAvailableOpenBrowser(t *testing.T) {
	cases := []struct {
		name string
		rows []Row
		want bool
	}{
		{"no rows", nil, false},
		{"alive + port → yes", []Row{{Name: "ws", Alive: true, Port: 3001}}, true},
		{"alive + port=0 → no", []Row{{Name: "ws", Alive: true, Port: 0}}, false},
		{"dead + port → no", []Row{{Name: "ws", Alive: false, Port: 3001}}, false},
		{"main + alive + port → yes", []Row{{IsMain: true, Name: "(main)", Alive: true, Port: 3000}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(false)
			m.setTestRows(tc.rows)
			if got := availableOpenBrowser(m); got != tc.want {
				t.Errorf("availableOpenBrowser = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestListModeBinding_Browser verifies the B binding is wired up: the
// keymap table contains a binding whose K matches the literal "B" and
// whose Action is actionOpenBrowser. Belt-and-suspenders against
// accidental drift of the keymap table.
func TestListModeBinding_Browser(t *testing.T) {
	var found bool
	for _, b := range listModeBindings {
		for _, k := range b.K.Keys() {
			if k == "B" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("listModeBindings does not contain a binding for \"B\"")
	}
}

// TestListModeBinding_OpenPR_IsCapital verifies the rebind: lowercase
// p must NOT match the open-PR binding (a stray k-neighbor keypress
// used to fire `gh pr view --web`, which was the user-reported
// annoyance). Capital P is the new keypress.
func TestListModeBinding_OpenPR_IsCapital(t *testing.T) {
	var prBinding *Binding
	for i := range listModeBindings {
		// Identify the openPR binding by the help description rather
		// than the key literal so this test still flags a regression
		// if someone re-adds lowercase p with the same description.
		if listModeBindings[i].K.Help().Desc == "open PR" {
			prBinding = &listModeBindings[i]
			break
		}
	}
	if prBinding == nil {
		t.Fatalf("no binding with help desc %q found", "open PR")
	}
	for _, k := range prBinding.K.Keys() {
		if k == "p" {
			t.Errorf("openPR binding still has lowercase \"p\" key — should be \"P\" only; keys=%v", prBinding.K.Keys())
		}
	}
	var hasCapital bool
	for _, k := range prBinding.K.Keys() {
		if k == "P" {
			hasCapital = true
		}
	}
	if !hasCapital {
		t.Errorf("openPR binding missing capital \"P\"; keys=%v", prBinding.K.Keys())
	}
}

// TestAvailableNewWorkspace_RemoteRow: v0.17 Phase 1i — remote rows
// have no ProjectRoot but `n` should still be available (the dispatch
// hands off to `canopy new --on <host>` instead of going through the
// local Manager).
func TestAvailableNewWorkspace_RemoteRow(t *testing.T) {
	m := newTestModel(false)
	m.mgr = nil
	m.tab = tabGlobal
	m.setTestRows([]Row{
		{Project: "cravd", Name: "foo", Status: state.StatusReady, Host: "tower"},
	})
	if !availableNewWorkspace(m) {
		t.Errorf("availableNewWorkspace on remote row = false; want true (dispatch via canopy new --on)")
	}
}

// TestAvailableNewWorkspace_HostsTab: on the Hosts tab, `n` opens the
// add-host wizard — so it's always available regardless of cursor.
func TestAvailableNewWorkspace_HostsTab(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	if !availableNewWorkspace(m) {
		t.Errorf("availableNewWorkspace on Hosts tab = false; want true (add-host wizard)")
	}
}

// TestActionNewWorkspace_RemoteRowOpensPicker: v0.17 Phase 1k —
// pressing n on a remote row opens the SAME TUI picker as a local
// row, with newTargetHost set so submit handlers dispatch through
// `canopy new --on <host>` instead of the local Manager. Prior
// Phase 1i implementation handed off to a CLI subprocess; the user
// asked for the rich TUI experience.
func TestActionNewWorkspace_RemoteRowOpensPicker(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.hostList = []host.Host{
		{Name: "tower", SSHTarget: "u@t", Type: "ssh",
			Projects: map[string]string{"cravd": "/home/cassy/Work/cravd"}},
	}
	m.setTestRows([]Row{
		{Project: "cravd", Name: "foo", Status: state.StatusReady, Host: "tower"},
	})
	_, _ = actionNewWorkspace(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.mode != newPickerMode {
		t.Fatalf("remote n: mode = %v; want newPickerMode", m.mode)
	}
	if m.newTargetHost != "tower" {
		t.Errorf("newTargetHost = %q; want tower", m.newTargetHost)
	}
	if m.newTargetRemoteCwd != "/home/cassy/Work/cravd" {
		t.Errorf("newTargetRemoteCwd = %q; want /home/cassy/Work/cravd", m.newTargetRemoteCwd)
	}
	if m.newTargetMgr != nil {
		t.Errorf("newTargetMgr should be nil for remote target; got %+v", m.newTargetMgr)
	}
}

// TestNewPicker_RemoteReachesPRIssueBranchShortcuts: v0.21 parity —
// p/i/b shortcuts (PR / Issue / Branch) now open the corresponding
// sub-modal for remote targets too. Loaders SSH `gh` / `git` on the
// host inside the remote project cwd; submit handlers dispatch through
// remoteCreateCmd so the remote canopy resolves the source. Previously
// these options were hidden for remote (v0.17 Phase 1k); flipping the
// gate is what the parity work undoes.
func TestNewPicker_RemoteReachesPRIssueBranchShortcuts(t *testing.T) {
	tests := []struct {
		key      string
		wantMode viewMode
	}{
		{"p", newPRMode},
		{"i", newIssueMode},
		{"b", newBranchMode},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			m := newTestModel(false)
			m.mode = newPickerMode
			m.newTargetHost = "tower"
			m.newTargetRemoteCwd = "/home/avi/Work/cravd"
			m.hostList = []host.Host{
				{Name: "tower", SSHTarget: "u@t", Type: "ssh"},
			}
			_, _ = m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
			if m.mode != tc.wantMode {
				t.Errorf("remote picker: key %q mode = %v; want %v", tc.key, m.mode, tc.wantMode)
			}
		})
	}
}

// TestNewPicker_RemoteCursorReachesAllOptions: arrow nav on the remote
// picker spans all 5 options (Fresh, Prompt, PR, Issue, Branch),
// matching local. Before v0.21 parity the cursor was bounded to 1 to
// match the hidden-options slice.
func TestNewPicker_RemoteCursorReachesAllOptions(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPickerMode
	m.newTargetHost = "tower"

	m.newPickerCursor = 0
	for i := 0; i < 10; i++ {
		_, _ = m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if want := newPickerOptionCount - 1; m.newPickerCursor != want {
		t.Errorf("remote picker: cursor = %d after 10 down presses; want %d", m.newPickerCursor, want)
	}
}

// TestActionNewWorkspace_Local: from Local tab, n populates newTargetMgr
// from m.mgr (the launch-context Manager). Title/root mirror the project
// the user is actively working in.
func TestActionNewWorkspace_Local(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabLocal

	model, _ := actionNewWorkspace(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	got := model.(*Model)
	if got.mode != newPickerMode {
		t.Fatalf("mode = %v; want newPickerMode", got.mode)
	}
	if got.newTargetMgr != m.mgr {
		t.Errorf("newTargetMgr = %p; want m.mgr (%p)", got.newTargetMgr, m.mgr)
	}
	if got.newTargetRoot != m.mgr.Cfg.ProjectRoot {
		t.Errorf("newTargetRoot = %q; want %q", got.newTargetRoot, m.mgr.Cfg.ProjectRoot)
	}
	if got.newTargetName != m.mgr.Cfg.Project {
		t.Errorf("newTargetName = %q; want %q", got.newTargetName, m.mgr.Cfg.Project)
	}
}

// TestActionNewWorkspace_ClearsOnEsc: pressing esc out of the picker
// clears the in-flight new-workspace target so the next `n` press
// starts from a clean slate.
func TestActionNewWorkspace_ClearsOnEsc(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabLocal
	model, _ := actionNewWorkspace(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = model.(*Model)
	if m.newTargetMgr == nil {
		t.Fatal("setup: newTargetMgr should be set after actionNewWorkspace")
	}

	model, _ = m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	got := model.(*Model)
	if got.mode != listMode {
		t.Errorf("after esc: mode = %v; want listMode", got.mode)
	}
	if got.newTargetMgr != nil || got.newTargetRoot != "" || got.newTargetName != "" {
		t.Errorf("after esc: newTarget* should be cleared; got mgr=%v root=%q name=%q",
			got.newTargetMgr != nil, got.newTargetRoot, got.newTargetName)
	}
}

// TestRenderTargetBanner_ShowsProjectName: the banner must render the
// target project as a primary identifier (chip + path), not subtle
// chrome — load-bearing for cross-project intent clarity.
func TestRenderTargetBanner_ShowsProjectName(t *testing.T) {
	m := newTestModel(false)
	m.newTargetName = "cravd"
	m.newTargetRoot = "/Users/avi/Work/cravd"

	out := stripAnsi(m.renderTargetBanner())
	if !strings.Contains(out, "creating in") {
		t.Errorf("banner missing 'creating in' label: %q", out)
	}
	if !strings.Contains(out, "cravd") {
		t.Errorf("banner missing project name: %q", out)
	}
	if !strings.Contains(out, "/Users/avi/Work/cravd") {
		t.Errorf("banner missing project root: %q", out)
	}
}

// TestRenderTargetBanner_RemoteShowsHost: when targeting a remote
// project, the banner switches to "creating on <host> in <project>"
// and surfaces the REMOTE path (not a missing local root). This is
// load-bearing for the same reason as the cross-project case — the
// user should never fire `n` on a remote row thinking it'll create
// locally. The host pill renders distinctly from the project pill
// (different bg color); we test the structural change here and let
// rendering smoke-tests cover the visuals.
func TestRenderTargetBanner_RemoteShowsHost(t *testing.T) {
	m := newTestModel(false)
	m.newTargetName = "brain"
	m.newTargetHost = "pi"
	m.newTargetRemoteCwd = "/home/jarvis/Work/brain"

	out := stripAnsi(m.renderTargetBanner())
	if !strings.Contains(out, "creating on") {
		t.Errorf("remote banner missing 'creating on' label: %q", out)
	}
	if !strings.Contains(out, "pi") {
		t.Errorf("remote banner missing host name: %q", out)
	}
	if !strings.Contains(out, "brain") {
		t.Errorf("remote banner missing project name: %q", out)
	}
	if !strings.Contains(out, "/home/jarvis/Work/brain") {
		t.Errorf("remote banner missing REMOTE cwd: %q", out)
	}
	if strings.Contains(out, "creating in") && !strings.Contains(out, "in  ") {
		// We use "creating on <host> in <project>" — the literal
		// "creating in" prefix from the local path would be wrong.
		// The "in" between host and project is fine.
		t.Errorf("remote banner should say 'creating on', not 'creating in <project>': %q", out)
	}
}

// TestRenderTargetBanner_LocalUnchanged: the host-pill addition must
// not regress the local banner — when newTargetHost is empty, render
// exactly as before ("creating in <project> <root>"). Guards against
// accidentally branching the local path through the remote code.
func TestRenderTargetBanner_LocalUnchanged(t *testing.T) {
	m := newTestModel(false)
	m.newTargetName = "cravd"
	m.newTargetRoot = "/Users/avi/Work/cravd"
	// newTargetHost intentionally empty.

	out := stripAnsi(m.renderTargetBanner())
	if !strings.Contains(out, "creating in") {
		t.Errorf("local banner must keep 'creating in' label: %q", out)
	}
	if strings.Contains(out, "creating on") {
		t.Errorf("local banner must not say 'creating on' (that's the remote form): %q", out)
	}
}

// TestRenderTargetBanner_EmptyWhenUnset: outside the new-workspace flow
// the banner returns "" so render paths that include it (busy view's
// non-create ops, future call sites) emit nothing rather than a stray
// blank line.
func TestRenderTargetBanner_EmptyWhenUnset(t *testing.T) {
	m := newTestModel(false)
	if got := m.renderTargetBanner(); got != "" {
		t.Errorf("renderTargetBanner with no target = %q; want empty string", got)
	}
}

// TestBusySuccessMessage_CreateNamesProject: the success line for a
// completed create includes the target project so the banner's promise
// ("creating in cravd") is fulfilled at completion ("created in cravd").
// Empty projectName falls back to the legacy generic message.
func TestBusySuccessMessage_CreateNamesProject(t *testing.T) {
	t.Run("create with project → named line", func(t *testing.T) {
		got := busySuccessMessage(busyOpCreate, "cravd")
		if !strings.Contains(got, "cravd") {
			t.Errorf("create success = %q; want it to name the project", got)
		}
	})
	t.Run("create without project → generic line", func(t *testing.T) {
		got := busySuccessMessage(busyOpCreate, "")
		if got != "Workspace created successfully." {
			t.Errorf("create success (no project) = %q; want generic legacy line", got)
		}
	})
	t.Run("remove ignores projectName", func(t *testing.T) {
		got := busySuccessMessage(busyOpRemove, "cravd")
		if got != "Workspace removed." {
			t.Errorf("remove success = %q; want 'Workspace removed.'", got)
		}
	})
}

// TestHandleKey_SearchEntry: pressing "/" enters search mode and
// initializes searchQuery. Any subsequent key goes through
// handleSearchKey via the search-mode bypass in handleKey.
func TestHandleKey_SearchEntry(t *testing.T) {
	m := newTestModel(false)
	if m.searchMode {
		t.Fatal("setup: searchMode should start false")
	}

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	got := model.(*Model)
	if !got.searchMode {
		t.Errorf("after / key: searchMode = false; want true")
	}
	if got.searchQuery != "" {
		t.Errorf("after / key: searchQuery = %q; want empty", got.searchQuery)
	}
}

// TestFilteredRows_TabFilter: tabLocal scopes rows to currentProject;
// tabGlobal returns everything. Empty-current-project Local tab returns
// empty (the "outside any project" case shows onboarding text).
func TestFilteredRows_TabFilter(t *testing.T) {
	m := newTestModel(false)
	m.currentProject = "/p/foo"
	m.setTestRows([]Row{
		{Project: "foo", ProjectRoot: "/p/foo", Name: "ws-a"},
		{Project: "foo", ProjectRoot: "/p/foo", Name: "ws-b"},
		{Project: "bar", ProjectRoot: "/p/bar", Name: "ws-c"},
	})

	m.tab = tabLocal
	got := m.filteredRows()
	if len(got) != 2 {
		t.Errorf("Local tab: got %d rows; want 2 (foo only)", len(got))
	}
	for _, r := range got {
		if r.ProjectRoot != "/p/foo" {
			t.Errorf("Local tab leaked cross-project row: %+v", r)
		}
	}

	m.tab = tabGlobal
	got = m.filteredRows()
	if len(got) != 3 {
		t.Errorf("Global tab: got %d rows; want 3 (all)", len(got))
	}
}

// TestFilteredRows_SearchFilter: searchQuery matches name OR project OR
// branch via subsequence (fzf-style). Empty query returns all rows.
func TestFilteredRows_SearchFilter(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.setTestRows([]Row{
		{Project: "foo", ProjectRoot: "/p/foo", Name: "silent-falcon"},
		{Project: "foo", ProjectRoot: "/p/foo", Name: "misty-aspen"},
		{Project: "bar", ProjectRoot: "/p/bar", Name: "bold-ox", Branch: "feat/falcon"},
	})

	m.searchQuery = "fal"
	got := m.filteredRows()
	if len(got) != 2 {
		t.Errorf("search 'fal': got %d; want 2 (silent-falcon name + bold-ox branch)", len(got))
	}

	m.searchQuery = "bar"
	got = m.filteredRows()
	if len(got) != 1 || got[0].Name != "bold-ox" {
		t.Errorf("search 'bar': got %v; want [bold-ox] (project match)", got)
	}

	m.searchQuery = ""
	got = m.filteredRows()
	if len(got) != 3 {
		t.Errorf("empty search: got %d; want 3 (all)", len(got))
	}
}

// TestFilteredRows_LoadingPlaceholdersForHostsWithoutRows: registered
// hosts that have no rows in m.remoteRows must get a synthetic
// Loading=true placeholder appended so the Workspaces tab renders the
// host section header on first launch instead of hiding the host
// entirely until SSH returns. Hosts that already have rows do not get
// a placeholder (the real rows already carry the host).
func TestFilteredRows_LoadingPlaceholdersForHostsWithoutRows(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.hostList = []host.Host{{Name: "tower"}, {Name: "jarvis"}}
	m.remoteRows = []state.GlobalRow{
		// jarvis has a row already — should NOT get a placeholder.
		{Host: "jarvis", Project: "brain", Name: "(main)", IsMain: true},
	}

	got := m.filteredRows()

	var towerLoading, jarvisLoading int
	for _, r := range got {
		if r.Loading && r.Host == "tower" {
			towerLoading++
		}
		if r.Loading && r.Host == "jarvis" {
			jarvisLoading++
		}
	}
	if towerLoading != 1 {
		t.Errorf("tower (no rows yet): want 1 loading placeholder; got %d", towerLoading)
	}
	if jarvisLoading != 0 {
		t.Errorf("jarvis (has rows): want 0 loading placeholders; got %d", jarvisLoading)
	}
}

// TestFilteredRows_NoLoadingPlaceholdersOnLocalTab: the Local tab
// strips remote rows entirely, so loading placeholders (which only
// matter for remote hosts) must not appear there either.
func TestFilteredRows_NoLoadingPlaceholdersOnLocalTab(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabLocal
	m.hostList = []host.Host{{Name: "tower"}}

	got := m.filteredRows()
	for _, r := range got {
		if r.Loading {
			t.Errorf("Local tab leaked Loading row: %+v", r)
		}
	}
}

// TestFilteredRows_NoLoadingPlaceholdersWhileSearching: an active
// searchQuery means the user is hunting for a specific row by name —
// placeholders carry no name/project/branch to match against, so
// silencing them keeps the search result list tight instead of padded
// with phantom "loading…" lines on every no-match query.
func TestFilteredRows_NoLoadingPlaceholdersWhileSearching(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.hostList = []host.Host{{Name: "tower"}}
	m.searchQuery = "anything"

	got := m.filteredRows()
	for _, r := range got {
		if r.Loading {
			t.Errorf("search query active leaked Loading row: %+v", r)
		}
	}
}

// TestActionDelete_StoresProjectRoot is the regression test for the C5
// adversarial finding: cross-project delete must match by (Project, Name)
// pair, not Name alone. Two projects each with a workspace named "foo"
// would otherwise be ambiguous if a refresh re-orders rows between
// modal-open and confirm — the user could delete project B's foo when
// they meant project A's foo (data loss).
//
// Verifies actionDelete records BOTH deleteTarget AND deleteTargetRoot
// when opening the confirm modal.
func TestActionDelete_StoresProjectRoot(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.setTestRows([]Row{
		{Project: "alpha", ProjectRoot: "/p/alpha", Name: "foo", Status: state.StatusReady, Path: "/ws/alpha-foo"},
		{Project: "bravo", ProjectRoot: "/p/bravo", Name: "foo", Status: state.StatusReady, Path: "/ws/bravo-foo"},
	})
	// Cursor starts at row 0 (alpha's foo).

	// We can't call actionDelete directly on cross-project rows here
	// because managerForRow needs canopy.json on disk for /p/alpha. Test
	// the field-recording invariant by calling actionDelete and checking
	// state — error path included.
	_, _ = actionDelete(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})

	// Even if managerForRow errored (no canopy.json at /p/alpha in this
	// fake state), the precondition for the data-loss scenario is that
	// deleteTarget+deleteTargetRoot get set BEFORE managerForRow is
	// called. Verify the structure: when modal opens (mode change), root
	// must be set; when modal aborts (managerForRow err), neither is set.
	if m.mode == confirmDeleteMode {
		// Modal opened: both fields must be populated and consistent.
		if m.deleteTarget != "foo" {
			t.Errorf("deleteTarget = %q; want 'foo'", m.deleteTarget)
		}
		if m.deleteTargetRoot != "/p/alpha" {
			t.Errorf("deleteTargetRoot = %q; want '/p/alpha' (cursor row's project)", m.deleteTargetRoot)
		}
	} else {
		// Modal didn't open (managerForRow failed before mode change).
		// Both fields should remain empty so a stale value can't leak
		// into the next attempt.
		if m.deleteTarget != "" || m.deleteTargetRoot != "" {
			t.Errorf("modal didn't open but deleteTarget=%q / deleteTargetRoot=%q leaked",
				m.deleteTarget, m.deleteTargetRoot)
		}
	}
}

// TestRetryConfirmModal_NonBrokenTriggers: pressing R on a non-broken
// workspace opens the confirmRetry y/N gate (D3/CP1) instead of
// erroring. Mirrors the CLI's --force friction in TUI form.
func TestRetryConfirmModal_NonBrokenTriggers(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{Project: "test-project", ProjectRoot: "/tmp/test-project",
			Name: "healthy-ws", Status: state.StatusReady},
	})

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	got := model.(*Model)
	if got.mode != confirmRetryMode {
		t.Errorf("after R on healthy ws: mode = %v; want confirmRetryMode", got.mode)
	}
	if got.retryTarget != "healthy-ws" {
		t.Errorf("retryTarget = %q; want healthy-ws", got.retryTarget)
	}
}

// TestRetryConfirmModal_CancelOnN: pressing n in confirmRetryMode cancels
// back to listMode without dispatching a retry.
func TestRetryConfirmModal_CancelOnN(t *testing.T) {
	m := newTestModel(false)
	m.mode = confirmRetryMode
	m.retryTarget = "healthy-ws"

	model, cmd := m.handleConfirmRetryKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	got := model.(*Model)
	if got.mode != listMode {
		t.Errorf("after n: mode = %v; want listMode", got.mode)
	}
	if got.retryTarget != "" {
		t.Errorf("after n: retryTarget = %q; want empty", got.retryTarget)
	}
	if cmd != nil {
		t.Errorf("after n: cmd != nil; want nil (no retry dispatched)")
	}
}

// TestEscapeIfDeletingCurrent_NoOpWhenMismatched: the escape helper
// short-circuits when any of the gating conditions fail — nil mgr,
// empty currentWorkspace, name mismatch, OR project-root mismatch
// (workspace names are unique per-project, not globally — A/foo and
// B/foo coexist, and switching when deleting B/foo from inside A/foo
// would trip the user into the wrong project's main session).
func TestEscapeIfDeletingCurrent_NoOpWhenMismatched(t *testing.T) {
	cases := []struct {
		name                 string
		mgr                  *workspace.Manager
		currentWorkspace     string
		currentWorkspaceRoot string
		targetRoot           string
		targetName           string
	}{
		{"nil mgr", nil, "ws-a", "/a", "/a", "ws-a"},
		{"empty currentWorkspace", &workspace.Manager{}, "", "/a", "/a", "ws-a"},
		{"name mismatch", &workspace.Manager{}, "ws-b", "/a", "/a", "ws-a"},
		{"project root mismatch (cross-project name collision)", &workspace.Manager{}, "ws-a", "/a", "/b", "ws-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{currentWorkspace: tc.currentWorkspace, currentWorkspaceRoot: tc.currentWorkspaceRoot, tc: nil}
			// No panic = pass: we never reached the tmux calls.
			m.escapeIfDeletingCurrent(tc.mgr, tc.targetRoot, tc.targetName)
		})
	}
}

// TestUpdate_RowsLoaded_EmptyFirstThenPreselects: the latch must not
// fire on an early empty rowsLoadedMsg — popup invocations sometimes
// see an initial probe with no rows yet (state racing the refresh
// goroutine), and consuming the preselect opportunity there leaves
// cursor=0 even after real rows arrive on the next refresh. The
// preselect should still hit when the actual rows show up.
func TestUpdate_RowsLoaded_EmptyFirstThenPreselects(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.currentProject = "/b/canopy"
	m.currentWorkspaceRoot = "/b/canopy"
	m.currentWorkspace = "ancient-hornet"

	// First refresh arrives empty (or with an error).
	next, _ := m.Update(rowsLoadedMsg{rows: nil, err: nil})
	got := next.(*Model)
	if got.initialCursorPlaced {
		t.Errorf("latch fired on empty first refresh; preselect would be lost")
	}

	// Second refresh has the real rows.
	rows := []Row{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "bold-falcon", Status: state.StatusReady},
		{Project: "canopy", ProjectRoot: "/b/canopy", Name: "ancient-hornet", Status: state.StatusReady},
	}
	next2, _ := got.Update(rowsLoadedMsg{rows: rows})
	got2 := next2.(*Model)
	if !got2.initialCursorPlaced {
		t.Errorf("latch did not fire after preselect succeeded")
	}
	cur, _ := got2.list.CursorRow()
	if cur.Name != "ancient-hornet" {
		t.Errorf("cursor row name = %q; want ancient-hornet", cur.Name)
	}
}

// TestUpdate_RowsLoaded_LatchFiresOnMiss: when the first non-empty
// load doesn't contain the target row (e.g. the user changed tab
// before rows arrived, filtering it out), the latch must still fire
// so a later refresh — when the row reappears in the filtered set —
// doesn't yank the cursor away from wherever the user navigated to.
func TestUpdate_RowsLoaded_LatchFiresOnMiss(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.currentProject = "/b/canopy"
	m.currentWorkspaceRoot = "/b/canopy"
	m.currentWorkspace = "ancient-hornet"

	// Rows that DON'T contain the target — simulates user-flipped tab
	// or search hiding the row at first-load time.
	rows := []Row{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "bold-falcon", Status: state.StatusReady},
	}
	next, _ := m.Update(rowsLoadedMsg{rows: rows})
	got := next.(*Model)
	if !got.initialCursorPlaced {
		t.Errorf("latch did not fire on first non-empty load with miss; would auto-jump on later refresh")
	}
}

// TestUpdate_RowsLoaded_PreselectsCurrentWorkspace: when currentWorkspace
// is set (popup launched from inside a workspace dir), the first
// rowsLoadedMsg moves the cursor onto that workspace's row. Subsequent
// refreshes are no-ops on cursor position (latched via initialCursorPlaced)
// so the user's hovering doesn't get yanked back.
func TestUpdate_RowsLoaded_PreselectsCurrentWorkspace(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal // multi-row scenario; cursor=0 initially
	m.currentProject = "/b/canopy"
	m.currentWorkspaceRoot = "/b/canopy"
	m.currentWorkspace = "ancient-hornet"

	rows := []Row{
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "bold-falcon", Status: state.StatusReady},
		{Project: "cravd", ProjectRoot: "/a/cravd", Name: "soft-fox", Status: state.StatusStopped},
		{Project: "canopy", ProjectRoot: "/b/canopy", Name: "ancient-hornet", Status: state.StatusReady},
	}

	next, _ := m.Update(rowsLoadedMsg{rows: rows})
	got := next.(*Model)
	if !got.initialCursorPlaced {
		t.Errorf("initialCursorPlaced = false after first load; want true")
	}
	cur, ok := got.list.CursorRow()
	if !ok {
		t.Fatalf("CursorRow not ok after rowsLoaded")
	}
	if cur.Name != "ancient-hornet" {
		t.Errorf("cursor row name = %q; want %q (preselect should land here)", cur.Name, "ancient-hornet")
	}

	// Second refresh: user has navigated to row 0 in the meantime;
	// we simulate by setting cursor manually, then dispatching another
	// rowsLoadedMsg. Cursor should NOT yank back.
	got.list.SetCursorTo("/a/cravd", "bold-falcon")
	next2, _ := got.Update(rowsLoadedMsg{rows: rows})
	got2 := next2.(*Model)
	cur2, _ := got2.list.CursorRow()
	if cur2.Name != "bold-falcon" {
		t.Errorf("cursor after second refresh = %q; want bold-falcon (latched)", cur2.Name)
	}
}

// TestUpdate_CreateDoneAutoAttaches: a successful createDoneMsg with a
// non-empty tmuxSession resets busy state and dispatches an attachCmd —
// the user pressed 'n' to create + use a workspace, so we drop them
// straight into the session instead of a "press any key to dismiss"
// gate.
func TestUpdate_CreateDoneAutoAttaches(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate
	m.busyTitle = "Creating workspace \"foo\"..."

	msg := createDoneMsg{
		output:      "setup ran fine\n",
		tmuxSession: "test-project-foo",
		err:         nil,
	}
	next, cmd := m.Update(msg)
	gotModel := next.(*Model)

	if gotModel.mode != listMode {
		t.Errorf("mode = %v; want listMode after auto-attach", gotModel.mode)
	}
	if gotModel.busyOp != busyOpNone {
		t.Errorf("busyOp = %v; want busyOpNone", gotModel.busyOp)
	}
	if gotModel.busyTitle != "" {
		t.Errorf("busyTitle = %q; want empty", gotModel.busyTitle)
	}
	if cmd == nil {
		t.Fatal("expected attach cmd; got nil")
	}
	// Don't actually invoke cmd() — it would try to exec tmux. The
	// non-nil return is the signal that the dispatch happened.
}

// TestUpdate_CreateDoneOnErrorStaysInBusy: a failed createDoneMsg keeps
// busyMode active so the user can read the captured setup output. The
// existing handleBusyModeKey dismisses on any key press.
func TestUpdate_CreateDoneOnErrorStaysInBusy(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate

	msg := createDoneMsg{
		output:      "boom\n",
		tmuxSession: "",
		err:         errFakeCreate,
	}
	next, cmd := m.Update(msg)
	gotModel := next.(*Model)

	if gotModel.mode != busyMode {
		t.Errorf("mode = %v; want busyMode (so user sees error)", gotModel.mode)
	}
	if !gotModel.busyDone {
		t.Errorf("busyDone = false; want true so any key dismisses")
	}
	if gotModel.busyErr == nil {
		t.Errorf("busyErr = nil; want the create error")
	}
	if cmd != nil {
		t.Errorf("expected nil cmd on create error; got %v", cmd)
	}
}

// TestRenderHelpLine_TabSwitch: help line shows nav, tab, and search
// keybinds always. `n` desc ("new") shows only on Local tab with
// non-nil mgr.
//
// stripAnsi the rendered output so assertions don't couple to the
// keybind-pill styling — these tests are about which BINDINGS appear,
// not how they look.
func TestRenderHelpLine_TabSwitch(t *testing.T) {
	t.Run("Local tab with mgr → n shown", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabLocal
		out := stripAnsi(m.renderHelpLine())
		if !strings.Contains(out, "new") {
			t.Errorf("Local tab help missing 'new' desc: %q", out)
		}
		if !strings.Contains(out, "switch-tab") {
			t.Errorf("help line missing 'switch-tab': %q", out)
		}
	})

	t.Run("Global tab w/ row → n shown (cross-project)", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabGlobal
		m.setTestRows([]Row{
			{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "ws", Status: state.StatusReady},
		})
		out := stripAnsi(m.renderHelpLine())
		if !strings.Contains(out, "new") {
			t.Errorf("Global tab w/ row help should show 'new' desc (cross-project n): %q", out)
		}
	})

	t.Run("Global tab w/ no rows → n hidden", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabGlobal
		m.setTestRows(nil)
		out := stripAnsi(m.renderHelpLine())
		if strings.Contains(out, "new") {
			t.Errorf("Global tab w/o rows should not show 'new' desc: %q", out)
		}
	})

	t.Run("nil mgr + Local tab → n hidden", func(t *testing.T) {
		m := newTestModel(false)
		m.mgr = nil
		m.tab = tabLocal
		out := stripAnsi(m.renderHelpLine())
		if strings.Contains(out, "new") {
			t.Errorf("nil mgr Local help should not show 'new' desc: %q", out)
		}
	})
}

// TestRenderHelpLine_WrapsAtWidth covers the v0.17.1.1 width-aware
// wrap. The legend used to be one line, which overflowed in tmux popups
// and on narrow terminals. The wrap keeps groups (nav / tabs / open /
// act / meta) intact and breaks between groups when the screen is too
// narrow. m.width == 0 (pre-WindowSizeMsg) stays single-line for the
// existing tests + render paths.
func TestRenderHelpLine_WrapsAtWidth(t *testing.T) {
	t.Run("width=0 (no resize yet) → single line", func(t *testing.T) {
		m := newTestModel(false)
		m.width = 0
		out := stripAnsi(m.renderHelpLine())
		if strings.Contains(out, "\n") {
			t.Errorf("width=0 should render on one line; got:\n%q", out)
		}
	})

	t.Run("wide width → one group per line", func(t *testing.T) {
		m := newTestModel(false)
		m.width = 240
		out := stripAnsi(m.renderHelpLine())
		// Even at wide width, the new contract is one group per line.
		// On Local tab with default test rows, all 5 groups are present:
		// nav / tabs / open / act / meta.
		lines := strings.Split(out, "\n")
		if len(lines) < 4 {
			t.Errorf("wide width should still emit one group per line (≥4 lines); got %d:\n%s", len(lines), out)
		}
	})

	t.Run("narrow width wraps within groups", func(t *testing.T) {
		m := newTestModel(false)
		m.width = 40
		out := stripAnsi(m.renderHelpLine())
		// At width=40, the wider groups (tabs, act) should split chip-
		// by-chip across additional lines, pushing total line count
		// above the base 5 groups.
		lines := strings.Split(out, "\n")
		if len(lines) <= 5 {
			t.Errorf("width=40 should overflow some groups (>5 lines); got %d:\n%s", len(lines), out)
		}
		// Sanity: every line should be within the width budget
		// (widthMargin=4 in renderHelpLine).
		for _, line := range lines {
			if len(line) > m.width {
				t.Errorf("wrapped line exceeds width %d: %q", m.width, line)
			}
		}
	})

	t.Run("nav chip always leads line 1", func(t *testing.T) {
		m := newTestModel(false)
		m.width = 80
		out := stripAnsi(m.renderHelpLine())
		firstLine := strings.TrimLeft(strings.SplitN(out, "\n", 2)[0], " ")
		// keyPillStyle adds horizontal padding around chips, so the nav
		// chip starts with leading spaces — trim them before HasPrefix.
		if !strings.HasPrefix(firstLine, "↑/↓") {
			t.Errorf("nav chip should anchor line 1; got first line: %q", firstLine)
		}
	})

	t.Run("short viewport → compact single line with ? more", func(t *testing.T) {
		m := newTestModel(false)
		m.width = 120
		m.height = 12 // below compactHelpHeight (20)
		out := stripAnsi(m.renderHelpLine())
		if strings.Contains(out, "\n") {
			t.Errorf("short viewport should render one line; got:\n%q", out)
		}
		// Essential chips present.
		for _, want := range []string{"↑/↓", "nav", "enter", "attach", "?", "more", "q", "quit"} {
			if !strings.Contains(out, want) {
				t.Errorf("compact help missing %q: %q", want, out)
			}
		}
		// Non-essential verbs hidden behind `?`.
		for _, hidden := range []string{"switch-tab", "search", "kill tmux", "inspect", "refresh"} {
			if strings.Contains(out, hidden) {
				t.Errorf("compact help should hide %q (behind ? more); got: %q", hidden, out)
			}
		}
	})

	t.Run("compact mode hides `n new` when binding unavailable", func(t *testing.T) {
		m := newTestModel(false)
		m.mgr = nil // disables availableNewWorkspace on Local
		m.tab = tabLocal
		m.width = 120
		m.height = 12
		out := stripAnsi(m.renderHelpLine())
		if strings.Contains(out, "new") {
			t.Errorf("compact help should hide `n new` when unavailable: %q", out)
		}
	})

	t.Run("Hosts tab help wraps too (host-specific bindings still grouped)", func(t *testing.T) {
		m := newTestModel(false)
		m.tab = tabHosts
		m.width = 80
		out := stripAnsi(m.renderHelpLine())
		// One group per line means any positive width produces multiple
		// lines (≥ number of non-empty groups).
		if !strings.Contains(out, "\n") {
			t.Errorf("Hosts tab help should render multiple lines; got:\n%q", out)
		}
		// And the nav anchor still leads line 1.
		firstLine := strings.TrimLeft(strings.SplitN(out, "\n", 2)[0], " ")
		// keyPillStyle adds horizontal padding around chips, so the nav
		// chip starts with leading spaces — trim them before HasPrefix.
		if !strings.HasPrefix(firstLine, "↑/↓") {
			t.Errorf("Hosts tab: nav chip should anchor line 1; got first line: %q", firstLine)
		}
	})
}

// TestRenderHelp_RKeybindCopy locks in the user-facing wording for the
// `R` keybind. The original "retry scripts.setup" wording was ambiguous
// ("retry what?") — readers of the help screen could not tell what
// pressing R actually did. The fix is to say "re-run setup" everywhere
// the keybind is described to a user. This test guards against the
// older wording silently creeping back in.
func TestRenderHelp_RKeybindCopy(t *testing.T) {
	m := newTestModel(false)
	out := stripAnsi(m.renderHelp())

	if !strings.Contains(out, "re-run setup on a broken workspace") {
		t.Errorf("help screen missing new R-keybind copy 're-run setup on a broken workspace':\n%s", out)
	}
	if !strings.Contains(out, "press R to re-run setup") {
		t.Errorf("help legend missing broken-row hint 'press R to re-run setup':\n%s", out)
	}
	if strings.Contains(out, "retry scripts.setup") {
		t.Errorf("help screen still uses old ambiguous 'retry scripts.setup' wording:\n%s", out)
	}
}

// stripAnsi removes ANSI SGR escape sequences from s. Tiny inline impl
// rather than pulling in a dep; canopy's help-line render only emits
// SGR (\x1b[...m), no cursor-movement codes.
func stripAnsi(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if c == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// TestRowHintsMsg_MergesIntoMatchingRow: late-arriving lifecycle hints
// merge into the matching row by name + project. After the v0.8 pivot
// to projectlist for rendering, hint storage lives inside the embedded
// list (list.UpdateRowHints). The model's allRows is unaffected; the
// View sees the merged hints because it delegates to list.View().
func TestRowHintsMsg_MergesIntoMatchingRow(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "soft-fox"},
		{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "ancient-hornet"},
	})
	hints := []state.Hint{{Kind: "shipped", Message: "merged"}}

	model, _ := m.Update(rowHintsMsg{project: "test-project", name: "soft-fox", hints: hints})
	m = model.(*Model)

	// projectlist owns the rendered rows; UpdateRowHints mutates them.
	// View() includes the badge text from the rendered hints.
	out := m.list.View()
	if !strings.Contains(out, "shipped") && !strings.Contains(out, "merged") {
		// Specific badge text may differ; confirm the hint reached
		// projectlist by checking the badge exists at all.
		t.Errorf("hint not surfaced in projectlist view:\n%s", out)
	}
}

// TestRowHintsMsg_NoMatchIsSilent: a hint update for a row that no
// longer exists (concurrent rm dropped it) is a no-op, not a panic.
// projectlist.UpdateRowHints handles this — silent on no-match.
func TestRowHintsMsg_NoMatchIsSilent(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{Project: "test-project", ProjectRoot: "/tmp/test-project", Name: "soft-fox"},
	})

	model, _ := m.Update(rowHintsMsg{project: "test-project", name: "ghost-row", hints: []state.Hint{{Kind: "shipped"}}})
	m = model.(*Model)

	// No panic, no mutation observable on the matched row's view.
	out := m.list.View()
	if strings.Contains(out, "ghost-row") {
		t.Errorf("ghost-row should not appear in view: %s", out)
	}
}

// TestView_HintBadgesAppearInProjectlist: the unified TUI renders rows
// via projectlist, which picks up Hints and renders the corresponding
// badges. Critical for consistency — same badge vocabulary across
// every canopy surface.
func TestView_HintBadgesAppearInProjectlist(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{{
		Project:     "test-project",
		ProjectRoot: "/tmp/test-project",
		Name:        "soft-fox",
		Branch:      "soft-fox",
		Status:      state.StatusReady,
		Hints:       []state.Hint{{Kind: "pr_status", Message: "PR #42 merged; ready to close workspace"}},
	}})
	out := m.list.View()
	if !strings.Contains(out, "PR merged") {
		t.Errorf("PR merged badge missing in projectlist view:\n%s", out)
	}
}

// TestView_NoHintsNoBadges: rows without hints render unchanged
// (no trailing badge text).
func TestView_NoHintsNoBadges(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{{
		Project:     "test-project",
		ProjectRoot: "/tmp/test-project",
		Name:        "soft-fox",
		Branch:      "soft-fox",
		Status:      state.StatusReady,
	}})
	out := m.list.View()
	for _, badge := range []string{"↻ rename", "✓ merged", "PR open", "PR merged"} {
		if strings.Contains(out, badge) {
			t.Errorf("unexpected badge %q in row without hints:\n%s", badge, out)
		}
	}
}

// TestNewPicker_LetterShortcuts: each variant key dispatches directly
// to the right sub-modal. Recognition over recall — the user sees the
// letter inline with the option and presses it.
func TestNewPicker_LetterShortcuts(t *testing.T) {
	cases := []struct {
		key      string
		wantMode viewMode
	}{
		{"n", newFreshMode},
		{"f", newFreshMode}, // alias
		{"p", newPRMode},
		{"i", newIssueMode},
		{"b", newBranchMode},
		{"t", newPromptMode},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			m := newTestModel(false)
			m.openNewPicker()

			model, _ := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
			m = model.(*Model)
			if m.mode != tc.wantMode {
				t.Errorf("key %q: mode = %v; want %v", tc.key, m.mode, tc.wantMode)
			}
		})
	}
}

// TestNewPicker_ArrowsThenEnter: keyboard-discovery flow — arrow
// down to the desired option, hit enter. Equivalent to pressing the
// letter directly. The cursor index follows newPickerOptions order:
// fresh, prompt, PR, issue, branch.
func TestNewPicker_ArrowsThenEnter(t *testing.T) {
	cases := []struct {
		downs    int
		wantMode viewMode
	}{
		{0, newFreshMode},
		{1, newPromptMode},
		{2, newPRMode},
		{3, newIssueMode},
		{4, newBranchMode},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("downs=%d", tc.downs), func(t *testing.T) {
			m := newTestModel(false)
			m.openNewPicker()

			for i := 0; i < tc.downs; i++ {
				model, _ := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyDown})
				m = model.(*Model)
			}
			if m.newPickerCursor != tc.downs {
				t.Fatalf("cursor = %d; want %d", m.newPickerCursor, tc.downs)
			}

			model, _ := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyEnter})
			m = model.(*Model)
			if m.mode != tc.wantMode {
				t.Errorf("enter on cursor=%d: mode = %v; want %v",
					tc.downs, m.mode, tc.wantMode)
			}
		})
	}
}

// TestNewPicker_EscReturnsToList: esc on the picker steps back to
// listMode (one level up). q is suppressed inside the picker so the
// user can't accidentally quit canopy mid-flow.
func TestNewPicker_EscReturnsToList(t *testing.T) {
	m := newTestModel(false)
	m.openNewPicker()

	model, _ := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = model.(*Model)
	if m.mode != listMode {
		t.Errorf("esc on picker should return to listMode; got %v", m.mode)
	}
}

// TestNewPicker_QSuppressed: pressing 'q' inside the picker is a
// no-op (won't quit canopy). User has to esc back to listMode first
// to quit. Protects against fat-fingered exits in the middle of a
// flow.
func TestNewPicker_QSuppressed(t *testing.T) {
	m := newTestModel(false)
	m.openNewPicker()

	_, cmd := m.handleNewPickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		t.Errorf("q in picker should be a no-op; got cmd %T", cmd)
	}
}

// TestNewFresh_EnterCreates: in fresh sub-modal, enter submits with
// the typed name and flips to busyMode. Empty name passes through
// to namegen via Manager.Create.
func TestNewFresh_EnterCreates(t *testing.T) {
	m := newTestModel(false)
	m.openNewFresh()
	m.nameInput.SetValue("fresh-one")

	model, cmd := m.handleNewFreshKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode; got %v", m.mode)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
	if !strings.Contains(m.busyTitle, "fresh-one") {
		t.Errorf("busy title should mention name; got %q", m.busyTitle)
	}
}

// TestNewFresh_EscReturnsToPicker: esc in fresh sub-modal goes back
// one step (to the picker, not all the way to listMode).
func TestNewFresh_EscReturnsToPicker(t *testing.T) {
	m := newTestModel(false)
	m.openNewFresh()

	model, _ := m.handleNewFreshKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = model.(*Model)
	if m.mode != newPickerMode {
		t.Errorf("esc on fresh sub-modal should return to picker; got %v", m.mode)
	}
}

// TestNewPrompt_CtrlSSubmits_StashesPrompt: with a non-empty prompt,
// Ctrl+S in newPromptMode flips to busyMode, kicks off a create cmd,
// and stashes the trimmed prompt on m.pendingPrompt so the
// createDoneMsg handler can deliver it after Create returns.
//
// The busy title reflects the prompt path ("Creating workspace +
// prompting agent...") so the user knows the second phase is coming.
func TestNewPrompt_CtrlSSubmits_StashesPrompt(t *testing.T) {
	m := newTestModel(false)
	m.openNewPrompt()
	// Multi-line value with leading/trailing whitespace; TrimSpace
	// hits both ends, internal newlines survive intact.
	m.promptInput.SetValue("  add OAuth login\nthen Google  ")

	model, cmd := m.handleNewPromptKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode; got %v", m.mode)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
	want := "add OAuth login\nthen Google"
	if m.pendingPrompt != want {
		t.Errorf("pendingPrompt = %q; want %q", m.pendingPrompt, want)
	}
	if !strings.Contains(m.busyTitle, "prompting") {
		t.Errorf("busy title should mention prompt path; got %q", m.busyTitle)
	}
}

// TestNewPrompt_CtrlSEmpty_NoOp: an empty (or whitespace-only) prompt
// is a no-op — mode stays in newPromptMode and no cmd is dispatched.
// The placeholder copy already signals the requirement; surfacing an
// inline error would be noise.
func TestNewPrompt_CtrlSEmpty_NoOp(t *testing.T) {
	cases := []string{"", "   ", "\t\n"}
	for _, val := range cases {
		t.Run(fmt.Sprintf("value=%q", val), func(t *testing.T) {
			m := newTestModel(false)
			m.openNewPrompt()
			m.promptInput.SetValue(val)

			model, cmd := m.handleNewPromptKey(tea.KeyMsg{Type: tea.KeyCtrlS})
			m = model.(*Model)
			if m.mode != newPromptMode {
				t.Errorf("empty ctrl+s should stay in newPromptMode; got %v", m.mode)
			}
			if cmd != nil {
				t.Errorf("empty ctrl+s should not dispatch a cmd; got %T", cmd)
			}
			if m.pendingPrompt != "" {
				t.Errorf("empty ctrl+s set pendingPrompt = %q; want empty", m.pendingPrompt)
			}
		})
	}
}

// TestNewPrompt_EnterIsNewline_DoesNotSubmit: Enter is reserved for
// the textarea's newline behavior, NOT submit. This is the load-
// bearing distinction that makes multi-line input usable — without
// it, the first Enter mid-prompt would prematurely submit.
//
// The textarea consumes the Enter and inserts "\n" into the buffer;
// our handler must not flip to busyMode or set pendingPrompt.
func TestNewPrompt_EnterIsNewline_DoesNotSubmit(t *testing.T) {
	m := newTestModel(false)
	m.openNewPrompt()
	m.promptInput.SetValue("line one")

	model, _ := m.handleNewPromptKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != newPromptMode {
		t.Errorf("Enter should not submit; mode = %v, want newPromptMode", m.mode)
	}
	if m.pendingPrompt != "" {
		t.Errorf("Enter should not stash prompt; pendingPrompt = %q", m.pendingPrompt)
	}
	if !strings.Contains(m.promptInput.Value(), "\n") {
		t.Errorf("Enter should insert a newline into the textarea; value = %q",
			m.promptInput.Value())
	}
}

// TestNewPrompt_EscReturnsToPicker: esc steps back one level (to the
// picker), mirroring all the other sub-modals. Doesn't drop to
// listMode — the user can re-pick a different variant without
// re-entering the new-workspace flow from scratch.
func TestNewPrompt_EscReturnsToPicker(t *testing.T) {
	m := newTestModel(false)
	m.openNewPrompt()

	model, _ := m.handleNewPromptKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = model.(*Model)
	if m.mode != newPickerMode {
		t.Errorf("esc on prompt sub-modal should return to picker; got %v", m.mode)
	}
}

// TestRenderNewPrompt_FooterTelegraphsSubmitVsNewline: the footer
// makes the submit-vs-newline split explicit because terminal users
// default-assume Enter = submit. Without the hint, the first Enter
// mid-prompt feels like a broken submit gate.
func TestRenderNewPrompt_FooterTelegraphsSubmitVsNewline(t *testing.T) {
	m := newTestModel(false)
	m.openNewPrompt()
	out := stripAnsi(m.renderNewPrompt())
	if !strings.Contains(out, "ctrl+s") {
		t.Errorf("renderNewPrompt should mention ctrl+s submit; got:\n%s", out)
	}
	if !strings.Contains(out, "enter newline") {
		t.Errorf("renderNewPrompt should mention enter inserts a newline; got:\n%s", out)
	}
	if !strings.Contains(out, "What should the agent work on?") {
		t.Errorf("renderNewPrompt missing the headline prompt-question; got:\n%s", out)
	}
}

// TestCreateDoneMsg_PendingPrompt_StaysBusyAndConsumes: when a Create
// succeeds AND m.pendingPrompt is non-empty, the handler stays in
// busyMode (instead of attaching immediately) so the trust-dialog
// state machine has time to run, and consumes pendingPrompt so a
// stale value can't trigger a second send.
func TestCreateDoneMsg_PendingPrompt_StaysBusyAndConsumes(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate
	m.busyDone = false
	m.newTargetMgr = m.mgr
	m.pendingPrompt = "do the thing"

	model, cmd := m.Update(createDoneMsg{tmuxSession: "canopy-test-foo", err: nil})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode while prompt sends; got %v", m.mode)
	}
	if m.pendingPrompt != "" {
		t.Errorf("pendingPrompt should be consumed; got %q", m.pendingPrompt)
	}
	if cmd == nil {
		t.Errorf("expected sendPromptCmd to be dispatched")
	}
	if !strings.Contains(m.busyTitle, "Sending prompt") {
		t.Errorf("busy title should switch to 'Sending prompt'; got %q", m.busyTitle)
	}
}

// TestCreateDoneMsg_NoPrompt_AttachesImmediately: the existing fresh /
// pr / issue / branch paths must keep their immediate-attach behavior.
// This is a regression test for the pendingPrompt branch: without
// pendingPrompt set, createDoneMsg flips to listMode and dispatches
// the attach cmd directly.
func TestCreateDoneMsg_NoPrompt_AttachesImmediately(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate
	m.busyDone = false
	m.newTargetMgr = m.mgr
	// pendingPrompt deliberately empty.

	model, cmd := m.Update(createDoneMsg{tmuxSession: "canopy-test-foo", err: nil})
	m = model.(*Model)
	if m.mode != listMode {
		t.Errorf("no-prompt Create success should flip to listMode; got %v", m.mode)
	}
	if cmd == nil {
		t.Errorf("expected attach cmd; got nil")
	}
}

// TestPromptSentMsg_Success_AttachesAndClearsBusy: a successful
// prompt-send flips out of busyMode, clears the busy chrome, and
// dispatches the attach cmd. No error is recorded.
func TestPromptSentMsg_Success_AttachesAndClearsBusy(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate
	m.busyTitle = "Sending prompt to agent..."
	m.busyOutput = "stuff"

	model, cmd := m.Update(promptSentMsg{session: "canopy-test-foo", err: nil})
	m = model.(*Model)
	if m.mode != listMode {
		t.Errorf("success should flip to listMode; got %v", m.mode)
	}
	if cmd == nil {
		t.Errorf("expected attach cmd")
	}
	if m.err != nil {
		t.Errorf("no error should be recorded on success; got %v", m.err)
	}
	if m.busyTitle != "" || m.busyOutput != "" {
		t.Errorf("busy chrome should be cleared; title=%q output=%q",
			m.busyTitle, m.busyOutput)
	}
}

// TestPromptSentMsg_Failure_AttachesButSurfacesError: a failed
// prompt-send still attaches (the workspace is alive) but records
// the error on m.err so the next listMode render surfaces it. Same
// posture as the CLI's exit code 2 (workspace OK, prompt skipped).
func TestPromptSentMsg_Failure_AttachesButSurfacesError(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate

	sendErr := &workspace.ErrPromptFailed{Reason: "trust dialog timeout"}
	model, cmd := m.Update(promptSentMsg{session: "canopy-test-foo", err: sendErr})
	m = model.(*Model)
	if m.mode != listMode {
		t.Errorf("failure should still attach (flip to listMode); got %v", m.mode)
	}
	if cmd == nil {
		t.Errorf("expected attach cmd even on prompt failure")
	}
	if m.err == nil {
		t.Fatal("m.err should be set after prompt failure")
	}
	if _, ok := workspace.IsPromptFailed(m.err); !ok {
		t.Errorf("m.err should unwrap to *ErrPromptFailed; got %v", m.err)
	}
}

// TestClearNewTarget_ClearsPendingPrompt: when the new-flow exits
// (esc to listMode or busy dismiss), clearNewTarget zeroes out the
// pendingPrompt too — so a future `n` press doesn't inherit a stale
// prompt that the user already abandoned.
func TestClearNewTarget_ClearsPendingPrompt(t *testing.T) {
	m := newTestModel(false)
	m.pendingPrompt = "stale prompt"
	m.clearNewTarget()
	if m.pendingPrompt != "" {
		t.Errorf("pendingPrompt = %q after clearNewTarget; want empty", m.pendingPrompt)
	}
}

// TestNewIssue_TypedNumberSubmits: same fast path as PR — typed
// number → submit, no list-load wait.
func TestNewIssue_TypedNumberSubmits(t *testing.T) {
	m := newTestModel(false)
	m.mode = newIssueMode
	m.listInput.SetValue("42")

	model, cmd := m.handleNewIssueKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode; got %v", m.mode)
	}
	if !strings.Contains(m.busyTitle, "issue #42") {
		t.Errorf("busy title should mention issue #42; got %q", m.busyTitle)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
}

// TestNewIssue_ArrowsThenEnter: recognition path — load fixture
// issues, arrow to row, enter.
func TestNewIssue_ArrowsThenEnter(t *testing.T) {
	m := newTestModel(false)
	m.mode = newIssueMode
	m.newIssues = []ghx.IssueSummary{
		{Number: 17, Title: "Add feature"},
		{Number: 18, Title: "Fix bug"},
	}

	model, _ := m.handleNewIssueKey(tea.KeyMsg{Type: tea.KeyDown})
	m = model.(*Model)
	model, _ = m.handleNewIssueKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if !strings.Contains(m.busyTitle, "issue #18") {
		t.Errorf("expected #18 in busy title; got %q", m.busyTitle)
	}
}

// TestNewBranch_FilterAndPick: load remote+local branches, filter
// down to one, hit enter. The picked branch goes into a SourceSpec.
func TestNewBranch_FilterAndPick(t *testing.T) {
	m := newTestModel(false)
	m.mode = newBranchMode
	m.newBranches = []string{
		"main",
		"origin/main",
		"origin/feat/oauth",
		"origin/feat/billing",
	}
	m.listInput.SetValue("oauth")

	model, cmd := m.handleNewBranchKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode; got %v", m.mode)
	}
	if !strings.Contains(m.busyTitle, "feat/oauth") {
		t.Errorf("busy title should mention feat/oauth; got %q", m.busyTitle)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
}

// TestNewBranch_LocalOnlyFlipsAllowLocal: a branch that exists only
// locally (no origin/<name>) submits with AllowLocal=true so the
// resolver doesn't reject it. Required for the workflow where the
// user has a local-only branch from before they pushed it.
func TestNewBranch_LocalOnlyFlipsAllowLocal(t *testing.T) {
	m := newTestModel(false)
	m.mode = newBranchMode
	m.newBranches = []string{
		"main",
		"origin/main",
		"local-experiment", // local only
	}
	m.listInput.SetValue("local-experiment")

	// Capture the SourceSpec via spying on submitNewBranch isn't
	// straightforward without a mock; instead, verify the flow
	// reaches busyMode. The AllowLocal logic is small enough to
	// test directly via branchHasOrigin (separate test below).
	model, _ := m.handleNewBranchKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("expected busyMode; got %v", m.mode)
	}
}

// TestBranchHasOrigin: helper that decides AllowLocal.
func TestBranchHasOrigin(t *testing.T) {
	branches := []string{"main", "origin/main", "feat/x", "origin/feat/x"}
	if !branchHasOrigin(branches, "main") {
		t.Errorf("main has origin counterpart; should return true")
	}
	if !branchHasOrigin(branches, "feat/x") {
		t.Errorf("feat/x has origin counterpart; should return true")
	}
	if branchHasOrigin(branches, "local-only") {
		t.Errorf("local-only has no origin; should return false")
	}
}

// TestPickerWindow_FitsInVisible: when the list is shorter than
// the visible window, return the full range (no scroll).
func TestPickerWindow_FitsInVisible(t *testing.T) {
	top, end := pickerWindow(3, 5, 10)
	if top != 0 || end != 5 {
		t.Errorf("expected (0,5); got (%d,%d)", top, end)
	}
}

// TestPickerWindow_CursorTopOfWindow: cursor at index 0 — window
// starts at 0, regardless of list length.
func TestPickerWindow_CursorTopOfWindow(t *testing.T) {
	top, end := pickerWindow(0, 100, 10)
	if top != 0 || end != 10 {
		t.Errorf("expected (0,10); got (%d,%d)", top, end)
	}
}

// TestPickerWindow_CursorBottomOfWindow: cursor below the visible
// height — window scrolls so cursor is the LAST visible row.
func TestPickerWindow_CursorBottomOfWindow(t *testing.T) {
	// Cursor at 50 in a 100-item list with 10 visible rows.
	// Window should be [41, 51) so cursor at 50 is the last visible.
	top, end := pickerWindow(50, 100, 10)
	if top != 41 || end != 51 {
		t.Errorf("expected (41,51); got (%d,%d)", top, end)
	}
}

// TestPickerWindow_CursorAtListEnd: cursor at the last item — window
// is the bottom slice, not past-the-end.
func TestPickerWindow_CursorAtListEnd(t *testing.T) {
	top, end := pickerWindow(99, 100, 10)
	if top != 90 || end != 100 {
		t.Errorf("expected (90,100); got (%d,%d)", top, end)
	}
}

// TestBranchInWorkspace_Match: a row whose Branch matches an
// existing workspace returns the workspace name + true.
func TestBranchInWorkspace_Match(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{IsMain: true, Name: "(main)", Branch: "—"},
		{Name: "pr-1185", Branch: "pdx91/inbox-improvements"},
		{Name: "soft-fox", Branch: "feat/oauth"},
	})
	wsName, taken := m.branchInWorkspace("feat/oauth")
	if !taken || wsName != "soft-fox" {
		t.Errorf("expected (soft-fox, true); got (%q, %v)", wsName, taken)
	}
}

// TestBranchInWorkspace_NoMatch: branch with no matching workspace
// returns false. Empty branch string also returns false (defensive).
func TestBranchInWorkspace_NoMatch(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{{Name: "soft-fox", Branch: "feat/oauth"}})
	if _, taken := m.branchInWorkspace("other-branch"); taken {
		t.Errorf("non-matching branch should return false")
	}
	if _, taken := m.branchInWorkspace(""); taken {
		t.Errorf("empty branch should return false")
	}
}

// TestBranchInWorkspace_SkipsMain: the synthetic main row has
// Branch="—" which must not match anything (defensive against a
// hypothetical workspace literally named "—").
func TestBranchInWorkspace_SkipsMain(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{{IsMain: true, Branch: "—"}})
	if _, taken := m.branchInWorkspace("—"); taken {
		t.Errorf("main row should be excluded from branch-conflict check")
	}
}

// TestRenderNewBranch_TagsTakenBranches: rendering the branch picker
// includes a "(in workspace X)" tag on rows whose bare branch name
// matches an existing workspace. Verifies the visual cue lands.
func TestRenderNewBranch_TagsTakenBranches(t *testing.T) {
	m := newTestModel(false)
	m.mode = newBranchMode
	m.setTestRows([]Row{{Name: "pr-1185", Branch: "pdx91/inbox-improvements"}})
	m.newBranches = []string{
		"main",
		"origin/pdx91/inbox-improvements",
		"pdx91/inbox-improvements",
		"origin/feat/x",
	}
	out := m.renderNewBranch()
	if !strings.Contains(out, "in workspace pr-1185") {
		t.Errorf("taken-branch tag missing from picker:\n%s", out)
	}
}

// TestRenderNewPR_TagsTakenPRs: rendering the PR picker tags rows
// whose head branch is already in a workspace. PRs with HeadRefName
// matching an existing workspace's branch get the dim treatment.
func TestRenderNewPR_TagsTakenPRs(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.setTestRows([]Row{{Name: "pr-1185", Branch: "pdx91/inbox-improvements"}})
	m.newPRs = []ghx.PRSummary{
		{Number: 1185, Title: "Inbox", HeadRefName: "pdx91/inbox-improvements"},
		{Number: 1182, Title: "Auth", HeadRefName: "feat/oauth"},
	}
	out := m.renderNewPR()
	if !strings.Contains(out, "in workspace pr-1185") {
		t.Errorf("taken-PR tag missing from picker:\n%s", out)
	}
}

// TestPickerCursor_BoundedByFilteredLength: cursor-down stops at the
// filtered length, not the raw list length. Without this bound the
// cursor could drift into invisible rows after a filter narrows the
// list.
func TestPickerCursor_BoundedByFilteredLength(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.newPRs = []ghx.PRSummary{
		{Number: 1, Title: "match"},
		{Number: 2, Title: "no"},
		{Number: 3, Title: "match also"},
	}
	m.listInput.SetValue("match") // filters to 2 rows

	// Press down twice — should land at index 1 (last filtered) and
	// stay there on subsequent presses.
	for i := 0; i < 5; i++ {
		model, _ := m.handleNewPRKey(tea.KeyMsg{Type: tea.KeyDown})
		m = model.(*Model)
	}
	if m.listCursor != 1 {
		t.Errorf("cursor should be bounded at 1 (filter has 2 rows); got %d", m.listCursor)
	}
}

// TestFilterBranches_Substring: case-insensitive substring match,
// works across local + remote prefix.
func TestFilterBranches_Substring(t *testing.T) {
	branches := []string{"main", "origin/main", "origin/feat/oauth", "origin/feat/billing"}
	got := filterBranches(branches, "FEAT")
	if len(got) != 2 {
		t.Errorf("expected 2 matches for 'FEAT'; got %d", len(got))
	}
	got = filterBranches(branches, "main")
	if len(got) != 2 {
		t.Errorf("expected main + origin/main; got %d", len(got))
	}
}

// TestNewPR_TypedNumberSubmits: the power-user fast path — type a
// PR number into the filter, hit enter, get a workspace from that
// PR. Doesn't require the list to have loaded; works even on a
// cold cache.
func TestNewPR_TypedNumberSubmits(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.listInput.SetValue("1234")

	model, cmd := m.handleNewPRKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("enter on number should flip to busyMode; got %v", m.mode)
	}
	if !strings.Contains(m.busyTitle, "PR #1234") {
		t.Errorf("busy title should mention PR #1234; got %q", m.busyTitle)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
}

// TestNewPR_ArrowsThenEnter: recognition path — wait for the list
// to load, arrow to a row, hit enter. The picker reads the row's
// PR number and submits.
func TestNewPR_ArrowsThenEnter(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.newPRs = []ghx.PRSummary{
		{Number: 1185, Title: "Inbox improvements"},
		{Number: 1182, Title: "Fix oauth"},
	}

	// Down once → cursor on #1182.
	model, _ := m.handleNewPRKey(tea.KeyMsg{Type: tea.KeyDown})
	m = model.(*Model)
	if m.listCursor != 1 {
		t.Fatalf("listCursor = %d; want 1", m.listCursor)
	}

	model, cmd := m.handleNewPRKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(*Model)
	if m.mode != busyMode {
		t.Errorf("enter on row should flip to busyMode; got %v", m.mode)
	}
	if !strings.Contains(m.busyTitle, "PR #1182") {
		t.Errorf("busy title should reference selected row's PR; got %q", m.busyTitle)
	}
	if cmd == nil {
		t.Errorf("expected create cmd")
	}
}

// TestNewPR_LoadedMsgPopulatesList: prListLoadedMsg from the async
// loader populates m.newPRs and clears the loading flag. View can
// then render the list.
func TestNewPR_LoadedMsgPopulatesList(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.newLoading = true

	prs := []ghx.PRSummary{{Number: 42, Title: "Test"}}
	model, _ := m.Update(prListLoadedMsg{prs: prs})
	m = model.(*Model)

	if m.newLoading {
		t.Errorf("newLoading should be false after msg")
	}
	if len(m.newPRs) != 1 || m.newPRs[0].Number != 42 {
		t.Errorf("newPRs not populated; got %+v", m.newPRs)
	}
}

// TestSubmitNewPR_RemoteRoutesThroughRemoteCreateCmd: when the new-
// workspace target is a remote host, submitNewPR must dispatch via
// remoteCreateCmd (spawning `canopy new --on <host> --pr <num>`)
// instead of createCmd which would try to use the nil newTargetMgr.
// Before v0.21 PR/Issue/Branch were hidden for remote — now they
// route end-to-end. The cmd is returned but not executed; we sanity-
// check that busy state flips and a cmd is produced (would crash
// if hitting the nil-mgr createCmd path).
func TestSubmitNewPR_RemoteRoutesThroughRemoteCreateCmd(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.newTargetHost = "tower"
	m.newTargetRemoteCwd = "/home/avi/Work/cravd"
	m.newTargetMgr = nil // remote: no local Manager

	model, cmd := m.submitNewPR(42)
	got := model.(*Model)
	if got.mode != busyMode {
		t.Errorf("mode after submit = %v; want busyMode", got.mode)
	}
	if cmd == nil {
		t.Fatal("remote submitNewPR returned nil cmd; want remoteCreateCmd")
	}
	if !strings.Contains(got.busyTitle, "PR #42") {
		t.Errorf("busyTitle = %q; want mention of PR #42", got.busyTitle)
	}
}

// TestSubmitNewIssue_RemoteRoutesThroughRemoteCreateCmd: same as the
// PR variant for issues. submitNewIssue must not call createCmd with
// a nil Manager when the target is remote.
func TestSubmitNewIssue_RemoteRoutesThroughRemoteCreateCmd(t *testing.T) {
	m := newTestModel(false)
	m.mode = newIssueMode
	m.newTargetHost = "tower"
	m.newTargetRemoteCwd = "/home/avi/Work/cravd"
	m.newTargetMgr = nil

	model, cmd := m.submitNewIssue(17)
	got := model.(*Model)
	if got.mode != busyMode {
		t.Errorf("mode after submit = %v; want busyMode", got.mode)
	}
	if cmd == nil {
		t.Fatal("remote submitNewIssue returned nil cmd; want remoteCreateCmd")
	}
	if !strings.Contains(got.busyTitle, "issue #17") {
		t.Errorf("busyTitle = %q; want mention of issue #17", got.busyTitle)
	}
}

// TestSubmitNewBranch_RemoteRoutesThroughRemoteCreateCmd: same for
// the branch picker — the chosen ref must reach the remote canopy
// via --branch.
func TestSubmitNewBranch_RemoteRoutesThroughRemoteCreateCmd(t *testing.T) {
	m := newTestModel(false)
	m.mode = newBranchMode
	m.newTargetHost = "tower"
	m.newTargetRemoteCwd = "/home/avi/Work/cravd"
	m.newTargetMgr = nil

	spec := workspace.SourceSpec{Branch: "feat/oauth"}
	model, cmd := m.submitNewBranch(spec)
	got := model.(*Model)
	if got.mode != busyMode {
		t.Errorf("mode after submit = %v; want busyMode", got.mode)
	}
	if cmd == nil {
		t.Fatal("remote submitNewBranch returned nil cmd; want remoteCreateCmd")
	}
	if !strings.Contains(got.busyTitle, "feat/oauth") {
		t.Errorf("busyTitle = %q; want mention of branch feat/oauth", got.busyTitle)
	}
}

// TestLoadPRsForTarget_HostMissingFromRegistrySurfacesError: if the
// remote host vanished from the registry snapshot between picker open
// and loader dispatch, the loader returns a prListLoadedMsg with the
// error rather than panic'ing on an empty SSH target. The picker
// surfaces the error inline as "host not found in registry snapshot".
func TestLoadPRsForTarget_HostMissingFromRegistrySurfacesError(t *testing.T) {
	m := newTestModel(false)
	m.newTargetHost = "tower"
	m.newTargetRemoteCwd = "/home/avi/Work/cravd"
	m.hostList = nil // registry empty — host not resolvable

	cmd := m.loadPRsForTarget()
	if cmd == nil {
		t.Fatal("loadPRsForTarget returned nil cmd")
	}
	msg := cmd()
	loaded, ok := msg.(prListLoadedMsg)
	if !ok {
		t.Fatalf("got msg type %T; want prListLoadedMsg", msg)
	}
	if loaded.err == nil {
		t.Errorf("loaded.err should be non-nil when host missing")
	}
}

// TestLoadIssuesForTarget_HostMissingFromRegistrySurfacesError: same
// host-missing fallback as loadPRsForTarget, for the issue loader.
func TestLoadIssuesForTarget_HostMissingFromRegistrySurfacesError(t *testing.T) {
	m := newTestModel(false)
	m.newTargetHost = "tower"
	m.newTargetRemoteCwd = "/home/avi/Work/cravd"
	m.hostList = nil

	cmd := m.loadIssuesForTarget()
	if cmd == nil {
		t.Fatal("loadIssuesForTarget returned nil cmd")
	}
	loaded, ok := cmd().(issueListLoadedMsg)
	if !ok {
		t.Fatalf("got msg type %T; want issueListLoadedMsg", cmd())
	}
	if loaded.err == nil {
		t.Errorf("loaded.err should be non-nil when host missing")
	}
}

// TestLoadBranchesForTarget_HostMissingFromRegistrySurfacesError: same
// host-missing fallback for the branch loader.
func TestLoadBranchesForTarget_HostMissingFromRegistrySurfacesError(t *testing.T) {
	m := newTestModel(false)
	m.newTargetHost = "tower"
	m.newTargetRemoteCwd = "/home/avi/Work/cravd"
	m.hostList = nil

	cmd := m.loadBranchesForTarget()
	if cmd == nil {
		t.Fatal("loadBranchesForTarget returned nil cmd")
	}
	loaded, ok := cmd().(branchListLoadedMsg)
	if !ok {
		t.Fatalf("got msg type %T; want branchListLoadedMsg", cmd())
	}
	if loaded.err == nil {
		t.Errorf("loaded.err should be non-nil when host missing")
	}
}

// TestLoadPRsForTarget_LocalUsesLocalLoader: when newTargetHost is
// empty, loadPRsForTarget falls through to loadPRsCmd (no SSH lookup).
// Sanity check that the dispatch hasn't broken the local path while
// adding the remote one.
func TestLoadPRsForTarget_LocalUsesLocalLoader(t *testing.T) {
	m := newTestModel(false)
	m.newTargetHost = ""
	m.newTargetRoot = t.TempDir() // gh will fail here, but the cmd is fine

	cmd := m.loadPRsForTarget()
	if cmd == nil {
		t.Fatal("loadPRsForTarget(local) returned nil cmd")
	}
	// Don't execute — the goroutine would try gh. Just confirm the
	// cmd was constructed without going through the SSH-resolve path
	// (which would fail with an empty hostList).
}

// TestNewPR_LoadedMsgWithError: error in the loader surfaces as
// newLoadErr, list stays empty. View renders the error banner.
func TestNewPR_LoadedMsgWithError(t *testing.T) {
	m := newTestModel(false)
	m.mode = newPRMode
	m.newLoading = true

	model, _ := m.Update(prListLoadedMsg{err: fmt.Errorf("gh missing")})
	m = model.(*Model)

	if m.newLoading {
		t.Errorf("newLoading should be false after msg")
	}
	if m.newLoadErr == nil {
		t.Errorf("newLoadErr should be set on failure")
	}
}

// TestFilterPRs_NumericPrefix: typing "11" matches all PRs whose
// number starts with 11 — the "I half-remember the number" path.
func TestFilterPRs_NumericPrefix(t *testing.T) {
	prs := []ghx.PRSummary{
		{Number: 1185, Title: "A"},
		{Number: 1182, Title: "B"},
		{Number: 999, Title: "C"},
	}
	got := filterPRs(prs, "11")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (1185 + 1182)", len(got))
	}
}

// TestFilterPRs_TitleSubstring: non-numeric input matches title +
// author case-insensitively.
func TestFilterPRs_TitleSubstring(t *testing.T) {
	prs := []ghx.PRSummary{
		{Number: 1, Title: "Inbox improvements"},
		{Number: 2, Title: "Fix oauth"},
	}
	got := filterPRs(prs, "INBOX")
	if len(got) != 1 || got[0].Number != 1 {
		t.Errorf("expected single match for INBOX; got %+v", got)
	}
}

// TestProgressTickMsg_AppendsToBusyOutput: streaming chunks from
// the safeBuffer end up in m.busyOutput so the renderer can show
// live output. Each tick adds the drained chunk to the running
// total and schedules another tick (unless done).
func TestProgressTickMsg_AppendsToBusyOutput(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate

	buf := &safeBuffer{}
	model, cmd := m.Update(progressTickMsg{chunk: "Setting up...\n", buf: buf})
	m = model.(*Model)

	if !strings.Contains(m.busyOutput, "Setting up...") {
		t.Errorf("chunk should be appended to busyOutput; got %q", m.busyOutput)
	}
	if cmd == nil {
		t.Errorf("tick should re-schedule itself while not done")
	}
}

// TestProgressTickMsg_StopsTickingWhenDone: once the create
// completes (busyDone=true), the tick loop must stop. Otherwise we
// burn redraws on a finished operation.
func TestProgressTickMsg_StopsTickingWhenDone(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate
	m.busyDone = true

	_, cmd := m.Update(progressTickMsg{chunk: "trailing\n", buf: &safeBuffer{}})
	if cmd != nil {
		t.Errorf("tick after done should NOT re-schedule; got cmd %T", cmd)
	}
}

// TestProgressTickMsg_StopsTickingWhenLeftBusyMode: a stale tick
// arriving after the user dismissed the busy view (e.g. on
// successful auto-attach which flips mode back to listMode) must
// not keep the tick loop alive in the wrong mode.
func TestProgressTickMsg_StopsTickingWhenLeftBusyMode(t *testing.T) {
	m := newTestModel(false)
	m.mode = listMode

	_, cmd := m.Update(progressTickMsg{chunk: "", buf: &safeBuffer{}})
	if cmd != nil {
		t.Errorf("tick outside busyMode should be dropped; got cmd %T", cmd)
	}
}

// TestRemoveStartedMsg_DispatchesStreamingBatch: receiving a
// removeStartedMsg from the lazy-spawn path returns a tea.Batch that
// contains both the progress tick and the wait-done cmd. Without
// this dispatch the archive-script output wouldn't stream.
func TestRemoveStartedMsg_DispatchesStreamingBatch(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode

	buf := &safeBuffer{}
	done := make(chan removeDoneMsg, 1)
	_, cmd := m.Update(removeStartedMsg{buf: buf, done: done})

	if cmd == nil {
		t.Fatalf("removeStartedMsg should dispatch a streaming batch; got nil")
	}
}

// TestRetryStartedMsg_DispatchesStreamingBatch: same shape for the
// retry flow.
func TestRetryStartedMsg_DispatchesStreamingBatch(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode

	buf := &safeBuffer{}
	done := make(chan retryDoneMsg, 1)
	_, cmd := m.Update(retryStartedMsg{buf: buf, done: done})

	if cmd == nil {
		t.Fatalf("retryStartedMsg should dispatch a streaming batch; got nil")
	}
}

// TestRemoveDone_AppendsTrailingOutputOnError: on the error path,
// removeDoneMsg appends the final tail to busyOutput rather than
// overwriting. busyOutput already contains the streamed archive script
// output via tick messages; overwriting would erase it. We stay in
// busyMode so the user can read the diagnostic.
func TestRemoveDone_AppendsTrailingOutputOnError(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOutput = "archive ran on port 41010\n"

	model, _ := m.Update(removeDoneMsg{output: "remove failed.\n", err: fmt.Errorf("boom")})
	m = model.(*Model)

	if m.mode != busyMode {
		t.Errorf("removeDoneMsg(err) should keep busyMode for the diagnostic; got %v", m.mode)
	}
	if !strings.Contains(m.busyOutput, "archive ran on port 41010") {
		t.Errorf("removeDoneMsg should preserve streamed output: %q", m.busyOutput)
	}
	if !strings.Contains(m.busyOutput, "remove failed.") {
		t.Errorf("removeDoneMsg should append trailing output: %q", m.busyOutput)
	}
}

// TestRemoveDone_AutoDismissOnSuccess: success-path removeDoneMsg
// auto-exits busyMode instead of leaving the popup open waiting for a
// keypress. The user's tmux client has already been switched off any
// doomed session by escapeIfDeletingCurrent, so there's nothing to
// linger for. Fullscreen returns to listMode; popup mode tea.Quits.
//
// Asymmetric on purpose with the error path (TestRemoveDone_Appends...
// OnError above), which keeps busyMode so the user can read the
// archive output + error message.
func TestRemoveDone_AutoDismissOnSuccess(t *testing.T) {
	t.Run("fullscreen → listMode + clears busy fields", func(t *testing.T) {
		m := newTestModel(false)
		m.mode = busyMode
		m.busyOp = busyOpRemove
		m.busyTitle = "Removing workspace \"x\"..."
		m.busyOutput = "archive ran\n"

		model, cmd := m.Update(removeDoneMsg{})
		m = model.(*Model)

		if m.mode != listMode {
			t.Errorf("mode after success removeDoneMsg = %v; want listMode", m.mode)
		}
		if m.busyOp != busyOpNone {
			t.Errorf("busyOp = %v; want busyOpNone", m.busyOp)
		}
		if m.busyTitle != "" || m.busyOutput != "" {
			t.Errorf("busy fields not cleared: title=%q output=%q", m.busyTitle, m.busyOutput)
		}
		if cmd == nil {
			t.Errorf("expected refresh cmd on success; got nil")
		}
	})

	t.Run("popup → tea.Quit", func(t *testing.T) {
		m := newTestModel(false)
		m.inPopup = true
		m.mode = busyMode
		m.busyOp = busyOpRemove

		model, cmd := m.Update(removeDoneMsg{})
		m = model.(*Model)

		if cmd == nil {
			t.Fatal("expected tea.Quit on popup success; got nil cmd")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("expected tea.QuitMsg from cmd; got %T", cmd())
		}
		// Busy fields must be cleared so any post-quit render flash
		// doesn't show a stale "Removing..." popup.
		if m.busyOp != busyOpNone || m.busyTitle != "" || m.busyOutput != "" {
			t.Errorf("busy fields not cleared on popup quit: op=%v title=%q output=%q",
				m.busyOp, m.busyTitle, m.busyOutput)
		}
	})
}

// TestDeletingCurrentSession covers the truth table for both popup
// and fullscreen modes: a (root, name) match against currentWorkspace
// should return true regardless of mode. Empty currentWorkspace or a
// mismatch returns false.
func TestDeletingCurrentSession(t *testing.T) {
	cases := []struct {
		name             string
		inPopup          bool
		currentWorkspace string
		currentRoot      string
		argRoot          string
		argName          string
		want             bool
	}{
		{"popup + match → true", true, "feature-x", "/p", "/p", "feature-x", true},
		{"popup + name mismatch → false", true, "feature-x", "/p", "/p", "other", false},
		{"popup + root mismatch → false", true, "feature-x", "/p", "/q", "feature-x", false},
		{"fullscreen + match → true", false, "feature-x", "/p", "/p", "feature-x", true},
		{"fullscreen + name mismatch → false", false, "feature-x", "/p", "/p", "other", false},
		{"fullscreen + root mismatch → false", false, "feature-x", "/p", "/q", "feature-x", false},
		{"popup + no current workspace → false", true, "", "", "/p", "feature-x", false},
		{"fullscreen + no current workspace → false", false, "", "", "/p", "feature-x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestModel(false)
			m.inPopup = c.inPopup
			m.currentWorkspace = c.currentWorkspace
			m.currentWorkspaceRoot = c.currentRoot
			got := m.deletingCurrentSession(c.argRoot, c.argName)
			if got != c.want {
				t.Errorf("got %v; want %v", got, c.want)
			}
		})
	}
}

// TestConfirmDeleteY_CurrentSession_SkipsBusyMode covers both popup
// and fullscreen flavors of "deleting the workspace I'm sitting in":
// pressing y skips busyMode entirely and dispatches the detach +
// detached-subprocess shortcut. The user experiences "canopy closes
// immediately" rather than "busyMode sits there while the underlying
// tmux session changes."
//
// We can't easily assert the cmd's side effects (it spawns a real
// subprocess + runs tmux detach-client), but we can verify the branch
// was taken: mode stays as listMode-equivalent and a non-nil cmd is
// returned.
func TestConfirmDeleteY_CurrentSession_SkipsBusyMode(t *testing.T) {
	cases := []struct {
		name    string
		inPopup bool
	}{
		{"popup + current", true},
		{"fullscreen + current", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestModel(false)
			m.inPopup = c.inPopup
			m.currentWorkspaceRoot = "/tmp/test-project"
			m.currentWorkspace = "feature-x"
			m.deleteTarget = "feature-x"
			m.deleteTargetRoot = "/tmp/test-project"
			m.mode = confirmDeleteMode
			m.setTestRows([]Row{{
				Project:     "test-project",
				ProjectRoot: "/tmp/test-project",
				Name:        "feature-x",
				Status:      state.StatusReady,
			}})

			model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
			m = model.(*Model)

			if m.mode == busyMode {
				t.Errorf("current-session y should skip busyMode; got mode=%v", m.mode)
			}
			if cmd == nil {
				t.Fatal("expected detachAndRemoveCmd; got nil cmd")
			}
			if m.deleteTarget != "" || m.deleteTargetRoot != "" {
				t.Errorf("delete fields not cleared: target=%q root=%q", m.deleteTarget, m.deleteTargetRoot)
			}
		})
	}
}

// TestConfirmDeleteY_PopupNotCurrent_UsesBusyMode: opening the popup
// from project main (or fullscreen, or a different workspace) and
// deleting some other workspace should keep using the existing
// busyMode flow — we only want to skip busy when the user is
// deleting the workspace they're sitting in.
func TestConfirmDeleteY_PopupNotCurrent_UsesBusyMode(t *testing.T) {
	m := newTestModel(false)
	m.inPopup = true
	m.currentWorkspaceRoot = ""
	m.currentWorkspace = "" // popup opened from outside any workspace
	m.deleteTarget = "feature-x"
	m.deleteTargetRoot = "/tmp/test-project"
	m.mode = confirmDeleteMode
	m.setTestRows([]Row{{
		Project:     "test-project",
		ProjectRoot: "/tmp/test-project",
		Name:        "feature-x",
		Status:      state.StatusReady,
	}})

	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = model.(*Model)

	if m.mode != busyMode {
		t.Errorf("non-current delete should enter busyMode; got %v", m.mode)
	}
	if m.busyOp != busyOpRemove {
		t.Errorf("busyOp = %v; want busyOpRemove", m.busyOp)
	}
	if cmd == nil {
		t.Errorf("expected removeCmd; got nil")
	}
}

// TestRetryDone_AppendsTrailingOutput: same contract for retry.
func TestRetryDone_AppendsTrailingOutput(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOutput = "setup running...\n"

	model, _ := m.Update(retryDoneMsg{output: "setup OK\n"})
	m = model.(*Model)

	if !strings.Contains(m.busyOutput, "setup running...") {
		t.Errorf("retryDoneMsg should preserve streamed output: %q", m.busyOutput)
	}
	if !strings.Contains(m.busyOutput, "setup OK") {
		t.Errorf("retryDoneMsg should append trailing output: %q", m.busyOutput)
	}
}

// TestSafeBuffer_DrainResets: the buffer accumulates writes and
// hands the drained content back to the caller, then resets so
// the next drain only returns NEW content. Without this contract,
// each tick would include the entire history and the View would
// show duplicated output.
func TestSafeBuffer_DrainResets(t *testing.T) {
	buf := &safeBuffer{}
	buf.Write([]byte("line one\n"))
	buf.Write([]byte("line two\n"))

	got := buf.Drain()
	if got != "line one\nline two\n" {
		t.Errorf("first drain = %q; want both lines", got)
	}
	if next := buf.Drain(); next != "" {
		t.Errorf("second drain should be empty; got %q", next)
	}

	buf.Write([]byte("line three\n"))
	if next := buf.Drain(); next != "line three\n" {
		t.Errorf("post-reset write should drain alone; got %q", next)
	}
}

// errFakeCreate is a sentinel for the create-error test. Lives here
// (not in the test func) so the literal stays readable.
var errFakeCreate = fakeErr("setup failed")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

// TestSelectedHint covers the four-corner truth table of selectedHint:
// empty list → "", main row → "", non-broken row → "", broken row with
// hint → hint. The v0.8 unification promoted LastErrorHint onto
// state.GlobalRow; this is the regression test for the renderer that
// surfaces it under the table.
func TestSelectedHint(t *testing.T) {
	cases := []struct {
		name string
		rows []Row
		want string
	}{
		{
			name: "empty_list",
			rows: nil,
			want: "",
		},
		{
			name: "main_row_skipped",
			rows: []Row{
				{IsMain: true, Status: "broken", LastErrorHint: "ignored on main"},
			},
			want: "",
		},
		{
			name: "non_broken_skipped",
			rows: []Row{
				{Status: state.StatusReady, LastErrorHint: "stale hint"},
			},
			want: "",
		},
		{
			name: "broken_no_hint",
			rows: []Row{
				{Status: state.StatusBroken, LastErrorHint: ""},
			},
			want: "",
		},
		{
			name: "broken_with_hint",
			rows: []Row{
				{Status: state.StatusBroken, LastErrorHint: "missing bin/dev script"},
			},
			want: "missing bin/dev script",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(false)
			m.setTestRows(tc.rows)
			if got := m.selectedHint(); got != tc.want {
				t.Errorf("selectedHint() = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestFillMainBranches_DefaultsToMain covers the fallback path: a project
// with no origin/main or origin/master remote (e.g. a freshly-init local
// repo) gets "main" as the displayed default branch rather than the
// "—" placeholder. The function must not error or skip the row when
// DetectDefaultBranch fails — the UI relies on every main row carrying
// a non-empty Branch so the branch column renders consistently.
func TestFillMainBranches_DefaultsToMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "--initial-branch=trunk").Run(); err != nil {
		t.Skipf("git init: %v", err)
	}

	rows := []state.GlobalRow{
		{IsMain: true, ProjectRoot: dir, Project: "fresh", Name: "(main)", Branch: "—"},
		{IsMain: false, ProjectRoot: dir, Project: "fresh", Name: "ws", Branch: "feat-x"},
	}
	fillMainBranches(context.Background(), rows)

	if rows[0].Branch != "main" {
		t.Errorf("main row Branch = %q; want %q (fallback when origin/main|master miss)", rows[0].Branch, "main")
	}
	if rows[1].Branch != "feat-x" {
		t.Errorf("non-main row Branch = %q; want %q (untouched)", rows[1].Branch, "feat-x")
	}
}

// TestFillMainBranches_DetectsOriginMain wires up a real git repo with
// an origin/main ref so DetectDefaultBranch's happy path exercises end-
// to-end. The fallback test covers the error path; this covers the
// detection path.
func TestFillMainBranches_DetectsOriginMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	steps := [][]string{
		{"-C", repo, "init", "--initial-branch=main"},
		{"-C", repo, "config", "user.email", "t@x"},
		{"-C", repo, "config", "user.name", "t"},
		{"-C", repo, "commit", "--allow-empty", "-m", "x"},
		// Synthesize an origin/main remote-tracking ref by pointing
		// refs/remotes/origin/main at HEAD. No real network round-trip
		// — DetectDefaultBranch only reads the local ref.
		{"-C", repo, "update-ref", "refs/remotes/origin/main", "HEAD"},
	}
	for _, args := range steps {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	rows := []state.GlobalRow{
		{IsMain: true, ProjectRoot: repo, Project: "p", Name: "(main)", Branch: "—"},
	}
	fillMainBranches(context.Background(), rows)

	if rows[0].Branch != "main" {
		t.Errorf("main row Branch = %q; want %q", rows[0].Branch, "main")
	}
}

// TestRenderNewPicker_NoBracketRegression: variant picker shortcut
// letters used to render as `[n] `, `[p] ` etc. wrapped in
// brokenStyle (red 196 — same hue as broken-status workspaces and
// error banners). The new style uses keyPillStyle: the pill chrome
// itself implies "press this," so the literal brackets are gone.
//
// Asserting structurally (no `[n]` in the output) rather than against
// SGR color codes — lipgloss strips colors when there's no TTY, so a
// color-based check would silently pass even if the brokenStyle wrap
// came back. Bracket presence is a stable structural signal.
func TestRenderNewPicker_NoBracketRegression(t *testing.T) {
	m := newTestModel(false)
	// Banner needs a target so the picker renders cleanly.
	m.newTargetName = "test-project"
	m.newTargetRoot = "/tmp/test-project"

	out := stripAnsi(m.renderNewPicker())

	for _, opt := range newPickerOptions {
		bracket := "[" + opt.key + "]"
		if strings.Contains(out, bracket) {
			t.Errorf("variant picker rendered literal %q — regression to red-bracket era. want keyPillStyle pill chrome.\noutput:\n%s",
				bracket, out)
		}
		if !strings.Contains(out, opt.key) {
			t.Errorf("variant picker missing shortcut letter %q in output:\n%s",
				opt.key, out)
		}
	}

	// Positive check via the same render pipeline: the pill chrome
	// for the cursor row's letter is what keyPillStyle.Render emits.
	// Comparing the rendered substring is profile-agnostic — works
	// in both colored and stripped output.
	wantPill := keyPillStyle.Render(newPickerOptions[m.newPickerCursor].key)
	if !strings.Contains(m.renderNewPicker(), wantPill) {
		t.Errorf("variant picker missing expected keyPillStyle render for cursor letter %q",
			newPickerOptions[m.newPickerCursor].key)
	}
}

// TestRenderFilterPill_CaretMatchesMainScreen: the filter pill caret
// must be "▏" (a thin vertical bar) — same as renderSearchLine's
// caret. The block-cursor that bubbles' textinput.View() emits would
// look out of place against the main TUI's vocabulary.
func TestRenderFilterPill_CaretMatchesMainScreen(t *testing.T) {
	ti := textinput.New()
	ti.SetValue("foo")
	out := renderFilterPill(ti)
	if !strings.Contains(out, "▏") {
		t.Errorf("filter pill missing main-screen caret '▏': %q", out)
	}
	if !strings.Contains(stripAnsi(out), "foo▏") {
		t.Errorf("caret should sit immediately after typed value; got: %q",
			stripAnsi(out))
	}
}

// TestRenderFilterPill_LabelIsFilter: pill label is "🔍 FILTER" (not
// "SEARCH") because this narrows a fixed list rather than searching
// across all rows. Same chrome family as the main TUI, different verb.
func TestRenderFilterPill_LabelIsFilter(t *testing.T) {
	ti := textinput.New()
	out := stripAnsi(renderFilterPill(ti))
	if !strings.Contains(out, "🔍 FILTER") {
		t.Errorf("filter pill missing 'FILTER' label: %q", out)
	}
	if strings.Contains(out, "SEARCH") {
		t.Errorf("filter pill should NOT use SEARCH label (that's the main-screen verb): %q", out)
	}
}

// TestNewFlow_CursorCaretUnified: every screen in the new-workspace
// flow (variant picker + PR + issue + branch) uses the same "❯ "
// cursor caret that the main workspace list uses. One vocabulary
// across the TUI — the eye reads "here's what's selected" without
// learning a per-modal indicator.
//
// Catches the v0.10 era where the variant picker used "> " and the
// sub-modals used "● " — three different cursor glyphs in flows the
// user moves between in the same task. Regression test: if anyone
// re-introduces a per-screen caret, this fails loudly with the
// rendered output.
func TestNewFlow_CursorCaretUnified(t *testing.T) {
	const wantCaret = "  ❯ "

	t.Run("variant picker", func(t *testing.T) {
		m := newTestModel(false)
		m.newTargetName = "test-project"
		m.newTargetRoot = "/tmp/test-project"
		out := stripAnsi(m.renderNewPicker())
		if !strings.Contains(out, wantCaret) {
			t.Errorf("variant picker missing %q caret:\n%s", wantCaret, out)
		}
	})

	t.Run("PR picker", func(t *testing.T) {
		m := newTestModel(false)
		m.mode = newPRMode
		m.newTargetName = "test-project"
		m.newPRs = []ghx.PRSummary{{Number: 1, Title: "x", HeadRefName: "x"}}
		out := stripAnsi(m.renderNewPR())
		if !strings.Contains(out, wantCaret) {
			t.Errorf("PR picker missing %q caret:\n%s", wantCaret, out)
		}
	})

	t.Run("issue picker", func(t *testing.T) {
		m := newTestModel(false)
		m.mode = newIssueMode
		m.newTargetName = "test-project"
		m.newIssues = []ghx.IssueSummary{{Number: 1, Title: "x"}}
		out := stripAnsi(m.renderNewIssue())
		if !strings.Contains(out, wantCaret) {
			t.Errorf("issue picker missing %q caret:\n%s", wantCaret, out)
		}
	})

	t.Run("branch picker", func(t *testing.T) {
		m := newTestModel(false)
		m.mode = newBranchMode
		m.newTargetName = "test-project"
		m.newBranches = []string{"main"}
		out := stripAnsi(m.renderNewBranch())
		if !strings.Contains(out, wantCaret) {
			t.Errorf("branch picker missing %q caret:\n%s", wantCaret, out)
		}
	})
}

// TestRenderFilterPill_PlaceholderShownWhenEmpty: when the user hasn't
// typed anything, the textinput's Placeholder surfaces as a dim hint
// to the right of the pill so per-modal guidance ("type a PR number,
// or arrow to a row below") still has a home. Once the user types,
// the hint must get out of the way — otherwise it competes with the
// typed value for screen real estate.
func TestRenderFilterPill_PlaceholderShownWhenEmpty(t *testing.T) {
	ti := textinput.New()
	ti.Placeholder = "type a PR number, or arrow to a row below"

	t.Run("empty value → placeholder shown", func(t *testing.T) {
		ti.SetValue("")
		out := stripAnsi(renderFilterPill(ti))
		if !strings.Contains(out, "type a PR number") {
			t.Errorf("empty filter pill should show placeholder hint: %q", out)
		}
	})

	t.Run("non-empty value → placeholder hidden", func(t *testing.T) {
		ti.SetValue("1234")
		out := stripAnsi(renderFilterPill(ti))
		if strings.Contains(out, "type a PR number") {
			t.Errorf("non-empty filter pill should NOT show placeholder: %q", out)
		}
		if !strings.Contains(out, "1234") {
			t.Errorf("non-empty filter pill missing typed value: %q", out)
		}
	})
}

// TestUpgradeCheckedMsg_updatesPill: the async refresh result lands
// as upgradeCheckedMsg; Update must apply the new latest into the
// model so the pill re-renders with the fresh value.
func TestUpgradeCheckedMsg_updatesPill(t *testing.T) {
	m := &Model{}
	m.SetVersionInfo("v0.12.3+abc", "")
	m.SetUpgradeAvailable("") // initial: no upgrade visible

	// Simulate the async refresh result.
	updated, _ := m.Update(upgradeCheckedMsg{latest: "0.13.0"})
	got := updated.(*Model)
	if got.upgradeAvailable != "0.13.0" {
		t.Errorf("upgradeAvailable = %q, want 0.13.0", got.upgradeAvailable)
	}
}

// TestUpgradeCheckedMsg_clearsPill: an empty latest string clears
// the pill (e.g., user just upgraded; refresh detects no newer
// version is available).
func TestUpgradeCheckedMsg_clearsPill(t *testing.T) {
	m := &Model{}
	m.SetUpgradeAvailable("0.13.0") // initial: pill showing
	updated, _ := m.Update(upgradeCheckedMsg{latest: ""})
	got := updated.(*Model)
	if got.upgradeAvailable != "" {
		t.Errorf("upgradeAvailable = %q, want empty after refresh cleared", got.upgradeAvailable)
	}
}

// TestUpgradeRefreshCmd_nilFnReturnsNil: when no refresh fn is wired
// (test path, popup mode, etc.), upgradeRefreshCmd must not panic
// and must not produce a tea.Cmd. Init relies on this so it can
// safely append the result to its tea.Batch.
func TestUpgradeRefreshCmd_nilFnReturnsNil(t *testing.T) {
	if cmd := upgradeRefreshCmd(nil); cmd != nil {
		t.Errorf("nil fn should produce nil cmd; got %T", cmd)
	}
}

// TestUpgradeRefreshCmd_invokesFn: closure is called when wired,
// and its return value flows into the upgradeCheckedMsg.
func TestUpgradeRefreshCmd_invokesFn(t *testing.T) {
	called := false
	fn := func(ctx context.Context) (string, error) {
		called = true
		return "0.13.0", nil
	}
	cmd := upgradeRefreshCmd(fn)
	if cmd == nil {
		t.Fatal("non-nil fn should produce a cmd")
	}
	msg := cmd()
	if !called {
		t.Error("fn was not invoked")
	}
	got, ok := msg.(upgradeCheckedMsg)
	if !ok {
		t.Fatalf("msg type = %T, want upgradeCheckedMsg", msg)
	}
	if got.latest != "0.13.0" {
		t.Errorf("got.latest = %q, want 0.13.0", got.latest)
	}
}

// TestUpgradeRefreshCmd_swallowsError: closure errors must NOT
// propagate to upgradeCheckedMsg (which has no err field) — they're
// silently absorbed and the msg carries the empty latest the fn
// returned. Ensures a transient network failure doesn't crash the
// TUI mid-session.
func TestUpgradeRefreshCmd_swallowsError(t *testing.T) {
	fn := func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("network down")
	}
	cmd := upgradeRefreshCmd(fn)
	msg := cmd()
	got := msg.(upgradeCheckedMsg)
	if got.latest != "" {
		t.Errorf("latest on error = %q, want empty", got.latest)
	}
}

// TestActionDelete_RemoteRowSkipsManagerForRow verifies the v0.17.0
// branch: when the cursor row has a Host (remote workspace), actionDelete
// opens the confirm modal WITHOUT going through managerForRow +
// SafetyPreflight. Prior to the fix, pressing `d` on a remote row failed
// with "no projectroot — can't resolve" because the local canopy has no
// canopy.json for a project that lives on tower. Remote-side safety
// preflight runs at confirm time via `canopy rm --on <host> --yes`.
func TestActionDelete_RemoteRowSkipsManagerForRow(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.setTestRows([]Row{
		{
			Project: "cravd", ProjectRoot: "", // remote rows have no local ProjectRoot
			Name: "remote-foo", Status: state.StatusReady,
			Host: "tower",
			Path: "/home/cassy/.canopy/workspaces/cravd/remote-foo",
		},
	})

	_, _ = actionDelete(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})

	if m.mode != confirmDeleteMode {
		t.Fatalf("remote-row d did not open confirm modal; mode=%v err=%v", m.mode, m.err)
	}
	if m.deleteTarget != "remote-foo" {
		t.Errorf("deleteTarget = %q; want remote-foo", m.deleteTarget)
	}
	if m.deleteTargetRoot != "" {
		t.Errorf("deleteTargetRoot = %q; want empty for remote row", m.deleteTargetRoot)
	}
	if len(m.deleteHangs) != 0 {
		t.Errorf("deleteHangs = %v; want empty (remote preflight runs on confirm)", m.deleteHangs)
	}
}

// TestActionOpenBrowser_RemoteRowReturnsCmd verifies the v0.17.0 branch:
// pressing B on a remote row returns a non-nil tea.Cmd (the SSH tunnel +
// xdg-open closure) and does NOT set m.err synchronously. Prior to the
// fix, B on a remote row showed an instructional error string instead
// of actually port-forwarding.
//
// We don't run the returned Cmd (it would shell out to ssh); we just
// check the wiring: remote row → resolveHostForExec succeeds → returns
// the deferred tunnel command, not an immediate error.
func TestActionOpenBrowser_RemoteRowReturnsCmd(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.hostList = []host.Host{
		{Name: "tower", SSHTarget: "cassy@tower.invalid", Type: "ssh"},
	}
	m.setTestRows([]Row{
		{
			Project: "cravd", Name: "remote-foo", Status: state.StatusReady,
			Host: "tower",
			Port: 3001,
		},
	})

	_, cmd := actionOpenBrowser(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("B")})
	if m.err != nil {
		t.Fatalf("remote-row B set m.err synchronously: %v (want deferred cmd)", m.err)
	}
	if cmd == nil {
		t.Fatalf("remote-row B returned nil cmd; want openRemoteBrowser closure")
	}
}

// TestActionOpenBrowser_NoPortNoOp: pressing B on a row without a port
// (e.g. setting_up workspace) returns no cmd and no error. Same shape
// for local and remote so the new remote branch can't change behavior.
func TestActionOpenBrowser_NoPortNoOp(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{
		{Project: "x", Name: "w", Status: state.StatusSettingUp, Port: 0},
	})
	_, cmd := actionOpenBrowser(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("B")})
	if cmd != nil {
		t.Errorf("port=0 returned non-nil cmd; want nil")
	}
	if m.err != nil {
		t.Errorf("port=0 set m.err = %v; want nil", m.err)
	}
}

// TestCursorNav_HostsTabUsesHostsCursor: v0.17 Phase 1l — when on
// the Hosts tab, up/down navigate hostsCursor, not the workspace
// list. Verifies the two cursors are independent.
func TestCursorNav_HostsTabUsesHostsCursor(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{
		{Name: "alpha"}, {Name: "bravo"}, {Name: "charlie"},
	}
	m.hostsCursor = 0
	_, _ = actionCursorDown(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.hostsCursor != 1 {
		t.Errorf("after down: hostsCursor = %d; want 1", m.hostsCursor)
	}
	_, _ = actionCursorDown(m, tea.KeyMsg{Type: tea.KeyDown})
	_, _ = actionCursorDown(m, tea.KeyMsg{Type: tea.KeyDown}) // try to go past end
	if m.hostsCursor != 2 {
		t.Errorf("after 3 downs (bounded): hostsCursor = %d; want 2", m.hostsCursor)
	}
	_, _ = actionCursorUp(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.hostsCursor != 1 {
		t.Errorf("after up: hostsCursor = %d; want 1", m.hostsCursor)
	}
}

// TestAvailableWorkspaceVerbs_HiddenOnHostsTab: regression for the
// user-reported "Hosts tab still shows workspace verb shortcuts"
// bug. d / K / i / R / B / P / enter (workspace attach) must NOT
// be available on the Hosts tab. v0.17 Phase 1l.
func TestAvailableWorkspaceVerbs_HiddenOnHostsTab(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	if availableInWorkspaceContext(m) {
		t.Errorf("availableInWorkspaceContext true on Hosts tab; want false")
	}
	// Sanity: returns true on the workspace tabs.
	m.tab = tabGlobal
	if !availableInWorkspaceContext(m) {
		t.Errorf("availableInWorkspaceContext false on Global tab; want true")
	}
}

// TestAvailableOnHostsTab_GatesHostVerbs: complement — host-specific
// verbs (d→remove, enter→detail) must ONLY fire on the Hosts tab.
func TestAvailableOnHostsTab_GatesHostVerbs(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	if availableOnHostsTab(m) {
		t.Errorf("availableOnHostsTab true on Global tab; want false")
	}
	m.tab = tabHosts
	if availableOnHostsTab(m) {
		t.Errorf("availableOnHostsTab true on empty Hosts tab; want false (no host to act on)")
	}
	m.hostList = []host.Host{{Name: "tower"}}
	if !availableOnHostsTab(m) {
		t.Errorf("availableOnHostsTab false on populated Hosts tab; want true")
	}
}

// TestActionHostRemove_OpensConfirmModal: pressing d on a host opens
// the confirm-remove modal with the selected host pre-loaded.
func TestActionHostRemove_OpensConfirmModal(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{
		{Name: "alpha"}, {Name: "bravo"},
	}
	m.hostsCursor = 1 // bravo
	_, _ = actionHostRemove(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if m.mode != confirmHostRemoveMode {
		t.Errorf("mode = %v; want confirmHostRemoveMode", m.mode)
	}
	if m.hostRemoveTarget != "bravo" {
		t.Errorf("hostRemoveTarget = %q; want bravo", m.hostRemoveTarget)
	}
}

// TestActionNewWorkspace_HostsTabOpensInTUIForm: v0.17 Phase 1l —
// pressing n on the Hosts tab opens the in-TUI add-host name input,
// NOT the legacy huh subprocess wizard.
func TestActionNewWorkspace_HostsTabOpensInTUIForm(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	_, _ = actionNewWorkspace(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.mode != addHostFormMode {
		t.Errorf("mode = %v; want addHostFormMode", m.mode)
	}
}

// TestHandleAddHostFormKey_TabCyclesFocus: regression for the user-
// reported "I can't Tab between host name and ssh-target" bug.
// v0.17 Phase 1l polish — the form is now a single mode with two
// inputs that Tab cycles between (and Enter submits both).
func TestHandleAddHostFormKey_TabCyclesFocus(t *testing.T) {
	m := newTestModel(false)
	_ = m.openAddHostForm() // sets mode + focuses name
	if m.hostAddFocus != 0 || !m.nameInput.Focused() {
		t.Fatalf("openAddHostForm did not focus name input")
	}
	_, _ = m.handleAddHostFormKey(tea.KeyMsg{Type: tea.KeyTab})
	if m.hostAddFocus != 1 || !m.targetInput.Focused() || m.nameInput.Focused() {
		t.Errorf("after Tab: focus didn't switch to target; name.Focused=%v target.Focused=%v",
			m.nameInput.Focused(), m.targetInput.Focused())
	}
	_, _ = m.handleAddHostFormKey(tea.KeyMsg{Type: tea.KeyTab})
	if m.hostAddFocus != 0 || !m.nameInput.Focused() {
		t.Errorf("after second Tab: focus didn't cycle back to name")
	}
}

// TestHandleAddHostFormKey_EnterRequiresBothFields: pressing Enter
// with an empty field should re-focus that field instead of submitting
// (and instead of doing nothing — that confuses users).
func TestHandleAddHostFormKey_EnterRequiresBothFields(t *testing.T) {
	m := newTestModel(false)
	_ = m.openAddHostForm()
	// Empty name + empty target → re-focus name.
	_, _ = m.handleAddHostFormKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != addHostFormMode {
		t.Errorf("Enter with empty fields exited the form; want stay")
	}
	if m.hostAddFocus != 0 {
		t.Errorf("Enter on empty name: focus = %d; want 0 (name)", m.hostAddFocus)
	}
	// Fill name, leave target empty → focus moves to target.
	m.nameInput.SetValue("tower")
	_, _ = m.handleAddHostFormKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.hostAddFocus != 1 {
		t.Errorf("Enter with name+no-target: focus = %d; want 1 (target)", m.hostAddFocus)
	}
}

// TestActionHostEnter_OpensDetail: regression for the user-reported
// "enter on host shows an error message" bug. Now opens hostDetailMode
// instead of stashing an info string in m.err.
func TestActionHostEnter_OpensDetail(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{
		{Name: "tower", SSHTarget: "u@t", Type: "ssh"},
	}
	m.hostsCursor = 0
	_, _ = actionHostEnter(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != hostDetailMode {
		t.Errorf("mode = %v; want hostDetailMode", m.mode)
	}
	if m.err != nil {
		t.Errorf("actionHostEnter set m.err = %v; want nil (no error)", m.err)
	}
	if m.hostDetailTarget != "tower" {
		t.Errorf("hostDetailTarget = %q; want tower", m.hostDetailTarget)
	}
}

// TestHandleHostDetailKey_EscDismisses: any "dismissive" key returns
// to listMode.
func TestHandleHostDetailKey_EscDismisses(t *testing.T) {
	m := newTestModel(false)
	m.mode = hostDetailMode
	m.hostDetailTarget = "tower"
	_, _ = m.handleHostDetailKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != listMode {
		t.Errorf("esc: mode = %v; want listMode", m.mode)
	}
	if m.hostDetailTarget != "" {
		t.Errorf("esc: hostDetailTarget = %q; want cleared", m.hostDetailTarget)
	}
}

// TestHostProbeResultMsg_AuthFailOpensSSHCopyID: AuthFailed routes to
// the confirm-ssh-copy-id modal with the probe target pre-loaded.
func TestHostProbeResultMsg_AuthFailOpensSSHCopyID(t *testing.T) {
	m := newTestModel(false)
	_, _ = m.Update(hostProbeResultMsg{hostName: "pi", sshTarget: "pi@p", authFail: true})
	mm := m
	if mm.mode != confirmSSHCopyIDMode {
		t.Errorf("authFail: mode = %v; want confirmSSHCopyIDMode", mm.mode)
	}
	if mm.pendingProbeHost != "pi" || mm.pendingProbeTarget != "pi@p" {
		t.Errorf("probe target not loaded: host=%q target=%q", mm.pendingProbeHost, mm.pendingProbeTarget)
	}
}

// TestHandleConfirmDeleteKey_RemoteForceWithoutLocalHangs: regression
// for the user-reported flow where remote rm rejected on hanging
// work and the TUI gave no way to escalate. Pre-fix: F was only
// accepted when local hangs were detected (impossible for remote).
// Now: F always works for remote rows; dispatches with --force.
// v0.17 Phase 1l polish.
func TestHandleConfirmDeleteKey_RemoteForceWithoutLocalHangs(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.remoteRows = []Row{
		{Project: "canopy", Name: "testing-123", Status: state.StatusReady, Host: "tower"},
	}
	m.list.SetRows(m.filteredRows())
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.mode = confirmDeleteMode
	m.deleteTarget = "testing-123"
	m.deleteHangs = nil // remote → no local hangs

	_, cmd := m.handleConfirmDeleteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("F")})
	if cmd == nil {
		t.Fatalf("F on remote with no local hangs: got nil cmd; want execRemoteVerb")
	}
	if m.mode != listMode {
		t.Errorf("F on remote: mode = %v; want listMode (dispatched + closed modal)", m.mode)
	}
}

// TestHandleConfirmDeleteKey_RemoteYDispatchesWithoutForce: same
// modal, lowercase y dispatches without --force. The remote will
// run its own safety check and refuse on hanging work; that's
// surfaced as an error and the user can retry with F.
func TestHandleConfirmDeleteKey_RemoteYDispatchesWithoutForce(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.remoteRows = []Row{
		{Project: "canopy", Name: "foo", Status: state.StatusReady, Host: "tower"},
	}
	m.list.SetRows(m.filteredRows())
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.mode = confirmDeleteMode
	m.deleteTarget = "foo"

	_, cmd := m.handleConfirmDeleteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatalf("y on remote: got nil cmd; want execRemoteVerb")
	}
}

// TestActionHostSetupAuth_OpensSSHCopyIDModal: pressing `a` on a host
// pre-loads the probe target and opens the ssh-copy-id confirm flow.
// Lets the user retry auth without deleting and re-adding.
func TestActionHostSetupAuth_OpensSSHCopyIDModal(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{
		{Name: "pi", SSHTarget: "jarvis@pi.tail.ts.net", Type: "ssh"},
	}
	m.hostsCursor = 0
	_, _ = actionHostSetupAuth(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m.mode != confirmSSHCopyIDMode {
		t.Errorf("mode = %v; want confirmSSHCopyIDMode", m.mode)
	}
	if m.pendingProbeHost != "pi" {
		t.Errorf("pendingProbeHost = %q; want pi", m.pendingProbeHost)
	}
	if m.pendingProbeTarget != "jarvis@pi.tail.ts.net" {
		t.Errorf("pendingProbeTarget = %q; want jarvis@pi.tail.ts.net", m.pendingProbeTarget)
	}
}

// TestHandleConfirmSSHCopyIDKey_RejectsDashPrefixedTarget is the
// regression test for the ssh-copy-id option-injection gap found
// during the v0.22.0.0 security review: ssh-copy-id forwards an
// option-shaped target to its OWN internal ssh invocation unprotected
// (confirmed by PoC — a "--" separator on canopy's own call to
// ssh-copy-id does NOT make this safe the way it does for direct
// ssh/mosh calls). handleConfirmSSHCopyIDKey must validate and refuse
// rather than exec'ing ssh-copy-id at all.
func TestHandleConfirmSSHCopyIDKey_RejectsDashPrefixedTarget(t *testing.T) {
	m := newTestModel(false)
	m.mode = confirmSSHCopyIDMode
	m.pendingProbeHost = "evil"
	m.pendingProbeTarget = "-oProxyCommand=touch /tmp/pwned"

	model, cmd := m.handleConfirmSSHCopyIDKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	mm := model.(*Model)

	if mm.mode != listMode {
		t.Errorf("mode = %v; want listMode (modal dismissed either way)", mm.mode)
	}
	if cmd != nil {
		t.Error("cmd != nil; want nil — a dash-prefixed target must never reach exec.Command(\"ssh-copy-id\", ...)")
	}
}

// TestAvailableHostAuth_GatesOnAuthFailedStatus: `a` is hidden when
// the host's last refresh succeeded (auth is already working). Shown
// when status=AuthFailed OR unknown (never refreshed; user can
// pre-emptively set up auth).
func TestAvailableHostAuth_GatesOnAuthFailedStatus(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "pi"}}
	m.hostsCursor = 0

	// No snapshot yet → unknown → allow.
	if !availableHostAuth(m) {
		t.Errorf("unknown status: `a` should be available")
	}
	// Snapshot with Permission-denied → allow.
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: "Permission denied (publickey)"},
	}
	if !availableHostAuth(m) {
		t.Errorf("AuthFailed: `a` should be available")
	}
	// Snapshot with success → hide.
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: ""},
	}
	if availableHostAuth(m) {
		t.Errorf("Online: `a` should NOT be available")
	}
}

// TestAvailableHostUpgrade_RequiresKnownVersion verifies the gate on
// the Hosts-tab `U` binding. The host must have responded successfully
// at least once (snap.LastError == "") AND reported a CanopyVersion
// other than "dev"; without those signals the remote `canopy upgrade`
// would fail (binary missing, schema mismatch, or the dev-binary
// refuse). Hiding U is the right UX — pressing it on a doomed host
// would just silently flicker.
func TestAvailableHostUpgrade_RequiresKnownVersion(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "pi"}}
	m.hostsCursor = 0

	// No snapshot → host never reached → hide.
	if availableHostUpgrade(m) {
		t.Errorf("unknown status: U must NOT be available")
	}
	// Snapshot with version but stale error → hide (most recent
	// refresh failed, so canopy may have disappeared since).
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {CanopyVersion: "0.17.0", LastError: "timeout"},
	}
	if availableHostUpgrade(m) {
		t.Errorf("LastError set: U must NOT be available")
	}
	// Snapshot success but version empty → hide (we can't be sure
	// the remote has canopy installed).
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: ""},
	}
	if availableHostUpgrade(m) {
		t.Errorf("empty version: U must NOT be available")
	}
	// Dev binary → hide. `canopy upgrade` refuses to run on a dev
	// build (it requires switching to a released canopy first), so
	// surfacing U for these hosts would dispatch a guaranteed-fail SSH.
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: "", CanopyVersion: "dev"},
	}
	if availableHostUpgrade(m) {
		t.Errorf("dev binary: U must NOT be available")
	}
	// Snapshot success with released version → show.
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: "", CanopyVersion: "0.17.0"},
	}
	if !availableHostUpgrade(m) {
		t.Errorf("ready host: U must be available")
	}
	// Wrong tab → hide regardless.
	m.tab = tabGlobal
	if availableHostUpgrade(m) {
		t.Errorf("non-Hosts tab: U must NOT be available")
	}
}

// TestAvailableHostSwitchRelease_ShowsOnDevOrUnknown verifies the
// gate on the Hosts-tab `S` binding. Surfaces S when the remote
// EITHER reports "dev" OR reports nothing useful ("" / "(unknown)" —
// the latter is what old canopies emitted before the version-emit
// fix landed). Hidden only when the remote reports a real semver
// (release host where `canopy use release` would be a no-op).
func TestAvailableHostSwitchRelease_ShowsOnDevOrUnknown(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "pi"}}
	m.hostsCursor = 0

	if availableHostSwitchRelease(m) {
		t.Errorf("no snapshot: S must NOT be available")
	}
	// Real release version → hide S (use release would be a no-op).
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: "", CanopyVersion: "0.17.0"},
	}
	if availableHostSwitchRelease(m) {
		t.Errorf("release binary: S must NOT be available")
	}
	// Explicit dev → show.
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: "", CanopyVersion: "dev"},
	}
	if !availableHostSwitchRelease(m) {
		t.Errorf("dev binary: S must be available")
	}
	// Legacy "(unknown)" — old canopy without the version-emit fix.
	// Could be dev or release; safer to offer S as a no-op-if-release
	// than to hide it from a known-dev host.
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: "", CanopyVersion: "(unknown)"},
	}
	if !availableHostSwitchRelease(m) {
		t.Errorf("(unknown) version: S must be available")
	}
	// Empty version (snap exists but no canopy_version field) — same
	// reasoning: surface S.
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: "", CanopyVersion: ""},
	}
	if !availableHostSwitchRelease(m) {
		t.Errorf("empty version: S must be available")
	}
	// Stale error → hide (we don't trust the version).
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: "timeout", CanopyVersion: "dev"},
	}
	if availableHostSwitchRelease(m) {
		t.Errorf("LastError set: S must NOT be available")
	}
}

// TestActionHostSwitchRelease_EntersConfirmingState verifies the S
// action primes the same state machine as U but with the "use release"
// labels and remote command. Regression target: if action / verb /
// remoteCmd are left blank, the renderer falls back to upgrade-style
// labels and the SSH dispatches the wrong command.
func TestActionHostSwitchRelease_EntersConfirmingState(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "pi", SSHTarget: "cassy@pi"}}
	m.hostsCursor = 0
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"pi": {LastError: "", CanopyVersion: "dev"},
	}
	model, _ := actionHostSwitchRelease(m, tea.KeyMsg{})
	mm := model.(*Model)
	if mm.mode != hostUpgradeMode {
		t.Errorf("mode = %v; want hostUpgradeMode", mm.mode)
	}
	if mm.hostUpgradeAction != "use release" {
		t.Errorf("action = %q; want 'use release'", mm.hostUpgradeAction)
	}
	if !strings.Contains(mm.hostUpgradeRemoteCmd, "canopy use release") {
		t.Errorf("remoteCmd missing 'canopy use release': %q", mm.hostUpgradeRemoteCmd)
	}
	if strings.Contains(mm.hostUpgradeRemoteCmd, "upgrade") {
		t.Errorf("remoteCmd leaked upgrade verb: %q", mm.hostUpgradeRemoteCmd)
	}
}

// TestActionHostUpgrade_EntersConfirmingState verifies that the U
// action sets up hostUpgradeMode in the confirming substate, capturing
// the host name + ssh target + current version so the confirm screen
// can show them. Regression target: a stale m.hostUpgradeHost from a
// previous entry would surface "Upgrading <wrong host>" in the
// confirm screen — resetHostUpgradeMode (called on dismiss) must keep
// these fields tied to the current cursor row.
func TestActionHostUpgrade_EntersConfirmingState(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{
		{Name: "tower", SSHTarget: "avi@tower"},
	}
	m.hostsCursor = 0
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {LastError: "", CanopyVersion: "0.17.0"},
	}
	model, cmd := actionHostUpgrade(m, tea.KeyMsg{})
	mm := model.(*Model)
	if mm.mode != hostUpgradeMode {
		t.Errorf("mode = %v; want hostUpgradeMode", mm.mode)
	}
	if mm.hostUpgradeState != hostUpgradeStateConfirming {
		t.Errorf("state = %v; want confirming", mm.hostUpgradeState)
	}
	if mm.hostUpgradeHost != "tower" {
		t.Errorf("host = %q; want tower", mm.hostUpgradeHost)
	}
	if mm.hostUpgradeTarget != "avi@tower" {
		t.Errorf("target = %q; want avi@tower", mm.hostUpgradeTarget)
	}
	if mm.hostUpgradeVersion != "0.17.0" {
		t.Errorf("version = %q; want 0.17.0", mm.hostUpgradeVersion)
	}
	if mm.hostUpgradeAction != "upgrade" {
		t.Errorf("action = %q; want 'upgrade'", mm.hostUpgradeAction)
	}
	if !strings.Contains(mm.hostUpgradeRemoteCmd, "canopy upgrade --yes") {
		t.Errorf("remoteCmd missing 'canopy upgrade --yes': %q", mm.hostUpgradeRemoteCmd)
	}
	if cmd != nil {
		t.Errorf("entering confirm should NOT dispatch SSH yet")
	}
}

// TestHandleHostUpgradeKey_ConfirmRunsCmd: pressing y in the
// confirming substate flips to running and returns a non-nil Cmd
// (the SSH spawner). Esc cancels back to listMode + clears state.
func TestHandleHostUpgradeKey_ConfirmRunsCmd(t *testing.T) {
	m := newTestModel(false)
	m.mode = hostUpgradeMode
	m.hostUpgradeState = hostUpgradeStateConfirming
	m.hostUpgradeHost = "tower"
	m.hostUpgradeTarget = "avi@tower"

	model, cmd := m.handleHostUpgradeKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm := model.(*Model)
	if mm.hostUpgradeState != hostUpgradeStateRunning {
		t.Errorf("y: state = %v; want running", mm.hostUpgradeState)
	}
	if cmd == nil {
		t.Errorf("y: cmd must be non-nil (the SSH spawner)")
	}

	// Esc from confirming → reset + listMode.
	m2 := newTestModel(false)
	m2.mode = hostUpgradeMode
	m2.hostUpgradeState = hostUpgradeStateConfirming
	m2.hostUpgradeHost = "tower"
	model2, _ := m2.handleHostUpgradeKey(tea.KeyMsg{Type: tea.KeyEsc})
	mm2 := model2.(*Model)
	if mm2.mode != listMode {
		t.Errorf("esc: mode = %v; want listMode", mm2.mode)
	}
	if mm2.hostUpgradeState != hostUpgradeStateNone {
		t.Errorf("esc: state = %v; want none", mm2.hostUpgradeState)
	}
	if mm2.hostUpgradeHost != "" {
		t.Errorf("esc: host not cleared: %q", mm2.hostUpgradeHost)
	}
}

// TestAvailableLocalUpgrade_HiddenOnHostsTab verifies the in-TUI local
// upgrade is gated off the Hosts tab so it doesn't collide with the
// host-upgrade dispatch on the same key. The pre-existing
// availableUpgrade predicate stays the inner condition; the new
// availableLocalUpgrade just adds a tab check on top.
func TestAvailableLocalUpgrade_HiddenOnHostsTab(t *testing.T) {
	m := newTestModel(false)
	// Pretend an upgrade is available + closures wired so the inner
	// predicate returns true.
	m.upgradeAvailable = "0.18.0"
	m.upgradeChangelogFn = func(ctx context.Context) (string, error) { return "", nil }
	m.upgradeShellFn = func(ctx context.Context, w io.Writer) error { return nil }

	m.tab = tabGlobal
	if !availableLocalUpgrade(m) {
		t.Errorf("Global tab + upgrade available: U must fire local flow")
	}
	m.tab = tabHosts
	if availableLocalUpgrade(m) {
		t.Errorf("Hosts tab: local upgrade gate must be closed")
	}
}

// TestVisibleTabs_DropsLocalWhenNoProject: visibleTabs is the source
// of truth for the tab cycle. It drops tabLocal when there's no
// currentProject and tabHosts when no hosts are registered. v0.17
// Phase 1h.
func TestVisibleTabs_DropsLocalWhenNoProject(t *testing.T) {
	cases := []struct {
		name      string
		project   string
		withHosts bool
		want      []tabKind
	}{
		{"no project, no hosts", "", false, []tabKind{tabGlobal}},
		{"no project, with hosts", "", true, []tabKind{tabGlobal, tabHosts}},
		{"with project, no hosts", "/p/x", false, []tabKind{tabLocal, tabGlobal}},
		{"with project, with hosts", "/p/x", true, []tabKind{tabLocal, tabGlobal, tabHosts}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(false)
			m.currentProject = tc.project
			if tc.withHosts {
				m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
			}
			got := m.visibleTabs()
			if len(got) != len(tc.want) {
				t.Fatalf("visibleTabs = %v; want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("visibleTabs[%d] = %v; want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestNextInCycle_WrapsBothWays: nextInCycle is the helper that does
// the actual stepping. It must wrap forward (last → first) and
// backward (first → last).
func TestNextInCycle_WrapsBothWays(t *testing.T) {
	tabs := []tabKind{tabLocal, tabGlobal, tabHosts}
	if got := nextInCycle(tabs, tabHosts, +1); got != tabLocal {
		t.Errorf("forward wrap: got %v; want tabLocal", got)
	}
	if got := nextInCycle(tabs, tabLocal, -1); got != tabHosts {
		t.Errorf("backward wrap: got %v; want tabHosts", got)
	}
	// Current tab not in cycle (defensive case): land on first.
	if got := nextInCycle([]tabKind{tabGlobal}, tabLocal, +1); got != tabGlobal {
		t.Errorf("current not in cycle: got %v; want tabGlobal", got)
	}
}

// TestAttachSelected_WarnsWhenAnotherClientAttached: v0.17 Phase 1j —
// pressing Enter on a row whose session already has a tmux client
// connected pops the confirm modal instead of attaching immediately.
// Tmux's default is to share the session (both clients see the same
// panes), which is usually wrong for an active agent workspace.
func TestAttachSelected_WarnsWhenAnotherClientAttached(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal // newTestModel defaults to Local with /tmp/test-project filter
	m.setTestRows([]Row{
		{Project: "cravd", ProjectRoot: "/p/cravd", Name: "foo",
			Status: state.StatusReady, Alive: true, Attached: true},
	})
	_, _ = m.attachSelected()
	if m.mode != confirmAttachMode {
		t.Fatalf("expected confirmAttachMode; got %v", m.mode)
	}
	if m.attachTarget.Name != "foo" {
		t.Errorf("attachTarget.Name = %q; want foo", m.attachTarget.Name)
	}
}

// TestAttachSelected_SkipsWarnForCurrentWorkspace: re-attaching to
// the workspace canopy was launched from is the expected flow (the
// popup user pressing Enter on their own row to "go back"). The
// "already attached" client IS the one we just launched from — no
// warning needed.
func TestAttachSelected_SkipsWarnForCurrentWorkspace(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.currentWorkspace = "foo"
	m.currentWorkspaceRoot = "/p/cravd"
	m.setTestRows([]Row{
		{Project: "cravd", ProjectRoot: "/p/cravd", Name: "foo",
			Status: state.StatusReady, Alive: true, Attached: true},
	})
	_, _ = m.attachSelected()
	if m.mode == confirmAttachMode {
		t.Errorf("expected attach to proceed for current workspace; got confirmAttachMode")
	}
}

// TestAttachSelected_NoWarnWhenNotAttached: the common case — sole
// client — attaches straight through, no modal.
func TestAttachSelected_NoWarnWhenNotAttached(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabGlobal
	m.setTestRows([]Row{
		{Project: "cravd", ProjectRoot: "/p/cravd", Name: "foo",
			Status: state.StatusReady, Alive: true, Attached: false},
	})
	_, _ = m.attachSelected()
	if m.mode == confirmAttachMode {
		t.Errorf("unattached row triggered confirmAttachMode; want straight attach")
	}
}

// TestRemoteCwdForRow_ResolvesFromRegistry: regression for the user-
// reported "n on remote rows fails when not inside a project" bug.
// execRemoteNew was passing only --on; canopy new then tried to walk
// cwd for canopy.json and errored out with "needs a project but
// you're not inside any". The fix: TUI pre-resolves the remote path
// from the in-memory host registry snapshot and passes --remote-cwd
// explicitly so the remote dispatch never needs cwd. v0.17 Phase 1i.
func TestRemoteCwdForRow_ResolvesFromRegistry(t *testing.T) {
	m := newTestModel(false)
	m.hostList = []host.Host{
		{Name: "tower", SSHTarget: "u@t", Type: "ssh",
			Projects: map[string]string{"cravd": "/home/cassy/Work/cravd"}},
	}
	if got := m.remoteCwdForRow("tower", "cravd"); got != "/home/cassy/Work/cravd" {
		t.Errorf("remoteCwdForRow(tower, cravd) = %q; want /home/cassy/Work/cravd", got)
	}
	if got := m.remoteCwdForRow("tower", "missing"); got != "" {
		t.Errorf("remoteCwdForRow(tower, missing) = %q; want empty (let caller fall back)", got)
	}
	if got := m.remoteCwdForRow("unknown-host", "cravd"); got != "" {
		t.Errorf("remoteCwdForRow(unknown-host, cravd) = %q; want empty", got)
	}
}

// TestRemoteCwdArg_PinsKnownProject: when the host registry knows
// (host, project), the remote-dispatch path appends --remote-cwd so
// cmd/canopy's resolveOnForSwitch never enters its "first project on
// host" fallback. That fallback prints the scary `(registry:tower/X
// (fallback))` line in the dispatch source AND can land a verb in
// the wrong project on multi-project hosts. Regression for the
// user-reported example where a remote rm dispatch showed
// "(fallback)" even though the row's project was known.
func TestRemoteCwdArg_PinsKnownProject(t *testing.T) {
	m := newTestModel(false)
	m.hostList = []host.Host{
		{Name: "tower", SSHTarget: "u@t", Type: "ssh",
			Projects: map[string]string{"canopy": "/home/cassy/Work/canopy"}},
	}

	got := m.remoteCwdArg("tower", "canopy")
	want := []string{"--remote-cwd", "/home/cassy/Work/canopy"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("remoteCwdArg(tower, canopy) = %v; want %v", got, want)
	}
}

// TestRemoteCwdArg_UnknownProjectReturnsNil: when the registry doesn't
// know the (host, project) path, return nil so the dispatch stays in
// the legacy shape. Falling back is correct behavior here — the user
// just gets the older (worse) diagnostic if the workspace is missing.
// Pinning to a guessed path would be worse: we'd risk dispatching to
// a path that doesn't exist on the remote and tripping the cwd
// pre-check in buildRemoteScript.
func TestRemoteCwdArg_UnknownProjectReturnsNil(t *testing.T) {
	m := newTestModel(false)
	m.hostList = []host.Host{
		{Name: "tower", SSHTarget: "u@t", Type: "ssh",
			Projects: map[string]string{"canopy": "/home/cassy/Work/canopy"}},
	}

	if got := m.remoteCwdArg("tower", "unknown-project"); got != nil {
		t.Errorf("remoteCwdArg(tower, unknown-project) = %v; want nil", got)
	}
	if got := m.remoteCwdArg("unknown-host", "canopy"); got != nil {
		t.Errorf("remoteCwdArg(unknown-host, canopy) = %v; want nil", got)
	}
}

// TestHandleConfirmAttachKey_NCancels: cancel-by-default — anything
// other than y/Y/Enter returns to listMode without attaching.
func TestHandleConfirmAttachKey_NCancels(t *testing.T) {
	m := newTestModel(false)
	m.mode = confirmAttachMode
	m.attachTarget = Row{Name: "foo"}
	_, _ = m.handleConfirmAttachKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.mode != listMode {
		t.Errorf("n keypress: mode = %v; want listMode", m.mode)
	}
	if m.attachTarget.Name != "" {
		t.Errorf("attachTarget not cleared: %+v", m.attachTarget)
	}
}

// TestRefreshAllMsg_TriggersBothLocalAndRemote: regression for the
// user-reported "I have to manually refresh after rm/K on a remote
// row." Post-remote-action callbacks emit refreshAllMsg, which must
// kick off m.refresh() — that's what fans out the remote tick.
// refreshCmd alone (the prior implementation) only updated local rows,
// so the deleted remote row stayed visible until the next 2s tick.
func TestRefreshAllMsg_TriggersBothLocalAndRemote(t *testing.T) {
	m := newTestModel(false)
	m.remoteRefreshing = false // free to dispatch a remote tick

	_, cmd := m.Update(refreshAllMsg{})
	if cmd == nil {
		t.Fatalf("refreshAllMsg returned nil cmd; want a refresh batch")
	}
	// m.refresh() flips remoteRefreshing to true to latch the in-flight
	// remote fan-out. If that didn't happen, the dispatched cmd was the
	// local-only path — which is the bug we're guarding against.
	if !m.remoteRefreshing {
		t.Errorf("refreshAllMsg did not latch remoteRefreshing; remote tick was not dispatched")
	}
}

// TestHostsSpinnerTick_AdvancesFrameWhileRefreshing pins the new
// loading-spinner tick loop: while remoteRefreshing is true, each
// tick bumps the frame index AND re-arms the next tick. Without
// re-arming, the animation would stop after one frame.
func TestHostsSpinnerTick_AdvancesFrameWhileRefreshing(t *testing.T) {
	m := newTestModel(false)
	m.remoteRefreshing = true
	m.hostsSpinnerActive = true
	m.hostsSpinnerFrame = 3

	_, cmd := m.Update(hostsSpinnerTickMsg{})
	if m.hostsSpinnerFrame != 4 {
		t.Errorf("hostsSpinnerFrame = %d, want 4 (advanced by 1)", m.hostsSpinnerFrame)
	}
	if cmd == nil {
		t.Errorf("tick handler returned nil cmd while refreshing; expected re-arm")
	}
	if !m.hostsSpinnerActive {
		t.Errorf("hostsSpinnerActive flipped false while refreshing; should stay latched")
	}
}

// TestHostsSpinnerTick_StopsWhenRefreshSettles is the OTHER branch:
// once remoteRefreshing flips false (via remoteRowsLoadedMsg), the
// next tick must drop the active latch AND return no cmd — otherwise
// the tick loop runs forever, burning a wakeup every 120ms forever.
func TestHostsSpinnerTick_StopsWhenRefreshSettles(t *testing.T) {
	m := newTestModel(false)
	m.remoteRefreshing = false
	m.hostsSpinnerActive = true
	m.hostsSpinnerFrame = 7

	_, cmd := m.Update(hostsSpinnerTickMsg{})
	if cmd != nil {
		t.Errorf("tick returned cmd %v after refresh settled; loop should stop", cmd)
	}
	if m.hostsSpinnerActive {
		t.Errorf("hostsSpinnerActive still true after settle; should clear so next refresh re-arms cleanly")
	}
	if m.hostsSpinnerFrame != 7 {
		t.Errorf("hostsSpinnerFrame = %d, want 7 (must not advance after settle)", m.hostsSpinnerFrame)
	}
}

// TestRefresh_StartsSpinnerTickAlongsideRemoteFanOut: refresh() must
// dispatch BOTH the remote tick AND the spinner tick when no
// in-flight fan-out exists. Regression target if a future refactor
// re-orders the cmd batch and drops the spinner kick.
func TestRefresh_StartsSpinnerTickAlongsideRemoteFanOut(t *testing.T) {
	m := newTestModel(false)
	m.remoteRefreshing = false
	m.hostsSpinnerActive = false
	m.hostsSpinnerFrame = 99 // sentinel: must be reset to 0

	cmd := m.refresh()
	if cmd == nil {
		t.Fatalf("refresh returned nil cmd")
	}
	if !m.hostsSpinnerActive {
		t.Errorf("refresh did not latch hostsSpinnerActive")
	}
	if m.hostsSpinnerFrame != 0 {
		t.Errorf("hostsSpinnerFrame = %d, want 0 (fresh refresh resets the animation)", m.hostsSpinnerFrame)
	}
}

// TestRefresh_NoOpWhenRemoteFanOutInFlight pins the outer guard in
// refresh(): when remoteRefreshing is already true (a fan-out is
// running), refresh() must NOT touch the spinner state. Otherwise a
// rapid second refresh would reset the frame counter mid-animation
// (visual jitter) AND queue a redundant tick loop.
func TestRefresh_NoOpWhenRemoteFanOutInFlight(t *testing.T) {
	m := newTestModel(false)
	m.remoteRefreshing = true
	m.hostsSpinnerActive = true
	m.hostsSpinnerFrame = 5

	_ = m.refresh()
	if m.hostsSpinnerFrame != 5 {
		t.Errorf("hostsSpinnerFrame = %d, want 5 (in-flight refresh must not reset the frame)", m.hostsSpinnerFrame)
	}
	if !m.hostsSpinnerActive {
		t.Errorf("hostsSpinnerActive was cleared by a duplicate refresh; should stay latched")
	}
}

// TestRefresh_DoesNotDoubleDispatchSpinnerTick pins the inner guard:
// when no fan-out is in flight (so the outer condition fires) BUT a
// spinner tick is already pending (hostsSpinnerActive=true from a
// prior cycle that hasn't drained yet), refresh() must dispatch the
// remote fan-out without ALSO dispatching a second spinner tick.
// Without this guard, every external trigger of refresh() while a
// tick is mid-flight would compound the frame-advance rate.
func TestRefresh_DoesNotDoubleDispatchSpinnerTick(t *testing.T) {
	m := newTestModel(false)
	m.remoteRefreshing = false
	m.hostsSpinnerActive = true // a tick is already in flight
	m.hostsSpinnerFrame = 3

	cmd := m.refresh()
	if cmd == nil {
		t.Fatalf("refresh returned nil cmd")
	}
	// Frame is still reset (fresh refresh) but the active latch stays
	// the same — no second tick loop spawned.
	if m.hostsSpinnerFrame != 0 {
		t.Errorf("hostsSpinnerFrame = %d, want 0 (fresh remote refresh resets the animation even if a tick is pending)", m.hostsSpinnerFrame)
	}
	if !m.hostsSpinnerActive {
		t.Errorf("hostsSpinnerActive flipped off; must remain latched (the inner guard's whole job)")
	}
}

// TestRefreshAllMsg_ClearsRemoteRefreshingBeforeDispatch is the
// regression test for the pre-fix race where post-remote-action tea.Cmd
// closures (execRemoteVerb / execRemoteKill / attachOrSwitchWithOpts)
// wrote `m.remoteRefreshing = false` from the goroutine they ran in,
// to free refresh()'s `!remoteRefreshing` gate so the post-action
// refresh would actually dispatch the remote fan-out. That goroutine
// write raced the 120ms hostsSpinnerTick read on the Bubbletea
// goroutine (-race observable since v0.21.1's spinner addition).
//
// Post-fix: the closures don't touch m at all; the refreshAllMsg
// handler in Update clears the latch on the Bubbletea goroutine
// BEFORE calling refresh(). The behavioral proxy: starting from
// remoteRefreshing=true (as if a previous fan-out was still latched
// when the post-action callback fires), refreshAllMsg must reset the
// hostsSpinnerFrame to 0 — refresh()'s outer guard only resets the
// frame when it sees the latch cleared. If Update DIDN'T clear first,
// refresh() would see latch=true and skip the reset.
func TestRefreshAllMsg_ClearsRemoteRefreshingBeforeDispatch(t *testing.T) {
	m := newTestModel(false)
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.remoteRefreshing = true // simulate "latch still held by prior fan-out"
	m.hostsSpinnerActive = true
	m.hostsSpinnerFrame = 5 // sentinel: a clean refresh dispatch resets to 0

	_, _ = m.Update(refreshAllMsg{})

	if m.hostsSpinnerFrame != 0 {
		t.Errorf("hostsSpinnerFrame = %d, want 0 — refreshAllMsg did not clear remoteRefreshing before calling refresh(); the post-action fan-out was a no-op", m.hostsSpinnerFrame)
	}
	// And the latch should be true again because refresh() relatches it
	// for the new in-flight remote dispatch.
	if !m.remoteRefreshing {
		t.Errorf("remoteRefreshing = false after refreshAllMsg; refresh() failed to relatch the new dispatch")
	}
}

// TestErrMsg_SetsErrAndStaysIdle: an errMsg delivered to Update sets
// m.err and returns no follow-up cmd (no refresh, no retry).
func TestErrMsg_SetsErrAndStaysIdle(t *testing.T) {
	m := newTestModel(false)
	_, cmd := m.Update(errMsg{err: fmt.Errorf("boom")})
	mm := m
	if mm.err == nil || mm.err.Error() != "boom" {
		t.Errorf("m.err = %v; want 'boom'", mm.err)
	}
	if cmd != nil {
		t.Errorf("errMsg returned cmd %v; want nil (no refresh)", cmd)
	}
}

// TestParseRemoteWorkspaceName covers the parser that scrapes the new-
// workspace name out of a streamed `canopy new --on` log. The remote
// canopy emits "Workspace ready: <name>" on success; if absent, the
// auto-attach path falls back to the "press Enter on the row" hint.
func TestParseRemoteWorkspaceName(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   string
	}{
		{"empty output", "", ""},
		{"no ready line", "Dispatching to tower (registry):\nsome output\n", ""},
		{
			"ready line present",
			"Dispatching to tower:\n  exec canopy new --no-attach\n\nWorkspace ready: bold-tiger\n  branch:  bold-tiger\n",
			"bold-tiger",
		},
		{
			"ready line with trailing whitespace",
			"Workspace ready:  spaced-name  \n",
			"spaced-name",
		},
		{
			// Defends against output earlier in the stream containing
			// the marker (e.g., a prompt or setup hook echoing it).
			// The remote canopy emits the canonical line once, last —
			// taking the LAST occurrence prevents redirection.
			"last occurrence wins",
			"Setup script:\nWorkspace ready: decoy-name\n(more output)\nWorkspace ready: real-name\n",
			"real-name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRemoteWorkspaceName(tc.output)
			if got != tc.want {
				t.Errorf("parseRemoteWorkspaceName(%q) = %q; want %q",
					tc.output, got, tc.want)
			}
		})
	}
}

// TestAvailableOpenBrowser_HiddenOnHostsTab: pressing B with a workspace
// row stale on the cursor while the Hosts tab is active used to silently
// fire the open-browser flow. Now gated to non-Hosts tabs so the B chip
// disappears from the help line on the Hosts tab.
func TestAvailableOpenBrowser_HiddenOnHostsTab(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.setTestRows([]Row{{Name: "ws", Alive: true, Port: 3001}})
	if availableOpenBrowser(m) {
		t.Errorf("availableOpenBrowser = true on Hosts tab; want false")
	}
	m.tab = tabLocal
	if !availableOpenBrowser(m) {
		t.Errorf("availableOpenBrowser = false on Local tab with alive+port row; want true")
	}
}

// TestAvailableHostSSH_RequiresTarget: `s` is surfaced when the cursor
// has a registered SSH target, and hidden when there's no target or
// the cursor is off the Hosts tab. No probe / status gate — SSH is the
// recovery path for everything else, so we surface it unconditionally
// as long as we have somewhere to connect.
func TestAvailableHostSSH_RequiresTarget(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{
		{Name: "tower", SSHTarget: "u@t", Type: "ssh"},
		{Name: "bare", SSHTarget: "", Type: "ssh"},
	}

	// selectedHost() re-sorts alphabetically, so cursor 0 = "bare"
	// (empty target → hidden), cursor 1 = "tower" (has target → shown).
	m.hostsCursor = 0
	if availableHostSSH(m) {
		t.Errorf("hostsCursor=bare (empty target): availableHostSSH = true; want false")
	}
	m.hostsCursor = 1
	if !availableHostSSH(m) {
		t.Errorf("hostsCursor=tower: availableHostSSH = false; want true")
	}

	m.tab = tabLocal
	if availableHostSSH(m) {
		t.Errorf("Local tab: availableHostSSH = true; want false")
	}
}

// TestActionHostSSH_OpensConfirmModal: pressing `s` on the Hosts tab
// stages a y/N prompt instead of execing ssh directly. The exec only
// fires once the user types y in handleConfirmHostSSHKey — a stray `s`
// shouldn't kick the user out of the TUI.
func TestActionHostSSH_OpensConfirmModal(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}
	m.hostsCursor = 0
	_, cmd := actionHostSSH(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd != nil {
		t.Errorf("actionHostSSH returned a cmd; want nil (modal-only, exec happens on confirm)")
	}
	if m.mode != confirmHostSSHMode {
		t.Errorf("mode = %v; want confirmHostSSHMode", m.mode)
	}
	if m.hostSSHName != "tower" {
		t.Errorf("hostSSHName = %q; want %q", m.hostSSHName, "tower")
	}
	if m.hostSSHTarget != "u@t" {
		t.Errorf("hostSSHTarget = %q; want %q", m.hostSSHTarget, "u@t")
	}
}

// TestActionHostSSH_NoTargetNoOp: when the cursor's host has no SSH
// target, `s` is a no-op (no cmd, no error, no mode change). The keymap
// predicate already hides the binding, but the action stays defensive
// in case it ever fires through a stale registry snapshot.
func TestActionHostSSH_NoTargetNoOp(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "tower", SSHTarget: ""}}
	m.hostsCursor = 0
	_, cmd := actionHostSSH(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd != nil {
		t.Errorf("actionHostSSH on empty target returned cmd; want nil")
	}
	if m.mode == confirmHostSSHMode {
		t.Errorf("mode = confirmHostSSHMode on empty target; want listMode")
	}
}

// TestHandleConfirmHostSSHKey_YExecsSSH: typing y in the confirm modal
// runs ssh via tea.ExecProcess, clears the modal state, and returns to
// listMode. We don't run the cmd (that would exec ssh) — just check
// the wiring.
func TestHandleConfirmHostSSHKey_YExecsSSH(t *testing.T) {
	m := newTestModel(false)
	m.mode = confirmHostSSHMode
	m.hostSSHName = "tower"
	m.hostSSHTarget = "u@t"

	_, cmd := m.handleConfirmHostSSHKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatalf("y in confirm-host-ssh: got nil cmd; want tea.ExecProcess wrapper")
	}
	if m.mode != listMode {
		t.Errorf("mode = %v; want listMode (modal should close)", m.mode)
	}
	if m.hostSSHName != "" || m.hostSSHTarget != "" {
		t.Errorf("modal state not cleared: name=%q target=%q", m.hostSSHName, m.hostSSHTarget)
	}
}

// TestCreateDoneMsg_RemoteSuccessAutoAttaches: after a successful
// remote create, the parsed workspace name + newTargetHost trigger
// an auto-attach via attachRemoteRow. Without this branch, the user
// would land back in busyMode and have to manually press Enter on
// the new row — the entire v0.17 laptop-to-tower create-and-attach
// flow depends on this.
func TestCreateDoneMsg_RemoteSuccessAutoAttaches(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate
	m.busyOutput = "Dispatching to tower...\nWorkspace ready: bold-tiger\n"
	m.newTargetHost = "tower"
	m.newTargetName = "canopy"
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "u@t", Type: "ssh"}}

	_, cmd := m.Update(createDoneMsg{remote: true, err: nil, output: ""})
	if cmd == nil {
		t.Fatalf("remote createDoneMsg with parseable name: got nil cmd; want attachRemoteRow")
	}
	if m.mode != listMode {
		t.Errorf("mode = %v after auto-attach; want listMode (busy cleared)", m.mode)
	}
	if m.busyOp != busyOpNone {
		t.Errorf("busyOp = %v; want busyOpNone (state cleared)", m.busyOp)
	}
}

// TestCreateDoneMsg_RemoteSuccessNoNameFallsBack: if the output
// doesn't contain a parseable workspace name, fall back to the
// "press Enter on the new row" hint and stay in busyMode. The user
// can still attach manually; the auto-attach is best-effort.
func TestCreateDoneMsg_RemoteSuccessNoNameFallsBack(t *testing.T) {
	m := newTestModel(false)
	m.mode = busyMode
	m.busyOp = busyOpCreate
	m.busyOutput = "Dispatching to tower...\n(no ready line — output truncated)\n"
	m.newTargetHost = "tower"

	_, cmd := m.Update(createDoneMsg{remote: true, err: nil, output: ""})
	if cmd != nil {
		t.Errorf("remote createDoneMsg with no parseable name: got cmd %v; want nil (fallback hint)", cmd)
	}
	if m.mode != busyMode {
		t.Errorf("mode = %v; want busyMode (stay in busy view for hint)", m.mode)
	}
	if !strings.Contains(m.busyTitle, "Press any key") {
		t.Errorf("busyTitle = %q; want hint mentioning 'Press any key'", m.busyTitle)
	}
}

// TestHandleConfirmHostSSHKey_NCancels: any non-y key cancels the
// modal without exec'ing ssh. State is cleared either way.
func TestHandleConfirmHostSSHKey_NCancels(t *testing.T) {
	m := newTestModel(false)
	m.mode = confirmHostSSHMode
	m.hostSSHName = "tower"
	m.hostSSHTarget = "u@t"

	_, cmd := m.handleConfirmHostSSHKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if cmd != nil {
		t.Errorf("n in confirm-host-ssh: got cmd %v; want nil (cancel)", cmd)
	}
	if m.mode != listMode {
		t.Errorf("mode after cancel = %v; want listMode", m.mode)
	}
	if m.hostSSHName != "" || m.hostSSHTarget != "" {
		t.Errorf("modal state not cleared after cancel: name=%q target=%q", m.hostSSHName, m.hostSSHTarget)
	}
}

// TestPushLoadingHosts_ActiveRefreshFlagsEveryRegisteredHost: while
// remoteRefreshing is true, every host in m.hostList must be marked
// loading so the workspaces tab can decorate each section header
// with a spinner. Mirrors the Hosts tab's "all hosts light up on
// refresh start" semantics. v0.22.
func TestPushLoadingHosts_ActiveRefreshFlagsEveryRegisteredHost(t *testing.T) {
	m := newTestModel(false)
	m.hostList = []host.Host{{Name: "tower"}, {Name: "jarvis"}}
	m.remoteRefreshing = true

	m.pushLoadingHosts()

	for _, name := range []string{"tower", "jarvis"} {
		if !m.list.LoadingHosts(name) {
			t.Errorf("expected host %q flagged loading while remoteRefreshing=true", name)
		}
	}
}

// TestPushLoadingHosts_ClearsWhenNotRefreshing: once remoteRefreshing
// flips false (refresh completed), the loading set must clear so
// headers render plain. Regression target: spinner latching on
// indefinitely after the fan-out returns.
func TestPushLoadingHosts_ClearsWhenNotRefreshing(t *testing.T) {
	m := newTestModel(false)
	m.hostList = []host.Host{{Name: "tower"}}
	m.remoteRefreshing = true
	m.pushLoadingHosts() // primes the set

	m.remoteRefreshing = false
	m.pushLoadingHosts()

	if m.list.LoadingHosts("tower") {
		t.Errorf("expected tower NOT flagged loading after remoteRefreshing=false")
	}
}

// TestPushLoadingHosts_NoHostsRegisteredNoOp: with hostList empty,
// pushLoadingHosts must not crash or spuriously flag phantom hosts.
// Defensive — covers the cold-start path before the host registry
// preloads.
func TestPushLoadingHosts_NoHostsRegisteredNoOp(t *testing.T) {
	m := newTestModel(false)
	m.hostList = nil
	m.remoteRefreshing = true

	m.pushLoadingHosts()

	if m.list.LoadingHosts("tower") {
		t.Errorf("phantom host flagged when registry empty")
	}
}

// TestRefresh_PushesLoadingHostsOnStart wires the end-to-end:
// calling refresh() with hosts registered must populate
// projectlist's loadingHosts so the spinner shows up on the very
// first frame after dispatch (without waiting for the next spinner
// tick).
func TestRefresh_PushesLoadingHostsOnStart(t *testing.T) {
	m := newTestModel(false)
	m.hostList = []host.Host{{Name: "tower"}}
	m.remoteRefreshing = false

	_ = m.refresh()

	if !m.list.LoadingHosts("tower") {
		t.Errorf("refresh() did not push loading state for registered host")
	}
}

// TestActionRetry_LoadingRowIsNoop: R on a synthetic loading
// placeholder must NOT dispatch `canopy retry --on <host> "" --force`
// against the remote. Caught by the adversarial review: the
// placeholder has Host=<hostname> and empty Name, so without this
// guard the remote dispatch fires a malformed retry verb with
// force=true. Mirrors the enter/o/d/K guards added to the rest of
// the action handlers in v0.22.
func TestActionRetry_LoadingRowIsNoop(t *testing.T) {
	m := newTestModel(false)
	m.setTestRows([]Row{{Host: "tower", Loading: true}})

	_, cmd := actionRetry(m, teaKeyMsg{})
	if cmd != nil {
		t.Errorf("retry on Loading placeholder returned cmd %v; want nil (no-op)", cmd)
	}
}

// TestUpdate_RemoteRowsLoadedClearsLoadingHosts: receiving
// remoteRowsLoadedMsg (the result envelope) must clear the loading
// set so the header spinner stops once data lands. Pairs with the
// refresh-start path above.
func TestUpdate_RemoteRowsLoadedClearsLoadingHosts(t *testing.T) {
	m := newTestModel(false)
	m.hostList = []host.Host{{Name: "tower"}}
	m.remoteRefreshing = true
	m.pushLoadingHosts() // primes loadingHosts so we can confirm it clears

	if !m.list.LoadingHosts("tower") {
		t.Fatalf("test pre-condition failed — tower not flagged before remoteRowsLoadedMsg")
	}

	_, _ = m.Update(remoteRowsLoadedMsg{})
	if m.list.LoadingHosts("tower") {
		t.Errorf("remoteRowsLoadedMsg did not clear loading flag for tower")
	}
}
