# Canopy

Go + Bubbletea TUI for managing git worktrees with paired tmux sessions and per-project setup hooks.

Design doc: `docs/design/v0-canopy.md` (canonical, committed)
Test plan: `docs/reviews/v0-test-plan.md` (canonical, committed)
gstack mirrors: `~/.gstack/projects/canopy/` (regenerated; not load-bearing)

## Project context

- First-time Go project. Lean on stdlib, prefer plain patterns over clever abstractions, gloss idioms in PR descriptions or code comments when they're non-obvious.
- v0 supports any project that has a `canopy.json` and is the cwd-walk-up project. Cross-project switching shipped in v0.11.0 — the Global tab in the TUI lists workspaces across every project and `n` works from any row.
- "Workspace" is the user-facing word for canopy's unit. "Worktree" is reserved for the git concept and lives only in `internal/git/`.
- Workspaces live on disk at `~/.canopy/workspaces/<project>/<name>` (the dir is a git worktree, but it's stored centrally so the source repo stays clean).
- State + logs in `~/.canopy/`. Project config in `<repo>/canopy.json`.

## canopy.json schema

```json
{
  "scripts": {
    "setup": "bin/canopy-setup",
    "run": "bin/dev",
    "archive": "bin/canopy-archive"
  }
}
```

Three string keys, three script paths. Scripts execute via `exec.CommandContext` (no `sh -c`) with cwd set to the workspace dir and these env vars:
- `CANOPY_WORKSPACE_PATH` — workspace dir (= worktree dir)
- `CANOPY_ROOT_PATH` — original repo root
- `CANOPY_PORT` — allocated port (3000-3999)

Script semantics:
- `setup` runs once on workspace creation. Failure → `status: broken`.
- `run` is the long-running server command, launched in the server pane and on resurrection.
- `archive` runs on workspace removal (DB drop, server kill).

## Package layout

```
cmd/canopy/main.go      Entry point; routes between TUI and subcommands.
internal/config/        canopy.json walk-up discovery + load. Read-only at startup.
internal/git/           Worktree add/remove/sanitize via os/exec wrapping git.
internal/tmux/          Session create/attach/kill/has via os/exec wrapping tmux.
internal/hooks/         Script execution via exec.CommandContext + CANOPY_* env.
internal/state/         JSON registry of workspaces. Single mutable. flock-protected.
internal/workspace/     Orchestration: Create / Remove / Resurrect / Reconcile.
internal/clog/          slog setup (named `clog` to avoid stdlib `log` collision).
internal/ui/            Bubbletea Model/Update/View + lipgloss.
```

Dependency direction is leaf-up: `cmd` and `ui` depend on `workspace`; `workspace` depends on `git`/`tmux`/`hooks`/`state`/`config`; everyone uses `log`. Never the other way.

## Error-handling convention

1. Wrap with `%w`: `return fmt.Errorf("canopy new: %w", err)`. Never lose the chain.
2. Sentinels for known cases: `var ErrBranchExists = errors.New("branch already exists")`. Tests use `errors.Is(err, ErrBranchExists)`.
3. Last return value is `error`. `(value, error)`. No exceptions to this rule.
4. No `panic` outside `main()`. Validation belongs in returned errors.
5. Errors are logged at the boundary that handles them, not at every level.

## Logging

- stdlib `log/slog`, JSON output, append to `~/.canopy/log/canopy.log`.
- INFO default, DEBUG with `--debug`.
- Every package opens with `var log = clog.Pkg("<pkgname>")` then uses `log.Info("...", "key", val)` with structured fields.
- Import: `import "github.com/oncactus/canopy/internal/clog"`.

## Workspace state machine

Five statuses: `setting_up`, `ready`, `stopped`, `broken`, `orphaned`. See design doc for transitions. The big UX win is `stopped` (tmux session died but workspace alive on disk) → `canopy switch` resurrects the session by re-running `scripts.run` in the server pane and rebuilding the other panes — without re-running `scripts.setup`. Claude conversation history is preserved automatically because Claude stores per-directory.

## Idempotency

`canopy new <branch>` always fast-fails on existing state with an explicit error and a suggested next command. Never destructive. See design doc's idempotency table.

## Testing

- stdlib `testing` only. No testify, no ginkgo. Table-driven where it fits.
- E2E tests gated behind `-tags=e2e` build tag. Use `t.TempDir()` and `tmux -L canopy-test` socket so they don't pollute the user's tmux server.
- The three CRITICAL tests (`state.WithLock`, `port.Allocate`, `worktree.Create` E2E) ship with v0. The other ~50 paths grow with the code as bugs surface.

### Test discipline (always-on)

Every code change ships with tests. No exceptions.

- **New behavior** → write a unit test that exercises it. Cover both branches of every conditional (`if` true AND false, env var set AND unset, success AND error path). Do NOT hand-wave with "the UI layer is intentionally test-light" — write the test.
- **Bug fix** → write a regression test FIRST that reproduces the bug, then fix.
- **Cosmetic-only changes** (color tweaks, spacing, copy edits) → still ship a test if there's a conditional or branch involved. Pure literal swaps in style values are the only exception.
- **TUI changes are testable.** Build a `*Model` literal directly for keymap/render tests; use `tmux.WithSocket("canopy-test")` to avoid touching the user's tmux server. See `internal/ui/model_test.go` for the pattern.
- **Coverage gate before shipping:** every new conditional branch in the diff must have a test. If you're about to commit code with new branches and no tests, stop and write them. The first place this rule slips is "I'll add tests later" — there is no later.

If a change genuinely cannot be tested (e.g., a typo in a log line), say so explicitly in the PR body and explain why. Don't quietly skip.

## Skill routing

When the user's request matches an available skill, invoke it via the Skill tool. The
skill has multi-step workflows, checklists, and quality gates that produce better
results than an ad-hoc answer. When in doubt, invoke the skill. A false positive is
cheaper than a false negative.

Key routing rules:
- Product ideas, "is this worth building", brainstorming → invoke /office-hours
- Strategy, scope, "think bigger", "what should we build" → invoke /plan-ceo-review
- Architecture, "does this design make sense" → invoke /plan-eng-review
- Design system, brand, "how should this look" → invoke /design-consultation
- Design review of a plan → invoke /plan-design-review
- Developer experience of a plan → invoke /plan-devex-review
- "Review everything", full review pipeline → invoke /autoplan
- Bugs, errors, "why is this broken", "wtf", "this doesn't work" → invoke /investigate
- Test the site, find bugs, "does this work" → invoke /qa (or /qa-only for report only)
- Code review, check the diff, "look at my changes" → invoke /review
- Visual polish, design audit, "this looks off" → invoke /design-review
- Developer experience audit, try onboarding → invoke /devex-review
- Ship, deploy, create a PR, "send it" → invoke /ship
- Merge + deploy + verify → invoke /land-and-deploy
- Configure deployment → invoke /setup-deploy
- Post-deploy monitoring → invoke /canary
- Update docs after shipping → invoke /document-release
- Weekly retro, "how'd we do" → invoke /retro
- Second opinion, codex review → invoke /codex
- Safety mode, careful mode, lock it down → invoke /careful or /guard
- Restrict edits to a directory → invoke /freeze or /unfreeze
- Upgrade gstack → invoke /gstack-upgrade
- Save progress, "save my work" → invoke /context-save
- Resume, restore, "where was I" → invoke /context-restore
- Security audit, OWASP, "is this secure" → invoke /cso
- Make a PDF, document, publication → invoke /make-pdf
- Launch real browser for QA → invoke /open-gstack-browser
- Import cookies for authenticated testing → invoke /setup-browser-cookies
- Performance regression, page speed, benchmarks → invoke /benchmark
- Review what gstack has learned → invoke /learn
- Tune question sensitivity → invoke /plan-tune
- Code quality dashboard → invoke /health

## Testing

`go test ./...` runs unit tests fast (<5s).
`go test -tags=e2e ./...` runs the full pair including the E2E happy-path and resurrection tests.
Test framework: stdlib `testing`.
