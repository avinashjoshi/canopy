// Main-session lifecycle for the project's source-repo tmux session.
//
// `canopy main` and the unified TUI's enter-on-main-row both need to
// "ensure the project's main session exists, then attach." This file
// owns the build half so both callers route through one implementation
// — without it, the TUI errored out on dead main rows ("main session
// not running — run `canopy main`") instead of just bringing it up.

package workspace

import (
	"context"
	"fmt"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/hooks"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

// MainSessionName returns the tmux session name canopy uses for the
// project's main-repo session. Mirrors the format hard-coded in
// cmd/canopy/main.go and state/listing.go:safeMainSessionName so the
// three places agree on what session to look for.
func (m *Manager) MainSessionName() string {
	return tmux.SafeName(m.Cfg.Project) + state.SessionSeparator + "main"
}

// EnsureMainSession idempotently brings up the project's main tmux
// session and returns its name. When the session is already alive,
// it's a cheap HasSession check and returns immediately. Otherwise it
// allocates the project's port base (same path canopy main uses, so
// ports stay consistent), exports CANOPY_PORT etc. in the session
// env, and builds the standard 3-pane layout (nvim + claude + shell).
//
// The TUI calls this before attaching to a dead main row so the user
// doesn't have to drop to a shell and run `canopy main` — the verb
// "open the main session" is one keystroke from the list.
func (m *Manager) EnsureMainSession(ctx context.Context) (string, error) {
	session := m.MainSessionName()

	alive, err := m.Tmux.HasSession(ctx, session)
	if err != nil {
		return "", fmt.Errorf("workspace.EnsureMainSession: probe %s: %w", session, err)
	}
	if alive {
		// Backfill @canopy-role tags for v0.15-style main sessions that
		// were started before v0.16. Best-effort: errors logged, never
		// block attach. Empty launcherType defaults to "agent:claude"
		// via agent.RoleForType, which matches the hardcoded
		// `claude --continue || claude` literal in buildMainSession.
		_ = BackfillRoles(ctx, m.Tmux, session, "")
		return session, nil
	}

	// One-shot migration: pre-v0.16 used `<project>-main` (hyphen). If
	// the new-format session isn't alive but the legacy one is, rename
	// it in-place rather than spawning a duplicate. Same shim that
	// SyncBranch runs for workspace sessions.
	legacy := tmux.SafeName(m.Cfg.Project) + "-main"
	if legacy != session {
		if hasLegacy, _ := m.Tmux.HasSession(ctx, legacy); hasLegacy {
			if err := m.Tmux.Rename(ctx, legacy, session, "main"); err != nil {
				log.Warn("workspace.main.legacy-migration-failed",
					"legacy", legacy, "new", session, "err", err)
			} else {
				log.Info("workspace.main.migrated-legacy-session",
					"from", legacy, "to", session)
				return session, nil
			}
		}
	}

	port, err := m.mainPort()
	if err != nil {
		return "", fmt.Errorf("workspace.EnsureMainSession: port: %w", err)
	}
	env := hooks.WorkspaceEnv(m.Cfg.ProjectRoot, m.Cfg.ProjectRoot, port)

	if err := buildMainSession(ctx, m.Tmux, session, m.Cfg.ProjectRoot, env); err != nil {
		return "", fmt.Errorf("workspace.EnsureMainSession: build %s: %w", session, err)
	}
	return session, nil
}

// mainPort returns the project's reserved base port. Atomic allocate-
// or-fetch under the state lock, mirroring Manager.Create's path so
// the main session and workspaces draw from the same block.
func (m *Manager) mainPort() (int, error) {
	var base int
	err := m.Store.WithLock(func(s *state.State) error {
		b, _, err := s.EnsureProjectBase(
			m.Cfg.ProjectRoot,
			m.Settings.Ports.Base,
			m.Settings.Ports.ProjectStride,
			MaxProjects,
		)
		if err != nil {
			return err
		}
		base = b
		return nil
	})
	return base, err
}

// buildMainSession creates the 3-pane layout for the main-repo session:
// nvim top-left, claude (--continue || claude) top-right, shell bottom.
// Same shape buildSession uses for workspaces — keeps muscle memory
// consistent across main and worktree sessions.
//
// Lives in this package (not cmd/canopy) so the TUI's auto-start path
// and the `canopy main` CLI share one implementation. The CLI body
// keeps the user-facing prints; this function is silent.
func buildMainSession(ctx context.Context, tc *tmux.Client, session, projectRoot string, env []string) error {
	idePane, err := tc.Create(ctx, session, projectRoot, `nvim .; exec "$SHELL"`, env...)
	if err != nil {
		return err
	}
	if err := tc.SetRole(ctx, idePane, "ide"); err != nil {
		return fmt.Errorf("workspace.buildMainSession: tag ide pane: %w", err)
	}
	shellPane, err := tc.SplitPane(ctx, session, projectRoot, "", tmux.SplitVertical, 15)
	if err != nil {
		return err
	}
	if err := tc.SetRole(ctx, shellPane, "terminal:shell"); err != nil {
		return fmt.Errorf("workspace.buildMainSession: tag shell pane: %w", err)
	}
	// Main session's agent is hardcoded to claude today (matches the
	// `claude --continue || claude` literal). When the parked global-
	// config PR ships, this becomes agent.RoleForType(settings.DefaultAgent).
	// For now, the role tag uses the canopy-default helper for symmetry.
	agentPane, err := tc.SplitPane(ctx, session, projectRoot, `claude --continue || claude; exec "$SHELL"`, tmux.SplitHorizontal, 30)
	if err != nil {
		return err
	}
	if err := tc.SetRole(ctx, agentPane, agent.RoleForType("")); err != nil {
		return fmt.Errorf("workspace.buildMainSession: tag agent pane: %w", err)
	}
	return nil
}
