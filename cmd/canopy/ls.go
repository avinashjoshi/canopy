package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

var lsFlags struct {
	all bool
}

// lsCmd returns the `canopy ls` cobra subcommand.
//
// Three modes:
//
//	cwd inside a project (canopy.json discoverable) + no --all
//	  -> show workspaces for that project (the project-scoped view)
//
//	--all flag, OR cwd outside any canopy project
//	  -> show every workspace canopy knows about, grouped by project
//
//	--all from inside a project
//	  -> same as global; explicit override
//
// Falling back to global mode when no canopy.json is discoverable means
// running `canopy ls` from your home directory just works — you see
// every workspace across cravd, canopy, brain, etc., without having to
// cd into anything first. That's the foundation the TUI (step 6) will
// build on for cross-project switching.
//
// Output: tab-aligned columns, one line per workspace. Project-scoped
// view omits the PROJECT column; global view adds it as the leading
// column with rows grouped by project (and sorted within each group).
func lsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List workspaces for the current project (or all projects with --all)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("ls: getwd: %w", err)
			}

			// Try to discover canopy.json. Errors that aren't "not found"
			// are real (permission, etc.) and worth surfacing.
			cfg, cfgErr := config.DiscoverAndLoad(cwd)
			if cfgErr != nil && !errors.Is(cfgErr, config.ErrNotFound) {
				return cfgErr
			}

			// Decide mode. --all forces global; missing canopy.json
			// gracefully falls back to global.
			global := lsFlags.all || cfg == nil
			if global {
				return lsGlobal(cmd.Context(), cmd.OutOrStdout())
			}
			return lsProject(cmd.Context(), cmd.OutOrStdout(), cfg.ProjectRoot, cfg.Project)
		},
	}
	cmd.Flags().BoolVar(&lsFlags.all, "all", false,
		"list workspaces across all projects (implicit when no canopy.json is found)")
	return cmd
}

// liveBadge returns a single-character indicator for whether the named
// tmux session is alive RIGHT NOW. Cheap call (one `tmux has-session`
// per row) — total cost for `canopy ls` with N workspaces is O(N) tmux
// queries, each <1ms. The output renders the same width whether alive
// or not so the column stays aligned.
func liveBadge(ctx context.Context, tc *tmux.Client, sessionName string) string {
	alive, err := tc.HasSession(ctx, sessionName)
	if err != nil || !alive {
		return "○"
	}
	return "●"
}

// mainSessionRow returns a synthetic display row for the project's
// `canopy main` tmux session (`<basename>-main`) IF it's currently alive.
// canopy main doesn't write a state.json workspace row — the session is
// ephemeral by design — so without this special-case query, `canopy ls`
// would hide a perfectly running main session and confuse users who
// just ran `canopy main`. Returns ok=false when the session isn't alive.
//
// In v2 state, st.Projects is keyed by canonical root path. The PortBase
// lookup walks the map looking for any entry whose Root has the matching
// basename — needed for the lsGlobal path where we only have the basename
// at hand. lsProject (which has both) gets the same code path for free
// since basename → root linkage in canonicalRoot is unambiguous when the
// uniqueness invariant holds.
func mainSessionRow(ctx context.Context, tc *tmux.Client, st *state.State, basename string) (mainRow, bool) {
	session := tmux.SafeName(basename) + "-main"
	alive, err := tc.HasSession(ctx, session)
	if err != nil || !alive {
		return mainRow{}, false
	}
	row := mainRow{Project: basename, Session: session}
	// v2 lookup: scan Projects for the entry whose key (canonical root)
	// has this basename. Falls back to v1 basename-keyed lookup for
	// pre-migration entries.
	for root, meta := range st.Projects {
		if filepath.Base(root) == basename {
			row.Port = meta.PortBase
			break
		}
	}
	return row, true
}

// mainRow is the data shape we render for a `canopy main` session. Port
// comes from state.Projects[project].PortBase — the project's reserved
// base port that canopy main exports as CANOPY_PORT.
type mainRow struct {
	Project string
	Session string
	Port    int // 0 if state has no Projects entry for this project
}

