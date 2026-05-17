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
	"strconv"
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
	Drift        Drift  // how Version compares to the reference passed to BuildRows
	LastSeen     time.Time
	LastError    string // raw error string from snapshot, displayed verbatim

	// ClipboardBridge is the v0.18 bridge status reported by the host.
	// One of "off", "bridged", "broken" — or "" when the remote canopy
	// is older than v0.18 (no field emitted). Drives the `📋` pill.
	ClipboardBridge string
}

// Drift describes how the host's reported canopy version compares to a
// reference version (the laptop's running binary, or — when the laptop
// is on a dev build — the upstream-latest cached value). Drives the
// yellow ⇑ / ⇓ badge on the version cell so users can spot "this host
// needs a `U` upgrade" without comparing strings by eye.
//
// "Unknown" covers every case where we deliberately suppress the badge:
// missing reference, missing remote version, dev on either side, or
// "(unknown)" sentinels emitted by older remote canopy builds.
type Drift int

const (
	// DriftUnknown — comparison not possible or not meaningful.
	// Includes: empty remote version, "dev" or "(unknown)" on either
	// side, missing reference. Renders the version cell in the existing
	// subtle gray with no badge.
	DriftUnknown Drift = iota
	// DriftSame — remote matches reference exactly. Same render as
	// DriftUnknown (no badge) — silence is the signal.
	DriftSame
	// DriftBehind — remote is older than reference. Yellow ⇑ badge
	// after the version: "this host should be upgraded."
	DriftBehind
	// DriftAhead — remote is newer than reference. Yellow ⇓ badge:
	// "your laptop is older than this host." Uncommon but worth
	// surfacing so users don't miss the inverted mismatch.
	DriftAhead
)

// Status enumerates the per-host health states the Hosts tab renders.
// Distinct from state.Status (workspace-level): this is host-level.
type Status int

const (
	// StatusUnknown — never refreshed this session AND no fan-out is
	// currently in flight. Renders neutral.
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
	// StatusLoading — host has no snapshot yet AND a refresh fan-out
	// is in flight. Renders a spinner glyph driven by spinnerFrame so
	// users on the first TUI launch see "we're checking" instead of an
	// empty Hosts tab. Resolves to one of the four real statuses above
	// once the per-host refresh result lands.
	StatusLoading
)

// spinnerFrames is the Braille rotation used by StatusLoading. Ten
// frames matches the bubbles/spinner Line preset visually but lives
// here as a string literal so the hosts package stays free of the
// charm spinner dependency. The render path indexes this slice by
// (frame mod len) so callers don't have to clamp.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerGlyph returns the Braille frame at the given index (mod the
// frame count, with a defensive guard against negative inputs from a
// caller that wrapped int math wrong).
func spinnerGlyph(frame int) string {
	n := len(spinnerFrames)
	idx := frame % n
	if idx < 0 {
		idx += n
	}
	return spinnerFrames[idx]
}

