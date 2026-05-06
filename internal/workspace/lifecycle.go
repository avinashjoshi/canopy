// Package workspace orchestrates canopy's workspace lifecycle: Create,
// Remove, Resurrect, Reconcile.
//
// This is the package that ties everything together. It depends on the
// leaf primitives (git, tmux, hooks, state, namegen, port, config) and
// is itself depended on by the CLI subcommands and (later) the Bubbletea
// TUI. Both frontends share one Manager so behavior is identical
// regardless of how the user invoked canopy.
//
// The lifecycle methods follow a consistent pattern:
//
//  1. Inside state.WithLock: validate (idempotency table), pick port,
//     insert state row with status=setting_up.
//  2. Outside the lock (slow): git worktree add, scripts.setup,
//     tmux session + 4-pane build.
//  3. Inside state.WithLock: flip status to ready (or broken on failure).
//
// Holding the lock through every step would block other canopy invocations
// for the duration of `bundle install`. Splitting into "fast registration"
// + "slow setup" + "fast finalization" lets multiple workspaces set up in
// parallel safely; only the lock-protected windows serialize.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/hooks"
	"github.com/avinashjoshi/canopy/internal/lifecycle"
	"github.com/avinashjoshi/canopy/internal/namegen"
	"github.com/avinashjoshi/canopy/internal/port"
	"github.com/avinashjoshi/canopy/internal/settings"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

var log = clog.Pkg("workspace")

// MaxProjects is the cap on how many distinct projects canopy can track
// concurrently before EnsureProjectBase runs out of base slots. With
// the default 1000 project_stride, 100 projects cover ports 3000-103000
// — far more than any real workflow needs.
const MaxProjects = 100

// Sentinel errors. Tests use errors.Is.
var (
	// ErrWorkspaceExists is returned when Create is called for a name
	// already in state (regardless of status).
	ErrWorkspaceExists = errors.New("workspace: already exists")

	// ErrWorkspaceNotFound is returned by Remove and Resurrect when the
	// workspace name isn't in state.
	ErrWorkspaceNotFound = errors.New("workspace: not found")

	// ErrSetupFailed wraps any failure during scripts.setup. The state
	// row's status flips to broken and last_error captures the chain.
	ErrSetupFailed = errors.New("workspace: setup script failed")

	// ErrCannotRetry is returned by RetrySetup when the workspace is
	// not in `broken` status. Retry is intentionally narrow — only
	// broken workspaces (where scripts.setup failed) can be retried.
	// stopped uses canopy switch (resurrect); orphaned uses canopy rm.
	ErrCannotRetry = errors.New("workspace: retry only applies to broken workspaces")
)

// Manager owns the dependency wiring for workspace operations. Construct
// it once at startup (in main, with config.DiscoverAndLoad output) and
// pass it to every CLI subcommand.
type Manager struct {
	Cfg   *config.Config
	Store *state.Store
	Tmux  *tmux.Client

	// CanopyHome is the directory canopy uses for state, logs, and
	// workspace storage. Defaults to ~/.canopy; tests override.
	// Workspace dirs live at <CanopyHome>/workspaces/<project>/<name>
	// so the source repo stays clean — git worktrees are perfectly happy
	// being created outside the source tree, and centralizing them
	// means canopy "owns" workspace storage instead of polluting every
	// repo with a worktrees/ directory.
	CanopyHome string

	// Settings controls port allocation strategy (base, project stride,
	// workspace stride). Loaded from ~/.canopy/config.json by New() with
	// sensible defaults when the file is absent. Tests override directly.
	Settings settings.Settings
}

// New constructs a Manager from a loaded config. It creates the canopy
// home (~/.canopy) and its state directory if missing, and loads the
// per-machine settings (~/.canopy/config.json) — Default()s apply when
// no config file exists.
func New(cfg *config.Config) (*Manager, error) {
	home, err := canopyHome()
	if err != nil {
		return nil, err
	}
	store, err := state.NewStore(home)
	if err != nil {
		return nil, err
	}
	st, err := settings.Load(home)
	if err != nil {
		return nil, fmt.Errorf("workspace.New: settings: %w", err)
	}
	m := &Manager{
		Cfg:        cfg,
		Store:      store,
		Tmux:       tmux.New(),
		CanopyHome: home,
		Settings:   st,
	}
	// Run the v1→v2 migration + basename-uniqueness gate up front so every
	// subsequent operation sees a v2-shaped state. If the user's state has
	// a basename collision (rare; would only happen if they manually edited
	// state.json), refuse to construct the Manager — it's safer to surface
	// "two projects share a name" once at startup than to silently corrupt
	// data on the next workspace mutation.
	if err := m.migrateAndGuard(); err != nil {
		return nil, err
	}
	return m, nil
}

// ErrBasenameCollision is returned when state already has a different
// project registered with the same basename as cfg.ProjectRoot. The user
// must rename one of the directories (or hand-edit state.json) before
// canopy will operate on either project.
var ErrBasenameCollision = errors.New("workspace: project basename collides with another registered project")

// migrateAndGuard runs the v1→v2 schema migration for this project under
// the state lock, then checks for basename collisions. Idempotent — safe
// to call on every Manager.New, including when state is already v2.
//
// On collision, returns ErrBasenameCollision wrapped with both colliding
// root paths in the error message. On success, state.json on disk is
// guaranteed to have a v2-shaped entry for this project (with Root field
// populated and a PortBase that may or may not be allocated yet — port
// allocation still happens lazily in Create's first phase).
func (m *Manager) migrateAndGuard() error {
	return m.Store.WithLock(func(s *state.State) error {
		s.MigrateLegacyProject(m.Cfg.Project, m.Cfg.ProjectRoot)
		if other := s.FindBasenameCollision(m.Cfg.ProjectRoot); other != "" {
			return fmt.Errorf(
				"%w: %q (basename %q) collides with already-registered project at %q. "+
					"Rename one of the directories so basenames are unique, then retry.",
				ErrBasenameCollision, m.Cfg.ProjectRoot, m.Cfg.Project, other)
		}
		return nil
	})
}

// workspacesDir returns <CanopyHome>/workspaces/<project>. Created on
// demand by Create; safe to call before the dir exists.
func (m *Manager) workspacesDir() string {
	return filepath.Join(m.CanopyHome, "workspaces", m.Cfg.Project)
}

// canopyHome returns the directory canopy uses for state and logs.
// ~/.canopy by default; tests override by constructing Manager directly.
func canopyHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("workspace.canopyHome: %w", err)
	}
	return filepath.Join(home, ".canopy"), nil
}

