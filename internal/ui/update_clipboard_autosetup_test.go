package ui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/clipboard"
	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
)

// fakeClipboardHostInstaller is the test double for clipboardAutoSetupCmd's
// injection seam (newClipboardHostInstaller). A recording fake so tests
// can assert not just the returned error but how InstallOnHost was
// called.
type fakeClipboardHostInstaller struct {
	installOnHostErr error
	calledWith       struct {
		hostName, sshTarget string
	}
}

func (f *fakeClipboardHostInstaller) InstallOnHost(_ context.Context, hostName, sshTarget string, _ io.Writer) error {
	f.calledWith.hostName = hostName
	f.calledWith.sshTarget = sshTarget
	return f.installOnHostErr
}

// withFakeClipboardInstallers swaps newClipboardHostInstaller for the
// given fake (or constructor error) for the duration of the test.
func withFakeClipboardInstallers(t *testing.T, hostInstaller *fakeClipboardHostInstaller, hostErr error) {
	t.Helper()
	origHost := newClipboardHostInstaller
	newClipboardHostInstaller = func(string) (clipboardHostInstaller, error) {
		if hostErr != nil {
			return nil, hostErr
		}
		return hostInstaller, nil
	}
	t.Cleanup(func() {
		newClipboardHostInstaller = origHost
	})
}

// TestClipboardAutoSetupCmd_HostConstructorError: clipboard.NewHostInstaller
// fails (e.g. ssh not on PATH) — the error must surface unwrapped (it's
// already descriptive from the constructor).
func TestClipboardAutoSetupCmd_HostConstructorError(t *testing.T) {
	withFakeClipboardInstallers(t, nil, errors.New("ssh not on PATH"))

	msg := clipboardAutoSetupCmd(host.Host{Name: "tower", SSHTarget: "avi@tower"}, "v1.0.0")().(clipboardAutoSetupMsg)
	if msg.err == nil || !strings.Contains(msg.err.Error(), "ssh not on PATH") {
		t.Errorf("err = %v; want the host constructor's error surfaced", msg.err)
	}
}

// TestClipboardAutoSetupCmd_HostInstallFailure: the constructor
// succeeds but InstallOnHost itself fails (e.g. SSH auth failure) —
// the error must surface, and the artifact name passed to InstallOnHost
// must be the SANITIZED name, not the raw (possibly messy) host.Name.
func TestClipboardAutoSetupCmd_HostInstallFailure(t *testing.T) {
	hostInstaller := &fakeClipboardHostInstaller{installOnHostErr: errors.New("ssh: connection refused")}
	withFakeClipboardInstallers(t, hostInstaller, nil)

	h := host.Host{Name: "avi@tower.example.com", SSHTarget: "avi@tower.example.com"}
	msg := clipboardAutoSetupCmd(h, "v1.0.0")().(clipboardAutoSetupMsg)
	if msg.err == nil || !strings.Contains(msg.err.Error(), "connection refused") {
		t.Errorf("err = %v; want InstallOnHost's error surfaced", msg.err)
	}
	wantName := clipboard.SanitizeArtifactName(h.Name)
	if hostInstaller.calledWith.hostName != wantName {
		t.Errorf("InstallOnHost hostName = %q; want %q (clipboard.SanitizeArtifactName(h.Name))", hostInstaller.calledWith.hostName, wantName)
	}
	if hostInstaller.calledWith.hostName == h.Name {
		t.Errorf("InstallOnHost was called with the RAW host name %q; want the sanitized form", h.Name)
	}
	if hostInstaller.calledWith.sshTarget != h.SSHTarget {
		t.Errorf("InstallOnHost sshTarget = %q; want %q (unsanitized — the real SSH target)", hostInstaller.calledWith.sshTarget, h.SSHTarget)
	}
}

