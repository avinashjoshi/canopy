# Contributing to Canopy

Thanks for the interest. Canopy is small enough that any contribution is appreciated, and it's intentionally opinionated, so the best contributions usually start with a brief discussion before code.

## Before you start

- For **bugs**: open an issue with reproduction steps. The simpler the repro, the faster the fix.
- For **features or behavior changes**: open an issue first describing the use case. Canopy follows a "calm and focused beats feature-dense" philosophy ([`docs/landscape.md`](docs/landscape.md)) — if a feature would add a second way to do an existing thing, expect a higher bar.
- For **typos / docs polish**: skip the issue, just open the PR.

## Development setup

```bash
git clone https://github.com/avinashjoshi/canopy.git
cd canopy
go test ./...                   # fast unit tests (<5s)
go test -tags=e2e ./...         # full E2E (real tmux, scratch repos, slow)
make build                      # build ./canopy in repo root
make install                    # build + copy to ~/.local/bin/canopy
```

Requirements: Go 1.22+, `git >= 2.30`, `tmux >= 3.0`. The E2E suite uses `tmux -L canopy-test` so it doesn't touch your ambient tmux server.

## Code conventions

- **stdlib-first.** Canopy avoids non-essential dependencies. Bubbletea + lipgloss + cobra + lumberjack are in; testify, ginkgo, and friends are out. New deps need a real reason.
- **Error wrapping with `%w`.** Every error path wraps the chain; tests use `errors.Is` against package sentinels. See `internal/workspace/lifecycle.go` for the pattern.
- **`var log = clog.Pkg("name")`** at the top of each package, then structured `log.Info("event.name", "key", val)`. Don't import the stdlib `log` — `clog` is the per-package shorthand.
- **Comments explain the WHY, not the what.** Well-named identifiers carry the what. Comments earn their keep by capturing constraints, invariants, and "this is non-obvious because…" context.
- **`gofmt -s` on every file.** CI runs `go vet`; please run it locally too.
- **No panics outside `main()`.** Validation belongs in returned errors.

## Architecture quick map

```
cmd/canopy/         Cobra root + subcommands
internal/clog/      slog setup (named clog to avoid stdlib log collision)
internal/config/    canopy.json walk-up discovery + load
internal/git/       worktree wrappers around os/exec git
internal/tmux/      session wrappers around os/exec tmux
internal/host/      multi-host remote dispatch: hosts.json registry, ssh/mosh
                    command builders (--on / --remote)
internal/hooks/     script execution with CANOPY_* env
internal/state/     JSON registry + flock-protected mutations
internal/workspace/ orchestration: Create / Remove / Resurrect / Reconcile
internal/ui/        Bubbletea Model/Update/View
```

Dependency direction is leaf-up: `cmd` and `ui` depend on `workspace`; `workspace` depends on the leaves. Never the other way. See [`docs/architecture.md`](docs/architecture.md) for the full map.

## Submitting a PR

1. Fork, branch from `main`, do your thing.
2. Make sure `go test ./...` and `go test -tags=e2e ./...` both pass.
3. Update [`CHANGELOG.md`](CHANGELOG.md) under `## [Unreleased]` if your change is user-visible.
4. Open the PR with a description that mentions the issue (`Closes #123`) or, if there's no issue, the use case.

One feature per PR. Bundle small refactors with the feature they enable; ship orthogonal cleanups separately so they're easy to review and revert.

## Project context

- [`docs/design/v0-canopy.md`](docs/design/v0-canopy.md) — design doc, premises, state machine.
- [`docs/reviews/v0-test-plan.md`](docs/reviews/v0-test-plan.md) — what's tested, what's deliberately not.
- [`TODOS.md`](TODOS.md) — deferred work organized by milestone.

If you've got an idea that'd take more than a small PR, file an issue and let's talk shape first.