// CreateOptions configures Manager.Create's source variant. Zero-value
// means "fresh" — random name, branch off origin/<default>, no source
// metadata. The opts struct lets canopy new --pr / --issue / --branch
// thread custom branches and source-context briefings through Create
// without a separate per-variant API.
//
// Mutual exclusion: at most one of SourceKind ("pr", "issue", "branch")
// should be set. The CLI gates this; Create itself doesn't enforce
// because internal callers may legitimately compose options that don't
// fit a single SourceKind.
type CreateOptions struct {
	// SourceKind ends up on the persisted Workspace row and drives
	// the briefing variant. Empty/"fresh" → ordinary new-workspace
	// flow. "pr"/"issue"/"branch" — see SourceContext below for the
	// corresponding briefing data.
	SourceKind string

	// SourceContext is the body text rendered into the agent
	// briefing's "Source context" section, wrapped in data-not-
	// instructions delimiters. For PRs/issues this is the upstream
	// body; for branch this is empty and the agent falls back to the
	// "read git log" instruction in sourceKindBlock.
	SourceContext string

	// Branch overrides the workspace's local branch name. When empty,
	// Create defaults to the workspace name (legacy v0 behavior).
	// Set this for canopy new --pr (use the PR's headRefName) or
	// canopy new --branch (use the supplied branch).
	Branch string

	// StartPoint overrides the git ref that the new worktree branches
	// from. When empty, Create defaults to origin/<default> (current
	// behavior). Set to a specific ref ("origin/feature/oauth",
	// "canopy/pr-42") to base the worktree on existing work.
	StartPoint string

	// CreateBranch controls whether `git worktree add` should create
	// a new branch (-b) or check out an existing one. true = create
	// new (-b Branch StartPoint); false = check out existing
	// (StartPoint must already be a branch ref). Defaults to true to
	// preserve legacy behavior for callers that don't set it.
	CreateBranch bool
}

