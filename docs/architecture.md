# Architecture

Map of the codebase for contributors and future-you. The design rationale lives in `design/v0-canopy.md`; this is the "how do I find things" companion.

## Package layout

```
cmd/canopy/                    Cobra root + every subcommand. One file per cmd.
  main.go                      Cobra root, --debug flag, version subcommand
  init.go                      canopy init (with --with-scripts, --force)
  new.go                       canopy new (with --name, --no-attach)
  ls.go                        canopy ls + canopy ls --all
  switch.go                    canopy switch (lazy reconcile + dispatch)
  rm.go                        canopy rm (confirm prompt, -y to skip)
  reconcile.go                 canopy reconcile
  main_session.go              canopy main (tmux at project root)
  upgrade.go                   canopy upgrade (--check, --force, --dismiss)
  upgrade_check.go             ~/.canopy/upgrade-check.json cache + 6h auto-check
  manager.go                   loadManager / loadConfig / getCwd helpers

internal/clog/                 Structured logging via slog + lumberjack rotation.
                               Every package: var log = clog.Pkg("name").
internal/git/                  worktree add/remove/sanitize, fetch, default-branch
                               detection. Pure os/exec wrappers, sentinel errors.
internal/tmux/                 Client struct (with optional named socket for tests).
                               HasSession / Create / SplitPane / SelectLayout /
                               SelectPaneDirection / Attach (syscall.Exec) /
                               KillServer / HasClient / WindowCount. SafeName
                               helper. Create and SplitPane return the new
                               pane ID (captured via -P -F '#{pane_id}') and
                               validate it matches tmux's `%<digits>` format
                               so a user tmux hook polluting stdout fails at
                               the boundary instead of poisoning SetRole.
                               roles.go: SetRole / LookupPane / LookupAllPanes /
                               SelectPane / PaneCount / PanesInOrder /
                               ListAllRoles / PaneCommands — pane addressing
                               by @canopy-role tmux user-option (process-
                               proof, persistent for the pane's lifetime).
                               Replaces positional pane indexing across the
                               workspace lifecycle. Role globs accept exact
                               match or a single trailing `*`; anything else
                               returns ErrInvalidGlob (fail fast instead of
                               silent no-match).
internal/host/                 Multi-host remote dispatch (v0.17+): hosts.json
                               registry, per-host refresh fan-out, and the
                               ssh/mosh command builders --on/--remote shell
                               out through. Every target passed to ssh/mosh
                               gets an explicit "--" separator before it
                               (v0.22) so an option-shaped target/hosts.json
                               entry can't be parsed as a flag. SSHAttachLoop
                               (v0.23) is the ssh-reconnect-loop attach
                               primitive behind --no-mosh and the automatic
                               fallback when mosh isn't installed locally —
                               re-dials with backoff on a transport failure
                               (ssh exit 255) but stops immediately on a
                               permanent failure (rejected key, changed host
                               key, unresolvable hostname).
internal/clipboard/            Local daemon (clip-text/clip-image/clip-copy
                               Unix sockets) + per-host installer bridging
                               the laptop's Wayland clipboard into a remote
                               workspace over SSH RemoteForward (v0.18+). All
                               of the installer's SSH calls run in batch mode
                               (v0.23) so a host without cached key auth fails
                               fast instead of hanging on a password prompt
                               against Bubbletea's raw-mode terminal.
                               SanitizeArtifactName (namesafe.go, v0.23) turns
                               a raw SSH target into a filesystem/systemd-safe
                               artifact name so unregistered `--remote` hosts
                               can auto-install without a `hosts.json` entry.
internal/agent/                Agent launcher metadata (claude / codex / aider).
                               RoleForType produces canonical role strings
                               (`agent:claude`, etc.) consumed by the tmux
                               role-addressing layer.
internal/hooks/                exec.CommandContext-based script runner with
                               process-group kill + WaitDelay so SIGINT cleanly
                               unwinds bundle install style children.
internal/state/                JSON registry at ~/.canopy/state.json with
                               flock-protected mutations. State.{Workspaces,
                               Projects} + Add/Find/Remove + EnsureProjectBase
                               + WithLock.
internal/namegen/              Random adjective-noun names ("bold-falcon").
                               Generate / Unique / All / IsValid.
internal/port/                 Allocate(min, max, stride, used) — pure function
                               with net.Listen probe for externally-held ports.
internal/config/               canopy.json walk-up discovery + load. ErrNotFound,
                               ErrInvalid sentinels. Validation is a no-op today
                               (scripts are optional).
internal/settings/             ~/.canopy/config.json — per-machine settings
                               (today: just port plan). Default() applies when
                               the file is absent.
internal/lifecycle/            Workspace-health hint detectors that feed the
                               TUI's HINTS column (rename_suggested,
                               stuck-state, ahead/behind, etc.). Pure
                               functions over a workspace path, invoked
                               from the statusline refresh tick. Tracked
                               vs untracked file accounting lives in
                               git_stats.go so detectors share one
                               porcelain-parse helper. rename_suggested
                               fires on either commits past origin/<default>
                               OR tracked-file edits — untracked noise
                               (build artifacts, setup byproducts) is
                               excluded so the hint doesn't loop forever.
internal/workspace/            ORCHESTRATION layer. Manager.{Create, Remove,
                               Resurrect, Reconcile, List, Find}. The thing
                               every CLI subcommand calls into. Composes git +
                               tmux + hooks + state + port + namegen.

docs/design/                   Design docs (committed; survives ~/.gstack rotations)
docs/reviews/                  Review artifacts (test plan, etc.)
docs/                          User-facing guides (this file is in here)
```

