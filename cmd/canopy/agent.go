package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// agentCmd returns the `canopy agent <subcommand>` cobra group. The
// only subcommand in v0.22 is `swap <type>`. The noun-space is
// deliberately reserved for future read commands (eng-review D11):
//
//	canopy agent list         # show current/available agents (TODO)
//	canopy agent status       # show running state (TODO)
//	canopy agent reset        # kill + relaunch same agent (TODO)
//	canopy agent swap <type>  # swap to a different agent  ← v0.22
//
// Putting `swap` under the noun keeps the verb shape consistent across
// all future subcommands and avoids the verb-collision foot-gun a bare
// `canopy agent <type>` would have introduced.
func agentCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "agent",
		Short: "Manage the running agent in the current workspace",
		Long: "Subcommands for inspecting and manipulating the agent process running\n" +
			"in the current workspace's tmux session. v0.22 ships `swap`; `list`,\n" +
			"`status`, and `reset` are reserved noun-space for follow-ups.",
	}
	c.AddCommand(agentSwapCmd())
	return c
}

// agentSwapCmd returns the `canopy agent swap <type>` subcommand.
//
// Locates the workspace by walking up from cwd to find canopy.json
// (same machinery as canopy ls / switch / rm), then finds the workspace
// row whose Path is an ancestor of cwd. Calls Manager.SwapAgent which
// validates the target against canopy.json's agents allowlist, kills
// the current agent pane, persists the new type, respawns the new
// agent, and restores byte-precise window-layout.
//
// Exits 0 on success, 1 on validation/state errors (e.g.,
// ErrAgentNotAllowed, ErrSwapAlreadyCurrent, missing session). The
// session stays on whatever screen it was on before — the user reads
// the success line in their shell and tmux already shows the new
// agent's UI in the agent pane.
func agentSwapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "swap <type>",
		Short: "Swap the running agent in this workspace to <type>",
		Long: "Locates the workspace by walking up from cwd to find canopy.json,\n" +
			"then kills the running agent's pane and respawns <type> in the same\n" +
			"layout. Window geometry is preserved byte-precise; the agent's own\n" +
			"per-directory conversation history is preserved by the agent itself\n" +
			"(claude --continue, codex equivalent), not by tmux scrollback.\n\n" +
			"<type> must be in canopy.json's `agents:` allowlist. Returns\n" +
			"ErrAgentNotAllowed if not.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			newType := args[0]

			mgr, err := loadManager()
			if err != nil {
				return err
			}

			// Locate this workspace by walking up from cwd until we hit
			// a registered workspace path. Use the same cwd-walk that
			// `canopy rm` and `canopy switch` use implicitly.
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("canopy agent swap: getwd: %w", err)
			}
			name, err := findWorkspaceFromCwd(ctx, mgr, cwd)
			if err != nil {
				return err
			}

			// Capture the CURRENT agent BEFORE SwapAgent runs.
			// SwapAgent's returned *Workspace has CurrentAgent already
			// mutated to newType (Step 5 in agent_swap.go), so we can't
			// derive the "from" agent for the success message after the
			// fact. (ultrareview bug_002, 2026-06-26.)
			oldType, err := currentAgentForWorkspace(ctx, mgr, name)
			if err != nil {
				return fmt.Errorf("canopy agent swap: look up current agent: %w", err)
			}

			ws, err := mgr.SwapAgent(ctx, name, newType)
			if err != nil {
				// Pretty-print known sentinels so the user gets clean
				// guidance instead of a wrapped chain.
				if errors.Is(err, agent.ErrAgentNotAllowed) {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"canopy agent swap: %q is not in this project's `agents:` list.\n"+
							"Allowed: %v\n"+
							"Edit canopy.json to add it, or pick from the allowed list.\n",
						newType, mgr.Cfg.Agents)
					return err
				}
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"Swapped %s → %s in workspace %q.\n"+
					"The new agent has been spawned in the same pane geometry.\n"+
					"Run `canopy switch %s` to attach if you're not already there.\n",
				oldType, ws.CurrentAgent, ws.Name, ws.Name)
			return nil
		},
	}
}

// findWorkspaceFromCwd walks up from cwd looking for a registered
// workspace path. Returns the workspace name. If no workspace contains
// cwd, returns a clean error suggesting `canopy ls` to see available
// workspaces.
//
// Lives here (rather than in the workspace package) because it's
// CLI-shape concern: the workspace package operates on names; the CLI
// gets a name from the filesystem.
func findWorkspaceFromCwd(ctx context.Context, mgr *workspace.Manager, cwd string) (string, error) {
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("canopy agent swap: abs cwd: %w", err)
	}
	if resolved, lerr := filepath.EvalSymlinks(absCwd); lerr == nil {
		absCwd = resolved
	}
	list, err := mgr.List(ctx)
	if err != nil {
		return "", fmt.Errorf("canopy agent swap: list workspaces: %w", err)
	}
	for _, ws := range list {
		wsPath := ws.Path
		if resolved, lerr := filepath.EvalSymlinks(wsPath); lerr == nil {
			wsPath = resolved
		}
		// Walk up from cwd: workspace match when cwd == wsPath OR cwd
		// is inside wsPath. Use filepath.Rel to detect ancestry safely.
		rel, rerr := filepath.Rel(wsPath, absCwd)
		if rerr != nil {
			continue
		}
		if isInsideWorkspace(rel) {
			return ws.Name, nil
		}
	}
	return "", fmt.Errorf(
		"canopy agent swap: cwd %q is not inside any registered workspace; cd into a workspace first or run `canopy ls` to see them",
		cwd)
}

// currentAgentForWorkspace returns the named workspace's CurrentAgent
// as currently persisted in state.json. Used by the swap CLI to render
// the "from" half of the success message — must be read BEFORE
// SwapAgent runs, because SwapAgent mutates the row in place.
//
// Falls back to the project's default agent (canopy.json `agents[0]`,
// then legacy `agent.type`, then "claude") when the row hasn't been
// migrated yet — same fallback shape that workspace.currentAgent uses
// internally. (ultrareview bug_002, 2026-06-26.)
func currentAgentForWorkspace(ctx context.Context, mgr *workspace.Manager, name string) (string, error) {
	rows, err := mgr.List(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Name == name && r.ProjectRoot == mgr.Cfg.ProjectRoot {
			if r.CurrentAgent != "" {
				return r.CurrentAgent, nil
			}
			return mgr.Cfg.DefaultAgent(), nil
		}
	}
	return "", fmt.Errorf("workspace %q not found in this project", name)
}

// isInsideWorkspace reports whether `rel` (output of filepath.Rel(wsPath, cwd))
// indicates cwd is the workspace dir itself or a descendant of it.
//
// rel == "." → cwd IS wsPath. Otherwise the only signal that cwd is
// OUTSIDE wsPath is a leading "..". A naive "first char != '.'" test
// trips on dot-prefixed subdirectories (.github, .config, .gstack)
// because filepath.Rel returns ".github" for a cwd one level inside the
// workspace's .github directory — first char IS '.', but the path is
// still inside. (codex review P2 #3, 2026-06-25.)
func isInsideWorkspace(rel string) bool {
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, `..\`) {
		return false
	}
	return true
}
