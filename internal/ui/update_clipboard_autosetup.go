// v0.22.x: wires the v0.18 clipboard bridge into `canopy --remote
// <host>` thin-client mode. Ordinary multi-host usage (the Hosts tab)
// keeps its explicit `c` keybind — Premise 1 in
// docs/design/v0.18-clipboard-bridge.md is deliberate per-host opt-in
// there, and update_host_clipboard.go is unchanged. A `--remote
// <host>` thin-client session has no Hosts tab at all (visibleTabs()
// suppresses it — see NewRemotePinned), so without this a thin-client
// user would never discover the `c` key exists and the bridge would
// silently stay off forever.
//
// maybeAutoSetupClipboardBridge auto-installs once per pinned session,
// the first time a clean remote snapshot confirms the bridge isn't
// already on. Unlike the `c` keybind's tea.ExecProcess + confirm-modal
// flow, this runs in the background with no confirmation — an
// unattended action shouldn't hijack the user's terminal — and
// surfaces only success/failure via a transient notice banner (see
// view.go).

package ui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/clipboard"
	"github.com/avinashjoshi/canopy/internal/host"
)

// clipboardAutoSetupNoticeDuration is how long the result banner stays
// on screen before auto-expiring. Mirrors addProjectToastExpireMsg's
// transient-confirmation UX.
const clipboardAutoSetupNoticeDuration = 6 * time.Second

// clipboardAutoSetupSupportedOS reports whether this platform is a
// candidate for clipboard-bridge auto-setup. Phase 1 of the bridge is
// Wayland-laptop-only (see the design doc's "Constraints" section);
// var-swapped (mirrors cmd/canopy/main.go's runGitBranchShowCurrent
// pattern) so tests can exercise both branches without needing to
// cross-compile.
var clipboardAutoSetupSupportedOS = func() bool { return runtime.GOOS == "linux" }

// maybeAutoSetupClipboardBridge inspects the just-landed remote
// snapshot for m.pinnedHost and, the first time it's known to be
// reachable, either skips (already bridged) or kicks off the
// background install. Latches clipboardAutoSetupTried regardless of
// outcome so repeated ~2s refresh ticks never retry mid-session.
//
// Phase 1 of the clipboard bridge is Linux-only (Wayland laptop side —
// see the design doc's "Constraints" section); skip entirely on other
// platforms so macOS/Windows thin-client users never see a doomed
// install attempt or its failure banner.
func (m *Model) maybeAutoSetupClipboardBridge() tea.Cmd {
	if m.pinnedHost.Name == "" || m.clipboardAutoSetupTried {
		return nil
	}
	if !clipboardAutoSetupSupportedOS() {
		m.clipboardAutoSetupTried = true
		return nil
	}
	snap, ok := m.remoteSnaps[m.pinnedHost.Name]
	if !ok || snap == nil || snap.LastError != "" {
		// Host not reachable yet (or errored) — don't latch; the next
		// refresh tick gets another chance once the host comes online.
		return nil
	}
	m.clipboardAutoSetupTried = true
	if snap.ClipboardBridge == "bridged" {
		return nil
	}
	version := m.versionLabel
	if version == "" {
		version = "dev"
	}
	m.clipboardAutoSetupNotice = fmt.Sprintf("📋 setting up clipboard bridge on %s...", m.pinnedHost.Name)
	m.clipboardAutoSetupNoticeFor = time.Time{}
	return clipboardAutoSetupCmd(m.pinnedHost, version)
}

// clipboardAutoSetupMsg carries the result of the background install
// kicked off by maybeAutoSetupClipboardBridge.
type clipboardAutoSetupMsg struct {
	host string
	err  error
}

// clipboardAutoSetupNoticeExpireMsg fires when the notice banner's
// display window closes.
type clipboardAutoSetupNoticeExpireMsg struct{}

