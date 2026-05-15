// Routing for the no-subcommand `canopy` invocation.
//
// v0.8 unification: there is now ONE TUI program for every canopy
// invocation. The routing decision is much simpler than v0.5-v0.7:
//
//	1. Fresh git repo with no canopy.json → InitSplashModel
//	   ("press i to canopy init"). On 'i', runInit fires synchronously.
//
//	2. Everything else (in-project, outside-any-project, popup) →
//	   the unified TUI via ui.RunUnified. Pre-selected tab + Local-row
//	   filter is computed from cwd via workspace.ResolveCurrentProject.
//
// The popup keybind in tmux invokes `canopy` directly (no popup-inner
// subcommand). CANOPY_IN_POPUP=1 set by `display-popup -E` flips
// rendering to popup mode.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/ui"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// routeRoot picks between the init splash (fresh canopy install, no
// known projects) and the unified TUI. Called from main.go's RunE when
// the user invokes `canopy` with no subcommand.
//
// Routing precedence (most specific first):
//
//  1. cwd has canopy.json → unified TUI with Manager scoped to that
//     project. If Manager construction fails (e.g. v1/v2 state collision),
//     fall back to global mode (mgr=nil) rather than refusing to launch —
//     the user can still see workspaces and act on cross-project rows.
//
//  2. cwd resolves to a registered workspace (state.json knows about it)
//     → unified TUI with mgr=nil but currentProject=resolved. Covers
//     workspace dirs, where canopy.json isn't checked into the worktree.
//
//  3. cwd is in a git repo with no canopy.json AND state.json has no
//     projects yet → init splash. The "first canopy install" path.
//
//  4. Anything else → unified TUI in global mode. Covers /tmp, $HOME,
//     ambient dirs.
func routeRoot(ctx context.Context, cwd string, stdout io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("unified TUI: home dir: %w", err)
	}
	store, err := state.NewStore(filepath.Join(home, ".canopy"))
	if err != nil {
		return err
	}
	tc := tmux.New()

	// Best-effort state load up front so the workspace-cwd resolver and
	// the "any registered projects?" check below can use it. nil state
	// is tolerated — both paths fall through cleanly.
	st, _ := store.Load()

	currentProject, cfg, err := resolveProjectContext(cwd, st)
	if err != nil {
		return err
	}
	// Best-effort: if cwd is inside a workspace dir, capture both the
	// workspace name and its project root so the unified TUI can
	// pre-select that exact row (project root + name disambiguates
	// across projects with same-named workspaces). Falls back to ("","")
	// when cwd is outside any workspace.
	currentWorkspaceRoot, currentWorkspace := workspace.ResolveCurrentWorkspace(cwd, st)

	var mgr *workspace.Manager
	if currentProject != "" {
		// Build the Manager from the CANONICAL project root. When
		// ResolveCurrentProject matched a workspace dir, cwd's own
		// canopy.json is the worktree's copy; we load via LoadFrom
		// against the source-repo root so Manager's git operations
		// (worktree add/remove) target the right repo. When the
		// walk-up populated cfg, we re-use it directly (LoadFrom
		// would just re-read the same file).
		pcfg := cfg
		if pcfg == nil {
			lc, perr := config.LoadFrom(currentProject)
			if perr == nil {
				pcfg = lc
			}
			// LoadFrom failure is non-fatal — the source repo may have
			// been moved or canopy.json deleted. TUI still launches in
			// read-only mode so the user can see workspaces and fix
			// the underlying issue from outside.
		}
		if pcfg != nil {
			m, err := workspace.New(pcfg)
			if err != nil {
				fmt.Fprintf(stdout,
					"warning: couldn't construct Manager for %s (%v). "+
						"Launching unified TUI in global mode — destructive verbs on this project's "+
						"rows won't work until the underlying state issue is resolved.\n",
					currentProject, err)
			} else {
				mgr = m
			}
		}
	}

	// Path 3: init splash gate. Only fire when the user has NO known
	// projects AND cwd is a git repo. This prevents the splash from
	// firing inside workspace dirs (which are git worktrees of repos
	// canopy DOES know about) and inside fresh repos for users who
	// already have other projects registered (where the right action
	// is "see your other projects" not "init this empty one").
	if cfg == nil && currentProject == "" {
		hasProjects := st != nil && len(st.Projects) > 0
		if !hasProjects {
			isRepo, _ := git.IsRepo(ctx, cwd)
			if isRepo {
				return runInitSplashFlow(ctx, cwd, stdout)
			}
		}
	}

	// Resolve the running binary's version surface so the TUI top bar
	// can show the version pill. versionLabel is the human-friendly
	// label ("v0.12.0+abc1234" or "dev"); devWorkspace is the canopy
	// workspace name when this is a dev build inside a known worktree,
	// or "" otherwise. The UI uses both to pick muted-gray-release vs
	// cyan-DEV styling.
	d := versionDetails()
	versionLabel := d.Version
	if d.IsDev {
		// Don't surface the literal "dev" string in the pill — it's
		// uninformative compared to the workspace name. Empty here
		// hands rendering control to devWorkspace below.
		versionLabel = ""
	}

	// Auto-check pill state. initialUpgradeForUI is a synchronous
	// cache-only read; it never blocks on the network. needsRefresh
	// is true when the cache was stale or missing — Init() fires
	// the closure as a tea.Cmd in that case, so the network call
	// happens after the TUI is already on screen.
	//
	// The closure itself is wired UNCONDITIONALLY (when not DEV) so
	// the `r` key can force a refresh even when the cache was fresh
	// at startup. The needsRefresh flag is passed via
	// RefreshOnInit so Init() can decide whether to fire on launch.
	initialUpgrade, needsRefresh := initialUpgradeForUI(d)
	var refreshFn ui.UpgradeRefreshFn
	if !d.IsDev {
		refreshFn = func(ctx context.Context) (string, error) {
			srcDir, err := upgradeSrcDir()
			if err != nil {
				return "", err
			}
			// Refuse cleanly if ~/.canopy/src is missing — auto-check
			// can't fall back to git without it. Treat as a no-op
			// (silent) so users without a working install.sh don't
			// see noise.
			if rerr := ensureSrcDirReady(srcDir); rerr != nil {
				return "", rerr
			}
			return refreshUpgradeForUI(ctx, srcDir, d)
		}
	}

	// Closures for the in-TUI upgrade flow (U key). These bridge
	// the network + shell layers without leaking package internals
	// across the cmd/canopy ↔ internal/ui boundary. nil on DEV (the
	// pill never shows on DEV anyway, so the keys never bind).
	var changelogFn ui.UpgradeChangelogFn
	var shellFn ui.UpgradeShellFn
	var dismissFn ui.UpgradeDismissFn
	if !d.IsDev {
		changelogFn = func(ctx context.Context) (string, error) {
			srcDir, err := upgradeSrcDir()
			if err != nil {
				return "", err
			}
			if err := ensureSrcDirReady(srcDir); err != nil {
				return "", err
			}
			body, err := fetchRemoteFile(ctx, srcDir, upgradeChangelogURL, "CHANGELOG.md")
			if err != nil {
				return "", err
			}
			current := normalizeRunningVersion(d.Version)
			// Use upgradeAvailable from the cache for the slice's
			// upper bound. If the cache is empty here something
			// went sideways (we shouldn't be in the flow without
			// it), fall back to slicing from current to "no end".
			path, perr := upgradeCheckPath()
			if perr != nil {
				return "", perr
			}
			cache, _ := readUpgradeCheck(path)
			if cache == nil || cache.LatestVersion == "" {
				return "", nil
			}
			return changelogSlice(body, current, cache.LatestVersion), nil
		}
		shellFn = func(ctx context.Context, w io.Writer) error {
			srcDir, err := upgradeSrcDir()
			if err != nil {
				return err
			}
			if err := ensureSrcDirReady(srcDir); err != nil {
				return err
			}
			err = upgradeRunShellStreaming(ctx, srcDir, w)
			if err == nil {
				// Mirror the CLI flow's post-success cache rewrite.
				// Failure here is non-fatal (upgrade succeeded;
				// stale pill for one cycle is acceptable).
				path, _ := upgradeCheckPath()
				if cache, _ := readUpgradeCheck(path); cache != nil && cache.LatestVersion != "" {
					if cerr := clearUpgradeCheck(cache.LatestVersion); cerr != nil {
						upgradeLog.Warn("upgrade.cache_clear_failed", "err", cerr)
					}
				}
			}
			return err
		}
		dismissFn = func() error {
			_, err := dismissUpgradeCheck()
			return err
		}
	}
	return ui.RunUnified(mgr, store, tc, currentProject, currentWorkspaceRoot, currentWorkspace, ui.RunUnifiedOptions{
		VersionLabel:   versionLabel,
		DevWorkspace:   d.DevWorkspace,
		InitialUpgrade: initialUpgrade,
		RefreshFn:      refreshFn,
		RefreshOnInit:  needsRefresh,
		ChangelogFn:    changelogFn,
		ShellFn:        shellFn,
		DismissFn:      dismissFn,
		// v0.20 Add Project: closure binds the TUI's form to
		// runInit. The TUI handles git clone via tea.ExecProcess
		// (so auth prompts work natively), then calls back here to
		// finish the init half.
		RunInitFunc: func(absPath string, withScripts, force bool) error {
			// Output discarded for in-TUI flows; success/error is
			// rendered by the form itself (toast / inline error).
			return runInit(absPath, initOptions{Force: force, WithScripts: withScripts}, io.Discard)
		},
	})
}


