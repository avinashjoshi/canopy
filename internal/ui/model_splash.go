// InitSplashModel is the onboarding screen shown when `canopy` runs in a
// directory that's a git repo but has no canopy.json. Single screen,
// two keys: 'i' to opt into init, anything else to quit.
//
// Why a Bubbletea Model for a single-screen prompt: consistency. Every
// canopy TUI surface goes through the same lipgloss styles + altscreen
// behavior, and a 30-line file means the `canopy` no-args UX feels
// uniform whether you land in the project view, global view, or init
// splash. The user never sees a raw shell prompt for any of these
// surfaces.
//
// Splash explicitly lists NOTHING — no projects, no workspaces. The
// global TUI handles "no project here, but other projects exist." Splash
// handles "no project here AND this looks like a fresh repo." Different
// surfaces, different copy, no shared rendering.

package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// InitSplashModel is the read-only init prompt. State is small: just
// the cwd we'll init when the user presses 'i', plus the didInit flag
// the caller checks after Run returns.
//
// We deliberately do NOT call runInit from inside the Bubbletea program.
// Instead, 'i' sets didInit=true and triggers tea.Quit. The caller
// (cmd/canopy/route.go) sees the flag, exits altscreen cleanly, and then
// runs runInit synchronously so its output prints to a normal terminal
// (post-altscreen). This avoids the Bubbletea-inside-Bubbletea trap that
// mid-program Model swaps create.
type InitSplashModel struct {
	cwd     string
	didInit bool

	width, height int
}

// NewInitSplash constructs an InitSplashModel for the given cwd. The
// path is shown to the user in the prompt copy so they can confirm
// they're about to init the right place.
func NewInitSplash(cwd string) *InitSplashModel {
	return &InitSplashModel{cwd: cwd}
}

// RunInitSplash launches the splash. Returns (didInit, err): didInit is
// true iff the user pressed 'i' (the caller should then run init);
// false means the user dismissed without initializing. err surfaces
// any tea.Program failure, which is rare.
func RunInitSplash(cwd string) (bool, error) {
	m := NewInitSplash(cwd)
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return false, err
	}
	// final is a tea.Model interface; cast back to *InitSplashModel.
	if sm, ok := final.(*InitSplashModel); ok {
		return sm.didInit, nil
	}
	return false, nil
}

// Init implements tea.Model. No startup work — splash is purely reactive.
func (m *InitSplashModel) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model. Only reacts to keypresses: 'i' opts into
// init and quits, 'q' / ctrl+c quits without init, everything else is
// ignored (so a stray modifier doesn't accidentally dismiss the splash).
func (m *InitSplashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "i", "I":
			m.didInit = true
			return m, tea.Quit
		case "q", "Q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	}
	return m, nil
}

// View implements tea.Model. Single screen: title + cwd context + two-
// key prompt. Soft tone — this is an onboarding moment, not an error.
func (m *InitSplashModel) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("canopy"))
	b.WriteString("\n\n")
	b.WriteString("  This directory isn't a canopy project yet.\n")
	b.WriteString("\n")
	b.WriteString("  ")
	b.WriteString(subtleStyle.Render(m.cwd))
	b.WriteString("\n\n")
	b.WriteString("  Press ")
	b.WriteString(titleStyle.Render("i"))
	b.WriteString(" to run ")
	b.WriteString(titleStyle.Render("canopy init"))
	b.WriteString(" — drops a minimal canopy.json,\n")
	b.WriteString("  no scripts, idempotent. You can re-run with ")
	b.WriteString(subtleStyle.Render("--with-scripts"))
	b.WriteString(" later.\n")
	b.WriteString("\n")
	b.WriteString("  Press ")
	b.WriteString(titleStyle.Render("q"))
	b.WriteString(" to quit without changes.\n")
	return b.String()
}

// Compile-time check.
var _ tea.Model = (*InitSplashModel)(nil)
