// In-TUI upgrade flow. Triggered by the `U` key on the workspace
// list when the auto-check pill is showing. Four states:
//
//   loading  → CHANGELOG fetch in flight
//   preview  → CHANGELOG visible, "Enter to upgrade, Esc to cancel"
//   running  → git pull + make install streaming live
//   doneOK   → success, "press any key to dismiss"
//   doneError → failure with stderr tail, "press any key to dismiss"
//
// Reuses safeBuffer + progressTick from busyMode for the streaming
// pane — same producer/consumer pattern, no new dep, identical
// rendering shape. The CLI's `canopy upgrade` flow is unchanged: it
// stays plain stdout. The TUI flow is in-TUI only and reachable via
// the U key when an upgrade is available; pressing U with no upgrade
// is a silent no-op.
//
// External integration: cmd/canopy supplies two closures via
// SetUpgradeChangelogFn / SetUpgradeShellFn. The UI doesn't import
// cmd/canopy (cycle); the closures bridge the network and shell
// layers without leaking package internals across the boundary.
package ui

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// upgradeState identifies which step of the in-TUI upgrade flow is
// active. Drives both the renderer (renderUpgrade switches on this)
// and the key handler (handleUpgradeKey gates which keys are bound
// per state).
type upgradeState int

const (
	upgradeStateNone     upgradeState = iota
	upgradeStateLoading               // CHANGELOG fetch in flight
	upgradeStatePreview               // CHANGELOG shown, awaiting confirm
	upgradeStateRunning               // git pull + make install streaming
	upgradeStateDoneOK                // success terminal state
	upgradeStateDoneError             // failure terminal state
)

// UpgradeChangelogFn fetches the CHANGELOG slice between the running
// version and latest. Returns the rendered preview text or "" when
// the fetch fails (best-effort — preview is informational, the flow
// proceeds regardless). Network errors surface for logging.
type UpgradeChangelogFn func(ctx context.Context) (preview string, err error)

// UpgradeShellFn runs `git pull --ff-only && make install` and pipes
// stdout/stderr into the writer. Cancellable via ctx (Ctrl-C in the
// running state). Returns the underlying error on shell failure;
// success is err == nil.
//
// The writer is io.Writer (not *safeBuffer) so cmd/canopy can supply
// the closure without depending on the unexported safeBuffer type.
// Internally we always pass a *safeBuffer (it satisfies io.Writer);
// the seam is just for keeping internal/ui types out of cmd/canopy.
type UpgradeShellFn func(ctx context.Context, w io.Writer) error

// upgradeFlowFields are the per-flow state carried on Model. Grouped
// here for readability — every field resets to zero when the user
// dismisses out of the upgrade flow back to listMode.
//
// Held by value on Model rather than as a *upgradeFlow pointer
// because the lifetime is the same as the model and the field set
// is small. resetUpgrade() returns to zero in one assignment.

// SetUpgradeChangelogFn wires the network closure used for the
// preview state. Pass nil to disable the in-TUI flow entirely
// (pressing U becomes a silent no-op even when the pill shows).
func (m *Model) SetUpgradeChangelogFn(fn UpgradeChangelogFn) {
	m.upgradeChangelogFn = fn
}

// SetUpgradeShellFn wires the shell closure used for the running
// state. Same nil-suppression as SetUpgradeChangelogFn — both must
// be set for the U key to work.
func (m *Model) SetUpgradeShellFn(fn UpgradeShellFn) {
	m.upgradeShellFn = fn
}

// SetUpgradeDismissFn wires the persistence closure for the D key
// (TUI dismissal). Implementation lives in cmd/canopy because the
// cache file path + write logic are package-main details.
func (m *Model) SetUpgradeDismissFn(fn func() error) {
	m.upgradeDismissFn = fn
}

// canEnterUpgradeMode reports whether pressing U should do anything.
// All three closures must be wired AND an upgrade must be available
// AND we must not already be in upgrade mode (re-entering would
// reset state mid-flow).
func (m *Model) canEnterUpgradeMode() bool {
	if m.mode == upgradeMode {
		return false
	}
	if m.upgradeAvailable == "" {
		return false
	}
	return m.upgradeChangelogFn != nil && m.upgradeShellFn != nil
}

