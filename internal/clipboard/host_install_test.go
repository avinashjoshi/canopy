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
// keyed by the command's *first non-bash arg* — that's what differs
// between InstallOnHost's SSH calls (id -u vs writing wl-paste vs
// writing wl-copy vs the verify probe).
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
// argv to match. Order matters: more specific classifiers come first
// (push uses `bash -c` + a path that overlaps with the verify shape).
func classifyCall(args []string, stdin []byte) string {
	if len(args) == 2 && args[0] == "id" && args[1] == "-u" {
		return "id"
	}
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
	// Verify-step 1: `bash -c "$HOME/.local/bin/wl-paste --list-types"`
	// — no stdin, absolute-path invocation.
	if len(args) >= 3 && args[0] == "bash" && args[1] == "-c" && strings.Contains(args[2], "wl-paste --list-types") {
		return "verify-wrapper"
	}
	// Verify-step 2: `bash -lc "command -v wl-paste"` — login-shell
	// PATH-precedence check.
	if len(args) >= 3 && args[0] == "bash" && args[1] == "-lc" && strings.Contains(args[2], "command -v wl-paste") {
		return "verify-path"
	}
	return "unknown"
}

func newTestHostInstaller(t *testing.T) (*HostInstaller, *fakeSSH) {
	t.Helper()
	home := t.TempDir()
	f := &fakeSSH{responses: map[string]fakeSSHResp{}}
	return &HostInstaller{
		SSHExec:  f.exec,
		HomeDir:  home,
		Version:  "v0.18.0+test",
		LocalUID: 1000,
	}, f
}

// happyPathResponses is the canned set every "everything works" test
// uses. Extracted so tests focused on one failure mode (e.g., bad
// UID) don't have to spell out five others.
func happyPathResponses() map[string]fakeSSHResp {
	return map[string]fakeSSHResp{
		"id":             {stdout: []byte("1001\n")},
		"push-wl-paste":  {},
		"push-wl-copy":   {},
		"verify-wrapper": {stdout: []byte("text/plain;charset=utf-8\n")},
		"verify-path":    {stdout: []byte("/home/avi/.local/bin/wl-paste\n")},
	}
}

func TestInstallOnHost_HappyPath(t *testing.T) {
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()

	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost: %v", err)
	}
	// Five SSH calls: id + push wl-paste + push wl-copy + verify-wrapper + verify-path.
	if len(f.calls) != 5 {
		t.Fatalf("expected 5 SSH calls, got %d: %v", len(f.calls), f.calls)
	}
	// SSH snippet landed in the per-host file:
	snippetPath := filepath.Join(inst.HomeDir, ".ssh", "config.d", "canopy", "tower.conf")
	data, err := os.ReadFile(snippetPath)
	if err != nil {
		t.Fatalf("snippet not written: %v", err)
	}
	body := string(data)
	for _, must := range []string{
		"Host tower",
		"/run/user/1000/canopy/clip-text.sock",
		"/run/user/1001/canopy/clip-text.sock",
		"v0.18.0+test",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("snippet missing %q\nbody:\n%s", must, body)
		}
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
	if !bytes.Contains(pasteCall.stdin, []byte("clip-image.sock")) {
		t.Error("wl-paste push stdin missing clip-image.sock reference (wrong template rendered?)")
	}
	if !bytes.Contains(copyCall.stdin, []byte("clip-copy.sock")) {
		t.Error("wl-copy push stdin missing clip-copy.sock reference")
	}
}

func TestInstallOnHost_UIDDetectionFailureAbortsBeforeWrites(t *testing.T) {
	inst, f := newTestHostInstaller(t)
	f.responses = map[string]fakeSSHResp{
		"id": {err: errors.New("connection refused")},
	}
	var out bytes.Buffer
	err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out)
	if err == nil {
		t.Fatal("InstallOnHost should fail when id -u fails")
	}
	// No snippet should have been written:
	snippetPath := filepath.Join(inst.HomeDir, ".ssh", "config.d", "canopy", "tower.conf")
	if _, err := os.Stat(snippetPath); err == nil {
		t.Error("snippet written despite UID detection failure — install must be atomic up through detect")
	}
	if len(f.calls) != 1 {
		t.Errorf("expected install to abort after the id call, got %d SSH calls", len(f.calls))
	}
}

func TestInstallOnHost_RefusesZeroRemoteUID(t *testing.T) {
	// `id -u` returns 0 means we're root on the remote. /run/user/0/
	// is root's runtime dir; an unprivileged user-mode daemon can't
	// listen there. Refuse loudly.
	inst, f := newTestHostInstaller(t)
	f.responses = map[string]fakeSSHResp{
		"id": {stdout: []byte("0\n")},
	}
	var out bytes.Buffer
	err := inst.InstallOnHost(context.Background(), "tower", "root@tower.lan", &out)
	if err == nil {
		t.Fatal("InstallOnHost must refuse remote UID 0")
	}
	if !strings.Contains(err.Error(), "UID 0") {
		t.Errorf("error message should mention UID 0; got %v", err)
	}
}

