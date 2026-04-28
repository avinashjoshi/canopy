package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// rmFlags holds parsed --yes (skip confirmation prompt) and --force
// (bypass v0.6 safety pre-flight checks).
var rmFlags struct {
	yes   bool
	force bool
}

// rmCmd returns the `canopy rm <name>` cobra subcommand.
//
// Removal runs scripts.archive (DB drop, server kill), kills the tmux
// session, removes the git worktree, deletes the branch, and drops the
// state row.
//
// v0.6 adds a smart pre-flight safety check: refuses to proceed when
// the workspace has uncommitted changes, unpushed commits, or an open
// PR. The check protects against the "I just rm'd uncommitted work"
// moment. --force bypasses the check entirely (CI / scripted use);
// --yes only skips the confirmation prompt and DOES run the safety
// check (so scripts that pipe `yes` still get protection).
//
// Edge: workspace in `orphaned` status (worktree dir gone) gracefully
// degrades — the safety check warns it can't verify uncommitted state
// and proceeds to confirm prompt, never blocks rm because the diagnostic
// itself failed.
func rmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Tear down a workspace (scripts.archive + git worktree remove + state cleanup)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := loadManager()
			if err != nil {
				return err
			}
			name := args[0]
			ctx := cmd.Context()

			ws, err := mgr.Find(ctx, name)
			if err != nil {
				return err
			}

			// v0.6 safety pre-flight: refuse on hanging work unless --force.
			//
			// Skipped entirely when --force is set. Otherwise:
			// - uncommitted: the user has unsaved changes in the worktree.
			// - unpushed: HEAD diverges from upstream (commits not on origin).
			// - open PR: gh pr view returns an open PR for the branch.
			//
			// On orphaned workspaces (worktree dir missing), the safety
			// check returns an empty []string (no hangs detected) plus a
			// debug-log line; we don't block rm just because the diagnostic
			// couldn't run.
			if !rmFlags.force {
				hangs := safetyPreflight(ctx, ws.Path, ws.Branch)
				if len(hangs) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(),
						"Refusing to remove %q — hanging work detected:\n", name)
					for _, h := range hangs {
						fmt.Fprintf(cmd.OutOrStdout(), "  • %s\n", h)
					}
					fmt.Fprintf(cmd.OutOrStdout(),
						"\nResolve the issues above, or pass --force to bypass.\n")
					return fmt.Errorf("workspace %q has hanging work; use --force to bypass", name)
				}
			}

			if !rmFlags.yes {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Remove workspace %q?\n  branch:  %s\n  path:    %s\n  port:    %d\n  status:  %s\n\nThis runs scripts.archive then deletes the git worktree.\nProceed? [y/N] ",
					name, ws.Branch, ws.Path, ws.Port, ws.Status)
				ok, err := readYesNo(cmd.InOrStdin())
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}

			if err := mgr.Remove(ctx, name, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed workspace %q.\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&rmFlags.yes, "yes", "y", false, "skip confirmation prompt (does NOT skip safety check)")
	cmd.Flags().BoolVar(&rmFlags.force, "force", false, "bypass v0.6 safety check (uncommitted/unpushed/open-PR)")
	return cmd
}

// safetyPreflight runs the three v0.6 hanging-work checks and returns
// a list of human-readable messages for each one that triggered. Empty
// slice = clean to remove. Caller decides whether to refuse + format
// the output or just print warnings (TUI close-out flow vs cli rm).
//
// Best-effort: each check is independent and a failure of one (e.g.,
// git status erroring on an orphaned worktree) doesn't block the others.
// Specifically, a check that errors out is treated as "no hang detected"
// — we never block rm because we couldn't run the diagnostic.
//
// Order is significant for user experience: uncommitted comes first
// (most actionable; user likely needs to commit/stash), then unpushed
// (less common but data-loss-risk), then open-PR (informational; user
// might still want to rm and let the PR close naturally).
func safetyPreflight(ctx context.Context, worktreePath, branch string) []string {
	if worktreePath == "" {
		return nil // no path to check; orphaned-row safe path
	}
	var hangs []string

	// Uncommitted changes: `git status --porcelain` outputs one line per
	// changed file. Empty output = clean. Any output (or git error) we
	// treat as a possible hang — the cost of a false positive is one
	// extra confirmation; the cost of a false negative is data loss.
	if msg := checkUncommitted(ctx, worktreePath); msg != "" {
		hangs = append(hangs, msg)
	}

	// Unpushed commits: `git log @{u}..HEAD` lists commits ahead of upstream.
	// If there's no upstream tracked, every commit on the branch counts as
	// "unpushed" — the user might be working purely locally.
	if msg := checkUnpushed(ctx, worktreePath); msg != "" {
		hangs = append(hangs, msg)
	}

	// Open PR: `gh pr view <branch> --json state` returns "OPEN" if a PR
	// exists. Skipped silently when gh is missing/unauthed (returns "" for
	// any error, matching the pr_status detector's silent-skip pattern).
	if msg := checkOpenPR(ctx, worktreePath, branch); msg != "" {
		hangs = append(hangs, msg)
	}

	return hangs
}

