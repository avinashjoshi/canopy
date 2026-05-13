package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/lifecycle"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

var lsFlags struct {
	all    bool
	asJSON bool
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
			out := cmd.OutOrStdout()

			// --json forces global mode (all workspaces) and emits a
			// stable JSON document instead of the tab-aligned table.
			// Used by v0.17.0 host.Refresher to fetch a remote host's
			// workspace listing without parsing free-form text. Always
			// global so the remote caller sees the full picture, not
			// just whatever project tower happens to be cwd'd into.
			if lsFlags.asJSON {
				return lsGlobalJSON(cmd.Context(), out)
			}

			var lsErr error
			if global {
				lsErr = lsGlobal(cmd.Context(), out)
			} else {
				lsErr = lsProject(cmd.Context(), out, cfg.ProjectRoot, cfg.Project)
			}
			if lsErr != nil {
				return lsErr
			}
			// Non-blocking auto-check hint. Cache-only read (no
			// network) so ls latency stays unchanged for users who
			// don't run the TUI. The TUI refresh path is what keeps
			// the cache warm; ls just consumes whatever's there.
			printUpgradeHint(out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&lsFlags.all, "all", false,
		"list workspaces across all projects (implicit when no canopy.json is found)")
	cmd.Flags().BoolVar(&lsFlags.asJSON, "json", false,
		"emit a stable JSON document instead of the tabular view (used by v0.17 host.Refresher to read remote workspace state)")
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
		// Match by canonical root path. v1-shaped rows (no ProjectRoot)
		// are dropped — Workspace.Project was removed in v0.15+ so the
		// fallback that used to fire is gone.
		if w.ProjectRoot == projectRoot {
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
			liveBadge(ctx, tc, w.TmuxSessionName()),
			w.Name, w.Branch, w.Status, w.Port, w.TmuxSessionName())
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

// LsJSONOutput is the wire format emitted by `canopy ls --json`. v0.17
// host.Refresher on the LAPTOP parses this struct from each remote
// host's response, so this is effectively a stable cross-version API.
// Schema_version bumps on any backwards-incompatible field change so
// the refresher can detect drift and degrade gracefully.
type LsJSONOutput struct {
	SchemaVersion int                `json:"schema_version"`
	CanopyVersion string             `json:"canopy_version"`
	Hostname      string             `json:"hostname,omitempty"`
	GeneratedAt   string             `json:"generated_at"`
	Workspaces    []LsJSONWorkspace  `json:"workspaces"`
}

// LsJSONWorkspace is the per-row shape. Mirrors state.GlobalRow with
// the fields the refresher actually needs to render a workspace row in
// the laptop TUI. v0.17 Phase 1g added MemRSS, CPU, Hints, and
// LastErrorHint so remote rows reach feature parity with local rows
// (CPU/mem column, PR-status badge, broken-row hint line).
//
// Schema is additive: older laptop clients ignore unknown fields, so
// rolling out a new column doesn't require coordinated upgrades.
type LsJSONWorkspace struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	Branch      string `json:"branch"`
	Status      string `json:"status"`
	Port        int    `json:"port,omitempty"`
	TmuxSession string `json:"tmux_session"`
	Alive       bool   `json:"alive"`

	// MemRSS is the summed resident set size (bytes) across every pane
	// in this row's tmux session. Zero for non-alive rows. Phase 1g.
	MemRSS int64 `json:"mem_rss,omitempty"`

	// CPU is the summed pcpu (single-core normalized). Zero for
	// non-alive rows. Phase 1g.
	CPU float64 `json:"cpu,omitempty"`

	// Hints are lifecycle detector results (rename_suggested, shipped,
	// pr_status, etc). Empty for rows the remote couldn't run detectors
	// against. Phase 1g.
	Hints []state.Hint `json:"hints,omitempty"`

	// LastErrorHint is the auto-detected diagnosis for broken
	// workspaces. Surfaced under the table when the cursor is on a
	// broken row. Empty for healthy rows. Phase 1g.
	LastErrorHint string `json:"last_error_hint,omitempty"`

	// AgentState is the workspace agent pane's classification at
	// emit time: "idle", "thinking", "awaiting_input", or "" (unknown
	// / no agent pane). Single-shot pattern match via
	// agent.ClassifyOneShot — so "thinking" is never set from this
	// path (it requires motion across observations the laptop tracks).
	// The laptop Refresher reads this onto GlobalRow.AgentState which
	// the row renderer uses to populate the badge for remote rows.
	// v0.17 Phase 1d.2.
	AgentState string `json:"agent_state,omitempty"`
}

// canopyVersionInfo is populated at link time by versionCmd.go; for the
// JSON output we fall back to "(unknown)" if not set. The refresher
// uses this to detect version drift between laptop and remote canopy.
var canopyVersionInfo = "(unknown)"

const lsJSONSchemaVersion = 3 // v0.17 Phase 1d.2: + agent_state

