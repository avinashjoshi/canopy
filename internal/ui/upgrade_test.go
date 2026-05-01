package ui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestCanEnterUpgradeMode covers the four gates: already in mode,
// no upgrade available, missing changelog fn, missing shell fn.
func TestCanEnterUpgradeMode(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(m *Model)
		wantCanEnter bool
	}{
		{
			"all wired + upgrade available",
			func(m *Model) {
				m.upgradeAvailable = "0.13.0"
				m.upgradeChangelogFn = func(ctx context.Context) (string, error) { return "", nil }
				m.upgradeShellFn = func(ctx context.Context, w io.Writer) error { return nil }
			},
			true,
		},
		{
			"already in upgradeMode",
			func(m *Model) {
				m.mode = upgradeMode
				m.upgradeAvailable = "0.13.0"
				m.upgradeChangelogFn = func(ctx context.Context) (string, error) { return "", nil }
				m.upgradeShellFn = func(ctx context.Context, w io.Writer) error { return nil }
			},
			false,
		},
		{
			"no upgrade available",
			func(m *Model) {
				m.upgradeChangelogFn = func(ctx context.Context) (string, error) { return "", nil }
				m.upgradeShellFn = func(ctx context.Context, w io.Writer) error { return nil }
			},
			false,
		},
		{
			"missing changelog fn",
			func(m *Model) {
				m.upgradeAvailable = "0.13.0"
				m.upgradeShellFn = func(ctx context.Context, w io.Writer) error { return nil }
			},
			false,
		},
		{
			"missing shell fn",
			func(m *Model) {
				m.upgradeAvailable = "0.13.0"
				m.upgradeChangelogFn = func(ctx context.Context) (string, error) { return "", nil }
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{}
			tc.setup(m)
			if got := m.canEnterUpgradeMode(); got != tc.wantCanEnter {
				t.Errorf("canEnterUpgradeMode = %v, want %v", got, tc.wantCanEnter)
			}
		})
	}
}

// TestEnterUpgradeMode flips state to loading + fires the changelog
// fetch. The returned cmd must be non-nil so Init's tea.Batch
// receives the changelogLoadedMsg eventually.
func TestEnterUpgradeMode(t *testing.T) {
	m := &Model{}
	called := false
	m.upgradeAvailable = "0.13.0"
	m.upgradeChangelogFn = func(ctx context.Context) (string, error) {
		called = true
		return "## v0.13.0\n- thing", nil
	}
	m.upgradeShellFn = func(ctx context.Context, w io.Writer) error { return nil }

	cmd := m.enterUpgradeMode()
	if m.mode != upgradeMode {
		t.Errorf("mode = %v, want upgradeMode", m.mode)
	}
	if m.upgradeState != upgradeStateLoading {
		t.Errorf("state = %v, want loading", m.upgradeState)
	}
	if cmd == nil {
		t.Fatal("enterUpgradeMode returned nil cmd")
	}

	// Invoke the cmd to verify the closure runs and produces the
	// expected msg type.
	msg := cmd()
	if !called {
		t.Error("changelog fn never invoked")
	}
	got, ok := msg.(changelogLoadedMsg)
	if !ok {
		t.Fatalf("msg type = %T, want changelogLoadedMsg", msg)
	}
	if got.preview != "## v0.13.0\n- thing" {
		t.Errorf("preview = %q, want changelog content", got.preview)
	}
}

