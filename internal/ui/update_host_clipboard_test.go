package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/host"
)

func TestActionHostClipboard_OpensConfirmModal(t *testing.T) {
	m := &Model{
		tab:         tabHosts,
		hostsCursor: 0,
		hostList: []host.Host{
			{Name: "tower", SSHTarget: "avi@tower.lan", Type: "ssh"},
		},
	}
	_, _ = actionHostClipboard(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if m.mode != confirmHostClipboardMode {
		t.Errorf("mode = %v, want confirmHostClipboardMode", m.mode)
	}
	if m.hostClipboardName != "tower" {
		t.Errorf("hostClipboardName = %q, want %q", m.hostClipboardName, "tower")
	}
	if m.hostClipboardTarget != "avi@tower.lan" {
		t.Errorf("hostClipboardTarget = %q, want %q", m.hostClipboardTarget, "avi@tower.lan")
	}
}

func TestActionHostClipboard_NoOpWhenNoTarget(t *testing.T) {
	// A host with no ssh_target is meaningless to clipboard-install.
	// Action must leave the mode untouched (modal would dead-end).
	m := &Model{
		tab:         tabHosts,
		hostsCursor: 0,
		hostList: []host.Host{
			{Name: "stale", Type: "ssh"}, // no SSHTarget
		},
		mode: listMode,
	}
	_, _ = actionHostClipboard(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if m.mode != listMode {
		t.Errorf("mode = %v, want listMode (action should no-op on empty target)", m.mode)
	}
}

func TestHandleConfirmHostClipboardKey_CancelOnNonYes(t *testing.T) {
	m := &Model{
		mode:                confirmHostClipboardMode,
		hostClipboardName:   "tower",
		hostClipboardTarget: "avi@tower.lan",
	}
	for _, k := range []string{"n", "N", "esc", "q", "\n"} {
		m.mode = confirmHostClipboardMode
		m.hostClipboardName = "tower"
		m.hostClipboardTarget = "avi@tower.lan"
		_, cmd := m.handleConfirmHostClipboardKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		if m.mode != listMode {
			t.Errorf("key %q: mode = %v, want listMode", k, m.mode)
		}
		if m.hostClipboardName != "" || m.hostClipboardTarget != "" {
			t.Errorf("key %q: state not cleared (name=%q target=%q)", k, m.hostClipboardName, m.hostClipboardTarget)
		}
		if cmd != nil {
			t.Errorf("key %q: cancel must not return a tea.Cmd", k)
		}
	}
}

func TestHandleConfirmHostClipboardKey_NoOpWhenNameEmpty(t *testing.T) {
	// Defensive: if hostClipboardName somehow got cleared between modal
	// open and y-press, don't fire a subprocess against a phantom host.
	m := &Model{
		mode:              confirmHostClipboardMode,
		hostClipboardName: "",
	}
	_, cmd := m.handleConfirmHostClipboardKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if m.mode != listMode {
		t.Errorf("mode = %v, want listMode after no-op confirm", m.mode)
	}
	if cmd != nil {
		t.Error("expected no tea.Cmd when name is empty")
	}
}

func TestAvailableHostClipboard_GatedOffHostsTab(t *testing.T) {
	m := &Model{tab: tabLocal}
	if availableHostClipboard(m) {
		t.Error("`c` must be unavailable off the Hosts tab")
	}
}

func TestAvailableHostClipboard_RequiresSSHTarget(t *testing.T) {
	m := &Model{
		tab:         tabHosts,
		hostsCursor: 0,
		hostList: []host.Host{
			{Name: "stale", Type: "ssh"}, // no SSHTarget
		},
	}
	if availableHostClipboard(m) {
		t.Error("`c` must be unavailable when cursor host has no ssh_target")
	}
}

func TestAvailableHostClipboard_AvailableForValidHost(t *testing.T) {
	m := &Model{
		tab:         tabHosts,
		hostsCursor: 0,
		hostList: []host.Host{
			{Name: "tower", SSHTarget: "avi@tower.lan", Type: "ssh"},
		},
	}
	if !availableHostClipboard(m) {
		t.Error("`c` must be available for a host with a non-empty ssh_target")
	}
}

func TestRenderConfirmHostClipboard_IncludesHostAndTarget(t *testing.T) {
	m := &Model{
		hostClipboardName:   "tower",
		hostClipboardTarget: "avi@tower.lan",
	}
	body := m.renderConfirmHostClipboard()
	for _, must := range []string{"tower", "avi@tower.lan", "clipboard bridge", "install"} {
		if !strings.Contains(body, must) {
			t.Errorf("modal body missing %q\nbody:\n%s", must, body)
		}
	}
}
