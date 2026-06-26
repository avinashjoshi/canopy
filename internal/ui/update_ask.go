// Ask (second-opinion popup) flow. `Q` from the workspaces tab opens
// the picker (askPickerMode) → user selects target agent → the TUI
// transitions to askInputMode (multi-line textarea) → Ctrl+S writes
// the question to ~/.canopy/tmp/ask-<rand>.md and spawns
// `canopy ask <agent> --file <path>` inside a tmux display-popup.
//
// Why a tmux popup (vs Bubbletea modal): the actual answer can be 100+
// lines and arrives over multiple seconds. The popup is its own pty,
// scrollable via the user's normal tmux key bindings, and dismissible
// with q. The TUI underneath stays present for instant return.
//
// Why temp-file vs piping stdin: tmux display-popup runs the command
// in a fresh pty inside the popup; the parent Bubbletea process's
// stdin doesn't pipe through to the popup'd command. The design doc
// reviewer caught this as a blocker; temp-file + --file plumbing is
// the fix. See ~/.gstack/projects/avinashjoshi-canopy/cassy-add-codex-
// support-concurrent-multi-agent-design-20260625-110939.md §3.

package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/agent"
)

// askPromptHeight is the textarea height (rows). 12 rows fits a
// multi-line question comfortably while leaving room for the picker
// header and the "Ctrl+S to submit / Esc to cancel" footer in an
// 80%-height popup.
const askPromptHeight = 12

// availableAsk gates the `Q` keybind. Same conditions as the swap
// picker: workspaces tab, local row, real workspace (not Main/loading),
// at least one launcher installed. Allowlist is not a gate (D6=A).
func availableAsk(m *Model) bool {
	if m.tab == tabHosts {
		return false
	}
	row, ok := m.list.CursorRow()
	if !ok || row.Loading || row.IsMain || row.Host != "" {
		return false
	}
	return len(agent.InstalledLaunchers()) > 0
}

// actionAsk opens the ask picker. Snapshots the installed-launchers
// list at open time so a refresh tick can't reshuffle the picker mid-
// decision. Picking a launcher outside canopy.json's allowlist auto-
// adds it (D6=A); the picker doesn't pre-filter to allowed-only.
func actionAsk(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok || row.Loading {
		return m, nil
	}
	installed := agent.InstalledLaunchers()
	if len(installed) == 0 {
		m.err = fmt.Errorf("no agent launchers installed on PATH (claude / codex / aider / opencode)")
		return m, nil
	}
	m.mode = askPickerMode
	m.askTarget = row.Name
	m.askTargetRoot = row.ProjectRoot
	m.askList = installed
	m.askAgent = ""
	m.askErr = ""

	// Cursor default: first agent that isn't the row's current. Same
	// rationale as the swap picker — asking the agent you're already
	// running is rarely what you wanted.
	m.askCursor = 0
	for i, a := range m.askList {
		if a != row.CurrentAgent {
			m.askCursor = i
			break
		}
	}
	return m, nil
}

// handleAskPickerKey is the keymap while the agent picker is up.
// Arrow nav + Enter → askInputMode. Esc cancels back to listMode.
func (m *Model) handleAskPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = listMode
		m.clearAskState()
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.askCursor > 0 {
			m.askCursor--
		}
		return m, nil
	case "down", "j":
		if m.askCursor < len(m.askList)-1 {
			m.askCursor++
		}
		return m, nil
	case "enter":
		if m.askCursor < 0 || m.askCursor >= len(m.askList) {
			return m, nil
		}
		m.askAgent = m.askList[m.askCursor]
		m.mode = askInputMode
		m.askInput = newAskTextarea()
		return m, m.askInput.Focus()
	}
	return m, nil
}

// newAskTextarea returns a textarea pre-configured for the question
// input stage. Multi-line, Ctrl+S to submit (handled in
// handleAskInputKey, not via textarea binding), Esc cancels.
func newAskTextarea() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "Type your question (Ctrl+S to submit, Esc to cancel)..."
	ta.SetWidth(80)
	ta.SetHeight(askPromptHeight)
	ta.CharLimit = 32 * 1024 // 32KB ceiling, mirrors canopy new --prompt-file
	return ta
}