// resolveProjectContext picks the canonical current-project root for the
// unified TUI launch given a cwd. ResolveCurrentProject (workspace-path
// prefix match first, canopy.json walk-up + state.Projects check second)
// is preferred so cwd inside a workspace dir maps to the source-repo
// ProjectRoot, not the worktree's own path. Naive DiscoverAndLoad would
// resolve ProjectRoot to the worktree dir (it contains the checked-in
// canopy.json), which matches no rows in state and leaves the Local
// tab empty — the user-reported popup bug.
//
// When ResolveCurrentProject comes up empty (cwd outside any registered
// project), fall through to DiscoverAndLoad so users in an unregistered
// canopy project — fresh clone, pre-init — still get a current-project
// context. Returns (currentProject, cfgFromWalkUp, err); cfg is non-nil
// only on the walk-up branch so the caller knows whether to re-use it
// for Manager construction or LoadFrom against the canonical root.
//
// ErrNotFound from the walk-up is folded into ("","",nil) — "no project
// here" is not an error, just the global-mode signal.
func resolveProjectContext(cwd string, st *state.State) (string, *config.Config, error) {
	if root := workspace.ResolveCurrentProject(cwd, st); root != "" {
		return root, nil, nil
	}
	cfg, err := config.DiscoverAndLoad(cwd)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return "", nil, nil
		}
		return "", nil, err
	}
	return cfg.ProjectRoot, cfg, nil
}

// runInitSplashFlow shows the v0.20 Add Project splash. The splash
// returns either Dismiss (user pressed esc) or Submit + an arg (path
// or URL). On Submit, we drop out of altscreen (splash's tea.Program
// already exited) and invoke runAddProject — the same orchestrator
// the CLI `canopy init` command uses. Because we're now post-altscreen,
// git's auth prompts (SSH passphrase, HTTPS credential helpers) work
// on the user's real tty.
func runInitSplashFlow(ctx context.Context, cwd string, stdout io.Writer) error {
	result, err := ui.RunInitSplash(cwd)
	if err != nil {
		return fmt.Errorf("init splash: %w", err)
	}
	if result.Action == ui.SplashDismiss {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("init splash: home dir: %w", err)
	}
	canopyHome := filepath.Join(home, ".canopy")
	_, err = runAddProject(ctx, result.Arg, addProjectOptions{}, stdout, canopyHome)
	return err
}
