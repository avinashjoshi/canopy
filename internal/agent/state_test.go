package agent

import (
	"strings"
	"testing"
)

func TestState_String(t *testing.T) {
	cases := []struct {
		state State
		want  string
	}{
		{StateUnknown, "unknown"},
		{StateIdle, "idle"},
		{StateThinking, "thinking"},
		{StateAwaitingInput, "awaiting_input"},
		{State(99), "unknown"}, // unrecognized value falls through to unknown
	}
	for _, c := range cases {
		if got := c.state.String(); got != c.want {
			t.Errorf("State(%d).String() = %q, want %q", c.state, got, c.want)
		}
	}
}

func TestLauncherFromRole(t *testing.T) {
	cases := []struct {
		role string
		want string
	}{
		{"agent:claude", "claude"},
		{"agent:codex", "codex"},
		{"agent:opencode", "opencode"},
		{"agent:claude:extra", "claude"}, // first non-empty token wins
		{"agent:", ""},                   // empty suffix → malformed
		{"agent::foo", ""},               // empty first token → malformed
		{"agent", ""},                    // missing colon → malformed
		{"ide", ""},                      // not an agent role
		{"terminal:shell", ""},           // not an agent role
		{"", ""},
	}
	for _, c := range cases {
		if got := LauncherFromRole(c.role); got != c.want {
			t.Errorf("LauncherFromRole(%q) = %q, want %q", c.role, got, c.want)
		}
	}
}

func TestNormalize_StripsAnsi(t *testing.T) {
	in := "\x1b[31mhello\x1b[0m world\n\x1b[2J\x1b[H"
	got := normalize(in)
	if strings.Contains(got, "\x1b") {
		t.Errorf("normalize() left ANSI escape: %q", got)
	}
	if !strings.Contains(got, "hello world") {
		t.Errorf("normalize() lost text content: %q", got)
	}
}

func TestNormalize_StripsSpinnerLine(t *testing.T) {
	cases := []string{
		"✻ Baked for 2s",
		"✻ Cooking for 17s",
		"● Churned for 1s",
		"  Brewing for 99s",
		"Thinking for 4s",
	}
	for _, line := range cases {
		in := "before\n" + line + "\nafter"
		got := normalize(in)
		if strings.Contains(got, "for ") && (strings.Contains(got, "s") || strings.Contains(got, "Baked")) {
			// Heuristic check: any spinner verb left in the output is a fail.
			for _, verb := range []string{"Baked", "Cooking", "Churned", "Brewing", "Thinking", "Simmering"} {
				if strings.Contains(got, verb) {
					t.Errorf("normalize() left spinner verb %q from input %q: %q", verb, line, got)
				}
			}
		}
		if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
			t.Errorf("normalize() ate context for %q: %q", line, got)
		}
	}
}

func TestNormalize_StripsFooter(t *testing.T) {
	in := "real content\n  ⏵⏵ auto mode on (shift+tab to cycle)\nmore content"
	got := normalize(in)
	if strings.Contains(got, "⏵⏵") {
		t.Errorf("normalize() left footer: %q", got)
	}
	if !strings.Contains(got, "real content") || !strings.Contains(got, "more content") {
		t.Errorf("normalize() ate non-footer content: %q", got)
	}
}

func TestNormalize_StableAcrossCosmeticChanges(t *testing.T) {
	// The same logical content with cosmetic-only differences must
	// normalize to the same string. This is the load-bearing property
	// for "stable for N polls = idle".
	t1 := "❯ Try \"explain this\"\n\x1b[31m✻ Baked for 1s\x1b[0m\n  ⏵⏵ auto mode on"
	t2 := "❯ Try \"explain this\"\n\x1b[31m✻ Baked for 17s\x1b[0m\n  ⏵⏵ auto mode on"
	t3 := "❯ Try \"explain this\"\n\x1b[32m● Cooking for 4s\x1b[0m\n  ⏵⏵ auto mode on"
	a, b, c := normalize(t1), normalize(t2), normalize(t3)
	if a != b || b != c {
		t.Errorf("normalize() not stable across cosmetic changes:\n  t1: %q\n  t2: %q\n  t3: %q", a, b, c)
	}
}