// handleAskInputKey is the keymap while the question textarea is up.
// Ctrl+S submits → dispatches the popup spawn cmd. Esc cancels back
// to listMode (loses the typed question — same as canopy new --prompt's
// esc behavior).
func (m *Model) handleAskInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = listMode
		m.clearAskState()
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+s":
		question := strings.TrimSpace(m.askInput.Value())
		if question == "" {
			m.askErr = "Question is empty — type something or Esc to cancel."
			return m, nil
		}
		// Resolve the workspace's worktree path for the popup cwd
		// (codex review P1 #1: must be Path, not ProjectRoot — the
		// `canopy ask` subprocess does cwd-walk-up to find its
		// workspace, and we want it landing in the worktree, not
		// the source checkout).
		popupCwd, err := m.resolveAskCwd()
		if err != nil {
			m.askErr = "Couldn't resolve workspace: " + err.Error()
			return m, nil
		}
		return m, askPopupCmd(m.askAgent, question, popupCwd, m.tc)
	}
	// Default: let the textarea handle the keypress (typing, nav, etc).
	var cmd tea.Cmd
	m.askInput, cmd = m.askInput.Update(msg)
	return m, cmd
}

// resolveAskCwd returns the cwd for the popup's `canopy ask` invocation.
// We want the WORKSPACE'S WORKTREE PATH (row.Path) — NOT the project root.
// `canopy ask` does its own cwd-walk-up to find the workspace from cwd;
// pointing it at the project root would land on the source checkout
// instead of the worktree, and the agent would answer against the
// wrong branch/files. (codex review P1 #1, 2026-06-25.)
//
// The snapshot (askTarget + askTargetRoot) was captured at open time;
// the row may have re-ordered or disappeared by submit time, so we
// re-walk filteredRows and only return when both (Name, ProjectRoot)
// still match. ProjectRoot is the (Project, Name) discriminator, NOT
// the popup cwd.
func (m *Model) resolveAskCwd() (string, error) {
	for _, r := range m.filteredRows() {
		if r.Name != m.askTarget {
			continue
		}
		if m.askTargetRoot != "" && r.ProjectRoot != m.askTargetRoot {
			continue
		}
		if r.Path == "" {
			return "", fmt.Errorf("workspace %q has no Path set; cannot dispatch popup", r.Name)
		}
		return r.Path, nil
	}
	return "", fmt.Errorf("workspace %q (root %q) is no longer in the row list", m.askTarget, m.askTargetRoot)
}

// clearAskState resets the ask fields after dismiss / popup completion.
func (m *Model) clearAskState() {
	m.askTarget = ""
	m.askTargetRoot = ""
	m.askList = nil
	m.askCursor = 0
	m.askAgent = ""
	m.askErr = ""
	// Don't bother resetting m.askInput — its zero value works and the
	// next open re-Newzs it anyway.
}

// askDoneMsg is posted by askPopupCmd after the tmux popup exits.
// err is nil on success; populated when temp-file write or popup spawn
// failed. successful popup exit (whether the agent answered cleanly or
// not) is success from canopy's perspective — the user saw the answer
// or error in the popup.
type askDoneMsg struct {
	err error
}

// askPopupCmd writes the question to ~/.canopy/tmp/ask-<rand>.md,
// spawns `canopy ask <agent> --file <path>` via tmux display-popup
// (blocks until the popup is dismissed), then deletes the temp file
// and posts an askDoneMsg. Runs in its own goroutine (Bubbletea's
// tea.Cmd contract) so the TUI event loop stays responsive.
//
// Tests on this command directly are hard — they'd need a running
// tmux server + a real canopy binary on PATH. The state-transition
// tests around it (mode flips, temp-file lifecycle from the unit
// helper below) cover what we can; the popup invocation itself is
// validated by manual smoke per the design doc's E2E step.
func askPopupCmd(agent, question, projectRoot string, tc tmuxClient) tea.Cmd {
	return func() tea.Msg {
		tmpPath, err := writeAskTempFile(question)
		if err != nil {
			return askDoneMsg{err: fmt.Errorf("ask: write temp file: %w", err)}
		}
		defer os.Remove(tmpPath)

		// Build the popup command. The `canopy` binary is whatever
		// the user has on PATH (likely the dev symlink during testing,
		// the released binary in production). Per CLAUDE.md, that's
		// user state — we don't second-guess it.
		//
		// Wrap with `bash -c '<cmd>; echo; read -p "..." _'` so the
		// popup stays open after canopy ask exits. Without the wait,
		// `tmux display-popup -E` closes the moment the child process
		// finishes, vanishing fast answers (and error messages from
		// agent-not-allowed / binary-missing / etc) before the user
		// can read them. The trailing `read` blocks until the user
		// presses enter or dismisses the popup with the normal tmux
		// key. (codex review P1 #2, 2026-06-25.)
		//
		// Path safety: tmpPath lands in $HOME/.canopy/tmp/ask-<hex>.md.
		// $HOME can contain spaces, `$`, or `'`, and the path then
		// inherits them. We pass tmpPath as a POSITIONAL ARGUMENT to
		// bash ($1) instead of interpolating it into the bash -c body —
		// that way only the OUTER shell sees the raw path, and it gets
		// single-quoted once via posixShellQuote. bash then expands
		// "$1" with full word integrity. This dodges the nested-quoting
		// problem that an unquoted `--file %s` interpolation had: a
		// home like /home/Foo Bar/ split on the space. (codex review
		// P2 #4, 2026-06-25.)
		popupCmd := fmt.Sprintf(
			`bash -c 'canopy ask %s --file "$1"; echo; echo "--- press enter to dismiss ---"; read -r _' _ %s`,
			posixShellQuote(agent), posixShellQuote(tmpPath))

		// Run the popup. tmux display-popup blocks until the wrapped
		// bash process exits, which now happens only after the user
		// presses enter on the dismiss prompt.
		ctx := context.Background()
		if err := tc.DisplayPopup(ctx, popupCmd, projectRoot); err != nil {
			return askDoneMsg{err: fmt.Errorf("ask: display-popup: %w", err)}
		}
		return askDoneMsg{}
	}
}

