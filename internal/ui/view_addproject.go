// view_addproject.go — render the v0.20 Add Project form.
//
// Modal-style layout: title, prompt, textinput, status (source-root
// or inline editor hint), and a footer that flips between three
// states:
//
//   - default: help legend  "enter: add · esc: cancel · ctrl+s: change source"
//   - error:   errorStyle red, "✗ <message>"  — replaces the legend
//   - toast:   readyStyle green, "✓ Added <name> at <path>" — replaces legend
//
// The form lives inside whatever Bubbletea program hosts it: the
// splash on first run, the main TUI on the Global tab. Both invoke
// renderAddProjectForm via their View() — no duplicated rendering.
//
// Truncation: when the source-root path is too long for the terminal
// width, left-truncate with `…` so the dir basename remains visible
// (decision #6A in v0.20-add-project.md).

package ui

import (
	"fmt"
	"strings"
)

// renderAddProjectForm draws the form. Returns plain text; the caller
// (View()) wraps it in any outer padding/border it wants.
func (m *Model) renderAddProjectForm() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Add Project"))
	b.WriteString("\n\n")
	if m.addProjectEditingSourceRoot {
		b.WriteString("  Source root:\n")
		b.WriteString("  " + m.addProjectInput.View() + "\n")
		b.WriteString("\n")
		b.WriteString("  " + subtleStyle.Render("Where canopy clones repos. Empty value → fall back to env/default."))
		b.WriteString("\n\n")
		b.WriteString("  " + subtleStyle.Render("enter: save  ·  esc: cancel"))
		return b.String()
	}

	b.WriteString("  Folder path or git URL:\n")
	b.WriteString("  " + m.addProjectInput.View() + "\n")
	b.WriteString("\n")

	// Status line: source-root + label (config / env / default).
	srcLine := m.renderSourceRootStatus()
	b.WriteString("  " + subtleStyle.Render(srcLine))
	b.WriteString("\n\n")

	// Footer: error, toast, or default legend.
	switch {
	case m.addProjectError != "":
		b.WriteString("  " + errorStyle.Render(m.addProjectError))
	case m.addProjectToast != "":
		b.WriteString("  " + readyStyle.Render(m.addProjectToast))
	default:
		b.WriteString("  " + subtleStyle.Render("enter: add  ·  esc: cancel  ·  ctrl+s: change source"))
	}
	return b.String()
}

// renderSourceRootStatus formats the "Source: <path>  (<label>)" line.
// On narrow terminals, the path is left-truncated with `…` so the
// basename always remains visible (decision #6A).
func (m *Model) renderSourceRootStatus() string {
	path, label, err := resolveCurrentSourceRoot()
	if err != nil {
		return fmt.Sprintf("Source: <unavailable: %v>", err)
	}
	// 6 = strlen("Source") + ": " + 1 space breathing room.
	// Reserve label width too (~12 chars max for the longest "(default)").
	budget := m.width - 18 - len(label)
	if budget < 12 {
		budget = 12
	}
	displayPath := path
	if len(path) > budget {
		// Left-truncate so the basename survives.
		keep := budget - 1 // 1 for the ellipsis
		if keep < 1 {
			keep = 1
		}
		displayPath = "…" + path[len(path)-keep:]
	}
	return fmt.Sprintf("Source: %s  (%s)", displayPath, label)
}
