package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/ghx"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// newWorkspaceFlags holds parsed CLI flags. Package-level so they're
// easy to test/inspect. v0.6 added --pr / --issue / --branch /
// --allow-local for the source-variant flows; the original --name
// and --no-attach still work as before.
var newWorkspaceFlags struct {
	name      string
	noAttach  bool
	pr        int    // --pr <num>: check out this PR's branch into a workspace
	issue     int    // --issue <num>: create workspace, briefing references this issue
	branch    string // --branch <name>: check out an existing branch
	allowLoc  bool   // --allow-local: with --branch, allow non-existent on origin
}

// newCmd returns the `canopy new` cobra subcommand.
//
// Source variants (mutually exclusive):
//
//	canopy new                       # fresh workspace, random name
//	canopy new --name fix-bug        # fresh, explicit name
//	canopy new --pr 42               # check out PR #42 into a new workspace
//	canopy new --issue 17            # implementation workspace seeded with issue body
//	canopy new --branch feat/x       # check out existing branch from origin
//	canopy new --branch feat/x --allow-local
//	                                 # check out existing local-only branch
func newCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new workspace and attach to its tmux session",
		Long: "Generates a random adjective-noun workspace name (or uses --name),\n" +
			"creates a git worktree, runs scripts.setup, builds the standard 4-pane\n" +
			"tmux session (nvim / claude / shell / scripts.run), and attaches.\n\n" +
			"Source variants (mutually exclusive):\n" +
			"  --pr <num>     check out PR <num>'s branch (briefing includes PR body)\n" +
			"  --issue <num>  fresh branch off main; briefing seeded with issue body\n" +
			"  --branch <n>   check out existing branch <n> from origin\n" +
			"  --allow-local  with --branch, allow checkout of a local-only branch",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateNewFlags(); err != nil {
				return err
			}
			mgr, err := loadManager()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			opts, name, err := buildCreateOptions(ctx, mgr.Cfg.ProjectRoot, newWorkspaceFlags.name)
			if err != nil {
				return err
			}

			ws, err := mgr.Create(ctx, name, opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				// Even on failure, print the workspace summary if we have one
				// so the user knows where to find logs / what to clean up.
				if ws != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"\nworkspace %q is in status %q.\n",
						ws.Name, ws.Status)
					if ws.LastErrorHint != "" {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"hint: %s\n", ws.LastErrorHint)
					}
					fmt.Fprintf(cmd.ErrOrStderr(),
						"See ~/.canopy/log/canopy.log for full details.\n"+
							"Once you've fixed the issue, `canopy retry %s` re-runs scripts.setup\n"+
							"against the existing worktree (preserves branch, port, claude history).\n"+
							"Or `canopy rm %s` to drop it entirely.\n",
						ws.Name, ws.Name)
				}
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"\nWorkspace ready: %s\n  branch:  %s\n  path:    %s\n  port:    %d\n  session: %s\n",
				ws.Name, ws.Branch, ws.Path, ws.Port, ws.TmuxSession)

			if newWorkspaceFlags.noAttach {
				fmt.Fprintf(cmd.OutOrStdout(),
					"\nSkipping attach (--no-attach). Run `canopy switch %s` to attach later.\n", ws.Name)
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "\nAttaching tmux session %s...\n", ws.TmuxSession)
			// Attach replaces the canopy process via syscall.Exec on success.
			// If we return from Attach, it failed.
			return mgr.Tmux.Attach(ctx, ws.TmuxSession)
		},
	}
	cmd.Flags().StringVar(&newWorkspaceFlags.name, "name", "",
		"explicit workspace name (default: random adjective-noun)")
	cmd.Flags().BoolVar(&newWorkspaceFlags.noAttach, "no-attach", false,
		"don't auto-attach to the tmux session after creation")
	cmd.Flags().IntVar(&newWorkspaceFlags.pr, "pr", 0,
		"check out the given PR number into a new workspace (uses gh)")
	cmd.Flags().IntVar(&newWorkspaceFlags.issue, "issue", 0,
		"seed the briefing with the given issue's body (uses gh)")
	cmd.Flags().StringVar(&newWorkspaceFlags.branch, "branch", "",
		"check out an existing branch (must exist on origin unless --allow-local)")
	cmd.Flags().BoolVar(&newWorkspaceFlags.allowLoc, "allow-local", false,
		"with --branch, allow a branch that exists only locally (no origin/<name>)")
	return cmd
}

