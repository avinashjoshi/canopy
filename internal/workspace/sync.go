package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

// SyncResult describes the outcome of a SyncBranch call. Callers use this
// to know whether a side effect fired (label change visible to the user)
// vs a no-op (branch was already in sync).
type SyncResult struct {
	// Changed is true when w.Branch was updated AND the tmux session was
	// renamed. False on no-op (branch already current, detached HEAD,
	// git error, etc.).
	Changed bool

	// OldBranch and NewBranch are populated when Changed is true.
	OldBranch string
	NewBranch string

	// OldSession and NewSession are populated when Changed is true and
	// the tmux session was renamed too. NewSession may equal OldSession
	// when the workspace name is sanitized identically (rare).
	OldSession string
	NewSession string
}

// SyncBranch refreshes a single workspace's recorded branch from
// `git rev-parse` and propagates the change to tmux. Lighter than
// Reconcile (which sweeps every workspace under exclusive flock); designed
// for the statusline tick path that needs to keep one workspace's labels
// in sync without blocking concurrent canopy CLI work.
//
// Pipeline (only when branch differs from recorded value):
//
//  1. tmux rename-session canopy-<old> -> canopy-<new>
//  2. write state.json under flock with new Branch + TmuxSession
//
// Tmux first, state.json second, per the eng-review D6 ordering decision:
// tmux is the authoritative liveness oracle, and the existing Reconcile
// path will catch divergence on the next refresh if a state write fails
// after a successful tmux rename.
//
// Detached HEAD, git errors, and orphaned status all no-op silently —
// see refreshBranchFromWorktree for the matrix. The user-visible label
// stays whatever it was, never blanks out.
//
// On tmux session-name collision (another workspace already holds the
// target name), returns SyncResult{Changed:false} wrapping
// tmux.ErrSessionNameInUse so callers can decide how to disambiguate.
// The state.json mutation is skipped in that case — keeping w.Branch
// stale is preferable to a half-applied rename.
func (m *Manager) SyncBranch(ctx context.Context, name string) (SyncResult, error) {
	return SyncWorkspaceBranch(ctx, m.Store, m.Tmux, m.Cfg.ProjectRoot, name)
}

