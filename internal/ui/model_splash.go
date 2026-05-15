// model_splash.go — Add Project splash for first-run / fresh-repo
// onboarding (v0.20 rewrite of the pre-v0.20 "press i" prompt).
//
// Reachable when canopy launches with no projects registered AND cwd
// is a git repo. The splash shows a textinput pre-loaded with the
// cwd path; the user can:
//
//   - Press Enter on the cwd → init this dir (the old "i" muscle
//     memory, preserved via the prefilled default).
//   - Edit the value to another path → init that dir from anywhere.
//   - Paste a git URL → splash quits with the URL, caller clones+inits.
//   - Press Esc → quit without changes.
//
// Why split splash from the main TUI's addProjectFormMode: the splash
// runs in its own tea.Program BEFORE the main TUI starts. Keeping the
// rendering shared but the program separate avoids the "Bubbletea-
// inside-Bubbletea" trap that mid-program Model swaps create. The
// splash returns user input to the caller; the caller runs the
// orchestrator AFTER altscreen drops, which means git.Clone inherits
// the real tty for SSH passphrase / HTTPS credential prompts.

package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// SplashAction names the outcome of a splash run. The caller switches
// on this to decide whether to invoke the orchestrator.
type SplashAction int

const (
	// SplashDismiss: user pressed esc / ctrl+c / q. Caller should
	// exit cleanly (no error, no init).
	SplashDismiss SplashAction = iota
	// SplashSubmit: user pressed Enter. The Arg field carries the
	// URL/path they typed (or the prefilled cwd if they didn't edit).
	SplashSubmit
)

// SplashResult is the splash's return shape. Replaces the pre-v0.20
// `(didInit bool, err error)` signature so the caller can distinguish
// "init cwd" from "init <other path>" from "clone <url>".
type SplashResult struct {
	Action SplashAction
	// Arg is the user's typed input. Empty when Action is
	// SplashDismiss. May be a path or a git URL when Action is
	// SplashSubmit — classification (looksLikeGitURL) happens in the
	// orchestrator, not here.
	Arg string
}

// InitSplashModel renders the Add Project splash screen. State: cwd
// (the default value), one textinput, and a sticky result for the
// caller to read after Run returns.
type InitSplashModel struct {
	cwd    string
	input  textinput.Model
	result SplashResult

	width, height int
}

// NewInitSplash constructs an InitSplashModel for the given cwd. The
// cwd is pre-loaded into the input so an Enter-on-default replicates
// pre-v0.20's one-key "init this dir" flow.
func NewInitSplash(cwd string) *InitSplashModel {
	ti := textinput.New()
	ti.Placeholder = "https://github.com/foo/bar.git or ~/code/foo"
	ti.CharLimit = 1024
	ti.Width = 60
	ti.SetValue(cwd)
	ti.CursorEnd()
	ti.Focus()
	return &InitSplashModel{cwd: cwd, input: ti}
}

// RunInitSplash launches the splash in its own tea.Program. Returns
// the user's choice + any tea.Program error (rare).
//
// The caller (cmd/canopy/route.go) reads the SplashResult and dispatches
// to the same `runAddProject` orchestrator the CLI uses, so the splash's
// URL path produces the same end state as `canopy init <url>`.
func RunInitSplash(cwd string) (SplashResult, error) {
	m := NewInitSplash(cwd)
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return SplashResult{Action: SplashDismiss}, err
	}
	if sm, ok := final.(*InitSplashModel); ok {
		return sm.result, nil
	}
	return SplashResult{Action: SplashDismiss}, nil
}

// Init implements tea.Model. Returns the textinput blink so the caret
// animates from the first frame.
func (m *InitSplashModel) Init() tea.Cmd {
	return textinput.Blink
}

// Update implements tea.Model. Enter submits; esc/ctrl+c dismiss;
// everything else forwards to the input.
func (m *InitSplashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "ctrl+c":
			m.result = SplashResult{Action: SplashDismiss}
			return m, tea.Quit
		case "enter":
			m.result = SplashResult{Action: SplashSubmit, Arg: strings.TrimSpace(m.input.Value())}
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// View implements tea.Model. Same form layout the main TUI's
// addProjectFormMode uses, minus the "ctrl+s change source" hint
// (splash users haven't configured source-root yet; they're typing a
// path or URL).
func (m *InitSplashModel) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Add Project"))
	b.WriteString("\n\n")
	b.WriteString("  Folder path or git URL:\n")
	b.WriteString("  " + m.input.View() + "\n")
	b.WriteString("\n")
	b.WriteString("  " + subtleStyle.Render("Press Enter to init the current directory, or paste a path / git URL."))
	b.WriteString("\n\n")
	b.WriteString("  " + subtleStyle.Render("enter: add  ·  esc: cancel"))
	return b.String()
}

// Compile-time check.
var _ tea.Model = (*InitSplashModel)(nil)
