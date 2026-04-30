package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFormatGitStats covers the per-count rendering rules: zero counts
// hidden, non-zero rendered with their leading glyph in fixed order
// (ahead → behind → dirty).
func TestFormatGitStats(t *testing.T) {
	cases := []struct {
		ahead, behind, dirty int
		want                 string
	}{
		{0, 0, 0, ""},
		{3, 0, 0, "↑3"},
		{0, 1, 0, "↓1"},
		{0, 0, 5, "*5"},
		{3, 1, 5, "↑3 ↓1 *5"},
		{0, 1, 5, "↓1 *5"},
		{3, 0, 5, "↑3 *5"},
		{3, 1, 0, "↑3 ↓1"},
	}
	for _, tc := range cases {
		if got := formatGitStats(tc.ahead, tc.behind, tc.dirty); got != tc.want {
			t.Errorf("formatGitStats(%d, %d, %d) = %q; want %q",
				tc.ahead, tc.behind, tc.dirty, got, tc.want)
		}
	}
}

// TestDetectGitStats_AllClean: a workspace with no commits past main,
// no commits behind, no uncommitted changes returns nil. The badge
// renderer's "no badge for clean rows" contract depends on this.
func TestDetectGitStats_AllClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "clean-ws")

	ws := makeWorkspace("clean-ws", "clean-ws", wt, source)
	got := detectGitStats(context.Background(), ws)
	if got != nil {
		t.Errorf("detectGitStats on clean workspace = %+v; want nil", got)
	}
}

// TestDetectGitStats_AheadAndDirty: a workspace with one local commit
// past main and one uncommitted file shows ↑1 *1.
func TestDetectGitStats_AheadAndDirty(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "ahead-and-dirty")

	// One real commit on the workspace branch.
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out, err := exec.Command("git", "-C", wt, "add", "feature.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", wt, "commit", "-m", "feat").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// One uncommitted file (modified, not staged).
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("write scratch: %v", err)
	}

	ws := makeWorkspace("ahead-and-dirty", "ahead-and-dirty", wt, source)
	got := detectGitStats(context.Background(), ws)
	if got == nil {
		t.Fatal("expected git_stats hint; got nil")
	}
	if got.Kind != "git_stats" {
		t.Errorf("Kind = %q; want git_stats", got.Kind)
	}
	if !strings.Contains(got.Message, "↑1") {
		t.Errorf("expected ↑1 in message %q", got.Message)
	}
	if !strings.Contains(got.Message, "*1") {
		t.Errorf("expected *1 in message %q", got.Message)
	}
}

// TestDetectGitStats_Behind: a workspace whose origin/main has advanced
// past it (no work on the branch yet) shows ↓1 — exactly the misty-
// marsh state at the time the user complained about the false-positive
// "shipped" badge. Now that branch correctly shows ↓N (informational)
// instead of ✓ merged (wrong claim).
func TestDetectGitStats_Behind(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "behind-ws")

	// Advance origin/main by one commit without doing anything on the workspace branch.
	if out, err := exec.Command("git", "-C", source, "checkout", "main").CombinedOutput(); err != nil {
		t.Fatalf("checkout main: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", source,
		"commit", "--allow-empty", "-m", "main advance").CombinedOutput(); err != nil {
		t.Fatalf("main advance: %v\n%s", err, out)
	}
	mainSha, _ := exec.Command("git", "-C", source, "rev-parse", "main").Output()
	if out, err := exec.Command("git", "-C", source, "update-ref",
		"refs/remotes/origin/main", strings.TrimSpace(string(mainSha))).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}

	ws := makeWorkspace("behind-ws", "behind-ws", wt, source)
	got := detectGitStats(context.Background(), ws)
	if got == nil {
		t.Fatal("expected git_stats hint for behind branch; got nil")
	}
	if !strings.Contains(got.Message, "↓1") {
		t.Errorf("expected ↓1 in message %q (branch is 1 behind main)", got.Message)
	}
	// No ahead, no dirty.
	if strings.Contains(got.Message, "↑") {
		t.Errorf("unexpected ahead glyph in %q", got.Message)
	}
	if strings.Contains(got.Message, "*") {
		t.Errorf("unexpected dirty glyph in %q", got.Message)
	}
}