func TestDetector_FirstObservation_IsUnknown(t *testing.T) {
	d := NewDetector()
	state, _ := d.Observe("%0", "claude", "anything")
	if state != StateUnknown {
		t.Errorf("first Observe returned %v, want StateUnknown", state)
	}
}

func TestDetector_StableContent_ClaudeWithIdleMarker_IsIdle(t *testing.T) {
	d := NewDetector()
	content := "❯ Try \"explain auth.ts\"\n  ⏵⏵ auto mode on (shift+tab to cycle)"
	d.Observe("%0", "claude", content)
	state, _ := d.Observe("%0", "claude", content)
	if state != StateIdle {
		t.Errorf("stable claude pane with idle marker = %v, want StateIdle", state)
	}
}

func TestDetector_ChangedContent_IsThinking(t *testing.T) {
	d := NewDetector()
	d.Observe("%0", "claude", "first content")
	state, _ := d.Observe("%0", "claude", "different content")
	if state != StateThinking {
		t.Errorf("changed content = %v, want StateThinking", state)
	}
}

func TestDetector_StableContent_AwaitingPattern_IsAwaitingInput(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"y/N", "Continue with deletion (y/N)"},
		{"square y/N", "Approve [y/N]"},
		{"approve command", "Approve this command? Run rm -rf /tmp/cache"},
		{"allow tool use", "Allow tool use of Bash to run npm install"},
		{"selector", "Choose option:\n  ❯ 1. Option A\n    2. Option B\nEnter to confirm · Esc to cancel"},
		{"numbered selector", "  ❯ 1. Yes, I trust this folder\n    2. No, exit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := NewDetector()
			d.Observe("%0", "claude", c.content)
			state, _ := d.Observe("%0", "claude", c.content)
			if state != StateAwaitingInput {
				t.Errorf("awaiting pattern %q = %v, want StateAwaitingInput", c.name, state)
			}
		})
	}
}

func TestDetector_StableContent_ClaudeNoMarkers_IsIdleLowConfidence(t *testing.T) {
	// Claude pane that's stable but has no idle markers (e.g.,
	// post-response content with only the response visible). Should
	// still classify as Idle but with weak confidence so dogfood logs
	// surface it for pattern tuning.
	d := NewDetector()
	content := "● Here is the answer to your question.\nIt has multiple lines."
	d.Observe("%0", "claude", content)
	state, conf := d.Observe("%0", "claude", content)
	if state != StateIdle {
		t.Errorf("stable claude no markers = %v, want StateIdle", state)
	}
	if conf >= 8 {
		t.Errorf("expected low confidence (<8), got %d", conf)
	}
}

func TestDetector_UnknownLauncher_StableContent_IsUnknown(t *testing.T) {
	d := NewDetector()
	d.Observe("%0", "codex", "stable content")
	state, _ := d.Observe("%0", "codex", "stable content")
	if state != StateUnknown {
		t.Errorf("unknown launcher stable = %v, want StateUnknown (no markers known)", state)
	}
}

func TestDetector_UnknownLauncher_ChangingContent_IsThinking(t *testing.T) {
	d := NewDetector()
	d.Observe("%0", "codex", "first")
	state, _ := d.Observe("%0", "codex", "different")
	if state != StateThinking {
		t.Errorf("unknown launcher changing = %v, want StateThinking (motion is launcher-agnostic)", state)
	}
}

func TestDetector_EmptyLauncher_TreatedAsMalformed(t *testing.T) {
	// Empty launcher comes from LauncherFromRole when the @canopy-role
	// tag is malformed. Detector must NOT silently default to claude;
	// classification stays Unknown for all stable cases.
	d := NewDetector()
	content := "❯ Try \"something\"\n  ⏵⏵ auto mode on"
	d.Observe("%0", "", content)
	state, _ := d.Observe("%0", "", content)
	if state != StateUnknown {
		t.Errorf("empty launcher with claude content = %v, want StateUnknown (malformed role)", state)
	}
}

func TestDetector_EmptyContent_IsUnknown(t *testing.T) {
	d := NewDetector()
	state, _ := d.Observe("%0", "claude", "")
	if state != StateUnknown {
		t.Errorf("empty content = %v, want StateUnknown", state)
	}
}

