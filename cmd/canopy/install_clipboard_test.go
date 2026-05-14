package main

import (
	"strings"
	"testing"
)

func TestInstallClipboardBridgeCmd_HasExpectedShape(t *testing.T) {
	// Cobra subcommand smoke: verify the Use string and that the
	// in-tmux annotation is set (the command is safe inside a tmux
	// session because it only edits ~/.config and ~/.ssh).
	cmd := newInstallClipboardBridgeCmd()
	if cmd.Use != "clipboard-bridge" {
		t.Errorf("Use = %q, want %q", cmd.Use, "clipboard-bridge")
	}
	if v, ok := cmd.Annotations[allowInTmuxAnnotation]; !ok || v != "true" {
		t.Errorf("missing %s annotation; got Annotations=%v", allowInTmuxAnnotation, cmd.Annotations)
	}
	// RunE wired (not nil); the real install path is exercised by
	// internal/clipboard install_test.go's TestInstall_RunsAllStepsInOrder.
	if cmd.RunE == nil {
		t.Error("RunE not wired")
	}
	if !strings.Contains(cmd.Long, "systemctl") {
		t.Error("Long help should mention systemctl so users know what it'll run")
	}
}

func TestInstallCmd_RegistersClipboardBridgeTarget(t *testing.T) {
	parent := newInstallCmd()
	var names []string
	for _, sub := range parent.Commands() {
		names = append(names, sub.Use)
	}
	wantPresent := []string{"tmux", "clipboard-bridge"}
	for _, w := range wantPresent {
		found := false
		for _, n := range names {
			if n == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("`canopy install` is missing the %q subcommand; got %v", w, names)
		}
	}
}
