package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/state"
)

// initOptions are the user-facing flags that govern runInit's behavior.
// Decoupled from cobra so the splash screen flow (which exits Bubbletea
// then calls runInit synchronously) doesn't have to construct cobra args.
type initOptions struct {
	Force       bool
	WithScripts bool
}

// initFlags holds the parsed flag values for `canopy init`.
var initFlags struct {
	force       bool
	withScripts bool
}

// initCmd returns the `canopy init` cobra subcommand.
//
// Three call shapes (v0.18+):
//
//   - `canopy init`                          init the cwd (backwards-compat)
//   - `canopy init <path>`                   init <path> (no cd-ing in)
//   - `canopy init <git-url>`                clone to source-root, init
//   - `canopy init <git-url> <dest>`         clone to <dest>, init
//
// Scripts are optional: by default the generated canopy.json has empty
// scripts and canopy will create workspaces with no setup hook, no
// server command, and no archive — fine for projects that just want
// git worktrees + tmux sessions.
//
// --with-scripts also writes stubs at bin/canopy-{setup,run,archive}
// for projects that want to grow into the full pattern.
//
// If a conductor.json exists in the resolved init dir, init mirrors
// its script paths into the new canopy.json. Stub scripts are not
// written in this mode — Conductor projects already have working
// scripts under bin/conductor-*.
//
// Refuses to overwrite an existing canopy.json unless --force is set.
// However, if canopy.json exists AND the project isn't yet in
// state.json (typical post-clone), init still REGISTERS the project
// so `canopy ls` sees it — see runInit's bug-fix comment.
func initCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init [path-or-url] [dest]",
		Short: "Onboard a project to canopy (creates canopy.json)",
		Long: "Onboard a project to canopy. Accepts:\n\n" +
			"  canopy init                              init the current directory\n" +
			"  canopy init <path>                       init <path> without cd-ing in\n" +
			"  canopy init <git-url>                    clone to source-root, then init\n" +
			"  canopy init <git-url> <dest>             clone to <dest>, then init\n\n" +
			"Source-root precedence (clone target): explicit <dest> > $CANOPY_SOURCE_ROOT >\n" +
			"~/.canopy/config.json source-root > default ~/.canopy/sources.\n\n" +
			"Auth (private URLs): canopy delegates to git, so SSH agent + HTTPS credential\n" +
			"helpers + host-key prompts work the same as a plain `git clone`.\n\n" +
			"Cloned dir already exists with .git: skip clone, init in place (idempotent).\n" +
			"Dest exists but isn't a git repo: refuse with a collision error.\n\n" +
			"By default canopy.json has empty scripts. Pass --with-scripts to scaffold\n" +
			"bin/canopy-{setup,run,archive} stubs. Conductor projects auto-mirror their\n" +
			"conductor.json scripts (stubs are skipped — bin/conductor-* already work).",
		Args: cobra.MaximumNArgs(2),
		// Already-initialized is a friendly "you're done" state, not an
		// error worthy of a usage block. SilenceUsage keeps the cobra
		// help text from printing when we return early below.
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			arg, dest := "", ""
			if len(args) > 0 {
				arg = args[0]
			}
			if len(args) > 1 {
				dest = args[1]
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("init: home dir: %w", err)
			}
			canopyHome := filepath.Join(home, ".canopy")
			_, err = runAddProject(cmd.Context(), arg, addProjectOptions{
				Force:        initFlags.force,
				WithScripts:  initFlags.withScripts,
				DestOverride: dest,
			}, cmd.OutOrStdout(), canopyHome)
			return err
		},
	}
	cmd.Flags().BoolVar(&initFlags.force, "force", false, "overwrite an existing canopy.json")
	cmd.Flags().BoolVar(&initFlags.withScripts, "with-scripts", false,
		"also write stub bin/canopy-{setup,run,archive} scripts (ignored when a conductor.json is detected)")
	return cmd
}

