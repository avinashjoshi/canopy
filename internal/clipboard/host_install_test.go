package clipboard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeSSH records every Exec invocation and returns canned responses
// keyed by the command's shape — that's what differs between
// InstallOnHost's SSH calls (push wl-paste vs push wl-copy vs the
// tmux-config splice vs the verify probe).
//
// Concurrency safety: the caller's tests are single-goroutine, but the
// installer may serialize multiple SSH calls in the future. Mutex is
// cheap and the lock contention is irrelevant in tests.
type fakeSSH struct {
	mu        sync.Mutex
	responses map[string]fakeSSHResp
	calls     []fakeSSHCall
}

type fakeSSHResp struct {
	stdout []byte
	stderr []byte
	err    error
}

type fakeSSHCall struct {
	target string
	stdin  []byte // captured stdin bytes for assertion
	args   []string
}

func (f *fakeSSH) exec(_ context.Context, target string, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	var stdinBytes []byte
	if stdin != nil {
		stdinBytes, _ = io.ReadAll(stdin)
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeSSHCall{
		target: target,
		stdin:  stdinBytes,
		args:   append([]string(nil), args...),
	})
	f.mu.Unlock()
	key := classifyCall(args, stdinBytes)
	resp, ok := f.responses[key]
	if !ok {
		return nil, nil, errors.New("fakeSSH: no canned response for key " + key)
	}
	return resp.stdout, resp.stderr, resp.err
}

// classifyCall reduces an arbitrary SSH invocation into one of the
// shapes InstallOnHost emits. Tests don't have to encode the full
// argv to match.
func classifyCall(args []string, stdin []byte) string {
	// Push wrappers: `bash -c "set -e; mkdir -p ...; cat > $HOME/.local/bin/<name>; chmod ..."`.
	// stdin carries the wrapper bytes. The `cat > $HOME/.local/bin/<name>`
	// part distinguishes which wrapper.
	if len(args) >= 3 && args[0] == "bash" && args[1] == "-c" && len(stdin) > 0 {
		switch {
		case strings.Contains(args[2], "$HOME/.local/bin/wl-paste"):
			return "push-wl-paste"
		case strings.Contains(args[2], "$HOME/.local/bin/wl-copy"):
			return "push-wl-copy"
		}
	}
	// Remote tmux config: `bash` + stdin script that updates
	// ~/.tmux.conf with the copy-pipe binds.
	if len(args) == 1 && args[0] == "bash" && len(stdin) > 0 &&
		strings.Contains(string(stdin), "copy-pipe-and-cancel") &&
		strings.Contains(string(stdin), "tmux.conf") {
		return "configure-remote-tmux"
	}
	// Verify: `bash -l` (login) + stdin = `command -v wl-paste`.
	if len(args) == 2 && args[0] == "bash" && args[1] == "-l" && len(stdin) > 0 &&
		strings.Contains(string(stdin), "command -v wl-paste") {
		return "verify-path"
	}
	return "unknown"
}

func newTestHostInstaller(t *testing.T) (*HostInstaller, *fakeSSH) {
	t.Helper()
	home := t.TempDir()
	f := &fakeSSH{responses: map[string]fakeSSHResp{}}
	return &HostInstaller{
		SSHExec: f.exec,
		// Default to a no-op success so tests that don't care about
		// legacy-cleanup call shape don't have to thread it through.
		LocalSystemctl: func(args ...string) error { return nil },
		HomeDir:        home,
		Version:        "v0.24.0+test",
	}, f
}

// happyPathResponses is the canned set every "everything works" test
// uses. Extracted so tests focused on one failure mode don't have to
// spell out every other call.
func happyPathResponses() map[string]fakeSSHResp {
	return map[string]fakeSSHResp{
		"push-wl-paste":         {},
		"push-wl-copy":          {},
		"configure-remote-tmux": {},
		"verify-path":           {stdout: []byte("/home/avi/.local/bin/wl-paste\n")},
	}
}

func TestInstallOnHost_HappyPath(t *testing.T) {
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()

	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost: %v", err)
	}
	// Four SSH calls: push×2 + configure-tmux + verify-path.
	if len(f.calls) != 4 {
		t.Fatalf("expected 4 SSH calls, got %d", len(f.calls))
	}
	if !strings.Contains(out.String(), "bridge active") {
		t.Errorf("expected success output; got:\n%s", out.String())
	}
}

