package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// RoleInfo is the (pane ID, role) tuple returned by LookupAllPanes.
//
// NOT named PaneInfo: that's already taken by process.go for the
// (Index, ID, PID, Title) tuple. Both types are needed for distinct
// purposes; both stay distinct.
type RoleInfo struct {
	// ID is the tmux pane ID (e.g. "%15"). Stable for the pane's lifetime.
	ID string
	// Role is the value of the @canopy-role tmux user-option for this pane.
	Role string
}

// ErrPaneNotFound is returned by LookupPane when no pane in the session
// has a @canopy-role matching the requested role (or role glob).
var ErrPaneNotFound = errors.New("tmux: pane with role not found")

// roleOption is the tmux user-option key canopy uses to tag pane roles.
// The leading "@" is required by tmux for user-defined options.
const roleOption = "@canopy-role"

// SetRole tags a pane with a canopy role using the @canopy-role tmux
// user-option. paneID must be a tmux pane ID (e.g. "%15"), typically
// the return value of Create or SplitPane. Idempotent: setting the
// same role twice is a no-op.
//
// Why user-options instead of pane title or environment vars: tmux user-
// options are namespaced (the leading @), persistent for the pane's
// lifetime, and process-proof (nothing inside the pane can clobber
// them via terminal escapes the way TUIs can override pane title).
//
// Verified at design time via a manual bash spike — the
// `<session>:.0` pane-target shorthand does NOT work; using a real
// pane ID is the canonical pattern.
func (c *Client) SetRole(ctx context.Context, paneID, role string) error {
	if paneID == "" {
		return fmt.Errorf("tmux.SetRole: empty pane ID")
	}
	if role == "" {
		return fmt.Errorf("tmux.SetRole(%s): empty role", paneID)
	}
	cmd := exec.CommandContext(ctx, "tmux",
		c.args("set-option", "-p", "-t", paneID, roleOption, role)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.SetRole(%s, %s): %w (stderr: %s)",
			paneID, role, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// LookupPane finds the pane in the given session whose @canopy-role
// matches role. Returns ErrPaneNotFound if no match.
//
// Glob support: a trailing "*" makes the match a prefix match. E.g.,
// `LookupPane(session, "agent:*")` matches any pane whose role starts
// with "agent:". Matched via strings.HasPrefix; NOT path.Match (wrong
// stdlib package — that's for filesystem globs with `/` separators).
//
// Multi-match behavior: if multiple panes match, log a warn line and
// return the first by tmux's list-panes output ORDER for the session.
// (NOT pane creation order — list-panes order is tmux's natural
// ordering for the session/window and is stable as long as panes
// aren't killed/recreated.) For deterministic plural cases (future
// multi-agent), use exact role names like "agent:claude" not the glob.
//
// Scope: uses `list-panes -s -t <session>` for session-scoped queries
// across all windows. NOT `-a` (server-wide; would match panes from
// other workspaces' sessions on the same tmux server).
func (c *Client) LookupPane(ctx context.Context, session, role string) (string, error) {
	matches, err := c.LookupAllPanes(ctx, session, role)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", ErrPaneNotFound
	}
	if len(matches) > 1 {
		// Surface accidental duplicates without breaking happy-path
		// (eng-review 3A). Intentional plural cases should use exact
		// role names, not the glob.
		log.Warn("tmux.lookup-pane.multi-match",
			"session", session, "role", role, "count", len(matches))
	}
	return matches[0].ID, nil
}

// LookupAllPanes returns every pane in the session whose @canopy-role
// matches role. Returns an empty slice (NOT ErrPaneNotFound) when no
// matches exist — collection-API convention: the caller may legitimately
// want to know "no agents in this session yet" without it being an error.
//
// See LookupPane for the matching rules (prefix glob via trailing "*",
// session-scoped via -s, output ordering via list-panes natural order).
func (c *Client) LookupAllPanes(ctx context.Context, session, role string) ([]RoleInfo, error) {
	// Format spec: `#{@canopy-role}` keeps the leading @. tmux's user-
	// option references retain the @ in `#{...}` (verified empirically;
	// `#{canopy-role}` returns empty). Don't strip.
	args := c.args("list-panes", "-s", "-t", session, "-F",
		fmt.Sprintf("#{pane_id}\t#{%s}", roleOption))

	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tmux.LookupAllPanes(%s, %s): %w (stderr: %s)",
			session, role, err, strings.TrimSpace(stderr.String()))
	}

	prefixMatch := strings.HasSuffix(role, "*")
	prefix := strings.TrimSuffix(role, "*")

	var matches []RoleInfo
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			// Pane with no @canopy-role set produces just the pane ID +
			// tab + empty string; SplitN(2) still returns 2 parts. A
			// 1-part line means tmux returned malformed output.
			log.Warn("tmux.list-panes.malformed-line", "line", line)
			continue
		}
		id, paneRole := parts[0], parts[1]
		if paneRole == "" {
			continue // pane has no role — not a match
		}
		if prefixMatch {
			if !strings.HasPrefix(paneRole, prefix) {
				continue
			}
		} else if paneRole != role {
			continue
		}
		matches = append(matches, RoleInfo{ID: id, Role: paneRole})
	}
	return matches, nil
}