// runInit is the extracted body of `canopy init`'s RunE. The cobra command
// is a thin wrapper; the splash screen flow (init splash → tea.Quit → main
// calls runInit) calls this directly so the same code path runs whether
// the user typed `canopy init` or pressed `i` in the splash.
//
// runInit's contract:
//
//  1. If canopy.json exists and !Force: print friendly "already initialized"
//     and return nil. Idempotent in spirit; the caller doesn't have to check.
//  2. Resolve cwd to its canonical absolute path (EvalSymlinks). Check
//     state.json for a basename collision with any OTHER registered project.
//     If collision: refuse with a clear error pointing at both paths,
//     leaving disk untouched.
//  3. Write canopy.json (with conductor.json mirror if present, or stub
//     scripts if WithScripts).
//  4. Register the project in state.Projects so canopy ls (global mode)
//     and the TUI know about it before the first canopy new. PortBase is
//     left zero — port allocation still happens lazily on first workspace
//     creation, same as before v0.5.
//  5. Print the next-steps block.
//
// Errors at any step are returned wrapped with %w. Steps 3 and 4 happen
// in that order so a state-write failure doesn't leave the user with a
// canopy.json the system doesn't know about — actually wait, that's the
// risk we accept: writing canopy.json first means a partial init can
// leave a canopy.json without a state entry. Mitigation: workspace.New
// will create the state entry lazily on first use anyway, so the worst
// case is a benign state-not-yet-written window between init and new.
func runInit(cwd string, opts initOptions, stdout io.Writer) error {
	canopyJSON := filepath.Join(cwd, "canopy.json")
	if _, err := os.Stat(canopyJSON); err == nil && !opts.Force {
		// Already initialized — friendly path, exit 0.
		//
		// v0.18 fix: also register the project in state.json if it
		// isn't already. The pre-v0.18 early-return skipped registration
		// entirely, so cloning a repo that shipped canopy.json (e.g.
		// canopy itself) would leave the project invisible to
		// `canopy ls`. The registerProject helper is idempotent — a
		// no-op if the project is already registered — so this is
		// safe to run unconditionally on the existing-canopy.json path.
		fmt.Fprintf(stdout,
			"%s already exists. This project is already initialized.\n",
			canopyJSON)
		canonicalRoot, cerr := canonicalize(cwd)
		if cerr == nil {
			if rerr := registerProject(canonicalRoot, filepath.Base(canonicalRoot)); rerr != nil {
				fmt.Fprintf(stdout,
					"warning: project not in state.json and couldn't register it: %v\n", rerr)
			}
		}
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "  - Run `canopy new` to create a workspace.")
		fmt.Fprintln(stdout, "  - Run `canopy init --force` to regenerate canopy.json.")
		fmt.Fprintln(stdout, "  - Run `canopy init --with-scripts --force` to also write bin/canopy-* stubs.")
		return nil
	}

	// Canonicalize cwd the same way config.Load does (EvalSymlinks → abs).
	// This is the key we'll register in state.Projects, so it has to match
	// what subsequent canopy.json discovery will produce.
	canonicalRoot, err := canonicalize(cwd)
	if err != nil {
		return fmt.Errorf("init: canonicalize cwd: %w", err)
	}
	basename := filepath.Base(canonicalRoot)

	// Basename uniqueness gate. Open state.json (read-only at this point)
	// and check for collisions BEFORE writing anything to disk. If a
	// collision exists, we refuse — the user must rename one of the
	// directories or hand-edit state.json. Pre-write check means a refused
	// init leaves disk untouched.
	if err := guardBasenameCollision(canonicalRoot, basename); err != nil {
		return err
	}

	// If a conductor.json sits next to us, mirror its scripts. The
	// presence of a conductor.json takes precedence over --with-scripts:
	// the user already has working scripts and we shouldn't generate
	// stubs that would shadow them.
	scripts, source := readConductor(cwd)
	generatedStubs := false
	if scripts != nil {
		// Conductor mode — use conductor.json's script paths verbatim.
	} else if opts.WithScripts {
		// Fresh project + opted in to scaffolding.
		scripts = stubScripts()
		generatedStubs = true
	} else {
		// Fresh project, default mode: empty scripts. canopy will
		// create workspaces with no hooks until the user fills them in.
		scripts = &canopyScripts{}
	}

	if err := writeCanopyJSON(canopyJSON, scripts); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wrote %s\n", canopyJSON)
	if source != "" {
		fmt.Fprintf(stdout,
			"  (mirrored scripts from %s — canopy uses the same schema)\n", source)
	}

	if generatedStubs {
		written, err := writeStubScripts(cwd, scripts)
		if err != nil {
			return err
		}
		for _, p := range written {
			fmt.Fprintf(stdout, "Wrote %s\n", p)
		}
	}

	// Register the project in state.Projects under the canonical root key.
	// PortBase stays zero until first workspace creation. Best-effort: a
	// failure here doesn't roll back the canopy.json write (workspace.New
	// will reconcile on first use), but we still surface the error so the
	// user knows.
	if err := registerProject(canonicalRoot, basename); err != nil {
		fmt.Fprintf(stdout,
			"warning: registered canopy.json but couldn't update state.json: %v\n", err)
		fmt.Fprintln(stdout, "  Project will be registered automatically on first `canopy new`.")
	}

	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Next steps:")
	switch {
	case source != "":
		fmt.Fprintln(stdout, "  1. Review canopy.json and confirm the script paths look right.")
		fmt.Fprintln(stdout, "  2. Commit canopy.json.")
		fmt.Fprintln(stdout, "  3. Run `canopy new` to verify.")
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "  Your existing bin/conductor-* scripts and any config files reading")
		fmt.Fprintln(stdout, "  CONDUCTOR_* env vars will keep working — canopy exports the CONDUCTOR_*")
		fmt.Fprintln(stdout, "  aliases alongside CANOPY_* for migration compatibility.")
	case generatedStubs:
		fmt.Fprintln(stdout, "  1. Edit bin/canopy-setup to install deps and prepare the workspace.")
		fmt.Fprintln(stdout, "  2. Edit bin/canopy-run with your dev-server command (or delete it if not needed).")
		fmt.Fprintln(stdout, "  3. Edit bin/canopy-archive to drop databases / kill processes (or delete it if not needed).")
		fmt.Fprintln(stdout, "  4. Commit canopy.json and bin/canopy-*.")
		fmt.Fprintln(stdout, "  5. Run `canopy new` to create your first workspace.")
	default:
		fmt.Fprintln(stdout, "  Run `canopy new` to create your first workspace — canopy will spin up")
		fmt.Fprintln(stdout, "  a worktree + tmux session with no setup hook.")
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "  Want hooks? Re-run `canopy init --with-scripts --force` to scaffold")
		fmt.Fprintln(stdout, "  bin/canopy-{setup,run,archive} stubs you can fill in.")
	}
	return nil
}