// validateNewFlags rejects mutually-exclusive combinations and other
// shapes that don't make sense (--allow-local without --branch, etc.).
// Run before any expensive work so the user gets a fast error.
func validateNewFlags() error {
	srcCount := 0
	if newWorkspaceFlags.pr > 0 {
		srcCount++
	}
	if newWorkspaceFlags.issue > 0 {
		srcCount++
	}
	if newWorkspaceFlags.branch != "" {
		srcCount++
	}
	if srcCount > 1 {
		return fmt.Errorf("--pr, --issue, and --branch are mutually exclusive (got %d)", srcCount)
	}
	if newWorkspaceFlags.allowLoc && newWorkspaceFlags.branch == "" {
		return fmt.Errorf("--allow-local only makes sense with --branch")
	}
	return nil
}

// buildCreateOptions converts the parsed flags + project root into a
// workspace.CreateOptions plus the resolved workspace name. Heavy
// lifting per source-variant lives here:
//
//   - --pr <n>: gh pr view; for same-repo PRs, set StartPoint to
//     origin/<headRefName>. For cross-repo (fork) PRs, fetch
//     refs/pull/<n>/head into a local canopy/pr-<n> ref and use
//     that. Briefing carries the PR title+body.
//   - --issue <n>: gh issue view; briefing carries the issue
//     title+body. No special branch handling — fresh worktree off
//     origin/<default>.
//   - --branch <name>: validate the branch exists (origin first,
//     then local if --allow-local). Use it as StartPoint and check
//     out the existing branch (no -b).
//   - none: return zero-value options + the user-supplied name.
//
// The returned name is what mgr.Create will use as the workspace
// identifier. For --pr we default to "pr-<num>" so the workspace
// label matches the PR; for --branch we default to the branch's
// basename. The user can still override with --name.
func buildCreateOptions(ctx context.Context, projectRoot, userName string) (workspace.CreateOptions, string, error) {
	switch {
	case newWorkspaceFlags.pr > 0:
		return optionsForPR(ctx, projectRoot, newWorkspaceFlags.pr, userName)
	case newWorkspaceFlags.issue > 0:
		return optionsForIssue(ctx, projectRoot, newWorkspaceFlags.issue, userName)
	case newWorkspaceFlags.branch != "":
		return optionsForBranch(ctx, projectRoot, newWorkspaceFlags.branch, newWorkspaceFlags.allowLoc, userName)
	}
	return workspace.CreateOptions{}, userName, nil
}