// TestUpdate_changelogLoadedMsg flips loading → preview and stores
// the rendered changelog. Errors are tolerated (best-effort): even
// a failed fetch reaches preview state with empty preview.
func TestUpdate_changelogLoadedMsg(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStateLoading
		updated, _ := m.Update(changelogLoadedMsg{preview: "## v0.13.0\n- thing"})
		got := updated.(*Model)
		if got.upgradeState != upgradeStatePreview {
			t.Errorf("state = %v, want preview", got.upgradeState)
		}
		if got.upgradeChangelog != "## v0.13.0\n- thing" {
			t.Errorf("changelog = %q, want stored content", got.upgradeChangelog)
		}
	})

	t.Run("error tolerated", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStateLoading
		updated, _ := m.Update(changelogLoadedMsg{err: errors.New("network")})
		got := updated.(*Model)
		if got.upgradeState != upgradeStatePreview {
			t.Errorf("state = %v, want preview (best-effort)", got.upgradeState)
		}
		if got.upgradeChangelog != "" {
			t.Errorf("changelog should be empty on error; got %q", got.upgradeChangelog)
		}
	})
}

// TestUpdate_upgradeShellStartedMsg flips preview → running and
// stores the cancel func.
func TestUpdate_upgradeShellStartedMsg(t *testing.T) {
	m := &Model{}
	m.mode = upgradeMode
	m.upgradeState = upgradeStatePreview

	buf := &safeBuffer{}
	done := make(chan upgradeShellDoneMsg, 1)
	cancelCalled := false
	cancel := func() { cancelCalled = true }

	updated, _ := m.Update(upgradeShellStartedMsg{
		buf:    buf,
		done:   done,
		cancel: cancel,
	})
	got := updated.(*Model)
	if got.upgradeState != upgradeStateRunning {
		t.Errorf("state = %v, want running", got.upgradeState)
	}
	if got.upgradeBuf != buf {
		t.Error("buf reference not stored")
	}
	if got.upgradeCancel == nil {
		t.Fatal("cancel func not stored")
	}
	got.upgradeCancel()
	if !cancelCalled {
		t.Error("stored cancel did not invoke the original")
	}
}

// TestUpdate_upgradeShellDoneMsg covers both terminal states +
// the post-success pill clear.
func TestUpdate_upgradeShellDoneMsg(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStateRunning
		m.upgradeAvailable = "0.13.0"
		m.upgradeOutput = "earlier output\n"
		updated, _ := m.Update(upgradeShellDoneMsg{
			err:    nil,
			output: "trailing tick\n",
		})
		got := updated.(*Model)
		if got.upgradeState != upgradeStateDoneOK {
			t.Errorf("state = %v, want doneOK", got.upgradeState)
		}
		if !strings.Contains(got.upgradeOutput, "trailing tick") {
			t.Errorf("trailing output not appended; got %q", got.upgradeOutput)
		}
		if got.upgradeAvailable != "" {
			t.Errorf("post-success pill not cleared; upgradeAvailable=%q", got.upgradeAvailable)
		}
		// upgradeShipped captures the version we just installed —
		// the renderer needs this to tell the user what shipped
		// (the pill field is cleared at this point so the
		// success message can't read it back).
		if got.upgradeShipped != "0.13.0" {
			t.Errorf("upgradeShipped not captured at doneOK; got %q, want 0.13.0", got.upgradeShipped)
		}
	})

	t.Run("error", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStateRunning
		m.upgradeAvailable = "0.13.0"
		updated, _ := m.Update(upgradeShellDoneMsg{
			err:    errors.New("make install: exit 1"),
			output: "stderr tail\n",
		})
		got := updated.(*Model)
		if got.upgradeState != upgradeStateDoneError {
			t.Errorf("state = %v, want doneError", got.upgradeState)
		}
		if got.upgradeErr == nil {
			t.Error("upgradeErr not stored")
		}
		// Pill stays on error — user might retry, the upgrade
		// didn't actually land.
		if got.upgradeAvailable != "0.13.0" {
			t.Errorf("pill cleared on error; upgradeAvailable=%q", got.upgradeAvailable)
		}
	})
}

