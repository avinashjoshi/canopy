package workspace

import (
	"errors"
	"strings"
	"testing"
)

// TestAgentFallbackShell_IncludesAgentName: the fallback shell
// command must mention the missing binary so the user can identify
// what to install. The pane shows this hint then drops to a shell.
func TestAgentFallbackShell_IncludesAgentName(t *testing.T) {
	got := agentFallbackShell("claude", errors.New(`exec: "claude": executable file not found in $PATH`))

	if !strings.Contains(got, "claude") {
		t.Errorf("fallback should mention agent name: %q", got)
	}
	if !strings.Contains(got, "not on PATH") {
		t.Errorf("fallback should explain the cause: %q", got)
	}
}

// TestAgentFallbackShell_ExecsRealShell: the fallback ends in
// `exec "$SHELL"` so the pane behaves like a normal shell after
// the hint scrolls off. Without the exec, a wrapping shell would
// linger and prompt-loop oddly.
func TestAgentFallbackShell_ExecsRealShell(t *testing.T) {
	got := agentFallbackShell("claude", errors.New("missing"))
	if !strings.Contains(got, `exec "${SHELL:-/bin/sh}"`) {
		t.Errorf("fallback should exec the user's shell: %q", got)
	}
}

// TestAgentFallbackShell_QuotesSafely: special chars in the error
// message (single-quotes, backticks, $) must not break the shell
// command. Tests the POSIX '\'' escape pattern in the quoted hint.
func TestAgentFallbackShell_QuotesSafely(t *testing.T) {
	// Error message with characters that would break naive quoting.
	tricky := errors.New(`exec failed; doesn't ` + "`fail`" + ` quietly`)
	got := agentFallbackShell("claude", tricky)

	// The output is wrapped in single quotes; embedded singles use
	// the '\'' escape. We verify the message actually appears,
	// proving the escape didn't drop content.
	if !strings.Contains(got, "doesn") {
		t.Errorf("apostrophe content should survive escaping: %q", got)
	}
	if !strings.Contains(got, "`fail`") {
		t.Errorf("backtick content should survive escaping: %q", got)
	}
}
