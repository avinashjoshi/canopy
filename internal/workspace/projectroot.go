// Project-root resolution for the unified TUI.
//
// When the unified TUI starts, it needs to know "the current project" so the
// Local tab can filter rows. Naive walk-up via config.DiscoverAndLoad is wrong
// when invoked from inside a workspace (a workspace dir IS a git worktree and
// DOES contain canopy.json — but its absolute path is the worktree dir, not
// the canonical source-repo root). Every state.GlobalRow keys by the canonical
// source-repo ProjectRoot, so the naive walk-up returns a path that matches no
// rows and the Local tab appears empty.
//
// ResolveCurrentProject does a two-step lookup that tracks the canonical
// root regardless of which worktree (or symlink) the user cd'd into:
//
//  1. Workspace-cwd match: scan registered workspaces for one whose Path is a
//     prefix of cwd. If found, return that workspace's ProjectRoot.
//  2. Project main-repo match: fall back to config.DiscoverAndLoad. If the
//     resolved root is in state.Projects, return it. Otherwise "".
//
// Symlinks: cwd is canonicalized via filepath.EvalSymlinks before any prefix
// matching. Without this, `~/work/foo -> ~/Work/foo` would defeat the prefix
// match and the Local tab would silently show empty.
//
// This was originally cmd/canopy/popup_inner.go:resolveProjectRoot. Lives
// in the workspace package now so the unified TUI can call it without
// importing cmd/canopy/*.

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/oncactus/canopy/internal/config"
	"github.com/oncactus/canopy/internal/state"
)

// ResolveCurrentProject maps a cwd to a canonical project root for the
// Local tab filter. Returns "" when cwd doesn't correspond to any
// registered project — caller treats this as "no current project, show
// Global tab pre-selected."
//
// nil state is tolerated — yields "" (best-effort fallback so a state-load
// failure during TUI startup doesn't make the popup unusable).
//
// All resolution paths emit DEBUG logs ("workspace.resolve_current_project")
// with the cwd, evalSymlinks result, and which step matched. The Local tab
// being empty is the most common "wat" moment for canopy users; structured
// logs make ~/.canopy/log/canopy.log the answer instead of a guessing game.
func ResolveCurrentProject(cwd string, st *state.State) string {
	if st == nil {
		log.Debug("workspace.resolve_current_project", "cwd", cwd, "result", "", "reason", "nil state")
		return ""
	}

	// Canonicalize cwd up front: resolve symlinks so prefix comparisons work
	// against state.Workspaces[*].Path values (which are also canonicalized
	// at write time via the workspace creation flow). Without this, a user
	// with `~/work/foo -> ~/Work/foo` cd'ing via the symlink defeats the
	// prefix match and the Local tab silently shows empty. EvalSymlinks
	// errors are non-fatal (e.g. a missing component) — fall through with
	// the unresolved path.
	resolved := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		resolved = r
	}

	// Step 1: workspace-path prefix match.
	cwdWithSlash := strings.TrimRight(resolved, "/") + "/"
	for _, ws := range st.Workspaces {
		if ws.Path == "" {
			continue
		}
		wsPathWithSlash := strings.TrimRight(ws.Path, "/") + "/"
		if cwdWithSlash == wsPathWithSlash || strings.HasPrefix(cwdWithSlash, wsPathWithSlash) {
			if ws.ProjectRoot != "" {
				log.Debug("workspace.resolve_current_project",
					"cwd", cwd, "evalSymlinks", resolved,
					"matched_workspace", ws.Name, "result", ws.ProjectRoot)
				return ws.ProjectRoot
			}
		}
	}

	// Step 2: registered-project canopy.json walk-up. Use the canonicalized
	// path so symlinks don't trip up DiscoverAndLoad's stat calls either.
	cfg, err := config.DiscoverAndLoad(resolved)
	if err != nil {
		if !errors.Is(err, config.ErrNotFound) {
			log.Warn("workspace.resolve_current_project.config_discover_failed",
				"cwd", cwd, "err", err.Error())
		}
		log.Debug("workspace.resolve_current_project",
			"cwd", cwd, "evalSymlinks", resolved, "result", "", "reason", "no canopy.json")
		return ""
	}
	if _, registered := st.Projects[cfg.ProjectRoot]; registered {
		log.Debug("workspace.resolve_current_project",
			"cwd", cwd, "evalSymlinks", resolved,
			"matched_project", cfg.ProjectRoot, "result", cfg.ProjectRoot)
		return cfg.ProjectRoot
	}
	// canopy.json found but project isn't in state.json — unregistered;
	// don't filter by it (Local tab would show zero rows anyway).
	log.Debug("workspace.resolve_current_project",
		"cwd", cwd, "evalSymlinks", resolved,
		"matched_project", cfg.ProjectRoot, "result", "", "reason", "project not in state")
	return ""
}

// ResolveCurrentProjectFromWD is the convenience variant for the common
// "use os.Getwd()" caller. Errors fall through to "" (the empty result
// is what callers handle anyway when cwd resolution fails).
func ResolveCurrentProjectFromWD(st *state.State) string {
	cwd, err := os.Getwd()
	if err != nil {
		log.Warn("workspace.resolve_current_project.getwd_failed", "err", err.Error())
		return ""
	}
	return ResolveCurrentProject(cwd, st)
}