// tmuxClient is the subset of *tmux.Client askPopupCmd needs. Lets us
// inject a fake in tests without spinning up a real tmux server.
type tmuxClient interface {
	DisplayPopup(ctx context.Context, command, cwd string) error
}

// writeAskTempFile creates ~/.canopy/tmp/ask-<rand>.md with the given
// body using an atomic tmpfile + rename. Returns the final path; the
// caller is responsible for os.Remove'ing it after the popup exits.
// The startup sweep (cmd/canopy/main.go init() → sweepAskTempFiles)
// is the backstop for the rare case where the caller's defer doesn't
// fire (TUI SIGKILL'd mid-popup).
func writeAskTempFile(body string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ask: home dir: %w", err)
	}
	tmpDir := filepath.Join(home, ".canopy", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", fmt.Errorf("ask: mkdir %s: %w", tmpDir, err)
	}
	rand := newAskRandSuffix()
	finalPath := filepath.Join(tmpDir, "ask-"+rand+".md")
	tmpPath := finalPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(body), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return finalPath, nil
}

// posixShellQuote wraps s in POSIX single quotes, escaping any
// embedded single quotes via the standard '\'' close-escape-reopen
// trick. Safe for any string — single-quoted POSIX strings have NO
// escape sequences except for that trick.
//
// Used by askPopupCmd to pass tmpPath (and the agent name) as
// positional arguments to bash. The OUTER shell that tmux runs the
// command through sees these single-quoted, preserving spaces and
// shell metacharacters that a $HOME with awkward characters might
// embed in the path.
func posixShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newAskRandSuffix returns a short random hex string for the temp
// file's name. 12 hex chars = 48 bits of entropy — overkill for a
// per-popup unique name; the startup sweep + atomic rename are the
// load-bearing safety bits.
func newAskRandSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Should never fail in practice (crypto/rand panics on
		// /dev/urandom unavailable). Fall back to a timestamp-based
		// suffix; collisions are still extremely unlikely.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// handleAskDone applies the askDoneMsg result. Always returns to
// listMode (the popup IS the answer surface — there's nothing for the
// TUI to render afterward besides going back to the list).
func (m *Model) handleAskDone(msg askDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = errors.New("ask popup failed: " + msg.err.Error())
	}
	m.mode = listMode
	m.clearAskState()
	return m, nil
}

// renderAskPicker draws the agent picker. Same density as the swap
// picker — simple text rows + cursor marker + footer hint.
func (m *Model) renderAskPicker() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nAsk an agent (quick second opinion) in workspace %q\n\n", m.askTarget)
	for i, a := range m.askList {
		marker := "  "
		if i == m.askCursor {
			marker = "▶ "
		}
		fmt.Fprintf(&b, "%s%s\n", marker, a)
	}
	b.WriteString("\n  ↑/↓ select • enter next • esc cancel\n")
	return b.String()
}

// renderAskInput draws the textarea + agent header + submit hint.
func (m *Model) renderAskInput() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nAsk %s a question (workspace %q)\n\n", m.askAgent, m.askTarget)
	b.WriteString(m.askInput.View())
	b.WriteString("\n\n")
	if m.askErr != "" {
		b.WriteString("  " + m.askErr + "\n")
	}
	b.WriteString("  Ctrl+S submit • Esc cancel\n")
	return b.String()
}