func TestInstallOnHost_VerifyFailureSurfacesAfterArtifactsWritten(t *testing.T) {
	// Verify is the LAST step. If verify fails, the artifacts (wrappers,
	// snippet) have already been written. The error must surface so
	// the caller flips the host's ClipboardBridge state to "broken",
	// but the artifacts stay on disk so a Repair retry has something
	// to work with.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	f.responses["verify-wrapper"] = fakeSSHResp{err: errors.New("wl-paste: command not found")}
	var out bytes.Buffer
	err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out)
	if err == nil {
		t.Fatal("InstallOnHost should report verify failure")
	}
	if !strings.Contains(err.Error(), "verify") {
		t.Errorf("error message should mention verify; got %v", err)
	}
	snippetPath := filepath.Join(inst.HomeDir, ".ssh", "config.d", "canopy", "tower.conf")
	if _, statErr := os.Stat(snippetPath); statErr != nil {
		t.Errorf("snippet should still exist after verify failure (for Repair retry); got %v", statErr)
	}
}

func TestInstallOnHost_VerifyRejectsImpostorOutput(t *testing.T) {
	// If something between the wrapper and the daemon swallowed our
	// output and the verify probe gets back, say, "image/png" (real
	// wl-paste's behavior when only an image is in clipboard), the
	// bridge isn't actually wired up — our wrapper ALWAYS emits
	// text/plain. Refuse the install.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	f.responses["verify-wrapper"] = fakeSSHResp{stdout: []byte("image/png\n")}
	var out bytes.Buffer
	err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out)
	if err == nil {
		t.Fatal("InstallOnHost should refuse when verify output doesn't carry text/plain")
	}
}

func TestInstallOnHost_VerifyClassifiesMissingSocat(t *testing.T) {
	// When socat isn't on the remote, the wrapper exits with a
	// distinctive stderr. Surface a `apt install socat` hint rather
	// than the raw error text.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	f.responses["verify-wrapper"] = fakeSSHResp{
		err:    errors.New("exit status 127"),
		stderr: []byte("/home/avi/.local/bin/wl-paste: line 50: socat: command not found"),
	}
	var out bytes.Buffer
	err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out)
	if err == nil {
		t.Fatal("InstallOnHost should report verify failure on missing socat")
	}
	if !strings.Contains(err.Error(), "socat") || !strings.Contains(err.Error(), "install") {
		t.Errorf("error should hint at installing socat; got %v", err)
	}
}

func TestInstallOnHost_VerifyWarnsOnPathPrecedenceButSucceeds(t *testing.T) {
	// Wrapper itself works (verify-wrapper passes), but login-shell PATH
	// resolves wl-paste to /usr/bin/wl-paste — Claude Code on the remote
	// won't use our wrapper. The install completes successfully (the
	// bridge is functionally installed) but the user gets a clear inline
	// warning + fix suggestion. Crucial test: without this branch the
	// user discovers the symptom much later when image paste silently
	// fails inside a remote Claude session.
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

func TestInstallOnHost_VerifyClassifiesConnectionRefused(t *testing.T) {
	// Daemon down on the laptop → socat fails to connect → wrapper
	// exits non-zero. Distinct error message from missing-socat case.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	f.responses["verify-wrapper"] = fakeSSHResp{
		err:    errors.New("exit status 1"),
		stderr: []byte("socat[1234] E connect(...) Connection refused"),
	}
	var out bytes.Buffer
	err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out)
	if err == nil {
		t.Fatal("InstallOnHost should report verify failure on connection refused")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Errorf("error should hint that the laptop daemon may be down; got %v", err)
	}
}

func TestInstallOnHost_RefusesNonNumericRemoteUID(t *testing.T) {
	inst, f := newTestHostInstaller(t)
	f.responses = map[string]fakeSSHResp{
		"id": {stdout: []byte("not-a-number\n")},
	}
	var out bytes.Buffer
	err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out)
	if err == nil {
		t.Fatal("InstallOnHost should refuse a non-numeric UID")
	}
}

func TestInstallOnHost_RefusesZeroLocalUID(t *testing.T) {
	// Mirror of the remote-UID check on the local side. running canopy
	// as root would point local sockets at /run/user/0/ — not where the
	// systemd user daemon listens.
	inst, _ := newTestHostInstaller(t)
	inst.LocalUID = 0
	var out bytes.Buffer
	err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out)
	if err == nil {
		t.Fatal("InstallOnHost must refuse local UID 0")
	}
	if !strings.Contains(err.Error(), "local UID") {
		t.Errorf("error should mention local UID; got %v", err)
	}
}
