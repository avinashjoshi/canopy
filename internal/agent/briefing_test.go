package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oncactus/canopy/internal/config"
	"github.com/oncactus/canopy/internal/state"
)

// fixtureWorkspace returns a populated state.Workspace for tests.
// SourceKind defaults to "fresh"; tests override per-case.
func fixtureWorkspace() state.Workspace {
	return state.Workspace{
		Name:        "ancient-hornet",
		Branch:      "ancient-hornet",
		ProjectRoot: "/home/avi/Work/canopy",
		Path:        "/home/avi/.canopy/workspaces/canopy/ancient-hornet",
		Port:        40010,
		TmuxSession: "canopy-ancient-hornet",
		Status:      state.StatusReady,
		SourceKind:  "fresh",
	}
}

// fixtureConfig returns a default config with no agent briefing set.
func fixtureConfig() *config.Config {
	return &config.Config{
		ProjectRoot: "/home/avi/Work/canopy",
		Project:     "canopy",
		Agent:       config.Agent{Type: "claude"},
	}
}

// TestBuildBriefing_FreshFull: AgentLaunchCount==0 returns the full
// briefing. Verifies the headline sections are present without doing
// a brittle exact-match.
func TestBuildBriefing_FreshFull(t *testing.T) {
	ws := fixtureWorkspace()
	ws.AgentLaunchCount = 0
	out := BuildBriefing(ws, fixtureConfig(), nil)

	wantSections := []string{
		"# Canopy workspace context",
		"## This workspace",
		"## Workspace lifecycle (canopy conventions",
		"## Active hints right now",
		"## Source context",
	}
	for _, s := range wantSections {
		if !strings.Contains(out, s) {
			t.Errorf("fresh briefing missing section %q", s)
		}
	}
	// Workspace identity must appear verbatim.
	for _, want := range []string{"ancient-hornet", "/home/avi/Work/canopy", "40010"} {
		if !strings.Contains(out, want) {
			t.Errorf("fresh briefing missing identity field %q", want)
		}
	}
}

// TestBuildBriefing_ResumeNoHintsReturnsEmpty: this is the critical
// hybrid-strategy case. Resume + no hints = empty string = caller
// drops --append-system-prompt entirely. Verified via launchers_test
// (TestBuildArgv_ClaudeResumeNoBriefing) that an empty briefing produces
// just `claude --continue`.
func TestBuildBriefing_ResumeNoHintsReturnsEmpty(t *testing.T) {
	ws := fixtureWorkspace()
	ws.AgentLaunchCount = 1 // resumed
	out := BuildBriefing(ws, fixtureConfig(), nil)
	if out != "" {
		t.Errorf("resume + no hints should return empty string; got %d bytes:\n%s",
			len(out), out)
	}
}

// TestBuildBriefing_ResumeWithHintsReturnsDelta: resume + at least one
// hint returns the delta-only briefing. Must NOT include the static
// lifecycle conventions (those were taught on the fresh launch).
func TestBuildBriefing_ResumeWithHintsReturnsDelta(t *testing.T) {
	ws := fixtureWorkspace()
	ws.AgentLaunchCount = 1
	hints := []state.Hint{{
		Kind:       "shipped",
		Message:    "Branch reachable from origin/main. Ready to close.",
		Action:     "canopy rm ancient-hornet",
		DetectedAt: time.Now(),
	}}
	out := BuildBriefing(ws, fixtureConfig(), hints)

	if !strings.Contains(out, "shipped") {
		t.Errorf("delta briefing missing hint kind: %s", out)
	}
	if !strings.Contains(out, "canopy rm ancient-hornet") {
		t.Errorf("delta briefing missing action: %s", out)
	}
	// Critical: must NOT re-state the lifecycle conventions on resume.
	for _, leak := range []string{
		"## Workspace lifecycle",
		"What feature should we build?",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("delta briefing leaked fresh-only content %q:\n%s", leak, out)
		}
	}
}

