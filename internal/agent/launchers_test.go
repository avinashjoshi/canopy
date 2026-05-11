package agent

import (
	"errors"
	"testing"
)

// TestResolve_KnownAgents covers the four shipped agents. Each must
// resolve cleanly and return a Launcher with non-empty Cmd.
func TestResolve_KnownAgents(t *testing.T) {
	cases := []string{"claude", "codex", "opencode", "aider"}
	for _, agent := range cases {
		t.Run(agent, func(t *testing.T) {
			l, err := Resolve(agent)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", agent, err)
			}
			if l.Cmd == "" {
				t.Errorf("Resolve(%q).Cmd is empty", agent)
			}
		})
	}
}

// TestResolve_EmptyDefaultsToClaude: backwards-compat for existing
// canopy.json files without an agent block.
func TestResolve_EmptyDefaultsToClaude(t *testing.T) {
	l, err := Resolve("")
	if err != nil {
		t.Fatalf("Resolve empty: %v", err)
	}
	if l.Cmd != "claude" {
		t.Errorf("Resolve empty .Cmd = %q; want claude", l.Cmd)
	}
}

// TestResolve_UnknownReturnsError: user typo or unsupported agent gets
// a clean ErrUnknownAgent + the known-types list in the message.
func TestResolve_UnknownReturnsError(t *testing.T) {
	_, err := Resolve("gpt-pilot")
	if !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("Resolve unknown: got %v; want errors.Is(... ErrUnknownAgent)", err)
	}
	msg := err.Error()
	for _, want := range []string{"gpt-pilot", "claude", "codex", "opencode", "aider"} {
		if !contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}
}