// canonicalize returns the canonical absolute path for dir, with symlinks
// resolved. Mirrors config.Load's logic so init's basename-uniqueness
// check uses the same key the rest of canopy will see for this project.
func canonicalize(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// guardBasenameCollision opens state.json read-only and refuses if any
// other project shares the basename. No-op if state.json doesn't exist
// (first-ever init). Returns a user-readable error pointing at both paths
// so the user can fix things by hand.
//
// Used as the early gate (before writing canopy.json) so a refused init
// leaves the user's disk fully untouched. registerProject re-checks under
// the state lock to close the TOCTOU window.
func guardBasenameCollision(canonicalRoot, basename string) error {
	store, err := openStateForInit()
	if err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("init: load state.json: %w", err)
	}
	if other := st.FindBasenameCollision(canonicalRoot); other != "" {
		return collisionError(basename, other, canonicalRoot)
	}
	return nil
}

// collisionError formats the user-facing message for a refused init. Used
// by both the early gate and the inside-lock recheck so the wording stays
// consistent.
func collisionError(basename, otherRoot, thisRoot string) error {
	return fmt.Errorf(
		"canopy init: project basename %q is already registered for %q.\n"+
			"  This canopy.json would create a second project also named %q at %q.\n"+
			"  canopy doesn't yet support same-named projects. Options:\n"+
			"    - Rename one of the directories.\n"+
			"    - Hand-edit ~/.canopy/state.json to remove the stale entry if that project is gone.",
		basename, otherRoot, basename, thisRoot)
}