// SyncWorkspaceBranch is the package-level entry point used by callers
// that don't have a full Manager (notably the statusline widget, which
// avoids constructing a Manager on every 15s tick because workspace.New
// runs the v1->v2 state migration under the flock).
//
// Same semantics as Manager.SyncBranch — see that doc comment for the
// pipeline, ordering, and error handling.
func SyncWorkspaceBranch(ctx context.Context, store *state.Store, tmuxClient *tmux.Client, projectRoot, name string) (SyncResult, error) {
	var result SyncResult

	// Fast path: peek at current state without holding the flock to
	// decide whether we need to do anything. The statusline tick hits
	// this every 15s; nearly all calls are no-ops where flock acquisition
	// would be pure overhead.
	st, err := store.Load()
	if err != nil {
		return result, fmt.Errorf("workspace.SyncBranch(%s): load: %w", name, err)
	}
	wPeek, err := st.Find(projectRoot, name)
	if err != nil {
		return result, fmt.Errorf("workspace.SyncBranch(%s): %w", name, ErrWorkspaceNotFound)
	}
	// Don't sync orphaned/setting_up rows — the worktree may not exist
	// or may be mid-creation. Reconcile owns those transitions.
	if wPeek.Status == state.StatusOrphaned || wPeek.Status == state.StatusSettingUp {
		return result, nil
	}

	// One-shot migration: pre-v0.16 sessions use `<project>-<branch>`
	// (hyphen separator). Detect a live legacy session and rename it
	// in-place to the new `<project>/<branch>` format BEFORE any other
	// sync logic runs, so the rest of the pipeline operates on the
	// correct session name. Idempotent: if no legacy session exists
	// (either because the workspace is fresh or already migrated), this
	// is a cheap HasSession check.
	expectedSession := wPeek.TmuxSessionName()
	legacySession := wPeek.LegacyTmuxSessionName()
	if expectedSession != legacySession {
		hasNew, _ := tmuxClient.HasSession(ctx, expectedSession)
		hasLegacy, _ := tmuxClient.HasSession(ctx, legacySession)
		switch {
		case hasNew && hasLegacy:
			// Both exist. Two parallel canopy versions or a manual tmux
			// new-session left behind a stale stub. The new format wins
			// (it's what canopy operates on); surface a warn so the
			// stale legacy session doesn't disappear silently.
			log.Warn("workspace.sync.legacy-session-stranded",
				"name", name, "legacy", legacySession, "new", expectedSession,
				"hint", "stale tmux session — kill manually with `tmux kill-session -t "+legacySession+"`")
		case !hasNew && hasLegacy:
			windowName := wPeek.Branch
			if windowName == "" {
				windowName = wPeek.Name
			}
			if err := tmuxClient.Rename(ctx, legacySession, expectedSession, windowName); err != nil {
				log.Warn("workspace.sync.legacy-migration-failed",
					"name", name, "legacy", legacySession, "new", expectedSession, "err", err)
			} else {
				log.Info("workspace.sync.migrated-legacy-session",
					"name", name, "from", legacySession, "to", expectedSession)
			}
		}
	}

	// Pinned workspaces opt out of branch auto-tracking. The user has
	// declared the current display label is the one they want — typically
	// because they rebase or check out multiple feature branches in this
	// worktree and don't want the statusline to flicker. `canopy rename
	// --unpin` clears the field and lets sync run again.
	//
	// Placed AFTER legacy session migration so a one-time hyphen→slash
	// session rename still happens for pinned workspaces left over from
	// pre-v0.16 — that's a session-name format upgrade, not a branch
	// switch the user is trying to suppress.
	if wPeek.PinDisplayName {
		return result, nil
	}

	// Snapshot the current values so we can decide if work is needed.
	wCopy := *wPeek
	if !refreshBranchFromWorktree(ctx, &wCopy) {
		// Branch already in sync (or git error / detached HEAD).
		// Don't reconcile the window name on every tick — RenameWindow
		// also pins automatic-rename off, which is a tmux server-state
		// mutation we shouldn't fire 4x/min per attached client. Window
		// names are set when sessions are renamed (legacy migration
		// above + the rename-pipeline below) and on workspace creation;
		// after that, the pinned name sticks.
		return result, nil
	}
	oldBranch := wPeek.Branch
	newBranch := wCopy.Branch

	// Branch changed. Rename the tmux session next. The new session name
	// follows the same `<project>-<sanitized-branch>` pattern as fresh
	// sessions get from Create.
	oldSession := wPeek.TmuxSessionName()
	newSession := tmuxSessionNameFor(wPeek.ProjectBasename(), newBranch)

	if oldSession != newSession {
		// Window name is just the branch (no `<project>-` prefix). The
		// session name carries the project namespace; duplicating it in
		// the window name is pure noise — the screenshot pain we hit
		// when both fields had the same long string. Branch alone is
		// short enough that "1:<branch>" reads as a useful tag, not a
		// truncated repeat.
		if err := tmuxClient.Rename(ctx, oldSession, newSession, newBranch); err != nil {
			if errors.Is(err, tmux.ErrSessionNotFound) {
				// Session was killed externally — nothing to rename.
				// Fall through and update state.json so the next attach
				// uses the new session name (Resurrect will create it).
				log.Warn("workspace.sync.tmux-session-gone",
					"name", name, "session", oldSession)
			} else if errors.Is(err, tmux.ErrSessionNameInUse) {
				// Collision: another workspace already holds the target
				// session name. Don't write state — keeping w.Branch
				// stale is preferable to a half-applied rename.
				return result, fmt.Errorf("workspace.SyncBranch(%s): %w", name, err)
			} else {
				return result, fmt.Errorf("workspace.SyncBranch(%s): tmux rename: %w", name, err)
			}
		}
	}

	// Now persist to state.json under flock. Only Branch is written —
	// TmuxSessionName() derives the session name from project + branch
	// at read time, so updating Branch is sufficient.
	err = store.WithLock(func(s *state.State) error {
		w, err := s.Find(projectRoot, name)
		if err != nil {
			return fmt.Errorf("workspace not found during sync: %w", err)
		}
		// Re-check inside the lock — another agent may have synced first.
		if w.Branch == newBranch {
			return nil
		}
		w.Branch = newBranch
		return nil
	})
	if err != nil {
		// The tmux rename succeeded but state.json didn't. The next
		// Reconcile call will catch the divergence and fix it. Surface
		// as a warn so the caller knows; not fatal.
		log.Warn("workspace.sync.state-write-failed",
			"name", name, "err", err, "old_session", oldSession, "new_session", newSession)
		return result, fmt.Errorf("workspace.SyncBranch(%s): state write: %w", name, err)
	}

	result.Changed = true
	result.OldBranch = oldBranch
	result.NewBranch = newBranch
	result.OldSession = oldSession
	result.NewSession = newSession
	return result, nil
}

// SetPin toggles a workspace's PinDisplayName field under flock. When
// pinned, SyncBranch is a no-op for this workspace until it's unpinned.
//
// SetPin only mutates state.json — it does not run SyncBranch. Callers
// that want "pin to the current branch" semantics should run SyncBranch
// (or set pinned=false → sync → SetPin true) themselves; keeping this
// method surgical lets `canopy rename --pin` and `--unpin` compose the
// pipeline they need without baking policy into the state mutation.
func (m *Manager) SetPin(ctx context.Context, name string, pinned bool) error {
	return m.Store.WithLock(func(s *state.State) error {
		w, err := s.Find(m.Cfg.ProjectRoot, name)
		if err != nil {
			return fmt.Errorf("workspace.SetPin(%s): %w", name, ErrWorkspaceNotFound)
		}
		w.PinDisplayName = pinned
		return nil
	})
}

// tmuxSessionNameFor produces the canonical "<project>/<branch>" tmux
// session name with both pieces sanitized for tmux's target syntax.
// Mirrors state.Workspace.TmuxSessionName so rename and create can't
// drift into different naming conventions.
func tmuxSessionNameFor(project, branch string) string {
	if project == "" {
		return tmux.SafeName(branch)
	}
	return tmux.SafeName(project) + state.SessionSeparator + tmux.SafeName(branch)
}