func TestNormalize_StripsInputLine(t *testing.T) {
	// Lines starting with `❯` are claude's input prompt. Anything
	// after the chevron is what the user is typing. Stripping
	// these makes "user typing" not flip the activity hash.
	cases := []string{
		"❯ ",
		"❯ explain auth.ts",
		"❯ explain auth",
		"❯ ", // bare prompt
		"  ❯ leading whitespace",
	}
	for _, line := range cases {
		in := "stable response above\n" + line + "\nstable footer below"
		got := normalize(in)
		if strings.Contains(got, "❯") {
			t.Errorf("normalize() left input line %q in output: %q", line, got)
		}
		if !strings.Contains(got, "stable response above") || !strings.Contains(got, "stable footer below") {
			t.Errorf("normalize() ate non-input content for %q: %q", line, got)
		}
	}
}

func TestDetector_TypingDoesNotFlipToThinking(t *testing.T) {
	// THE LOAD-BEARING UX TEST. User typing into claude's input
	// prompt changes the pane content character-by-character.
	// Without input-line stripping in normalize, this would flip
	// the hash and the badge would say ⚡ Thinking when claude is
	// actually idle waiting for the user to finish composing.
	d := NewDetector()
	base := "● claude responded with this text\n────────────────\n"
	footer := "\n────────────────\n  ⏵⏵ auto mode on (shift+tab to cycle)"

	d.Observe("%0", "claude", base+"❯ "+footer)
	d.Observe("%0", "claude", base+"❯ h"+footer)
	d.Observe("%0", "claude", base+"❯ he"+footer)
	state, _ := d.Observe("%0", "claude", base+"❯ hel"+footer)
	if state == StateThinking {
		t.Errorf("user typing into prompt classified as %v, want NOT StateThinking (claude is idle, user is composing)", state)
	}
	if state != StateIdle {
		t.Errorf("user typing into prompt classified as %v, want StateIdle (claude is at input prompt)", state)
	}
}

func TestDetector_NormalizationStability_NotSticky(t *testing.T) {
	// Three observations with cosmetic-only differences (spinner
	// elapsed counter ticks). Should classify as stable Idle, NOT
	// sticky Thinking. This is the load-bearing test for normalize()
	// vs raw-hash classification.
	d := NewDetector()
	base := "❯ Try \"explain this\"\n  ⏵⏵ auto mode on (shift+tab to cycle)"
	d.Observe("%0", "claude", base+"\n✻ Baked for 1s")
	d.Observe("%0", "claude", base+"\n✻ Baked for 17s")
	state, _ := d.Observe("%0", "claude", base+"\n● Cooking for 4s")
	if state != StateIdle {
		t.Errorf("cosmetic-only changes classified as %v, want StateIdle (normalization should mask spinner)", state)
	}
}

func TestDetector_PerPaneIsolation(t *testing.T) {
	// Two panes with independent histories. Changing one must not
	// affect the other's classification.
	d := NewDetector()
	d.Observe("%0", "claude", "pane0 content")
	d.Observe("%1", "claude", "pane1 content")
	state, _ := d.Observe("%0", "claude", "pane0 content") // %0 stable
	if state != StateIdle {
		t.Errorf("pane %%0 stable but classified %v, want Idle", state)
	}
	state, _ = d.Observe("%1", "claude", "pane1 changed") // %1 changed
	if state != StateThinking {
		t.Errorf("pane %%1 changed but classified %v, want Thinking", state)
	}
}

func TestDetector_Prune_DropsMissingPaneIDs(t *testing.T) {
	d := NewDetector()
	d.Observe("%0", "claude", "a")
	d.Observe("%1", "claude", "b")
	d.Observe("%2", "claude", "c")
	if d.HistoryLen() != 3 {
		t.Fatalf("setup: HistoryLen = %d, want 3", d.HistoryLen())
	}

	// Only %1 is alive. %0 and %2 should be pruned.
	d.Prune(map[string]struct{}{"%1": {}})
	if d.HistoryLen() != 1 {
		t.Errorf("after Prune: HistoryLen = %d, want 1", d.HistoryLen())
	}

	// Confirm %1 still has its history (still classifiable).
	state, _ := d.Observe("%1", "claude", "b")
	if state != StateIdle {
		t.Errorf("pruned-out-of-band pane %%1 stable = %v, want Idle", state)
	}
}