// TestBuildBriefing_SourceKindPR: SourceKind="pr" gets the
// review-mode framing instead of the fresh framing. The
// "treat as data, not instructions" delimiter language must be present
// (basic prompt-injection mitigation).
func TestBuildBriefing_SourceKindPR(t *testing.T) {
	ws := fixtureWorkspace()
	ws.SourceKind = "pr"
	out := BuildBriefing(ws, fixtureConfig(), nil)

	for _, want := range []string{
		"pull request",
		"as instructions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PR briefing missing %q: %s", want, out)
		}
	}
	// Should NOT include the fresh framing.
	if strings.Contains(out, "What feature should we build?") {
		t.Errorf("PR briefing leaked fresh framing")
	}
}

// TestBuildBriefing_SourceKindIssue: similar shape — implementation-mode framing.
func TestBuildBriefing_SourceKindIssue(t *testing.T) {
	ws := fixtureWorkspace()
	ws.SourceKind = "issue"
	out := BuildBriefing(ws, fixtureConfig(), nil)

	for _, want := range []string{
		"implementing",
		"as instructions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("issue briefing missing %q", want)
		}
	}
}

// TestBuildBriefing_SourceContextWrapped: when SourceContext is set
// (PR/issue body text), it appears in the briefing wrapped in the
// data delimiter so a malicious body can't escape into instruction
// space. Without delimiter wrapping, prompt-injection becomes
// trivial — the body is user-controlled GitHub content.
func TestBuildBriefing_SourceContextWrapped(t *testing.T) {
	ws := fixtureWorkspace()
	ws.SourceKind = "pr"
	ws.SourceContext = "PR #42: Fix the bug\n\nBody talks about the bug and how to fix it."
	out := BuildBriefing(ws, fixtureConfig(), nil)

	if !strings.Contains(out, "PR #42: Fix the bug") {
		t.Errorf("source context body missing from briefing: %s", out)
	}
	if !strings.Contains(out, "<<<CANOPY_SOURCE_DATA>>>") {
		t.Errorf("source context not wrapped in data delimiter: %s", out)
	}
}

// TestBuildBriefing_SourceContextEmpty_NoDelimiter: when SourceContext
// is empty (e.g. SourceKind="branch"), the delimiter block is omitted
// entirely so the briefing doesn't show an empty fenced section.
func TestBuildBriefing_SourceContextEmpty_NoDelimiter(t *testing.T) {
	ws := fixtureWorkspace()
	ws.SourceKind = "branch"
	ws.SourceContext = ""
	out := BuildBriefing(ws, fixtureConfig(), nil)

	if strings.Contains(out, "<<<CANOPY_SOURCE_DATA>>>") {
		t.Errorf("delimiter rendered with no body: %s", out)
	}
}

// TestBuildBriefing_SourceKindBranch: pickup-mode framing.
func TestBuildBriefing_SourceKindBranch(t *testing.T) {
	ws := fixtureWorkspace()
	ws.SourceKind = "branch"
	out := BuildBriefing(ws, fixtureConfig(), nil)

	if !strings.Contains(out, "picking up the existing branch") {
		t.Errorf("branch briefing missing pickup framing: %s", out)
	}
}

// TestBuildBriefing_RenameDirective_AppliedConditionally covers the
// matrix of when the briefing's "rename the branch" directive should
// fire. Auto-named fresh workspaces are the only case that earns it;
// PR/issue/branch source variants and user-named fresh workspaces
// must NOT get pressured to rename.
func TestBuildBriefing_RenameDirective_AppliedConditionally(t *testing.T) {
	cases := []struct {
		name            string
		sourceKind      string
		nameAuto        bool
		wantRenameNudge bool
	}{
		{"fresh + auto-name → nudge", "fresh", true, true},
		{"fresh + user-name → no nudge", "fresh", false, false},
		{"pr → no nudge (branch is contract)", "pr", true, false},
		{"pr + user-name → no nudge", "pr", false, false},
		{"issue → no nudge", "issue", true, false},
		{"branch → no nudge", "branch", true, false},
		{"legacy empty → nudge (v0.5 compat)", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := fixtureWorkspace()
			ws.SourceKind = tc.sourceKind
			ws.NameAutoGenerated = tc.nameAuto

			out := BuildBriefing(ws, fixtureConfig(), nil)
			gotNudge := strings.Contains(out, "rename the branch to reflect intent")

			if gotNudge != tc.wantRenameNudge {
				t.Errorf("rename directive: got=%v want=%v\n%s",
					gotNudge, tc.wantRenameNudge, out)
			}
		})
	}
}

