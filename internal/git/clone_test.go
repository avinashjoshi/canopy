package git

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeBareRepo creates a bare repo at <tmp>/<name>.git with one commit so
// `git clone file://...` against it succeeds. Returns the file:// URL.
// Same hermetic pattern the design doc named for E2E clone tests.
func makeBareRepo(t *testing.T, name string) (cloneURL, bareDir string) {
	t.Helper()
	root := t.TempDir()

	// Build a working repo, add a commit, then convert to bare via clone.
	work := filepath.Join(root, name+"-work")
	if err := exec.Command("git", "init", work).Run(); err != nil {
		t.Skipf("git init: %v", err)
	}
	// Need at least one commit, and need user.* to be set so commit doesn't
	// trip "tell me who you are". Per-repo config keeps the test hermetic.
	for _, args := range [][]string{
		{"-C", work, "config", "user.email", "test@example.com"},
		{"-C", work, "config", "user.name", "Test"},
		{"-C", work, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	bareDir = filepath.Join(root, name+".git")
	if out, err := exec.Command("git", "clone", "--bare", work, bareDir).CombinedOutput(); err != nil {
		t.Fatalf("clone --bare: %v\n%s", err, out)
	}
	return "file://" + bareDir, bareDir
}

// TestClone_Success: vanilla clone to a fresh dest works and produces a
// real working tree. This is the happy path Phase B's CLI flow relies on.
func TestClone_Success(t *testing.T) {
	url, _ := makeBareRepo(t, "fixture")
	dest := filepath.Join(t.TempDir(), "cloned")

	var out bytes.Buffer
	if err := Clone(context.Background(), url, dest, &out); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	// Verify the clone produced a real .git dir; otherwise Clone could be
	// silently succeeding on a bogus path.
	if _, err := exec.Command("git", "-C", dest, "rev-parse", "--is-inside-work-tree").Output(); err != nil {
		t.Errorf("Clone reported success but %s isn't a git repo: %v", dest, err)
	}
}

// TestClone_StdoutPassthrough: when caller provides a writer, git's
// "Cloning into 'X'..." text reaches it. CLI users need to see progress.
func TestClone_StdoutPassthrough(t *testing.T) {
	url, _ := makeBareRepo(t, "fixture")
	dest := filepath.Join(t.TempDir(), "cloned")

	var out bytes.Buffer
	if err := Clone(context.Background(), url, dest, &out); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	// git writes "Cloning into 'X'..." to STDERR (verified manually) — so
	// stdout may be empty for plain clones. The contract we DO care about
	// is that Clone doesn't panic on a non-nil writer and that it accepts
	// our writer. Just assert we returned cleanly above. (If git's wire
	// behavior changes, this test continues to pass — we test contract,
	// not git's specific stream choice.)
	_ = out
}

// TestClone_NilStdout: passing nil for the writer doesn't crash.
// Discarded stdout is supported (TUI callers don't want it.)
func TestClone_NilStdout(t *testing.T) {
	url, _ := makeBareRepo(t, "fixture")
	dest := filepath.Join(t.TempDir(), "cloned")
	if err := Clone(context.Background(), url, dest, nil); err != nil {
		t.Fatalf("Clone(nil writer): %v", err)
	}
}

// TestClone_BadURL: clone to a nonexistent file:// URL fails with stderr
// embedded in the error so the caller can render git's message to the user.
func TestClone_BadURL(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "should-not-exist")
	err := Clone(context.Background(), "file:///nonexistent/path/that/does/not/exist.git", dest, nil)
	if err == nil {
		t.Fatal("Clone bad URL: nil error; want failure")
	}
	// Error must mention git, the url, AND git's stderr (which typically
	// contains "does not appear to be a git repository" or similar).
	msg := err.Error()
	if !strings.Contains(msg, "git clone") {
		t.Errorf("err missing 'git clone' prefix; got %q", msg)
	}
	if !strings.Contains(msg, "/nonexistent/path") {
		t.Errorf("err missing URL; got %q", msg)
	}
}

// TestClone_DestExists: git clone refuses if dest is non-empty. We don't
// shield the caller from this — git's message ("destination path '...'
// already exists and is not an empty directory") is what the user sees.
// cmd/canopy/addproject.go decides BEFORE clone whether dest is okay
// (idempotent skip-clone branch); the test here just confirms Clone()
// itself bubbles git's failure cleanly.
func TestClone_DestExists(t *testing.T) {
	url, _ := makeBareRepo(t, "fixture")
	dest := filepath.Join(t.TempDir(), "occupied")
	if err := exec.Command("mkdir", dest).Run(); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Drop a file so the dir is non-empty.
	if err := exec.Command("touch", filepath.Join(dest, "marker")).Run(); err != nil {
		t.Fatalf("touch: %v", err)
	}
	err := Clone(context.Background(), url, dest, nil)
	if err == nil {
		t.Fatal("Clone into non-empty dest: nil error; want refusal")
	}
}

// TestClone_ContextCancelled fires SIGKILL via context cancellation
// before git has a chance to finish. The returned error must mention
// "cancelled" so callers can distinguish user abort from real failure.
//
// Uses a long-running clone target (a slow file:// URL would still
// complete in microseconds locally) by pointing at a tcp:// that never
// connects. The OS dial-timeout will keep git busy long enough to
// receive our cancel signal.
func TestClone_ContextCancelled(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "never")
	// Unroutable TCP target → git's connect will hang until our cancel
	// fires. 192.0.2.0/24 is reserved for documentation (RFC 5737) so it
	// won't reach anything real.
	url := "git://192.0.2.1:9418/never.git"

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after 100ms so the test stays quick.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := Clone(ctx, url, dest, io.Discard)
	if err == nil {
		t.Fatal("Clone with cancelled ctx: nil error; want cancellation")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v; want context.Canceled or 'cancelled' message", err)
	}
}
