// update_setowner.go — owner-edit modal (`o`) and the review-filter
// toggle (`m`) for the Workspaces tab. Lets the user mark a row as
// someone else's work to review, or clear it back to their own.
//
// The modal mirrors the Add Project form (update_addproject.go): a
// single textinput, Enter submits, Esc cancels. The one extra affordance
// is ctrl+d, which clears the owner back to "mine" — a distinct,
// explicit action so a fat-fingered empty Enter can't silently wipe
// ownership (empty Enter is rejected with an inline hint instead).
//
// Local rows mutate state.json directly via Manager.SetOwner. Remote
// rows dispatch `canopy set-owner --on <host>` over SSH through
// execRemoteVerb, the same path rm/retry use, with the row's project
// pinned via remoteCwdArg so a multi-project host edits the right one.

package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// renderOwnerForm draws the owner-edit modal: title, the target
// workspace (annotated with its host for remote rows), the textinput,
// a one-line gloss, and a footer that flips between the key legend and
// an inline error (e.g. empty submit).
func (m *Model) renderOwnerForm() string {
	var b strings.Builder
	row := m.ownerTarget
	b.WriteString(titleStyle.Render("Set owner"))
	b.WriteString("\n\n")
	target := row.Name
	if row.Host != "" {
		target += subtleStyle.Render("  (on " + row.Host + ")")
	}
	b.WriteString("  Workspace: " + target + "\n\n")
	b.WriteString("  Owner (github login or name):\n")
	b.WriteString("  " + m.ownerInput.View() + "\n\n")
	b.WriteString("  " + subtleStyle.Render("A login marks this as theirs to review. Yours stays unmarked."))
	b.WriteString("\n\n")
	if m.ownerError != "" {
		b.WriteString("  " + errorStyle.Render(m.ownerError))
	} else {
		b.WriteString("  " + subtleStyle.Render("enter: set  ·  ctrl+d: clear to me  ·  esc: cancel"))
	}
	return b.String()
}

// actionSetOwner handles `o`: open the owner-edit modal for the cursor
// row. No-ops on loading placeholders and the synthetic (main) row —
// neither is a real workspace that can carry an owner.
func actionSetOwner(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok || row.Loading || row.IsMain {
		return m, nil
	}
	m.ownerTarget = row
	m.mode = ownerFormMode
	m.ownerError = ""
	m.ownerInput.Reset()
	// Prefill with the current login so editing (rather than retyping)
	// is the common path. The reserved self-marker and the empty/legacy
	// states prefill blank — there's no login to show.
	if row.Owner != "" && row.Owner != state.OwnerSelfMarker {
		m.ownerInput.SetValue(row.Owner)
		m.ownerInput.CursorEnd()
	}
	m.ownerInput.Focus()
	return m, textinputBlink()
}

// actionToggleReviewFilter handles `m`: flip the "hide rows I'm only
// reviewing" filter so the list collapses to the user's own work (and
// back). Re-projects rows immediately so the toggle feels instant.
func actionToggleReviewFilter(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.hideReviewing = !m.hideReviewing
	m.list.SetRows(m.filteredRows())
	return m, nil
}

// closeOwnerForm returns to listMode and clears modal-only state.
func (m *Model) closeOwnerForm() {
	m.mode = listMode
	m.ownerInput.Blur()
	m.ownerInput.Reset()
	m.ownerError = ""
	m.ownerTarget = Row{}
}

// handleOwnerFormKey routes keys while the owner modal is open. Esc
// cancels, Enter sets the typed login, ctrl+d clears to "mine", and any
// other key forwards to the textinput (clearing a stale error first).
func (m *Model) handleOwnerFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.closeOwnerForm()
		return m, nil
	case "ctrl+d":
		return m.submitOwner(true)
	case "enter":
		return m.submitOwner(false)
	}
	if m.ownerError != "" {
		m.ownerError = ""
	}
	var cmd tea.Cmd
	m.ownerInput, cmd = m.ownerInput.Update(msg)
	return m, cmd
}

// submitOwner applies the owner change. clear=true resets the row to the
// user's own (state.OwnerSelfMarker); clear=false reads the textinput,
// normalizes it, and rejects an empty value with an inline hint rather
// than treating it as a clear.
func (m *Model) submitOwner(clear bool) (tea.Model, tea.Cmd) {
	row := m.ownerTarget

	ownerVal := state.OwnerSelfMarker
	if !clear {
		norm, ok := state.NormalizeOwner(m.ownerInput.Value())
		if !ok {
			m.ownerError = "type a login, or press ctrl+d to clear to yourself"
			return m, nil
		}
		ownerVal = norm
	}

	// Remote rows: dispatch `canopy set-owner --on <host>` over SSH. The
	// remote argument list is [name, login] for a set or [name, --clear]
	// for a clear, plus the row's project pin so multi-project hosts edit
	// the right workspace. execRemoteVerb appends `--on <host>` itself.
	if row.Host != "" {
		var args []string
		if clear {
			args = []string{row.Name, "--clear"}
		} else {
			args = []string{row.Name, ownerVal}
		}
		args = append(args, m.remoteCwdArg(row.Host, row.Project)...)
		m.closeOwnerForm()
		return m, m.execRemoteVerb(row.Host, "set-owner", args, false)
	}

	// Local row: flock'd single-field write via the row's manager.
	mgr, err := m.managerForRow(row)
	if err != nil {
		m.ownerError = "✗ " + err.Error()
		return m, nil
	}
	if err := mgr.SetOwner(context.Background(), row.Name, ownerVal); err != nil {
		if errors.Is(err, workspace.ErrWorkspaceNotFound) {
			m.ownerError = fmt.Sprintf("✗ no workspace named %q", row.Name)
		} else {
			m.ownerError = "✗ " + err.Error()
		}
		return m, nil
	}
	m.closeOwnerForm()
	return m, m.refresh()
}
