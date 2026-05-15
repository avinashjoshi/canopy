package canopyinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/state"
)

// TestLooksLikeGitURL covers the 13-row matrix from the design doc's
// test diagram. Whitelist + SSH regex must accept real URLs and reject
// every shape of local path.
func TestLooksLikeGitURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		// URL shapes — must return true.
		{"https://github.com/foo/bar.git", true},
		{"http://example.com/foo/bar", true},
		{"git://github.com/foo/bar.git", true},
		{"ssh://git@host.example.com/foo/bar", true},
		{"file:///tmp/repo.git", true},
		{"git@github.com:foo/bar.git", true},
		{"git@gitlab.com:org/sub/repo.git", true},

		// Path shapes — must return false.
		{"/tmp/foo", false},
		{"./relative", false},
		{"../parent/path", false},
		{"~/code/foo", false},
		{"foo", false},
		{"foo/bar", false},

		// Edge cases — must return false (path or rejected).
		{"", false},
		{"git@host", false},   // no colon
		{"user@host:", false}, // no path after colon
		{"mailto:x@y", false}, // unsupported scheme
		{"gopher://host/path", false},
		{"c:foo", false}, // Windows-style; canopy is unix but be defensive
	}
	for _, tc := range cases {
		got := LooksLikeGitURL(tc.in)
		if got != tc.want {
			t.Errorf("LooksLikeGitURL(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// TestDeriveBasename: every real URL shape produces the expected name.
func TestDeriveBasename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		want      string
		wantError bool
	}{
		{"https://github.com/foo/bar.git", "bar", false},
		{"https://github.com/foo/bar", "bar", false},
		{"http://example.com/owner/proj.git", "proj", false},
		{"git@github.com:foo/bar.git", "bar", false},
		{"git@gitlab.com:org/sub/repo.git", "repo", false},
		{"ssh://git@host/path/repo/", "repo", false}, // trailing slash
		{"file:///tmp/repo.git", "repo", false},
		// Bare repo (no .git, no slashes after host) — derives "host"
		// for ssh://host/. Real-world: not a case we care about, but
		// the function should still produce SOMETHING readable.
		{"git://host.example/", "", true}, // path is empty after host
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := DeriveBasename(tc.in)
		if tc.wantError {
			if err == nil {
				t.Errorf("DeriveBasename(%q): nil error; want refused", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("DeriveBasename(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("DeriveBasename(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestResolveCloneDest_OverrideWins: explicit destOverride beats env
// and config. This is the `canopy init <url> <dest>` second-positional
// path.
func TestResolveCloneDest_OverrideWins(t *testing.T) {
	t.Setenv("CANOPY_SOURCE_ROOT", "/from-env")
	c := &config.UserConfig{SourceRoot: "/from-config"}

	got, src, err := ResolveCloneDest("https://github.com/foo/bar.git", "/explicit/dest", c, "/home/canopy")
	if err != nil {
		t.Fatalf("ResolveCloneDest: %v", err)
	}
	if got != "/explicit/dest" {
		t.Errorf("dest = %q; want /explicit/dest", got)
	}
	if src != "override" {
		t.Errorf("source = %q; want override", src)
	}
}

// TestResolveCloneDest_OverrideRelative: relative dest is resolved
// against the test's cwd. We can't predict the absolute path exactly,
// but we can assert it's absolute.
func TestResolveCloneDest_OverrideRelative(t *testing.T) {
	// Not parallel — sibling tests Unsetenv CANOPY_SOURCE_ROOT.
	got, _, err := ResolveCloneDest("https://github.com/foo/bar.git", "./relative-dest", nil, "/home/canopy")
	if err != nil {
		t.Fatalf("ResolveCloneDest: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("dest = %q; want absolute", got)
	}
	if filepath.Base(got) != "relative-dest" {
		t.Errorf("dest = %q; want basename relative-dest", got)
	}
}

// TestResolveCloneDest_FromEnv: env wins over config, basename appended.
func TestResolveCloneDest_FromEnv(t *testing.T) {
	t.Setenv("CANOPY_SOURCE_ROOT", "/from-env")
	c := &config.UserConfig{SourceRoot: "/from-config"}

	got, src, err := ResolveCloneDest("https://github.com/foo/bar.git", "", c, "/home/canopy")
	if err != nil {
		t.Fatalf("ResolveCloneDest: %v", err)
	}
	if got != "/from-env/bar" {
		t.Errorf("dest = %q; want /from-env/bar", got)
	}
	if src != "env" {
		t.Errorf("source = %q; want env", src)
	}
}

// TestResolveCloneDest_FromConfig: env unset, config wins over default.
func TestResolveCloneDest_FromConfig(t *testing.T) {
	os.Unsetenv("CANOPY_SOURCE_ROOT")
	c := &config.UserConfig{SourceRoot: "/from-config"}

	got, src, err := ResolveCloneDest("https://github.com/foo/bar.git", "", c, "/home/canopy")
	if err != nil {
		t.Fatalf("ResolveCloneDest: %v", err)
	}
	if got != "/from-config/bar" {
		t.Errorf("dest = %q; want /from-config/bar", got)
	}
	if src != "config" {
		t.Errorf("source = %q; want config", src)
	}
}

// TestResolveCloneDest_FromDefault: nothing set, default applies.
// Default is <canopyHome>/sources/<basename>.
func TestResolveCloneDest_FromDefault(t *testing.T) {
	os.Unsetenv("CANOPY_SOURCE_ROOT")
	got, src, err := ResolveCloneDest("https://github.com/foo/bar.git", "", nil, "/home/canopy")
	if err != nil {
		t.Fatalf("ResolveCloneDest: %v", err)
	}
	want := "/home/canopy/sources/bar"
	if got != want {
		t.Errorf("dest = %q; want %q", got, want)
	}
	if src != "default" {
		t.Errorf("source = %q; want default", src)
	}
}

// TestResolveCloneDest_BadURL: a URL that derives an empty basename
// fails before any dest is built.
func TestResolveCloneDest_BadURL(t *testing.T) {
	// Not parallel — keeps the ResolveCloneDest test group serial.
	_, _, err := ResolveCloneDest("git://host.example/", "", nil, "/home/canopy")
	if err == nil {
		t.Fatal("ResolveCloneDest with empty basename: nil error; want refused")
	}
}

// TestEnsureSourceRoot_Idempotent: missing dir is created; existing
// dir is a no-op. Both must succeed.
func TestEnsureSourceRoot_Idempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dest := filepath.Join(root, "fresh", "bar") // parent doesn't exist

	if err := EnsureSourceRoot(dest); err != nil {
		t.Fatalf("EnsureSourceRoot first call: %v", err)
	}
	// Parent should exist now.
	if _, err := os.Stat(filepath.Join(root, "fresh")); err != nil {
		t.Errorf("EnsureSourceRoot didn't create parent: %v", err)
	}
	// Second call: idempotent.
	if err := EnsureSourceRoot(dest); err != nil {
		t.Fatalf("EnsureSourceRoot second call: %v", err)
	}
}

// TestEnsureSourceRoot_PermissionDenied wraps the underlying error so
// the user sees the path that failed. We simulate via a parent dir
// that's not writable. Skipped if run as root (where chmod is ignored).
func TestEnsureSourceRoot_PermissionDenied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod doesn't enforce")
	}
	t.Parallel()
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o555); err != nil { // r-x, no write
		t.Fatalf("MkdirAll: %v", err)
	}
	dest := filepath.Join(locked, "child", "bar")
	err := EnsureSourceRoot(dest)
	if err == nil {
		t.Fatal("EnsureSourceRoot into read-only parent: nil error; want failure")
	}
	if !strings.Contains(err.Error(), "ensure source-root") {
		t.Errorf("err missing wrapping prefix; got %q", err)
	}
}

// TestValidateDestNotInsideWorkspace covers the path-safety check
// (decision #7): refuse clones that land inside any registered
// workspace's path.
func TestValidateDestNotInsideWorkspace(t *testing.T) {
	t.Parallel()
	st := &state.State{
		Workspaces: []state.Workspace{
			{Name: "ws-a", Path: "/home/avi/.canopy/workspaces/proj/ws-a"},
			{Name: "ws-b", Path: "/home/avi/.canopy/workspaces/proj/ws-b"},
		},
	}
	cases := []struct {
		dest      string
		wantError bool
		desc      string
	}{
		{"/home/avi/Work/bar", false, "outside any workspace — ok"},
		{"/home/avi/.canopy/workspaces/proj/ws-a", true, "exact workspace path — refused"},
		{"/home/avi/.canopy/workspaces/proj/ws-a/sources/x", true, "inside workspace — refused"},
		{"/home/avi/.canopy/workspaces/proj/ws-a-tail", false, "prefix-match must not false-fire (ws-a vs ws-a-tail)"},
		{"/home/avi/.canopy/workspaces/proj", false, "parent of workspaces — ok"},
	}
	for _, tc := range cases {
		err := ValidateDestNotInsideWorkspace(tc.dest, st)
		if tc.wantError && err == nil {
			t.Errorf("%s: dest=%s nil error; want refused", tc.desc, tc.dest)
		}
		if !tc.wantError && err != nil {
			t.Errorf("%s: dest=%s err=%v; want ok", tc.desc, tc.dest, err)
		}
	}
}

// TestValidateDestNotInsideWorkspace_NilState: a nil state (state.json
// missing on a fresh install) trivially passes — no workspaces to
// collide with.
func TestValidateDestNotInsideWorkspace_NilState(t *testing.T) {
	t.Parallel()
	if err := ValidateDestNotInsideWorkspace("/home/avi/Work/bar", nil); err != nil {
		t.Errorf("nil state: %v", err)
	}
}

// TestValidateDestNotInsideWorkspace_EmptyState: a state with no
// workspaces also trivially passes.
func TestValidateDestNotInsideWorkspace_EmptyState(t *testing.T) {
	t.Parallel()
	st := &state.State{}
	if err := ValidateDestNotInsideWorkspace("/home/avi/Work/bar", st); err != nil {
		t.Errorf("empty state: %v", err)
	}
}
