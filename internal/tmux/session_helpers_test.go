package tmux_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCapturePane_RoundTrip verifies that CapturePane returns the
// content of a pane after a known string was sent into it.
func TestCapturePane_RoundTrip(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	paneID, err := c.Create(ctx, "capture-test", cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Send a marker string into the shell. SendKeysLiteral types it
	// without interpretation; SendKeyName submits it.
	const marker = "CAPTURE_MARKER_XYZ"
	if err := c.SendKeysLiteral(ctx, paneID, "echo "+marker); err != nil {
		t.Fatalf("SendKeysLiteral: %v", err)
	}
	if err := c.SendKeyName(ctx, paneID, "Enter"); err != nil {
		t.Fatalf("SendKeyName Enter: %v", err)
	}

	// Wait for the shell to render the echo output.
	time.Sleep(300 * time.Millisecond)

	got, err := c.CapturePane(ctx, paneID)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(got, marker) {
		t.Errorf("CapturePane output missing marker:\n--- captured ---\n%s\n--- end ---", got)
	}
}

// TestCapturePane_ContextTimeout verifies that the context timeout is
// honored. A 1-nanosecond deadline must produce an error rather than
// hanging.
func TestCapturePane_ContextTimeout(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	paneID, err := c.Create(ctx, "timeout-test", cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tightCtx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	_, err = c.CapturePane(tightCtx, paneID)
	if err == nil {
		t.Error("CapturePane with 1ns timeout returned nil error; want timeout error")
	}
}

// TestSendKeysLiteral_DoesNotInterpretKeyNames verifies the codex
// review v3-B2 fix: a prompt containing the literal word "Enter" must
// be typed as text, NOT interpreted as a keypress.
func TestSendKeysLiteral_DoesNotInterpretKeyNames(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	paneID, err := c.Create(ctx, "literal-test", cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// "Enter" must arrive as 5 characters in the input buffer, not as
	// a key press that would submit the empty line. We send `echo
	// "press Enter to continue"` then submit with a SEPARATE Enter
	// keypress; if SendKeysLiteral incorrectly interpreted the word
	// "Enter", the partial command would have already submitted and
	// the echo wouldn't fire correctly.
	const phrase = `echo "press Enter to continue done"`
	if err := c.SendKeysLiteral(ctx, paneID, phrase); err != nil {
		t.Fatalf("SendKeysLiteral: %v", err)
	}
	if err := c.SendKeyName(ctx, paneID, "Enter"); err != nil {
		t.Fatalf("SendKeyName: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	got, err := c.CapturePane(ctx, paneID)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	// The full phrase including the literal word "Enter" should appear
	// in the echo output. If literal mode wasn't honored, we'd see the
	// shell prompt re-rendered after partial input.
	if !strings.Contains(got, "press Enter to continue done") {
		t.Errorf("literal text not preserved in pane:\n--- captured ---\n%s\n--- end ---", got)
	}
}

// TestSendKeysLiteral_LeadingDashSafe verifies a prompt starting with
// `-` doesn't get parsed as a tmux flag.
func TestSendKeysLiteral_LeadingDashSafe(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	paneID, err := c.Create(ctx, "dash-test", cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := c.SendKeysLiteral(ctx, paneID, "--help"); err != nil {
		t.Errorf("SendKeysLiteral with leading dash: %v", err)
	}
}

// TestLoadAndPasteBuffer_RoundTrip verifies that multi-line content
// survives load-buffer + paste-buffer + delete-buffer.
func TestLoadAndPasteBuffer_RoundTrip(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	// Use `cat` as the long-running pane process so we can paste into
	// stdin and read what came out via capture-pane.
	paneID, err := c.Create(ctx, "paste-test", cwd, "cat")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const content = "LINE_ONE_OK\nLINE_TWO_OK\nLINE_THREE_OK\n"
	if err := c.LoadAndPasteBuffer(ctx, paneID, "canopy-paste-test-prompt", content); err != nil {
		t.Fatalf("LoadAndPasteBuffer: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	got, err := c.CapturePane(ctx, paneID)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	for _, line := range []string{"LINE_ONE_OK", "LINE_TWO_OK", "LINE_THREE_OK"} {
		if !strings.Contains(got, line) {
			t.Errorf("pasted line missing in pane: %q\n--- captured ---\n%s\n--- end ---", line, got)
		}
	}
}

// TestLoadAndPasteBuffer_NamedBufferIsolation verifies that two
// concurrent load-and-paste operations with DIFFERENT buffer names
// don't clobber each other (codex review M4 — named-buffer discipline).
func TestLoadAndPasteBuffer_NamedBufferIsolation(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	paneA, err := c.Create(ctx, "iso-a", cwd, "cat")
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	paneB, err := c.Create(ctx, "iso-b", cwd, "cat")
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}

	// Load both buffers, paste both, in interleaved order. With named
	// buffers each paste must pick up its OWN content.
	if err := c.LoadAndPasteBuffer(ctx, paneA, "canopy-iso-a-prompt", "MARKER_AAAA\n"); err != nil {
		t.Fatalf("paste A: %v", err)
	}
	if err := c.LoadAndPasteBuffer(ctx, paneB, "canopy-iso-b-prompt", "MARKER_BBBB\n"); err != nil {
		t.Fatalf("paste B: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	gotA, _ := c.CapturePane(ctx, paneA)
	gotB, _ := c.CapturePane(ctx, paneB)
	if !strings.Contains(gotA, "MARKER_AAAA") || strings.Contains(gotA, "MARKER_BBBB") {
		t.Errorf("pane A leaked across buffers:\n%s", gotA)
	}
	if !strings.Contains(gotB, "MARKER_BBBB") || strings.Contains(gotB, "MARKER_AAAA") {
		t.Errorf("pane B leaked across buffers:\n%s", gotB)
	}
}

// TestListAgentPanes_NoServer returns nil rows + nil err when no tmux
// server is running yet — important so the TUI poll doesn't error on
// first launch before any workspace exists.
func TestListAgentPanes_NoServer(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()

	rows, err := c.ListAgentPanes(ctx)
	if err != nil {
		t.Errorf("ListAgentPanes with no server: err = %v, want nil", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListAgentPanes with no server: got %d rows, want 0", len(rows))
	}
}

// TestListAgentPanes_FiltersByAgentPrefix verifies that ListAgentPanes
// returns only panes whose role starts with "agent:" — sessions
// without the role tag, or with non-agent roles, are filtered out.
func TestListAgentPanes_FiltersByAgentPrefix(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	agentPane, err := c.Create(ctx, "ws-with-agent", cwd, "")
	if err != nil {
		t.Fatalf("Create agent ws: %v", err)
	}
	if err := c.SetRole(ctx, agentPane, "agent:claude"); err != nil {
		t.Fatalf("SetRole agent: %v", err)
	}

	idePane, err := c.Create(ctx, "ws-with-ide", cwd, "")
	if err != nil {
		t.Fatalf("Create ide ws: %v", err)
	}
	if err := c.SetRole(ctx, idePane, "ide"); err != nil {
		t.Fatalf("SetRole ide: %v", err)
	}

	if _, err := c.Create(ctx, "ws-untagged", cwd, ""); err != nil {
		t.Fatalf("Create untagged ws: %v", err)
	}

	rows, err := c.ListAgentPanes(ctx)
	if err != nil {
		t.Fatalf("ListAgentPanes: %v", err)
	}

	var foundAgent, foundIDE, foundUntagged bool
	for _, r := range rows {
		switch r.Session {
		case "ws-with-agent":
			foundAgent = true
			if r.Role != "agent:claude" {
				t.Errorf("agent pane role = %q, want agent:claude", r.Role)
			}
		case "ws-with-ide":
			foundIDE = true
		case "ws-untagged":
			foundUntagged = true
		}
	}
	if !foundAgent {
		t.Error("ListAgentPanes did not return ws-with-agent")
	}
	if foundIDE {
		t.Error("ListAgentPanes returned ws-with-ide (role=ide should be filtered)")
	}
	if foundUntagged {
		t.Error("ListAgentPanes returned ws-untagged (no role tag should be filtered)")
	}
}

// TestListAgentPanes_RoleWithPipeCharacter verifies that an
// @canopy-role value containing the `|` character (which the prior
// design used as a field separator) doesn't break parsing — the SOH
// delimiter (codex review v3-B4) keeps fields distinct.
func TestListAgentPanes_RoleWithPipeCharacter(t *testing.T) {
	requireTmux(t)
	c := newClient(t)
	ctx := context.Background()
	cwd := t.TempDir()

	paneID, err := c.Create(ctx, "ws-pipe", cwd, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Set a role with a literal pipe to prove the parser doesn't choke.
	if err := c.SetRole(ctx, paneID, "agent:custom|with|pipes"); err != nil {
		t.Fatalf("SetRole with pipe: %v", err)
	}

	rows, err := c.ListAgentPanes(ctx)
	if err != nil {
		t.Fatalf("ListAgentPanes: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Session == "ws-pipe" {
			found = true
			if r.Role != "agent:custom|with|pipes" {
				t.Errorf("role with pipes mangled: got %q, want %q", r.Role, "agent:custom|with|pipes")
			}
			if r.ID == "" {
				t.Error("pane ID empty (parser ate it)")
			}
		}
	}
	if !found {
		t.Error("ListAgentPanes did not return the pipe-roled pane")
	}
}