// canDismissUpgrade reports whether pressing D should write the
// dismissal cache. Same gate as the upgrade pill plus the dismissal
// closure being wired. Used outside upgradeMode (D from the list
// view) and inside the preview state.
func (m *Model) canDismissUpgrade() bool {
	return m.upgradeAvailable != "" && m.upgradeDismissFn != nil
}

// enterUpgradeMode flips to upgradeMode in the loading state and
// fires the changelog fetch. Caller is responsible for gating via
// canEnterUpgradeMode.
func (m *Model) enterUpgradeMode() tea.Cmd {
	m.mode = upgradeMode
	m.upgradeState = upgradeStateLoading
	m.upgradeChangelog = ""
	m.upgradeOutput = ""
	m.upgradeErr = nil
	m.upgradeBuf = nil
	m.upgradeCancel = nil
	return loadChangelogCmd(m.upgradeChangelogFn)
}

// resetUpgradeMode returns the upgrade fields to zero. Called when
// the user dismisses out of the flow (Esc from preview, any key
// from doneOK/doneError) or re-enters from a clean state.
func (m *Model) resetUpgradeMode() {
	m.upgradeState = upgradeStateNone
	m.upgradeChangelog = ""
	m.upgradeChangelogInit = false
	m.upgradeShipped = ""
	m.upgradeOutput = ""
	m.upgradeErr = nil
	m.upgradeBuf = nil
	if m.upgradeCancel != nil {
		// Best-effort cancel of the running subprocess if a
		// terminal state hasn't fired yet. Idempotent.
		m.upgradeCancel()
	}
	m.upgradeCancel = nil
}

// initUpgradeViewport sizes the changelog scroll pane and loads the
// content. Reserves vertical space for: title (2 lines), version
// header (2 lines), footer hint (2 lines). Width gets a 4-column
// margin so text doesn't crowd the terminal edges.
//
// Falls back to 80x20 when WindowSizeMsg hasn't fired yet — that
// shouldn't happen in practice (Bubbletea sends it on program start
// before any user keypress) but defends against test paths that
// build a *Model literal without dispatching messages.
func (m *Model) initUpgradeViewport() {
	w := m.width - 4
	if w < 20 {
		w = 76
	}
	h := m.height - 8
	if h < 5 {
		h = 16
	}
	m.upgradeChangelogVP = viewport.New(w, h)
	m.upgradeChangelogVP.SetContent(m.upgradeChangelog)
	m.upgradeChangelogInit = true
}

// changelogLoadedMsg lands when loadChangelogCmd's network fetch
// returns. preview is the CHANGELOG slice (may be empty when the
// fetch failed or no slice exists between versions). err is logged
// but does not abort the flow — preview is best-effort; we still
// flip to the preview state so the user can confirm/cancel.
type changelogLoadedMsg struct {
	preview string
	err     error
}

// loadChangelogCmd runs the changelog fetch in a goroutine and emits
// a changelogLoadedMsg. 10s timeout matches upgradeFetchTimeout in
// cmd/canopy — both calls hit the same upstream and have the same
// "interactive, not infinite" tolerance.
func loadChangelogCmd(fn UpgradeChangelogFn) tea.Cmd {
	if fn == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		preview, err := fn(ctx)
		return changelogLoadedMsg{preview: preview, err: err}
	}
}

// upgradeShellStartedMsg is the lazy-spawn bridge — same shape as
// createStartedMsg / removeStartedMsg. Carries the safeBuffer +
// done channel + the cancel func so Update can store the cancel
// reference for Ctrl-C handling.
type upgradeShellStartedMsg struct {
	buf    *safeBuffer
	done   <-chan upgradeShellDoneMsg
	cancel context.CancelFunc
}

// upgradeShellDoneMsg fires when the shell goroutine finishes. err
// is nil on success; non-nil error includes "context canceled" when
// the user hit Ctrl-C.
type upgradeShellDoneMsg struct {
	err    error
	output string // trailing buffer content the final tick missed
}

