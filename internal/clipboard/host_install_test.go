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
	// Remote socket-dir mkdir: `bash` (no -c) + script piped via stdin.
	// Pattern from refresh.go: avoids the SSH-word-split trap that
	// turned bash -c "mkdir -p X && chmod Y X" into a no-op + error.
	if len(args) == 1 && args[0] == "bash" && len(stdin) > 0 &&
		strings.Contains(string(stdin), "mkdir -p /run/user/") &&
		strings.Contains(string(stdin), "/canopy") {
		return "mkdir-remote-sockdir"
	}
	// Verify-step 1: `bash` (no -c) + stdin = `exec ".../wl-paste"
	// --list-types`. Stdin pattern avoids SSH word-split.
	if len(args) == 1 && args[0] == "bash" && len(stdin) > 0 &&
		strings.Contains(string(stdin), "wl-paste") &&
		strings.Contains(string(stdin), "--list-types") {
		return "verify-wrapper"
	}
	// Verify-step 2: `bash -l` (login) + stdin = `command -v wl-paste`.
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
		// Default to a no-op CloseMaster so existing tests don't have
		// to thread it through. Tests that specifically assert master
		// teardown reach in and override this field.
		CloseMaster: func(string) {},
		HomeDir:     home,
		Version:     "v0.18.0+test",
		LocalUID:    1000,
	}, f
}

// happyPathResponses is the canned set every "everything works" test
// uses. Extracted so tests focused on one failure mode (e.g., bad
// UID) don't have to spell out six others.
func happyPathResponses() map[string]fakeSSHResp {
	return map[string]fakeSSHResp{
		"id":                   {stdout: []byte("1001\n")},
		"mkdir-remote-sockdir": {},
		"push-wl-paste":        {},
		"push-wl-copy":         {},
		"verify-wrapper":       {stdout: []byte("text/plain;charset=utf-8\n")},
		"verify-path":          {stdout: []byte("/home/avi/.local/bin/wl-paste\n")},
	}
}

func TestInstallOnHost_HappyPath(t *testing.T) {
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()

	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost: %v", err)
	}
	// Six SSH calls: id + mkdir + push wl-paste + push wl-copy + verify-wrapper + verify-path.
	if len(f.calls) != 6 {
		t.Fatalf("expected 6 SSH calls, got %d: %v", len(f.calls), f.calls)
	}
	// SSH snippet landed in the per-host file (filename keyed by canopy
	// host name; Host directive INSIDE keyed by SSH hostname):
	snippetPath := filepath.Join(inst.HomeDir, ".ssh", "config.d", "canopy", "tower.conf")
	data, err := os.ReadFile(snippetPath)
	if err != nil {
		t.Fatalf("snippet not written: %v", err)
	}
	body := string(data)
	for _, must := range []string{
		"Host tower.lan", // hostname parsed from "avi@tower.lan" — NOT "Host tower" (canopy alias)
		"/run/user/1000/canopy/clip-text.sock",
		"/run/user/1001/canopy/clip-text.sock",
		"v0.18.0+test",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("snippet missing %q\nbody:\n%s", must, body)
		}
	}
}

func TestInstallOnHost_CreatesRemoteSocketDirWithDetectedUID(t *testing.T) {
	// Regression: SSH RemoteForward bind() needs the parent dir to
	// exist. sshd doesn't mkdir. Without this step the snippet's
	// directives + Include scope can be perfect and the bridge still
	// silently fails because /run/user/<uid>/canopy/ on the remote
	// doesn't exist.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()
	// Detect a non-default UID to confirm we use IT, not the local
	// one. (Both default to 1001 in happyPathResponses; bump the
	// remote.)
	f.responses["id"] = fakeSSHResp{stdout: []byte("2042\n")}

	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost: %v", err)
	}
	// Find the mkdir call and assert its stdin script uses the
	// detected UID. (The script lives in stdin, NOT argv — argv is
	// just `bash`. The script-via-stdin pattern is load-bearing; see
	// the long comment in ensureRemoteSocketDir for why.)
	var found bool
	for _, c := range f.calls {
		if classifyCall(c.args, c.stdin) != "mkdir-remote-sockdir" {
			continue
		}
		body := string(c.stdin)
		if !strings.Contains(body, "/run/user/2042/canopy") {
			t.Errorf("mkdir step used wrong UID in script; body=\n%s", body)
		}
		if !strings.Contains(body, "chmod 0700") {
			t.Errorf("mkdir step missing chmod 0700; body=\n%s", body)
		}
		// argv must be just `bash` so the remote shell tokenizes the
		// whole script from stdin, not from a word-split bash -c arg.
		if len(c.args) != 1 || c.args[0] != "bash" {
			t.Errorf("mkdir step argv = %v, want exactly [bash] (script comes via stdin)", c.args)
		}
		found = true
		break
	}
	if !found {
		t.Errorf("InstallOnHost did not invoke the remote-socket-dir mkdir step")
	}
}

