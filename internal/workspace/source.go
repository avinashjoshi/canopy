package workspace

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/oncactus/canopy/internal/ghx"
	"github.com/oncactus/canopy/internal/git"
)

// SourceSpec is the parsed-input form of canopy new's source-variant
// flags. Both surfaces (cmd/canopy/new.go --pr/--issue/--branch and
// internal/ui's new-workspace modal) populate a SourceSpec and hand it
// to Manager.ResolveSource for the gh + git plumbing. Keeps the
// branch-routing logic in one place; the CLI and TUI just collect
// input and render output.
//
// Mutual exclusion: at most one of PR / Issue / Branch should be set.
// Validate() enforces that at the boundary; the resolver itself does
// not double-check.
type SourceSpec struct {
	PR         int    // canopy new --pr <num>
	Issue      int    // canopy new --issue <num>
	Branch     string // canopy new --branch <name>
	AllowLocal bool   // canopy new --branch <n> --allow-local
}

// IsZero returns true when no source variant is selected. A zero
// SourceSpec produces a "fresh workspace off origin/<default>" — same
// as canopy new with no flags.
func (s SourceSpec) IsZero() bool {
	return s.PR == 0 && s.Issue == 0 && s.Branch == ""
}

// Validate rejects mutually-exclusive combinations and other shapes
// that don't make sense (--allow-local without --branch). Run before
// any expensive work so callers fail fast.
func (s SourceSpec) Validate() error {
	srcCount := 0
	if s.PR > 0 {
		srcCount++
	}
	if s.Issue > 0 {
		srcCount++
	}
	if s.Branch != "" {
		srcCount++
	}
	if srcCount > 1 {
		return fmt.Errorf("--pr, --issue, and --branch are mutually exclusive (got %d)", srcCount)
	}
	if s.AllowLocal && s.Branch == "" {
		return errors.New("--allow-local only makes sense with --branch")
	}
	return nil
}

// ParseSourceSpec interprets a human-typed source string from the TUI
// modal. Accepted shapes:
//
//	""                          → zero spec (fresh)
//	"pr 1234"  / "pr:1234"      → PR=1234
//	"issue 42" / "issue:42"     → Issue=42
//	"branch feat/oauth"         → Branch="feat/oauth"
//	"branch feat/oauth local"   → Branch="feat/oauth", AllowLocal=true
//	"branch:feat/oauth"         → Branch="feat/oauth"
//
// Whitespace tolerant; case-insensitive on the keyword. Errors return
// a clear message so the modal can show it as a status line.
func ParseSourceSpec(input string) (SourceSpec, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return SourceSpec{}, nil
	}

	// Normalize "kind:rest" to "kind rest" so both delimiters work.
	if idx := strings.IndexByte(input, ':'); idx > 0 && !strings.ContainsAny(input[:idx], " \t") {
		input = input[:idx] + " " + input[idx+1:]
	}

	parts := strings.Fields(input)
	if len(parts) < 2 {
		return SourceSpec{}, fmt.Errorf("source needs an argument (e.g. `pr 1234`, `branch feat/x`); got %q", input)
	}
	kind := strings.ToLower(parts[0])
	rest := parts[1:]

	switch kind {
	case "pr":
		n, err := strconv.Atoi(rest[0])
		if err != nil || n <= 0 {
			return SourceSpec{}, fmt.Errorf("--pr expects a positive integer; got %q", rest[0])
		}
		return SourceSpec{PR: n}, nil

	case "issue":
		n, err := strconv.Atoi(rest[0])
		if err != nil || n <= 0 {
			return SourceSpec{}, fmt.Errorf("--issue expects a positive integer; got %q", rest[0])
		}
		return SourceSpec{Issue: n}, nil

	case "branch":
		spec := SourceSpec{Branch: rest[0]}
		// "branch feat/x local" → also flip AllowLocal. Modifier word
		// goes after the branch name; tolerant to "--allow-local"
		// too in case users type it as they would on the CLI.
		for _, mod := range rest[1:] {
			switch strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(mod, "--"), "-")) {
			case "local", "allow-local", "allowlocal":
				spec.AllowLocal = true
			default:
				return SourceSpec{}, fmt.Errorf("unknown branch modifier %q (try `local`)", mod)
			}
		}
		return spec, nil

	default:
		return SourceSpec{}, fmt.Errorf("unknown source kind %q (expected pr / issue / branch)", kind)
	}
}

// ResolveSource translates a SourceSpec into a CreateOptions plus the
// suggested workspace name. Heavy lifting per source-variant lives
// here:
//
//   - PR: gh pr view + branch routing (same-repo / cross-repo)
//   - Issue: gh issue view; fresh branch off origin/<default>
//   - Branch: validate exists locally or on origin (--allow-local)
//   - zero spec: returns zero-value options + the user-supplied name
//     (or "" so the caller falls back to namegen)
//
// The returned name is the SUGGESTED workspace name. Callers that
// have an explicit user-supplied name should prefer that and pass
// it through to Create directly; the name returned here is the
// derived default for when the user didn't choose one ("pr-1234",
// "issue-42", branch basename for --branch).
//
// All gh shellouts and git operations happen at call time; this is
// not a pure function. Errors include enough context that the caller
// can surface them to the user as-is.
func (m *Manager) ResolveSource(ctx context.Context, spec SourceSpec) (CreateOptions, string, error) {
	if err := spec.Validate(); err != nil {
		return CreateOptions{}, "", err
	}
	switch {
	case spec.PR > 0:
		return m.resolvePR(ctx, spec.PR)
	case spec.Issue > 0:
		return m.resolveIssue(ctx, spec.Issue)
	case spec.Branch != "":
		return m.resolveBranch(ctx, spec.Branch, spec.AllowLocal)
	}
	return CreateOptions{}, "", nil
}