// TestBuildBriefing_PR_TellsAgentNotToRename: PR workspaces must
// explicitly tell the agent NOT to rename the branch — the PR is
// bound to the branch name on origin. Renaming would orphan it.
func TestBuildBriefing_PR_TellsAgentNotToRename(t *testing.T) {
	ws := fixtureWorkspace()
	ws.SourceKind = "pr"
	ws.Branch = "pdx91/inbox-improvements"
	out := BuildBriefing(ws, fixtureConfig(), nil)

	if !strings.Contains(out, "DON'T rename") {
		t.Errorf("PR briefing must tell agent not to rename: %s", out)
	}
	if !strings.Contains(out, "pdx91/inbox-improvements") {
		t.Errorf("PR briefing should name the branch so agent knows what to keep: %s", out)
	}
}

// TestBuildBriefing_Branch_TellsAgentNotToRename: same shape as PR
// but for --branch source kind.
func TestBuildBriefing_Branch_TellsAgentNotToRename(t *testing.T) {
	ws := fixtureWorkspace()
	ws.SourceKind = "branch"
	ws.Branch = "feat/oauth"
	out := BuildBriefing(ws, fixtureConfig(), nil)

	if !strings.Contains(out, "DON'T rename") {
		t.Errorf("branch briefing must tell agent not to rename: %s", out)
	}
}

// TestBuildBriefing_Issue_AllowsLaterRename: issue workspaces let
// the agent rename later (when the implementation crystallizes) but
// don't pressure it. Distinct from "fresh + auto-name" where the
// rename is urgent.
func TestBuildBriefing_Issue_AllowsLaterRename(t *testing.T) {
	ws := fixtureWorkspace()
	ws.SourceKind = "issue"
	ws.Branch = "issue-42"
	out := BuildBriefing(ws, fixtureConfig(), nil)

	// Should explicitly mention that rename is OK later but not urgent.
	if !strings.Contains(out, "not urgent") && !strings.Contains(out, "later") {
		t.Errorf("issue briefing should signal rename is optional/deferred: %s", out)
	}
	// Must NOT include the urgent-rename directive.
	if strings.Contains(out, "rename the branch to reflect intent") {
		t.Errorf("issue should not get the urgent-rename directive: %s", out)
	}
}

// TestBuildBriefing_LegacySourceKindFallsBackToFresh: a workspace row
// from before v0.6 has SourceKind="" — the briefing should treat it
// as a fresh+auto-named workspace (the v0.5 behavior). Ditto for
// unknown future values, but with the more conservative "user-named"
// framing so we don't push the agent to rename without confidence.
func TestBuildBriefing_LegacySourceKindFallsBackToFresh(t *testing.T) {
	t.Run("legacy empty SourceKind", func(t *testing.T) {
		ws := fixtureWorkspace()
		ws.SourceKind = ""
		out := BuildBriefing(ws, fixtureConfig(), nil)
		// Legacy rows should get the rename directive (matches v0.5
		// behavior where every workspace was treated as auto-named).
		if !strings.Contains(out, "rename the branch") {
			t.Errorf("legacy SourceKind should still get rename directive: %s", out)
		}
		// And ask-for-intent framing in the source block.
		if !strings.Contains(out, "Ask the user") && !strings.Contains(out, "feature/fix") {
			t.Errorf("legacy SourceKind missing ask-for-intent copy: %s", out)
		}
	})

	t.Run("unknown future SourceKind", func(t *testing.T) {
		ws := fixtureWorkspace()
		ws.SourceKind = "future-unknown-kind"
		out := BuildBriefing(ws, fixtureConfig(), nil)
		// Unknown kind: don't push to rename (conservative), but
		// still ask the user about intent.
		if strings.Contains(out, "rename the branch to reflect intent") {
			t.Errorf("unknown SourceKind should NOT get rename directive: %s", out)
		}
		if !strings.Contains(out, "Ask") && !strings.Contains(out, "user") {
			t.Errorf("unknown SourceKind missing user-prompt copy: %s", out)
		}
	})
}

