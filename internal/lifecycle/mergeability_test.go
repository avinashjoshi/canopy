package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFormatMergeability covers the singular-vs-plural rendering rule:
// 1 conflict → "⚠ conflict" (no count, tightest), N conflicts → counted.
func TestFormatMergeability(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{1, "⚠ conflict"},
		{2, "⚠ 2 conflicts"},
		{5, "⚠ 5 conflicts"},
		{42, "⚠ 42 conflicts"},
	}
	for _, tc := range cases {
		if got := formatMergeability(tc.n); got != tc.want {
			t.Errorf("formatMergeability(%d) = %q; want %q", tc.n, got, tc.want)
		}
	}
}

// TestDetectMergeability_Clean: a fresh workspace where HEAD ==
// origin/main hits the ancestor short-circuit and returns nil. This is
// the happy steady state — most workspaces most of the time.
func TestDetectMergeability_Clean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "clean-merge")

	ws := makeWorkspace("clean-merge", "clean-merge", wt, source)
	if got := detectMergeability(context.Background(), ws); got != nil {
		t.Errorf("clean workspace = %+v; want nil", got)
	}
}

// TestDetectMergeability_NoConflictDivergence: branch and main both
// advanced past the merge-base, but on disjoint files. merge-tree
// returns exit 0 (clean merge) → detector returns nil. Verifies that
// the ancestor short-circuit does NOT fire here (target is no longer
// an ancestor of HEAD), so this exercises the actual merge-tree path.
func TestDetectMergeability_NoConflictDivergence(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)
	wt := setupWorkspace(t, source, "no-conflict")

	// Branch commit edits file A.
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("branch-a\n"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	mustGit(t, wt, "add", "a.txt")
	mustGit(t, wt, "commit", "-m", "branch a")

	// Main commit edits file B (different file → no conflict).
	mustGit(t, source, "checkout", "main")
	if err := os.WriteFile(filepath.Join(source, "b.txt"), []byte("main-b\n"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	mustGit(t, source, "add", "b.txt")
	mustGit(t, source, "commit", "-m", "main b")
	advanceOriginMain(t, source)

	ws := makeWorkspace("no-conflict", "no-conflict", wt, source)
	if got := detectMergeability(context.Background(), ws); got != nil {
		t.Errorf("disjoint-file divergence = %+v; want nil (clean merge)", got)
	}
}

// TestDetectMergeability_Conflict: branch and main both modify the same
// file with different content. merge-tree exits 1 → detector returns a
// Hint with Kind="mergeability" and a message+action that hints at the
// fix.
func TestDetectMergeability_Conflict(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	source := setupSourceRepo(t)

	// Seed shared.txt on main before branching so both sides have a
	// common ancestor for the file. Forces a real modify/modify
	// conflict (vs an add/add) — closer to the realistic case.
	if err := os.WriteFile(filepath.Join(source, "shared.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write shared.txt: %v", err)
	}
	mustGit(t, source, "add", "shared.txt")
	mustGit(t, source, "commit", "-m", "seed shared")
	advanceOriginMain(t, source)

	wt := setupWorkspace(t, source, "conflict-ws")

	// Branch overwrites shared.txt with one version.
	if err := os.WriteFile(filepath.Join(wt, "shared.txt"), []byte("branch-version\n"), 0o644); err != nil {
		t.Fatalf("branch write: %v", err)
	}
	mustGit(t, wt, "add", "shared.txt")
	mustGit(t, wt, "commit", "-m", "branch shared")

	// Main overwrites shared.txt with a different version.
	mustGit(t, source, "checkout", "main")
	if err := os.WriteFile(filepath.Join(source, "shared.txt"), []byte("main-version\n"), 0o644); err != nil {
		t.Fatalf("main write: %v", err)
	}
	mustGit(t, source, "add", "shared.txt")
	mustGit(t, source, "commit", "-m", "main shared")
	advanceOriginMain(t, source)

	ws := makeWorkspace("conflict-ws", "conflict-ws", wt, source)
	got := detectMergeability(context.Background(), ws)
	if got == nil {
		t.Fatal("expected mergeability hint; got nil (is git ≥2.38?)")
	}
	if got.Kind != "mergeability" {
		t.Errorf("Kind = %q; want mergeability", got.Kind)
	}
	if !strings.Contains(got.Message, "conflict") {
		t.Errorf("expected 'conflict' in Message %q", got.Message)
	}
	if !strings.Contains(got.Action, "rebase") {
		t.Errorf("expected 'rebase' in Action %q", got.Action)
	}
}

// mustGit runs `git -C path args...` and fails the test on error.
// Local helper kept here (vs in detect_test.go) so this lane stays
// self-contained for parallel review with the stuck_state and
// push_state lanes — no shared edits to detect_test.go.
func mustGit(t *testing.T, path string, args ...string) {
	t.Helper()
	full := append([]string{"-C", path}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// advanceOriginMain forges refs/remotes/origin/main to point at source's
// current main HEAD. Used after committing on source's main to simulate
// a fetch from a real remote, without needing one.
func advanceOriginMain(t *testing.T, source string) {
	t.Helper()
	out, err := exec.Command("git", "-C", source, "rev-parse", "main").Output()
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	sha := strings.TrimSpace(string(out))
	if cout, err := exec.Command("git", "-C", source, "update-ref",
		"refs/remotes/origin/main", sha).CombinedOutput(); err != nil {
		t.Fatalf("update-ref origin/main: %v\n%s", err, cout)
	}
}
