// Agent swap orchestration. Implements the v0.22 `canopy agent swap
// <type>` verb: take a running workspace that currently has agent X in
// its tmux pane, kill that pane, persist the new agent type in state,
// and respawn the new agent in the same pane geometry.
//
// Why kill-pane (not kill-the-running-process-inside-the-pane): eng-
// review D5 picked clean-visual-state over preserved-scrollback. The
// old agent's per-directory conversation history survives via the
// agent's OWN resume mechanism (claude --continue, codex equivalent),
// not via tmux scrollback. The new pane shows ONLY the new agent's UI;
// no claude TUI artifacts leaking into codex's startup render.
//
// Why save+restore window-layout (not naive split-window with the same
// percentage): tmux's `split-window -l <N>%` takes N as percent of the
// TARGET PANE. After kill-pane the remaining panes redistribute, so a
// fresh 30% split off the IDE post-kill produces drifted geometry vs.
// the original 30% split. Capturing window_layout before kill and
// SelectLayout-ing after respawn restores byte-precise geometry.
//
// Atomicity story: if respawn fails after kill, the workspace is left
// without an agent pane. The persisted state.Workspace.CurrentAgent
// still reflects the user's intent (the new agent), and the next
// canopy switch <ws> resurrects the agent in the new shape. Eng-review
// Open Q #3 — acceptable v1 risk.
package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

// ErrSwapNoAgentPane is returned by SwapAgent when no pane in the
// workspace's tmux session is tagged with `agent:*`. Either the
// workspace's session is gone (in which case the user should run
// canopy switch to resurrect it first) or the agent pane was manually
// killed; either way the swap can't proceed without first restoring a
// pane to kill.
var ErrSwapNoAgentPane = errors.New("workspace.SwapAgent: no agent:* pane found in session")

// ErrSwapAlreadyCurrent is returned when the user asks to swap to the
// agent that's already running. Treating it as an error (rather than a
// no-op) catches typos and prevents silent unnecessary churn (a kill +
// respawn of the same agent would still lose tmux scrollback).
var ErrSwapAlreadyCurrent = errors.New("workspace.SwapAgent: workspace is already running this agent")

