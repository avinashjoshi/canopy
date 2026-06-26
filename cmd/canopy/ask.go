package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// askFlags holds the parsed CLI flags for `canopy ask`. v0.22.
var askFlags struct {
	file    string        // --file <path>: read question body from file (used by the TUI popup)
	stdin   bool          // --stdin: read question body from stdin
	timeout time.Duration // --timeout: per-invocation deadline (default 60s)
}

// Exit codes for `canopy ask`. The TUI popup parses these to render
// distinct messages instead of trying to grep stderr. See design doc
// §1 step 7 (the exit-code-collision fix).
const (
	askExitOK                 = 0
	askExitGeneric            = 1
	askExitAgentNotAllowed    = 2
	askExitLauncherNoExec     = 3
	askExitTimeout            = 4
	askExitBinaryNotInstalled = 5
)

// askCmd returns the `canopy ask <agent> [question]` cobra subcommand.
// Pattern + behavior: design doc at
//   ~/.gstack/projects/avinashjoshi-canopy/cassy-add-codex-support-concurrent-multi-agent-design-20260625-110939.md
//
// Three input modes:
//   - positional inline: `canopy ask codex "question"`
//   - --file <path>:     `canopy ask codex --file ./bug.md`  (TUI popup uses this)
//   - --stdin:           `echo "question" | canopy ask codex --stdin`
//
// Exactly one must be supplied; mixing is an error so users don't
// accidentally lose intent (e.g., piping stdin while also passing a
// positional and not knowing which canopy actually used).
func askCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ask <agent> [question]",
		Short: "Ask a one-shot question to a different agent without leaving the workspace",
		Long: "Runs `<agent> exec` (or its non-interactive equivalent) with the question and\n" +
			"a brief workspace-context prefix. Output streams to stdout; agent chatter to\n" +
			"stderr. Atomic — no session continuity, no multi-turn. For multi-turn handoff,\n" +
			"use `canopy agent swap <agent>` instead.\n\n" +
			"Input modes (exactly one):\n" +
			"  positional      canopy ask codex \"what does this regex do?\"\n" +
			"  --file <path>   canopy ask codex --file ./context.md\n" +
			"  --stdin         echo \"question\" | canopy ask codex --stdin\n\n" +
			"Exit codes: 0 success, 1 generic error, 2 ErrAgentNotAllowed,\n" +
			"3 ErrLauncherNoExec, 4 timeout, 5 binary missing.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			agentName := args[0]

			// Load the question. Exactly one input mode must be active.
			question, err := loadAskQuestion(args[1:], askFlags.file, askFlags.stdin, cmd.InOrStdin())
			if err != nil {
				return err
			}
			if strings.TrimSpace(question) == "" {
				return fmt.Errorf("canopy ask: question body is empty")
			}

			mgr, err := loadManager()
			if err != nil {
				return err
			}

			// D6=A: auto-add the agent to canopy.json if it's installed
			// AND has a one-shot exec mode. `canopy ask` is invoked by
			// the TUI popup AND directly by users; both want the same
			// "any installed launcher just works" behavior. We check
			// Resolve / VerifyInstalled / ResolveExec FIRST so a launcher
			// that fails any of those gates doesn't leave a useless
			// config side-effect on disk (codex review P2 #7).
			if !mgr.Cfg.AllowsAgent(agentName) {
				launcher, lerr := agent.Resolve(agentName)
				if lerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "canopy ask: %v\n", lerr)
					exitWithCode(askExitAgentNotAllowed)
					return nil
				}
				if err := launcher.VerifyInstalled(); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "canopy ask: %v\n", err)
					exitWithCode(askExitBinaryNotInstalled)
					return nil
				}
				if _, err := launcher.ResolveExec(); err != nil {
					if errors.Is(err, agent.ErrLauncherNoExec) {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"canopy ask: launcher %q has no one-shot mode wired up yet.\n",
							agentName)
						exitWithCode(askExitLauncherNoExec)
						return nil
					}
					return err
				}
				// All gates passed — safe to mutate canopy.json now.
				if err := config.AddAgentToCanopyJSON(mgr.Cfg.ProjectRoot, agentName); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "canopy ask: auto-add %q to canopy.json failed: %v\n", agentName, err)
					exitWithCode(askExitGeneric)
					return nil
				}
				// Re-load so the rest of this function sees the updated
				// allowlist (not strictly required since we don't gate
				// again, but keeps state consistent for any future
				// downstream check).
				updated, err := config.LoadFrom(mgr.Cfg.ProjectRoot)
				if err == nil {
					mgr.Cfg = updated
				}
			}

			// Resolve the launcher + its exec mode.
			launcher, err := agent.Resolve(agentName)
			if err != nil {
				return err
			}
			execMode, err := launcher.ResolveExec()
			if err != nil {
				if errors.Is(err, agent.ErrLauncherNoExec) {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"canopy ask: launcher %q has no one-shot mode wired up yet.\n",
						agentName)
					exitWithCode(askExitLauncherNoExec)
					return nil
				}
				return err
			}

			// Verify the binary is on PATH before spawning. Without
			// this, exec.CommandContext below would surface a
			// cryptic os/exec error; VerifyInstalled gives the
			// canonical install hint.
			if err := launcher.VerifyInstalled(); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "canopy ask: %v\n", err)
				exitWithCode(askExitBinaryNotInstalled)
				return nil
			}

			// Locate workspace via cwd-walk-up. Same machinery as
			// canopy agent swap. The workspace gives us name + path
			// + branch to render the context prefix.
			cwd, _ := os.Getwd()
			wsName, err := findWorkspaceFromCwd(ctx, mgr, cwd)
			if err != nil {
				// Soft-fail: if we're not inside a workspace, still
				// support `canopy ask` with a less-rich prefix.
				// (Open Q #3 in the design — for v1 we just emit a
				// degraded prefix and proceed.)
				wsName = ""
			}

			// Build the assembled prompt. Four canopy-generated fields
			// + the user's question. Premise 5.
			assembled := buildAskPrefix(mgr, wsName, cwd) + "\n\n---\n\n" + question

			// Run the agent's exec mode under a timeout context.
			ctxRun, cancel := context.WithTimeout(ctx, askFlags.timeout)
			defer cancel()

			return runAskExec(ctxRun, launcher, execMode, assembled, mgr,
				cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&askFlags.file, "file", "",
		"read question body from <path> (used by the TUI popup)")
	cmd.Flags().BoolVar(&askFlags.stdin, "stdin", false,
		"read question body from stdin (use with `echo ... | canopy ask <agent> --stdin`)")
	cmd.Flags().DurationVar(&askFlags.timeout, "timeout", 60*time.Second,
		"per-invocation timeout (default 60s)")
	return cmd
}

