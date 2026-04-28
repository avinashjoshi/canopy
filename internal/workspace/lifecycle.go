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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/hooks"
	"github.com/avinashjoshi/canopy/internal/namegen"
	"github.com/avinashjoshi/canopy/internal/port"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

var log = clog.Pkg("workspace")

// Default port range for new workspaces. Configurable on Manager so
// tests and future config can override.
const (
	DefaultPortMin = 3000
	DefaultPortMax = 3999
)

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

	// PortMin and PortMax bound the port allocator. Defaults to
	// DefaultPortMin/Max; tests and future config override.
	PortMin int
	PortMax int
}

// New constructs a Manager from a loaded config. It creates the canopy
// home (~/.canopy) and its state directory if missing.
func New(cfg *config.Config) (*Manager, error) {
	home, err := canopyHome()
	if err != nil {
		return nil, err
	}
	store, err := state.NewStore(home)
	if err != nil {
		return nil, err
	}
	return &Manager{
		Cfg:        cfg,
		Store:      store,
		Tmux:       tmux.New(),
		CanopyHome: home,
		PortMin:    DefaultPortMin,
		PortMax:    DefaultPortMax,
	}, nil
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

// Create runs the full workspace setup lifecycle. name may be empty to
// have the manager generate a random name via namegen.
//
// Output from scripts.setup streams to stdout/stderr live so the caller
// (CLI or TUI) can show progress.
func (m *Manager) Create(ctx context.Context, name string, stdout, stderr io.Writer) (*state.Workspace, error) {
	if stdout == nil || stderr == nil {
		return nil, fmt.Errorf("workspace.Create: stdout and stderr writers required")
	}

	// Phase 1: register the workspace under state.WithLock. This computes
	// the name (if missing), allocates a port, and inserts a row with
	// status=setting_up. After this returns, OTHER processes can see the
	// workspace exists and won't pick the same port.
	var ws state.Workspace
	err := m.Store.WithLock(func(s *state.State) error {
		// If no name supplied, generate one. Skip names already in state.
		if name == "" {
			used := make([]string, 0, len(s.Workspaces))
			for _, w := range s.Workspaces {
				if w.Project == m.Cfg.Project {
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
		if _, err := s.Find(m.Cfg.Project, name); err == nil {
			return fmt.Errorf("workspace.Create(%s): %w", name, ErrWorkspaceExists)
		}

		// Port allocation: scan in-use ports across the WHOLE state (not
		// just this project) so multi-project users don't double-bind.
		used := make([]int, 0, len(s.Workspaces))
		for _, w := range s.Workspaces {
			used = append(used, w.Port)
		}
		p, err := port.Allocate(m.PortMin, m.PortMax, used)
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
		safeBranch := git.Sanitize(name)
		wsPath := filepath.Join(m.workspacesDir(), safeBranch)
		session := tmux.SafeName(m.Cfg.Project) + "-" + tmux.SafeName(safeBranch)

		ws = state.Workspace{
			Project:     m.Cfg.Project,
			Name:        name,
			Branch:      name,
			Path:        wsPath,
			TmuxSession: session,
			Port:        p,
			Status:      state.StatusSettingUp,
			CreatedAt:   time.Now().UTC(),
		}
		return s.Add(ws)
	})
	if err != nil {
		return nil, err
	}

	log.Info("workspace.create.registered",
		"project", ws.Project, "name", ws.Name, "path", ws.Path, "port", ws.Port)

	// Phase 2: slow operations outside the lock. If any of these fail,
	// the workspace transitions to status=broken via the helper below.
	setupErr := m.runSetup(ctx, &ws, stdout, stderr)
	if setupErr != nil {
		_ = m.markBroken(&ws, setupErr)
		return &ws, setupErr
	}

	// Phase 3: flip to ready under the lock.
	err = m.Store.WithLock(func(s *state.State) error {
		row, err := s.Find(ws.Project, ws.Name)
		if err != nil {
			return err
		}
		row.Status = state.StatusReady
		row.LastError = ""
		ws = *row
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspace.Create: finalize: %w", err)
	}

	log.Info("workspace.create.ready", "name", ws.Name)
	return &ws, nil
}

// runSetup executes the slow lifecycle steps: git worktree add,
// scripts.setup, and the 4-pane tmux session build. Any failure propagates
// up to Create which marks the workspace broken.
func (m *Manager) runSetup(ctx context.Context, ws *state.Workspace, stdout, stderr io.Writer) error {
	// Ensure parent dir exists. git worktree add creates the leaf dir
	// itself but won't mkdir intermediate parents.
	parent := filepath.Dir(ws.Path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("workspace.runSetup: mkdir %s: %w", parent, err)
	}

	// 1. git worktree add -b <branch> <path>
	if err := git.Add(ctx, m.Cfg.ProjectRoot, ws.Branch, ws.Path); err != nil {
		return fmt.Errorf("workspace.runSetup: git: %w", err)
	}

	// 2. scripts.setup with CANOPY_* env, cwd = workspace dir.
	// Empty scripts.setup -> skip; canopy.json is allowed to omit hooks
	// entirely for projects that just want the worktree + tmux session.
	if m.Cfg.Scripts.Setup != "" {
		scriptPath := filepath.Join(m.Cfg.ProjectRoot, m.Cfg.Scripts.Setup)
		if err := hooks.Run(ctx, scriptPath, hooks.Options{
			Cwd:    ws.Path,
			Env:    hooks.WorkspaceEnv(ws.Path, m.Cfg.ProjectRoot, ws.Port),
			Stdout: stdout,
			Stderr: stderr,
		}); err != nil {
			return fmt.Errorf("workspace.runSetup: %w: %v", ErrSetupFailed, err)
		}
	} else {
		fmt.Fprintln(stdout, "(no scripts.setup configured; skipping)")
	}

	// 3. tmux session + 4 panes (nvim, claude, $SHELL, scripts.run).
	if err := m.buildSession(ctx, ws); err != nil {
		return fmt.Errorf("workspace.runSetup: tmux: %w", err)
	}
	return nil
}

// buildSession creates the tmux session and adds the standard 4-pane
// layout. Each pane runs a different command; the layout call rearranges
// them into a clean 2×2.
//
// Pane commands are wrapped in keepAlive so that when the user exits
// nvim or the dev server crashes, the pane drops to a shell instead of
// vanishing. Without this, a missing script or a `:q` from nvim closes
// the pane and the layout gets unbalanced. The shell pane (empty cmd)
// is naturally a shell so it doesn't need wrapping.
func (m *Manager) buildSession(ctx context.Context, ws *state.Workspace) error {
	if err := m.Tmux.Create(ctx, ws.TmuxSession, ws.Path, keepAlive("nvim")); err != nil {
		return err
	}
	// Pane 1: claude (per-dir storage means resurrection picks up history).
	if err := m.Tmux.SplitPane(ctx, ws.TmuxSession, ws.Path, keepAlive("claude"), tmux.SplitHorizontal); err != nil {
		return err
	}
	// Pane 2: default shell (empty cmd, no wrapping needed).
	if err := m.Tmux.SplitPane(ctx, ws.TmuxSession, ws.Path, "", tmux.SplitVertical); err != nil {
		return err
	}
	// Pane 3: scripts.run (the long-running server command).
	if err := m.Tmux.SplitPane(ctx, ws.TmuxSession, ws.Path, keepAlive(m.Cfg.Scripts.Run), tmux.SplitVertical); err != nil {
		return err
	}
	// Tile into a 2×2 regardless of split history.
	return m.Tmux.SelectLayout(ctx, ws.TmuxSession, "tiled")
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
func (m *Manager) markBroken(ws *state.Workspace, cause error) error {
	return m.Store.WithLock(func(s *state.State) error {
		row, err := s.Find(ws.Project, ws.Name)
		if err != nil {
			return err
		}
		row.Status = state.StatusBroken
		row.LastError = cause.Error()
		ws.Status = state.StatusBroken
		ws.LastError = cause.Error()
		return nil
	})
}

// Remove tears down a workspace: scripts.archive, tmux kill, git worktree
// remove, then drop the state row. Failures in archive or tmux are logged
// but don't block removal — a half-removed workspace is worse than a
// best-effort full removal.
func (m *Manager) Remove(ctx context.Context, name string, stdout, stderr io.Writer) error {
	if stdout == nil || stderr == nil {
		return fmt.Errorf("workspace.Remove: stdout and stderr writers required")
	}

	// Look up the workspace (don't hold the lock during the slow ops).
	st, err := m.Store.Load()
	if err != nil {
		return fmt.Errorf("workspace.Remove: load: %w", err)
	}
	ws, err := st.Find(m.Cfg.Project, name)
	if err != nil {
		return fmt.Errorf("workspace.Remove(%s): %w", name, ErrWorkspaceNotFound)
	}
	wsCopy := *ws // capture before slice mutations

	// 1. scripts.archive — log failure but proceed. Empty -> skip.
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

	// 2. tmux kill — log failure but proceed.
	if err := m.Tmux.Kill(ctx, wsCopy.TmuxSession); err != nil && !errors.Is(err, tmux.ErrSessionNotFound) {
		log.Warn("workspace.remove.tmux-failed", "name", name, "err", err)
	}

	// 3. git worktree remove with --force (we already accept that user's
	// uncommitted changes might exist; canopy rm is the explicit "drop
	// this" command).
	if err := git.Remove(ctx, m.Cfg.ProjectRoot, wsCopy.Path, true); err != nil &&
		!errors.Is(err, git.ErrPathNotFound) {
		log.Warn("workspace.remove.git-failed", "name", name, "err", err)
		fmt.Fprintf(stderr, "warning: git worktree remove failed: %v\n", err)
	}

	// 4. Delete the underlying branch. Canopy's workspaces are ephemeral
	// by design — leaving the branch behind every `canopy rm` would
	// pile up dead branches the user has to clean by hand. force=true
	// because the branch may have unmerged work and the user explicitly
	// asked for removal anyway.
	if err := git.DeleteBranch(ctx, m.Cfg.ProjectRoot, wsCopy.Branch, true); err != nil {
		log.Warn("workspace.remove.branch-delete-failed", "name", name, "branch", wsCopy.Branch, "err", err)
		fmt.Fprintf(stderr, "warning: failed to delete branch %s: %v\n", wsCopy.Branch, err)
	}

	// 5. Drop the state row.
	return m.Store.WithLock(func(s *state.State) error {
		return s.Remove(m.Cfg.Project, name)
	})
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
	ws, err := st.Find(m.Cfg.Project, name)
	if err != nil {
		return nil, fmt.Errorf("workspace.Resurrect(%s): %w", name, ErrWorkspaceNotFound)
	}
	wsCopy := *ws

	// If the workspace dir is gone (orphaned), refuse to resurrect — the
	// user should `canopy rm` first.
	if _, err := os.Stat(wsCopy.Path); os.IsNotExist(err) {
		return nil, fmt.Errorf("workspace.Resurrect(%s): workspace dir missing at %s", name, wsCopy.Path)
	}

	// Rebuild the tmux session. Pane 1 uses `claude --continue` so the
	// prior conversation history is restored. Same keep-alive wrapping
	// as buildSession — pane survives if the user exits nvim/claude.
	if err := m.Tmux.Create(ctx, wsCopy.TmuxSession, wsCopy.Path, keepAlive("nvim")); err != nil {
		return nil, fmt.Errorf("workspace.Resurrect: tmux create: %w", err)
	}
	if err := m.Tmux.SplitPane(ctx, wsCopy.TmuxSession, wsCopy.Path, keepAlive("claude --continue"), tmux.SplitHorizontal); err != nil {
		return nil, err
	}
	if err := m.Tmux.SplitPane(ctx, wsCopy.TmuxSession, wsCopy.Path, "", tmux.SplitVertical); err != nil {
		return nil, err
	}
	if err := m.Tmux.SplitPane(ctx, wsCopy.TmuxSession, wsCopy.Path, keepAlive(m.Cfg.Scripts.Run), tmux.SplitVertical); err != nil {
		return nil, err
	}
	if err := m.Tmux.SelectLayout(ctx, wsCopy.TmuxSession, "tiled"); err != nil {
		return nil, err
	}

	// Flip status to ready.
	err = m.Store.WithLock(func(s *state.State) error {
		row, err := s.Find(m.Cfg.Project, name)
		if err != nil {
			return err
		}
		row.Status = state.StatusReady
		row.LastError = ""
		wsCopy = *row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &wsCopy, nil
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
		if w.Project == m.Cfg.Project {
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
	w, err := st.Find(m.Cfg.Project, name)
	if err != nil {
		return nil, fmt.Errorf("workspace.Find(%s): %w", name, ErrWorkspaceNotFound)
	}
	wCopy := *w
	return &wCopy, nil
}
