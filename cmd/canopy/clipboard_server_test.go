package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClipboardSocketDir_PrefersXDGRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/12345")
	got, err := clipboardSocketDir()
	if err != nil {
		t.Fatalf("clipboardSocketDir: %v", err)
	}
	want := filepath.Join("/run/user/12345", "canopy")
	if got != want {
		t.Errorf("clipboardSocketDir = %q, want %q", got, want)
	}
}

func TestClipboardSocketDir_FallsBackToTmpWhenXDGUnset(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	got, err := clipboardSocketDir()
	if err != nil {
		t.Fatalf("clipboardSocketDir: %v", err)
	}
	// We can't assert the exact UID at compile time, but the shape is
	// stable: /tmp/canopy-<digits>. Match that shape so the test
	// survives running under any UID.
	if !strings.HasPrefix(got, "/tmp/canopy-") {
		t.Errorf("clipboardSocketDir fallback = %q, want /tmp/canopy-<uid>", got)
	}
}
