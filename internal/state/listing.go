// Project-grouped listing helpers used by both `canopy ls --all` (CLI,
// tabwriter rendering) and the global TUI (lipgloss rendering via the
// internal/ui/projectlist sub-component).
//
// The data assembly lives here so there's exactly ONE place that walks
// State.Projects + State.Workspaces and probes tmux liveness. Everything
// downstream consumes []GlobalRow.
//
// Tmux access is taken as an interface (LivenessProbe) rather than a
// concrete *tmux.Client to keep state from importing tmux. Tests can pass
// a fake.

package state

import (
	"context"
	"path/filepath"
	"sort"
)

// LivenessProbe is the slice of tmux.Client that BuildGlobalRows uses. We
// only need HasSession, and decoupling here lets state stay a leaf package.
//
// Concrete *tmux.Client satisfies this interface naturally — production
// callers pass tmux.New(); tests pass a fake.
type LivenessProbe interface {
	HasSession(ctx context.Context, name string) (bool, error)
}

// AttachedProbe returns the set of tmux session names with at least
// one client attached. Single batch call so populating the column
// across N rows costs one round-trip, not N. Production callers pass
// *tmux.Client (which implements AttachedSessions); tests pass a fake.
type AttachedProbe interface {
	AttachedSessions(ctx context.Context) (map[string]bool, error)
}

// LoadValue is the public face of an aggregate session probe: total
// RSS in bytes plus summed CPU percent across the pane process tree.
// Mirrors tmux.SessionLoad without forcing a state→tmux import.
type LoadValue struct {
	RSS int64
	CPU float64
}

// LoadProbe returns the aggregate RSS+CPU for one tmux session.
// Production callers pass *tmux.Client (which implements
// SessionLoad); tests pass a fake. The probe is wrapped in a TTL
// cache (see LoadCache below) so refresh ticks don't spawn redundant
// ps invocations.
type LoadProbe interface {
	SessionLoad(ctx context.Context, session string) (LoadValue, error)
}

// MemProbe is the legacy single-value form. Kept so older callers
// (and any external consumers) that only want RSS don't have to
// switch to LoadProbe. Internally, BuildGlobalRowsWithLoad uses
// LoadProbe so the CPU number comes for free off the same ps call.
type MemProbe interface {
	SessionRSS(ctx context.Context, session string) (int64, error)
}

// GlobalRow is one row in the cross-project listing. Mirrors the fields the
// CLI tabwriter and the TUI lipgloss table both render.
//
// IsMain marks the synthetic `<project>-main` row that's emitted when a
// project's main tmux session is alive (canopy main, no workspace). For
// main rows: Name="(main)", Branch="—", Status="main".
//
// Path is the worktree directory on disk. Empty for main rows (no
// associated worktree). The TUI uses this as a fallback when a row's
// ProjectRoot is unmigrated (a v1-format basename rather than a canonical
// absolute path) — git's common-dir lookup against the worktree finds
// the source repo without needing migration to have run first.
type GlobalRow struct {
	ProjectRoot string // canonical absolute path; authoritative ID
	Project     string // basename for display (filepath.Base(ProjectRoot))

	IsMain bool
	Name   string
	Branch string
	Status Status
	Port   int

	Path        string // worktree dir on disk (workspace rows); empty for main rows
	TmuxSession string
	Alive       bool

	// Attached is true when at least one tmux client is currently
	// attached to TmuxSession. Distinct from Alive: a session can be
	// running (Alive=true) with no client (Attached=false) — that's
	// the typical "background" state. The TUI surfaces an `⊙` glyph
	// next to attached rows so the user can tell at a glance which
	// workspace they're "in" right now (and therefore probably
	// shouldn't K).
	//
	// Populated by BuildGlobalRowsWithAttached when an AttachedProbe
	// is supplied; left at false for callers that don't probe it.
	Attached bool

	// MemRSS is the summed resident set size (bytes) across every
	// process in every pane of this row's tmux session. Populated by
	// BuildGlobalRowsWithLoad when a LoadProbe is supplied; left at
	// 0 for callers using the older BuildGlobalRows. 0 also signals
	// "skip rendering" — the column shows "—" rather than "0B".
	MemRSS int64

	// CPU is the summed pcpu (single-core normalized; can exceed 100
	// on multi-core boxes) across the same process tree as MemRSS.
	// Populated alongside MemRSS by BuildGlobalRowsWithLoad — the
	// underlying ps probe returns both in one shot, so no extra cost.
	CPU float64

	// LastErrorHint is the auto-detected diagnosis from workspace.Diagnose,
	// populated only when Status==broken AND canopy recognized the failure
	// signature. Rendered as a "hint:" line under the table when the cursor
	// sits on this row. Mirrors the field on the (deleted) ui.Row type;
	// promoted to GlobalRow during v0.8 unification so cross-project rows
	// can carry the diagnosis through to the unified TUI.
	LastErrorHint string

	// Hints are v0.6 lifecycle detector results for this row, populated
	// when the caller wants the row decorated with badges (the global
	// TUI). Empty when the caller skips detection (e.g., canopy ls --all
	// from the CLI prints rows without hints to keep the tabwriter
	// output narrow).
	//
	// Populated by BuildGlobalRowsWithHints; the older BuildGlobalRows
	// leaves this empty for callers that don't need detector decoration.
	Hints []Hint
}

