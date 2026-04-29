package main

import (
	"context"
	"strings"
	"testing"
)

// TestRunPopup_outsideTmux verifies the launcher refuses cleanly when
// $TMUX is unset, with a hint pointing at the alternative (`canopy`
// directly or starting tmux first).
func TestRunPopup_outsideTmux(t *testing.T) {
	// Force TMUX unset for the duration of this test. Restored by t.Setenv.
	t.Setenv("TMUX", "")

	cmd := newPopupCmd()
	cmd.SetArgs([]string{})
	cmd.SetContext(context.Background())
	// Silence cobra's auto-print of errors to stderr; we read the err
	// directly from Execute.
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute: got nil; want error about missing tmux")
	}
	msg := err.Error()
	// Three things the message must convey: it requires tmux, the user
	// is outside tmux, and there's an alternative.
	for _, want := range []string{"requires tmux", "outside", "Run `canopy`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\ngot: %s", want, msg)
		}
	}
}

// TestShellQuote covers the path-quoting helper used to embed the
// canopy binary path in the display-popup shell command. Real install
// paths (~/go/bin/canopy, /usr/bin/canopy, ./canopy) shouldn't get
// quoted; paths with spaces must.
func TestShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/usr/bin/canopy", "/usr/bin/canopy"},
		{"/home/avi/go/bin/canopy", "/home/avi/go/bin/canopy"},
		{"./canopy", "./canopy"},
		{"canopy-v0.7", "canopy-v0.7"},
		{"/path with spaces/canopy", "'/path with spaces/canopy'"},
		{"/path/with$dollar/canopy", "'/path/with$dollar/canopy'"},
		{"/path/with&amp/canopy", "'/path/with&amp/canopy'"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := shellQuote(tc.in); got != tc.want {
				t.Errorf("shellQuote(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSelfBinaryPath_returnsAbsolute verifies the helper returns a
// non-empty absolute path. We don't assert a specific value because
// `go test` invokes a temp test binary that varies per run.
func TestSelfBinaryPath_returnsAbsolute(t *testing.T) {
	path, err := selfBinaryPath()
	if err != nil {
		t.Fatalf("selfBinaryPath: %v", err)
	}
	if path == "" {
		t.Fatal("selfBinaryPath returned empty")
	}
	if !strings.HasPrefix(path, "/") {
		t.Errorf("selfBinaryPath returned non-absolute %q", path)
	}
}
