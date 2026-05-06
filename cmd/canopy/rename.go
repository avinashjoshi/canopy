// Command canopy rename forces a label refresh from the worktree's current
// branch — useful immediately after `git branch -m` when the user wants
// the tmux session name, statusline, and TUI labels to update without
// waiting for the next statusline auto-sync tick (~15s).
//
// With no argument, rename targets the workspace whose tmux session is
// currently attached. With an explicit name, it targets that workspace.
// Either way the new label comes from `git rev-parse --abbrev-ref HEAD`
// inside the worktree — there's no `--to <name>` flag because auto-sync
// would clobber it next tick. Pinning is deferred (see TODOS for the
// `--pin/--unpin` follow-up).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

func newRenameCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename [<workspace>]",
		Short: "Refresh workspace labels from the current branch.",
		Long: `Forces an immediate refresh of the workspace's labels (tmux session
name, statusline widget, canopy TUI rows) from the worktree's current
branch — usually right after running ` + "`git branch -m <intent-slug>`" + `.

Without an argument, rename targets the workspace whose tmux session is
currently attached. To target another workspace, pass its name.

There is no --to flag: the new label always comes from the worktree's
current branch. Statusline auto-sync would clobber any explicit value
on the next tick. Pinning is on the roadmap.

  canopy rename                    # rename current workspace from git branch
  canopy rename feat-oauth         # rename a specific workspace`,
		Args: cobra.MaximumNArgs(1),
		// Allow inside tmux: the common case is "I just renamed my branch in
		// this very pane and want the labels to refresh."
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE:        runRename,
	}
	return cmd
}

func runRename(cmd *cobra.Command, args []string) error {
	mgr, err := loadManager()
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	wsName, err := resolveTargetWorkspace(ctx, mgr, args)
	if err != nil {
		return err
	}

	res, err := mgr.SyncBranch(ctx, wsName)
	if err != nil {
		// Surface tmux name collisions specifically — they're the one
		// case the user can act on immediately.
		if errors.Is(err, tmux.ErrSessionNameInUse) {
			return fmt.Errorf("rename: another workspace already holds the tmux session name for branch '%s' — pick a different branch name or rename the colliding workspace", res.NewBranch)
		}
		return fmt.Errorf("rename: %w", err)
	}

	if !res.Changed {
		// SyncBranch returns an empty NewBranch on no-op (it didn't
		// need to compute one). Read the current value so the message
		// is informative instead of `(<empty>) — no labels updated`.
		current := ""
		if w, err := mgr.Find(ctx, wsName); err == nil {
			current = w.Branch
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"%s: branch already in sync (%s) — no labels updated.\n",
			wsName, current)
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"%s: %s -> %s   (tmux session %s -> %s)\n",
		wsName, res.OldBranch, res.NewBranch, res.OldSession, res.NewSession)
	return nil
}

// resolveTargetWorkspace picks which workspace to sync. Order:
//
//  1. Explicit positional arg → trust the caller.
//  2. Inside a canopy tmux session → match by TmuxSession field.
//  3. cwd is inside a workspace dir → match by Path prefix.
//  4. Otherwise → actionable error directing the user to pass a name.
//
// The cwd fallback is the difference between "rename only works from
// inside the workspace's own tmux session" and "rename works from any
// shell that's cd'd into the workspace dir." The latter matters because
// a user might rename branches from their editor's terminal pane (which
// runs outside canopy's tmux session) and still want one command to
// sync the labels.
func resolveTargetWorkspace(ctx context.Context, mgr *workspace.Manager, args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}

	wss, err := mgr.List(ctx)
	if err != nil {
		return "", fmt.Errorf("rename: list workspaces: %w", err)
	}

	// Try tmux session first: that's the strongest "I'm in workspace X"
	// signal because tmux session names are unique and managed by canopy.
	t := tmux.New()
	if sessionName, err := t.CurrentSession(ctx); err == nil && sessionName != "" {
		for _, w := range wss {
			if w.TmuxSessionName() == sessionName {
				return w.Name, nil
			}
		}
	}

	// Fall back to cwd: useful when the user runs `canopy rename` from
	// an editor terminal, a popup shell, or any pane that isn't part of
	// canopy's session. Match by longest workspace.Path prefix so a
	// nested-worktree edge case still picks the right row.
	//
	// Symlink discipline: we ALWAYS compare resolved-to-resolved paths.
	// If EvalSymlinks fails on either side (broken symlink, permission
	// denied), skip that comparison entirely rather than mix raw and
	// resolved paths — mixing causes false negatives where one side is
	// `~/work/canopy-cwi` (symlink) and the other is `/home/avi/.canopy/...`
	// (canonical).
	cwd, err := os.Getwd()
	if err == nil && cwd != "" {
		realCwd, cwdErr := filepath.EvalSymlinks(cwd)
		var bestName string
		var bestLen int
		if cwdErr == nil {
			for _, w := range wss {
				if w.Path == "" {
					continue
				}
				realWS, wsErr := filepath.EvalSymlinks(w.Path)
				if wsErr != nil {
					continue // can't compare reliably; skip
				}
				if realCwd == realWS || strings.HasPrefix(realCwd, realWS+string(filepath.Separator)) {
					if len(realWS) > bestLen {
						bestName = w.Name
						bestLen = len(realWS)
					}
				}
			}
		}
		if bestName != "" {
			return bestName, nil
		}
	}

	return "", fmt.Errorf("rename: not inside a canopy workspace (no tmux session match, cwd not under any workspace dir) — pass the workspace name explicitly: canopy rename <workspace>")
}