// loadAskQuestion picks exactly one input mode (positional, --file,
// --stdin) and returns the resulting question body. Mixing modes is a
// clean error — the user almost certainly didn't intend it and we'd
// rather fail loud than silently pick one.
func loadAskQuestion(positional []string, file string, useStdin bool, stdin io.Reader) (string, error) {
	modes := 0
	if len(positional) > 0 {
		modes++
	}
	if file != "" {
		modes++
	}
	if useStdin {
		modes++
	}
	switch modes {
	case 0:
		return "", fmt.Errorf("canopy ask: provide a question (positional, --file <path>, or --stdin)")
	case 1:
		// fall through
	default:
		return "", fmt.Errorf("canopy ask: provide exactly one of <positional>, --file, --stdin (got %d)", modes)
	}

	switch {
	case len(positional) > 0:
		return strings.Join(positional, " "), nil
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("canopy ask: read --file %s: %w", file, err)
		}
		return string(data), nil
	case useStdin:
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("canopy ask: read stdin: %w", err)
		}
		return string(data), nil
	}
	return "", fmt.Errorf("canopy ask: unreachable input-mode branch")
}

// buildAskPrefix assembles the four-field canopy-generated context
// prefix. Premise 5 (D3 of office-hours): no primary-agent pane
// capture; only fixed-shape canopy data so prompt-injection surface
// stays minimal.
//
// wsName empty (caller wasn't inside a workspace) yields a degraded
// prefix that still names the project + cwd; that's the soft-fall
// behavior described in design Open Q #3.
func buildAskPrefix(mgr *workspace.Manager, wsName, cwd string) string {
	var b strings.Builder
	if wsName != "" {
		fmt.Fprintf(&b, "You are being asked a quick question by a user in canopy workspace %q on branch %q.\n",
			wsName, branchHint(cwd))
	} else {
		fmt.Fprintf(&b, "You are being asked a quick question by a user in canopy project %q.\n",
			mgr.Cfg.Project)
	}
	fmt.Fprintf(&b, "Working directory: %s\n", cwd)
	fmt.Fprintf(&b, "Repo root: %s\n", mgr.Cfg.ProjectRoot)
	b.WriteString("\nThe user's question follows.")
	return b.String()
}

// branchHint is a best-effort branch-name pull from `git symbolic-ref`.
// Returns empty string on any failure (detached HEAD, non-git dir,
// etc.) — the prefix renders "branch %q" with empty value, which is
// acceptable for the soft-fall path.
func branchHint(cwd string) string {
	cmd := exec.Command("git", "-C", cwd, "symbolic-ref", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runAskExec dispatches the launcher's exec command with the assembled
// prompt + the canopy env subset, streams stdout/stderr, and maps
// timeout / generic failure into the exit-code contract.
func runAskExec(
	ctx context.Context,
	launcher agent.Launcher,
	execMode *agent.ExecMode,
	prompt string,
	mgr *workspace.Manager,
	stdout, stderr io.Writer,
) error {
	// Build argv: [Cmd, Args..., (prompt as positional OR via stdin)]
	argv := append([]string{launcher.Cmd}, execMode.Args...)
	if execMode.PromptMode == agent.PromptArg {
		argv = append(argv, prompt)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(),
		"CANOPY_WORKSPACE_PATH="+os.Getenv("CANOPY_WORKSPACE_PATH"), // pass-through if set
		"CANOPY_ROOT_PATH="+mgr.Cfg.ProjectRoot,
		"CANOPY_PORT="+os.Getenv("CANOPY_PORT"),
	)
	if execMode.PromptMode == agent.PromptStdin {
		cmd.Stdin = strings.NewReader(prompt)
	}

	if err := cmd.Run(); err != nil {
		// Distinguish timeout from generic error via the context.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			fmt.Fprintf(stderr, "canopy ask: timed out after %s\n", askFlags.timeout)
			exitWithCode(askExitTimeout)
			return nil
		}
		// Surface the child's exit code if possible; otherwise generic.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(askExitGeneric)
		}
		return err
	}
	return nil
}

// exitWithCode flushes any pending writes and calls os.Exit. Wrapper
// so tests can stub it (var exitWithCode = os.Exit) without touching
// every call site.
var exitWithCode = func(code int) {
	os.Exit(code)
}

// sweepAskTempFiles deletes stale `~/.canopy/tmp/ask-*.md` files older
// than 1 hour. Called from main.go init() once per CLI invocation; a
// backstop for the rare case where the TUI itself is SIGKILL'd before
// its defer-removed the temp file. v0.22.
func sweepAskTempFiles() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	tmpDir := filepath.Join(home, ".canopy", "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "ask-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(tmpDir, e.Name()))
		}
	}
}