func TestInstallOnHost_PushWrapperPipesScriptToStdin(t *testing.T) {
	// The wrapper bytes must reach SSH stdin verbatim — capturing them
	// here defends against a regression where someone "simplifies" the
	// push by losing the stdin payload.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost: %v", err)
	}
	var pasteCall, copyCall *fakeSSHCall
	for i := range f.calls {
		switch classifyCall(f.calls[i].args, f.calls[i].stdin) {
		case "push-wl-paste":
			pasteCall = &f.calls[i]
		case "push-wl-copy":
			copyCall = &f.calls[i]
		}
	}
	if pasteCall == nil || copyCall == nil {
		t.Fatal("missing push calls in recorded SSH invocations")
	}
	if !bytes.Contains(pasteCall.stdin, []byte("#!/usr/bin/env bash")) {
		t.Errorf("wl-paste push stdin missing shebang; got first 50 bytes: %q", head(string(pasteCall.stdin), 50))
	}
	if !bytes.Contains(pasteCall.stdin, []byte("52;c;")) {
		t.Error("wl-paste push stdin missing OSC 52 marker (wrong template rendered?)")
	}
	if !bytes.Contains(copyCall.stdin, []byte("52;c;")) {
		t.Error("wl-copy push stdin missing OSC 52 marker")
	}
}

func TestInstallOnHost_PushWrapperFailureAbortsBeforeTmuxAndVerify(t *testing.T) {
	inst, f := newTestHostInstaller(t)
	f.responses = map[string]fakeSSHResp{
		"push-wl-paste": {err: errors.New("connection refused")},
	}
	var out bytes.Buffer
	err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out)
	if err == nil {
		t.Fatal("InstallOnHost should fail when the wrapper push fails")
	}
	if len(f.calls) != 1 {
		t.Errorf("expected install to abort after the first failing push, got %d SSH calls", len(f.calls))
	}
}

func TestInstallOnHost_TmuxConfigFailureIsWarningNotError(t *testing.T) {
	// Configuring tmux copy-mode binds is UX polish. A failure there
	// must not abort the install — the bridge still works for direct
	// wl-copy/wl-paste calls.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	f.responses["configure-remote-tmux"] = fakeSSHResp{err: errors.New("permission denied")}
	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost should succeed even when the tmux config splice fails: %v", err)
	}
	if !strings.Contains(out.String(), "warning") {
		t.Errorf("expected a warning about the tmux config failure; got:\n%s", out.String())
	}
}

func TestInstallOnHost_TmuxConfigSpliceIncludesAllowPassthrough(t *testing.T) {
	// tmux 3.3+ defaults allow-passthrough to off, which silently drops
	// the DCS-wrapped OSC 52 sequences the wrappers emit from inside a
	// tmux pane (every canopy workspace is a tmux session). Regression
	// guard: the splice script itself must set it.
	if !strings.Contains(remoteTmuxConfigScript, "set -g allow-passthrough on") {
		t.Error("remoteTmuxConfigScript missing `set -g allow-passthrough on`")
	}
}

func TestInstallOnHost_VerifyWarnsOnPathPrecedenceButSucceeds(t *testing.T) {
	// login-shell PATH resolves wl-paste to /usr/bin/wl-paste — Claude
	// Code on the remote won't use our wrapper. The install completes
	// successfully (the bridge is functionally installed) but the user
	// gets a clear inline warning + fix suggestion.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	f.responses["verify-path"] = fakeSSHResp{stdout: []byte("/usr/bin/wl-paste\n")}
	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost should succeed even when PATH precedence is wrong: %v", err)
	}
	body := out.String()
	for _, must := range []string{"warning", "/usr/bin/wl-paste", "PATH", ".bashrc"} {
		if !strings.Contains(body, must) {
			t.Errorf("PATH-precedence warning missing %q\nout:\n%s", must, body)
		}
	}
}

func TestInstallOnHost_VerifyWarnsWhenWrapperNotFoundAtAll(t *testing.T) {
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	f.responses["verify-path"] = fakeSSHResp{stdout: []byte("")}
	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost should succeed (warning only) when PATH resolves nothing: %v", err)
	}
	if !strings.Contains(out.String(), "returned nothing") {
		t.Errorf("expected a warning about command -v returning nothing; got:\n%s", out.String())
	}
}

// --- legacy cleanup ---