// optionsForPR fetches PR metadata via gh, prepares the local branch
// (fetching the PR head for cross-repo PRs), and returns options +
// resolved name. Returns a clear error when gh is unavailable or the
// PR doesn't exist — the user knows immediately what to fix.
func optionsForPR(ctx context.Context, projectRoot string, num int, userName string) (workspace.CreateOptions, string, error) {
	pr, err := ghx.FetchPR(ctx, projectRoot, num)
	if err != nil {
		if errors.Is(err, ghx.ErrUnavailable) {
			return workspace.CreateOptions{}, "", fmt.Errorf(
				"--pr requires the gh CLI: install via https://cli.github.com/ and run `gh auth login`")
		}
		return workspace.CreateOptions{}, "", fmt.Errorf("--pr %d: %w", num, err)
	}

	// Branch routing — three sub-cases, each picks the right git
	// worktree shape:
	//
	//  A. Cross-repo (fork) PR: the head isn't on origin as a branch
	//     ref. Fetch it under refs/heads/canopy/pr-<num> via the
	//     refs/pull/<num>/head refspec (works for forks AND same-repo,
	//     so it's the universal PR-fetch primitive). Then check out
	//     that local branch via AddExisting (no -b).
	//  B. Same-repo PR with no local branch yet: create a tracking
	//     branch via `git worktree add -b <head> <path> origin/<head>`.
	//     This is the common case for `gh pr checkout`-style flows.
	//  C. Same-repo PR with a local branch already (e.g. user previously
	//     ran `gh pr checkout 1185`): check out the local branch.
	//
	// Why this matters: `git worktree add <path> origin/<head>` (no -b)
	// produces a DETACHED HEAD because git treats origin/<head> as a
	// commit-ish, not a branch reference. The PR's hints then fail
	// (`gh pr view HEAD` is meaningless) and `git status` shows
	// "not on any branch" — the bug just-reported by users.
	name := userName
	if name == "" {
		name = fmt.Sprintf("pr-%d", pr.Number)
	}

	if pr.IsCrossRepository {
		ref := fmt.Sprintf("canopy/pr-%d", pr.Number)
		spec := fmt.Sprintf("refs/pull/%d/head:%s", pr.Number, ref)
		if err := git.FetchRefspec(ctx, projectRoot, "origin", spec); err != nil {
			return workspace.CreateOptions{}, "", fmt.Errorf("fetch PR #%d head: %w", pr.Number, err)
		}
		return workspace.CreateOptions{
			SourceKind:    "pr",
			SourceContext: formatPRContext(pr),
			Branch:        ref,
			StartPoint:    ref,
			CreateBranch:  false, // local ref already exists post-fetch
		}, name, nil
	}

	// Same-repo. Make sure origin/<head> is locally known.
	branch := pr.HeadRefName
	originRef := "origin/" + branch
	if !git.RefExists(ctx, projectRoot, originRef) {
		_ = git.Fetch(ctx, projectRoot, "origin")
	}

	if git.RefExists(ctx, projectRoot, branch) {
		// Case C: local branch already exists; check it out.
		return workspace.CreateOptions{
			SourceKind:    "pr",
			SourceContext: formatPRContext(pr),
			Branch:        branch,
			StartPoint:    branch,
			CreateBranch:  false,
		}, name, nil
	}
	// Case B: create a fresh tracking branch from origin/<head>.
	return workspace.CreateOptions{
		SourceKind:    "pr",
		SourceContext: formatPRContext(pr),
		Branch:        branch,
		StartPoint:    originRef,
		CreateBranch:  true, // -b <branch> on top of origin/<branch>
	}, name, nil
}

// formatPRContext renders the PR metadata into the briefing's
// "Source context" body. Wrapped by sourceKindBlock with the
// data-not-instructions delimiter; we just produce the inner text.
func formatPRContext(pr *ghx.PR) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PR #%d: %s\n", pr.Number, pr.Title)
	if pr.IsCrossRepository && pr.HeadRepositoryOwner.Login != "" {
		fmt.Fprintf(&b, "From fork: %s\n", pr.HeadRepositoryOwner.Login)
	}
	fmt.Fprintf(&b, "Base: %s\n", pr.BaseRefName)
	fmt.Fprintf(&b, "Head: %s\n\n", pr.HeadRefName)
	if body := strings.TrimSpace(pr.Body); body != "" {
		b.WriteString(body)
	} else {
		b.WriteString("(no PR body)")
	}
	return b.String()
}