// registerProject creates an entry in state.Projects for canonicalRoot if
// one doesn't already exist. PortBase stays zero — lazy port allocation
// in workspace.Create assigns one on first use.
//
// Holds the state lock for the full check + write window so a concurrent
// canopy init in a different terminal can't sneak a colliding registration
// in between guardBasenameCollision and this call.
//
// Idempotent: if the entry already exists (re-running init --force on a
// known project), this is a no-op.
func registerProject(canonicalRoot, basename string) error {
	store, err := openStateForInit()
	if err != nil {
		return err
	}
	return store.WithLock(func(s *state.State) error {
		// Re-check inside the lock — closes the TOCTOU between the early
		// guard and the registration write.
		if other := s.FindBasenameCollision(canonicalRoot); other != "" {
			return collisionError(basename, other, canonicalRoot)
		}
		if s.Projects == nil {
			s.Projects = map[string]state.ProjectMeta{}
		}
		if _, ok := s.Projects[canonicalRoot]; ok {
			return nil // already registered, nothing to do
		}
		s.Projects[canonicalRoot] = state.ProjectMeta{Root: canonicalRoot}
		return nil
	})
}

// openStateForInit returns a state.Store rooted at ~/.canopy. Same as the
// one in ls.go's openStateReadOnly but kept locally to avoid threading a
// dependency between unrelated subcommands.
func openStateForInit() (*state.Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("init: home dir: %w", err)
	}
	return state.NewStore(filepath.Join(home, ".canopy"))
}