// BuildRows assembles the display rows by joining the host registry
// (the source of truth for "which hosts are registered") with the
// remotes-cache (the source of truth for "what we last knew about
// each host"). Hosts registered but never refreshed render with
// Status=Unknown. Hosts in the cache but not in the registry are
// orphans — surfaced as a separate "(stale)" entry until the next
// refresh prunes them.
//
// referenceVersion is the version each host's CanopyVersion is compared
// against to compute Row.Drift. Pass the bare semver of the laptop's
// running canopy (release builds) or the cached upstream-latest semver
// (dev builds). Pass "" to suppress drift detection across the board —
// every row gets DriftUnknown.
//
// refreshing flags that a remote-fan-out is in flight RIGHT NOW. Hosts
// without a snapshot under that condition render StatusLoading + the
// spinner; with refreshing=false they fall back to StatusUnknown so
// the row still appears (visibility-first per the package doc). The
// spinnerFrame is forwarded into Render's glyph lookup.
//
// Sort: name ASC. Deterministic so the cursor doesn't shuffle.
func BuildRows(hosts []host.Host, snapshots map[string]*state.RemoteHostSnapshot, referenceVersion string, refreshing bool) []Row {
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
			if refreshing {
				r.Status = StatusLoading
				r.StatusDetail = "checking…"
			} else {
				r.Status = StatusUnknown
				r.StatusDetail = "(never refreshed)"
			}
		} else {
			r.Workspaces = len(snap.Workspaces)
			r.Version = snap.CanopyVersion
			r.Drift = ComputeDrift(snap.CanopyVersion, referenceVersion)
			r.LastSeen = snap.LastSeen
			r.LastError = snap.LastError
			r.ClipboardBridge = snap.ClipboardBridge
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
// column visibility per the D2 tiered drop. cursor indicates the
// selected row (-1 for "no cursor", e.g. when the cursor is out of
// bounds or rendering for a non-interactive context). spinnerFrame
// drives the StatusLoading glyph animation — callers advance it from
// a tea.Cmd while a remote refresh fan-out is in flight (the same
// condition BuildRows uses to flip rows into StatusLoading). v0.17
// Phase 1l; spinner add in v0.22.
func Render(rows []Row, width int, cursor int, spinnerFrame int) string {
	if len(rows) == 0 {
		return emptyState()
	}
	var b strings.Builder
	for i, r := range rows {
		b.WriteString(renderRow(r, width, i == cursor, spinnerFrame))
		b.WriteByte('\n')
	}
	return b.String()
}

// renderRow is the per-host line. Format adapts to terminal width.
// selected toggles the `❯ ` caret + selection bg padded across the
// full terminal width — matches the workspace list's selected-row
// treatment so the two tabs feel like the same surface.
func renderRow(r Row, width int, selected bool, spinnerFrame int) string {
	// Build plain (un-styled) parts for the selection path so the
	// outer style's background wins across the entire row; inner
	// foreground colors are dropped by selectionStyle anyway.
	if selected {
		parts := []string{statusGlyphPlain(r.Status, spinnerFrame), r.Name}
		if width >= 80 && r.Version != "" {
			parts = append(parts, "v"+r.Version+driftGlyphPlain(r.Drift))
		}
		if width >= 80 {
			if pill := clipboardPillPlain(r.ClipboardBridge); pill != "" {
				parts = append(parts, pill)
			}
		}
		if width >= 100 {
			parts = append(parts, fmt.Sprintf("%dp %dw", r.Projects, r.Workspaces))
		}
		if width >= 160 && r.SSHTarget != "" {
			parts = append(parts, r.SSHTarget)
		}
		parts = append(parts, r.StatusDetail)
		body := "❯ " + strings.Join(parts, "  ")
		s := selectionStyle()
		if width > 0 {
			s = s.Width(width)
		}
		return s.Render(body)
	}
	// Non-selected: full per-column styling for visual density.
	glyph := statusGlyph(r.Status, spinnerFrame)
	name := nameStyle().Render(r.Name)
	detail := statusDetailStyle(r.Status).Render(r.StatusDetail)
	parts := []string{glyph, name}
	if width >= 80 && r.Version != "" {
		parts = append(parts, renderVersionCell(r.Version, r.Drift))
	}
	if width >= 80 {
		if pill := clipboardPill(r.ClipboardBridge); pill != "" {
			parts = append(parts, pill)
		}
	}
	if width >= 100 {
		parts = append(parts, subtleStyle().Render(fmt.Sprintf("%dp %dw", r.Projects, r.Workspaces)))
	}
	if width >= 160 && r.SSHTarget != "" {
		parts = append(parts, subtleStyle().Render(r.SSHTarget))
	}
	parts = append(parts, detail)
	return "  " + strings.Join(parts, "  ")
}

// clipboardPill renders the bridge state pill. Returns "" for the off
// case (no pill, keep the row uncluttered for the common state) and
// for the unknown/empty case (pre-v0.18 remote or no snapshot yet).
//
// v0.18 design-doc palette: 📋 (clipboard) glyph + suffix word, never
// glyph alone (a11y rule from /plan-design-review).
//   - bridged  → green   📋 bridged
//   - broken   → amber   📋! broken
//   - off      → ""      (no pill — common state, keep row lean)
//   - unknown  → ""      (same — no signal to display)
func clipboardPill(bridge string) string {
	switch bridge {
	case "bridged":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render("📋 bridged")
	case "broken":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("📋! broken")
	default:
		return ""
	}
}

// clipboardPillPlain is the selected-row variant: no inner foreground
// color (the selection style's bright-white fg wins uniformly).
func clipboardPillPlain(bridge string) string {
	switch bridge {
	case "bridged":
		return "📋 bridged"
	case "broken":
		return "📋! broken"
	default:
		return ""
	}
}

// statusGlyphPlain returns the same glyph as statusGlyph but without
// the foreground color — used in the selected-row path where the
// outer selectionStyle's bright-white fg should win uniformly. The
// loading branch consumes the same spinnerFrame so the cursor's row
// stays in lockstep with the rest of the column.
func statusGlyphPlain(s Status, spinnerFrame int) string {
	switch s {
	case StatusOnline:
		return "●"
	case StatusOffline:
		return "○"
	case StatusAuthFailed:
		return "!"
	case StatusBroken:
		return "✗"
	case StatusLoading:
		return spinnerGlyph(spinnerFrame)
	default:
		return "·"
	}
}

// selectionStyle mirrors projectlist.selectionStyle: dark-grey bg +
// bright-white fg + bold, padded to terminal width. The two tabs
// share the same selected-row vocabulary so the user reads "this is
// the cursor" identically across both surfaces. v0.17 Phase 1l polish.
func selectionStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Background(lipgloss.Color("237")).
		Foreground(lipgloss.Color("231")).
		Bold(true)
}

func emptyState() string {
	return subtleStyle().Render(
		"No hosts registered. Press `n` to add one, or run from terminal:\n" +
			"  canopy host add <name> <ssh-target>")
}