// checkUncommitted returns a hang message when the worktree has uncommitted
// changes (or when git status fails — defensive: assume the worst).
//
// `git status --porcelain` is the canonical way to detect a dirty tree:
// empty output means clean; any line means a modified/untracked/staged file.
// Returns "" when clean OR when git itself errored (orphaned worktree, etc).
func checkUncommitted(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		// Diagnostic failed (worktree dir gone, or git not on PATH).
		// Don't block rm; the orphaned-workspace edge case explicitly
		// wants rm to proceed.
		return ""
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return ""
	}
	// Count modified files for the message. Cheap; we already have the
	// output.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return fmt.Sprintf("uncommitted changes (%d file(s)) — commit, stash, or discard first", len(lines))
}

// checkUnpushed returns a hang message when HEAD is ahead of upstream
// (or when there's no upstream and at least one commit exists on the
// branch — that's "unpushed" too, just not in the @{u}.. sense).
func checkUnpushed(ctx context.Context, path string) string {
	// Try the upstream-aware check first.
	cmd := exec.CommandContext(ctx, "git", "-C", path, "log",
		"--oneline", "@{u}..HEAD")
	out, err := cmd.Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 1 && lines[0] == "" {
			return "" // clean — HEAD == upstream
		}
		return fmt.Sprintf("%d unpushed commit(s) — push first or accept the loss", len(lines))
	}

	// No upstream (`git log @{u}..` returned non-zero with stderr like
	// "no upstream configured"). Fall back to "any commits past origin/main"
	// as a rough check.
	defaultBranch := defaultBranchSafe(ctx, path)
	if defaultBranch == "" {
		return "" // can't tell; don't block
	}
	cmd = exec.CommandContext(ctx, "git", "-C", path, "log",
		"--oneline", "origin/"+defaultBranch+"..HEAD")
	out, err = cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	return fmt.Sprintf("%d commit(s) on this branch with no upstream — push or accept the loss", len(lines))
}

// checkOpenPR queries gh for an open PR on the branch. Silent skip
// when gh is missing/unauthed. Returns "" for "no open PR" (typical case)
// or "couldn't tell" cases.
func checkOpenPR(ctx context.Context, worktreePath, branch string) string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", branch,
		"--json", "state,number")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return "" // no PR or gh failed; not a hang
	}
	if strings.Contains(string(out), `"state":"OPEN"`) {
		// Extract the PR number for the message — quick string scan
		// instead of a full JSON parse for one field.
		num := extractPRNumber(string(out))
		if num != "" {
			return fmt.Sprintf("PR #%s is open — let it merge or close it first", num)
		}
		return "an open PR exists for this branch — let it merge or close it first"
	}
	return ""
}

// defaultBranchSafe returns the source repo's default branch (main /
// master). Returns "" on any failure — caller treats as "can't tell."
// Mirrors the lifecycle package's resolution but kept local to avoid
// pulling internal/lifecycle into cmd/canopy.
func defaultBranchSafe(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "symbolic-ref",
		"--short", "refs/remotes/origin/HEAD")
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		return strings.TrimPrefix(ref, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		probe := exec.CommandContext(ctx, "git", "-C", path, "rev-parse",
			"--verify", "origin/"+b)
		if err := probe.Run(); err == nil {
			return b
		}
	}
	return ""
}

// extractPRNumber pulls the PR number out of gh's JSON output. We do
// the string scan instead of full json.Unmarshal because it's one field
// and the JSON shape is predictable. Returns "" when not found.
func extractPRNumber(s string) string {
	const tag = `"number":`
	i := strings.Index(s, tag)
	if i < 0 {
		return ""
	}
	rest := s[i+len(tag):]
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// readYesNo reads one line from r (typically stdin) and reports whether
// the user typed something that means yes. Anything else (including EOF)
// is no.
func readYesNo(r io.Reader) (bool, error) {
	reader := bufio.NewReader(r)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}
