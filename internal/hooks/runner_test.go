package hooks_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oncactus/canopy/internal/clog"
	"github.com/oncactus/canopy/internal/hooks"
)

func TestMain(m *testing.M) {
	teardown, _ := clog.Init(false)
	defer teardown()
	m.Run()
}

// requireUnix skips on Windows. canopy is Linux-first; macOS works as a
// side effect; Windows is explicitly out of scope (see design doc).
func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hooks tests use POSIX shebang scripts")
	}
}

// writeScript drops a small bash script at path with executable bit set.
func writeScript(t *testing.T, path, content string) {
	t.Helper()
	full := "#!/usr/bin/env bash\nset -euo pipefail\n" + content
	if err := os.WriteFile(path, []byte(full), 0o755); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// TestWorkspaceEnv covers both the canonical CANOPY_* env triplet AND
// the CONDUCTOR_* aliases canopy exports for migration compatibility.
func TestWorkspaceEnv(t *testing.T) {
	t.Parallel()
	env := hooks.WorkspaceEnv("/home/avi/Work/cravd/worktrees/feature-x", "/home/avi/Work/cravd", 3001)

	want := map[string]string{
		"CANOPY_WORKSPACE_PATH":    "/home/avi/Work/cravd/worktrees/feature-x",
		"CANOPY_ROOT_PATH":         "/home/avi/Work/cravd",
		"CANOPY_PORT":              "3001",
		"CONDUCTOR_WORKSPACE_PATH": "/home/avi/Work/cravd/worktrees/feature-x",
		"CONDUCTOR_ROOT_PATH":      "/home/avi/Work/cravd",
		"CONDUCTOR_PORT":           "3001",
	}
	got := map[string]string{}
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		got[parts[0]] = parts[1]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%q] = %q; want %q", k, got[k], v)
		}
	}
	if len(env) != 6 {
		t.Errorf("env len = %d; want 6 (CANOPY_* triplet + CONDUCTOR_* aliases)", len(env))
	}
}

// TestRun_HappyPath: a script that exits 0 and writes to stdout. The
// provided writer must capture the output.
func TestRun_HappyPath(t *testing.T) {
	requireUnix(t)
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "ok.sh")
	writeScript(t, script, `echo "hello canopy"`)

	var stdout, stderr bytes.Buffer
	err := hooks.Run(context.Background(), script, hooks.Options{
		Cwd:    dir,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "hello canopy") {
		t.Errorf("stdout missing expected content: %q", stdout.String())
	}
}

// TestRun_PassesEnv verifies that CANOPY_* env vars reach the script.
// The script echoes one of them; we verify it shows up in stdout.
func TestRun_PassesEnv(t *testing.T) {
	requireUnix(t)
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "env.sh")
	writeScript(t, script, `echo "port is ${CANOPY_PORT}"`)

	var stdout, stderr bytes.Buffer
	err := hooks.Run(context.Background(), script, hooks.Options{
		Cwd:    dir,
		Env:    hooks.WorkspaceEnv("/wp", "/rp", 4242),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(stdout.String(), "port is 4242") {
		t.Errorf("env not passed; stdout: %q", stdout.String())
	}
}

// TestRun_PassesCwd verifies that the script runs in opts.Cwd, not the
// current process's cwd.
func TestRun_PassesCwd(t *testing.T) {
	requireUnix(t)
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "pwd.sh")
	writeScript(t, script, `pwd`)

	// Resolve the expected cwd. macOS's /tmp is a symlink to /private/tmp,
	// so $TempDir might come back differently from what `pwd` prints. Run
	// `pwd` ourselves first to capture the canonical form.
	wantCwd := dir
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		wantCwd = resolved
	}

	var stdout, stderr bytes.Buffer
	err := hooks.Run(context.Background(), script, hooks.Options{
		Cwd:    dir,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.TrimSpace(stdout.String())
	if got != wantCwd {
		t.Errorf("pwd = %q; want %q", got, wantCwd)
	}
}

// TestRun_ScriptExitsNonZero: ErrScriptFailed sentinel + non-zero exit.
func TestRun_ScriptExitsNonZero(t *testing.T) {
	requireUnix(t)
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	writeScript(t, script, `echo "broken"; exit 7`)

	var stdout, stderr bytes.Buffer
	err := hooks.Run(context.Background(), script, hooks.Options{
		Cwd:    dir,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if !errors.Is(err, hooks.ErrScriptFailed) {
		t.Errorf("Run(exit-7): got %v; want errors.Is(... ErrScriptFailed)", err)
	}
	// Output up to the failure should still be captured.
	if !strings.Contains(stdout.String(), "broken") {
		t.Errorf("stdout missing pre-failure output: %q", stdout.String())
	}
}

// TestRun_ScriptNotFound: ErrScriptNotFound sentinel for missing file.
func TestRun_ScriptNotFound(t *testing.T) {
	requireUnix(t)
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.sh")

	var stdout, stderr bytes.Buffer
	err := hooks.Run(context.Background(), missing, hooks.Options{
		Cwd:    dir,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if !errors.Is(err, hooks.ErrScriptNotFound) {
		t.Errorf("Run(missing-script): got %v; want errors.Is(... ErrScriptNotFound)", err)
	}
}

// TestRun_NotExecutable: file exists but isn't chmod +x. Should ALSO
// surface as ErrScriptNotFound (the user's fix is the same: chmod +x or
// fix the path).
func TestRun_NotExecutable(t *testing.T) {
	requireUnix(t)
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "noexec.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\necho hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := hooks.Run(context.Background(), script, hooks.Options{
		Cwd:    dir,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err == nil {
		t.Errorf("Run(non-executable): got nil; want error")
	}
}

// TestRun_ContextCancel verifies that cancelling the context kills a
// long-running script. The eng review's failure-modes table flagged
// this as the path SIGINT during `bundle install` takes.
func TestRun_ContextCancel(t *testing.T) {
	requireUnix(t)
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "sleep.sh")
	writeScript(t, script, `sleep 30`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var stdout, stderr bytes.Buffer
	start := time.Now()
	err := hooks.Run(ctx, script, hooks.Options{
		Cwd:    dir,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	dur := time.Since(start)
	if err == nil {
		t.Errorf("Run(cancelled): got nil; want error")
	}
	if dur > 5*time.Second {
		t.Errorf("Run took %v after cancel; expected <5s", dur)
	}
}

// TestRun_RequiresWriters: nil Stdout/Stderr -> error before exec.
func TestRun_RequiresWriters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := hooks.Run(context.Background(), filepath.Join(dir, "any.sh"), hooks.Options{
		Cwd: dir,
	})
	if err == nil {
		t.Errorf("Run(nil-writers): got nil; want misuse error")
	}
}