// runUpgradeShellCmd kicks off the shell flow asynchronously. Same
// structure as createCmd: build a buffer + done chan, run the work
// in a goroutine, return a started-msg so Update can dispatch the
// streaming + completion cmds.
func runUpgradeShellCmd(fn UpgradeShellFn) tea.Cmd {
	if fn == nil {
		return nil
	}
	return func() tea.Msg {
		buf := &safeBuffer{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan upgradeShellDoneMsg, 1)
		go func() {
			err := fn(ctx, buf)
			done <- upgradeShellDoneMsg{err: err, output: buf.Drain()}
		}()
		return upgradeShellStartedMsg{buf: buf, done: done, cancel: cancel}
	}
}

// waitUpgradeShellDoneCmd blocks on the done channel and emits the
// completion msg. Single-shot per upgrade flow. Mirrors
// waitDoneCmd / waitRemoveDoneCmd.
func waitUpgradeShellDoneCmd(done <-chan upgradeShellDoneMsg) tea.Cmd {
	return func() tea.Msg { return <-done }
}

// upgradeProgressTickMsg is the streaming-tail message for the
// upgrade running state. Distinct from progressTickMsg so the
// busyMode handler doesn't accidentally consume our ticks (and
// vice versa).
type upgradeProgressTickMsg struct {
	chunk string
	buf   *safeBuffer
}

// upgradeProgressTickCmd schedules the next stream-drain tick.
// Same 150ms cadence as busyMode's progressTickCmd; the eye reads
// updates at this rate without burning cycles. Stops naturally
// when Update sees we're no longer in the running state.
func upgradeProgressTickCmd(buf *safeBuffer) tea.Cmd {
	return tea.Tick(progressTickInterval, func(time.Time) tea.Msg {
		return upgradeProgressTickMsg{chunk: buf.Drain(), buf: buf}
	})
}

// availableUpgrade gates the U key. Returns true only when:
//   - An upgrade is genuinely available (pill is showing).
//   - Both fetch closures are wired (route.go set them up).
// Avoids surfacing U in the help line when there's nothing to do.
func availableUpgrade(m *Model) bool {
	return m.canEnterUpgradeMode()
}

// availableDismissUpgrade gates the D key. Same idea as
// availableUpgrade but pairs with the dismiss closure.
func availableDismissUpgrade(m *Model) bool {
	return m.canDismissUpgrade()
}

// actionUpgrade is the U-key handler — flips to upgradeMode and
// fires the changelog fetch. The flow takes over the screen until
// the user dismisses out.
func actionUpgrade(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.canEnterUpgradeMode() {
		return m, nil
	}
	return m, m.enterUpgradeMode()
}

// actionDismissUpgrade is the D-key handler — runs the dismissal
// closure and clears the in-memory pill state. Errors are logged
// but not surfaced; dismissal is a "best effort" gesture and a
// failure here just means the pill comes back next invocation.
func actionDismissUpgrade(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.canDismissUpgrade() {
		return m, nil
	}
	if err := m.upgradeDismissFn(); err != nil {
		log.Warn("upgrade.dismiss_failed", "err", err)
	}
	m.upgradeAvailable = ""
	return m, nil
}

