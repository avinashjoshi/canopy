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

// TestResolveExec_RegisteredLaunchers covers the v0.22 ExecMode wiring.
// Each launcher with an Exec entry must return its mapped Args +
// PromptMode without error. opencode (no Exec mode known as of 2026-06)
// returns ErrLauncherNoExec — that's the contract `canopy ask` uses
// to surface "this agent doesn't support quick-query yet."
func TestResolveExec_RegisteredLaunchers(t *testing.T) {
	cases := []struct {
		launcher   string
		wantArgs   []string
		wantMode   PromptMode
		wantErrSnt error // when non-nil, expect errors.Is(err, snt)
	}{
		{"claude", []string{"-p"}, PromptArg, nil},
		{"codex", []string{"exec"}, PromptArg, nil},
		{"aider", []string{"--message"}, PromptArg, nil},
		{"opencode", nil, 0, ErrLauncherNoExec},
	}
	for _, tc := range cases {
		t.Run(tc.launcher, func(t *testing.T) {
			l, err := Resolve(tc.launcher)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.launcher, err)
			}
			exec, err := l.ResolveExec()
			if tc.wantErrSnt != nil {
				if !errors.Is(err, tc.wantErrSnt) {
					t.Fatalf("ResolveExec err = %v; want errors.Is(..., %v)", err, tc.wantErrSnt)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveExec: %v", err)
			}
			if !equalSlice(exec.Args, tc.wantArgs) {
				t.Errorf("Args = %v; want %v", exec.Args, tc.wantArgs)
			}
			if exec.PromptMode != tc.wantMode {
				t.Errorf("PromptMode = %v; want %v", exec.PromptMode, tc.wantMode)
			}
		})
	}
}

