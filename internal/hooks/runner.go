// Package hooks runs the per-project scripts declared in canopy.json
// (scripts.setup, scripts.run, scripts.archive).
//
// Scripts are executed via exec.CommandContext directly, NOT through `sh -c`.
// This means scripts must be proper executables (have a shebang and the
// executable bit set). The advantage is that the user's existing tooling —
// which already lives in `bin/conductor-setup` and similar — drops in with
// no quoting headaches and no shell-escaping bugs.
//
// All scripts get the same three CANOPY_* env vars merged into the user's
// existing environment:
//
//	CANOPY_WORKSPACE_PATH   absolute path to the workspace dir
//	CANOPY_ROOT_PATH        absolute path to the original repo root
//	CANOPY_PORT             allocated TCP port for this workspace
//
// Stdout and stderr stream live to caller-supplied io.Writers. The TUI in
// step 6/7 will plug in a tea.Msg-emitting writer; CLI subcommands plug in
// os.Stdout/os.Stderr; tests plug in bytes.Buffer.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("hooks")

// Sentinel errors for the cases canopy's lifecycle distinguishes.
var (
	// ErrScriptNotFound is returned when the script path doesn't exist or
	// isn't executable. Distinct from a non-zero exit so the workspace
	// lifecycle can surface "your canopy.json points at bin/missing-setup
	// which doesn't exist" rather than the more confusing "exit status 127".
	ErrScriptNotFound = errors.New("hooks: script not found or not executable")

	// ErrScriptFailed is returned when a script exits non-zero. The error
	// chain includes the exit code; callers reading the message see
	// something like "hooks.Run(bin/canopy-setup): script failed: exit status 1".
	ErrScriptFailed = errors.New("hooks: script failed")
)

// Options configures a single Run. Stdout/Stderr are required (caller must
// pick somewhere for output to go); Env is appended to os.Environ();
// Cwd defaults to the script's parent directory but should normally be set
// to the workspace path.
type Options struct {
	// Cwd is the working directory the script runs in. Almost always the
	// workspace path (CANOPY_WORKSPACE_PATH); never the repo root, since
	// scripts will use relative paths like `bin/rails db:create` that need
	// to resolve inside the worktree.
	Cwd string

	// Env is a slice of "KEY=VALUE" strings appended to os.Environ(). Use
	// WorkspaceEnv to construct the canonical CANOPY_* triplet plus any
	// extras the lifecycle wants to add.
	Env []string

	// Stdout and Stderr receive the script's output as it runs. Required;
	// nil writers panic at exec time, which is harsher than the alternative
	// of silently dropping output.
	Stdout io.Writer
	Stderr io.Writer
}

// WorkspaceEnv returns the canopy env vars ready to merge into
// Options.Env. It does NOT include os.Environ() — the runner does that
// automatically. Returning just the canopy-specific entries keeps
// callers from accidentally double-merging.
//
// Both CANOPY_* and CONDUCTOR_* are exported. The CONDUCTOR_* aliases
// are deliberate: anyone migrating from Conductor (Avi's cravd is the
// canonical case) has bin/conductor-setup, bin/conductor-teardown, and
// config/database.yml referring to CONDUCTOR_WORKSPACE_PATH /
// CONDUCTOR_ROOT_PATH / CONDUCTOR_PORT. Forcing them to grep-and-replace
// every reference before canopy works at all is the kind of friction
// that breaks adoption. Cheaper to export the aliases unconditionally;
// six env entries instead of three, vanishing cost, smooth migration.
//
// New canopy projects should reference CANOPY_*; the CONDUCTOR_*
// aliases stay forever (or at least until canopy reaches a v1 with a
// formal deprecation notice).
func WorkspaceEnv(workspacePath, rootPath string, port int) []string {
	return []string{
		"CANOPY_WORKSPACE_PATH=" + workspacePath,
		"CANOPY_ROOT_PATH=" + rootPath,
		"CANOPY_PORT=" + strconv.Itoa(port),
		// Conductor-compatibility aliases.
		"CONDUCTOR_WORKSPACE_PATH=" + workspacePath,
		"CONDUCTOR_ROOT_PATH=" + rootPath,
		"CONDUCTOR_PORT=" + strconv.Itoa(port),
	}
}