// handleUpgradeKey routes key events while mode == upgradeMode.
// Per-state gating: Enter/Esc/D in preview, Ctrl-C in running, any
// key in doneOK/doneError, Esc in loading.
//
// Returns the updated model + tea.Cmd to dispatch. The caller
// (Update) is responsible for the type assertion to tea.Model.
func (m *Model) handleUpgradeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch m.upgradeState {
	case upgradeStateLoading:
		// Loading is fast (one HTTP fetch). Esc cancels back to the
		// list; everything else is ignored so users don't
		// accidentally start the upgrade before the changelog
		// renders.
		if key == "esc" || key == "q" {
			m.resetUpgradeMode()
			m.mode = listMode
			return m, nil
		}
		return m, nil

	case upgradeStatePreview:
		switch key {
		case "enter":
			// Confirm: kick off the shell. Stays in upgradeMode;
			// upgradeShellStartedMsg flips state to running.
			return m, runUpgradeShellCmd(m.upgradeShellFn)
		case "esc", "q":
			m.resetUpgradeMode()
			m.mode = listMode
			return m, nil
		case "d", "D":
			// Dismiss this version + return to list. Mirrors the
			// canopy upgrade --dismiss CLI flag. Failure is
			// non-fatal (logged via the closure).
			if m.upgradeDismissFn != nil {
				if err := m.upgradeDismissFn(); err != nil {
					log.Warn("upgrade.dismiss_failed", "err", err)
				}
			}
			// Clear the pill immediately so re-render doesn't show
			// the arrow until the next 6h refresh notices the
			// dismissal.
			m.upgradeAvailable = ""
			m.resetUpgradeMode()
			m.mode = listMode
			return m, nil
		}
		// All other keys forward to the changelog viewport so
		// j/k/PgUp/PgDn/space/g/G all scroll the preview. Confirm/
		// cancel keys are handled above (they short-circuit) — only
		// scroll-style keys reach here.
		if m.upgradeChangelogInit {
			var cmd tea.Cmd
			m.upgradeChangelogVP, cmd = m.upgradeChangelogVP.Update(msg)
			return m, cmd
		}
		return m, nil

	case upgradeStateRunning:
		// Ctrl-C cancels the subprocess via context. The goroutine
		// returns with a "context canceled" error → flips to
		// doneError. Other keys are ignored to prevent accidental
		// dismissal mid-build.
		if key == "ctrl+c" {
			if m.upgradeCancel != nil {
				m.upgradeCancel()
			}
		}
		return m, nil

	case upgradeStateDoneOK, upgradeStateDoneError:
		// Any key dismisses the terminal screen. Refresh the list
		// on success because the running canopy may have just
		// changed under us (the binary is now newer).
		m.resetUpgradeMode()
		m.mode = listMode
		return m, m.refresh()
	}
	return m, nil
}

// renderUpgrade is the View dispatcher for upgradeMode. Branches on
// upgradeState; every branch returns a complete screen including
// the title and footer. No top-bar pills here — the upgrade flow
// owns the screen end-to-end while it's running.
func (m *Model) renderUpgrade() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("canopy upgrade"))
	b.WriteString("\n\n")

	switch m.upgradeState {
	case upgradeStateLoading:
		b.WriteString(subtleStyle.Render("Fetching changelog..."))
		b.WriteString("\n\n")
		b.WriteString(subtleStyle.Render("  Esc to cancel"))
		return b.String()

	case upgradeStatePreview:
		b.WriteString(upgradePreviewHeader(m))
		b.WriteString("\n\n")
		if m.upgradeChangelog == "" {
			b.WriteString(subtleStyle.Render("  (changelog unavailable — upgrading anyway is fine)"))
		} else if m.upgradeChangelogInit {
			b.WriteString(m.upgradeChangelogVP.View())
			// Scroll position indicator only when there's more
			// content than fits — keeps the chrome quiet for
			// short changelogs.
			if m.upgradeChangelogVP.TotalLineCount() > m.upgradeChangelogVP.Height {
				b.WriteString("\n")
				b.WriteString(subtleStyle.Render(upgradeScrollIndicator(&m.upgradeChangelogVP)))
			}
		} else {
			// WindowSizeMsg hadn't fired before changelog loaded —
			// fall back to plain output. Rare path; viewport
			// always initializes once size lands.
			b.WriteString(m.upgradeChangelog)
		}
		b.WriteString("\n\n")
		b.WriteString(upgradeFooterPreview())
		return b.String()

	case upgradeStateRunning:
		b.WriteString(subtleStyle.Render("Upgrading..."))
		b.WriteString("\n\n")
		if m.upgradeOutput == "" {
			b.WriteString(subtleStyle.Render("  Working — git pull and make install in flight."))
		} else {
			b.WriteString(m.upgradeOutput)
		}
		b.WriteString("\n\n")
		b.WriteString(subtleStyle.Render("  Ctrl-C to cancel"))
		return b.String()

	case upgradeStateDoneOK:
		// upgradeAvailable was cleared by the doneOK transition; pull
		// the version we shipped from the success message context.
		// Fallback to "the new version" when the field was already
		// reset (defensive — should always have a value here).
		shipped := m.upgradeShipped
		if shipped == "" {
			shipped = "the new version"
		} else {
			shipped = "v" + shipped
		}
		b.WriteString(readyStyle.Render("✓ Upgraded to " + shipped))
		b.WriteString("\n\n")
		// Important UX note: the running canopy process is the OLD
		// binary still in memory (Linux/Mac keep the inode alive
		// after the file is replaced). New `canopy` invocations get
		// the new binary; this session does not. Tell the user.
		b.WriteString(subtleStyle.Render("  This canopy session is still running the old binary."))
		b.WriteString("\n")
		b.WriteString(subtleStyle.Render("  Press q to quit, then re-run canopy to use " + shipped + "."))
		b.WriteString("\n\n")
		if m.upgradeOutput != "" {
			b.WriteString(subtleStyle.Render("Output:"))
			b.WriteString("\n")
			b.WriteString(m.upgradeOutput)
			b.WriteString("\n\n")
		}
		b.WriteString(subtleStyle.Render("  Press any key to return."))
		return b.String()

	case upgradeStateDoneError:
		errMsg := "(unknown error)"
		if m.upgradeErr != nil {
			errMsg = m.upgradeErr.Error()
		}
		b.WriteString(errorStyle.Render("✗ Upgrade failed: " + errMsg))
		b.WriteString("\n\n")
		if m.upgradeOutput != "" {
			b.WriteString(subtleStyle.Render("Output:"))
			b.WriteString("\n")
			b.WriteString(m.upgradeOutput)
			b.WriteString("\n\n")
		}
		b.WriteString(subtleStyle.Render("  Press any key to return."))
		return b.String()
	}

	// upgradeStateNone shouldn't be reachable when mode == upgradeMode
	// but defend against future regressions.
	b.WriteString(subtleStyle.Render("(no active upgrade)"))
	return b.String()
}