// TestResolveExec_CodexOmitsApprovalFlag pins the design decision: the
// codex Exec.Args must NOT include --ask-for-approval. The interactive
// pane uses on-request mode (so canopy can detect awaiting-input), but
// exec mode is non-interactive and the approval flag would either be
// ignored or hang waiting for a UI that doesn't exist. Pin the absence
// so a future refactor that "consolidates" codex args doesn't
// reintroduce it.
func TestResolveExec_CodexOmitsApprovalFlag(t *testing.T) {
	l, _ := Resolve("codex")
	exec, err := l.ResolveExec()
	if err != nil {
		t.Fatalf("ResolveExec(codex): %v", err)
	}
	for _, a := range exec.Args {
		if a == "--ask-for-approval" {
			t.Errorf("codex Exec.Args contains --ask-for-approval (%v); exec mode must omit it", exec.Args)
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

// TestBuildArgv_CodexCarriesApprovalFlag: codex's argv must include
// --ask-for-approval on-request in both Fresh and Resume, followed by
// the briefing as POSITIONAL prompt. Resume additionally prefixes
// `resume --last` (codex's continue-most-recent verb, equivalent to
// `claude --continue`). Without the approval flag, codex auto-applies
// edits and the AwaitingMarkers classifier never fires.
//
// As of codex-cli 0.142.2 (2026-06-25), codex removed --instructions
// entirely and added `codex resume --last`. Last verified 2026-06-25.
func TestBuildArgv_CodexCarriesApprovalFlag(t *testing.T) {
	l, _ := Resolve("codex")
	for _, tc := range []struct {
		name   string
		resume bool
		want   []string
	}{
		{"fresh", false, []string{"codex", "--ask-for-approval", "on-request", "BRIEFING-TEXT"}},
		{"resume", true, []string{"codex", "resume", "--last", "--ask-for-approval", "on-request", "BRIEFING-TEXT"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := l.BuildArgv(tc.resume, "BRIEFING-TEXT")
			if !equalSlice(got, tc.want) {
				t.Errorf("BuildArgv codex %s = %v; want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestBuildArgv_CodexResumeEmptyBriefing pins the post-0.142.2 Resume
// argv when no briefing is set (the "Resume + no hints" hybrid case):
// `codex resume --last --ask-for-approval on-request` — no positional
// PROMPT, so codex opens to its interactive shell on the resumed
// session. Without this assertion, a regression that dropped --last
// or ate the approval flag would slip through.
func TestBuildArgv_CodexResumeEmptyBriefing(t *testing.T) {
	l, _ := Resolve("codex")
	got := l.BuildArgv(true, "")
	want := []string{"codex", "resume", "--last", "--ask-for-approval", "on-request"}
	if !equalSlice(got, want) {
		t.Errorf("BuildArgv codex resume empty briefing = %v; want %v", got, want)
	}
}

// TestBuildArgv_CodexFreshEmptyBriefing: Fresh path with no briefing
// drops the positional [PROMPT] (last arg), leaving the approval flag
// + value intact. The Resume equivalent is covered by
// TestBuildArgv_CodexResumeEmptyBriefing (different argv shape since
// Resume prefixes `resume --last`).
func TestBuildArgv_CodexFreshEmptyBriefing(t *testing.T) {
	l, _ := Resolve("codex")
	got := l.BuildArgv(false, "")
	want := []string{"codex", "--ask-for-approval", "on-request"}
	if !equalSlice(got, want) {
		t.Errorf("BuildArgv codex fresh empty briefing = %v; want %v", got, want)
	}
}

// TestBuildArgv_EmptyBriefingOnlyPopsFlagPrefix is a regression-pin
// for the v0.22 BuildArgv fix: the strip-on-empty-briefing logic must
// pop the preceding arg ONLY if it looks like a flag (starts with `-`).
// Without this guard, codex's positional briefing (where the preceding
// arg is a flag VALUE like "on-request") gets the value eaten and the
// resulting argv is malformed.
//
// Built from a synthetic Launcher so we don't depend on any of the
// shipped agents' argv staying any particular shape over time.
func TestBuildArgv_EmptyBriefingOnlyPopsFlagPrefix(t *testing.T) {
	cases := []struct {
		name string
		tail []string
		want []string
	}{
		{
			name: "preceding is flag → strip both",
			tail: []string{"--instructions", "{{briefing}}"},
			want: []string{"FAKE"},
		},
		{
			name: "preceding is flag VALUE → keep it, drop only briefing",
			tail: []string{"--mode", "single", "{{briefing}}"},
			want: []string{"FAKE", "--mode", "single"},
		},
		{
			name: "preceding is plain positional → keep it, drop only briefing",
			tail: []string{"foo", "{{briefing}}"},
			want: []string{"FAKE", "foo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := Launcher{Cmd: "FAKE", Fresh: tc.tail, Resume: tc.tail}
			got := l.BuildArgv(false, "")
			if !equalSlice(got, tc.want) {
				t.Errorf("BuildArgv = %v; want %v", got, tc.want)
			}
		})
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

// TestPlanLaunch_CodexFresh: fresh codex launch with briefing inlines
// the cat shell substitution as the positional [PROMPT] argv.
// `--ask-for-approval on-request` is preserved as the leading flag pair.
func TestPlanLaunch_CodexFresh(t *testing.T) {
	l, _ := Resolve("codex")
	plan := l.PlanLaunch("/tmp/briefing.md", false, "/tmp/worktree")
	want := `codex --ask-for-approval on-request "$(cat /tmp/briefing.md)"`
	if plan.ShellCommand != want {
		t.Errorf("ShellCommand = %q\nwant %q", plan.ShellCommand, want)
	}
	if plan.PreRun != "" {
		t.Errorf("codex PreRun should be empty; got %q", plan.PreRun)
	}
}

// TestPlanLaunch_CodexResume: resumed codex with briefing produces
// `codex resume --last --ask-for-approval on-request "$(cat ...)"`.
func TestPlanLaunch_CodexResume(t *testing.T) {
	l, _ := Resolve("codex")
	plan := l.PlanLaunch("/tmp/briefing.md", true, "/tmp/worktree")
	want := `codex resume --last --ask-for-approval on-request "$(cat /tmp/briefing.md)"`
	if plan.ShellCommand != want {
		t.Errorf("ShellCommand = %q\nwant %q", plan.ShellCommand, want)
	}
}

// TestPlanLaunch_CodexResumeNoBriefing pins the bug codex review caught
// 2026-06-25 (P1 #2): when briefing is empty AND the {{briefing}} token
// is a positional argument preceded by a flag VALUE (not a flag NAME),
// PlanLaunch must NOT pop the preceding arg. Otherwise codex spawns with
// `codex resume --last --ask-for-approval` (no value) and fails to parse.
//
// Mirrors the BuildArgv regression test for the same shape.
func TestPlanLaunch_CodexResumeNoBriefing(t *testing.T) {
	l, _ := Resolve("codex")
	plan := l.PlanLaunch("", true, "/tmp/worktree")
	want := `codex resume --last --ask-for-approval on-request`
	if plan.ShellCommand != want {
		t.Errorf("ShellCommand = %q\nwant %q\n(if 'on-request' is missing, the strip-on-empty guard is broken)",
			plan.ShellCommand, want)
	}
}

// TestPlanLaunch_CodexFreshNoBriefing: same shape, fresh path.
func TestPlanLaunch_CodexFreshNoBriefing(t *testing.T) {
	l, _ := Resolve("codex")
	plan := l.PlanLaunch("", false, "/tmp/worktree")
	want := `codex --ask-for-approval on-request`
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
