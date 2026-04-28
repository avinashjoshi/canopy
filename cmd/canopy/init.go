package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// initFlags holds the parsed flag values for `canopy init`.
var initFlags struct {
	force       bool
	withScripts bool
}

// initCmd returns the `canopy init` cobra subcommand.
//
// Onboards a project to canopy by dropping a minimal canopy.json into
// the current directory. Scripts are optional: by default the generated
// canopy.json has empty scripts and canopy will create workspaces with
// no setup hook, no server command, and no archive — fine for projects
// that just want git worktrees + tmux sessions.
//
// --with-scripts also writes stubs at bin/canopy-{setup,run,archive}
// for projects that want to grow into the full pattern.
//
// If a conductor.json exists, init mirrors its script paths into the
// new canopy.json (Conductor's schema is identical to canopy's). Stub
// scripts are not written in this mode — Conductor projects already
// have working scripts under bin/conductor-*.
//
// Refuses to overwrite an existing canopy.json unless --force is set.
func initCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Onboard the current directory to canopy (creates canopy.json)",
		Long: "Drops a minimal canopy.json into the current directory.\n\n" +
			"By default the canopy.json has no scripts — canopy will create\n" +
			"workspaces with just a worktree + tmux session, no setup or run\n" +
			"hooks. Pass --with-scripts to also generate stub scripts at\n" +
			"bin/canopy-{setup,run,archive} you can customize.\n\n" +
			"If a conductor.json exists in the current directory, init mirrors\n" +
			"its script paths into canopy.json verbatim — Conductor's schema is\n" +
			"identical to canopy's. The bin/conductor-* scripts are NOT copied\n" +
			"or renamed; canopy invokes them directly.",
		// Already-initialized is a friendly "you're done" state, not an
		// error worthy of a usage block. SilenceUsage keeps the cobra
		// help text from printing when we return early below.
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("init: getwd: %w", err)
			}

			canopyJSON := filepath.Join(cwd, "canopy.json")
			if _, err := os.Stat(canopyJSON); err == nil && !initFlags.force {
				// Friendly path: this project is already initialized. Print a
				// helpful message, exit 0 — `canopy init` is idempotent in
				// spirit. --force is documented inline so the user doesn't
				// have to read --help.
				fmt.Fprintf(cmd.OutOrStdout(),
					"%s already exists. This project is already initialized.\n",
					canopyJSON)
				fmt.Fprintln(cmd.OutOrStdout(), "")
				fmt.Fprintln(cmd.OutOrStdout(), "  - Run `canopy new` to create a workspace.")
				fmt.Fprintln(cmd.OutOrStdout(), "  - Run `canopy init --force` to regenerate canopy.json.")
				fmt.Fprintln(cmd.OutOrStdout(), "  - Run `canopy init --with-scripts --force` to also write bin/canopy-* stubs.")
				return nil
			}

			// If a conductor.json sits next to us, mirror its scripts. The
			// presence of a conductor.json takes precedence over --with-scripts:
			// the user already has working scripts and we shouldn't generate
			// stubs that would shadow them.
			scripts, source := readConductor(cwd)
			generatedStubs := false
			if scripts != nil {
				// Conductor mode — use conductor.json's script paths verbatim.
			} else if initFlags.withScripts {
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
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", canopyJSON)
			if source != "" {
				fmt.Fprintf(cmd.OutOrStdout(),
					"  (mirrored scripts from %s — canopy uses the same schema)\n", source)
			}

			if generatedStubs {
				written, err := writeStubScripts(cwd, scripts)
				if err != nil {
					return err
				}
				for _, p := range written {
					fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", p)
				}
			}

			fmt.Fprintln(cmd.OutOrStdout(), "")
			fmt.Fprintln(cmd.OutOrStdout(), "Next steps:")
			switch {
			case source != "":
				fmt.Fprintln(cmd.OutOrStdout(), "  1. Review canopy.json and confirm the script paths look right.")
				fmt.Fprintln(cmd.OutOrStdout(), "  2. Commit canopy.json.")
				fmt.Fprintln(cmd.OutOrStdout(), "  3. Run `canopy new` to verify.")
				fmt.Fprintln(cmd.OutOrStdout(), "")
				fmt.Fprintln(cmd.OutOrStdout(), "  Your existing bin/conductor-* scripts and any config files reading")
				fmt.Fprintln(cmd.OutOrStdout(), "  CONDUCTOR_* env vars will keep working — canopy exports the CONDUCTOR_*")
				fmt.Fprintln(cmd.OutOrStdout(), "  aliases alongside CANOPY_* for migration compatibility.")
			case generatedStubs:
				fmt.Fprintln(cmd.OutOrStdout(), "  1. Edit bin/canopy-setup to install deps and prepare the workspace.")
				fmt.Fprintln(cmd.OutOrStdout(), "  2. Edit bin/canopy-run with your dev-server command (or delete it if not needed).")
				fmt.Fprintln(cmd.OutOrStdout(), "  3. Edit bin/canopy-archive to drop databases / kill processes (or delete it if not needed).")
				fmt.Fprintln(cmd.OutOrStdout(), "  4. Commit canopy.json and bin/canopy-*.")
				fmt.Fprintln(cmd.OutOrStdout(), "  5. Run `canopy new` to create your first workspace.")
			default:
				fmt.Fprintln(cmd.OutOrStdout(), "  Run `canopy new` to create your first workspace — canopy will spin up")
				fmt.Fprintln(cmd.OutOrStdout(), "  a worktree + tmux session with no setup hook.")
				fmt.Fprintln(cmd.OutOrStdout(), "")
				fmt.Fprintln(cmd.OutOrStdout(), "  Want hooks? Re-run `canopy init --with-scripts --force` to scaffold")
				fmt.Fprintln(cmd.OutOrStdout(), "  bin/canopy-{setup,run,archive} stubs you can fill in.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&initFlags.force, "force", false, "overwrite an existing canopy.json")
	cmd.Flags().BoolVar(&initFlags.withScripts, "with-scripts", false,
		"also write stub bin/canopy-{setup,run,archive} scripts (ignored when a conductor.json is detected)")
	return cmd
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