func (m *Manager) resolvePR(ctx context.Context, num int) (CreateOptions, string, error) {
	pr, err := ghx.FetchPR(ctx, m.Cfg.ProjectRoot, num)
	if err != nil {
		if errors.Is(err, ghx.ErrUnavailable) {
			return CreateOptions{}, "", fmt.Errorf(
				"--pr requires the gh CLI: install via https://cli.github.com/ and run `gh auth login`")
		}
		return CreateOptions{}, "", fmt.Errorf("--pr %d: %w", num, err)
	}

	name := fmt.Sprintf("pr-%d", pr.Number)

	if pr.IsCrossRepository {
		// Cross-repo: fetch refs/pull/<n>/head into a local ref, then
		// check out that local branch.
		ref := fmt.Sprintf("canopy/pr-%d", pr.Number)
		spec := fmt.Sprintf("refs/pull/%d/head:%s", pr.Number, ref)
		if err := git.FetchRefspec(ctx, m.Cfg.ProjectRoot, "origin", spec); err != nil {
			return CreateOptions{}, "", fmt.Errorf("fetch PR #%d head: %w", pr.Number, err)
		}
		return CreateOptions{
			SourceKind:    "pr",
			SourceContext: formatPRContext(pr),
			Branch:        ref,
			StartPoint:    ref,
			CreateBranch:  false,
		}, name, nil
	}

	// Same-repo. Refresh origin/<head> if missing, then choose
	// existing-local vs new-tracking-branch.
	branch := pr.HeadRefName
	originRef := "origin/" + branch
	if !git.RefExists(ctx, m.Cfg.ProjectRoot, originRef) {
		_ = git.Fetch(ctx, m.Cfg.ProjectRoot, "origin")
	}
	if git.RefExists(ctx, m.Cfg.ProjectRoot, branch) {
		return CreateOptions{
			SourceKind:    "pr",
			SourceContext: formatPRContext(pr),
			Branch:        branch,
			StartPoint:    branch,
			CreateBranch:  false,
		}, name, nil
	}
	return CreateOptions{
		SourceKind:    "pr",
		SourceContext: formatPRContext(pr),
		Branch:        branch,
		StartPoint:    originRef,
		CreateBranch:  true,
	}, name, nil
}

func (m *Manager) resolveIssue(ctx context.Context, num int) (CreateOptions, string, error) {
	iss, err := ghx.FetchIssue(ctx, m.Cfg.ProjectRoot, num)
	if err != nil {
		if errors.Is(err, ghx.ErrUnavailable) {
			return CreateOptions{}, "", fmt.Errorf(
				"--issue requires the gh CLI: install via https://cli.github.com/ and run `gh auth login`")
		}
		return CreateOptions{}, "", fmt.Errorf("--issue %d: %w", num, err)
	}
	name := fmt.Sprintf("issue-%d", iss.Number)
	return CreateOptions{
		SourceKind:    "issue",
		SourceContext: formatIssueContext(iss),
	}, name, nil
}

func (m *Manager) resolveBranch(ctx context.Context, branch string, allowLocal bool) (CreateOptions, string, error) {
	_ = git.Fetch(ctx, m.Cfg.ProjectRoot, "origin")

	originRef := "origin/" + branch
	localExists := git.RefExists(ctx, m.Cfg.ProjectRoot, branch)
	originExists := git.RefExists(ctx, m.Cfg.ProjectRoot, originRef)

	switch {
	case localExists:
		return CreateOptions{
			SourceKind:   "branch",
			Branch:       branch,
			StartPoint:   branch,
			CreateBranch: false,
		}, defaultBranchName(branch), nil

	case originExists:
		return CreateOptions{
			SourceKind:   "branch",
			Branch:       branch,
			StartPoint:   originRef,
			CreateBranch: true,
		}, defaultBranchName(branch), nil

	case allowLocal:
		return CreateOptions{}, "", fmt.Errorf(
			"branch %q not found locally (--allow-local was set). Run `git branch %s` first, "+
				"or drop --allow-local to require an origin/<branch>.", branch, branch)

	default:
		return CreateOptions{}, "", fmt.Errorf(
			"branch %q not found on origin/. Run `git fetch origin` then retry, "+
				"or pass --allow-local if you have a local-only branch.", branch)
	}
}

// formatPRContext renders the PR metadata into the briefing's
// "Source context" body (wrapped by sourceKindBlock with the
// data-not-instructions delimiter).
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

// defaultBranchName returns the branch's basename — the part after
// the last "/". "feature/oauth" → "oauth"; "main" → "main"; "" → "".
func defaultBranchName(branch string) string {
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		return branch[i+1:]
	}
	return branch
}