// clipboardLocalInstaller and clipboardHostInstaller are the minimal
// interfaces clipboardAutoSetupCmd needs out of *clipboard.LocalInstaller
// and *clipboard.HostInstaller respectively. Both concrete types already
// satisfy these structurally — no changes to internal/clipboard needed.
// Existing purely to give tests an injection seam (see
// newClipboardLocalInstaller / newClipboardHostInstaller below): a real
// LocalInstaller.Install() writes a systemd unit + edits ~/.ssh/config,
// and a real HostInstaller.InstallOnHost() SSHes to a remote host —
// neither is something a unit test should do.
type clipboardLocalInstaller interface {
	Install(out io.Writer) error
}
type clipboardHostInstaller interface {
	InstallOnHost(ctx context.Context, hostName, sshTarget string, out io.Writer) error
}

// newClipboardLocalInstaller / newClipboardHostInstaller are var-swapped
// constructors (same pattern as clipboardAutoSetupSupportedOS above)
// wrapping clipboard.NewLocalInstaller / clipboard.NewHostInstaller.
var (
	newClipboardLocalInstaller = func() (clipboardLocalInstaller, error) {
		return clipboard.NewLocalInstaller()
	}
	newClipboardHostInstaller = func(version string) (clipboardHostInstaller, error) {
		return clipboard.NewHostInstaller(version)
	}
)

// clipboardAutoSetupCmd runs the laptop-side one-time bootstrap
// (clipboard.LocalInstaller — systemd unit, SSH config Include) THEN
// the per-host install (clipboard.HostInstaller) — together, the full
// "canopy install clipboard-bridge" + "canopy host clipboard <name>"
// sequence a manual setup would require, run unattended so `--remote
// <host>` needs neither step run by hand. Both installers are
// idempotent (safe to re-run every session).
//
// h.Name may be an unregistered raw --remote target shaped like
// `user@host:port` (no `canopy host add` required — that's the whole
// point of wiring this into thin-client mode). clipboard.SanitizeArtifactName
// keys the on-disk SSH snippet + systemd tunnel unit off a filesystem/
// systemd-safe name instead of the raw spec.
//
// clipboardAutoSetupTimeout bounds InstallOnHost's SSH round-trips
// (id -u, mkdir, wrapper pushes, the tmux-config splice, the verify
// probe — each already fails fast on auth via host.SSHCmdBatch after
// the v0.22.x fix, but a genuinely unreachable host can still hang at
// the TCP level well past ssh's own ConnectTimeout=5 under some network
// conditions). This is a partial mitigation, not a complete one: the
// systemd steps inside both LocalInstaller.Install and
// HostInstaller.EnsureTunnelUnit shell out via plain exec.Command with
// no context parameter at all (a pre-existing internal/clipboard
// limitation, not introduced here), so a wedged `systemctl` call is
// still unbounded even with this timeout in place. Run unattended in
// the background (this is the whole point of auto-setup — see this
// file's header comment), so a bound here at least keeps a single
// pinned `--remote` session from hanging on the SSH portion forever
// rather than eventually surfacing the failure banner.
const clipboardAutoSetupTimeout = 90 * time.Second

// Transcript output is discarded on success; logged at Warn on failure
// so the reason is still recoverable from ~/.canopy/log/canopy.log
// even though the notice banner only has room for one line.
func clipboardAutoSetupCmd(h host.Host, version string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		local, err := newClipboardLocalInstaller()
		if err != nil {
			return clipboardAutoSetupMsg{host: h.Name, err: fmt.Errorf("local bootstrap: %w", err)}
		}
		if err := local.Install(&buf); err != nil {
			log.Warn("ui.clipboard.autosetup-local-failed", "host", h.Name, "err", err, "transcript", buf.String())
			return clipboardAutoSetupMsg{host: h.Name, err: fmt.Errorf("local bootstrap: %w", err)}
		}

		installer, err := newClipboardHostInstaller(version)
		if err != nil {
			return clipboardAutoSetupMsg{host: h.Name, err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), clipboardAutoSetupTimeout)
		defer cancel()
		artifactName := clipboard.SanitizeArtifactName(h.Name)
		if err := installer.InstallOnHost(ctx, artifactName, h.SSHTarget, &buf); err != nil {
			log.Warn("ui.clipboard.autosetup-host-failed", "host", h.Name, "err", err, "transcript", buf.String())
			return clipboardAutoSetupMsg{host: h.Name, err: err}
		}
		return clipboardAutoSetupMsg{host: h.Name}
	}
}