func TestDetector_Prune_EmptyActiveSet_DropsAll(t *testing.T) {
	d := NewDetector()
	d.Observe("%0", "claude", "a")
	d.Observe("%1", "claude", "b")
	d.Prune(map[string]struct{}{})
	if d.HistoryLen() != 0 {
		t.Errorf("after Prune(empty): HistoryLen = %d, want 0", d.HistoryLen())
	}
}

func TestDetector_HistoryRingBuffer_Bounded(t *testing.T) {
	// 10 distinct observations on the same pane should leave only
	// historyLen (3) entries in the ring.
	d := NewDetector()
	for i := 0; i < 10; i++ {
		d.Observe("%0", "claude", string(rune('a'+i)))
	}
	d.mu.Lock()
	got := len(d.history["%0"])
	d.mu.Unlock()
	if got != historyLen {
		t.Errorf("history slice for %%0 = %d entries, want %d", got, historyLen)
	}
}

func TestIsClaudeRendering_PositiveMarkers(t *testing.T) {
	cases := map[string]string{
		"placeholder hint":       "❯ Try \"explain auth.ts\"",
		"auto-mode footer":       "  ⏵⏵ auto mode on (shift+tab to cycle)",
		"shift-tab hint":         "Press shift+tab to cycle modes",
		"tips banner":            "Tips for getting started\nRun /init to create a project file",
		"welcome banner":         "Welcome back Avinash!",
		"version line":           "Claude Code v2.1.133",
		"composite welcome chrome": "Welcome back Avi\nClaude Code v2.0.0\nTips for getting started",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if !IsClaudeRendering(content) {
				t.Errorf("expected IsClaudeRendering = true for %s, got false", name)
			}
		})
	}
}

func TestIsClaudeRendering_RejectsStaleScrollback(t *testing.T) {
	// THE LOAD-BEARING SECURITY TEST. If claude crashed, the welcome
	// banner + tips can persist in tmux scrollback while the LIVE
	// cursor area is now a keepAlive shell prompt. The check must
	// look at the bottom of the pane, not anywhere on screen.
	stale := strings.Join([]string{
		"Welcome back Avinash!", // claude marker — but stale at top
		"Claude Code v2.1.133",  // claude marker — but stale at top
		"Tips for getting started",
		"",
		"... lots of scrolled output ...",
		"... more scrolled output ...",
		"... claude crashed somewhere here ...",
		"keepAlive: agent process exited",
		"$ ",
		"$ ls",
		"file1.txt  file2.txt",
		"$ ", // shell prompt at bottom — the live area
		"",
		"",
		"",
	}, "\n")
	if IsClaudeRendering(stale) {
		t.Error("IsClaudeRendering with stale claude scrollback + shell at bottom = true; want false (must check bottom, not anywhere)")
	}
}

func TestIsClaudeRendering_RejectsShellMimicry(t *testing.T) {
	// The whole point of v3-B1: a shell that prints `❯ ` (starship,
	// oh-my-posh) must NOT pass IsClaudeRendering. Bare chevron is
	// excluded from claudeIdleMarkers for exactly this reason.
	shellOutputs := []string{
		"❯ ",                                  // bare starship prompt
		"❯ ls\nfile1.txt  file2.txt\n❯ ",      // shell session
		"~/canopy ❯ git status",               // starship with cwd
		"avi@host ❯ pwd\n/home/avi\n❯ ",       // shell prompt with output
		"$ ls",                                // bash prompt
		"% ls",                                // zsh prompt
		"keepAlive: agent process exited\n❯ ", // canopy keepAlive shell hint
	}
	for _, content := range shellOutputs {
		if IsClaudeRendering(content) {
			t.Errorf("IsClaudeRendering(%q) = true, want false (shell content must not pass)", content)
		}
	}
}

func TestIsClaudeRendering_EmptyContent(t *testing.T) {
	if IsClaudeRendering("") {
		t.Error("IsClaudeRendering(\"\") = true, want false")
	}
}

