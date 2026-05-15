// init_source.go — helpers that turn a `canopy init` positional arg
// into a usable filesystem path.
//
// Split from init.go to keep the cobra wiring there focused on parsing.
// Pure functions here so they're easy to unit-test in isolation.
//
// Concepts:
//
//   - looksLikeGitURL classifies the user's first positional arg as a
//     git URL vs a local path. Strict whitelist (decision #1 in the
//     v0.18 design doc): http/https/git/ssh/file scheme, OR SSH-style
//     `user@host:path`. Everything else is a path.
//
//   - deriveBasename pulls the project name out of a URL the way
//     `git clone` does: strip .git suffix, take the last path segment.
//     Used both for the basename-collision check (pre-clone, decision
//     #6) and for default destination naming.
//
//   - resolveCloneDest implements the precedence stack from decision
//     #2: CLI 2nd-positional dest > $CANOPY_SOURCE_ROOT > config
//     source-root > ~/.canopy/sources. The returned dest is the
//     directory the URL will be cloned INTO.
//
//   - ensureSourceRoot creates the source-root directory lazily, only
//     when a clone is about to happen (decision #4). `canopy config
//     set source-root /home/avi/Work` does NOT make ~/Work; that
//     would mutate the filesystem on a config command.
//
//   - validateDestNotInsideWorkspace refuses clones that would land
//     inside ~/.canopy/workspaces/* (decision #7). Cloning into a
//     workspace dir confuses canopy reconcile and creates phantom
//     repos inside worktrees.

package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/state"
)

// allowedURLSchemes is the closed set of URL schemes that count as
// "this is a git URL, clone it." Anything else (including bare
// `gopher://`, `mailto:`, etc.) falls through to "treat as path."
//
// Order matters only for human readability: https first because that's
// what GitHub copy-paste produces.
var allowedURLSchemes = map[string]bool{
	"https": true,
	"http":  true,
	"git":   true,
	"ssh":   true,
	"file":  true,
}

// sshLikeRE matches the SSH-shortcut URL form `git@host:owner/repo.git`
// that GitHub and GitLab give you when you click "Use SSH". This form
// has no scheme so url.Parse won't classify it as a URL. The pattern
// requires user@host:something-non-empty; this is strict enough that
// no real filesystem path will accidentally match (Linux/macOS paths
// can't contain `@host:` at the start).
//
// Pattern breakdown:
//
//	^[A-Za-z0-9_.-]+   user (no `:` allowed)
//	@                  literal @
//	[A-Za-z0-9.-]+     host (no `:` allowed, no `/`)
//	:                  literal :
//	.+                 path (anything non-empty, may contain /)
var sshLikeRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9.-]+:.+`)

// looksLikeGitURL reports whether s is recognized as a git URL.
// Strict whitelist; everything else is a path. False positives are
// the failure mode that worries us here (treating a real local path
// as a URL would try to git-clone it instead of init it), so we err
// toward "path" on anything ambiguous.
//
// Empty string is a path (the cwd default branch handles it upstream).
func looksLikeGitURL(s string) bool {
	if s == "" {
		return false
	}
	// SSH shortcut first — it has no scheme so url.Parse would
	// classify it as a relative path with `:` in it.
	if sshLikeRE.MatchString(s) {
		return true
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return false
	}
	return allowedURLSchemes[strings.ToLower(u.Scheme)]
}

// deriveBasename extracts the project name from a git URL the way
// `git clone <url>` would name the default destination dir:
//
//	https://github.com/foo/bar.git           → bar
//	https://github.com/foo/bar               → bar
//	git@github.com:foo/bar.git               → bar
//	ssh://git@github.com/foo/bar/            → bar
//	file:///tmp/repo.git                     → repo
//
// Returns ("", error) if the URL has no meaningful basename — empty
// path, all slashes, etc. Callers use this as the basename-collision
// key BEFORE cloning so a stale Projects entry doesn't waste a clone.
func deriveBasename(rawURL string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("derive basename: empty URL")
	}

	var path string
	if sshLikeRE.MatchString(rawURL) {
		// Everything after the first `:` is the path component.
		i := strings.Index(rawURL, ":")
		path = rawURL[i+1:]
	} else {
		u, err := url.Parse(rawURL)
		if err != nil {
			return "", fmt.Errorf("derive basename: parse %q: %w", rawURL, err)
		}
		path = u.Path
	}

	// Trim trailing slashes (e.g. `git clone https://x/y/` → "y").
	path = strings.TrimRight(path, "/")
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".git")

	if base == "" || base == "." || base == "/" {
		return "", fmt.Errorf("derive basename: %q has no usable name", rawURL)
	}
	return base, nil
}