## Dependency direction

Strictly leaf-up. Lower packages don't import higher ones.

```
                cmd/canopy
                    │
                    ▼
          ┌──── workspace ──────┐
          │                     │
    ┌─────┼─────────────┬───────┼─────┐
    ▼     ▼             ▼       ▼     ▼
  git   tmux         hooks    state  config
                                │      │
                                ▼      ▼
                              clog  settings (ports)
                                ▲
                                │
                            (used by all)
```

`workspace` is the only package that imports the others. The `cmd/canopy` subcommands import `workspace` and a small set of leaves (config for canopy.json discovery, settings for `canopy main`'s port allocation, tmux for `canopy main`'s session creation, state directly for `canopy ls`'s read-only path). They never reach into the lower packages directly.

## Workspace lifecycle (the Manager)

`workspace.Manager.Create` is the canonical example of the layered orchestration:

```
┌─ Phase 1 (state.WithLock):
│   1. namegen.Unique() if name was empty
│   2. EnsureProjectBase to get this project's port base
│   3. port.Allocate(base+stride, base+project_stride-1, workspace_stride, used)
│   4. state.Add(workspace with status=setting_up)
└─

┌─ Phase 2 (slow operations, no lock held):
│   5. mkdir parent
│   6. git.DetectDefaultBranch + git.Fetch (best-effort)
│   7. git.Add(repoRoot, branch, path, "origin/<default>")
│   8. hooks.Run(scripts.setup, env=CANOPY_*) -- if non-empty
│   9. tmux.Create + 2 SplitPane (each returns paneID) + tmux.SetRole on each
│      pane (`ide`, `terminal:shell`, `agent:<launcher>`) — no SelectLayout,
│      proportions are baked in
└─

┌─ Phase 3 (state.WithLock):
│  10. flip status to ready
└─
```

Splitting into "fast registration" + "slow setup" + "fast finalization" means multiple `canopy new` invocations on different workspaces don't block each other for the duration of `bundle install`. They serialize only on the lock-protected windows (read state, allocate port, persist) which take milliseconds.

Failure in Phase 2 calls `markBroken` (a single state.WithLock call that flips status to `broken` + records `last_error`). The state row stays so the user can `canopy rm` to clean up.

## State machine

Five workspace statuses. Reconcile transitions between them based on disk + tmux truth.

```
                      canopy new
                           │
                           ▼
                    ┌─────────────┐  scripts.setup     ┌────────────┐
                    │ setting_up  │───────fails───────▶│   broken   │
                    └─────────────┘                    └────────────┘
                           │ scripts.setup ok                │
                           ▼                                 │
                    ┌─────────────┐  tmux dies        ┌────────────┐
        canopy main │    ready    │──────────────────▶│  stopped   │
                    └─────────────┘                   └────────────┘
                           │                                 │
                           │ workspace dir gone              │ canopy switch
                           ▼                                 ▼
                    ┌─────────────┐                  (Resurrect rebuilds
                    │  orphaned   │                   tmux WITHOUT re-running
                    └─────────────┘                   scripts.setup; claude
                           │                          uses --continue || claude)
                           │ user confirms canopy rm
                           ▼
                       (removed)
```

`Reconcile` walks state.json, observes each row's disk + tmux state, and updates the status field. Never deletes rows — orphaned workspaces (dir gone from disk) stay in state.json until the user runs `canopy rm` explicitly.

`canopy switch` runs Reconcile lazily before dispatching, so a stale `ready` status doesn't trip the user up.

## Locking

Single point: `state.Store.WithLock`. Holds an advisory `flock(2)` on `~/.canopy/state.json.lock` for the read-modify-save window. Two `canopy new` invocations in parallel terminals serialize cleanly: the second waits, then sees the first's writes when the lock releases.

Atomic write: `state.Store.Save` writes to `state.json.tmp` first and `rename(2)`s into place. POSIX rename is atomic within a single filesystem, so readers always see a complete document.

## Port plan

Each project gets a base port (default 40000, 41000, 42000, ... — first-come-first-served, persisted in `state.Projects`). Within a project:

```
project_base + 0   = canopy main  (reserved, allocated by canopy main only)
project_base + 10  = canopy new ws#1
project_base + 20  = canopy new ws#2
...
```

`port.Allocate` is pure — it takes (min, max, stride, used) and returns the smallest free slot in the strided range. Concurrent uniqueness comes from `state.WithLock` wrapping the whole "scan + pick + persist" sequence.

## Where to add things

| Adding... | Goes in... |
|---|---|
| A new CLI subcommand | `cmd/canopy/<name>.go`, register in `main.go`'s `root.AddCommand` |
| A new git operation | `internal/git/worktree.go` |
| A new tmux operation | `internal/tmux/session.go` |
| A new pane role or role-lookup helper | `internal/tmux/roles.go` |
| A new agent launcher | `internal/agent/launchers.go` (add to the registry; `RoleForType` picks up the new type automatically) |
| A new remote-dispatch ssh/mosh call site | `internal/host/ssh.go` — always insert `"--"` before the target arg (option-injection fix, v0.22); use `SSHAttachLoop` for a suspend-tolerant fallback path (v0.23) |
| A new clipboard-bridge SSH call | `internal/clipboard/host_install.go` — must run in batch mode (v0.23 fix) so a host without cached key auth fails fast instead of hanging |
| A new env var canopy passes to scripts | `internal/hooks/runner.go`'s `WorkspaceEnv` |
| A new field on canopy.json | `internal/config/config.go`'s `Config`/`Scripts` struct |
| A new workspace state field | `internal/state/state.go`'s `Workspace` struct (and bump `SchemaVersion` if breaking) |
| A new lifecycle method | `internal/workspace/lifecycle.go` |
| A new per-machine setting | `internal/settings/settings.go`'s `Settings` struct + `validate` |

## Tests

- `internal/*/foo_test.go` — package-local. Use `package foo_test` (external) for boundary tests, `package foo` for white-box.
- `internal/workspace/lifecycle_test.go` — the only E2E suite. Spins up real git + real tmux on a scoped socket. ~250ms per test.
- The 3 critical concurrency tests live in `internal/state/state_test.go` (TestWithLock_ParallelWriters) and `internal/port/port_test.go` (TestAllocate_ConcurrentDistinctPorts, TestAllocate_Exhaustion).
- `make test` runs all of them. `-race` works (no known data races).

## See also

- `docs/design/v0-canopy.md` — the design doc with full rationale
- `docs/reviews/v0-test-plan.md` — coverage plan
- `TODOS.md` — deferred work, organized by milestone