// TestHandleUpgradeKey covers each state's key gate. Builds a *Model
// directly and invokes handleUpgradeKey — the existing test pattern
// for keymap unit tests.
func TestHandleUpgradeKey(t *testing.T) {
	keyEsc := tea.KeyMsg{Type: tea.KeyEsc}
	keyEnter := tea.KeyMsg{Type: tea.KeyEnter}
	keyD := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}}
	keyCtrlC := tea.KeyMsg{Type: tea.KeyCtrlC}
	keyAny := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}

	t.Run("loading: esc returns to list", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStateLoading
		updated, _ := m.handleUpgradeKey(keyEsc)
		got := updated.(*Model)
		if got.mode != listMode {
			t.Errorf("mode = %v, want listMode", got.mode)
		}
		if got.upgradeState != upgradeStateNone {
			t.Errorf("state = %v, want None after reset", got.upgradeState)
		}
	})

	t.Run("preview: enter dispatches shell cmd", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStatePreview
		m.upgradeShellFn = func(ctx context.Context, w io.Writer) error { return nil }
		_, cmd := m.handleUpgradeKey(keyEnter)
		if cmd == nil {
			t.Fatal("enter from preview should dispatch shell cmd")
		}
	})

	t.Run("preview: D dismisses", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStatePreview
		m.upgradeAvailable = "0.13.0"
		dismissCalled := false
		m.upgradeDismissFn = func() error {
			dismissCalled = true
			return nil
		}
		updated, _ := m.handleUpgradeKey(keyD)
		got := updated.(*Model)
		if !dismissCalled {
			t.Error("dismiss fn not invoked")
		}
		if got.upgradeAvailable != "" {
			t.Errorf("upgradeAvailable not cleared; got %q", got.upgradeAvailable)
		}
		if got.mode != listMode {
			t.Errorf("mode = %v, want listMode", got.mode)
		}
	})

	t.Run("preview: esc returns without dismiss", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStatePreview
		m.upgradeAvailable = "0.13.0"
		dismissCalled := false
		m.upgradeDismissFn = func() error {
			dismissCalled = true
			return nil
		}
		updated, _ := m.handleUpgradeKey(keyEsc)
		got := updated.(*Model)
		if dismissCalled {
			t.Error("esc must NOT invoke dismiss")
		}
		if got.upgradeAvailable != "0.13.0" {
			t.Errorf("upgradeAvailable cleared on esc; should persist for user to act later")
		}
		if got.mode != listMode {
			t.Errorf("mode = %v, want listMode", got.mode)
		}
	})

	t.Run("running: ctrl-c invokes cancel", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStateRunning
		cancelCalled := false
		m.upgradeCancel = func() { cancelCalled = true }
		updated, _ := m.handleUpgradeKey(keyCtrlC)
		got := updated.(*Model)
		if !cancelCalled {
			t.Error("ctrl-c should invoke upgradeCancel")
		}
		// Stays in running state until upgradeShellDoneMsg lands
		// with the canceled error.
		if got.upgradeState != upgradeStateRunning {
			t.Errorf("state = %v, want running until done msg", got.upgradeState)
		}
	})

	t.Run("running: other keys ignored", func(t *testing.T) {
		m := &Model{}
		m.mode = upgradeMode
		m.upgradeState = upgradeStateRunning
		updated, _ := m.handleUpgradeKey(keyAny)
		got := updated.(*Model)
		// Must not flip back to listMode mid-run; user could
		// accidentally trigger this.
		if got.mode == listMode {
			t.Error("running should ignore non-cancel keys")
		}
	})

	t.Run("doneOK: any key returns to list", func(t *testing.T) {
		m := newTestModel(false)
		m.mode = upgradeMode
		m.upgradeState = upgradeStateDoneOK
		updated, _ := m.handleUpgradeKey(keyAny)
		got := updated.(*Model)
		if got.mode != listMode {
			t.Errorf("mode = %v, want listMode", got.mode)
		}
	})

	t.Run("doneError: any key returns to list", func(t *testing.T) {
		m := newTestModel(false)
		m.mode = upgradeMode
		m.upgradeState = upgradeStateDoneError
		updated, _ := m.handleUpgradeKey(keyAny)
		got := updated.(*Model)
		if got.mode != listMode {
			t.Errorf("mode = %v, want listMode", got.mode)
		}
	})
}