// TestClipboardAutoSetupCmd_FullSuccessPath: everything succeeds — the
// message carries no error and the right host name.
func TestClipboardAutoSetupCmd_FullSuccessPath(t *testing.T) {
	hostInstaller := &fakeClipboardHostInstaller{}
	withFakeClipboardInstallers(t, hostInstaller, nil)

	msg := clipboardAutoSetupCmd(host.Host{Name: "tower", SSHTarget: "avi@tower"}, "v1.0.0")().(clipboardAutoSetupMsg)
	if msg.err != nil {
		t.Errorf("err = %v; want nil on full success", msg.err)
	}
	if msg.host != "tower" {
		t.Errorf("host = %q; want %q", msg.host, "tower")
	}
	if hostInstaller.calledWith.hostName != "tower" {
		t.Errorf("InstallOnHost hostName = %q; want %q (clean name, no sanitization needed)", hostInstaller.calledWith.hostName, "tower")
	}
}

// TestMaybeAutoSetupClipboardBridge_NotPinnedIsNoop: ordinary
// multi-host mode (m.pinnedHost.Name == "") must never trigger
// auto-setup — that mode keeps the Hosts tab's explicit `c` keybind
// (Premise 1 in docs/design/v0.18-clipboard-bridge.md: per-host
// opt-in). Also must not latch clipboardAutoSetupTried.
func TestMaybeAutoSetupClipboardBridge_NotPinnedIsNoop(t *testing.T) {
	m := newTestModel(false)
	// m.pinnedHost is the zero value (Name == "") by default.

	if cmd := m.maybeAutoSetupClipboardBridge(); cmd != nil {
		t.Error("expected nil cmd when not in pinned thin-client mode")
	}
	if m.clipboardAutoSetupTried {
		t.Error("clipboardAutoSetupTried should stay false outside pinned mode")
	}
}

// TestMaybeAutoSetupClipboardBridge_NoSnapshotYetDoesNotLatch: before
// the pinned host's first refresh has landed (or after one that
// errored — host offline), the function must return nil WITHOUT
// latching clipboardAutoSetupTried, so the next refresh tick gets
// another chance once the host is actually reachable.
func TestMaybeAutoSetupClipboardBridge_NoSnapshotYetDoesNotLatch(t *testing.T) {
	m := newTestModel(false)
	m.pinnedHost = host.Host{Name: "tower", SSHTarget: "avi@tower"}

	if cmd := m.maybeAutoSetupClipboardBridge(); cmd != nil {
		t.Error("expected nil cmd with no snapshot yet")
	}
	if m.clipboardAutoSetupTried {
		t.Error("clipboardAutoSetupTried should NOT latch before a clean snapshot is known")
	}

	// A snapshot that errored (host offline / auth failure) is the
	// same "not reachable yet" case — still must not latch.
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {LastError: "Permission denied (publickey)"},
	}
	if cmd := m.maybeAutoSetupClipboardBridge(); cmd != nil {
		t.Error("expected nil cmd when the snapshot carries LastError")
	}
	if m.clipboardAutoSetupTried {
		t.Error("clipboardAutoSetupTried should NOT latch on an errored snapshot")
	}
}

// TestMaybeAutoSetupClipboardBridge_AlreadyBridgedSkipsButLatches: a
// clean snapshot reporting the bridge is already up must skip the
// install (no wasted SSH round-trips) but still latch so later ticks
// don't re-check.
func TestMaybeAutoSetupClipboardBridge_AlreadyBridgedSkipsButLatches(t *testing.T) {
	m := newTestModel(false)
	m.pinnedHost = host.Host{Name: "tower", SSHTarget: "avi@tower"}
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {ClipboardBridge: "bridged"},
	}

	if cmd := m.maybeAutoSetupClipboardBridge(); cmd != nil {
		t.Error("expected nil cmd when the bridge is already up")
	}
	if !m.clipboardAutoSetupTried {
		t.Error("clipboardAutoSetupTried should latch true once a clean snapshot is known")
	}
	if m.clipboardAutoSetupNotice != "" {
		t.Errorf("notice = %q; want empty (no install was attempted)", m.clipboardAutoSetupNotice)
	}
}