// portCell renders the port column for a main row: the actual port
// number when state knows about the project, or "—" when it doesn't
// (rare; only happens for tmux sessions left behind from a state.json
// migration or a hand-deletion).
func (m mainRow) portCell() string {
	if m.Port == 0 {
		return "—"
	}
	return fmt.Sprintf("%d", m.Port)
}

// lsProject prints the workspaces for a single project — the canonical
// view from inside a project directory. Matches by canonical root path
// (v2) but falls back to basename for legacy v1 rows that haven't been
// migrated yet (e.g. user runs `canopy ls` before running any project-
// scoped command that triggers migration).
func lsProject(ctx context.Context, out io.Writer, projectRoot, projectBasename string) error {
	store, err := openStateReadOnly()
	if err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return err
	}

	rows := []state.Workspace{}
	for _, w := range st.Workspaces {
		// v2 row: match by canonical root path.
		if w.ProjectRoot == projectRoot {
			rows = append(rows, w)
			continue
		}
		// v1 row (no ProjectRoot yet): fall back to basename match. Once
		// migration runs (in workspace.New), this branch becomes dead.
		if w.ProjectRoot == "" && w.Project == projectBasename {
			rows = append(rows, w)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	tc := tmux.New()
	main, mainAlive := mainSessionRow(ctx, tc, st, projectBasename)

	if len(rows) == 0 && !mainAlive {
		fmt.Fprintf(out, "No workspaces in project %q. Run `canopy new` to create one.\n", projectBasename)
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TMUX\tNAME\tBRANCH\tSTATUS\tPORT\tSESSION")
	// Prepend the canopy main row if its tmux session is currently alive.
	// Branch column shows "—" because main doesn't have a single canopy-
	// owned branch; PORT shows the project's reserved base.
	if mainAlive {
		fmt.Fprintf(tw, "●\t(main)\t—\tmain\t%s\t%s\n", main.portCell(), main.Session)
	}
	for _, w := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			liveBadge(ctx, tc, w.TmuxSession),
			w.Name, w.Branch, w.Status, w.Port, w.TmuxSession)
	}
	return tw.Flush()
}

// lsGlobal prints every workspace canopy knows about, grouped by project.
// Used when --all is set or when no canopy.json is discoverable from cwd.
//
// Delegates row assembly to state.BuildGlobalRows so the TUI's GlobalModel
// renders from the same source of truth. CLI output is tabwriter-formatted
// here; the TUI uses lipgloss but consumes identical row data.
func lsGlobal(ctx context.Context, out io.Writer) error {
	store, err := openStateReadOnly()
	if err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return err
	}

	tc := tmux.New()
	rows := st.BuildGlobalRows(ctx, tc)
	if len(rows) == 0 {
		// Distinguish the two empty cases for the user. If state has no
		// projects at all, suggest init. If state has projects but every
		// session is dead and there are no workspaces, suggest canopy new.
		if len(st.Projects) == 0 && len(st.Workspaces) == 0 {
			fmt.Fprintln(out, "No projects. Run `canopy init` + `canopy new` from a project to create one.")
		} else {
			fmt.Fprintln(out, "No workspaces or main sessions. Run `canopy new` or `canopy main` from a project.")
		}
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TMUX\tPROJECT\tNAME\tBRANCH\tSTATUS\tPORT\tSESSION")
	for _, r := range rows {
		badge := "○"
		if r.Alive {
			badge = "●"
		}
		port := "—"
		if r.Port > 0 {
			port = fmt.Sprintf("%d", r.Port)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			badge, r.Project, r.Name, r.Branch, r.Status, port, r.TmuxSession)
	}
	return tw.Flush()
}

// openStateReadOnly returns a state.Store rooted at ~/.canopy. Used for
// listing-only callers that don't need the full workspace.Manager (and
// in particular don't need a canopy.json — the global view works from
// anywhere).
func openStateReadOnly() (*state.Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("ls: home dir: %w", err)
	}
	return state.NewStore(filepath.Join(home, ".canopy"))
}