// Create runs the full workspace setup lifecycle. name may be empty to
// have the manager generate a random name via namegen. opts customizes
// source variant + branch routing; zero-value opts gives the ordinary
// "fresh workspace off origin/<default>" flow.
//
// Output from scripts.setup streams to stdout/stderr live so the caller
// (CLI or TUI) can show progress.
func (m *Manager) Create(ctx context.Context, name string, opts CreateOptions, stdout, stderr io.Writer) (*state.Workspace, error) {
	if stdout == nil || stderr == nil {
		return nil, fmt.Errorf("workspace.Create: stdout and stderr writers required")
	}

	// Phase 1: register the workspace under state.WithLock. This computes
	// the name (if missing), allocates a port, and inserts a row with
	// status=setting_up. After this returns, OTHER processes can see the
	// workspace exists and won't pick the same port.
	//
	// nameWasEmpty captures whether the caller passed "" before namegen
	// runs — used downstream to flag the workspace as auto-named, which
	// in turn drives the briefing's rename directive.
	nameWasEmpty := name == ""

	var ws state.Workspace
	err := m.Store.WithLock(func(s *state.State) error {
		// If no name supplied, generate one. Skip names already in state.
		if name == "" {
			used := make([]string, 0, len(s.Workspaces))
			for _, w := range s.Workspaces {
				if w.ProjectRoot == m.Cfg.ProjectRoot {
					used = append(used, w.Name)
				}
			}
			generated, ok := namegen.Unique(used)
			if !ok {
				return fmt.Errorf("workspace.Create: namegen wordlist exhausted (%d workspaces)", len(used))
			}
			name = generated
		}

		// Idempotency check: existing row -> ErrWorkspaceExists.
		if _, err := s.Find(m.Cfg.ProjectRoot, name); err == nil {
			return fmt.Errorf("workspace.Create(%s): %w", name, ErrWorkspaceExists)
		}

		// Port allocation strategy: each project gets its own block. The
		// project base is the same forever (assigned first time canopy
		// sees the project, persisted in state.Projects keyed by canonical
		// root path). Workspaces within the project pick the smallest free
		// slot at offset project_base + N×workspace_stride — typically a
		// stride of 10 so adjacent ports per workspace are reserved for
		// sidecars (Rails + Sidekiq + Redis, etc.).
		ports := m.Settings.Ports
		projectBase, isNew, err := s.EnsureProjectBase(
			m.Cfg.ProjectRoot, ports.Base, ports.ProjectStride, MaxProjects)
		if err != nil {
			return fmt.Errorf("workspace.Create(%s): %w", name, err)
		}
		if isNew {
			log.Info("workspace.create.project-registered",
				"project", m.Cfg.Project, "root", m.Cfg.ProjectRoot, "port_base", projectBase)
		}
		// Used ports across the WHOLE state — workspaces in other projects
		// shouldn't collide either, even though stride math makes that rare.
		used := make([]int, 0, len(s.Workspaces))
		for _, w := range s.Workspaces {
			used = append(used, w.Port)
		}
		// Allocate within this project's block, starting at base+stride
		// (the base itself is RESERVED for `canopy main` — the main-branch
		// session). Workspaces from canopy new walk base+10, base+20, ...
		// up to just before the next project's base.
		p, err := port.Allocate(
			projectBase+ports.WorkspaceStride,
			projectBase+ports.ProjectStride-1,
			ports.WorkspaceStride,
			used,
		)
		if err != nil {
			return fmt.Errorf("workspace.Create(%s): %w", name, err)
		}

		// Compute paths and tmux session identifier. Workspace dirs live
		// in canopy's home, NOT inside the source repo — the repo stays
		// clean and canopy "owns" workspace storage.
		//
		// Use git.Sanitize for the on-disk path component (allows dots,
		// matches typical git/repo conventions) but tmux.SafeName for the
		// tmux session name (must NOT contain dots or colons, which are
		// tmux's target-syntax separators).
		// Branch defaults to the workspace name (the v0 "branch ==
		// name" rule). canopy new --pr / --branch override this so the
		// local branch matches the PR head or the user-supplied name.
		branch := name
		if opts.Branch != "" {
			branch = opts.Branch
		}
		safeName := git.Sanitize(name)
		wsPath := filepath.Join(m.workspacesDir(), safeName)
		// Tmux session name is computed on demand from project + branch
		// via state.Workspace.TmuxSessionName(). At create time, branch
		// and name usually match (auto-generated names), so the initial
		// session name reads as `<project>-<branch>` straight away.
		// Subsequent `git branch -m` calls + SyncBranch transparently
		// rename the live tmux session (the computed value moves; old
		// stored field gone in v0.15+).

		// Default SourceKind to "fresh" so v0.6+ rows always carry an
		// explicit kind (older "fresh" was implicit). SourceContext
		// stays whatever the caller passed; empty for fresh/branch,
		// non-empty for pr/issue.
		sourceKind := opts.SourceKind
		if sourceKind == "" {
			sourceKind = "fresh"
		}

		ws = state.Workspace{
			ProjectRoot:       m.Cfg.ProjectRoot, // v2 authoritative key (basename derived via ProjectBasename())
			Name:              name,
			Branch:            branch,
			Path:              wsPath,
			Port:              p,
			Status:            state.StatusSettingUp,
			CreatedAt:         time.Now().UTC(),
			SourceKind:        sourceKind,
			SourceContext:     opts.SourceContext,
			NameAutoGenerated: nameWasEmpty,
		}
		return s.Add(ws)
	})
	if err != nil {
		return nil, err
	}

	log.Info("workspace.create.registered",
		"project", ws.ProjectBasename(), "name", ws.Name, "path", ws.Path, "port", ws.Port)

	// Phase 2: slow operations outside the lock. If any of these fail,
	// the workspace transitions to status=broken via the helper below.
	// Tee stderr through a buffer so we can run Diagnose() against it
	// when scripts.setup fails — the user still sees stderr live, AND
	// the buffer feeds the auto-hint registry.
	var capturedStderr bytes.Buffer
	teedStderr := io.MultiWriter(stderr, &capturedStderr)
	setupErr := m.runSetup(ctx, &ws, opts, stdout, teedStderr)
	if setupErr != nil {
		_ = m.markBroken(&ws, setupErr, capturedStderr.Bytes())
		return &ws, setupErr
	}

	// Phase 3: flip to ready under the lock + bump AgentLaunchCount
	// (the agent pane was just spawned in buildSession's runSetup).
	// AgentLaunchCount drives the v0.6 hybrid briefing strategy:
	// count==0 → fresh briefing; count>0 → delta-only on the next launch.
	err = m.Store.WithLock(func(s *state.State) error {
		row, err := s.Find(ws.ProjectRoot, ws.Name)
		if err != nil {
			return err
		}
		row.Status = state.StatusReady
		row.LastError = ""
		row.AgentLaunchCount++
		ws = *row
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspace.Create: finalize: %w", err)
	}

	log.Info("workspace.create.ready", "name", ws.Name, "agent_launch_count", ws.AgentLaunchCount)
	return &ws, nil
}

// runSetup executes the slow lifecycle steps: git worktree add,
// scripts.setup, and the 4-pane tmux session build. Any failure propagates
// up to Create which marks the workspace broken.
//
// opts threads source-variant routing into the worktree creation:
// when opts.StartPoint is set we use that ref instead of origin/<default>,
// and when opts.CreateBranch is false we check out the existing branch
// (no -b) so callers can resurrect a remote branch into a workspace
// without forking a new local branch off it.
func (m *Manager) runSetup(ctx context.Context, ws *state.Workspace, opts CreateOptions, stdout, stderr io.Writer) error {
	// Ensure parent dir exists. git worktree add creates the leaf dir
	// itself but won't mkdir intermediate parents.
	parent := filepath.Dir(ws.Path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("workspace.runSetup: mkdir %s: %w", parent, err)
	}

	// 1. git worktree add. Two cases:
	//
	//   - Fresh workspace (opts.StartPoint == ""): branch off
	//     origin/<default> so new workspaces start from the latest
	//     pushed code. Fetch first (best-effort); fall through to
	//     local HEAD if both fetch and default-branch detection fail.
	//   - Source-flag workspace (opts.StartPoint != ""): use the
	//     caller-supplied ref. canopy new --pr passes "canopy/pr-<num>"
	//     (a local fetched ref); --branch passes "origin/<name>" or the
	//     local branch name.
	startPoint := opts.StartPoint
	createNewBranch := opts.CreateBranch
	if startPoint == "" {
		// Fresh path. Default to origin/<default>; fall through on miss.
		if defaultBranch, err := git.DetectDefaultBranch(ctx, m.Cfg.ProjectRoot); err == nil {
			if ferr := git.Fetch(ctx, m.Cfg.ProjectRoot, "origin"); ferr != nil {
				log.Warn("workspace.create.fetch-failed", "err", ferr)
				fmt.Fprintf(stderr, "warning: git fetch origin failed: %v\n", ferr)
				fmt.Fprintf(stderr, "  proceeding with the local copy of origin/%s\n", defaultBranch)
			}
			startPoint = "origin/" + defaultBranch
		}
		// Fresh workspaces always create a new branch (the workspace
		// name == branch name); zero-value opts.CreateBranch was false
		// historically, but that's the wrong default for the legacy
		// path. Force true here.
		createNewBranch = true
		if startPoint != "" {
			fmt.Fprintf(stdout, "Basing %s on %s\n", ws.Branch, startPoint)
		}
	} else {
		fmt.Fprintf(stdout, "Basing %s on %s\n", ws.Branch, startPoint)
	}

	if createNewBranch {
		if err := git.Add(ctx, m.Cfg.ProjectRoot, ws.Branch, ws.Path, startPoint); err != nil {
			return fmt.Errorf("workspace.runSetup: git: %w", err)
		}
	} else {
		// Check out an existing branch into the worktree (no -b).
		// startPoint here is already a branch ref (origin/<name> or a
		// local branch); git.AddExisting wraps `git worktree add <path>
		// <branch>`.
		if err := git.AddExisting(ctx, m.Cfg.ProjectRoot, startPoint, ws.Path); err != nil {
			return fmt.Errorf("workspace.runSetup: git: %w", err)
		}
	}

	// 2 + 3: setup script + tmux session. Extracted so RetrySetup can
	// invoke them on an already-existing worktree without redoing git.
	return m.runSetupHooksOnly(ctx, ws, stdout, stderr)
}

// runSetupHooksOnly runs scripts.setup (if configured) then builds the
// tmux session. Shared between Create (after git worktree add) and
// RetrySetup (where the worktree already exists). Caller is responsible
// for ensuring ws.Path is a real, ready-to-use directory.
//
// Setup output is tee'd to ~/.canopy/log/setup-<workspace>.log via
// io.MultiWriter so the diagnostic drawer can show it later when the
// user is staring at a `broken` workspace asking "why?". The tee is
// best-effort: if the file open fails (permission, disk full), the
// hook still runs to completion, just without the disk capture.
func (m *Manager) runSetupHooksOnly(ctx context.Context, ws *state.Workspace, stdout, stderr io.Writer) error {
	if m.Cfg.Scripts.Setup != "" {
		setupLog, closeLog := openSetupLog(ws.Name)
		defer closeLog()
		runStdout := stdout
		runStderr := stderr
		if setupLog != nil {
			runStdout = io.MultiWriter(stdout, setupLog)
			runStderr = io.MultiWriter(stderr, setupLog)
		}
		scriptPath := filepath.Join(m.Cfg.ProjectRoot, m.Cfg.Scripts.Setup)
		if err := hooks.Run(ctx, scriptPath, hooks.Options{
			Cwd:    ws.Path,
			Env:    hooks.WorkspaceEnv(ws.Path, m.Cfg.ProjectRoot, ws.Port),
			Stdout: runStdout,
			Stderr: runStderr,
		}); err != nil {
			return fmt.Errorf("workspace.runSetupHooksOnly: %w: %v", ErrSetupFailed, err)
		}
	} else {
		fmt.Fprintln(stdout, "(no scripts.setup configured; skipping)")
	}

	// Build the tmux session if not already alive. Retry on a broken
	// workspace usually has no tmux session yet (setup failed before
	// the buildSession step), but we check defensively in case a future
	// failure mode leaves a half-built session lying around.
	alive, err := m.Tmux.HasSession(ctx, ws.TmuxSessionName())
	if err != nil {
		return fmt.Errorf("workspace.runSetupHooksOnly: tmux check: %w", err)
	}
	if alive {
		return nil
	}
	if err := m.buildSession(ctx, ws); err != nil {
		return fmt.Errorf("workspace.runSetupHooksOnly: tmux: %w", err)
	}
	return nil
}

// RetrySetup re-runs scripts.setup on an existing broken workspace.
// Used to recover from a transient or fixable setup failure (missing
// credential, network blip, dependency conflict that the user has now
// resolved) without losing the worktree, branch, port, or claude
// conversation history that an alternative canopy rm + canopy new
// cycle would discard.
//
// Only allowed on workspaces with status=broken. Other statuses get
// ErrCannotRetry — stopped uses canopy switch to resurrect; orphaned
// (dir missing) uses canopy rm; setting_up means a retry is already
// in flight.
//
// Output from scripts.setup streams to stdout/stderr. On success, the
// status flips to ready and last_error is cleared. On failure, the
// status stays broken with last_error updated to reflect the new
// failure (the retry replaces, not appends, the error chain).
func (m *Manager) RetrySetup(ctx context.Context, name string, force bool, stdout, stderr io.Writer) (*state.Workspace, error) {
	if stdout == nil || stderr == nil {
		return nil, fmt.Errorf("workspace.RetrySetup: stdout and stderr writers required")
	}

	// Phase 1: validate + flip to setting_up under the lock so a parallel
	// retry on the same name fast-fails.
	var ws state.Workspace
	err := m.Store.WithLock(func(s *state.State) error {
		row, err := s.Find(m.Cfg.ProjectRoot, name)
		if err != nil {
			return fmt.Errorf("workspace.RetrySetup(%s): %w", name, ErrWorkspaceNotFound)
		}
		// setting_up always refuses, regardless of force. Two concurrent
		// setup hooks on the same worktree would race over the dir, the
		// port, and the agent briefing file. Always wait for the in-flight
		// setup to finish before retrying.
		if row.Status == state.StatusSettingUp {
			return fmt.Errorf("workspace.RetrySetup(%s): another setup is in progress (status=setting_up); wait for it to finish",
				name)
		}
		// orphaned always refuses, regardless of force. The dir doesn't
		// exist; setup has nothing to run against. Right verb is canopy rm
		// to drop the row, then canopy new to recreate.
		if row.Status == state.StatusOrphaned {
			return fmt.Errorf("workspace.RetrySetup(%s): workspace is orphaned (no on-disk dir); run canopy rm",
				name)
		}
		// broken is the unconditional retry path (the v0 contract). Other
		// statuses (ready, stopped) require force=true — re-running setup
		// on a healthy workspace can be destructive depending on what
		// scripts.setup does (DB drops, port reservations, agent briefing
		// files, etc.). The user has to opt in.
		if row.Status != state.StatusBroken && !force {
			return fmt.Errorf("workspace.RetrySetup(%s) in status %q: %w",
				name, row.Status, ErrCannotRetry)
		}
		// Defensive: if the dir vanished between original Create and now,
		// the user should canopy rm — we can't run setup against nothing.
		if _, err := os.Stat(row.Path); os.IsNotExist(err) {
			return fmt.Errorf("workspace.RetrySetup(%s): workspace dir missing at %s; run canopy rm",
				name, row.Path)
		}
		row.Status = state.StatusSettingUp
		row.LastError = ""
		ws = *row
		return nil
	})
	if err != nil {
		return nil, err
	}

	log.Info("workspace.retry.start",
		"project", ws.ProjectBasename(), "name", ws.Name, "path", ws.Path, "port", ws.Port)
	start := time.Now()

	// Phase 2: re-run hooks + tmux build (slow, no lock held). Same
	// stderr-tee trick as Create so Diagnose() gets fed if hooks fail.
	var capturedStderr bytes.Buffer
	teedStderr := io.MultiWriter(stderr, &capturedStderr)
	hookErr := m.runSetupHooksOnly(ctx, &ws, stdout, teedStderr)
	if hookErr != nil {
		_ = m.markBroken(&ws, hookErr, capturedStderr.Bytes())
		log.Info("workspace.retry.failure",
			"name", ws.Name, "err", hookErr.Error(), "duration_ms", time.Since(start).Milliseconds())
		return &ws, hookErr
	}

	// Phase 3: flip to ready under the lock.
	err = m.Store.WithLock(func(s *state.State) error {
		row, err := s.Find(ws.ProjectRoot, ws.Name)
		if err != nil {
			return err
		}
		row.Status = state.StatusReady
		row.LastError = ""
		ws = *row
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspace.RetrySetup: finalize: %w", err)
	}

	log.Info("workspace.retry.success",
		"name", ws.Name, "duration_ms", time.Since(start).Milliseconds())
	return &ws, nil
}

// buildSession creates the tmux session and lays out canopy's standard
// 3-pane workspace, modeled on omarchy's `tdl` layout:
//
//	+-------------------------+--------+
//	|                         |        |
//	|         nvim            | claude |
//	|        (~70%)           | (~30%) |
//	|                         |        |
//	+-------------------------+--------+
//	|             shell  (~15%)        |
//	+----------------------------------+
//
// scripts.run is NOT launched automatically. Most of the time the user
// wants a terminal, not a permanent server pane — when they need to run
// the server, they type the command themselves (or future versions will
// add `canopy run` to invoke it on demand).
//
// CANOPY_* env vars are set at session level (via tmux -e), so every
// pane (nvim's wrapped sh, claude's wrapped sh, the bare shell, plus
// any panes the user creates later with prefix-c) sees CANOPY_PORT
// etc. without canopy doing per-pane plumbing.
//
// Layout sequence (all splits use -d so the active pane stays on the
// initial nvim pane and subsequent splits target it):
//  1. new-session: pane 0 = nvim, full window.
//  2. split-v with -l 15%: pane 1 = shell, 15% of window height at the
//     bottom. nvim becomes the top 85%, full-width.
//  3. split-h with -l 30%: pane 2 = claude, 30% of nvim's width on the
//     right. nvim becomes top-left ~70%.
//
// nvim and claude are wrapped in keepAlive so :q from nvim or claude
// ending drops the pane to a shell instead of closing it.
func (m *Manager) buildSession(ctx context.Context, ws *state.Workspace) error {
	env := hooks.WorkspaceEnv(ws.Path, m.Cfg.ProjectRoot, ws.Port)
	if err := m.Tmux.Create(ctx, ws.TmuxSessionName(), ws.Path, keepAlive("nvim ."), env...); err != nil {
		return err
	}
	// Shell, ~15% of window height, full-width bottom strip.
	if err := m.Tmux.SplitPane(ctx, ws.TmuxSessionName(), ws.Path, "", tmux.SplitVertical, 15); err != nil {
		return err
	}
	// Agent pane, ~30% of the top pane's width on the right. Uses
	// canopy.json's agent.type to pick the launcher (claude by default;
	// codex/opencode/aider supported); briefing is built from workspace
	// state + canopy lifecycle conventions and handed to the agent
	// inline (claude/codex), via temp file (aider), or via AGENTS.md
	// in the worktree (opencode).
	agentCmd, err := m.agentPaneCmd(ws, false /* fresh: AgentLaunchCount==0 */)
	if err != nil {
		return fmt.Errorf("workspace.buildSession: agent pane: %w", err)
	}
	if err := m.Tmux.SplitPane(ctx, ws.TmuxSessionName(), ws.Path, keepAlive(agentCmd), tmux.SplitHorizontal, 30); err != nil {
		return err
	}
	// Land the active pane on the agent (claude) pane — that's the
	// thing the user wants to interact with first after `n`. Splits
	// above all use -d (active stays on nvim), so we explicitly move
	// right to the just-created agent pane. Best-effort: a select-
	// pane failure shouldn't tear down workspace creation.
	if err := m.Tmux.SelectPaneDirection(ctx, ws.TmuxSessionName(), "R"); err != nil {
		log.Warn("workspace.build.select-agent-pane-failed", "session", ws.TmuxSessionName(), "err", err.Error())
	}
	return nil
}

// agentPaneCmd assembles the shell command for the workspace's agent
// pane. resume=false means "fresh launch" (AgentLaunchCount==0 path —
// full briefing); resume=true means "resurrect" (hybrid strategy —
// hints-only delta or no briefing flag if no hints active).
//
// Sequence:
//
//  1. Resolve the launcher from canopy.json's agent.type
//     (default "claude"). Returns ErrUnknownAgent for typos.
//  2. Verify the agent binary is on PATH. Surfaces a clean error with
//     install hint instead of a cryptic shell failure inside the pane.
//  3. Build the briefing string via agent.BuildBriefing (handles the
//     hybrid fresh-vs-resume strategy).
//  4. Write briefing to a temp file (or skip if empty — happens on
//     resume + no active hints; the launcher then drops the briefing
//     flag entirely).
//  5. Build the shell command via launcher.PlanLaunch, prepending any
//     PreRun (e.g., the cp-to-AGENTS.md for opencode).
//
// Detector hints are read once at this call's time. Future detector
// state changes between this call and the agent actually launching are
// not reflected — that's acceptable; the agent gets a fresh briefing
// on every relaunch.
func (m *Manager) agentPaneCmd(ws *state.Workspace, resume bool) (string, error) {
	launcher, err := agent.Resolve(m.Cfg.Agent.Type)
	if err != nil {
		// Typo in canopy.json's agent.type — fail loud. This is a
		// config error, not an environment gap; we don't want to
		// silently fall back when the user wrote "cluade" instead
		// of "claude".
		return "", err
	}
	if err := launcher.VerifyInstalled(); err != nil {
		// Binary not on PATH — degrade gracefully. The user gets a
		// shell in the agent pane with an install hint so they
		// know what's missing. Workspace creation continues; the
		// other 3 panes (nvim / server / shell) work normally.
		//
		// Why not fail: a fresh canopy install with no agents yet
		// installed should still produce working workspaces. Hard
		// failure here would block the whole onboarding path on a
		// secondary feature. The hint in the pane keeps the missing
		// dependency visible without making it a blocker.
		log.Warn("agent.binary-missing", "type", m.Cfg.Agent.Type, "err", err)
		return agentFallbackShell(launcher.Cmd, err), nil
	}

	// Detector hints. RunFast is the cheap-only set (rename + shipped);
	// pr_status is excluded here because shelling to gh during workspace
	// creation is the wrong place for that latency. The next reconcile /
	// TUI refresh will pick it up.
	hints := lifecycle.RunFast(context.Background(), *ws)

	briefing := agent.BuildBriefing(*ws, m.Cfg, hints)

	// Write briefing to a temp file. Empty briefing → empty path → the
	// launcher drops the flag entirely (handled in PlanLaunch).
	briefingPath := ""
	if briefing != "" {
		path, err := writeBriefingTemp(briefing, ws.Name)
		if err != nil {
			return "", fmt.Errorf("write briefing temp file: %w", err)
		}
		briefingPath = path
	}

	plan := launcher.PlanLaunch(briefingPath, resume, ws.Path)
	cmd := plan.ShellCommand
	if plan.PreRun != "" {
		cmd = plan.PreRun + " && " + cmd
	}
	return cmd, nil
}

// agentFallbackShell returns the command to run in the agent pane
// when the configured agent binary isn't on PATH. Prints a short
// hint identifying what's missing, then drops into the user's
// default shell so the pane stays usable. The user can install
// the agent and `canopy switch` to relaunch with the real agent.
//
// Format: a single shell line that echoes the hint, prints a
// blank line, then exec's $SHELL. exec replaces the wrapper so
// the pane behaves like a regular shell after the hint scrolls
// off-screen.
func agentFallbackShell(cmdName string, lookupErr error) string {
	hint := fmt.Sprintf(
		"agent %q not on PATH — falling back to a shell. %v",
		cmdName, lookupErr)
	// Single-quote the message so embedded $ / backticks don't
	// expand. Escape any internal single-quotes via the standard
	// '\'' POSIX trick.
	quoted := "'" + strings.ReplaceAll(hint, "'", `'\''`) + "'"
	return fmt.Sprintf(`echo %s; echo; exec "${SHELL:-/bin/sh}"`, quoted)
}

// writeBriefingTemp writes the briefing string to a temp file under
// ~/.canopy/tmp/. Returns the absolute path. The temp file persists
// after the agent reads it (the agent might re-read mid-session); we
// rely on system tmp cleanup to evict eventually.
//
// Filename pattern: agent-briefing-<workspace>-<random>.md so a `ls
// ~/.canopy/tmp/` shows which workspace each briefing belongs to.
func writeBriefingTemp(briefing, workspaceName string) (string, error) {
	tmpDir := filepath.Join(canopyHomeOrFallback(), "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	pattern := "agent-briefing-" + workspaceName + "-*.md"
	f, err := os.CreateTemp(tmpDir, pattern)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(briefing); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// canopyHomeOrFallback returns ~/.canopy or os.TempDir() as a last
// resort. canopy.Manager already has CanopyHome on it, but this helper
// is called from agentPaneCmd which has access to *Manager — refactor:
// take the manager. Simpler.
func canopyHomeOrFallback() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return filepath.Join(home, ".canopy")
}

// keepAlive wraps a shell command so that when the command exits (cleanly
// or otherwise), the pane drops to the user's shell instead of closing.
// Returns "" unchanged so the caller can pass it straight through to
// tmux.Create / tmux.SplitPane (empty -> default shell, naturally
// persistent).
//
// The exec keeps the process count at one — exec $SHELL replaces the sh
// process rather than spawning a child, so `ps` inside the pane shows
// just the shell.
func keepAlive(cmd string) string {
	if cmd == "" {
		return ""
	}
	// Single quotes inside double quotes is fine for tmux's `sh -c <cmd>`
	// since the whole thing is one shell-c argument. The user's $SHELL is
	// expanded by sh at runtime, not by Go.
	return cmd + `; exec "$SHELL"`
}

// markBroken flips a workspace's status to broken and persists the error
// chain. Best-effort: if state.WithLock itself fails, the in-memory
// workspace is still left in the broken state for the caller's report.
//
// capturedStderr is the (potentially empty) bytes captured from
// scripts.setup's stderr. It's fed to Diagnose() to attach a one-line
// user-facing hint to the row when canopy recognizes the failure
// signature. Pass nil/empty if no stderr was captured — Diagnose
// short-circuits to "" and the row's LastErrorHint stays empty.
func (m *Manager) markBroken(ws *state.Workspace, cause error, capturedStderr []byte) error {
	hint := Diagnose(capturedStderr)
	return m.Store.WithLock(func(s *state.State) error {
		row, err := s.Find(ws.ProjectRoot, ws.Name)
		if err != nil {
			return err
		}
		row.Status = state.StatusBroken
		row.LastError = cause.Error()
		row.LastErrorHint = hint
		ws.Status = state.StatusBroken
		ws.LastError = cause.Error()
		ws.LastErrorHint = hint
		return nil
	})
}

// Remove tears down a workspace: drop the state row first, then run
// scripts.archive, tmux kill, git worktree remove, branch delete, and
// log cleanup against the in-memory snapshot. Failures in archive,
// tmux, or git are logged but don't block removal — a half-removed
// workspace is worse than a best-effort full removal.
//
// The state row drops at step 1 (right after we snapshot the row to
// wsCopy) so concurrent canopy invocations don't see a stale row
// during the slow archive/git/tmux work — important for the popup-
// detach flow where the user closes canopy immediately after pressing
// `y` and reopens it before the detached subprocess has finished.
// Steps 2-5 work entirely off wsCopy, so they don't need the row to
// still exist. Failures after the drop leave residue on disk
// (worktree, branch) but no state row; `canopy reconcile` discovers
// the orphan and the user can clean up manually if needed. This is
// strictly better than the prior "row hangs around as zombie" UX.
func (m *Manager) Remove(ctx context.Context, name string, stdout, stderr io.Writer) error {
	if stdout == nil || stderr == nil {
		return fmt.Errorf("workspace.Remove: stdout and stderr writers required")
	}

	// Snapshot + drop the state row up front. Steps 2+ don't touch
	// state — they work off wsCopy.
	var wsCopy state.Workspace
	if err := m.Store.WithLock(func(s *state.State) error {
		ws, err := s.Find(m.Cfg.ProjectRoot, name)
		if err != nil {
			return fmt.Errorf("workspace.Remove(%s): %w", name, ErrWorkspaceNotFound)
		}
		wsCopy = *ws
		return s.Remove(m.Cfg.ProjectRoot, name)
	}); err != nil {
		return err
	}

	// 2. scripts.archive — log failure but proceed. Empty -> skip.
	if m.Cfg.Scripts.Archive != "" {
		scriptPath := filepath.Join(m.Cfg.ProjectRoot, m.Cfg.Scripts.Archive)
		if err := hooks.Run(ctx, scriptPath, hooks.Options{
			Cwd:    wsCopy.Path,
			Env:    hooks.WorkspaceEnv(wsCopy.Path, m.Cfg.ProjectRoot, wsCopy.Port),
			Stdout: stdout,
			Stderr: stderr,
		}); err != nil {
			log.Warn("workspace.remove.archive-failed", "name", name, "err", err)
			fmt.Fprintf(stderr, "warning: archive script failed: %v\n", err)
		}
	}

	// 3. tmux kill — log failure but proceed.
	if err := m.Tmux.Kill(ctx, wsCopy.TmuxSessionName()); err != nil && !errors.Is(err, tmux.ErrSessionNotFound) {
		log.Warn("workspace.remove.tmux-failed", "name", name, "err", err)
	}

	// 4. git worktree remove with --force (we already accept that user's
	// uncommitted changes might exist; canopy rm is the explicit "drop
	// this" command).
	if err := git.Remove(ctx, m.Cfg.ProjectRoot, wsCopy.Path, true); err != nil &&
		!errors.Is(err, git.ErrPathNotFound) {
		log.Warn("workspace.remove.git-failed", "name", name, "err", err)
		fmt.Fprintf(stderr, "warning: git worktree remove failed: %v\n", err)
	}

	// 5. Delete the underlying branch. Canopy's workspaces are ephemeral
	// by design — leaving the branch behind every `canopy rm` would
	// pile up dead branches the user has to clean by hand. force=true
	// because the branch may have unmerged work and the user explicitly
	// asked for removal anyway.
	if err := git.DeleteBranch(ctx, m.Cfg.ProjectRoot, wsCopy.Branch, true); err != nil {
		log.Warn("workspace.remove.branch-delete-failed", "name", name, "branch", wsCopy.Branch, "err", err)
		fmt.Fprintf(stderr, "warning: failed to delete branch %s: %v\n", wsCopy.Branch, err)
	}

	// 6. Per-workspace logs (setup output + the workspace-scoped slog
	// fan-out file). Best-effort; missing files aren't errors.
	removeSetupLog(name)
	if err := clog.RemoveWorkspaceLog(name); err != nil {
		log.Warn("workspace.remove.workspace-log-failed", "name", name, "err", err)
	}
	return nil
}

// Resurrect rebuilds the tmux session for a workspace whose dir is on
// disk but whose tmux session has died (status: stopped, typical after
// a laptop reboot). It does NOT re-run scripts.setup — the database,
// dependencies, etc. are already in place. Per-directory claude history
// is preserved automatically (claude --continue resumes).
func (m *Manager) Resurrect(ctx context.Context, name string) (*state.Workspace, error) {
	st, err := m.Store.Load()
	if err != nil {
		return nil, fmt.Errorf("workspace.Resurrect: load: %w", err)
	}
	ws, err := st.Find(m.Cfg.ProjectRoot, name)
	if err != nil {
		return nil, fmt.Errorf("workspace.Resurrect(%s): %w", name, ErrWorkspaceNotFound)
	}
	wsCopy := *ws

	// If the workspace dir is gone (orphaned), refuse to resurrect — the
	// user should `canopy rm` first.
	if _, err := os.Stat(wsCopy.Path); os.IsNotExist(err) {
		return nil, fmt.Errorf("workspace.Resurrect(%s): workspace dir missing at %s", name, wsCopy.Path)
	}

	// Rebuild the same tdl-style 3-pane layout as buildSession. The
	// agent pane uses agentPaneCmd with resume=true: launcher's Resume
	// argv is selected (e.g., claude --continue ...), and the briefing
	// follows the hybrid strategy — hints-only delta when active hints
	// exist, no --append-system-prompt at all when none. The agent's
	// conversation history is preserved by the agent itself
	// (claude --continue resumes; aider --restore-chat-history; etc).
	env := hooks.WorkspaceEnv(wsCopy.Path, m.Cfg.ProjectRoot, wsCopy.Port)
	if err := m.Tmux.Create(ctx, wsCopy.TmuxSessionName(), wsCopy.Path, keepAlive("nvim ."), env...); err != nil {
		return nil, fmt.Errorf("workspace.Resurrect: tmux create: %w", err)
	}
	if err := m.Tmux.SplitPane(ctx, wsCopy.TmuxSessionName(), wsCopy.Path, "", tmux.SplitVertical, 15); err != nil {
		return nil, err
	}
	agentCmd, err := m.agentPaneCmd(&wsCopy, true /* resume */)
	if err != nil {
		return nil, fmt.Errorf("workspace.Resurrect: agent pane: %w", err)
	}
	if err := m.Tmux.SplitPane(ctx, wsCopy.TmuxSessionName(), wsCopy.Path, keepAlive(agentCmd), tmux.SplitHorizontal, 30); err != nil {
		return nil, err
	}
	// Land active pane on the agent — same rationale as buildSession.
	if err := m.Tmux.SelectPaneDirection(ctx, wsCopy.TmuxSessionName(), "R"); err != nil {
		log.Warn("workspace.resurrect.select-agent-pane-failed", "session", wsCopy.TmuxSessionName(), "err", err.Error())
	}

	// Flip status to ready + bump AgentLaunchCount. The agent pane was
	// just respawned with the resume launcher; next time we resurrect,
	// the briefing strategy will see count>1 and emit a delta-only
	// briefing.
	err = m.Store.WithLock(func(s *state.State) error {
		row, err := s.Find(m.Cfg.ProjectRoot, name)
		if err != nil {
			return err
		}
		row.Status = state.StatusReady
		row.LastError = ""
		row.AgentLaunchCount++
		wsCopy = *row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &wsCopy, nil
}

// BareAttach returns a tmux session name to attach to for a "diagnostic"
// view of the workspace: a one-pane shell at the workspace dir with
// CANOPY_* env vars set, but WITHOUT running scripts.setup or rebuilding
// the standard 3-pane layout.
//
// Subsumes the v0.5 `canopy debug` TODO: when a workspace is broken,
// the user wants to drop into its dir and poke (run the failing script
// manually with bash -x, inspect intermediate state) without canopy
// re-running the same broken setup script in a loop. The detail drawer
// is the natural launch surface — when staring at a broken workspace,
// you want one keystroke to get inside.
//
// Naming: a NEW session is always created with a "-debug" suffix to
// avoid colliding with the workspace's normal session if it's alive.
// If the debug session already exists, this re-attaches to it (no-op
// session reuse). Status field on the workspace row is NOT touched —
// debugging doesn't transition broken→ready; the user has to fix the
// underlying issue and run retry.
func (m *Manager) BareAttach(ctx context.Context, name string) (string, error) {
	st, err := m.Store.Load()
	if err != nil {
		return "", fmt.Errorf("workspace.BareAttach: load: %w", err)
	}
	ws, err := st.Find(m.Cfg.ProjectRoot, name)
	if err != nil {
		return "", fmt.Errorf("workspace.BareAttach(%s): %w", name, ErrWorkspaceNotFound)
	}
	if _, err := os.Stat(ws.Path); os.IsNotExist(err) {
		return "", fmt.Errorf("workspace.BareAttach(%s): workspace dir missing at %s — run `canopy rm %s` to drop the orphan",
			name, ws.Path, name)
	}

	debugSession := ws.TmuxSessionName() + "-debug"
	exists, err := m.Tmux.HasSession(ctx, debugSession)
	if err != nil {
		return "", fmt.Errorf("workspace.BareAttach: probe: %w", err)
	}
	if exists {
		log.Info("bare-attach.reuse", "name", name, "session", debugSession)
		return debugSession, nil
	}

	env := hooks.WorkspaceEnv(ws.Path, m.Cfg.ProjectRoot, ws.Port)
	// Single-pane shell, no shellCmd — drops the user at their default
	// shell with CANOPY_* env vars set so manual hook reruns inherit
	// the right environment. No setup, no scripts.run, no agent pane.
	if err := m.Tmux.Create(ctx, debugSession, ws.Path, "", env...); err != nil {
		return "", fmt.Errorf("workspace.BareAttach: tmux create: %w", err)
	}
	log.Info("bare-attach.created", "name", name, "session", debugSession, "path", ws.Path)
	return debugSession, nil
}

// BareAttachMain is the main-row counterpart of BareAttach: open a
// one-pane shell at the project root with CANOPY_* env vars set and
// the project's main port. No scripts, no auto-running claude/nvim
// — pure "drop me in this project's source repo with the canopy env
// loaded" gesture.
//
// Distinct from BareAttach (the workspace variant) because main rows
// have no state.json entry to look up, no worktree path on disk —
// the cwd is the project root and the port is the project's port
// base. Same -debug session-name suffix convention so a regular
// `canopy main` attach doesn't collide.
//
// Used from the inspect drawer (`b` keybind) when the cursor is on
// a main row. Reuses an existing -debug session if alive.
func (m *Manager) BareAttachMain(ctx context.Context) (string, error) {
	debugSession := m.MainSessionName() + "-debug"
	exists, err := m.Tmux.HasSession(ctx, debugSession)
	if err != nil {
		return "", fmt.Errorf("workspace.BareAttachMain: probe: %w", err)
	}
	if exists {
		log.Info("bare-attach-main.reuse", "session", debugSession)
		return debugSession, nil
	}

	port, err := m.mainPort()
	if err != nil {
		return "", fmt.Errorf("workspace.BareAttachMain: port: %w", err)
	}
	env := hooks.WorkspaceEnv(m.Cfg.ProjectRoot, m.Cfg.ProjectRoot, port)
	// Single pane, no shellCmd — drops the user at their default
	// shell with CANOPY_* env vars set. No nvim, no claude, no
	// auto-runs.
	if err := m.Tmux.Create(ctx, debugSession, m.Cfg.ProjectRoot, "", env...); err != nil {
		return "", fmt.Errorf("workspace.BareAttachMain: tmux create: %w", err)
	}
	log.Info("bare-attach-main.created", "session", debugSession, "path", m.Cfg.ProjectRoot)
	return debugSession, nil
}

// List returns all workspaces for the current project. Used by `canopy
// ls` and the TUI's initial render.
func (m *Manager) List(ctx context.Context) ([]state.Workspace, error) {
	st, err := m.Store.Load()
	if err != nil {
		return nil, err
	}
	out := []state.Workspace{}
	for _, w := range st.Workspaces {
		if w.ProjectRoot == m.Cfg.ProjectRoot {
			out = append(out, w)
		}
	}
	return out, nil
}

// Find returns a single workspace by name. Used by `canopy switch`.
func (m *Manager) Find(ctx context.Context, name string) (*state.Workspace, error) {
	st, err := m.Store.Load()
	if err != nil {
		return nil, err
	}
	w, err := st.Find(m.Cfg.ProjectRoot, name)
	if err != nil {
		return nil, fmt.Errorf("workspace.Find(%s): %w", name, ErrWorkspaceNotFound)
	}
	wCopy := *w
	return &wCopy, nil
}

// OrphanDir is a workspace-shaped directory found under
// ~/.canopy/workspaces/<project>/ that has no matching row in state.json.
// Reconcile reports these via DiscoverOrphans so the user can decide
// whether to adopt them (future feature) or rm them manually.
type OrphanDir struct {
	Project string
	Name    string
	Path    string
}

// DiscoverOrphans walks ~/.canopy/workspaces/<project>/* for the current
// project and returns any directory entry that has no corresponding
// state.json row. Read-only — does not mutate state. The CLI surface
// (canopy reconcile) prints the result so users can take action.
//
// The scan is per-project (current project only) by default. Cross-
// project scanning lives in DiscoverOrphansAllProjects, which lsGlobal
// and a future canopy doctor will use.
func (m *Manager) DiscoverOrphans(ctx context.Context) ([]OrphanDir, error) {
	st, err := m.Store.Load()
	if err != nil {
		return nil, err
	}
	knownNames := map[string]struct{}{}
	for _, w := range st.Workspaces {
		if w.ProjectRoot == m.Cfg.ProjectRoot {
			knownNames[filepath.Base(w.Path)] = struct{}{}
		}
	}

	projDir := m.workspacesDir()
	entries, err := os.ReadDir(projDir)
	if os.IsNotExist(err) {
		return nil, nil // no workspaces dir yet — no orphans
	}
	if err != nil {
		return nil, fmt.Errorf("workspace.DiscoverOrphans: read %s: %w", projDir, err)
	}

	out := []OrphanDir{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, known := knownNames[e.Name()]; known {
			continue
		}
		out = append(out, OrphanDir{
			Project: m.Cfg.Project,
			Name:    e.Name(),
			Path:    filepath.Join(projDir, e.Name()),
		})
	}
	return out, nil
}

// Reconcile walks every workspace for the current project, compares its
// recorded status against disk + tmux reality, and updates state.json
// where they disagree. Returns the per-workspace transition list so the
// caller (CLI or TUI) can print what changed.
//
// Reconcile NEVER deletes a workspace row. Stale rows transition to
// orphaned and stay in state.json until the user explicitly runs
// `canopy rm`. Better to leave a row a user might still want than to
// silently disappear data.
//
// Status mapping (for each workspace row):
//
//	dir on disk + tmux session alive -> ready
//	dir on disk + tmux session gone  -> stopped
//	dir gone, regardless of tmux     -> orphaned
//	status==setting_up older than 5m -> broken (probably crashed mid-setup)
//
// The setting_up timeout exists because a canopy process killed during
// hook execution leaves a setting_up row behind; without the timeout
// it would block re-creation forever.
func (m *Manager) Reconcile(ctx context.Context) ([]ReconcileChange, error) {
	changes := []ReconcileChange{}
	err := m.Store.WithLock(func(s *state.State) error {
		for i := range s.Workspaces {
			w := &s.Workspaces[i]
			if w.ProjectRoot != m.Cfg.ProjectRoot {
				continue
			}
			newStatus, err := m.observeStatus(ctx, w)
			if err != nil {
				// Don't fail the whole reconcile on one bad row; log and skip.
				log.Warn("reconcile.observe-failed", "name", w.Name, "err", err)
				continue
			}
			if newStatus != w.Status {
				changes = append(changes, ReconcileChange{
					Name: w.Name, From: w.Status, To: newStatus,
				})
				w.Status = newStatus
			}

			// Refresh branch from the worktree. Shared with SyncBranch
			// (the per-workspace path hit by the statusline tick) so
			// detached-HEAD/empty/orphaned semantics live in one place.
			if newStatus != state.StatusOrphaned {
				refreshBranchFromWorktree(ctx, w)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changes, nil
}

// refreshBranchFromWorktree updates w.Branch from `git rev-parse` if the
// recorded value is stale. Returns true iff w.Branch was mutated.
//
// Shared by Reconcile (full-project sweep) and SyncBranch (per-workspace
// statusline path). Centralizing the detached-HEAD/empty-string/error
// semantics here means there's exactly one place to reason about
// "when do we trust git over our cached value?"
//
// Rules:
//   - git error → preserve w.Branch, log WARN, return false
//   - branch == "" (detached HEAD, mid-rebase) → preserve w.Branch,
//     log DEBUG, return false. Critical: never blank out the displayed
//     label just because the user is mid-rebase.
//   - branch == w.Branch → no-op, return false
//   - branch differs → mutate w.Branch, log INFO, return true
//
// Caller is responsible for: persisting w to state.json, holding the
// flock, and propagating side effects (tmux session rename, etc.).
func refreshBranchFromWorktree(ctx context.Context, w *state.Workspace) bool {
	branch, err := git.CurrentBranch(ctx, w.Path)
	if err != nil {
		log.Warn("workspace.branch-read-failed", "name", w.Name, "err", err)
		return false
	}
	if branch == "" {
		log.Debug("workspace.branch-empty", "name", w.Name, "path", w.Path)
		return false
	}
	if branch == w.Branch {
		return false
	}
	log.Info("workspace.branch-refreshed", "name", w.Name, "from", w.Branch, "to", branch)
	w.Branch = branch
	return true
}

// ReconcileChange records one workspace's status transition during a
// Reconcile pass. Useful for printing "what changed" reports.
type ReconcileChange struct {
	Name string
	From state.Status
	To   state.Status
}

// observeStatus computes the right status for a workspace row based on
// what's actually on disk and in tmux RIGHT NOW. Pure observation — does
// not mutate state. Caller decides whether to persist the transition.
func (m *Manager) observeStatus(ctx context.Context, w *state.Workspace) (state.Status, error) {
	// Setting_up that's gone stale (probably a crashed `canopy new`).
	const settingUpTimeout = 5 * time.Minute
	if w.Status == state.StatusSettingUp {
		if time.Since(w.CreatedAt) > settingUpTimeout {
			return state.StatusBroken, nil
		}
		return w.Status, nil // still might be legitimately running
	}
	// Broken stays broken until the user `canopy rm`s and recreates.
	if w.Status == state.StatusBroken {
		return state.StatusBroken, nil
	}

	// Disk check first — if the dir is gone, nothing else matters.
	if _, err := os.Stat(w.Path); os.IsNotExist(err) {
		return state.StatusOrphaned, nil
	} else if err != nil {
		return "", fmt.Errorf("stat %s: %w", w.Path, err)
	}

	alive, err := m.Tmux.HasSession(ctx, w.TmuxSessionName())
	if err != nil {
		return "", fmt.Errorf("tmux.HasSession(%s): %w", w.TmuxSessionName(), err)
	}
	if alive {
		return state.StatusReady, nil
	}
	return state.StatusStopped, nil
}
