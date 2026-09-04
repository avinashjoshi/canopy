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
internal/config/        Two parts: (1) canopy.json walk-up discovery + load
                        (project config, read-only at startup); (2) UserStore
                        for ~/.canopy/config.json — flock-protected user prefs
                        (source-root and friends), schema v1 (v0.20+).
internal/canopyinit/    Pure-input helpers shared between CLI and TUI for the
                        Add Project flow: URL detect, basename derive, clone-dest
                        resolution, lazy mkdir, workspace path safety. Lives in
                        internal/ so internal/ui can import without violating
                        the leaf-up rule (v0.20+).
internal/git/           Worktree add/remove/sanitize via os/exec wrapping git.
                        Plus Clone() for `canopy init <url>` (v0.20+).
internal/tmux/          Session create/attach/kill/has via os/exec wrapping tmux.
                        Role-addressing layer (roles.go) tags panes with
                        @canopy-role and looks them up by role name; replaces
                        positional pane indexing across the workspace lifecycle.
internal/host/          Multi-host remote dispatch (v0.17+): hosts.json registry,
                        per-host refresh fan-out with bounded concurrency, and the
                        ssh/mosh command builders remote verbs (`--on`/`--remote`)
                        shell out through. Every target passed to ssh/mosh gets an
                        explicit `--` separator before it (v0.22) so a target or
                        hosts.json entry shaped like a flag can't be parsed as one.
                        SSHAttachLoop (v0.23) is the ssh-reconnect-loop attach
                        primitive behind `--no-mosh` and the automatic fallback
                        when mosh isn't installed locally.
internal/clipboard/     Remote clipboard bridge, text only, both directions
                        (v0.18+; rewritten in v0.24.x — see docs/clipboard-bridge.md).
                        Per-host installer pushes wl-paste/wl-copy wrapper scripts
                        to the remote; the wrappers talk OSC 52 terminal escape
                        sequences directly to the attached terminal, so there's no
                        laptop-side daemon or persistent SSH tunnel. Installer SSH
                        calls run in batch mode (v0.23) so a host without cached
                        key auth fails fast instead of hanging. SanitizeArtifactName
                        (v0.23) makes a raw `--remote` SSH target safe to use as an
                        artifact name, both for unattended auto-install and for
                        finding pre-OSC52 artifacts left on the laptop to clean up.
internal/agent/         Agent launcher metadata (claude / codex / aider) and
                        canonical role strings (RoleForType → `agent:<launcher>`).
internal/hooks/         Script execution via exec.CommandContext + CANOPY_* env.
internal/state/         JSON registry of workspaces. Single mutable. flock-protected.
internal/lifecycle/     Workspace-health hint detectors (rename_suggested, etc.).
                        Pure functions over a workspace path; consumed by the
                        statusline refresh tick. Untracked-file noise is
                        explicitly excluded from "intent gathered" signals.
internal/workspace/     Orchestration: Create / Remove / Resurrect / Reconcile.
internal/clog/          slog setup (named `clog` to avoid stdlib `log` collision).
internal/ui/            Bubbletea Model/Update/View + lipgloss.
```

Dependency direction is leaf-up: `cmd` and `ui` depend on `workspace`; `workspace` depends on `git`/`tmux`/`hooks`/`state`/`config`; `cmd` and `ui` also depend directly on `host` for remote-dispatch verbs; everyone uses `log`. Never the other way.

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
- Import: `import "github.com/avinashjoshi/canopy/internal/clog"`.

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

## Multi-workspace dogfooding

Canopy is its own dogfood: developers run multiple parallel worktrees (one per
in-flight feature), each with its own `./canopy` build. Three rules keep these
from interfering with each other.

1. **NEVER run bare `canopy` for testing inside a worktree.** The bare command
   resolves to whatever `~/.local/bin/canopy` (a symlink) currently points at,
   which may be a different workspace's binary OR the released `canopy.bin`.
   Testing through it gives signal about somebody else's code, not yours.

2. **Always invoke your workspace's local build:**
   - `./canopy <cmd>` from the worktree root, or
   - `/tmp/canopy-XX <cmd>` (XX = initials of your workspace name; e.g.
     `/tmp/canopy-iw` for `install-and-dev-workflow`).

   Both of these are stable per-workspace paths that don't collide across
   parallel agents. The auto-build hook keeps them fresh after canopy
   feature changes.

3. **The global `canopy` (the `~/.local/bin/canopy` symlink) belongs to the
   user's interactive flow.** Treat it as user state, not Claude state. If
   the user asks "make my workspace the active canopy," run:
   - `canopy use <workspace-name>` from anywhere, or
   - `make dev` from inside that worktree.

   Either flips the global symlink at this workspace's `./canopy`. The user
   can flip back to released with `canopy use release` (or `make release`).
   Both surfaces show the active state in the TUI status bar (gray release
   pill vs cyan DEV pill) and in the tmux statusline (`[DEV:<workspace>]`
   suffix). If those indicators look wrong after a switch, treat it as a
   bug and surface it.

## Testing

`go test ./...` runs unit tests fast (<5s).
`go test -tags=e2e ./...` runs the full pair including the E2E happy-path and resurrection tests.
Test framework: stdlib `testing`.
