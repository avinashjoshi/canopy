// Package canopyinit hosts the shared logic for the "add a project to
// canopy" feature (v0.20). Two consumers import this package:
//
//   - cmd/canopy (the CLI: `canopy init [PATH_OR_URL] [DEST]`)
//   - internal/ui (the TUI: addProjectFormMode on splash and Global tab)
//
// Putting the pure-input helpers here lets both call sites validate +
// resolve destinations using the same code. Without this package the
// UI would either duplicate ~150 LOC of URL/path logic or import
// cmd/canopy (forbidden by the leaf-up dependency rule in CLAUDE.md).
//
// What lives here:
//
//   - URL detection (LooksLikeGitURL) — strict whitelist + SSH-shortcut regex
//   - Basename derivation (DeriveBasename) — what `git clone` would name
//   - Destination resolution (ResolveCloneDest) — env > config > default
//   - Lazy mkdir (EnsureSourceRoot) — created just before the clone
//   - Workspace path safety (ValidateDestNotInsideWorkspace) — refuse
//     clones that would land inside ~/.canopy/workspaces/*
//
// What does NOT live here (intentionally):
//
//   - runInit itself — stays in cmd/canopy because it ties cobra-adjacent
//     state helpers together. The UI gets a callback for that.
//   - git.Clone — already public in internal/git.

package canopyinit

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

// allowedURLSchemes — the closed set of schemes that count as "git URL,
// clone it." Everything else falls through to path semantics.
var allowedURLSchemes = map[string]bool{
	"https": true,
	"http":  true,
	"git":   true,
	"ssh":   true,
	"file":  true,
}

// sshLikeRE matches GitHub/GitLab's SSH shortcut `git@host:owner/repo.git`.
// Requires user@host:something-non-empty. Strict enough that no Linux
// or macOS path will accidentally match.
var sshLikeRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9.-]+:.+`)

// LooksLikeGitURL reports whether s is recognized as a git URL.
// Strict whitelist; everything else is a path. False positives matter
// more than false negatives here — treating a real local path as a URL
// would try to git-clone it instead of init it.
func LooksLikeGitURL(s string) bool {
	if s == "" {
		return false
	}
	if sshLikeRE.MatchString(s) {
		return true
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return false
	}
	return allowedURLSchemes[strings.ToLower(u.Scheme)]
}

// DeriveBasename extracts the project name from a git URL the way
// `git clone <url>` names the default destination dir.
func DeriveBasename(rawURL string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("derive basename: empty URL")
	}
	var path string
	if sshLikeRE.MatchString(rawURL) {
		i := strings.Index(rawURL, ":")
		path = rawURL[i+1:]
	} else {
		u, err := url.Parse(rawURL)
		if err != nil {
			return "", fmt.Errorf("derive basename: parse %q: %w", rawURL, err)
		}
		path = u.Path
	}
	path = strings.TrimRight(path, "/")
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".git")
	if base == "" || base == "." || base == "/" {
		return "", fmt.Errorf("derive basename: %q has no usable name", rawURL)
	}
	return base, nil
}

// ResolveCloneDest computes the absolute path a URL clone will land at.
// Precedence: destOverride > $CANOPY_SOURCE_ROOT > config.SourceRoot >
// <canopyHome>/sources. Returns (absDest, sourceLabel, err).
//
// canopyHome is passed in so tests can inject t.TempDir().
func ResolveCloneDest(rawURL, destOverride string, c *config.UserConfig, canopyHome string) (absDest, sourceLabel string, err error) {
	if destOverride != "" {
		// Expand leading `~` so a TUI-typed dest like `~/Work/foo`
		// resolves to `$HOME/Work/foo` rather than passing literal
		// `~` to filepath.Abs (which would build `/cwd/~/Work/foo`).
		abs, err := filepath.Abs(config.ExpandTilde(destOverride))
		if err != nil {
			return "", "", fmt.Errorf("resolve dest: %w", err)
		}
		return abs, "override", nil
	}
	base, err := DeriveBasename(rawURL)
	if err != nil {
		return "", "", err
	}
	root, src := config.ResolveSourceRoot(c, canopyHome)
	return filepath.Join(root, base), string(src), nil
}

// EnsureSourceRoot creates the parent directory of dest if it doesn't
// exist. Lazy mkdir — setting source-root via `canopy config set` does
// NOT create the dir; we create it here just before a clone that
// needs it.
func EnsureSourceRoot(dest string) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("ensure source-root %s: %w", parent, err)
	}
	return nil
}

// ValidateDestNotInsideWorkspace refuses a clone dest that would land
// inside ~/.canopy/workspaces/* (a canopy-managed worktree). Cloning
// there creates a phantom repo inside an existing worktree, which
// reconcile and the Global tab treat as garbage.
//
// st may be nil (state.json missing); in that case there are no
// workspaces to collide with and the check passes.
func ValidateDestNotInsideWorkspace(dest string, st *state.State) error {
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