// readConductor returns the scripts block from a conductor.json sibling
// of cwd, plus the source path used (empty if no conductor.json exists
// or it didn't parse). Conductor's schema is identical to canopy's, so
// we just lift the three string paths.
func readConductor(cwd string) (*canopyScripts, string) {
	path := filepath.Join(cwd, "conductor.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ""
	}
	if err != nil {
		return nil, ""
	}
	var doc struct {
		Scripts canopyScripts `json:"scripts"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, ""
	}
	// Mirror as long as conductor.json declares at least one script —
	// scripts are optional in canopy too, so a partial conductor.json is
	// still useful to copy. An entirely empty conductor.json is treated
	// as "no conductor here" so we fall through to default-init behavior.
	if doc.Scripts.Setup == "" && doc.Scripts.Run == "" && doc.Scripts.Archive == "" {
		return nil, ""
	}
	return &doc.Scripts, path
}

// canopyScripts mirrors config.Scripts. Defined here separately so cmd/
// doesn't import config just for the JSON shape (cmd already imports
// config via loadManager but a small amount of duplication keeps init
// independent of config validation rules).
//
// omitempty keeps a default `canopy init` output looking clean —
// `{"scripts":{}}` rather than three empty strings.
type canopyScripts struct {
	Setup   string `json:"setup,omitempty"`
	Run     string `json:"run,omitempty"`
	Archive string `json:"archive,omitempty"`
}

// stubScripts returns the canonical canopy.json paths for a fresh
// project that has no Conductor history.
func stubScripts() *canopyScripts {
	return &canopyScripts{
		Setup:   "bin/canopy-setup",
		Run:     "bin/canopy-run",
		Archive: "bin/canopy-archive",
	}
}

// writeCanopyJSON writes the script block to a JSON file with stable
// formatting (two-space indent, trailing newline) so the file looks
// like something a human would commit.
//
// Uses json.Encoder with SetEscapeHTML(false) so shell metachars in
// script values (the `&&` in "rm .sock && bin/dev") stay readable as
// `&&`, not the default `&&`. JSON is still valid either way;
// the unescaped form is what humans want to read.
func writeCanopyJSON(path string, scripts *canopyScripts) error {
	doc := struct {
		Scripts *canopyScripts `json:"scripts"`
	}{Scripts: scripts}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("init: create %s: %w", path, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("init: encode: %w", err)
	}
	return nil
}

// writeStubScripts drops three executable scripts at the paths named in
// scripts. Skips any that already exist (preserving user customizations).
// Returns the list of paths actually written.
func writeStubScripts(cwd string, scripts *canopyScripts) ([]string, error) {
	pairs := []struct {
		path string
		body string
	}{
		{scripts.Setup, stubSetup},
		{scripts.Run, stubRun},
		{scripts.Archive, stubArchive},
	}
	written := []string{}
	for _, p := range pairs {
		fullPath := filepath.Join(cwd, p.path)
		if _, err := os.Stat(fullPath); err == nil {
			continue // skip; preserve existing
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return written, fmt.Errorf("init: mkdir %s: %w", filepath.Dir(fullPath), err)
		}
		if err := os.WriteFile(fullPath, []byte(p.body), 0o755); err != nil {
			return written, fmt.Errorf("init: write %s: %w", fullPath, err)
		}
		written = append(written, fullPath)
	}
	return written, nil
}

// Stub script bodies. Each is a runnable bash script that documents the
// CANOPY_* env vars and exits 0 cleanly. The user replaces the TODO
// section with their actual project setup.

const stubSetup = `#!/usr/bin/env bash
# canopy setup: runs once when canopy creates a new workspace.
#
# Available env vars (set by canopy before this script is invoked):
#   CANOPY_WORKSPACE_PATH  absolute path to the new workspace dir
#   CANOPY_ROOT_PATH       absolute path to the original repo root
#   CANOPY_PORT            allocated TCP port for this workspace
#
# Common things to do here:
#   - install dependencies (bundle install, npm install, go mod download)
#   - symlink shared secrets from $CANOPY_ROOT_PATH (.env, credentials)
#   - create per-workspace databases keyed by $CANOPY_PORT
#   - copy or template config files
#
# On failure, canopy marks the workspace as "broken" and surfaces the
# error to the user. The setup script can be re-run by removing the
# workspace (canopy rm) and creating a new one.
set -euo pipefail
cd "${CANOPY_WORKSPACE_PATH}"

echo "TODO: customize bin/canopy-setup for your project"
echo "  workspace: ${CANOPY_WORKSPACE_PATH}"
echo "  root:      ${CANOPY_ROOT_PATH}"
echo "  port:      ${CANOPY_PORT}"
`

const stubRun = `#!/usr/bin/env bash
# canopy run: the long-running command for the workspace's server pane.
#
# This is what tmux launches in the bottom-right pane. When the user
# hits prefix-d to detach, this command keeps running. When canopy
# resurrects a stopped workspace, this command is re-launched.
#
# Common values:
#   bin/dev                          (Rails / Procfile-based apps)
#   npm run dev                      (Next.js, Vite, etc.)
#   mix phx.server                   (Phoenix)
#   go run ./cmd/server              (Go HTTP services)
#
# CANOPY_PORT is set so your server can bind to a unique port per
# workspace and avoid collisions when running multiple branches at once.
set -euo pipefail
cd "${CANOPY_WORKSPACE_PATH}"

echo "TODO: replace this with your dev-server command"
echo "Press Ctrl-C to stop. Workspace listening on port ${CANOPY_PORT}."

# Keep the pane alive until you replace this with your real command.
exec sleep infinity
`

const stubArchive = `#!/usr/bin/env bash
# canopy archive: runs when canopy removes a workspace.
#
# Common things to do here:
#   - drop the per-workspace database
#   - kill any background processes that started in setup
#   - remove temporary files
#
# Failures here are logged but don't block removal. Canopy's removal
# always proceeds: scripts.archive -> tmux kill -> git worktree remove
# -> state row drop. A best-effort full removal is better than a
# half-removed workspace stuck in state.
set -euo pipefail

echo "TODO: customize bin/canopy-archive for your project"
echo "  workspace: ${CANOPY_WORKSPACE_PATH}"
echo "  port:      ${CANOPY_PORT}"
`
