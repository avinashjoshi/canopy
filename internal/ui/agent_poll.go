// Agent-state polling for the badge column. Runs an agent.Detector
// over every Ready workspace's agent pane on a 2s tick. Polling is
// gated by a generation token so re-Init never doubles the loop.
package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

// agentPollInterval is the gap between badge-poll ticks. 2s is the
// sweet spot from the v3 design: fast enough that "claude went idle"
// shows up before the user wonders, slow enough that a 5-workspace
// poll costs <5% CPU.
const agentPollInterval = 2 * time.Second

// agentPollTickMsg is the per-tick wakeup. The gen field is the
// generation token captured at schedule time; if it doesn't match the
// current Model.agentPollGen, the tick is stale (Init re-fired) and
// drops itself without rescheduling. Single-in-flight invariant.
type agentPollTickMsg struct {
	gen uint64
}

// agentPollResultMsg carries the latest poll's session-name → state
// map. Update applies it to m.agentStates + the projectlist's view,
// then schedules the next tick. Reschedule lives ONLY here so a
// dropped (stale) tick can never restart the loop.
type agentPollResultMsg struct {
	gen    uint64
	states map[string]agent.State
	active map[string]struct{} // pane IDs seen this tick (for Detector.Prune)
}

// scheduleAgentPollTick returns a tea.Cmd that fires after
// agentPollInterval with the current generation. Capture the gen at
// call time so a re-Init that bumps agentPollGen invalidates this
// pending tick before it lands.
func scheduleAgentPollTick(gen uint64) tea.Cmd {
	return tea.Tick(agentPollInterval, func(time.Time) tea.Msg {
		return agentPollTickMsg{gen: gen}
	})
}

// runAgentPoll is the actual work: list every agent:* pane on the
// tmux server (one batched call), capture each pane (one call per
// pane, wrapped with a 500ms timeout), classify with the Detector,
// emit agentPollResultMsg.
//
// Errors from individual panes are logged + skipped; the message
// always lands so the reschedule fires. A total ListAgentPanes
// failure (e.g. tmux server crashed) yields an empty states map +
// empty active set; badges blank out, which is the right UX.
func runAgentPoll(d *agent.Detector, tx *tmux.Client, gen uint64) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		// Batched: one tmux call lists every agent:* pane on the server.
		listCtx, listCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer listCancel()
		panes, err := tx.ListAgentPanes(listCtx)
		if err != nil {
			log.Warn("agent poll: ListAgentPanes failed", "err", err)
			return agentPollResultMsg{
				gen:    gen,
				states: nil,
				active: nil,
			}
		}

		states := make(map[string]agent.State, len(panes))
		active := make(map[string]struct{}, len(panes))
		for _, p := range panes {
			active[p.ID] = struct{}{}
			launcher := agent.LauncherFromRole(p.Role)
			if launcher == "" {
				// Malformed role tag — Detector would return Unknown
				// anyway; skip the capture-pane call as an optimization.
				continue
			}
			capCtx, capCancel := context.WithTimeout(ctx, 500*time.Millisecond)
			content, err := tx.CapturePane(capCtx, p.ID)
			capCancel()
			if err != nil {
				// Pane died between list and capture (race) or capture
				// hung. Skip; next tick will retry.
				continue
			}
			s, _ := d.Observe(p.ID, launcher, content)
			states[p.Session] = s
		}

		return agentPollResultMsg{
			gen:    gen,
			states: states,
			active: active,
		}
	}
}

// startAgentPolling kicks off the poll loop. Increments the
// generation token (so any in-flight tick from a prior Init drops
// itself) and returns the Cmd that schedules the FIRST tick. All
// subsequent ticks reschedule themselves from agentPollResultMsg's
// handler, so this is the one-and-only entry point.
func (m *Model) startAgentPolling() tea.Cmd {
	if m.tc == nil {
		// No tmux client wired (e.g. some test paths) — no polling.
		return nil
	}
	if m.detector == nil {
		m.detector = agent.NewDetector()
	}
	m.agentPollGen++
	return scheduleAgentPollTick(m.agentPollGen)
}