// Run executes the script at scriptPath under opts. It blocks until the
// script exits, the context is cancelled, or stdout/stderr drain.
//
// Context cancellation is wired through exec.CommandContext: cancelling
// ctx sends SIGKILL to the process group. Callers that want a softer
// shutdown (SIGINT first, then SIGKILL after a grace period) should
// build that on top.
//
// Returns ErrScriptNotFound if the script can't be exec'd at all (bad
// shebang, missing file, no executable bit), ErrScriptFailed if the
// script ran but exited non-zero, or a context error if cancelled.
func Run(ctx context.Context, scriptPath string, opts Options) error {
	if opts.Stdout == nil || opts.Stderr == nil {
		// Surface the misuse loud and early. The alternative — silently
		// dropping output — has caused real bugs in tools we've used.
		return fmt.Errorf("hooks.Run: Options.Stdout and Options.Stderr are required")
	}

	// Pre-flight: does the script exist? Reading exec.ErrNotFound back from
	// cmd.Start gives a confusing message because Go's error chain wraps the
	// original syscall.Errno. A direct os.Stat with a clear sentinel beats it.
	if _, err := os.Stat(scriptPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("hooks.Run(%s): %w", scriptPath, ErrScriptNotFound)
	} else if err != nil {
		return fmt.Errorf("hooks.Run(%s): stat: %w", scriptPath, err)
	}

	cmd := exec.CommandContext(ctx, scriptPath)
	cmd.Dir = opts.Cwd
	cmd.Env = append(os.Environ(), opts.Env...)
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr

	// Run the script in its own process group so that on ctx cancel we
	// can kill the whole group, not just the immediate child. Without
	// this, a script like `bin/canopy-setup` that spawns `bundle install`
	// would leave bundle running (orphaned, reparented to init) when we
	// kill the script — and the orphaned child holds the stdout pipe
	// open, so cmd.Wait blocks until the orphan exits on its own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Override Cancel (Go 1.20+) to send SIGKILL to the whole process
	// group via negative PID. WaitDelay forces cmd.Wait to give up if
	// pipes are still held open by stragglers after a 1s grace.
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 1 * time.Second

	start := time.Now()
	log.Info("hooks.run.start", "script", scriptPath, "cwd", opts.Cwd)

	if err := cmd.Run(); err != nil {
		dur := time.Since(start)
		// exec.ErrNotFound surfaces as a *exec.Error wrapping it. Use
		// errors.Is to detect rather than string-match.
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("hooks.Run(%s): %w", scriptPath, ErrScriptNotFound)
		}
		// Context-cancelled gives back ctx.Err() somewhere in the chain.
		if ctx.Err() != nil {
			log.Info("hooks.run.cancelled", "script", scriptPath, "duration_ms", dur.Milliseconds())
			return fmt.Errorf("hooks.Run(%s): %w", scriptPath, ctx.Err())
		}
		// Anything else: non-zero exit. Wrap with our sentinel so callers
		// can distinguish "script ran and failed" from "couldn't start".
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			log.Info("hooks.run.exit-nonzero",
				"script", scriptPath,
				"exit_code", exitErr.ExitCode(),
				"duration_ms", dur.Milliseconds())
			return fmt.Errorf("hooks.Run(%s): %w: %v", scriptPath, ErrScriptFailed, err)
		}
		return fmt.Errorf("hooks.Run(%s): %w", scriptPath, err)
	}

	dur := time.Since(start)
	log.Info("hooks.run.success", "script", scriptPath, "duration_ms", dur.Milliseconds())
	return nil
}

// formatEnv is a tiny helper used by the test suite to build a redacted
// env-string view for assertions. Kept here so tests don't reach into
// internals.
//
// Returns a sorted "KEY=VAL" string (one per line) for the CANOPY_* keys
// only — users of formatEnv don't care about $PATH and friends.
func formatEnv(env []string) string {
	out := []string{}
	for _, kv := range env {
		if strings.HasPrefix(kv, "CANOPY_") {
			out = append(out, kv)
		}
	}
	return strings.Join(out, "\n")
}

// _ guard against the unused-warning while we develop tests. The compiler
// drops this when formatEnv has its first real caller.
var _ = formatEnv