// TestMaybeAutoSetupClipboardBridge_OffTriggersInstall: the core
// "auto-setup on first connect" case — a clean snapshot reporting the
// bridge is off must kick off the background install (non-nil cmd),
// latch immediately (so a fast-arriving second refresh tick doesn't
// double-fire it), and set the "in progress" notice.
func TestMaybeAutoSetupClipboardBridge_OffTriggersInstall(t *testing.T) {
	m := newTestModel(false)
	m.pinnedHost = host.Host{Name: "tower", SSHTarget: "avi@tower"}
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {ClipboardBridge: "off"},
	}

	cmd := m.maybeAutoSetupClipboardBridge()
	if cmd == nil {
		t.Fatal("expected a non-nil cmd to kick off the background install")
	}
	if !m.clipboardAutoSetupTried {
		t.Error("clipboardAutoSetupTried should latch true immediately, before the async install even runs")
	}
	if m.clipboardAutoSetupNotice == "" {
		t.Error("expected an in-progress notice to be set")
	}
}

// TestMaybeAutoSetupClipboardBridge_EmptyBridgeStateTriggersInstall:
// an older remote canopy that predates the ClipboardBridge probe field
// reports it as "" (omitempty), not "off" — the field's own doc
// comment in internal/state/remotes_cache.go notes JSON decoding
// leaves it empty when absent. That must be treated the same as "off",
// not skipped as if already bridged.
func TestMaybeAutoSetupClipboardBridge_EmptyBridgeStateTriggersInstall(t *testing.T) {
	m := newTestModel(false)
	m.pinnedHost = host.Host{Name: "tower", SSHTarget: "avi@tower"}
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {ClipboardBridge: ""},
	}

	cmd := m.maybeAutoSetupClipboardBridge()
	if cmd == nil {
		t.Fatal("expected a non-nil cmd for an empty (unknown) ClipboardBridge state")
	}
}

// TestMaybeAutoSetupClipboardBridge_AlreadyTriedIsNoop: once latched,
// subsequent calls within the same session (e.g. every ~2s refresh
// tick) must be a pure no-op, regardless of what the snapshot says.
func TestMaybeAutoSetupClipboardBridge_AlreadyTriedIsNoop(t *testing.T) {
	m := newTestModel(false)
	m.pinnedHost = host.Host{Name: "tower", SSHTarget: "avi@tower"}
	m.clipboardAutoSetupTried = true
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {ClipboardBridge: "off"},
	}

	if cmd := m.maybeAutoSetupClipboardBridge(); cmd != nil {
		t.Error("expected nil cmd once clipboardAutoSetupTried is already true")
	}
}

// TestUpdate_RemoteRowsLoadedWiresClipboardAutoSetup: the
// remoteRowsLoadedMsg handler in Update() must actually forward
// maybeAutoSetupClipboardBridge's non-nil cmd back to Bubbletea (not
// just call the helper and discard the result) — this is the
// end-to-end wiring, distinct from the direct maybeAutoSetupClipboardBridge
// unit tests above which call the helper without going through Update.
func TestUpdate_RemoteRowsLoadedWiresClipboardAutoSetup(t *testing.T) {
	m := newTestModel(false)
	m.pinnedHost = host.Host{Name: "tower", SSHTarget: "avi@tower"}
	m.hostList = []host.Host{m.pinnedHost}
	m.remoteSnaps = map[string]*state.RemoteHostSnapshot{
		"tower": {ClipboardBridge: "off"},
	}

	_, cmd := m.Update(remoteRowsLoadedMsg{})
	if cmd == nil {
		t.Fatal("Update(remoteRowsLoadedMsg{}) returned nil cmd; want the clipboard auto-setup cmd forwarded")
	}
	if !m.clipboardAutoSetupTried {
		t.Error("clipboardAutoSetupTried should latch true after Update dispatches the auto-setup")
	}
	if m.clipboardAutoSetupNotice == "" {
		t.Error("expected an in-progress notice to be set via Update")
	}
}