func TestIsTrustDialog(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"trust selector", " ❯ 1. Yes, I trust this folder\n   2. No, exit\n Enter to confirm · Esc to cancel", true},
		{"trust phrase alone", "Yes, I trust this folder", true},
		{"selector phrase alone", "Enter to confirm · Esc to cancel", true},
		{"normal prompt", "❯ Try \"something\"\n⏵⏵ auto mode on", false},
		{"shell", "❯ ls", false},
		{"empty", "", false},
		{"awaiting tool permission (NOT trust)", "Continue (y/N)", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTrustDialog(c.content); got != c.want {
				t.Errorf("IsTrustDialog(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestClassifyTwoShot covers the v0.19 remote-status-observability
// stateless motion classifier. Two pane captures taken a short interval
// apart are diffed via normalize() — same definition of "motion" the
// Detector uses for local rows, so badges match across local/remote.
func TestClassifyTwoShot(t *testing.T) {
	idleClaude := "❯ Try \"explain auth.ts\"\n  ⏵⏵ auto mode on (shift+tab to cycle)"
	awaitingClaude := "Allow tool use of Bash? (y/N)"

	cases := []struct {
		name     string
		launcher string
		prev     string
		cur      string
		want     State
	}{
		{"empty prev", "claude", "", idleClaude, StateUnknown},
		{"empty cur", "claude", idleClaude, "", StateUnknown},
		{"empty launcher", "", "a", "b", StateUnknown},
		{"non-claude launcher with motion", "aider", "first", "second", StateUnknown},
		{"non-claude launcher stable", "codex", "stable", "stable", StateUnknown},
		{"claude motion → Thinking", "claude", "first response chunk", "first response chunk plus more", StateThinking},
		{
			"claude cosmetic-only motion → NOT Thinking (normalize masks spinner)",
			"claude",
			idleClaude + "\n✻ Baked for 1s",
			idleClaude + "\n✻ Baked for 17s",
			StateIdle,
		},
		{
			"claude stable + awaiting pattern → AwaitingInput",
			"claude",
			awaitingClaude,
			awaitingClaude,
			StateAwaitingInput,
		},
		{
			"claude stable + idle marker → Idle",
			"claude",
			idleClaude,
			idleClaude,
			StateIdle,
		},
		{
			"claude stable + no marker → Unknown",
			"claude",
			"opaque text with no markers",
			"opaque text with no markers",
			StateUnknown,
		},
		{
			"awaiting beats idle when stable + both match (matches Observe order)",
			"claude",
			"Welcome back\nAllow tool use? (y/N)",
			"Welcome back\nAllow tool use? (y/N)",
			StateAwaitingInput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyTwoShot(tc.launcher, tc.prev, tc.cur); got != tc.want {
				t.Errorf("ClassifyTwoShot(%q, prev=%q, cur=%q) = %v; want %v",
					tc.launcher, tc.prev, tc.cur, got, tc.want)
			}
		})
	}
}

// TestClassifyOneShot covers the v0.17 Phase 1d.2 single-shot
// classifier used by `canopy ls --json` to stamp each workspace's
// agent_state without diff/history. AwaitingInput pattern matches
// take precedence over Idle markers (matches Observe's order).
func TestClassifyOneShot(t *testing.T) {
	cases := []struct {
		name     string
		launcher string
		content  string
		want     State
	}{
		{"empty content", "claude", "", StateUnknown},
		{"empty launcher", "", "anything", StateUnknown},
		{"non-claude launcher", "aider", "Welcome back to aider", StateUnknown},
		{"claude idle marker", "claude", "Welcome back\n❯ Try \"...\"", StateIdle},
		{"claude awaiting (y/N)", "claude", "Allow tool use? (y/N)", StateAwaitingInput},
		{"claude awaiting selector", "claude", "Enter to confirm · Esc to cancel", StateAwaitingInput},
		{"claude no markers", "claude", "just some text", StateUnknown},
		{
			"awaiting beats idle when both match",
			"claude",
			"Welcome back\nAllow tool use? (y/N)",
			StateAwaitingInput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyOneShot(tc.launcher, tc.content); got != tc.want {
				t.Errorf("ClassifyOneShot(%q, %q) = %v; want %v", tc.launcher, tc.content, got, tc.want)
			}
		})
	}
}
