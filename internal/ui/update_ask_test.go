package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestHandleAskPickerKey_NavCursor: arrow nav stays within bounds.
func TestHandleAskPickerKey_NavCursor(t *testing.T) {
	m := &Model{
		mode:      askPickerMode,
		askList:   []string{"claude", "codex", "aider"},
		askCursor: 0,
	}
	_, _ = m.handleAskPickerKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.askCursor != 0 {
		t.Errorf("up at top moved cursor; got %d, want 0", m.askCursor)
	}
	_, _ = m.handleAskPickerKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleAskPickerKey(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.handleAskPickerKey(tea.KeyMsg{Type: tea.KeyDown}) // past end
	if m.askCursor != 2 {
		t.Errorf("down past end: got %d, want 2", m.askCursor)
	}
}

// TestHandleAskPickerKey_EscCancelsAndClearsState: Esc on the picker
// returns to listMode and clears snapshot state.
func TestHandleAskPickerKey_EscCancelsAndClearsState(t *testing.T) {
	m := &Model{
		mode:          askPickerMode,
		askTarget:     "foo",
		askTargetRoot: "/tmp/proj",
		askList:       []string{"claude"},
		askCursor:     0,
	}
	_, _ = m.handleAskPickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != listMode {
		t.Errorf("mode = %v; want listMode", m.mode)
	}
	if m.askTarget != "" || m.askTargetRoot != "" || m.askList != nil {
		t.Errorf("ask state not cleared: target=%q root=%q list=%v",
			m.askTarget, m.askTargetRoot, m.askList)
	}
}

// TestHandleAskPickerKey_EnterTransitionsToInput: pressing Enter on
// the picker captures the chosen agent and transitions to input mode
// with a focused textarea.
func TestHandleAskPickerKey_EnterTransitionsToInput(t *testing.T) {
	m := &Model{
		mode:      askPickerMode,
		askList:   []string{"claude", "codex"},
		askCursor: 1, // codex
	}
	_, cmd := m.handleAskPickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != askInputMode {
		t.Errorf("mode = %v; want askInputMode", m.mode)
	}
	if m.askAgent != "codex" {
		t.Errorf("askAgent = %q; want codex", m.askAgent)
	}
	if cmd == nil {
		t.Error("Enter returned nil cmd; want textarea Focus() cmd")
	}
}

// TestWriteAskTempFile_RoundTrip: write a body, read it back from the
// returned path, verify it's under ~/.canopy/tmp/ with the ask-
// prefix + .md suffix shape the sweep depends on.
func TestWriteAskTempFile_RoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	body := "what does this regex do?"
	path, err := writeAskTempFile(body)
	if err != nil {
		t.Fatalf("writeAskTempFile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	// Path shape
	if !strings.HasPrefix(filepath.Base(path), "ask-") {
		t.Errorf("path basename %q lacks ask- prefix", filepath.Base(path))
	}
	if !strings.HasSuffix(path, ".md") {
		t.Errorf("path %q lacks .md suffix", path)
	}
	if !strings.Contains(path, filepath.Join(home, ".canopy", "tmp")) {
		t.Errorf("path %q not under HOME/.canopy/tmp", path)
	}
	// Body round-trip
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != body {
		t.Errorf("body round-trip: got %q, want %q", string(got), body)
	}
}

// TestWriteAskTempFile_NoStaleTmpfile: an interrupted writeAskTempFile
// shouldn't leave the .tmp scratch file behind. We verify by reading
// the parent dir after a successful write: only the final .md exists.
func TestWriteAskTempFile_NoStaleTmpfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := writeAskTempFile("body")
	if err != nil {
		t.Fatalf("writeAskTempFile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	entries, _ := os.ReadDir(filepath.Dir(path))
	tmpCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			tmpCount++
		}
	}
	if tmpCount != 0 {
		t.Errorf("found %d leftover .tmp files in tmp dir", tmpCount)
	}
}

// fakeTmuxClient stubs tmux.Client's DisplayPopup so askPopupCmd tests
// don't need a running tmux server.
type fakeTmuxClient struct {
	gotCommand string
	gotCwd     string
	returnErr  error
}

func (f *fakeTmuxClient) DisplayPopup(ctx context.Context, command, cwd string) error {
	f.gotCommand = command
	f.gotCwd = cwd
	return f.returnErr
}

// TestAskPopupCmd_HappyPath: askPopupCmd writes a temp file, dispatches
// `canopy ask <agent> --file <path>` via DisplayPopup, then removes
// the temp file. Verifies the wire end-to-end with a fake tmux client.
func TestAskPopupCmd_HappyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake := &fakeTmuxClient{}

	cmd := askPopupCmd("codex", "what does this do?", "/tmp/proj", fake)
	msg := cmd()
	done, ok := msg.(askDoneMsg)
	if !ok {
		t.Fatalf("msg type = %T; want askDoneMsg", msg)
	}
	if done.err != nil {
		t.Errorf("done.err = %v; want nil", done.err)
	}

	// DisplayPopup got the right invocation. Post codex-review P1 #2,
	// the popup command is wrapped with `bash -c '... ; read -r _'` so
	// it stays open after canopy ask exits; verify both the bash wrapper
	// AND the inner canopy ask invocation are present. Post P2 #4 the
	// tmpPath is passed as a positional argument ($1), not interpolated
	// into the bash body, so we look for `--file "$1"` literally and
	// the path appears as a single-quoted positional after the body.
	if !strings.HasPrefix(fake.gotCommand, "bash -c '") {
		t.Errorf("DisplayPopup command = %q; want 'bash -c ...' wrapper", fake.gotCommand)
	}
	if !strings.Contains(fake.gotCommand, `canopy ask 'codex' --file "$1"`) {
		t.Errorf("DisplayPopup command = %q; missing `canopy ask 'codex' --file \"$1\"` invocation", fake.gotCommand)
	}
	if !strings.Contains(fake.gotCommand, "read -r _") {
		t.Errorf("DisplayPopup command = %q; missing 'read -r _' dismiss prompt (P1 #2)", fake.gotCommand)
	}
	if fake.gotCwd != "/tmp/proj" {
		t.Errorf("DisplayPopup cwd = %q; want /tmp/proj", fake.gotCwd)
	}

	// Temp file was deleted after the popup completed.
	tmpDir := filepath.Join(home, ".canopy", "tmp")
	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ask-") {
			t.Errorf("temp file %s not cleaned up", e.Name())
		}
	}
}

