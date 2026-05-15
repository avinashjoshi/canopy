// update_settings.go — top-level settings modal (v0.18 Phase D1).
//
// Reachable from any tab via the `,` keybind. Tiny modal: title, one
// textinput for source-root, status hint, help legend. Enter saves to
// ~/.canopy/config.json via the same flock-protected WithLock path
// the CLI's `canopy config set` uses. Esc cancels.
//
// Why a separate mode rather than re-using addProjectFormMode's inline
// editor: discoverability. The pre-D1 flow required `a` (open Add
// Project) → ctrl+s (toggle inline editor). Two keys deep, only
// visible after opening the form. The `,` keybind surfaces settings
// directly in the help legend (?), so users find it without
// excavating.
//
// Reuses the same textinput field (addProjectInput) as the Add Project
// form. Mode disambiguates between "I'm typing a URL/path" and "I'm
// typing a source-root value" without juggling two textinputs.

package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/config"
)

// actionOpenSettings is the binding-table entry for the `,` keybind.
// Delegates to openSettingsForm.
func actionOpenSettings(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m, m.openSettingsForm()
}

// openSettingsForm transitions into settingsFormMode. Seeds the input
// with the current effective source-root value so the user sees what
// they're about to replace (rather than typing into an empty field
// with no reference).
func (m *Model) openSettingsForm() tea.Cmd {
	root, _, err := resolveCurrentSourceRoot()
	if err != nil {
		m.err = err
		return nil
	}
	m.mode = settingsFormMode
	m.addProjectInput.Reset()
	m.addProjectInput.SetValue(root)
	m.addProjectInput.CursorEnd()
	m.addProjectInput.Focus()
	m.addProjectError = ""
	m.addProjectToast = ""
	return textinputBlink()
}

// closeSettingsForm returns to listMode and clears form state.
func (m *Model) closeSettingsForm() {
	m.mode = listMode
	m.addProjectInput.Blur()
	m.addProjectInput.Reset()
	m.addProjectError = ""
	m.addProjectToast = ""
}

// handleSettingsFormKey routes keys for the standalone settings modal.
// Esc cancels; Enter saves; anything else forwards to the input.
func (m *Model) handleSettingsFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.closeSettingsForm()
		return m, nil
	case "enter":
		return m.submitSettings()
	}
	if m.addProjectError != "" {
		m.addProjectError = ""
	}
	var cmd tea.Cmd
	m.addProjectInput, cmd = m.addProjectInput.Update(msg)
	return m, cmd
}

// submitSettings writes the new source-root to ~/.canopy/config.json.
// Empty input is treated as "unset" — clears the config key so the
// effective value falls back to env / default per ResolveSourceRoot.
//
// On success the modal closes and the user returns to whichever tab
// they were on. No toast: settings changes are silent (the user
// already sees the value they typed; redundant feedback is noise).
func (m *Model) submitSettings() (tea.Model, tea.Cmd) {
	newRoot := strings.TrimSpace(m.addProjectInput.Value())
	canopyHome, err := canopyHomeDir()
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	store, err := config.NewUserStore(canopyHome)
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	err = store.WithLock(func(c *config.UserConfig) error {
		c.SourceRoot = newRoot
		return nil
	})
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	m.closeSettingsForm()
	return m, nil
}

// renderSettingsForm draws the modal. Same lipgloss styles as
// renderAddProjectForm so settings looks like a sibling surface, not
// a different visual language.
func (m *Model) renderSettingsForm() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Settings"))
	b.WriteString("\n\n")
	b.WriteString("  Source root:\n")
	b.WriteString("  " + m.addProjectInput.View() + "\n")
	b.WriteString("\n")
	b.WriteString("  " + subtleStyle.Render("Where canopy clones repos. Empty value → fall back to env/default."))
	b.WriteString("\n\n")
	if m.addProjectError != "" {
		b.WriteString("  " + errorStyle.Render(m.addProjectError))
	} else {
		b.WriteString("  " + subtleStyle.Render("enter: save  ·  esc: cancel"))
	}
	return b.String()
}