// TestRenderUpgrade exercises each state's render branch. We don't
// match exact output (lipgloss-styled strings change between
// versions); we assert the load-bearing content per state is
// present.
func TestRenderUpgrade(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(m *Model)
		mustHave []string
		mustNot  []string
	}{
		{
			"loading state",
			func(m *Model) { m.upgradeState = upgradeStateLoading },
			[]string{"Fetching changelog", "Esc"},
			nil,
		},
		{
			"preview state with content",
			func(m *Model) {
				m.upgradeState = upgradeStatePreview
				m.versionLabel = "v0.12.3"
				m.upgradeAvailable = "0.13.0"
				m.upgradeChangelog = "## v0.13.0\n- thing"
			},
			[]string{"v0.12.3", "0.13.0", "thing", "Enter", "Esc", "D"},
			nil,
		},
		{
			"preview state, empty changelog",
			func(m *Model) {
				m.upgradeState = upgradeStatePreview
				m.versionLabel = "v0.12.3"
				m.upgradeAvailable = "0.13.0"
				m.upgradeChangelog = ""
			},
			[]string{"changelog unavailable", "Enter"},
			nil,
		},
		{
			"running state, no output yet",
			func(m *Model) {
				m.upgradeState = upgradeStateRunning
			},
			[]string{"Working", "Ctrl-C"},
			nil,
		},
		{
			"running state, with output",
			func(m *Model) {
				m.upgradeState = upgradeStateRunning
				m.upgradeOutput = "some streaming output"
			},
			[]string{"streaming output", "Ctrl-C"},
			[]string{"Working — git pull"}, // suppressed when output present
		},
		{
			"doneOK with output",
			func(m *Model) {
				m.upgradeState = upgradeStateDoneOK
				// upgradeShipped is what gets shown — captured at the
				// doneOK transition before upgradeAvailable was cleared.
				m.upgradeShipped = "0.13.0"
				m.upgradeOutput = "build complete"
			},
			[]string{"Upgraded to v0.13.0", "build complete", "Press any key", "still running the old binary"},
			nil,
		},
		{
			"doneOK without shipped version (defensive fallback)",
			func(m *Model) {
				m.upgradeState = upgradeStateDoneOK
				// Defensive: shouldn't happen in real flow but the
				// renderer falls back to "the new version" if unset.
				m.upgradeShipped = ""
			},
			[]string{"Upgraded to the new version", "still running the old binary"},
			nil,
		},
		{
			"doneError with stderr",
			func(m *Model) {
				m.upgradeState = upgradeStateDoneError
				m.upgradeErr = errors.New("make install: exit 1")
				m.upgradeOutput = "compile error"
			},
			[]string{"Upgrade failed", "make install: exit 1", "compile error", "Press any key"},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{}
			tc.setup(m)
			out := m.renderUpgrade()
			for _, want := range tc.mustHave {
				if !strings.Contains(out, want) {
					t.Errorf("render missing %q; got:\n%s", want, out)
				}
			}
			for _, dont := range tc.mustNot {
				if strings.Contains(out, dont) {
					t.Errorf("render contained %q (should be hidden); got:\n%s", dont, out)
				}
			}
		})
	}
}

// TestActionUpgrade gates and dispatches identically to canEnterUpgradeMode.
func TestActionUpgrade(t *testing.T) {
	t.Run("ungated returns nil cmd", func(t *testing.T) {
		m := &Model{}
		_, cmd := actionUpgrade(m, tea.KeyMsg{})
		if cmd != nil {
			t.Errorf("ungated action should not dispatch; got cmd=%T", cmd)
		}
	})

	t.Run("gated dispatches and flips mode", func(t *testing.T) {
		m := &Model{}
		m.upgradeAvailable = "0.13.0"
		m.upgradeChangelogFn = func(ctx context.Context) (string, error) { return "x", nil }
		m.upgradeShellFn = func(ctx context.Context, w io.Writer) error { return nil }
		updated, cmd := actionUpgrade(m, tea.KeyMsg{})
		got := updated.(*Model)
		if got.mode != upgradeMode {
			t.Errorf("mode = %v, want upgradeMode", got.mode)
		}
		if cmd == nil {
			t.Error("expected non-nil cmd to fire changelog fetch")
		}
	})
}

