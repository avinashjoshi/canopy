# canopy (Rust) — working notes for agents

Read `docs/design/v1-architecture.md` first. This file is the operational companion.

## Layout

```
crates/canopy-core     domain + persistence. No sockets, no tmux, no TUI. Pure functions + files.
crates/canopy-proto    API wire types (newline-JSON), events, schema. Shared by server and client.
crates/canopy-server   daemon: api (socket), app (state + runtime), manager (lifecycle), backend
                       (SessionBackend trait + tmux impl), pollers, agent (state tracker), hints,
                       launcher (agent table + briefing), scripts (runner), git, integration (hooks).
crates/canopy-client   blocking API client, attach exec, ratatui TUI.
crates/canopy          the binary: clap routing only.
```

Dependency direction is strictly leaf-up: `core <- proto <- server/client <- bin`. The client may
depend on the server crate only for `daemon::ensure_running` and read-only helpers; never the reverse.

## Rules

- Canonical state only in `state.json`. Hints, agent state, liveness, PR status are runtime-derived
  and never persisted.
- Every mutation goes through `manager` and publishes an event. The TUI holds no state and runs
  no pollers; it renders snapshots and refreshes on events.
- Backend-specific code lives under `server/backend/`. Nothing above the trait may call `tmux`.
- Every new conditional branch gets a test, both directions. Bug fix => regression test first.
- Errors: `thiserror` enums in libraries, `anyhow` at the binary edge. No `panic!` outside tests.
- Idempotency is non-negotiable: `new` fast-fails on existing state; `retry` re-runs setup in
  place; `install`/`integration install` are re-runnable.
- Taste rule inherited from v0: "would this make canopy feel like Orca?" If yes, push back.

## Dogfooding without touching your real setup

canopy develops inside canopy: this repo is a canopy project, and every workspace gets its own
sandbox. Never run `cargo run -p canopy` bare in a workspace; it would talk to the live server.

```
bin/dev doctor                   # this checkout's debug build, private CANOPY_HOME/socket/tmux server
bin/dev new --no-attach          # sessions land on tmux server canopy-dev-<workspace>
bin/dev attach <project>/<ws>    # look at them
canopy run                       # tab with the sandboxed server in the foreground, debug logs
```

`bin/dev` rebuilds when sources are newer than `target/debug/canopy`. The sandbox is
`<checkout>/.sandbox` (git-ignored); `canopy rm` tears it down via `bin/canopy-archive`. The raw
knobs are `CANOPY_HOME`, `CANOPY_SOCKET_PATH`, `CANOPY_TMUX_SOCKET` (scopes to `tmux -L <name>`).

## Testing

```
cargo test --workspace            # unit + a real-tmux test on a scoped socket (skips if tmux missing)
cargo clippy --workspace --all-targets
```

The real-tmux and real-git tests live in `server/backend/tmux.rs` and `server/git.rs`.

## Releasing

Tag `vX.Y.Z` after bumping `[workspace.package].version`; `.github/workflows/release.yml`
publishes the binaries that `install.sh` and `canopy upgrade` download. Asset names are shared
between `install.sh` and `crates/canopy/src/upgrade.rs`. Details: `docs/releasing.md`.