func TestCleanupLegacyArtifacts_RemovesStuckTunnelUnit(t *testing.T) {
	// This is the direct regression guard for the bug that motivated
	// retiring the tunnel: a laptop-side systemd unit stuck reporting
	// "start of the service was attempted too often" (StartLimitBurst).
	inst, _ := newTestHostInstaller(t)
	unitDir := filepath.Join(inst.HomeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	unitPath := filepath.Join(unitDir, "canopy-clipboard-tunnel-tower.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	var systemctlCalls [][]string
	inst.LocalSystemctl = func(args ...string) error {
		systemctlCalls = append(systemctlCalls, append([]string(nil), args...))
		return nil
	}

	var out bytes.Buffer
	inst.cleanupLegacyArtifacts("tower", &out)

	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit file should have been removed; stat err = %v", err)
	}
	if !strings.Contains(out.String(), "removed legacy systemd unit canopy-clipboard-tunnel-tower.service") {
		t.Errorf("expected removal to be reported; got:\n%s", out.String())
	}
	var sawResetFailed, sawDisable, sawReload bool
	for _, c := range systemctlCalls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "reset-failed") {
			sawResetFailed = true
		}
		if strings.Contains(joined, "disable") && strings.Contains(joined, "--now") {
			sawDisable = true
		}
		if strings.Contains(joined, "daemon-reload") {
			sawReload = true
		}
	}
	if !sawResetFailed {
		t.Error("expected a reset-failed call (clears the StartLimitBurst throttle before disable)")
	}
	if !sawDisable {
		t.Error("expected a disable --now call")
	}
	if !sawReload {
		t.Error("expected a daemon-reload call after removing a unit")
	}
}

func TestCleanupLegacyArtifacts_RemovesDaemonUnit(t *testing.T) {
	inst, _ := newTestHostInstaller(t)
	unitDir := filepath.Join(inst.HomeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	unitPath := filepath.Join(unitDir, "canopy-clipboard.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	var out bytes.Buffer
	inst.cleanupLegacyArtifacts("tower", &out)

	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("daemon unit file should have been removed; stat err = %v", err)
	}
}

func TestCleanupLegacyArtifacts_RemovesSSHSnippet(t *testing.T) {
	inst, _ := newTestHostInstaller(t)
	snippetDir := filepath.Join(inst.HomeDir, ".ssh", "config.d", "canopy")
	if err := os.MkdirAll(snippetDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	snippetPath := filepath.Join(snippetDir, "tower.conf")
	if err := os.WriteFile(snippetPath, []byte("Host canopy-tunnel-tower\n"), 0o600); err != nil {
		t.Fatalf("write snippet: %v", err)
	}

	var out bytes.Buffer
	inst.cleanupLegacyArtifacts("tower", &out)

	if _, err := os.Stat(snippetPath); !os.IsNotExist(err) {
		t.Errorf("snippet file should have been removed; stat err = %v", err)
	}
	if !strings.Contains(out.String(), "removed legacy SSH snippet") {
		t.Errorf("expected removal to be reported; got:\n%s", out.String())
	}
}

func TestCleanupLegacyArtifacts_NoopWhenNothingToClean(t *testing.T) {
	// The common case: a host that never had the pre-OSC52 bridge
	// installed. Must be silent (no output lines) and must not call
	// systemctl at all (avoids spurious "unit not found" noise/log
	// warnings on every single install).
	inst, _ := newTestHostInstaller(t)
	var systemctlCalled bool
	inst.LocalSystemctl = func(args ...string) error {
		systemctlCalled = true
		return nil
	}

	var out bytes.Buffer
	inst.cleanupLegacyArtifacts("tower", &out)

	if out.String() != "" {
		t.Errorf("expected no output when there's nothing to clean up; got:\n%s", out.String())
	}
	if systemctlCalled {
		t.Error("systemctl should not be invoked when no legacy unit files exist on disk")
	}
}

func TestCleanupLegacyArtifacts_SystemctlFailureDoesNotAbortInstall(t *testing.T) {
	// Best-effort: even if disable/reset-failed themselves error (e.g.
	// systemd not running at all — some minimal/WSL setups), cleanup
	// must still remove the unit file and the overall InstallOnHost
	// flow must still complete successfully.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	unitDir := filepath.Join(inst.HomeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	unitPath := filepath.Join(unitDir, "canopy-clipboard-tunnel-tower.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	inst.LocalSystemctl = func(args ...string) error {
		return errors.New("Failed to connect to bus: No such file or directory")
	}

	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost should succeed despite systemctl failures during cleanup: %v", err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit file should still be removed even when systemctl itself errors; stat err = %v", err)
	}
}