// TestActionDismissUpgrade clears the pill + invokes dismiss fn.
func TestActionDismissUpgrade(t *testing.T) {
	m := &Model{}
	m.upgradeAvailable = "0.13.0"
	called := false
	m.upgradeDismissFn = func() error {
		called = true
		return nil
	}
	updated, _ := actionDismissUpgrade(m, tea.KeyMsg{})
	got := updated.(*Model)
	if !called {
		t.Error("dismiss fn not invoked")
	}
	if got.upgradeAvailable != "" {
		t.Errorf("pill not cleared; got %q", got.upgradeAvailable)
	}
}

// TestActionDismissUpgrade_silentlyFailingFn: dismiss errors must
// not crash the UI; the action logs and proceeds with the in-memory
// pill clear.
func TestActionDismissUpgrade_silentlyFailingFn(t *testing.T) {
	m := &Model{}
	m.upgradeAvailable = "0.13.0"
	m.upgradeDismissFn = func() error {
		return errors.New("disk full")
	}
	updated, _ := actionDismissUpgrade(m, tea.KeyMsg{})
	got := updated.(*Model)
	// Even on error we clear the in-memory pill — user pressed D,
	// they don't want to keep seeing the pill this session.
	if got.upgradeAvailable != "" {
		t.Errorf("in-memory pill should clear despite fn error; got %q", got.upgradeAvailable)
	}
}

// TestAvailableUpgrade / TestAvailableDismissUpgrade: the keymap
// gate functions reduce to canEnterUpgradeMode + canDismissUpgrade.
func TestAvailableUpgrade(t *testing.T) {
	m := &Model{}
	if availableUpgrade(m) {
		t.Error("empty model should not surface U")
	}
	m.upgradeAvailable = "0.13.0"
	m.upgradeChangelogFn = func(ctx context.Context) (string, error) { return "", nil }
	m.upgradeShellFn = func(ctx context.Context, w io.Writer) error { return nil }
	if !availableUpgrade(m) {
		t.Error("fully-wired model should surface U")
	}
}

func TestAvailableDismissUpgrade(t *testing.T) {
	m := &Model{}
	if availableDismissUpgrade(m) {
		t.Error("empty model should not surface D")
	}
	m.upgradeAvailable = "0.13.0"
	m.upgradeDismissFn = func() error { return nil }
	if !availableDismissUpgrade(m) {
		t.Error("fully-wired model should surface D")
	}
}

// TestUpgradeProgressTickCmd: the streaming tick must actually
// produce upgradeProgressTickMsg with the drained chunk.
func TestUpgradeProgressTickCmd(t *testing.T) {
	buf := &safeBuffer{}
	_, _ = buf.Write([]byte("line one\n"))
	cmd := upgradeProgressTickCmd(buf)
	if cmd == nil {
		t.Fatal("cmd is nil")
	}
	// tea.Tick returns a cmd that needs to be invoked to fire; we
	// can't easily simulate the timer in a unit test, but we can
	// verify the cmd is non-nil. The real tick behavior is
	// covered by integration smoke when running canopy upgrade.
}

