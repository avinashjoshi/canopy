package projectlist

import (
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/state"
)

// stripStyle removes ANSI escapes so tests can compare visible glyphs
// without color noise. Reuses the same pattern as the renderer's
// stripAnsi helper.
func stripStyle(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEsc {
			if c == 'm' {
				inEsc = false
			}
			continue
		}
		if c == 0x1b {
			inEsc = true
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func TestAgentBadge_PerState(t *testing.T) {
	cases := []struct {
		name      string
		state     agent.State
		wantGlyph string
	}{
		{"awaiting", agent.StateAwaitingInput, "✋"},
		{"thinking", agent.StateThinking, "⚡"},
		{"idle", agent.StateIdle, "💤"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := state.GlobalRow{
				Alive:       true,
				IsMain:      false,
				TmuxSession: "canopy/test-ws",
			}
			states := map[string]agent.State{"canopy/test-ws": c.state}
			got := stripStyle(agentBadge(r, states, true))
			if !strings.Contains(got, c.wantGlyph) {
				t.Errorf("agentBadge(%v) = %q, want contains %q", c.state, got, c.wantGlyph)
			}
		})
	}
}

func TestAgentBadge_UnknownState_BlankSlot(t *testing.T) {
	r := state.GlobalRow{Alive: true, TmuxSession: "canopy/x"}
	states := map[string]agent.State{"canopy/x": agent.StateUnknown}
	got := agentBadge(r, states, true)
	if strings.ContainsAny(got, "✋⚡💤") {
		t.Errorf("StateUnknown = %q, want blank slot (no glyph)", got)
	}
	if got != "  " {
		t.Errorf("StateUnknown padding = %q, want two spaces", got)
	}
}

func TestAgentBadge_MissingFromMap_BlankSlot(t *testing.T) {
	// Workspace exists but agent-state map has no entry yet (pre-first-tick).
	r := state.GlobalRow{Alive: true, TmuxSession: "canopy/never-polled"}
	got := agentBadge(r, map[string]agent.State{}, false)
	if got != "  " {
		t.Errorf("missing-key = %q, want two spaces", got)
	}
}

func TestAgentBadge_NilMap_BeforePoll_BlankSlot(t *testing.T) {
	// Before first poll lands (polled=false), nil map → blank.
	// Don't panic on map lookup with nil map.
	r := state.GlobalRow{Alive: true, TmuxSession: "canopy/x"}
	got := agentBadge(r, nil, false)
	if got != "  " {
		t.Errorf("nil map pre-poll = %q, want two spaces", got)
	}
}

func TestAgentBadge_AfterPoll_NoEntry_RendersNoAI(t *testing.T) {
	// After first poll lands (polled=true) but the row's session
	// isn't in the map → workspace has no agent pane → No-AI badge.
	r := state.GlobalRow{Alive: true, TmuxSession: "canopy/shell-only"}
	got := stripStyle(agentBadge(r, map[string]agent.State{}, true))
	if !strings.Contains(got, "·") {
		t.Errorf("alive row missing-from-map after poll = %q, want No-AI badge (·)", got)
	}
	if strings.ContainsAny(got, "✋⚡💤") {
		t.Errorf("No-AI rendering accidentally got an active glyph: %q", got)
	}
}

func TestAgentBadge_BeforePoll_AliveButNotInMap_BlankSlot(t *testing.T) {
	// Before first poll lands (polled=false), absent-from-map rows
	// stay blank — we don't know yet whether they have an agent pane.
	r := state.GlobalRow{Alive: true, TmuxSession: "canopy/maybe-has-agent"}
	got := agentBadge(r, map[string]agent.State{}, false)
	if got != "  " {
		t.Errorf("alive row pre-poll = %q, want two spaces (don't infer No-AI yet)", got)
	}
}

func TestAgentBadge_DeadWorkspace_NoBadge(t *testing.T) {
	// Stopped workspace shouldn't show a badge — there's no agent to
	// poll. Even if the map has a stale entry from before kill, the
	// renderer ignores it because Alive=false.
	r := state.GlobalRow{Alive: false, TmuxSession: "canopy/x"}
	states := map[string]agent.State{"canopy/x": agent.StateThinking}
	got := agentBadge(r, states, true)
	if strings.ContainsAny(got, "✋⚡💤") {
		t.Errorf("dead workspace got badge = %q, want blank", got)
	}
}

func TestAgentBadge_MainRow_NoBadge(t *testing.T) {
	// Main rows have no agent pane. Never render a badge regardless
	// of map contents.
	r := state.GlobalRow{IsMain: true, Alive: true, TmuxSession: "canopy/main"}
	states := map[string]agent.State{"canopy/main": agent.StateThinking}
	got := agentBadge(r, states, true)
	if strings.ContainsAny(got, "✋⚡💤") {
		t.Errorf("main row got badge = %q, want blank", got)
	}
}

func TestAgentBadge_EmptySession_NoBadge(t *testing.T) {
	// Edge: row with no TmuxSession (shouldn't happen in practice but
	// the renderer must not panic on map lookup with empty key).
	r := state.GlobalRow{Alive: true, TmuxSession: ""}
	states := map[string]agent.State{"": agent.StateThinking}
	got := agentBadge(r, states, true)
	if got != "  " {
		t.Errorf("empty TmuxSession = %q, want blank", got)
	}
}

func TestAgentBadge_KeyedBySession_NotByName(t *testing.T) {
	// Codex review v3 H10: workspace names can collide across projects
	// in Global tab; the badge map MUST key by tmux session (which is
	// project-prefixed) not by workspace name.
	//
	// Two workspaces with the same Name "fix-bug" but different
	// Project / TmuxSession resolve to independent badges.
	rA := state.GlobalRow{Alive: true, Name: "fix-bug", TmuxSession: "canopy/fix-bug"}
	rB := state.GlobalRow{Alive: true, Name: "fix-bug", TmuxSession: "other-proj/fix-bug"}
	states := map[string]agent.State{
		"canopy/fix-bug":     agent.StateThinking,
		"other-proj/fix-bug": agent.StateAwaitingInput,
	}
	gotA := stripStyle(agentBadge(rA, states, true))
	gotB := stripStyle(agentBadge(rB, states, true))
	if !strings.Contains(gotA, "⚡") {
		t.Errorf("rowA = %q, want Thinking (⚡)", gotA)
	}
	if !strings.Contains(gotB, "✋") {
		t.Errorf("rowB = %q, want AwaitingInput (✋)", gotB)
	}
}