// optionsForIssue fetches issue metadata and seeds the briefing. No
// special branch handling — the workspace is a fresh branch off
// origin/<default>, just like a no-flag canopy new, but the briefing
// tells the agent it's working on issue <num> with the body as spec.
func optionsForIssue(ctx context.Context, projectRoot string, num int, userName string) (workspace.CreateOptions, string, error) {
	iss, err := ghx.FetchIssue(ctx, projectRoot, num)
	if err != nil {
		if errors.Is(err, ghx.ErrUnavailable) {
			return workspace.CreateOptions{}, "", fmt.Errorf(
				"--issue requires the gh CLI: install via https://cli.github.com/ and run `gh auth login`")
		}
		return workspace.CreateOptions{}, "", fmt.Errorf("--issue %d: %w", num, err)
	}

	name := userName
	if name == "" {
		name = fmt.Sprintf("issue-%d", iss.Number)
	}

	return workspace.CreateOptions{
		SourceKind:    "issue",
		SourceContext: formatIssueContext(iss),
		// Branch / StartPoint / CreateBranch left at zero values so
		// the legacy "fresh branch off origin/<default>" path runs.
	}, name, nil
}

// formatIssueContext mirrors formatPRContext for issues.
func formatIssueContext(iss *ghx.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Issue #%d: %s\n", iss.Number, iss.Title)
	if iss.State != "" {
		fmt.Fprintf(&b, "State: %s\n", iss.State)
	}
	b.WriteString("\n")
	if body := strings.TrimSpace(iss.Body); body != "" {
		b.WriteString(body)
	} else {
		b.WriteString("(no issue body)")
	}
	return b.String()
}

// optionsForBranch validates the branch exists (origin first, then
// local if --allow-local) and prepares to check it out into a worktree.
// Returns a clear error explaining the --allow-local escape hatch when
// only a local branch matches.
func optionsForBranch(ctx context.Context, projectRoot, branch string, allowLocal bool, userName string) (workspace.CreateOptions, string, error) {
	// Try fetching first so a recently-pushed branch is visible.
	// Best-effort; errors fall through (the RefExists check below
	// either passes or surfaces a clear miss).
	_ = git.Fetch(ctx, projectRoot, "origin")

	originRef := "origin/" + branch
	localExists := git.RefExists(ctx, projectRoot, branch)
	originExists := git.RefExists(ctx, projectRoot, originRef)

	switch {
	case localExists:
		// Local branch already exists — check it out as-is. Don't
		// re-create from origin even if origin/<branch> also exists;
		// that would either error (branch taken) or surprise the
		// user by silently overwriting their local state.
		return workspace.CreateOptions{
			SourceKind:   "branch",
			Branch:       branch,
			StartPoint:   branch,
			CreateBranch: false,
		}, defaultBranchName(branch, userName), nil

	case originExists:
		// No local branch yet — create a fresh tracking branch from
		// origin/<branch>. `git worktree add -b <name> <path>
		// origin/<name>` is the right invocation; using AddExisting
		// here would detach HEAD, which is the bug we're fixing.
		return workspace.CreateOptions{
			SourceKind:   "branch",
			Branch:       branch,
			StartPoint:   originRef,
			CreateBranch: true,
		}, defaultBranchName(branch, userName), nil

	case allowLocal:
		// Neither local nor origin — but --allow-local was passed,
		// so the user expected a local branch to exist. Surface a
		// clearer error for that specific failure mode.
		return workspace.CreateOptions{}, "", fmt.Errorf(
			"branch %q not found locally (--allow-local was set). Run `git branch %s` first, "+
				"or drop --allow-local to require an origin/<branch>.", branch, branch)

	default:
		return workspace.CreateOptions{}, "", fmt.Errorf(
			"branch %q not found on origin/. Run `git fetch origin` then retry, "+
				"or pass --allow-local if you have a local-only branch.", branch)
	}
}

// defaultBranchName returns the user-supplied name when set, else the
// branch's basename (the part after the last "/"). "feature/oauth" →
// "oauth"; "main" → "main"; "" stays "" (caller's defensive check).
func defaultBranchName(branch, userName string) string {
	if userName != "" {
		return userName
	}
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		return branch[i+1:]
	}
	return branch
}