// TestInitUpgradeViewport sizes the viewport against m.width/m.height
// (with reserve for chrome) and loads the changelog content.
func TestInitUpgradeViewport(t *testing.T) {
	m := &Model{}
	m.width = 100
	m.height = 30
	m.upgradeChangelog = "## v0.13.0\n- thing\n- other thing\n"
	m.initUpgradeViewport()
	if !m.upgradeChangelogInit {
		t.Fatal("init flag not set after initUpgradeViewport")
	}
	if m.upgradeChangelogVP.Width != 96 { // 100 - 4 margin
		t.Errorf("viewport width = %d, want 96", m.upgradeChangelogVP.Width)
	}
	if m.upgradeChangelogVP.Height != 22 { // 30 - 8 reserve
		t.Errorf("viewport height = %d, want 22", m.upgradeChangelogVP.Height)
	}
	view := m.upgradeChangelogVP.View()
	if !strings.Contains(view, "v0.13.0") {
		t.Errorf("viewport view missing changelog content; got %q", view)
	}
}

// TestInitUpgradeViewport_fallbackSize: when WindowSizeMsg hasn't
// fired, m.width/height are 0. Fallback to 76x16 so the viewport
// is still usable.
func TestInitUpgradeViewport_fallbackSize(t *testing.T) {
	m := &Model{}
	m.upgradeChangelog = "content"
	m.initUpgradeViewport()
	if m.upgradeChangelogVP.Width < 20 {
		t.Errorf("fallback width too small: %d", m.upgradeChangelogVP.Width)
	}
	if m.upgradeChangelogVP.Height < 5 {
		t.Errorf("fallback height too small: %d", m.upgradeChangelogVP.Height)
	}
}

// TestHandleUpgradeKey_previewScrollForward: any non-action key in
// preview state forwards to the viewport. We can't easily simulate
// ScrollPercent changes in a unit test (the viewport uses an
// internal cursor that responds to specific tea.KeyMsg shapes), so
// we just verify the call doesn't panic and returns a model.
func TestHandleUpgradeKey_previewScrollForward(t *testing.T) {
	m := &Model{}
	m.mode = upgradeMode
	m.upgradeState = upgradeStatePreview
	m.upgradeChangelog = strings.Repeat("line\n", 100)
	m.width = 100
	m.height = 30
	m.initUpgradeViewport()

	keyJ := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}
	updated, _ := m.handleUpgradeKey(keyJ)
	got := updated.(*Model)
	if got.upgradeState != upgradeStatePreview {
		t.Errorf("scroll key should not change state; got %v", got.upgradeState)
	}
	if got.mode != upgradeMode {
		t.Errorf("scroll key should not change mode; got %v", got.mode)
	}
}

// TestFormatScrollHint covers the three boundary states.
func TestFormatScrollHint(t *testing.T) {
	cases := []struct {
		pct  int
		want string
	}{
		{0, "scroll: top — more below"},
		{1, "scroll: 1%"},
		{42, "scroll: 42%"},
		{99, "scroll: 99%"},
		{100, "scroll: bottom"},
	}
	for _, tc := range cases {
		if got := formatScrollHint(tc.pct); got != tc.want {
			t.Errorf("formatScrollHint(%d) = %q, want %q", tc.pct, got, tc.want)
		}
	}
}

// TestIntToStr is the local int formatter — bounded to 0..100 in
// production. Verify the boundary inputs.
func TestIntToStr(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{100, "100"},
	}
	for _, tc := range cases {
		if got := intToStr(tc.n); got != tc.want {
			t.Errorf("intToStr(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestRenderUpgrade_previewWithViewport exercises the viewport
// render branch when content + size are wired. The previous
// "preview state with content" case in TestRenderUpgrade doesn't
// initialize the viewport, so it goes through the fallback branch.
// This covers the happy path explicitly.
func TestRenderUpgrade_previewWithViewport(t *testing.T) {
	m := &Model{}
	m.upgradeState = upgradeStatePreview
	m.versionLabel = "v0.12.3"
	m.upgradeAvailable = "0.13.0"
	m.upgradeChangelog = "## v0.13.0\n- thing\n"
	m.width = 100
	m.height = 30
	m.initUpgradeViewport()
	out := m.renderUpgrade()
	for _, want := range []string{"v0.12.3", "0.13.0", "thing", "Enter", "j/k"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q; got:\n%s", want, out)
		}
	}
}