func lsGlobalJSON(ctx context.Context, out io.Writer) error {
	store, err := openStateReadOnly()
	if err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return err
	}
	tc := tmux.New()
	// v0.17 Phase 1g: build rows with CPU/mem populated. Hints run
	// after — they need each row's Path+ProjectRoot, which the cheaper
	// BuildGlobalRowsWithLoad path already attaches.
	memCache := state.NewMemCache(state.DefaultMemCacheTTL)
	rows := st.BuildGlobalRowsWithLoad(ctx, tc, lsLoadAdapter{c: tc}, memCache)

	doc := LsJSONOutput{
		SchemaVersion: lsJSONSchemaVersion,
		CanopyVersion: canopyVersionInfo,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Workspaces:    make([]LsJSONWorkspace, 0, len(rows)),
	}
	if host, herr := os.Hostname(); herr == nil {
		doc.Hostname = host
	}
	// Look up per-workspace LastErrorHint from state.json (only set for
	// broken workspaces; healthy rows leave it empty).
	hintByKey := make(map[string]string, len(st.Workspaces))
	for _, w := range st.Workspaces {
		hintByKey[w.ProjectRoot+"|"+w.Name] = w.LastErrorHint
	}
	// v0.17 Phase 1k follow-up: resolve each project's default branch
	// ONCE per project and substitute it for the "—" placeholder on
	// main rows. The laptop's fillMainBranches only fires for local
	// rows; without doing this on the remote too, the laptop renders
	// "(main) ↗ —" for remote projects. DetectDefaultBranch is one
	// git rev-parse — cheap to amortize.
	mainBranchByRoot := make(map[string]string)
	for _, r := range rows {
		if !r.IsMain || r.ProjectRoot == "" {
			continue
		}
		if _, ok := mainBranchByRoot[r.ProjectRoot]; ok {
			continue
		}
		b, err := git.DetectDefaultBranch(ctx, r.ProjectRoot)
		if err != nil || b == "" {
			b = "main" // fallback matches fillMainBranches'
		}
		mainBranchByRoot[r.ProjectRoot] = b
	}
	// v0.17 Phase 1d.2: classify each workspace's agent pane via
	// single-shot pattern matching so the laptop can render the
	// awaiting-input / idle badge on remote rows. One ListAgentPanes
	// call up front; per-pane CapturePane bounded by a tight timeout
	// so a wedged pane can't stall ls --json. Errors fail-open: the
	// row's agent_state stays empty and the laptop renders blank.
	agentStateBySession := classifyAgentPanes(ctx, tc)
	for _, r := range rows {
		var hints []state.Hint
		// Main rows have no Path / no detector input — skip them.
		if !r.IsMain && r.Path != "" {
			ws := state.Workspace{
				Name:        r.Name,
				Branch:      r.Branch,
				Path:        r.Path,
				ProjectRoot: r.ProjectRoot,
				Status:      r.Status,
			}
			hints = lifecycle.RunFast(ctx, ws)
		}
		branch := r.Branch
		if r.IsMain {
			if resolved, ok := mainBranchByRoot[r.ProjectRoot]; ok {
				branch = resolved
			}
		}
		var agentState string
		if s, ok := agentStateBySession[r.TmuxSession]; ok && s != agent.StateUnknown {
			agentState = s.String()
		}
		doc.Workspaces = append(doc.Workspaces, LsJSONWorkspace{
			Name:          r.Name,
			Project:       r.Project,
			Branch:        branch,
			Status:        string(r.Status),
			Port:          r.Port,
			TmuxSession:   r.TmuxSession,
			Alive:         r.Alive,
			MemRSS:        r.MemRSS,
			CPU:           r.CPU,
			Hints:         hints,
			LastErrorHint: hintByKey[r.ProjectRoot+"|"+r.Name],
			AgentState:    agentState,
		})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// lsLoadAdapter wraps *tmux.Client to satisfy state.LoadProbe.
// Mirrors the ui.tmuxLoadAdapter — copied here to avoid importing
// internal/ui from cmd/canopy.
type lsLoadAdapter struct{ c *tmux.Client }

func (a lsLoadAdapter) SessionLoad(ctx context.Context, session string) (state.LoadValue, error) {
	if a.c == nil {
		return state.LoadValue{}, nil
	}
	got, err := a.c.SessionLoad(ctx, session)
	if err != nil {
		return state.LoadValue{}, err
	}
	return state.LoadValue{RSS: got.RSS, CPU: got.CPU}, nil
}

// classifyAgentPanes runs ListAgentPanes + per-pane CapturePane,
// classifies each pane via agent.ClassifyOneShot, and returns a map
// keyed by tmux session name. Used by lsGlobalJSON to stamp each row's
// agent_state. v0.17 Phase 1d.2.
//
// Failure modes are all fail-open — the map just doesn't carry an
// entry for the missing/timed-out pane, and the row's agent_state in
// the JSON output stays empty. Per-pane CapturePane wrapped in a
// 500ms timeout so one wedged pane can't stall the whole emit.
func classifyAgentPanes(ctx context.Context, tc *tmux.Client) map[string]agent.State {
	out := make(map[string]agent.State)
	listCtx, listCancel := context.WithTimeout(ctx, 1*time.Second)
	defer listCancel()
	panes, err := tc.ListAgentPanes(listCtx)
	if err != nil || len(panes) == 0 {
		return out
	}
	for _, p := range panes {
		launcher := agent.LauncherFromRole(p.Role)
		if launcher == "" {
			continue
		}
		capCtx, capCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		content, err := tc.CapturePane(capCtx, p.ID)
		capCancel()
		if err != nil {
			continue
		}
		out[p.Session] = agent.ClassifyOneShot(launcher, content)
	}
	return out
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