// upgradePreviewHeader renders the "v0.12.3 → v0.13.0" line above the
// changelog. Reuses the version-label color palette for consistency
// with the top-bar pill.
func upgradePreviewHeader(m *Model) string {
	current := strings.TrimSpace(m.versionLabel)
	if current == "" {
		current = "(unknown)"
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("245")).
		Render(current+" → ") +
		lipgloss.NewStyle().
			Foreground(lipgloss.Color("220")).
			Bold(true).
			Render("v"+m.upgradeAvailable)
}

// upgradeFooterPreview renders the action hint line for the preview
// state. Three primary actions plus the scroll hint when the
// changelog is taller than the viewport.
func upgradeFooterPreview() string {
	return "  " +
		keyPillStyle.Render("Enter") + subtleStyle.Render(" upgrade") +
		"   " +
		keyPillStyle.Render("Esc") + subtleStyle.Render(" cancel") +
		"   " +
		keyPillStyle.Render("D") + subtleStyle.Render(" dismiss this version") +
		"   " +
		keyPillStyle.Render("j/k") + subtleStyle.Render(" scroll") +
		"   " +
		keyPillStyle.Render("PgUp/PgDn") + subtleStyle.Render(" page")
}

// upgradeScrollIndicator renders a "[X-Y / N]" line at the bottom of
// the changelog viewport so the user knows there's more content
// above or below. Suppressed when content fits — chrome should
// disappear when not load-bearing.
func upgradeScrollIndicator(vp *viewport.Model) string {
	pct := int(vp.ScrollPercent() * 100)
	return "  " + lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		Italic(true).
		Render(formatScrollHint(pct))
}

// formatScrollHint returns "scroll: top" / "scroll: 42%" / "scroll: bottom"
// for the indicator line. Wrapper so the percentage display is testable
// without rendering through lipgloss.
func formatScrollHint(pct int) string {
	switch {
	case pct <= 0:
		return "scroll: top — more below"
	case pct >= 100:
		return "scroll: bottom"
	default:
		return "scroll: " + intToStr(pct) + "%"
	}
}

// intToStr is a tiny stdlib-only int formatter so this file doesn't
// pull strconv just for percentage rendering. Bounded 0..100 input
// from ScrollPercent so the simple loop is sufficient.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

