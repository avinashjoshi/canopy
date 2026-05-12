// Package hosts is the Bubbletea subcomponent that renders canopy's
// Hosts tab — the fleet view of every registered remote SSH host.
// v0.17.0 Phase 1c.
//
// Sibling of internal/ui/projectlist: both are tab-scoped subcomponents
// that the parent ui.Model embeds. Hosts is for managing the host
// registry (hosts.json); projectlist is for navigating workspaces.
//
// Display priorities:
//
//  1. Visibility — every registered host appears, even ones whose
//     refresh failed. Status pill shows why (auth, offline, drift).
//     Per the v0.17 design's P2-TODO from dogfooding: silent
//     disappearance is bad UX.
//
//  2. Glyph + color status — the "online" dot is `●` green; "offline"
//     is `○` gray; "auth-failed" is `!` amber; "broken" is `✗` red.
//     Color-blind users get the glyph alongside; sighted users get
//     both. D3 from /plan-design-review.
//
//  3. Width-aware columns — tiered drop at narrow terminals (D2).
//     Always visible: name, status glyph. ≥80c adds version.
//     ≥100c adds workspaces. ≥120c adds last-seen. ≥160c adds RTT
//     and ssh-target.
package hosts

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
)

// Row is the rendered shape of one host in the Hosts tab. Built from
// host.Host + (optional) state.RemoteHostSnapshot at render time.
type Row struct {
	Name         string
	SSHTarget    string
	Type         string // "ssh" for v0.17
	Status       Status
	StatusDetail string // free-form: "v0.17.0", "last seen 2m ago", "Permission denied"
	Projects     int    // count from host.Host.Projects
	Workspaces   int    // count from RemoteHostSnapshot.Workspaces
	Version      string // from snapshot.CanopyVersion, empty if never reached
	LastSeen     time.Time
	LastError    string // raw error string from snapshot, displayed verbatim
}

// Status enumerates the per-host health states the Hosts tab renders.
// Distinct from state.Status (workspace-level): this is host-level.
type Status int

const (
	// StatusUnknown — never refreshed this session. Renders neutral.
	StatusUnknown Status = iota
	// StatusOnline — most recent refresh succeeded.
	StatusOnline
	// StatusOffline — refresh failed with network/timeout error
	// (could be host asleep, tailscale not connected, etc).
	StatusOffline
	// StatusAuthFailed — SSH said "Permission denied". Recovery is
	// `ssh-copy-id` or similar; the next refresh will retry.
	StatusAuthFailed
	// StatusBroken — refresh succeeded at SSH level but the remote
	// canopy responded with an error (binary not installed, version
	// drift, malformed JSON).
	StatusBroken
)

// BuildRows assembles the display rows by joining the host registry
// (the source of truth for "which hosts are registered") with the
// remotes-cache (the source of truth for "what we last knew about
// each host"). Hosts registered but never refreshed render with
// Status=Unknown. Hosts in the cache but not in the registry are
// orphans — surfaced as a separate "(stale)" entry until the next
// refresh prunes them.
//
// Sort: name ASC. Deterministic so the cursor doesn't shuffle.
func BuildRows(hosts []host.Host, snapshots map[string]*state.RemoteHostSnapshot) []Row {
	rows := make([]Row, 0, len(hosts))
	for _, h := range hosts {
		r := Row{
			Name:      h.Name,
			SSHTarget: h.SSHTarget,
			Type:      h.Type,
			Projects:  len(h.Projects),
		}
		snap := snapshots[h.Name]
		if snap == nil {
			r.Status = StatusUnknown
			r.StatusDetail = "(never refreshed)"
		} else {
			r.Workspaces = len(snap.Workspaces)
			r.Version = snap.CanopyVersion
			r.LastSeen = snap.LastSeen
			r.LastError = snap.LastError
			switch {
			case snap.LastError == "":
				r.Status = StatusOnline
				r.StatusDetail = humanizeSince(snap.LastSeen)
			case strings.Contains(snap.LastError, "Permission denied"):
				r.Status = StatusAuthFailed
				r.StatusDetail = "key auth not set up"
			case strings.Contains(snap.LastError, "canopy: not found"):
				r.Status = StatusBroken
				r.StatusDetail = "canopy not installed on remote"
			case strings.Contains(snap.LastError, "timeout"),
				strings.Contains(snap.LastError, "Connection refused"),
				strings.Contains(snap.LastError, "no route to host"):
				r.Status = StatusOffline
				r.StatusDetail = "unreachable"
			default:
				r.Status = StatusBroken
				r.StatusDetail = truncate(snap.LastError, 60)
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// Render returns the rendered Hosts tab as a string. Width drives
// column visibility per the D2 tiered drop.
func Render(rows []Row, width int) string {
	if len(rows) == 0 {
		return emptyState()
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(renderRow(r, width))
		b.WriteByte('\n')
	}
	return b.String()
}

// renderRow is the per-host line. Format adapts to terminal width.
func renderRow(r Row, width int) string {
	// Minimum row format: <glyph> <name>  <detail>
	// Adds columns as width permits.
	glyph := statusGlyph(r.Status)
	name := nameStyle().Render(r.Name)
	detail := statusDetailStyle(r.Status).Render(r.StatusDetail)

	// Build optional middle columns based on width budget.
	parts := []string{glyph, name}
	if width >= 80 && r.Version != "" {
		parts = append(parts, subtleStyle().Render("v"+r.Version))
	}
	if width >= 100 {
		parts = append(parts, subtleStyle().Render(fmt.Sprintf("%dp %dw", r.Projects, r.Workspaces)))
	}
	if width >= 160 && r.SSHTarget != "" {
		parts = append(parts, subtleStyle().Render(r.SSHTarget))
	}
	parts = append(parts, detail)
	return strings.Join(parts, "  ")
}

func emptyState() string {
	return subtleStyle().Render(
		"No hosts registered. Press `n` to add one, or run from terminal:\n" +
			"  canopy host add <name> <ssh-target>")
}

// statusGlyph maps the status enum to a colored glyph. Per D3 a11y:
// glyph + color, never color alone. Same vocabulary as v0.16.1
// agent-state badges so users learn one set of symbols.
func statusGlyph(s Status) string {
	switch s {
	case StatusOnline:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render("●") // green
	case StatusOffline:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("○") // gray
	case StatusAuthFailed:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("!") // amber
	case StatusBroken:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✗") // red
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("·") // dim
	}
}

func statusDetailStyle(s Status) lipgloss.Style {
	switch s {
	case StatusOnline:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("46"))
	case StatusAuthFailed:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	case StatusBroken:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	}
}

func nameStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
}

func subtleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
}

// humanizeSince renders "12s ago", "3m ago", "2h ago" — the relative
// time strings the Hosts tab uses for last-seen pills.
func humanizeSince(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