// statusGlyph maps the status enum to a colored glyph. Per D3 a11y:
// glyph + color, never color alone. Same vocabulary as v0.16.1
// agent-state badges so users learn one set of symbols. Loading
// renders the spinner at the supplied frame in the same dim cyan as
// other "in-progress" surfaces (matches the workspace-create progress
// pane vocabulary).
func statusGlyph(s Status, spinnerFrame int) string {
	switch s {
	case StatusOnline:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render("●") // green
	case StatusOffline:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("○") // gray
	case StatusAuthFailed:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("!") // amber
	case StatusBroken:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✗") // red
	case StatusLoading:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("87")).Render(spinnerGlyph(spinnerFrame)) // cyan
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
	case StatusLoading:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("87"))
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

// renderVersionCell renders the "v<version><glyph>" cell for one row,
// styled by drift. DriftSame / DriftUnknown render in the subtle gray
// already used for non-drift versions; DriftBehind / DriftAhead use
// the yellow ⇑ / ⇓ vocabulary the top-bar version pill established
// for "upgrade available" (render.go's renderVersionPill).
func renderVersionCell(version string, d Drift) string {
	body := "v" + version
	switch d {
	case DriftBehind, DriftAhead:
		return driftStyle().Render(body + driftGlyphPlain(d))
	default:
		return subtleStyle().Render(body)
	}
}

// driftStyle is the yellow attention color used by the version-pill
// upgrade arrow (render.go:220). Keeping the palette identical means
// "yellow on the version" reads as "upgrade available" everywhere it
// appears, top-bar pill and Hosts tab alike.
func driftStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
}

// driftGlyphPlain returns the trailing glyph (with leading space) for
// a row's drift. Plain because the selected-row path can't carry
// inner foreground colors — the outer selection bg wins anyway.
// Empty string for DriftSame/DriftUnknown keeps the cell unchanged in
// the common case.
func driftGlyphPlain(d Drift) string {
	switch d {
	case DriftBehind:
		return " ⇑"
	case DriftAhead:
		return " ⇓"
	default:
		return ""
	}
}

// ComputeDrift compares a remote canopy version against a reference
// (laptop or upstream-latest). Returns DriftUnknown the moment either
// input is non-comparable — "dev", "(unknown)", "", or any string that
// doesn't parse as a dotted-number sequence — because the alternative
// (showing a yellow upgrade arrow with no real comparison behind it)
// would be misleading.
//
// Both inputs are normalized via ExtractBareSemver, so callers can pass
// raw wire forms (the laptop's "v0.17.4.0+abc" or the remote's
// "0.17.4.0+abc") without pre-trimming.
func ComputeDrift(remote, reference string) Drift {
	r := extractBareSemver(remote)
	ref := extractBareSemver(reference)
	if r == "" || ref == "" {
		return DriftUnknown
	}
	cmp := compareSemver(r, ref)
	switch {
	case cmp < 0:
		return DriftBehind
	case cmp > 0:
		return DriftAhead
	default:
		return DriftSame
	}
}

// ExtractBareSemver is the exported normalizer paired with
// ComputeDrift. The host-detail drawer uses it to format the reference
// version in the drift annotation line ("upgrade available: v0.17.4.0"
// reads better than "v0.17.4.0+abc1234").
func ExtractBareSemver(s string) string { return extractBareSemver(s) }

// extractBareSemver strips the conventional "v" prefix and "+sha"
// build-metadata suffix from a canopy version string and returns the
// bare dotted-number form ("0.17.4.0"). Returns "" for "dev",
// "(unknown)", "", or any input whose first segment isn't a number
// — those don't represent a comparable release.
//
// Tolerant of both wire forms canopy emits:
//   - laptop running binary: "v0.17.4.0+abc1234" (Makefile ldflags)
//   - remote ls --json:      "0.17.4.0+abc1234" ("v" already stripped)
func extractBareSemver(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "dev" || s == "(unknown)" {
		return ""
	}
	s = strings.TrimPrefix(s, "v")
	if i := strings.Index(s, "+"); i >= 0 {
		s = s[:i]
	}
	// Guard against arbitrary strings sneaking through — require the
	// first segment to parse as a number. Anything else (e.g. a
	// branch name, "main-abc1234") is not a comparable release.
	parts := strings.SplitN(s, ".", 2)
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return ""
	}
	return s
}

// compareSemver compares two dotted-number version strings by
// integer-valued components. Returns -1 if a < b, 0 if equal, 1 if a
// > b. Both inputs must already be normalized (no "v" prefix, no
// "+sha" suffix) — callers route through extractBareSemver.
//
// Tolerant of trailing zeros and length differences: "0.17" compares
// equal to "0.17.0.0". Non-numeric components are treated as 0 — a
// defensive choice against malformed remote payloads, not a feature.
//
// Parallel to cmd/canopy.compareSemver: kept duplicated rather than
// dragged across the package boundary because cmd/canopy is package
// main and can't be imported. ~20 lines is cheaper than carving out a
// shared internal/version package for v0.17.
func compareSemver(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	n := len(aParts)
	if len(bParts) > n {
		n = len(bParts)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(aParts) {
			ai, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bi, _ = strconv.Atoi(bParts[i])
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}