// resolveCloneDest computes the absolute path a URL clone will land at.
// Precedence (highest wins):
//
//  1. destOverride non-empty → use it verbatim (resolved to absolute
//     against cwd if relative)
//  2. $CANOPY_SOURCE_ROOT env / config.json source-root / default —
//     handled by config.ResolveSourceRoot. The basename derived from
//     the URL is appended to the resolved source-root.
//
// canopyHome is passed in (not derived from os.UserHomeDir) so tests
// can inject a tempdir.
//
// Returns (absDest, sourceLabel, err). sourceLabel reports where the
// SOURCE-ROOT came from for the destOverride==""  path (env/config/
// default), and "override" when destOverride was used. The label is
// surfaced by `canopy init` so the user knows why their clone landed
// where it did.
func resolveCloneDest(rawURL, destOverride string, c *config.UserConfig, canopyHome string) (absDest, sourceLabel string, err error) {
	if destOverride != "" {
		abs, err := filepath.Abs(destOverride)
		if err != nil {
			return "", "", fmt.Errorf("resolve dest: %w", err)
		}
		return abs, "override", nil
	}
	base, err := deriveBasename(rawURL)
	if err != nil {
		return "", "", err
	}
	root, src := config.ResolveSourceRoot(c, canopyHome)
	return filepath.Join(root, base), string(src), nil
}

// ensureSourceRoot creates the parent directory of dest (i.e., the
// source-root) if it doesn't exist. Lazy mkdir per decision #4: we
// don't create on `canopy config set`; we create just before the
// first clone that would write into it.
//
// MkdirAll is idempotent — no-op if the dir already exists. Returns
// a wrapped error if creation fails (permission denied, path is a
// file, etc.) so the user sees the actionable cause.
//
// Operates on filepath.Dir(dest) so callers can pass the eventual
// dest path and we DIY the parent.
func ensureSourceRoot(dest string) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("ensure source-root %s: %w", parent, err)
	}
	return nil
}

// validateDestNotInsideWorkspace refuses a clone destination that
// would land inside ~/.canopy/workspaces/* (a canopy-managed worktree).
// Cloning there would create a phantom git repo inside an existing
// worktree, which `canopy reconcile` and the global tab would treat
// as garbage.
//
// The check walks every registered workspace's Path and verifies
// `dest` is not under any of them. Matches by prefix (with a trailing
// separator) so `~/.canopy/workspaces/foo-bar/sources` doesn't
// false-match against `~/.canopy/workspaces/foo`.
//
// st may be nil (state.json missing); in that case there are no
// workspaces to collide with and the check trivially passes.
func validateDestNotInsideWorkspace(dest string, st *state.State) error {
	if st == nil || len(st.Workspaces) == 0 {
		return nil
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("validate dest: %w", err)
	}
	for _, ws := range st.Workspaces {
		if ws.Path == "" {
			continue
		}
		wsAbs, err := filepath.Abs(ws.Path)
		if err != nil {
			continue
		}
		// Equal-path or descendant — use trailing separator on the
		// workspace path so dir prefix-matches don't false-fire.
		if absDest == wsAbs || strings.HasPrefix(absDest, wsAbs+string(filepath.Separator)) {
			return fmt.Errorf(
				"refusing to clone into %s: that path is inside canopy workspace %q at %s. "+
					"Pick a different destination (canopy init <url> <other-dest>) or "+
					"configure source-root: canopy config set source-root <path>",
				dest, ws.Name, wsAbs)
		}
	}
	return nil
}
