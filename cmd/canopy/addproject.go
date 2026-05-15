// addproject.go — shared orchestrator for `canopy init [PATH_OR_URL] [DEST]`.
//
// Single code path callable from:
//
//   - CLI: cobra `canopy init` RunE wraps runAddProject and prints to
//     cmd.OutOrStdout(). Clone runs synchronously, inheriting the tty
//     so git's auth prompts (SSH passphrase, HTTPS credential helpers)
//     work transparently.
//
//   - TUI: addProjectFormMode (Phase C) calls runAddProject from a
//     tea.ExecProcess callback after dropping altscreen, so git gets
//     the real tty. Same code, same outcome.
//
// Flow:
//
//	  arg empty / path  arg URL
//	       │              │
//	       ▼              ▼
//	  abs(arg)        pre-clone basename + path-safety checks
//	       │              ▼
//	       │         resolveCloneDest → ensureSourceRoot
//	       │              ▼
//	       │         existing .git? skip clone (idempotent)
//	       │         existing non-git dir? error: collision
//	       │              ▼
//	       │         git.Clone(ctx, url, dest)
//	       │              │
//	       └──────┬───────┘
//	              ▼
//	     runInit(dest, opts, stdout)
//	     (canonicalize, basename check, write canopy.json, register state)

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
)

// addProjectOptions carries the user-facing flags for the unified
// init/add-project flow. Mirrors initOptions but adds the per-call
// dest override so the TUI and CLI share one struct.
type addProjectOptions struct {
	// Force overwrites an existing canopy.json. Passed through to runInit.
	Force bool
	// WithScripts scaffolds bin/canopy-* stubs. Passed through to runInit.
	WithScripts bool
	// DestOverride is the optional 2nd positional / --to flag. Empty means
	// "compute dest from source-root + URL basename". Ignored for non-URL
	// args (runAddProject errors out — you can't override a local path's
	// own dest).
	DestOverride string
}

// runAddProject is the orchestrator. Returns the absolute path of the
// added project (so the TUI can render "✓ Added <name> at <path>") and
// an error.
//
// Context cancellation aborts the clone (SIGKILLs git via
// exec.CommandContext). Local-path inits are synchronous filesystem
// work and don't honor cancellation — they're sub-100ms anyway.
//
// stdout is where init's "Wrote canopy.json" + next-steps print. Clone
// progress (git's "Cloning into 'X'..." + counting objects) also goes
// to stdout via Clone's writer arg.
//
// canopyHome is the path to ~/.canopy. Threaded explicitly so tests
// can inject t.TempDir() without monkey-patching HOME.
func runAddProject(ctx context.Context, arg string, opts addProjectOptions, stdout io.Writer, canopyHome string) (absDest string, err error) {
	// Branch 1: empty arg → cwd, today's behavior preserved.
	if arg == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("canopy init: getwd: %w", err)
		}
		if opts.DestOverride != "" {
			return "", errors.New("canopy init: cannot pass <dest> without a URL or path as the first arg")
		}
		return cwd, runInit(cwd, initOptions{Force: opts.Force, WithScripts: opts.WithScripts}, stdout)
	}

	// Branch 2: arg is a local path.
	if !looksLikeGitURL(arg) {
		if opts.DestOverride != "" {
			return "", errors.New("canopy init: <dest> only valid when first arg is a git URL")
		}
		abs, err := filepath.Abs(arg)
		if err != nil {
			return "", fmt.Errorf("canopy init: resolve path: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("canopy init: %s: %w", abs, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("canopy init: %s is not a directory", abs)
		}
		return abs, runInit(abs, initOptions{Force: opts.Force, WithScripts: opts.WithScripts}, stdout)
	}

	// Branch 3: arg is a git URL. Pre-clone safety checks first, then
	// clone (or skip if idempotent), then runInit on the result.
	return runAddProjectFromURL(ctx, arg, opts, stdout, canopyHome)
}