// SwapAgent replaces the workspace's running agent pane with a fresh
// pane of newType. Returns the updated *state.Workspace on success.
//
// Validates:
//   - newType is non-empty and in m.Cfg.Agents allowlist → else
//     agent.ErrAgentNotAllowed.
//   - newType != ws.CurrentAgent → else ErrSwapAlreadyCurrent.
//
// Sequence (see file-level comment for the why):
//
//   1. capture the session's window_layout
//   2. look up the current agent:* pane
//   3. tmux kill-pane on it
//   4. update state: ws.CurrentAgent = newType, save under WithLock
//   5. tmux split-window from the IDE pane, run the new agent's
//      command (resume mode = true; the launcher's Resume argv carries
//      claude --continue / codex equivalent so per-directory history
//      survives the swap from the AGENT's perspective)
//   6. tag the new pane with agent.RoleForType(newType)
//   7. select-layout to restore captured geometry byte-precise
//   8. select-pane onto the new pane so the user lands on it
//
// All log events have structured fields so downstream observability can
// correlate "swap attempted" with "swap completed" / "swap failed
// mid-flight".
func (m *Manager) SwapAgent(ctx context.Context, name, newType string) (*state.Workspace, error) {
	// Validate newType FIRST, before any tmux state change. Cheap gate
	// that fails fast on typos / disallowed types without leaving the
	// session in a half-modified state.
	if !m.Cfg.AllowsAgent(newType) {
		return nil, fmt.Errorf("%w: %q (allowed: %v)",
			agent.ErrAgentNotAllowed, newType, m.Cfg.Agents)
	}

	// Look up the workspace row + verify session exists.
	st, err := m.Store.Load()
	if err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: load state: %w", err)
	}
	ws, err := st.Find(m.Cfg.ProjectRoot, name)
	if err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent(%s): %w", name, ErrWorkspaceNotFound)
	}
	wsCopy := *ws // defensive copy so subsequent state.Load doesn't share

	oldType := currentAgent(&wsCopy, m.Cfg)
	if oldType == newType {
		return nil, fmt.Errorf("%w: %q (current = %q)",
			ErrSwapAlreadyCurrent, newType, oldType)
	}

	session := wsCopy.TmuxSessionName()
	if has, err := m.Tmux.HasSession(ctx, session); err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: HasSession: %w", err)
	} else if !has {
		return nil, fmt.Errorf("workspace.SwapAgent: session %q not running (try `canopy switch %s` first)", session, name)
	}

	// Step 1: capture window-layout before kill so respawn can restore
	// byte-precise geometry (kill-pane redistributes the remaining
	// panes, so a fresh percentage-based split drifts).
	layout, err := m.Tmux.CaptureWindowLayout(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: capture layout: %w", err)
	}

	// Step 2: verify the target launcher is installed on PATH BEFORE
	// any destructive tmux/state operations. Without this, an allowed-
	// in-canopy.json but not-installed-locally agent name (e.g., codex
	// on a host where codex isn't on PATH) would tear down the running
	// agent pane and only THEN fail when agentPaneCmd's Resolve →
	// VerifyInstalled tripped — leaving the session paneless and the
	// state mid-swap. (codex review P1 #3, 2026-06-25.)
	launcher, err := agent.Resolve(newType)
	if err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: resolve launcher %q: %w", newType, err)
	}
	if err := launcher.VerifyInstalled(); err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: %w", err)
	}

	// Step 3: find the current agent pane. There should be exactly one
	// in v0.22 (concurrent multi-agent is deferred); reject otherwise
	// so a future refactor doesn't silently mis-target.
	agentPanes, err := m.Tmux.LookupAllPanes(ctx, session, "agent:*")
	if err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: lookup agent pane: %w", err)
	}
	if len(agentPanes) == 0 {
		return nil, ErrSwapNoAgentPane
	}
	if len(agentPanes) > 1 {
		return nil, fmt.Errorf("workspace.SwapAgent: session %q has %d agent:* panes; expected 1", session, len(agentPanes))
	}
	oldAgentPaneID := agentPanes[0].ID

	// Step 4: find the IDE pane (we'll respawn off it). It's the most
	// stable anchor in canopy's 3-pane layout — never killed by
	// workspace lifecycle ops in the normal case.
	idePaneID, err := m.Tmux.LookupPane(ctx, session, "ide")
	if err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: lookup ide pane: %w", err)
	}

	log.Info("workspace.agent-swap.start",
		"session", session,
		"workspace", name,
		"from_agent", oldType,
		"to_agent", newType,
		"old_pane", oldAgentPaneID)

	// Step 4: kill the old agent pane.
	if err := m.Tmux.KillPane(ctx, oldAgentPaneID); err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: kill-pane %s: %w", oldAgentPaneID, err)
	}

	// Step 5: persist the new agent type. Done BEFORE the respawn so
	// agentPaneCmd (which reads ws.CurrentAgent via currentAgent) sees
	// the new value when we call it. Persisted under WithLock for the
	// usual race-safety against concurrent state mutations.
	//
	// Initialize AgentLaunches[newType] to 0 if absent. This is
	// load-bearing for the briefing path: BuildBriefing's
	// `launchCountFor` (briefing.go) returns the legacy total
	// AgentLaunchCount whenever AgentLaunches[agentType] is missing AND
	// agentType matches ws.CurrentAgent — a migration-window fallback
	// that breaks the first-swap case once we mutate CurrentAgent to
	// newType but haven't yet recorded that newType has zero prior
	// launches. Without the explicit zero, BuildBriefing returns ""
	// for the first swap and the new agent spawns with no workspace
	// context. (ultrareview bug_001, 2026-06-26.)
	var updated state.Workspace
	err = m.Store.WithLock(func(s *state.State) error {
		row, ferr := s.Find(m.Cfg.ProjectRoot, name)
		if ferr != nil {
			return ferr
		}
		row.CurrentAgent = newType
		if row.AgentLaunches == nil {
			row.AgentLaunches = map[string]int{}
		}
		if _, ok := row.AgentLaunches[newType]; !ok {
			row.AgentLaunches[newType] = 0
		}
		updated = *row
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: persist new agent: %w", err)
	}

	// Step 6: spawn the new agent. Resume vs Fresh decided by the
	// per-(workspace, agent) launch counter (v0.22+):
	//   - AgentLaunches[newType] == 0 → Fresh. The agent has never
	//     run in this workspace, so its --continue/--resume machinery
	//     would find no prior session and exit immediately ("No
	//     conversation found to continue"). Fresh dodges that.
	//   - AgentLaunches[newType] > 0 → Resume. The agent has prior
	//     history here from an earlier launch; the agent's own resume
	//     argv (claude --continue, etc.) reaches it.
	// This makes "swap claude → codex → claude" work the way the
	// design promised in D5: swap-back auto-resumes the original
	// conversation.
	resume := updated.AgentLaunches[newType] > 0
	newAgentCmd, err := m.agentPaneCmd(&updated, resume)
	if err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: build new agent cmd: %w", err)
	}
	// The split direction + size match buildSession's original. After
	// SelectLayout restores byte-precise geometry, the size argument
	// here only matters for the brief window between split and select.
	//
	// SelectPane(idePaneID) first so the upcoming SplitPane (which
	// targets the SESSION'S ACTIVE pane via `tmux split-window -t
	// <session>`) splits off the IDE pane specifically. Without this,
	// a swap invoked while the user's focus was on the shell pane
	// would split the new agent off the shell, producing a wrong
	// layout that SelectLayout's geometry restore can't fix on its
	// own. (codex review P1 #4, 2026-06-25.)
	if err := m.Tmux.SelectPane(ctx, idePaneID); err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: select ide pane before split: %w", err)
	}
	newAgentPane, err := m.Tmux.SplitPane(ctx, session, updated.Path, keepAlive(newAgentCmd), tmux.SplitHorizontal, 30)
	if err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: split new agent pane: %w", err)
	}
	if err := m.Tmux.SetRole(ctx, newAgentPane, agent.RoleForType(newType)); err != nil {
		return nil, fmt.Errorf("workspace.SwapAgent: tag new agent pane: %w", err)
	}

	// Step 7: restore byte-precise geometry.
	if err := m.Tmux.SelectLayout(ctx, session, layout); err != nil {
		// Non-fatal: layout drift is a UX paper cut, not a correctness
		// bug. Log loudly so dogfood notices.
		log.Warn("workspace.agent-swap.restore-layout-failed",
			"session", session, "err", err.Error())
	}

	// Step 8: land focus on the new agent pane (same rationale as
	// buildSession: user expects to interact with the agent first).
	if err := m.Tmux.SelectPane(ctx, newAgentPane); err != nil {
		log.Warn("workspace.agent-swap.select-pane-failed",
			"session", session, "err", err.Error())
	}

	// Step 9: bump the per-agent launch counter now that the new
	// agent has spawned successfully. Done AFTER spawn (not in
	// step 5's Save) so a failed spawn doesn't lie about history.
	// Next swap to this agent will see AgentLaunches[newType]>0
	// and use Resume.
	err = m.Store.WithLock(func(s *state.State) error {
		row, ferr := s.Find(m.Cfg.ProjectRoot, name)
		if ferr != nil {
			return ferr
		}
		bumpAgentLaunches(row, newType)
		row.AgentLaunchCount++ // legacy total counter, kept in sync
		updated = *row
		return nil
	})
	if err != nil {
		// Non-fatal: the swap succeeded, but next-swap heuristics
		// won't know. Log loud; user-visible state is correct.
		log.Warn("workspace.agent-swap.bump-launches-failed",
			"session", session, "err", err.Error())
	}

	log.Info("workspace.agent-swap.done",
		"session", session,
		"workspace", name,
		"from_agent", oldType,
		"to_agent", newType,
		"new_pane", newAgentPane,
		"agent_launches", updated.AgentLaunches[newType])

	return &updated, nil
}
