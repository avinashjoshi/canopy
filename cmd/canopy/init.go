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

// initFlags holds parsed --force (overwrite existing canopy.json).
var initFlags struct {
	force bool
}

// initCmd returns the `canopy init` cobra subcommand.
//
// Onboards a project to canopy by dropping a canopy.json plus stub
// scripts at bin/canopy-{setup,run,archive}. Detects an existing
// conductor.json (Conductor's config) and offers to translate it.
//
// Refuses to overwrite an existing canopy.json unless --force is set.
func initCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Onboard the current directory to canopy (creates canopy.json + bin/canopy-* stubs)",
		Long: "Drops a canopy.json and three stub scripts (bin/canopy-{setup,run,archive})\n" +
			"into the current directory. Edit the scripts to match your project's needs,\n" +
			"then commit them and run `canopy new`.\n\n" +
			"If a conductor.json exists in the current directory, init will copy its\n" +
			"script paths into canopy.json verbatim — Conductor's schema is identical\n" +
			"to canopy's. The bin/conductor-* scripts are NOT copied or renamed; you\n" +
			"likely want to keep them and have canopy invoke them directly.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("init: getwd: %w", err)
			}

			canopyJSON := filepath.Join(cwd, "canopy.json")
			if _, err := os.Stat(canopyJSON); err == nil && !initFlags.force {
				return fmt.Errorf("init: %s already exists (pass --force to overwrite)", canopyJSON)
			}

			// If a conductor.json sits next to us, mirror its scripts. Otherwise
			// emit stub paths.
			scripts, source := readConductor(cwd)
			if scripts == nil {
				scripts = stubScripts()
			}

			if err := writeCanopyJSON(canopyJSON, scripts); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", canopyJSON)
			if source != "" {
				fmt.Fprintf(cmd.OutOrStdout(),
					"  (mirrored scripts from %s — canopy uses the same schema)\n", source)
			}

			// Write stub scripts only when the source was the stub list (no
			// conductor.json present). If the user already has bin/conductor-*
			// from Conductor, canopy's pointing at them is enough — we don't
			// want to clobber working scripts.
			if source == "" {
				written, err := writeStubScripts(cwd, scripts)
				if err != nil {
					return err
				}
				for _, p := range written {
					fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", p)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "")
				fmt.Fprintln(cmd.OutOrStdout(), "Next steps:")
				fmt.Fprintln(cmd.OutOrStdout(), "  1. Edit bin/canopy-setup to install deps and prepare the workspace.")
				fmt.Fprintln(cmd.OutOrStdout(), "  2. Edit bin/canopy-run with your dev-server command.")
				fmt.Fprintln(cmd.OutOrStdout(), "  3. Edit bin/canopy-archive to drop databases / kill processes.")
				fmt.Fprintln(cmd.OutOrStdout(), "  4. Commit canopy.json and bin/canopy-*.")
				fmt.Fprintln(cmd.OutOrStdout(), "  5. Run `canopy new` to create your first workspace.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "")
				fmt.Fprintln(cmd.OutOrStdout(), "Next steps:")
				fmt.Fprintln(cmd.OutOrStdout(), "  1. Review canopy.json and confirm the script paths are correct.")
				fmt.Fprintln(cmd.OutOrStdout(), "  2. If your scripts read CONDUCTOR_PORT / CONDUCTOR_WORKSPACE_PATH /")
				fmt.Fprintln(cmd.OutOrStdout(), "     CONDUCTOR_ROOT_PATH from env, switch them to the CANOPY_* equivalents.")
				fmt.Fprintln(cmd.OutOrStdout(), "  3. If config files (database.yml, etc.) reference CONDUCTOR_*, update those too.")
				fmt.Fprintln(cmd.OutOrStdout(), "  4. Commit canopy.json.")
				fmt.Fprintln(cmd.OutOrStdout(), "  5. Run `canopy new` to verify.")
			}

			return nil
		},
	}
	cmd.Flags().BoolVar(&initFlags.force, "force", false, "overwrite an existing canopy.json")
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
	if doc.Scripts.Setup == "" || doc.Scripts.Run == "" || doc.Scripts.Archive == "" {
		return nil, ""
	}
	return &doc.Scripts, path
}

// canopyScripts mirrors config.Scripts. Defined here separately so cmd/
// doesn't import config just for the JSON shape (cmd already imports
// config via loadManager but a small amount of duplication keeps init
// independent of config validation rules).
type canopyScripts struct {
	Setup   string `json:"setup"`
	Run     string `json:"run"`
	Archive string `json:"archive"`
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