// ListAllRoles returns every (pane ID, role) pair in the session for
// panes that have @canopy-role set. Used by the backfill path to check
// whether all required roles are present without doing N LookupPane
// calls.
//
// Equivalent to LookupAllPanes(ctx, session, "*") but more readable
// at the call site.
func (c *Client) ListAllRoles(ctx context.Context, session string) ([]RoleInfo, error) {
	return c.LookupAllPanes(ctx, session, "*")
}

// SelectPane focuses the given pane (by tmux pane ID like "%15").
// Separate from SelectPaneDirection which moves focus by direction
// (-L, -R, -U, -D). Use this when you have a specific pane ID in
// scope and want to land focus there.
//
// Multi-window note: if the target pane is in a window other than
// the currently-active one for the attached client, plain
// `select-pane -t <paneID>` selects the pane within its window but
// doesn't switch the attached client's view to that window. Canopy
// today creates one window per session (verified — no new-window
// calls in internal/ or cmd/), so this is a non-issue. If multi-
// window layouts ever ship, this needs to also call select-window.
//
// Errors are non-fatal at the call site (existing pattern from
// SelectPaneDirection): failure to select a pane shouldn't tear
// down a workspace build.
func (c *Client) SelectPane(ctx context.Context, paneID string) error {
	if paneID == "" {
		return fmt.Errorf("tmux.SelectPane: empty pane ID")
	}
	cmd := exec.CommandContext(ctx, "tmux",
		c.args("select-pane", "-t", paneID)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.SelectPane(%s): %w (stderr: %s)",
			paneID, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// PaneCount returns the total number of panes in the session across
// all windows. Used by the backfill path to skip non-canonical
// layouts (only 3-pane sessions get retroactively tagged).
//
// Uses `-s` (session-scoped, all-windows) for the same reason
// LookupAllPanes does. NOT `-a` (server-wide).
func (c *Client) PaneCount(ctx context.Context, session string) (int, error) {
	args := c.args("list-panes", "-s", "-t", session, "-F", "#{pane_id}")
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("tmux.PaneCount(%s): %w (stderr: %s)",
			session, err, strings.TrimSpace(stderr.String()))
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line != "" {
			count++
		}
	}
	return count, nil
}

// PanesInOrder returns the pane IDs of the session's panes in tmux's
// natural list-panes output ORDER (NOT pane index — tmux's pane-base-
// index is user-configurable and codex-corrected: positional logic
// must use list-panes output order, not raw `#{pane_index}`, to be
// safe across `pane-base-index 1` configs).
//
// Used by the backfill path to map the canonical 3-pane layout
// (position 0 = ide, position 1 = terminal:shell, position 2 = agent)
// without tripping on base-index config.
func (c *Client) PanesInOrder(ctx context.Context, session string) ([]string, error) {
	args := c.args("list-panes", "-s", "-t", session, "-F", "#{pane_id}")
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tmux.PanesInOrder(%s): %w (stderr: %s)",
			session, err, strings.TrimSpace(stderr.String()))
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids, nil
}