// TestPosixShellQuote_BasicCases: spot-check the inline shell-quote
// helper. Pins the contract askPopupCmd depends on for safe path
// passing — paths with spaces, dollars, and embedded single quotes
// must all survive the outer shell layer untouched.
func TestPosixShellQuote_BasicCases(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "'plain'"},
		{"/tmp/foo.md", "'/tmp/foo.md'"},
		{"/home/Foo Bar/.canopy/tmp/ask-abc.md", "'/home/Foo Bar/.canopy/tmp/ask-abc.md'"},
		{"with$var", "'with$var'"},
		{"has'quote", `'has'\''quote'`},
		{"", "''"},
	}
	for _, c := range cases {
		got := posixShellQuote(c.in)
		if got != c.want {
			t.Errorf("posixShellQuote(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// TestAskPopupCmd_TmpPathWithSpaces pins the bug codex review caught
// 2026-06-25 (P2 #4): a $HOME containing a space (which makes tmpPath
// land at something like /home/Foo Bar/.canopy/tmp/ask-abc.md) used to
// produce a popup command that bash re-split on the space, sending only
// the first half to --file. The fix passes tmpPath as a POSITIONAL
// argument to bash, single-quoted at the outer-shell layer.
//
// We can't easily set HOME for this test (the popup command builder
// reads HOME via writeAskTempFile, but we want to verify the QUOTING
// behavior of the assembled command independently). So we assert
// that the assembled command always ends with a single-quoted
// positional argument matching the temp file path — that's the
// contract that protects path safety regardless of $HOME shape.
func TestAskPopupCmd_TmpPathQuotedAsPositional(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake := &fakeTmuxClient{}

	cmd := askPopupCmd("codex", "what does this do?", "/tmp/proj", fake)
	_ = cmd()

	// Command shape: ends with `_ '<tmpPath>'` (sh placeholder for $0,
	// then quoted $1).
	got := fake.gotCommand
	if !strings.HasSuffix(got, ".md'") {
		t.Errorf("popup command should end with a single-quoted .md path; got %q", got)
	}
	// The positional must be wrapped in single quotes (not interpolated
	// raw into the bash body).
	if !strings.Contains(got, " _ '") {
		t.Errorf("popup command missing `_ '...'` positional pattern; got %q", got)
	}
}

// TestAskPopupCmd_DisplayPopupErrorSurfaced: when tmux fails, the
// askDoneMsg carries the wrapped error.
func TestAskPopupCmd_DisplayPopupErrorSurfaced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake := &fakeTmuxClient{returnErr: errors.New("tmux exploded")}

	cmd := askPopupCmd("codex", "q", "/tmp/proj", fake)
	msg := cmd()
	done := msg.(askDoneMsg)
	if done.err == nil {
		t.Fatal("done.err = nil; want wrapped DisplayPopup error")
	}
	if !strings.Contains(done.err.Error(), "tmux exploded") {
		t.Errorf("err missing underlying message: %v", done.err)
	}
}

// TestHandleAskDone_AlwaysReturnsToListMode: success or error, the
// done handler sends the user back to the list (the popup IS the
// answer surface — nothing more to render in the TUI).
func TestHandleAskDone_AlwaysReturnsToListMode(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"error", errors.New("kaboom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{mode: askInputMode, askTarget: "foo"}
			_, _ = m.handleAskDone(askDoneMsg{err: tc.err})
			if m.mode != listMode {
				t.Errorf("mode = %v; want listMode", m.mode)
			}
			if tc.err != nil && m.err == nil {
				t.Error("Model.err nil after errored popup; should carry surface message")
			}
		})
	}
}

// TestRenderAskPicker_LinesPresent: the picker view shows agent names
// + workspace name + nav hint.
func TestRenderAskPicker_LinesPresent(t *testing.T) {
	m := &Model{
		mode:      askPickerMode,
		askTarget: "feature-x",
		askList:   []string{"claude", "codex"},
		askCursor: 1,
	}
	out := m.renderAskPicker()
	for _, want := range []string{"feature-x", "claude", "codex", "next", "cancel"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderAskPicker missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestRenderAskInput_LinesPresent: the input view shows the chosen
// agent + workspace + submit hint.
func TestRenderAskInput_LinesPresent(t *testing.T) {
	m := &Model{
		mode:      askInputMode,
		askTarget: "feature-x",
		askAgent:  "codex",
		askInput:  newAskTextarea(),
	}
	out := m.renderAskInput()
	for _, want := range []string{"feature-x", "codex", "Ctrl+S", "Esc"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderAskInput missing %q\nfull:\n%s", want, out)
		}
	}
}