func TestRoleForType(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty defaults to claude", "", "agent:claude"},
		{"claude", "claude", "agent:claude"},
		{"codex", "codex", "agent:codex"},
		{"opencode", "opencode", "agent:opencode"},
		{"unknown launcher echoes back", "future-agent", "agent:future-agent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoleForType(tc.in); got != tc.want {
				t.Errorf("RoleForType(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestKnownAgents_SortedAndComplete: returns all four in a stable order.
// The deterministic order matters for error messages and CLI help text.
func TestKnownAgents(t *testing.T) {
	got := KnownAgents()
	want := []string{"aider", "claude", "codex", "opencode"}
	if len(got) != len(want) {
		t.Fatalf("KnownAgents len = %d; want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("KnownAgents[%d] = %q; want %q", i, got[i], name)
		}
	}
}

// TestBuildArgv_ClaudeFresh: the canonical case. Verifies the
// {{briefing}} token gets replaced inline and the result is a valid
// argv: [claude, --append-system-prompt, <text>].
func TestBuildArgv_ClaudeFresh(t *testing.T) {
	l, _ := Resolve("claude")
	got := l.BuildArgv(false, "BRIEFING-TEXT")
	want := []string{"claude", "--append-system-prompt", "BRIEFING-TEXT"}
	if !equalSlice(got, want) {
		t.Errorf("BuildArgv claude fresh = %v; want %v", got, want)
	}
}

// TestBuildArgv_ClaudeResume: claude with --continue prefix.
func TestBuildArgv_ClaudeResume(t *testing.T) {
	l, _ := Resolve("claude")
	got := l.BuildArgv(true, "BRIEFING-TEXT")
	want := []string{"claude", "--continue", "--append-system-prompt", "BRIEFING-TEXT"}
	if !equalSlice(got, want) {
		t.Errorf("BuildArgv claude resume = %v; want %v", got, want)
	}
}

// TestBuildArgv_ClaudeResumeNoBriefing: hybrid briefing strategy says
// "resume + no active hints = skip --append-system-prompt entirely."
// BuildArgv with empty briefing must drop BOTH the {{briefing}} token
// AND the preceding flag, leaving just `claude --continue`.
func TestBuildArgv_ClaudeResumeNoBriefing(t *testing.T) {
	l, _ := Resolve("claude")
	got := l.BuildArgv(true, "")
	want := []string{"claude", "--continue"}
	if !equalSlice(got, want) {
		t.Errorf("BuildArgv claude resume + empty briefing = %v; want %v", got, want)
	}
}

// TestBuildArgv_ClaudeFreshNoBriefing: edge case for fresh-with-no-briefing.
// Drops --append-system-prompt entirely. Practically this is unusual
// (fresh launches always have at least the conventions briefing) but
// the BuildArgv logic must handle it cleanly.
func TestBuildArgv_ClaudeFreshNoBriefing(t *testing.T) {
	l, _ := Resolve("claude")
	got := l.BuildArgv(false, "")
	want := []string{"claude"}
	if !equalSlice(got, want) {
		t.Errorf("BuildArgv claude fresh + empty briefing = %v; want %v", got, want)
	}
}

// TestBuildArgv_OpencodeMode: opencode uses BriefingAgentsMd, so
// BuildArgv has no {{briefing}} token to substitute. Verify both fresh
// and resume return just [opencode].
func TestBuildArgv_OpencodeMode(t *testing.T) {
	l, _ := Resolve("opencode")
	if l.BriefingMode != BriefingAgentsMd {
		t.Errorf("opencode BriefingMode = %v; want BriefingAgentsMd", l.BriefingMode)
	}
	for _, resume := range []bool{false, true} {
		got := l.BuildArgv(resume, "/tmp/foo.md")
		want := []string{"opencode"}
		if !equalSlice(got, want) {
			t.Errorf("BuildArgv opencode resume=%v = %v; want %v", resume, got, want)
		}
	}
}

// TestBuildArgv_AiderMode: aider uses BriefingFile (--message-file <path>).
// Verify the path gets interpolated in.
func TestBuildArgv_AiderMode(t *testing.T) {
	l, _ := Resolve("aider")
	if l.BriefingMode != BriefingFile {
		t.Errorf("aider BriefingMode = %v; want BriefingFile", l.BriefingMode)
	}
	got := l.BuildArgv(false, "/tmp/agent-briefing.md")
	want := []string{"aider", "--message-file", "/tmp/agent-briefing.md"}
	if !equalSlice(got, want) {
		t.Errorf("BuildArgv aider fresh = %v; want %v", got, want)
	}
}

// TestVerifyInstalled_NotFound: reaching for a definitely-not-installed
// binary returns a clean error with an install hint.
func TestVerifyInstalled_NotFound(t *testing.T) {
	l := Launcher{Cmd: "definitely-not-a-real-agent-cli-binary"}
	err := l.VerifyInstalled()
	if err == nil {
		t.Fatal("VerifyInstalled on missing binary returned nil; expected error")
	}
	if !contains(err.Error(), "definitely-not-a-real-agent-cli-binary") {
		t.Errorf("error missing binary name: %s", err)
	}
}

// TestPlanLaunch_ClaudeFresh: fresh launch with briefing inlines via
// $(cat <path>) shell substitution.
func TestPlanLaunch_ClaudeFresh(t *testing.T) {
	l, _ := Resolve("claude")
	plan := l.PlanLaunch("/tmp/briefing.md", false, "/tmp/worktree")
	want := `claude --append-system-prompt "$(cat /tmp/briefing.md)"`
	if plan.ShellCommand != want {
		t.Errorf("ShellCommand = %q\nwant %q", plan.ShellCommand, want)
	}
	if plan.PreRun != "" {
		t.Errorf("claude PreRun should be empty; got %q", plan.PreRun)
	}
}

// TestPlanLaunch_ClaudeResume: resume with briefing has --continue prefix.
func TestPlanLaunch_ClaudeResume(t *testing.T) {
	l, _ := Resolve("claude")
	plan := l.PlanLaunch("/tmp/briefing.md", true, "/tmp/worktree")
	want := `claude --continue --append-system-prompt "$(cat /tmp/briefing.md)"`
	if plan.ShellCommand != want {
		t.Errorf("ShellCommand = %q\nwant %q", plan.ShellCommand, want)
	}
}

// TestPlanLaunch_ClaudeResumeNoBriefing: resume with empty path drops the
// --append-system-prompt flag entirely. This is the "hybrid strategy:
// resume + no active hints = no briefing flag" case.
func TestPlanLaunch_ClaudeResumeNoBriefing(t *testing.T) {
	l, _ := Resolve("claude")
	plan := l.PlanLaunch("", true, "/tmp/worktree")
	want := `claude --continue`
	if plan.ShellCommand != want {
		t.Errorf("ShellCommand = %q\nwant %q", plan.ShellCommand, want)
	}
}

// TestPlanLaunch_AiderFile: aider uses --message-file with the path
// directly (no $(cat) — aider reads the file itself).
func TestPlanLaunch_AiderFile(t *testing.T) {
	l, _ := Resolve("aider")
	plan := l.PlanLaunch("/tmp/briefing.md", false, "/tmp/worktree")
	want := `aider --message-file /tmp/briefing.md`
	if plan.ShellCommand != want {
		t.Errorf("ShellCommand = %q\nwant %q", plan.ShellCommand, want)
	}
}

// TestPlanLaunch_OpencodeAgentsMd: opencode pre-copies the briefing into
// the worktree's AGENTS.md (PreRun) and then just invokes opencode.
func TestPlanLaunch_OpencodeAgentsMd(t *testing.T) {
	l, _ := Resolve("opencode")
	plan := l.PlanLaunch("/tmp/briefing.md", false, "/tmp/worktree")
	if plan.ShellCommand != "opencode" {
		t.Errorf("ShellCommand = %q; want opencode", plan.ShellCommand)
	}
	wantPreRun := "cp /tmp/briefing.md /tmp/worktree/AGENTS.md"
	if plan.PreRun != wantPreRun {
		t.Errorf("PreRun = %q\nwant %q", plan.PreRun, wantPreRun)
	}
}

// TestPlanLaunch_OpencodeNoBriefing: empty briefing path → no PreRun
// (don't overwrite an existing AGENTS.md if there's nothing to write).
func TestPlanLaunch_OpencodeNoBriefing(t *testing.T) {
	l, _ := Resolve("opencode")
	plan := l.PlanLaunch("", true, "/tmp/worktree")
	if plan.ShellCommand != "opencode" {
		t.Errorf("ShellCommand = %q; want opencode", plan.ShellCommand)
	}
	if plan.PreRun != "" {
		t.Errorf("opencode PreRun should be empty when briefingPath is empty; got %q", plan.PreRun)
	}
}

// TestPlanLaunch_PathWithSpaces: paths containing spaces / shell-special
// characters get single-quoted properly.
func TestPlanLaunch_PathWithSpaces(t *testing.T) {
	l, _ := Resolve("claude")
	plan := l.PlanLaunch("/tmp/with spaces/briefing.md", false, "")
	// Path should appear single-quoted.
	if !contains(plan.ShellCommand, "'/tmp/with spaces/briefing.md'") {
		t.Errorf("path with spaces not single-quoted: %s", plan.ShellCommand)
	}
}

// TestShellQuote_BasicCases: spot-check shellQuote for the typical inputs.
func TestShellQuote_BasicCases(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"claude", "claude"},
		{"/tmp/foo.md", "/tmp/foo.md"},
		{"with spaces", "'with spaces'"},
		{"has'quote", `'has'\''quote'`},
		{"", "''"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// equalSlice compares two []string by value. testing.T doesn't ship
// with reflect.DeepEqual-shaped helpers and pulling in reflect for one
// comparison is heavy; manual loop keeps the test file self-contained.
func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// contains is a tiny substring helper. Same shape as the one in
// state_test.go; not exported so we don't worry about cross-package
// duplication.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
