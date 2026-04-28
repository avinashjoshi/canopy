package git_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/clog"
	canopygit "github.com/avinashjoshi/canopy/internal/git"
)

// TestMain initializes clog once for the whole test binary so package-level
// `var log = clog.Pkg("git")` references resolve. Without this, slog's
// default handler would fire and the tests would print JSON to stderr.
func TestMain(m *testing.M) {
	teardown, err := clog.Init(false)
	if err != nil {
		// Logging init failure isn't a reason to abort tests — fall back to
		// the stdlib default handler.
		_ = err
	}
	defer teardown()
	m.Run()
}

// TestSanitize is the workhorse table-driven test. The cases enumerate the
// shapes of branch names canopy will see in practice plus a handful of edge
// cases we've explicitly decided how to handle.
func TestSanitize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "fix-bug", "fix-bug"},
		{"slashed two segments", "feature/oauth", "feature-oauth"},
		{"slashed three segments", "feature/sub/x", "feature-sub-x"},
		{"colon space", "feat: bug", "feat-bug"},
		{"jira ticket preserves case", "JIRA-1234", "JIRA-1234"},
		{"leading whitespace", "  spaced", "spaced"},
		{"trailing whitespace", "spaced  ", "spaced"},
		{"surrounding whitespace", "  spaced  ", "spaced"},
		{"underscore preserved", "snake_case", "snake_case"},
		{"dot preserved", "v1.2.3", "v1.2.3"},
		{"leading slash", "/leading", "leading"},
		{"trailing slash", "trailing/", "trailing"},
		{"only unsafe chars", "//", ""},
		{"empty", "", ""},
		{"runs collapse to single hyphen", "a///b", "a-b"},
		{"emoji and punctuation", "feat: 🚀 launch!", "feat-launch"},
	}

	for _, tc := range cases {
		tc := tc // pin loop variable for parallel-safe subtests
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := canopygit.Sanitize(tc.in)
			if got != tc.want {
				t.Errorf("Sanitize(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAddRemove_HappyPath exercises Add and Remove against a real scratch
// git repo. It is not a unit test in the strict sense — it shells out to
// `git` — but the alternative (mocking exec.Command) loses the actual value
// these wrappers provide, which is "did we get the git CLI invocation
// right." Slow tests are fine; they run in <1 second and only on demand.
func TestAddRemove_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping integration test")
	}

	repo := newScratchRepo(t)
	wt := filepath.Join(t.TempDir(), "wt-feature-x")
	ctx := context.Background()

	if err := canopygit.Add(ctx, repo, "feature-x", wt); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The worktree dir should now exist with a .git file pointing back to
	// the main repo. Don't assert the file shape; that's git's contract,
	// not canopy's.
	if _, err := exec.Command("git", "-C", wt, "rev-parse", "--show-toplevel").Output(); err != nil {
		t.Fatalf("worktree not usable: %v", err)
	}

	if err := canopygit.Remove(ctx, repo, wt, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

// TestAdd_BranchExists verifies that adding the same branch twice produces
// ErrBranchExists, which the workspace lifecycle relies on for idempotency
// (see design doc's idempotency table).
func TestAdd_BranchExists(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping integration test")
	}

	repo := newScratchRepo(t)
	first := filepath.Join(t.TempDir(), "wt-1")
	second := filepath.Join(t.TempDir(), "wt-2")
	ctx := context.Background()

	if err := canopygit.Add(ctx, repo, "feature-x", first); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := canopygit.Add(ctx, repo, "feature-x", second)
	if !errors.Is(err, canopygit.ErrBranchExists) {
		t.Fatalf("second Add: got %v; want errors.Is(... ErrBranchExists)", err)
	}
}

// TestRemove_PathNotFound covers the "tried to remove a worktree that
// doesn't exist" case — common when state.json reconciliation proposes
// removing an already-cleaned-up worktree.
func TestRemove_PathNotFound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping integration test")
	}

	repo := newScratchRepo(t)
	bogus := filepath.Join(t.TempDir(), "does-not-exist")

	err := canopygit.Remove(context.Background(), repo, bogus, false)
	if !errors.Is(err, canopygit.ErrPathNotFound) {
		t.Fatalf("Remove(missing): got %v; want errors.Is(... ErrPathNotFound)", err)
	}
}

// newScratchRepo creates a brand-new git repo in a temp dir with one empty
// initial commit (worktree-add requires at least one commit to know what
// HEAD points at). Returns the absolute path. Cleanup is automatic via t.TempDir.
func newScratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	steps := [][]string{
		{"init", "--initial-branch=main", dir},
		{"-C", dir, "config", "user.email", "test@canopy.local"},
		{"-C", dir, "config", "user.name", "canopy-test"},
		{"-C", dir, "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range steps {
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}
