package git

import (
	"context"
	"os/exec"
	"strings"
)

// IsRepo reports whether dir is inside a git working tree.
//
// Implemented via `git -C <dir> rev-parse --is-inside-work-tree`. git prints
// "true" on success; on a non-repo dir it exits non-zero with a message on
// stderr. We treat any non-zero exit as "not a repo" rather than a failure
// condition, because the caller (cmd/canopy/route.go) wants a binary
// signal: route to init splash if this is a fresh repo, route to global
// mode otherwise. A missing git binary, a permissions error, or a dir that
// doesn't exist all collapse to "not a repo" via the returned error.
//
// The (bool, error) shape is preserved so callers that want to distinguish
// "definitely not a repo" from "couldn't even run git" can: a true return
// always means a repo was confirmed; a false return with a nil error means
// confirmed-not-a-repo; a false return with a non-nil error means we
// couldn't tell (git missing, exec failed, etc.) and the caller can decide.
func IsRepo(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		// ExitError with stderr "not a git repository" is the common
		// not-a-repo case; we collapse it to (false, nil) so callers
		// don't have to introspect the exit code. Any other error (git
		// binary missing, dir doesn't exist) we surface as (false, err).
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}
