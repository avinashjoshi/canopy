package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
)

// TestActionHostInstall_EntersConfirmingState verifies that pressing
// I on a Hosts-tab row primes the same state machine as U / S, but
// with install-flavored labels and the curl|bash remote payload.
//
// Regression target: action / verb / remoteCmd are all sourced from
// the install opts; a slip-up that left them blank would render
// upgrade-flavored copy AND dispatch the wrong remote command.
func TestActionHostInstall_EntersConfirmingState(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "tower", SSHTarget: "avi@tower"}}
	m.hostsCursor = 0
	// Snapshot present but with a "canopy not installed" error — this
	// is the canonical "I should be visible here" host. The action
	// itself doesn't gate on snapshot state (availableOnHostsTab does
	// the cursor check), so the field shapes here just feed the
	// confirm screen.
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {LastError: "canopy: not found", CanopyVersion: ""},
	}

	model, cmd := actionHostInstall(m, tea.KeyMsg{})
	mm := model.(*Model)

	if mm.mode != hostUpgradeMode {
		t.Errorf("mode = %v; want hostUpgradeMode (shared state machine)", mm.mode)
	}
	if mm.hostUpgradeState != hostUpgradeStateConfirming {
		t.Errorf("state = %v; want confirming", mm.hostUpgradeState)
	}
	if mm.hostUpgradeAction != "install" {
		t.Errorf("action = %q; want 'install'", mm.hostUpgradeAction)
	}
	if mm.hostUpgradeVerb != "Installing" {
		t.Errorf("verb = %q; want 'Installing'", mm.hostUpgradeVerb)
	}
	if mm.hostUpgradeHost != "tower" {
		t.Errorf("host = %q; want tower", mm.hostUpgradeHost)
	}
	if mm.hostUpgradeTarget != "avi@tower" {
		t.Errorf("target = %q; want avi@tower", mm.hostUpgradeTarget)
	}
	// Remote command must be the curl|bash payload, NOT a canopy
	// subcommand — install runs on hosts where canopy isn't there yet.
	if !strings.Contains(mm.hostUpgradeRemoteCmd, "install.sh") {
		t.Errorf("remoteCmd must reference install.sh; got %q", mm.hostUpgradeRemoteCmd)
	}
	if !strings.Contains(mm.hostUpgradeRemoteCmd, "--yes") {
		t.Errorf("remoteCmd must pass --yes (non-interactive); got %q", mm.hostUpgradeRemoteCmd)
	}
	if strings.Contains(mm.hostUpgradeRemoteCmd, "canopy upgrade") {
		t.Errorf("remoteCmd leaked upgrade verb; got %q", mm.hostUpgradeRemoteCmd)
	}
	if cmd != nil {
		t.Errorf("entering confirm should NOT dispatch SSH yet (only y/Enter does)")
	}
}

// TestActionHostInstall_NoTargetSkips guards the SSHTarget == ""
// branch in enterHostUpgrade. A host registered with an empty target
// (shouldn't be possible via the validator, but the wizard can hand
// us a synthetic one) must not enter the install flow.
func TestActionHostInstall_NoTargetSkips(t *testing.T) {
	m := newTestModel(false)
	m.tab = tabHosts
	m.hostList = []host.Host{{Name: "broken", SSHTarget: ""}}
	m.hostsCursor = 0

	model, cmd := actionHostInstall(m, tea.KeyMsg{})
	mm := model.(*Model)

	if mm.mode == hostUpgradeMode {
		t.Errorf("mode should NOT enter hostUpgradeMode when ssh target is empty")
	}
	if cmd != nil {
		t.Errorf("no Cmd expected when target is empty; got non-nil")
	}
}

// TestRenderHostUpgrade_InstallConfirm checks the confirm screen
// renders install-friendly copy (not the upgrade-flavored back-tick
// command line) AND suppresses the `current: v…` line when no version
// is known. Regression target: rendering `current: v` (trailing empty
// version) is the visible artifact a future renderer change might
// accidentally reintroduce.
func TestRenderHostUpgrade_InstallConfirm(t *testing.T) {
	m := newTestModel(false)
	m.mode = hostUpgradeMode
	m.hostUpgradeState = hostUpgradeStateConfirming
	m.hostUpgradeHost = "tower"
	m.hostUpgradeTarget = "avi@tower"
	m.hostUpgradeAction = "install"
	m.hostUpgradeVerb = "Installing"
	m.hostUpgradeSuccess = "Install complete"
	m.hostUpgradeVersion = "" // host has no canopy yet

	out := m.renderHostUpgrade()
	if !strings.Contains(out, "Install canopy on this host?") {
		t.Errorf("install confirm screen should show install-friendly prompt; got:\n%s", out)
	}
	if strings.Contains(out, "Run `canopy install` on this host?") {
		t.Errorf("install screen must NOT render the back-tick `canopy install` line (no such subcommand); got:\n%s", out)
	}
	if strings.Contains(out, "current: v\n") || strings.Contains(out, "current: v ") {
		t.Errorf("empty version must suppress the `current:` line; got:\n%s", out)
	}
	// The target line should still render — it's the load-bearing
	// "where will this run" hint.
	if !strings.Contains(out, "avi@tower") {
		t.Errorf("confirm screen must show ssh target; got:\n%s", out)
	}
}

// TestRenderHostUpgrade_InstallConfirmWithKnownVersion: when the host
// IS already running canopy and the user presses I to reinstall, we
// SHOULD show the current version (so they know what they're about
// to overwrite). This is the symmetric case of the test above.
func TestRenderHostUpgrade_InstallConfirmWithKnownVersion(t *testing.T) {
	m := newTestModel(false)
	m.mode = hostUpgradeMode
	m.hostUpgradeState = hostUpgradeStateConfirming
	m.hostUpgradeHost = "tower"
	m.hostUpgradeTarget = "avi@tower"
	m.hostUpgradeAction = "install"
	m.hostUpgradeVerb = "Installing"
	m.hostUpgradeVersion = "0.17.1.0"

	out := m.renderHostUpgrade()
	if !strings.Contains(out, "current: v0.17.1.0") {
		t.Errorf("known version must surface in `current:` line; got:\n%s", out)
	}
}

// TestAvailableOnHostsTab_InstallVisibility confirms the install
// binding is available when the Hosts tab is active AND a row is
// selectable, and hidden everywhere else. The I keybind uses the
// shared availableOnHostsTab predicate, so this test doubles as
// coverage of "install never bleeds into the workspace tabs."
func TestAvailableOnHostsTab_InstallVisibility(t *testing.T) {
	cases := []struct {
		name    string
		tab     int
		hosts   []host.Host
		want    bool
		comment string
	}{
		{"hosts tab with rows", int(tabHosts), []host.Host{{Name: "tower"}}, true, "I shows when Hosts tab has at least one row"},
		{"hosts tab empty", int(tabHosts), nil, false, "I hides when there are no host rows to act on"},
		{"local tab", int(tabLocal), []host.Host{{Name: "tower"}}, false, "I never bleeds into Local tab"},
		{"global tab", int(tabGlobal), []host.Host{{Name: "tower"}}, false, "I never bleeds into Global tab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(false)
			m.tab = tabKind(tc.tab)
			m.hostList = tc.hosts
			got := availableOnHostsTab(m)
			if got != tc.want {
				t.Errorf("%s — availableOnHostsTab = %v; want %v", tc.comment, got, tc.want)
			}
		})
	}
}
