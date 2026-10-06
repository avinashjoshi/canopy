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

```
export CANOPY_HOME=/tmp/canopy-dev CANOPY_SOCKET_PATH=/tmp/canopy-dev/canopy.sock CANOPY_TMUX_SOCKET=canopy-dev
cargo run -p canopy -- version
```

`CANOPY_TMUX_SOCKET` scopes the server to `tmux -L <name>`, so sessions never land on your real
tmux server. `canopy server stop` + `tmux -L canopy-dev kill-server` cleans up.

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