// runAddProjectFromURL handles the URL branch of runAddProject. Split
// out so the orchestrator stays readable; the URL flow has 4 separate
// failure modes that each need their own error.
func runAddProjectFromURL(ctx context.Context, rawURL string, opts addProjectOptions, stdout io.Writer, canopyHome string) (absDest string, err error) {
	// Load user config so source-root precedence works. Reads outside
	// the lock are fine here (decision: config Load is snapshot-safe).
	store, err := config.NewUserStore(canopyHome)
	if err != nil {
		return "", fmt.Errorf("canopy init: open user config: %w", err)
	}
	userCfg, err := store.Load()
	if err != nil {
		return "", fmt.Errorf("canopy init: load user config: %w", err)
	}

	// Resolve where the clone will land BEFORE doing any network work,
	// so a bad config / unparseable URL fails fast.
	dest, sourceLabel, err := resolveCloneDest(rawURL, opts.DestOverride, userCfg, canopyHome)
	if err != nil {
		return "", fmt.Errorf("canopy init: %w", err)
	}

	// Pre-clone basename collision check (design decision #6). Derives
	// the basename from the URL, asks state.json whether any *other*
	// project already owns that basename, refuses if so. Catches the
	// "you already have project 'bar' at /old/path" case BEFORE
	// downloading megabytes only to discard them.
	store2, err := openStateForInit()
	if err != nil {
		return "", err
	}
	st, err := store2.Load()
	if err != nil {
		return "", fmt.Errorf("canopy init: load state: %w", err)
	}
	if other := st.FindBasenameCollision(dest); other != "" {
		basename := filepath.Base(dest)
		return "", fmt.Errorf(
			"canopy init: project %q is already registered at %s. "+
				"To proceed:\n"+
				"  - Use a different destination: canopy init %s <other-dest>\n"+
				"  - Or edit ~/.canopy/state.json to remove the stale entry",
			basename, other, rawURL)
	}

	// Pre-clone path safety: refuse a dest that would land inside a
	// known canopy workspace (decision #7). Reconcile would treat the
	// clone as garbage and the user would get phantom data inside a
	// worktree.
	if err := validateDestNotInsideWorkspace(dest, st); err != nil {
		return "", fmt.Errorf("canopy init: %w", err)
	}

	// Decide between three sub-cases:
	//
	//   (a) dest doesn't exist → clone fresh
	//   (b) dest exists AND has .git → skip clone, init in place (idempotent)
	//   (c) dest exists, not a git repo → error: collision
	skipClone := false
	if info, err := os.Stat(dest); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("canopy init: %s exists and is not a directory", dest)
		}
		// Inspect .git to decide. A real worktree has a .git dir or
		// .git file; a random dir does not.
		if _, gerr := os.Stat(filepath.Join(dest, ".git")); gerr == nil {
			skipClone = true
			fmt.Fprintf(stdout, "Re-using existing repo at %s (idempotent rerun).\n", dest)
		} else {
			return "", fmt.Errorf(
				"canopy init: %s exists and isn't a git repo. "+
					"Pick a different destination, or remove the directory first",
				dest)
		}
	}

	if !skipClone {
		if err := ensureSourceRoot(dest); err != nil {
			return "", fmt.Errorf("canopy init: %w", err)
		}
		fmt.Fprintf(stdout, "Cloning %s into %s (source-root: %s)...\n", rawURL, dest, sourceLabel)
		if err := git.Clone(ctx, rawURL, dest, stdout); err != nil {
			return "", fmt.Errorf("canopy init: %w", err)
		}
	}

	// Now runInit on the cloned/existing dir. The bug-fix in init.go
	// (Phase B) ensures it registers even when canopy.json already
	// exists — critical for repos that ship canopy.json.
	if err := runInit(dest, initOptions{Force: opts.Force, WithScripts: opts.WithScripts}, stdout); err != nil {
		return dest, err
	}
	return dest, nil
}
