// Package inbox renders canopy's triage queue — the cathedral surface
// of v0.17.0. Sorts every row in the user's view by agent state, with
// the workspaces needing attention right now at the top:
//
//	✋ awaiting-input      (top — agent is literally stuck on a prompt)
//	⚡ thinking (stale)    (might be stuck; investigate)
//	⚡ thinking            (working, no action needed)
//	💤 idle                (agent paused, ready for next prompt)
//	·  inactive            (no agent or unknown — bottom)
//
// Within each state group, secondary sort is by recency of most recent
// activity (most-recently-touched first). D11 from /plan-design-review.
//
// Empty state: "All caught up. 🌱". The product personality lives here.
//
// v0.17.0 Phase 1d.1 limitation: agent state for LOCAL rows comes from
// the laptop's polling map (`agentStates`). REMOTE rows from
// host.Refresher don't yet carry agent state (Phase 1d.2 adds it via
// `canopy ls --json` extension). For now, remote rows are treated as
// "inactive" and fall to the bottom group.
package inbox

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/state"
)

// Row is the rendered shape of one inbox entry. Built from
// state.GlobalRow + (for local rows) the agent-state polling map.
type Row struct {
	State       agent.State
	Host        string // empty for local, host name for remote
	Project     string
	Name        string
	TmuxSession string
	Branch      string
	// LastActivity is the most recent time signal the inbox knows about.
	// For local rows: derived from the agent-state poll timestamp.
	// For remote rows: zero (we don't have it yet in Phase 1d.1).
	LastActivity time.Time
}

// Sort orders rows by (state-bucket, recency desc). State-bucket
// priority is the user-facing "what needs me NOW" order.
//
// Stable sort so ties (same state + same recency) preserve the input
// order, which downstream callers can use to influence sub-sort
// (e.g., by name).
func Sort(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		bi := stateBucket(rows[i].State)
		bj := stateBucket(rows[j].State)
		if bi != bj {
			return bi < bj
		}
		// Within bucket: most-recently-active first.
		return rows[i].LastActivity.After(rows[j].LastActivity)
	})
}

// stateBucket maps agent state to its inbox priority. Lower is more
// urgent (sorts to the top). Keep the gaps so future fine-grained
// states (awaiting-permission vs. awaiting-text) can insert without
// renumbering.
func stateBucket(s agent.State) int {
	switch s {
	case agent.StateAwaitingInput:
		return 10
	case agent.StateThinking:
		return 20
	case agent.StateIdle:
		return 30
	default:
		// Unknown / inactive — agent state not detected.
		return 90
	}
}

// BuildFromRows assembles inbox rows from a slice of state.GlobalRow
// (the same rows the Global tab renders) plus the laptop's agent-state
// polling map. Local rows get their state from agentStates; remote
// rows are tagged Unknown for now (Phase 1d.2 lifts this).
//
// Excludes IsMain rows: main sessions have no agent and aren't part
// of the triage flow.
func BuildFromRows(globals []state.GlobalRow, agentStates map[string]agent.State) []Row {
	out := make([]Row, 0, len(globals))
	now := time.Now() // anchor for "recency" of local rows where we don't have a true timestamp
	for _, g := range globals {
		if g.IsMain {
			continue
		}
		st := agent.StateUnknown
		var activity time.Time
		if g.Host == "" {
			if v, ok := agentStates[g.TmuxSession]; ok {
				st = v
				// We don't yet track per-state transition times; use
				// "now" as the activity anchor so the recency sort
				// preserves input order for equally-recent local rows.
				activity = now
			}
		}
		// Remote rows: state stays Unknown, activity zero. Phase 1d.2
		// will populate from `canopy ls --json` agent_state field.
		out = append(out, Row{
			State:        st,
			Host:         g.Host,
			Project:      g.Project,
			Name:         g.Name,
			TmuxSession:  g.TmuxSession,
			Branch:       g.Branch,
			LastActivity: activity,
		})
	}
	Sort(out)
	return out
}

// Render returns the rendered Inbox tab. Width controls truncation.
func Render(rows []Row, width int) string {
	if !anyAttention(rows) {
		return emptyState(rows)
	}
	var b strings.Builder
	prevBucket := -1
	for _, r := range rows {
		// Drop the "inactive" tail — once we've hit unknown-state rows,
		// the user has scanned the actionable section; further rows
		// are just noise in the triage view.
		if stateBucket(r.State) == 90 && prevBucket != 90 && prevBucket != -1 {
			b.WriteByte('\n')
			b.WriteString(subtleStyle().Render(fmt.Sprintf("— %d more inactive (run `tab` for the Global view) —", countInactive(rows))))
			b.WriteByte('\n')
			break
		}
		b.WriteString(renderRow(r, width))
		b.WriteByte('\n')
		prevBucket = stateBucket(r.State)
	}
	return b.String()
}

// renderRow is one inbox line: glyph + name + host + project + recency.
func renderRow(r Row, width int) string {
	glyph := stateGlyph(r.State)
	name := nameStyle().Render(r.Name)
	host := r.Host
	if host == "" {
		host = "local"
	}
	context := subtleStyle().Render(fmt.Sprintf("%s · %s", host, r.Project))
	return fmt.Sprintf("  %s  %s  %s", glyph, name, context)
}

// emptyState fires when no row needs attention. The phrasing is
// intentionally warm — this is the "you've cleared the queue" moment.
func emptyState(rows []Row) string {
	total := len(rows)
	switch {
	case total == 0:
		return subtleStyle().Render("Nothing here yet. Run `canopy new` to spin up your first agent.")
	case total == 1:
		return subtleStyle().Render("All caught up. 🌱   (1 workspace, all idle)")
	default:
		return subtleStyle().Render(fmt.Sprintf("All caught up. 🌱   (%d workspaces, all idle)", total))
	}
}

// anyAttention returns true if at least one row is in an actionable
// state (awaiting, thinking, idle). All-unknown means everything's
// inactive and the empty state is the right render.
func anyAttention(rows []Row) bool {
	for _, r := range rows {
		if stateBucket(r.State) < 90 {
			return true
		}
	}
	return false
}

func countInactive(rows []Row) int {
	n := 0
	for _, r := range rows {
		if stateBucket(r.State) == 90 {
			n++
		}
	}
	return n
}

func stateGlyph(s agent.State) string {
	switch s {
	case agent.StateAwaitingInput:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render("✋")
	case agent.StateThinking:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Render("⚡")
	case agent.StateIdle:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("💤")
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("·")
	}
}

func nameStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Bold(true)
}

func subtleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
}