func TestHostnameFromSSHTarget(t *testing.T) {
	// SSH's Host pattern matching keys off the hostname portion of
	// the target. The snippet's Host directive must use that exact
	// string — anything else silently fails to match.
	cases := []struct {
		target string
		want   string
	}{
		{"tower.lan", "tower.lan"},
		{"avi@tower.lan", "tower.lan"},
		{"avi@tower.lan:22", "tower.lan"},
		{"tower.lan:2222", "tower.lan"},
		{"cassy@a10i-tower.geep-carat.ts.net", "a10i-tower.geep-carat.ts.net"},
		{"cassy@a10i-tower.geep-carat.ts.net:22", "a10i-tower.geep-carat.ts.net"},
		// LastIndex defends against an @ inside the user portion.
		{"weird@user@host.example.com", "host.example.com"},
		// A trailing :word that ISN'T digits is left attached (defends
		// against accidental IPv6 mangling; IPv6 isn't supported
		// upstream yet but the parser shouldn't actively make it
		// worse).
		{"host:notaport", "host:notaport"},
	}
	for _, c := range cases {
		got := hostnameFromSSHTarget(c.target)
		if got != c.want {
			t.Errorf("hostnameFromSSHTarget(%q) = %q, want %q", c.target, got, c.want)
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

func TestInstallOnHost_ResetsControlMasterBetweenSnippetAndVerify(t *testing.T) {
	// The bug this catches: RemoteForward directives in the freshly-
	// written snippet only take effect at the NEXT SSH handshake. An
	// already-open ControlMaster from a prior canopy command (host
	// install / refresh) carries no forwards, so verify would reuse
	// that master and the remote socket would never get created.
	// CloseMaster must fire AFTER the snippet write and BEFORE verify.
	inst, f := newTestHostInstaller(t)
	f.responses = happyPathResponses()

	var closedTargets []string
	var orderTrace []string
	inst.CloseMaster = func(target string) {
		closedTargets = append(closedTargets, target)
		orderTrace = append(orderTrace, "close-master")
	}
	// Hook into the snippet+verify ordering by wrapping SSHExec to
	// log each call's classification, so we can assert close-master
	// fell between snippet and verify-wrapper. Drain stdin into a
	// byte slice for our classification AND wrap it back into a
	// reader so the underlying fakeSSH still sees the push payload
	// (otherwise its own classifier reports "unknown" and the canned
	// response lookup misses).
	originalExec := inst.SSHExec
	inst.SSHExec = func(ctx context.Context, target string, stdin io.Reader, args ...string) ([]byte, []byte, error) {
		var stdinBytes []byte
		if stdin != nil {
			stdinBytes, _ = io.ReadAll(stdin)
		}
		orderTrace = append(orderTrace, classifyCall(args, stdinBytes))
		return originalExec(ctx, target, bytes.NewReader(stdinBytes), args...)
	}

	var out bytes.Buffer
	if err := inst.InstallOnHost(context.Background(), "tower", "avi@tower.lan", &out); err != nil {
		t.Fatalf("InstallOnHost: %v", err)
	}
	if len(closedTargets) != 1 || closedTargets[0] != "avi@tower.lan" {
		t.Errorf("CloseMaster should fire exactly once with the SSH target; got %v", closedTargets)
	}
	// Order assertion: id → push×2 → close-master → verify-wrapper → verify-path.
	// Specifically, close-master MUST be between the last push and verify-wrapper.
	closeIdx, verifyIdx := -1, -1
	lastPushIdx := -1
	for i, c := range orderTrace {
		switch c {
		case "close-master":
			closeIdx = i
		case "verify-wrapper":
			if verifyIdx == -1 {
				verifyIdx = i
			}
		case "push-wl-paste", "push-wl-copy":
			lastPushIdx = i
		}
	}
	if !(lastPushIdx < closeIdx && closeIdx < verifyIdx) {
		t.Errorf("CloseMaster must fall between last push (%d) and verify-wrapper (%d); got close at %d\ntrace: %v", lastPushIdx, verifyIdx, closeIdx, orderTrace)
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
