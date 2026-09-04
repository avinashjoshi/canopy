package clipboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOSC52Wrapper_PasteQueryReadDecode and TestOSC52Wrapper_CopyWrite
// are execution-based (not just static-content) tests for the OSC 52
// wrapper scripts' read/write logic: sending the query, reading the
// terminal's reply from a real pty in raw mode, parsing out the
// base64 payload, and (for copy) producing the correctly-escaped
// sequence including tmux's DCS passthrough wrapping.
//
// Go's stdlib has no pty allocation primitive, and the wrapper scripts
// open /dev/tty directly (the whole point of OSC 52 — querying the
// real controlling terminal) rather than stdin/stdout, so this can't
// be exercised with exec.Cmd's ordinary pipe plumbing. Both tests
// shell out to a small Python pty harness (testdata/osc52_test_harness.py)
// that plays the role of "the local terminal emulator" on the pty
// master side — the same tier as this package's existing skip-if-
// missing external test deps (bash -n, socat, timeout; see
// scripts_test.go). Skipped when python3 isn't on PATH.
func TestOSC52Wrapper_PasteQueryReadDecode(t *testing.T) {
	runOSC52HarnessTest(t, "paste", WrapperWlPaste)
}

func TestOSC52Wrapper_CopyWrite(t *testing.T) {
	runOSC52HarnessTest(t, "copy", WrapperWlCopy)
}

func runOSC52HarnessTest(t *testing.T, mode string, w WrapperScript) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}

	content, _, err := WrapperContent(w, "v0.19.0+test")
	if err != nil {
		t.Fatalf("WrapperContent: %v", err)
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, string(w))
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		t.Fatalf("write rendered wrapper: %v", err)
	}

	harness := filepath.Join("testdata", "osc52_test_harness.py")
	cmd := exec.Command("python3", harness, mode, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("osc52_test_harness.py %s failed: %v\noutput:\n%s", mode, err, out)
	} else {
		t.Logf("osc52_test_harness.py %s output:\n%s", mode, out)
	}
}