// BuildGlobalRows is the single source of truth for the cross-project
// listing. It assembles rows from s in a stable order and probes each
// session's tmux liveness via probe.
//
// The project list is the union of:
//
//   - every project that owns at least one workspace in s.Workspaces
//   - every project recorded in s.Projects (which includes projects that
//     have only been touched via `canopy main` and have no workspaces yet)
//
// For each project, we emit (in this order):
//
//  1. The synthetic main row, if `<basename>-main` tmux session is alive.
//  2. All workspace rows, sorted by name.
//
// Liveness probe failures are non-fatal: a row whose HasSession errored
// renders as Alive=false, same as a confirmed-dead session. The user sees
// a dim ○ badge instead of a crash.
//
// Sort order: projects sorted by canonical root path (stable, doesn't
// change as workspaces come and go); within a project, workspaces by name.
//
// Pure-ish function: doesn't mutate s. The "ish" is the one tmux side
// effect (HasSession queries the daemon). The main "main session aliveness"
// query for a project that's only in s.Projects (no workspaces) is the
// reason we take the probe at all.
func (s *State) BuildGlobalRows(ctx context.Context, probe LivenessProbe) []GlobalRow {
	// Single batch call to enumerate which sessions have a client
	// attached. The cost is one tmux round-trip per Build call
	// regardless of N rows — `tmux list-sessions -F` is dirt cheap
	// and avoids N-per-row probing. Optional: only runs if the probe
	// also implements AttachedProbe (production *tmux.Client does;
	// liveness-only fakes in tests opt out by not implementing).
	attached := map[string]bool{}
	if ap, ok := probe.(AttachedProbe); ok {
		if got, err := ap.AttachedSessions(ctx); err == nil {
			attached = got
		}
	}
	// Deterministic project iteration order: collect roots, sort.
	rootSet := map[string]struct{}{}
	for _, w := range s.Workspaces {
		// Prefer ProjectRoot (v2). Fall back to legacy basename so v1
		// rows that haven't yet been migrated still render — they'll
		// appear under their basename until the next project-scoped
		// command runs migration.
		if w.ProjectRoot != "" {
			rootSet[w.ProjectRoot] = struct{}{}
		} else if w.Project != "" {
			rootSet[w.Project] = struct{}{}
		}
	}
	for root := range s.Projects {
		rootSet[root] = struct{}{}
	}

	roots := make([]string, 0, len(rootSet))
	for r := range rootSet {
		roots = append(roots, r)
	}
	sort.Strings(roots)

	// Pre-bucket workspaces by their effective project key.
	byProject := map[string][]Workspace{}
	for _, w := range s.Workspaces {
		key := w.ProjectRoot
		if key == "" {
			key = w.Project
		}
		byProject[key] = append(byProject[key], w)
	}
	for k := range byProject {
		ws := byProject[k]
		sort.Slice(ws, func(i, j int) bool { return ws[i].Name < ws[j].Name })
		byProject[k] = ws
	}

	rows := make([]GlobalRow, 0, len(s.Workspaces)+len(roots))

	for _, root := range roots {
		basename := filepath.Base(root)

		// Main row (synthetic) — always included so each project shows
		// its main entry regardless of whether `canopy main` has been
		// run yet. Alive reflects the actual session state; the
		// activate handler decides what enter does (attach if alive,
		// otherwise surface a "run canopy main" hint).
		//
		// Earlier versions gated this on session liveness, but that
		// hid the existence of a main option from users who hadn't
		// started one yet — confusing UX since you couldn't tell
		// from the TUI whether a project HAD a main concept at all.
		// The session name uses the basename, not the canonical
		// root, because tmux session names need to stay short and
		// human-readable. The basename-uniqueness invariant enforced
		// at canopy init prevents same-name collisions.
		mainSession := safeMainSessionName(basename)
		alive, _ := probe.HasSession(ctx, mainSession)
		mainRow := GlobalRow{
			ProjectRoot: root,
			Project:     basename,
			IsMain:      true,
			Name:        "(main)",
			Branch:      "—",
			Status:      "main",
			TmuxSession: mainSession,
			Alive:       alive,
			Attached:    attached[mainSession],
		}
		if meta, ok := s.Projects[root]; ok {
			mainRow.Port = meta.PortBase
		}
		rows = append(rows, mainRow)

		// Workspace rows.
		for _, w := range byProject[root] {
			alive, _ := probe.HasSession(ctx, w.TmuxSession)
			rows = append(rows, GlobalRow{
				ProjectRoot:   root,
				Project:       basename,
				Name:          w.Name,
				Branch:        w.Branch,
				Status:        w.Status,
				Port:          w.Port,
				Path:          w.Path,
				TmuxSession:   w.TmuxSession,
				Alive:         alive,
				Attached:      attached[w.TmuxSession],
				LastErrorHint: w.LastErrorHint,
			})
		}
	}

	return rows
}

// safeMainSessionName returns the tmux session name for a project's main
// session, given its basename. Mirrors what `canopy main` writes so the
// listing's liveness probe checks the right session.
//
// We deliberately don't import internal/tmux here (which would create a
// state→tmux package dependency for something this trivial). The rule is
// the same as tmux.SafeName for typical basenames (alphanumerics + hyphen
// + underscore pass through; everything else collapses to '-').
func safeMainSessionName(basename string) string {
	var b []byte
	prevDash := false
	for _, r := range basename {
		safe := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-'
		if safe {
			b = append(b, byte(r))
			prevDash = (r == '-')
			continue
		}
		if !prevDash {
			b = append(b, '-')
			prevDash = true
		}
	}
	// Trim trailing dash before suffixing.
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	return string(b) + "-main"
}