// TestUpdate_ClipboardAutoSetupMsg_Success covers the clipboardAutoSetupMsg
// switch case in Update() on the success path (err == nil): the notice
// must announce readiness, an expiry deadline must be armed, and a
// tea.Tick cmd must be returned to clear it later.
func TestUpdate_ClipboardAutoSetupMsg_Success(t *testing.T) {
	m := newTestModel(false)

	_, cmd := m.Update(clipboardAutoSetupMsg{host: "tower"})
	if cmd == nil {
		t.Fatal("Update(clipboardAutoSetupMsg{success}) returned nil cmd; want the notice-expire tea.Tick")
	}
	if !strings.Contains(m.clipboardAutoSetupNotice, "tower") {
		t.Errorf("notice = %q; want it to mention the host", m.clipboardAutoSetupNotice)
	}
	if strings.Contains(m.clipboardAutoSetupNotice, "failed") {
		t.Errorf("notice = %q; success path must not say failed", m.clipboardAutoSetupNotice)
	}
	if m.clipboardAutoSetupNoticeFor.IsZero() {
		t.Error("clipboardAutoSetupNoticeFor should be armed (non-zero) after a result lands")
	}
}

// TestUpdate_ClipboardAutoSetupMsg_Failure covers the same switch case
// on the error path: the notice must surface the failure and the host,
// distinct from the success-path copy.
func TestUpdate_ClipboardAutoSetupMsg_Failure(t *testing.T) {
	m := newTestModel(false)

	_, cmd := m.Update(clipboardAutoSetupMsg{host: "tower", err: errors.New("ssh: connection refused")})
	if cmd == nil {
		t.Fatal("Update(clipboardAutoSetupMsg{failure}) returned nil cmd; want the notice-expire tea.Tick")
	}
	if !strings.Contains(m.clipboardAutoSetupNotice, "tower") {
		t.Errorf("notice = %q; want it to mention the host", m.clipboardAutoSetupNotice)
	}
	if !strings.Contains(m.clipboardAutoSetupNotice, "failed") {
		t.Errorf("notice = %q; want it to say the install failed", m.clipboardAutoSetupNotice)
	}
	if !strings.Contains(m.clipboardAutoSetupNotice, "connection refused") {
		t.Errorf("notice = %q; want the underlying error surfaced", m.clipboardAutoSetupNotice)
	}
}

// TestUpdate_ClipboardAutoSetupNoticeExpireMsg covers the second new
// switch case: the expiry message must clear both the notice text and
// the deadline, and return no further cmd.
func TestUpdate_ClipboardAutoSetupNoticeExpireMsg(t *testing.T) {
	m := newTestModel(false)
	m.clipboardAutoSetupNotice = "📋 clipboard bridge ready on tower"

	_, cmd := m.Update(clipboardAutoSetupNoticeExpireMsg{})
	if cmd != nil {
		t.Errorf("Update(clipboardAutoSetupNoticeExpireMsg{}) cmd = %v; want nil", cmd)
	}
	if m.clipboardAutoSetupNotice != "" {
		t.Errorf("notice = %q; want cleared", m.clipboardAutoSetupNotice)
	}
	if !m.clipboardAutoSetupNoticeFor.IsZero() {
		t.Error("clipboardAutoSetupNoticeFor should be reset to zero value")
	}
}

// TestView_ClipboardAutoSetupNotice_RendersWhenSet and its sibling
// below cover view.go's new notice-banner block: it must render the
// notice text when set, and must NOT appear at all (no stray blank
// section) when empty — mirroring the m.err banner immediately above
// it in View().
func TestView_ClipboardAutoSetupNotice_RendersWhenSet(t *testing.T) {
	m := newTestModel(false)
	m.clipboardAutoSetupNotice = "📋 setting up clipboard bridge on tower..."

	out := stripANSIForView(m.View())
	if !strings.Contains(out, "setting up clipboard bridge on tower") {
		t.Errorf("View missing clipboard auto-setup notice:\n%s", out)
	}
}

func TestView_ClipboardAutoSetupNotice_AbsentWhenEmpty(t *testing.T) {
	m := newTestModel(false)
	m.clipboardAutoSetupNotice = ""

	out := stripANSIForView(m.View())
	if strings.Contains(out, "clipboard bridge") {
		t.Errorf("View should not mention the clipboard bridge notice when empty:\n%s", out)
	}
}
