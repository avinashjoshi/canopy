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
			return lsProject(cmd.Context(), cmd.OutOrStdout(), cfg.Project)
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

// lsProject prints the workspaces for a single project — the canonical
// view from inside a project directory.
func lsProject(ctx context.Context, out io.Writer, project string) error {
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
		if w.Project == project {
			rows = append(rows, w)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	if len(rows) == 0 {
		fmt.Fprintf(out, "No workspaces in project %q. Run `canopy new` to create one.\n", project)
		return nil
	}

	tc := tmux.New()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TMUX\tNAME\tBRANCH\tSTATUS\tPORT\tSESSION")
	for _, w := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			liveBadge(ctx, tc, w.TmuxSession),
			w.Name, w.Branch, w.Status, w.Port, w.TmuxSession)
	}
	return tw.Flush()
}

// lsGlobal prints every workspace canopy knows about, grouped by project.
// Used when --all is set or when no canopy.json is discoverable from cwd.
func lsGlobal(ctx context.Context, out io.Writer) error {
	store, err := openStateReadOnly()
	if err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return err
	}
	if len(st.Workspaces) == 0 {
		fmt.Fprintln(out, "No workspaces. Run `canopy new` from a canopy-initialized project to create one.")
		return nil
	}

	// Group + stable-sort by project, then by name within each project.
	rows := append([]state.Workspace{}, st.Workspaces...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Project != rows[j].Project {
			return rows[i].Project < rows[j].Project
		}
		return rows[i].Name < rows[j].Name
	})

	tc := tmux.New()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TMUX\tPROJECT\tNAME\tBRANCH\tSTATUS\tPORT\tSESSION")
	for _, w := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			liveBadge(ctx, tc, w.TmuxSession),
			w.Project, w.Name, w.Branch, w.Status, w.Port, w.TmuxSession)
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