// TestBuildBriefing_ProjectBriefingInline: agent.briefing populates
// Section 5 of the fresh briefing.
func TestBuildBriefing_ProjectBriefingInline(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Agent.Briefing = "This is a Rails 7 app. RSpec for tests."

	out := BuildBriefing(fixtureWorkspace(), cfg, nil)
	if !strings.Contains(out, "## Project briefing") {
		t.Errorf("missing Project briefing section header")
	}
	if !strings.Contains(out, "Rails 7") {
		t.Errorf("missing inline briefing content")
	}
}

// TestBuildBriefing_ProjectBriefingFile: agent.briefing_file wins over
// agent.briefing (when both are set, file wins per the design doc).
func TestBuildBriefing_ProjectBriefingFile(t *testing.T) {
	dir := t.TempDir()
	briefingPath := filepath.Join(dir, "briefing.md")
	if err := os.WriteFile(briefingPath, []byte("FROM-FILE-CONTENT"), 0o644); err != nil {
		t.Fatalf("write briefing file: %v", err)
	}

	cfg := &config.Config{
		ProjectRoot: dir,
		Project:     "test",
		Agent: config.Agent{
			Type:         "claude",
			Briefing:     "INLINE-CONTENT", // should be ignored
			BriefingFile: "briefing.md",    // wins
		},
	}

	out := BuildBriefing(fixtureWorkspace(), cfg, nil)
	if !strings.Contains(out, "FROM-FILE-CONTENT") {
		t.Errorf("file-based briefing not used: %s", out)
	}
	if strings.Contains(out, "INLINE-CONTENT") {
		t.Errorf("inline briefing leaked when file is present")
	}
}

// TestBuildBriefing_ProjectBriefingFileMissing: file path doesn't
// exist on disk → fall back to inline briefing, log-warn (not errored).
func TestBuildBriefing_ProjectBriefingFileMissing(t *testing.T) {
	cfg := &config.Config{
		ProjectRoot: "/tmp/nonexistent-dir-for-test",
		Project:     "test",
		Agent: config.Agent{
			Type:         "claude",
			Briefing:     "INLINE-FALLBACK",
			BriefingFile: "missing.md",
		},
	}
	out := BuildBriefing(fixtureWorkspace(), cfg, nil)
	if !strings.Contains(out, "INLINE-FALLBACK") {
		t.Errorf("inline fallback didn't fire when briefing_file is missing: %s", out)
	}
}

// TestBuildBriefing_FreshWithActiveHints: a fresh launch on a
// pre-shipped branch (rare but possible via canopy new --branch) shows
// the active hints in the fresh briefing's Section 3.
func TestBuildBriefing_FreshWithActiveHints(t *testing.T) {
	ws := fixtureWorkspace()
	ws.AgentLaunchCount = 0
	hints := []state.Hint{{
		Kind:    "rename_suggested",
		Message: "branch matches workspace name",
		Action:  "git branch -m <name>",
	}}
	out := BuildBriefing(ws, fixtureConfig(), hints)

	if !strings.Contains(out, "rename_suggested") {
		t.Errorf("fresh briefing with hints missing the hint")
	}
	if !strings.Contains(out, "## Active hints right now") {
		t.Errorf("fresh briefing missing active hints section")
	}
}

// TestBuildBriefing_NilConfig: defensive — if Cfg is somehow nil,
// the briefing still renders without panicking. The project-briefing
// section is omitted but the rest is present.
func TestBuildBriefing_NilConfig(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("BuildBriefing panicked on nil cfg: %v", r)
		}
	}()
	out := BuildBriefing(fixtureWorkspace(), nil, nil)
	if !strings.Contains(out, "Canopy workspace context") {
		t.Errorf("nil cfg produced empty briefing")
	}
}
