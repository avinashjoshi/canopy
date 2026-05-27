# Changelog

All notable changes to canopy are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and canopy adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.21.3.0] - 2026-05-26 — Idempotent `canopy rm --force` on stale rows

Force-deleting a remote workspace row whose underlying workspace had already vanished on the remote no longer dumps a scary `remote canopy rm failed: exit status 1` into the user's scrollback. The remote canopy now exits cleanly with `Workspace "<name>" not found — already removed.`, mirroring Unix `rm -f` semantics, and the local TUI's post-dispatch refresh drops the stale row as it always did.

The trap was a TUI flow: the local canopy refreshes remote rows on its own cadence by SSH'ing `canopy ls --json` against each registered host. If the user (or any other tool) removed a workspace on the remote between two refresh ticks, the local Global tab still showed the row. Pressing `d` + `F` on that row dispatched `canopy rm <name> --yes --force` over SSH; the remote's `mgr.Find` returned `ErrWorkspaceNotFound`; the SSH dispatch propagated `exit status 1`; the user saw the failure even though the next refresh tick would have dropped the row anyway. The user's stated intent ("make this row go away") was already satisfied — the error message just made it look broken.

### Fixed

- **`canopy rm <name> --force` is now idempotent for missing workspaces.** New `rmHandleFindErr` helper in `cmd/canopy/rm.go` swallows `workspace.ErrWorkspaceNotFound` when `--force` is set, prints `Workspace %q not found — already removed.`, and returns success. Strict mode (no `--force`) still errors out so CLI typos like `canopy rm fixx` still surface. Only `ErrWorkspaceNotFound` is swallowed — other Find failures (state.json I/O errors, etc.) still bubble up so the user sees the real cause. Unit tests cover all three branches of the helper.

## [0.21.2.0] - 2026-05-26 — Self-heal remote add-project registration

The "I added a project on tower from the TUI but pressing Enter on its `(main)` row errors with `project not registered for that host`" trap is gone. Adding a project now tells you up front when the laptop couldn't auto-register it, and every refresh tick self-heals an unregistered remote project once the remote canopy is running this version.

The trap had two compounding pieces. First, when the laptop dispatched `canopy init` over SSH it relied on a `CANOPY_INIT_RESULT_FILE` round-trip to learn the canonical project path on the remote and write it into `hosts.json` — but if the remote canopy was pre-v0.20, the env var did nothing, the result file came back empty, the warning went only to `~/.canopy/log/canopy.log`, and the user saw a green "Added" toast. Meanwhile `canopy ls --json` on the remote happily reported the new project's row, so the row showed up in the Global tab and pressing Enter on its `(main)` blew up — with a recovery hint that pointed at a flag (`--host`) the CLI never accepted.

### Added

- **`project_root` field in `canopy ls --json` wire output.** New per-row string carrying the canonical absolute path of the row's project on the host emitting the JSON. Additive — pre-v0.21.2 laptops parse and ignore. Lets the laptop's refresher discover `(host, project) → path` pairs without an extra SSH round-trip per refresh.
- **`autoRegisterRemoteOrphans` self-healing pass in `refreshRemoteCmd`.** Every refresh tick walks the per-host `canopy ls --json` results, picks up any `(host, project)` pair where the remote sent a `project_root` but the laptop's `hosts.json` doesn't have the registration, validates the path against the same safety contract the v0.20 result-file channel uses (absolute, UTF-8, no control characters, ≤1 KiB), and writes it through `reg.AddProject`. Dedupes across multiple workspaces of the same project; skips errored hosts; no-ops when the remote is too old to send the field. Idempotent.
- **Surfaced auto-register warnings in the Add Project toast.** `registerRemoteAddProject` now returns the failure reason instead of swallowing it into a log line. When set, the success toast carries a `⚠ <reason>` follow-up line with the literal manual-recovery command (`canopy project add <name> <path> --on <host>`), and the toast window stretches from 3 s to 8 s so the user can copy it.

### Fixed

- **Wrong CLI flag in the "project not registered" attach error.** The hint at `update_attach.go:211` said `canopy project add --host tower <path>`, but `canopy project add` has only ever accepted `--on`. Copy-pasting the hint produced `unknown flag --host`. Now emits `canopy project add <project> <remote-path> --on <host>` with the project name in the right slot.

## [0.21.1.0] - 2026-05-16 — Hosts-tab first-load spinner + clearer upgrade errors

Two remote-host papercuts the user hit on first dogfood after v0.21:

The Hosts tab no longer renders every host as a neutral `· (never refreshed)` for the duration of the SSH fan-out at startup. Hosts without a cached snapshot now animate a Braille spinner (`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`, 120 ms cadence) until their per-host refresh result lands. The spinner stops itself once the fan-out settles — no leaked tick loop.

`canopy upgrade` over SSH now produces a useful error when the source clone or install target isn't writable by the current user (the classic "previous install ran as root via sudo" trap). Instead of the misleading `there are local commits in the source clone` hint, the error names the actual cause and tells the user to `sudo chown -R $(whoami) …` the right directory. Same treatment for the in-TUI `U` flow. `make install` now pre-flights `$(BIN_DIR)` writability + `$(BIN_REAL)` ownership before `go build` would write a partial artifact, so the failure surfaces with actionable text immediately rather than mid-build.

### Added

- **Per-host loading spinner on the Hosts tab.** New `StatusLoading` enum + Braille frame animation while a remote refresh fan-out is in flight. Hosts with a cached snapshot keep their previous status; only never-refreshed hosts spin. A `hostsSpinnerActive` latch prevents stacked tick loops if `r` is pressed mid-refresh.
- **Permission-denied detection in the upgrade error path.** New `isPermissionDeniedStderr` sniff with explicit denies for SSH/network forms (`(publickey)`, `(password)`, `(keyboard-interactive)`, `(none)`, `please try again`, `ssh:`) and a required filesystem signal (open/openat/mkdir/cannot/unable-to or a path fragment). Bias toward false negatives — chown is destructive, so steering a user toward it for unrelated failures is worse than missing the hint.
- **Pure error-wrap helpers `wrapPullErr` / `wrapMakeErr`** factored out of `upgradeRunShell` / `upgradeRunShellStreaming`. Same prose, classification logic split into testable pure functions — both branches (permission-denied vs generic) pinned in unit tests across both verbosity levels (CLI vs streaming).
- **Makefile `install` pre-flight.** Probes `$(BIN_DIR)` writability with a touch/rm dance, then refuses if `$(BIN_REAL)` exists owned by someone else. Both paths emit recovery instructions naming the exact directory.

### Fixed

- **Data race in `upgradeRunShellStreaming`'s shared output buffer.** `exec.Cmd` only serializes its stdout/stderr copy goroutines when `Stdout == Stderr` (interface equality); the previous code called `io.MultiWriter(...)` twice, producing two distinct values that raced on the shared `strings.Builder`. Fixed by building one tee writer per command and assigning it to both file descriptors. New `TestUpgradeRunShellStreaming_NoRaceOnSharedBuf` runs cleanly under `-race`.
- **`make install` chown hint no longer hardcodes `~/.local/bin`.** `$(BIN_DIR)` honors `$PREFIX`; the prior wording misled users with `PREFIX=/opt/canopy` or distro-default install paths. Hint now points at the path mentioned in stderr.

### Known concern (not yet fixed)

- `m.remoteRefreshing = false` is currently written from goroutines in `update_remote.go` / `update_attach.go` outside the Bubbletea Update loop. The pre-existing race was effectively benign while only Update read the field; the new 120 ms spinner tick reads it more frequently and increases the window for `-race` to fire. Fix is a separate cleanup PR — let `remoteRowsLoadedMsg` be the sole owner of clearing the latch.

## [0.21.0.0] - 2026-05-15 — Clipboard bridge for remote workspaces

The laptop's clipboard is now available inside any registered canopy host. Paste a screenshot you copied locally into Claude Code running on tower; copy text from a remote tmux session straight to the local Wayland clipboard; nvim's `y` yanks land on the laptop. None of it requires per-session setup once installed.

User reference + troubleshooting: [docs/clipboard-bridge.md](docs/clipboard-bridge.md). Architecture + design history: [docs/design/v0.18-clipboard-bridge.md](docs/design/v0.18-clipboard-bridge.md).

### Added

- **`canopy install clipboard-bridge`** — one-time per laptop. Writes the systemd user unit that supervises `canopy clipboard-server` (the local daemon), creates `~/.ssh/config.d/canopy/`, and adds the `Include` marker block to `~/.ssh/config` wrapped in `Host *` so it loads regardless of where it lands in the file.
- **`canopy host clipboard <name>`** — per remote host. Full install flow: detects remote UID via SSH; `mkdir -p /run/user/<uid>/canopy` so sshd can bind() the forward sockets; pushes wl-paste/wl-copy wrapper scripts to `~/.local/bin/` on the remote; writes a per-host SSH snippet with a dedicated `Host canopy-tunnel-<name>` alias; writes + enables a per-host systemd user unit (`canopy-clipboard-tunnel-<name>.service`) that holds `ssh -N` open persistently with respawn-on-failure; splices tmux copy-mode bindings into the remote's `~/.tmux.conf` via marker block; verifies end-to-end including PATH precedence for the wrapper.
- **`c` key on the Hosts tab** — TUI surface for the same install flow. Streams the install transcript inline.
- **`📋 bridged` / `📋!` status pill on the Hosts tab** — visible at terminal width ≥ 80c. Populated from the new `clipboard_bridge` field in `canopy ls --json` (schema bumped to v4).
- **`canopy clipboard-server`** — the local daemon. New canopy subcommand; ships in the same binary, no separate goreleaser target. Listens on three Unix sockets in `$XDG_RUNTIME_DIR/canopy/` (clip-text, clip-image, clip-copy) and proxies clipboard reads/writes through the local `wl-clipboard` provider.
- **Tmux copy-mode integration** — `bind-key -T copy-mode-vi y/Enter/C-S-c/MouseDragEnd1Pane send-keys -X copy-pipe-and-cancel "wl-copy"` is auto-spliced into the remote's `~/.tmux.conf`. `set -g extended-keys on` enables modifier distinction for `C-S-c`. `tmux source-file` re-reads in live sessions.

### Architecture

- **Wayland local + Linux remote** in Phase 1. X11/macOS providers are Phase 2 single-file additions behind the `Provider` interface already in place.
- **SSH `RemoteForward` of Unix sockets** is the transport. No new daemons-on-the-wire, no new ports, no third-party services. The local daemon's sockets are mirrored on the remote at the same paths (`/run/user/<uid>/canopy/clip-*.sock`).
- **Persistent tunnel as systemd user unit** — the SSH connection that owns the forwards lives independently of the canopy TUI, any user session, or mosh-attach lifecycle. `WantedBy=default.target` so it comes up at login; `Restart=on-failure` with `StartLimitBurst=10` so a permanently broken setup doesn't pin a CPU.
- **Dedicated `Host canopy-tunnel-<name>` alias** in each per-host SSH snippet — normal `ssh user@host` doesn't match the alias, so the user's everyday SSH (and mosh's bootstrap SSH) doesn't try to bind the RemoteForward sockets and doesn't conflict with the persistent tunnel.

### Caveats (documented in docs/clipboard-bridge.md)

- Tailscale SSH on a bridged host must be disabled (`tailscale set --ssh=false`) — Tailscale's embedded SSH doesn't support Unix-socket forwarding.
- Mosh-attached sessions get `Ctrl+Shift+C` translated to plain `Ctrl+C` (mosh doesn't faithfully forward extended-keys sequences). Use `y` / `Enter` in tmux copy-mode, or SSH-attach instead.
- Single laptop per host at a time — concurrent bridges fight over the bind. Multi-laptop arbitration is parked for v0.19+.
- nvim integration is a manual one-line config (`vim.opt.clipboard = "unnamedplus"`) — install prints the hint; auto-rewriting nvim configs is too varied.

### Out of scope (tracked)

- X11 local + macOS local providers (Phase 2 — single-file Provider additions).
- macOS remote, Windows local (inherited from v0.17.x; project-level).
- `canopy switch --ssh` flag (v0.18.x — for users who want extended-keys fidelity in mosh-attach territory).
- Auto-install of `socat` on the remote when missing (v0.18.x — install.sh's pattern exists; just needs wiring).
- Continuous clipboard content sync (out of scope — different security model than the request/response bridge).
- Audit logging of clipboard content (out of scope — compliance feature; needs separate security review).

### Bug fixes shipped during dogfood

Eight failure modes surfaced + permanently fixed in the same release. Full registry in [docs/design/v0.18-clipboard-bridge.md](docs/design/v0.18-clipboard-bridge.md#failure-modes-registry--design-time--dogfood-discovered).

## [0.20.0.1] - 2026-05-15 — tmux statusline: yellow pill marks remote-attached sessions, workspace folder name shown alongside renamed branch

When you have multiple tmux sessions across local and remote canopies, every statusline looked identical. After `git branch -m`, the folder name vanished from view — the statusline only knew about the branch. This release fixes both visual gaps and includes a stale-tag clear so the pill never lies.

### Added

- **Yellow background pill `@<host>` on canopy-driven remote attaches.** When `canopy switch --on <host>` mosh-attaches into a registered host, the remote canopy's statusline now renders `#[bg=yellow,fg=black] @tower #[default]` as a prefix. The pill mirrors the existing TUI DEV-pill convention (cyan bg pill), guarantees contrast across themes (vs. a foreground-only color, which is muddy on solarized and near-invisible on light themes), and uses the *registered* host nickname from `hosts.json` (not `os.Hostname()`). Pill style codes assembled outside the `escapeForTmux` boundary; only the user-controlled host name inside the pill is escaped, so a hostile `CANOPY_REMOTE_HOST=tower#[bg=red]` can't inject style codes.
- **Workspace folder name shown alongside branch when they differ.** Today's auto-slug workspaces have `wsName == branch` so the statusline renders one identifier (`canopy / robust-otter`). After `git branch -m`, the names diverge; the new format renders both (`canopy / robust-otter / tmux-statusline-remote-local-context`) so you can always identify which directory you're sitting in. The `/` separator extends the existing project-slash-workspace path metaphor; no new visual idioms introduced.
- **Proportional width-collapse: both names shrink together, project survives last.** Under narrow terminals, `wsName` and `branch` share the truncation budget weighted by their display widths, then both drop to initials (`canopy / ro / tsr`), then the whole segment drops below the existing 30-col threshold. Honors the "I want to see both" intent — the alternative ("drop wsName first") silently undoes the feature the user just turned on.

### Fixed

- **Stale `CANOPY_REMOTE_HOST` no longer persists across local re-attach.** Adversarial review caught this: if you `canopy switch foo --on tower` (sets the session env), then later physically attach to that same session on the tower box, the previously-set tmux session env stayed and the pill kept rendering `@tower` — falsely signaling a remote attach. `propagateRemoteHostEnv` now explicitly *unsets* the session env via `tmux set-environment -u` when `CANOPY_REMOTE_HOST` is empty in the calling process. The pill always reflects the actual connection.

### Internal

- New `internal/tmux/env.go`: `SetSessionEnv` / `UnsetSessionEnv` wrap `tmux set-environment -t <session>` (and its `-u` form) with the same missing-session-swallow + error-propagation contract as the rest of `internal/tmux`.
- `cmd/canopy/switch.go`'s `buildRemoteSwitchCmd` gains a `hostName` parameter that conditionally exports `CANOPY_REMOTE_HOST` in the mosh remote bash one-liner; shell-quoted to neutralize injection from hostile nicknames.
- `propagateRemoteHostEnv` helper called from `canopy switch` (named workspace, resurrected, and `canopy main` attach paths) so every attach reconciles the session tag with this process's env.
- 6 new tests; ~96% coverage of the new code paths. Regression test pins today's local-only output shape so the wsName-folded-into-branch case can't drift.

## [0.20.0.0] - 2026-05-15 — Add Project from anywhere: `canopy init <url>`, configurable source-root, TUI Add Project form, remote-host dispatch

Onboarding a project used to require `cd`-ing into it and running `canopy init` from inside. That breaks the flow when you have a GitHub URL in your clipboard, or a project sitting at `~/code/foo` you've been meaning to register, or a remote canopy on `tower` you want to feed without SSH-ing in first. This release does the obvious thing: `canopy init` accepts a path, a git URL, OR a remote-host target, and the TUI grows a matching Add Project form that lives on the splash screen for first-time onboarding and on the Global tab (`a` keybind) for everything after.

### Added

- **`canopy init [PATH_OR_URL] [DEST]` accepts path or git URL.** Auto-detects: scheme-prefixed URLs (`https://`, `http://`, `git://`, `ssh://`, `file://`) and SSH-shortcut URLs (`git@github.com:foo/bar.git`) route through `git clone`; anything else is a local-folder init. No `--type` flag, no separate `clone` subcommand. Optional second positional overrides the destination: `canopy init <url> ~/code/foo`. Backwards-compatible: `canopy init` with no args still inits the cwd.
- **`canopy init <url> --on <host>` dispatches to a registered remote canopy.** Reuses the v0.17 SSH plumbing (`internal/host.SSHCmd`) with a new `SSHRunUser` helper that wraps the remote command in `bash -lc` (so the user's login PATH picks up `~/.local/bin/canopy`) and forces pty allocation (`-t`) so git auth prompts from the remote come back to the local terminal. After dispatch, the laptop fetches a `CANOPY_INIT_RESULT_FILE` written by the remote canopy and auto-registers the project in `~/.canopy/hosts.json` so the next `canopy new --on <host>` resolves the project path without a manual `canopy project add`.
- **`canopy config set|get|list|unset` subcommand.** First user-level config key is `source-root` (where `canopy init <url>` clones go). Storage is `~/.canopy/config.json` with flock-protected reads/writes via the same `WithLock` pattern as `state.json`. Precedence: per-call `--to`/2nd positional > `$CANOPY_SOURCE_ROOT` env > `config.json source-root` > default `~/.canopy/sources`. `canopy config get source-root` prints the value with its origin label (`(config)`, `(env)`, `(default)`) so users can debug "why isn't my setting taking effect?" at a glance. `ls` is aliased to `list` to match canopy's `canopy ls` / `host ls` / `project ls` muscle memory.
- **TUI Add Project form.** Lives on the splash screen for first-run users (replaces the single-key `i` shortcut with a text input pre-loaded with cwd — Enter on the default preserves the muscle memory) and on the Global tab for everyone else (`a` keybind). Modal overlay (`lipgloss.RoundedBorder`, list behind dimmed via `subtleStyle`), `tea.ExecProcess` for git clone so SSH passphrases and HTTPS credential prompts work natively, 3-second `✓ Added <name> at <path>` toast then auto-close. `Tab`/`Shift+Tab` cycles a target picker between local canopy and each registered host; the violet pill on the Target line makes "you're about to add to a remote machine" unmissable. A `Connecting to <host>...` preamble line prints before the SSH handshake so the multi-second first-connect doesn't look like a hang.
- **Settings modal (`,` keybind, top-level).** Edits source-root from any tab without going through `a` then `ctrl+s`. Same flock-protected save flow as the CLI `canopy config set`. Discoverable in the help legend.
- **Auto-register remote projects.** After `canopy init <url> --on <host>` succeeds, the laptop fetches the remote's canonical project root from a hardened temp file (128-bit-random suffix, opened with O_EXCL on the remote so symlink pre-creation fails the write, contents validated as absolute UTF-8 path with no control chars before touching `hosts.json`) and writes the entry to its own `~/.canopy/hosts.json`. Eliminates the post-init manual `canopy project add` step that pre-v0.20 required before `canopy new --on <host>` could resolve the project's path on the remote.

### Changed

- **Splash screen is now a form, not a single-key prompt.** `RunInitSplash` returns a `SplashResult` (Action + Arg) instead of `(didInit bool, err error)`. The caller in `cmd/canopy/route.go` switches on the action and passes the typed value through to the same `runAddProject` orchestrator the CLI uses. Pre-v0.20 behavior is preserved by pre-loading the input field with cwd: pressing Enter on the default mirrors the old `i` keypress.
- **`canopy init`'s early-return path now registers the project in `state.json`.** Pre-v0.20, when `canopy init` was run in a directory that already had `canopy.json`, it printed "already initialized" and returned without touching `state.json`. That worked fine for the rare hand-created-canopy.json case but broke the moment we shipped `canopy init <url>`: cloning a repo that *itself* ships a `canopy.json` (canopy's own repo, for instance) would clone successfully but leave the project invisible to `canopy ls`. The early-return now calls `registerProject` (idempotent — no-op if already registered) so the cloned-existing-canopy.json path is whole.

### Fixed

- **Tilde expansion in source-root paths.** Setting source-root via the TUI Settings modal — or hand-editing `~/.canopy/config.json` — left literal `~/Work` in the stored value. The shell wasn't there to expand it, so `filepath.Abs` later produced absolute nonsense like `/home/cassy/~/Work/cravd` and clones landed in a directory literally named `~`. `config.ExpandTilde` handles the expansion at the read boundary (env var and config file both), so every caller sees a clean absolute path regardless of where the value came from.

### Refactored

- **Init helpers moved to `internal/canopyinit/`.** `LooksLikeGitURL`, `DeriveBasename`, `ResolveCloneDest`, `EnsureSourceRoot`, and `ValidateDestNotInsideWorkspace` were in `cmd/canopy/init_source.go` (package main). Moved to `internal/canopyinit/source.go` as public exports so `internal/ui` can call them without violating the leaf-up dependency rule (cmd → ui, never ui → cmd). The TUI Add Project form's validation logic shares the exact same implementation the CLI uses; no more drift hazard.

## [0.19.0.0] - 2026-05-14 — Remote workspace observability: live claude status, attach indicator, attach warning, stale UX

Three things broke the remote-workspace experience: claude always looked like it was sleeping (the ⚡ thinking badge never fired across SSH), there was no way to tell if someone was already attached to a remote tmux session before you stomped on it, and when wifi flickered the TUI kept showing last-known data with no visual cue. This release closes all three. Remote rows now do everything local rows do: ⚡ when claude is mid-response, ⊙ when someone has a client attached, the confirm-attach modal fires before Enter steals/shares an active session, and stale data dims itself with a "⚠ stale Ns" pill on the host header so you know to retry.

### Added

- **Live claude activity on remote workspaces (⚡ Thinking badge).** `canopy ls --json` on a remote host now captures each agent pane twice (100ms apart, all panes in parallel) and runs the same motion-aware classifier (`ClassifyTwoShot`) that local rows use. Before this, the remote path used single-shot classification and could never return "thinking" by construction — the badge stayed on 💤 even while claude was streaming a response. Per-pane work runs in goroutines so wall-clock stays under ~700ms regardless of how many agent panes a host has.
- **Attached-client indicator (⊙) on remote workspaces.** `LsJSONWorkspace.attached` joins the wire format; `RemoteWorkspace.Attached` and `RemoteWorkspaceRow.Attached` pick it up on the laptop side and stamp `GlobalRow.Attached`. The existing ⊙ glyph renderer and confirm-attach modal start working for remote rows automatically — they were already wired, just never had a signal.
- **Confirm-attach modal fires for remote-attached sessions.** Press Enter on a remote workspace where someone (you, in another mosh session, or a teammate) already has a tmux client attached and you get the share/cancel modal. Y/Enter proceeds with shared attach via `canopy switch --share`; N/Esc cancels. Same flow you already know from local rows, now uniform across the fleet.
- **"⚠ stale Ns" host-section banner + per-row dim when SSH refresh stops landing.** When a host's most-recent successful refresh is older than 10s (≈5 missed TUI ticks), the host header in the Workspaces tab gets an amber "⚠ stale 14s" pill and every remote row from that host renders dimmed via lipgloss's `Faint` (~50% opacity). Gives you a visual "this is last-known, retry with `r`" cue instead of silently showing stale data.

### Changed

- **Wire format: `lsJSONSchemaVersion` bumped from 3 → 4** (added `attached`, `agent_state` now motion-aware). Additive — older laptop clients ignore the new field and older remotes leave it false, so partial rollouts work in both directions.
- **`internal/agent`: new `ClassifyTwoShot(launcher, prev, cur)` helper.** Stateless companion to `ClassifyOneShot` — pure function, no shared state, easy to test. Lives next to `ClassifyOneShot` in `state.go` and reuses the same `normalize()` motion definition so local and remote produce identical badges for identical pane content.
- **`state.GlobalRow` gains `LastSeen time.Time`.** Zero for local rows; the host's most-recent successful refresh timestamp for remote rows. Render-layer only — not persisted; recomputed every refresh tick.
- **`classifyAgentPanes` in `cmd/canopy/ls.go` now does parallel double-capture.** Each agent pane gets its own goroutine; per-pane captures bounded by the existing 500ms timeout each, with a 100ms gap between captures. If the second capture fails after the first succeeded, falls back to `ClassifyOneShot` on the first — lose motion detection that tick but keep awaiting/idle pattern matches.

### Fixed

- **Older remotes (v0.18.x and earlier) without the new fields still render correctly.** Additive JSON: missing `attached` defaults to false, missing thinking-state defaults to the existing single-shot behavior. Verified by `TestRemoteWorkspace_LegacyParseStillWorks`.

## [0.18.0.1] - 2026-05-14 — Capture P3 TODO: recognize my own remote attach

A planning artifact from /plan-eng-review for remote-workspace observability gaps. The full implementation (live claude status, attached-client indicator, attach-warn modal, SSH-drop freshness) was scoped in the review; this release captures one deferred design question from that session — should the confirm-attach modal recognize "this is my own remote attach" and skip the prompt — as a P3 TODO with full context so it's not lost when the implementation lands.

### Added

- **TODOS.md entry (P3): "Recognize 'this is my own remote attach' to skip confirm-attach modal."** Documents the friction that will appear once the remote-status implementation wires `Attached` through for remote rows, and lays out the two cross-machine identity design options (hint file vs marker comparison) with their tradeoffs. Notes why the over-warn-by-default behavior is correct as a starting point.

## [0.18.0.0] - 2026-05-14 — TUI picker for `canopy use`

Typing `canopy use` always meant "see a list, then run the command again with the name you saw." Two steps. This release collapses it to one: on an interactive terminal, `canopy use` (no args) opens a single-screen picker that shows every workspace canopy knows about — release row first, marked with `▶` for whichever is active — and lets you arrow to one and press Enter. The symlink swap, the build flow, the error messages all use the same code paths as the CLI; the picker is just a faster front door. Piped invocations (`canopy use | grep …`, CI scripts, anything with stdin redirected) still get the tabular list, byte-identical to before. `--list` forces tabular even on a TTY, for screen recordings or scripts that want it.

### Added

- **Interactive picker on bare `canopy use`.** ↑/↓ (or `j`/`k`) to move, Enter to switch, `b` to build-then-switch on a workspace row, `q`/Esc/Ctrl-C to cancel without changes. `▶` marks the currently-active target so you don't accidentally switch to where you already are. Rows whose binary doesn't exist on disk render dim; pressing Enter on them shows a one-line nudge ("press b to build it now") instead of crashing out post-altscreen with a stat error. Narrow terminals (<40 cols) get a hint pointing at `--list` instead of letting lipgloss wrap the rows into something unreadable.
- **`canopy use --list` flag.** Forces the tabular output even when stdin is a TTY. Documented escape hatch for screen recordings, debugging, and personal preference.
- **`internal/ui.UseRow` + `internal/ui.UsePickerModel`.** The picker follows the existing `InitSplashModel` precedent — single-screen Bubbletea model that sets state, exits cleanly, and hands control back to `cmd/canopy/use.go` for the actual symlink swap. The boundary keeps build output ("Building canopy in …") on a normal terminal post-altscreen and lets `switchToRelease` / `switchToWorkspace` errors flow through cobra's `RunE` chain unchanged.

### Changed

- **`printUseList` and the new picker now share `useRows()`.** Both surfaces build rows the same way (release first, canopy worktrees alphabetically, non-canopy projects filtered out), so a column edit can't ship to one without the other. Tabular output is byte-compatible with v0.17.5.0 — locked in by the existing `TestPrintUseList` substring assertions plus a new `TestRunUse_NotTTY_FallsBackToList`.
- **TTY detection uses `term.IsTerminal` (ioctl) instead of the mode-bit check** the rest of canopy's prompts use. The mode-bit pattern returns true for `/dev/null` (also a character device), which would route `canopy use < /dev/null` into the altscreen path and fail. The ioctl check distinguishes real ptys from other character devices, so piped and `</dev/null` invocations correctly fall back to tabular.

### Fixed

- **`canopy use < /dev/null` no longer errors with "could not open a new TTY".** Was a latent bug in the copy-pasted `hostInstallIsTerminal` pattern — surfaced as soon as the picker became the default path on a TTY. The new `term.IsTerminal` check is the standard Go answer and matches what bubbletea itself uses internally.

## [0.17.6.0] - 2026-05-14 — Clearer errors when a remote project path is mis-registered

Three symptoms with one root cause kept tripping users up on a fresh remote host: `n` on a remote row failed with bash's terse `cd: No such file or directory`, attaching to a remote main row exited mosh silently with no on-screen reason, and the TUI's "creating in brain" banner read identically for a local `brain` project and a remote one on a different machine. All three trace back to the same thing — the registered remote path doesn't exist on the host (e.g. `/home/avi/Work/brain` registered for a host whose remote user is `jarvis`, not `avi`). This release surfaces that root cause in every flow: the banner now shouts the host, the dispatch scripts pre-check the path and emit a copy-pasteable remediation, and `canopy project add --on` warns when the path doesn't exist on the remote at register-time.

### Added

- **Host pill in the new-workspace banner.** When `n` is pressed on a remote row, the banner now reads `creating on [pi] in [brain] /home/jarvis/Work/brain` — the host name renders as a cyan pill (color 37) so it can't be confused with the violet project pill (color 99). For a remote target the trailing path shows the REMOTE cwd (`newTargetRemoteCwd`), not the missing local root. Closes the failure mode where "creating in brain" looked identical for the local `brain` project and a remote one on pi, making it easy to fire `n` thinking it'd create locally and end up with a remote workspace. `internal/ui/view.go renderTargetBanner`.
- **Dir-existence pre-check in `buildRemoteScript`.** Every remote canopy dispatch (via SSH) now emits `if [ ! -d <path> ]; then echo "..." >&2; exit 7; fi` before the `cd`. Exit 7 is a sentinel the laptop-side dispatcher (`dispatchNewToRemote`, `dispatchVerbToRemote`) keys off to rewrap the failure with a clear "remote project path X does not exist on host Y" error plus a copy-pasteable `canopy project add <project> <correct-path> --on <host>` hint. Without it, the user saw bash's terse `cd: No such file or directory` and had to guess at the registration. `cmd/canopy/new.go buildRemoteScript`, `dispatchNewToRemote`, `cmd/canopy/host_resolve.go dispatchVerbToRemote`.
- **SSH pre-probe before mosh exec in `dispatchSwitchToRemote`.** `canopy switch --on <host>` exec-replaces itself with mosh via `syscall.Exec`, which means a `cd` failure inside the mosh child shell tears down with no message back to the TUI — the user pressed Enter, the screen flashed, and they're back at the list with nothing to act on. A 1-roundtrip `ssh <target> test -d <path>` (sub-100ms over a warm ControlMaster socket) now runs before the mosh exec; on miss it surfaces the same actionable error the new-workspace path uses, in the terminal the TUI is still drawing in. `cmd/canopy/switch.go`, `cmd/canopy/host_resolve.go probeRemoteCwd`.
- **Path probe at registration time in `canopy project add --on`.** The CLI now runs a 5s SSH `test -d <path>` against the registered host before persisting the project entry. If the host's reachable and the path doesn't exist (`test -d` exits 1), the user sees a loud warning: `path "..." does not exist on host "..." — canopy new --on ... and canopy switch --on ... will fail until the path exists`. Best-effort: if the probe fails for transport reasons (host asleep, key not set up), the registration still proceeds — registering ahead of a `git clone` on the remote is a real workflow. `cmd/canopy/project.go projectAddCmd`.

### Why

The original failure-mode trace: a user registered `brain` for the pi host with the path they copy-pasted from their local config (`/home/avi/Work/brain`), but the pi's remote user is `jarvis` — so the actual path is `/home/jarvis/...` and every `cd` on the remote fails. Three separate UX problems flowed from that one mismatch, and none of them named the root cause. Now all three (create, attach-main, attach-named) emit the same actionable error, and `project add` catches the mismatch at the source.


## [0.17.5.0] - 2026-05-14 — Remote version-drift indicator on the Hosts tab

A new release on canopy lit up the local upgrade pill but said nothing about the fleet — the laptop knew it was upgrading and what to, the Hosts tab knew each remote's installed version, but the two never met. So a host on v0.17.3 sat next to a host on v0.17.4 with no way to tell which one needed `U` without comparing the strings by eye. This release closes that gap: every row carries a yellow `⇑` next to its version when it's behind the laptop, a `⇓` when it's ahead, and silence when matched. The host detail drawer spells the same out longhand with a `press U` nudge.

### Added

- **Drift badge on Hosts-tab rows.** Every host with a reported `canopy_version` now compares against a reference (your laptop's running version on release builds; the cached upstream-latest on dev builds). Older than reference renders `v0.17.3.0 ⇑` in yellow; newer renders `v0.17.4.0 ⇓`. Same vocabulary as the top-bar version pill's upgrade arrow — one palette, one meaning everywhere. Suppressed silently when either side is `dev` / `(unknown)` / unreachable so the badge never lies about a comparison we couldn't actually make.
- **Spelled-out drift annotation in the host detail drawer.** Press Enter on a host row to open the detail view and the `canopy: v0.17.3.0` line now grows a yellow trailing `⇑ upgrade available (your local: v0.17.4.0) — press U` (or `⇓ host is ahead of your local (v0.17.4.0)` in the inverted case). The badge tells you *that* there's drift; the drawer tells you *what to press*. Silent when matched.
- **`Model.hostReferenceVersion()`** picks the bare semver each remote is compared against, on a priority ladder: release laptop wins (use its own version, the dogfood case), dev laptop falls back to the cached upstream-latest (the contributor case), dev with no cache returns empty so badges suppress entirely instead of mis-firing.

### Changed

- **`hosts.BuildRows` signature** grew a `referenceVersion string` parameter so the renderer can compute drift per row. Single internal caller in `view.go renderHostsTab`; no external API breakage. Pass `""` to suppress drift detection globally (the dev-with-no-cache fallback path).

## [0.17.4.0] - 2026-05-14 — Remote workspace attach/kill fixes + SSH from Hosts tab

A handful of dogfooding-found rough edges in the v0.17 remote-workspaces flow. Three were reported back-to-back: couldn't attach to a remote project's main session, kill on a remote main row hit the wrong workspace, and the Open Browser key was active on the Hosts tab where it had no port to point at. Plus an auto-attach win on remote create.

### Added

- **Auto-attach after `canopy new --on host`.** Press `n` on a remote row, fill in the picker, hit submit — the laptop mosh-attaches to the freshly-created workspace as soon as the remote canopy reports `Workspace ready: <name>`. No more "press any key, then Enter on the new row" follow-up. Exit code 2 from the remote (workspace OK but prompt delivery failed) still auto-attaches — the workspace is alive, only the initial agent message needs re-sending.
- **`s` on the Hosts tab — interactive SSH into the host.** Drops you into a shell on the remote until you `exit` / Ctrl-D; canopy refreshes when you return. Light y/N confirmation gate first so a stray `s` doesn't bounce you out of the TUI by accident.
- **`canopy switch --on <host> --main`** — new flag. Routes to the remote's `canopy main` for project-main session attach. Keyed off an explicit flag rather than the literal string `"(main)"` so a workspace happening to be named `(main)` still attaches via `canopy switch` instead of being silently redirected.

### Fixed

- **Attach to a remote project's main session.** Enter on a remote `(main)` row used to dispatch `canopy switch (main)` on the remote, which tried to look up a workspace literally named `(main)` and failed. The TUI now passes `--main` and the remote runs `canopy main` instead. Project context flows through via `--remote-cwd` from the host registry; if the project isn't registered for that host, the attach refuses with a clear "run `canopy project add --host <host> <path>`" hint rather than silently attaching to the wrong project.
- **Kill on a remote main row hit the wrong workspace.** Local rows come first in the unified list, so `(main)` row resolution matched the local main before the remote one — `K` on the remote main row killed the local main session instead. The confirm-kill resolver now disambiguates by `Host` + `Project` so the right tmux session goes down.
- **Open Browser (`B`) hidden on the Hosts tab.** The key used to be active everywhere; on the Hosts tab there's no port to point a browser at, so the action was either a no-op or worse.

### Changed

- SSH dispatched via the new `s` keybinding uses `ssh -- <target>` so a registry SSHTarget with a leading `-` can't be interpreted as an ssh flag. Defense in depth against a corrupted `~/.canopy/hosts.json`.
- `parseRemoteWorkspaceName` now takes the LAST `Workspace ready: <name>` line in the streamed output rather than the first. Prevents output earlier in the stream (a setup hook echoing the marker, a `--prompt` body containing the literal text) from redirecting the auto-attach.
- `canopy new --on host` now preserves the remote's exit-code-2 distinction ("workspace created, prompt failed") locally instead of collapsing every non-zero remote exit to exit 1.

## [0.17.3.0] - 2026-05-13 — Remote canopy install

The third Hosts-tab follow-up surfaced by dogfooding: adding a host that didn't have canopy yet meant dropping out to a separate SSH session to run the curl|sh installer. install.sh itself only detected missing OS deps (git / tmux 3.2+ / Go 1.22+ / make) and printed the right `apt-get`/`pacman`/`brew` line for the user to copy. Now both halves close: the installer can install its own prereqs, and the laptop can drive the install over SSH from three different surfaces.

### Added

- **`canopy host install <name>`** — new CLI verb. SSHes to the named host, pipes install.sh from main to bash with `--yes`, streams output to the laptop, then re-probes to confirm canopy is reachable on the new install. `--reinstall` wipes `~/.canopy/src` on the remote and re-clones fresh (state — workspaces, hosts.json, state.json — is preserved). Local y/N confirmation gate skips with `--yes`.
- **`I` on the Hosts tab** — installs (or reinstalls) canopy on the selected remote. Reuses the existing `hostUpgradeMode` confirm → run → done state machine, so the UX matches `U` (upgrade) and `S` (use release). Always visible while the Hosts tab has rows — install.sh is idempotent, so pressing `I` on a healthy host is safe (it prints "already installed" and exits).
- **install.sh `--yes`** — non-interactive mode. Auto-confirms dep installs. Required when install.sh runs over SSH (stdin isn't a tty), used by `canopy host install` automatically.
- **install.sh `--reinstall`** — wipes `~/.canopy/src` before cloning fresh. Recovery path when the source clone is corrupt or you want to drop back to whatever main is right now without manually `rm -rf`'ing.
- **install.sh dep auto-install** — when `git`, `tmux 3.2+`, `Go`, or `make` is missing, install.sh now prints the right `sudo apt-get install -y` / `sudo pacman -S --noconfirm` / `sudo dnf install -y` / `brew install` line and prompts y/N to run it. `--yes` auto-confirms. macOS uses brew (no sudo); Linux distros use their package manager (passwordless sudo required in `--yes` mode).
- **Host-add wizard now auto-offers install** — the `probeBroken` branch (SSH works but no canopy on the remote) opens a confirm form and invokes the same install path instead of printing a "do it yourself" message. Same UX whether the install fires from the CLI, the TUI, or the wizard.

### Changed

- The shared `host.InstallScript(reinstall bool)` builder in `internal/host` is the single source of truth for the SSH payload (`curl`/`wget` fallback → `bash -s -- --yes [--reinstall]`). The CLI surface, the wizard, and the TUI all use it — single test surface, no drift.
- Confirm screen in `update_host_upgrade.go` now suppresses the `current: v…` line when the remote hasn't reported a version yet (a fresh-install case). Install-flavored confirm copy ("Install canopy on this host?") replaces the back-tick `Run \`canopy install\`` line that fits upgrade but doesn't fit a curl|bash payload.
- Carries the new `Binding.Group` field forward: the `I` keybind is grouped under `meta` alongside the other host-management verbs (`U`, `S`).

## [0.17.2.0] - 2026-05-13 — Help legend wrap, viewport crop, refreshed docs

Two TUI ergonomics fixes that surfaced when dogfooding canopy in small tmux popups and short terminals: the help legend overflowed the right edge in narrow windows, and the top of the workspace list scrolled off-screen in short ones. Plus a documentation pass that brings the README from v0.14 → v0.17 (the README was 13 minor releases behind the code).

### Added

- **Grouped, width-aware help legend.** Each keybinding chip carries a `Group` label (nav / tabs / open / act / meta). The bottom-bar renderer renders one group per line so the layout is predictable and the eye learns where each verb category lives. Groups that overflow a narrow width wrap chip-by-chip within the group; no chip is ever cut off. Applies to the Workspaces tab AND the Hosts tab — same code path, host-specific bindings (enter→host detail, d→remove host, a→set up auth, U→upgrade host, S→switch release) automatically get the right groups.
- **Compact help mode for short viewports.** Below 20 rows of terminal height, the legend collapses to a single line: `↑/↓ nav   enter <verb>   n new   ?  more   q quit`. The full legend lives behind `?`. Frees up four lines of vertical space so the brand pill and tab bar stay on-screen.
- **Viewport-aware projectlist.** The workspace table now crops to its allotted height instead of overflowing the terminal. The cursor row is centered when possible; rows hidden above/below show as dim `↑N more` / `↓N more` indicators on the top/bottom edge. The crop replaces a content line rather than adding one, so the visible window stays exactly the allotted height.
- **`docs/remote-workspaces.md`** — new end-to-end guide for v0.17 remote dispatch: host registry, project registry, mosh+tmux attach, `U`/`S` maintenance flow, TUI Remote hosts tab keys, and a remote-only troubleshooting section.

### Changed

- **README rewritten for v0.17.** "What's new in v0.17" callout, agent-state badges section, workspace-health badges section, end-to-end walkthrough, remote-workspaces section with a screenshot of the Hosts tab, and a corrected port default (was incorrectly listed as 3000; actual default is 40000). New hero screenshot `docs/images/tui-workspaces.png` replaces the older `tui-global.png`.
- **`docs/getting-started.md` refreshed.** Full keybinding table, agent + health badge legends, the `--prompt` / `--no-attach` fire-and-forget flow, `canopy rename` for branch identity, and a remote-workspaces pointer.
- **`docs/troubleshooting.md` gained a remote-host section.** SSH auth recovery, `(unknown)` version recovery, mosh fallback, deprecated `host project` syntax, `--remote-cwd` from `$HOME`. Also fixed the stale port-default reference.
- **`docs/canopy-json.md`** — fixed the stale claim that `scripts.run` was a "v0.5 TODO." It's been on-demand via `canopy run` (or `<prefix>r`) since v0.7.
- **Chrome reserve bumped 6 → 9 lines** in the parent's `SetSize` call to make room for the 5-line grouped help. Auto-shaves 4 lines when compact mode is active.

### Internal

- New `Binding.Group` field. The five logical buckets are populated for every entry in `listModeBindings`; renderer falls back to "act" for any future binding that forgets to set one.
- `projectlist.renderTable` now returns `(string, cursorLine int)` so the parent View can crop around the cursor without re-parsing the rendered output. Internal API only; no external callers.
- 12 new tests across `internal/ui/model_test.go` (7 wrap + compact subtests) and `internal/ui/projectlist/clip_test.go` (5 crop subtests covering no-height, crop-to-fit, cursor visibility, and ↑N/↓N marker emission).
## [0.17.1.0] - 2026-05-13 — Remote canopy maintenance

Two follow-ups to v0.17's Remote workspaces work, both surfaced by dogfooding: the laptop couldn't tell what version a remote was running, and it had no way to upgrade a remote without dropping out to a separate SSH session.

### Added

- **`U` on the Hosts tab** runs `canopy upgrade --yes` on the selected remote and streams the output into the TUI. Confirm screen first (no flicker, no surprise SSH), then a streaming pane while `git pull && make install` runs on the host. Errors stay on screen instead of being hidden inside an alt-screen suspend the way `tea.ExecProcess` does. Ctrl-C cancels via `context.CancelFunc`; any key dismisses the done screen and refreshes the Hosts tab so the new version appears.
- **`S` on the Hosts tab** runs `canopy use release` on the selected remote — the recovery path for hosts running a dev binary, where `canopy upgrade` refuses with "Switch to the released canopy first." Same streaming machinery as `U`; once it completes the next refresh tick reports a release version and `U` becomes available.
- Both flows share a parameterized state machine (`hostUpgradeMode`: confirming → running → doneOK | doneError) in `internal/ui/update_host_upgrade.go`, so adding a third remote-maintenance verb later is a 10-line addition.

### Changed

- The previous `U` on Hosts shelled out via `tea.ExecProcess` with `ssh -t`, which flickered the alt-screen and hid the dev-binary error inside the suspended TUI. Replaced with the streaming flow above.
- `availableHostUpgrade` and `availableHostSwitchRelease` gate the two keys against the remote's reported version: `U` hides on dev hosts (would error), `S` hides on hosts reporting a real semver (would no-op). Hosts reporting `"(unknown)"` or empty version surface both keys (legacy remotes pre-dating the version-emit fix below).
- The pre-existing local `U` (the laptop's own upgrade flow) is now gated off the Hosts tab via `availableLocalUpgrade` so it doesn't collide with the new per-host dispatch on the same key.

### Fixed

- **`canopy_version` over the wire was always `"(unknown)"`.** `cmd/canopy/ls.go` declared `canopyVersionInfo = "(unknown)"` and never assigned it, so every `canopy ls --json` reported unknown to the laptop. The Hosts-tab version column showed `v(unknown)` for every host, and the workspace-detail drawer had no honest version line. Now wired from the package-level `version` (with the conventional leading `v` stripped so the laptop's display layer can prefix `v` without producing `vv0.17.1.0+abc`).
- The host-upgrade SSH command earlier passed `bash -l -c <multi-line-script>` as separate argv tokens, which SSH joins with spaces — bash saw `-c set` (the script's first word) and dumped its full environment to stdout before any real command ran. The streaming flow uses a single-arg shell-parseable string, the same pitfall the `internal/host/refresh.go` script-via-stdin path documents.

## [0.17.0.0] - 2026-05-13 — Remote workspaces

Canopy workspaces can now live on SSH-reachable machines. The laptop becomes a thin client; the heavy work (scripts.setup, scripts.run, agent panes) happens on the host. One unified TUI views and acts on every workspace across every host.

### Added

- **Host registry** (`~/.canopy/hosts.json`). Register SSH-reachable boxes once with `canopy host add tower cassy@tower.tail.ts.net`, then refer to them by name everywhere (`--on tower`). Multi-project per host: one tower can hold canopy + cravd + dotfiles independently. Run `canopy host show tower` to see what's where.
- **Project registry under `canopy project`.** Top-level namespace replaces the awkward `canopy host project add`. `canopy project add cravd /home/cassy/Work/cravd --on tower` binds a project name to a remote path. `canopy project ls` shows local + remote in one view.
- **Remote dispatch flag `--on`** on `canopy new`, `canopy switch`, `canopy rm`, `canopy retry`. Use it from anywhere; canopy resolves the host + remote cwd from the registry. `--remote-cwd <path>` overrides when you need a one-off.
- **`canopy new --on tower --prompt "..."`** for fire-and-forget remote agent dispatch. The prompt travels over SSH via base64 + heredoc + umask-077 temp file; never appears in `ps aux` on either machine.
- **`canopy switch --on tower foo`** attaches via mosh+tmux. UDP transport, state-sync resilience, laptop-suspend tolerance.
- **`canopy switch --share`** allows multi-attach instead of stealing the existing client. Propagates over SSH via `CANOPY_NO_DETACH=1`.
- **Refresher** fans out `canopy ls --json` to every registered host on the TUI's 2s tick. Per-host goroutines with 3s deadlines so one slow host can't block the others. SSH ControlMaster reuse keeps subsequent fetches near-zero latency. `BatchMode=yes` prevents password-prompt hangs that would corrupt the Bubbletea render.
- **`canopy ls --json` schema v3** carries `mem_rss`, `cpu`, `hints`, `last_error_hint`, `agent_state` per workspace row. Backwards-compatible: older laptop parsers ignore unknown fields.
- **`agent.ClassifyOneShot`** — single-shot agent-pane classifier (idle / awaiting_input / unknown) for the remote-side `canopy ls --json`. The 💤 / ✋ badges now render for remote rows in the laptop TUI (no more blind columns).
- **TUI tabs:** `<project> · Workspaces · Remote hosts` (project tab dropped when launched outside any project). Left/right/h/l/Tab/Shift+Tab cycle.
- **Remote hosts tab** (renamed from "Hosts"): independent cursor, full-row selection highlight matching the Workspaces tab. Verbs gated to host-specific actions: `enter` opens a detail drawer, `d` removes a host, `a` runs ssh-copy-id on a host that lost key auth, `n` opens an in-TUI add-host form (Tab-nav between name + ssh-target). Post-add probe offers ssh-copy-id automatically when auth fails.
- **`n` on a remote row** opens the full new-workspace picker (Fresh + Prompt; PR/Issue/Branch hidden for remote since they need remote `gh`). Streams setup output to the busy view via subprocess capture, same UX as local.
- **Workspace-list verbs work on remote rows:** `d` (with `F` to force-remove on hanging work), `K` (kill remote tmux session via SSH), `B` (auto-creates `ssh -L` port forward to laptop, then xdg-open), `i` (drawer), `R` (retry), `enter` (attach via mosh).
- **Attach-share warning:** pressing Enter on a session another client is already on now pops a y/N confirm before stealing/sharing. Skips the prompt when re-attaching to your own launching workspace (the expected flow).

### Changed

- `canopy host project add/ls/rm` renamed to top-level `canopy project add/ls/rm`. The `host project` subcommands are removed (no aliasing — clean v0.17 cut).
- Remote action callbacks (rm/retry/kill/wizard/attach) now fire a combined local+remote refresh via `refreshAllMsg` instead of local-only `refreshCmd`. Previously you had to manually press `r` to see a deleted remote row disappear.
- Workspace verb keybindings (d/K/B/P/R/i/enter) are gated to the Workspaces tab — pressing them on the Remote hosts tab no longer fires against stale cursor rows.
- The `f` keybind (focus project) is gone. With contextual tabs the "load this project into Local" action has no remaining purpose.
- `internal/ui/update.go` carved from 3,202 lines into 8 focused sibling files (`update_attach.go`, `update_delete.go`, `update_host.go`, `update_kill.go`, `update_new.go`, `update_remote.go`, `update_retry.go`, `update_tabs.go`). No behavior change.

### Fixed

- Refresher used `bash -lc "<script>"` argv form which SSH word-split on the remote — only the first word ran. Now pipes the script via stdin to `bash -l`.
- Refresher's first call did flock I/O on the UI thread before returning the tea.Cmd, freezing the TUI on open against an unreachable host. All I/O is now inside the returned closure.
- Refresher caused password-prompt hangs against hosts without key auth. Now uses `BatchMode=yes` so auth failures fast-fail.
- `canopy host add --interactive` probe used `canopy --version` (not a flag). Switched to `canopy version` subcommand.
- `canopy new --prompt` over SSH leaked the temp prompt file on success because the script used `exec canopy`, replacing bash before the trap could fire. Removed the `exec` so trap runs on shell exit.
- Remote `(main)` rows showed status "main" (literal) instead of "running" / "not started", and branch "↗ —" instead of the actual default branch. Both caused by missing `IsMain=true` on the wire-format → GlobalRow conversion and a laptop-only `fillMainBranches` pass. Now both happen on the remote side of `canopy ls --json`.
- Remote workspace rows showed a misleading `·` "no agent pane" badge even when Claude was running. Single-shot agent classification on the remote populates the correct 💤 / ✋ instead.
- The TUI's confirm-attach modal promised "share the session" but the attach path still passed `-d` / called `detachOtherClients`, kicking the other client off. `tmux.AttachOptions{Shared: true}` now skips both; `canopy switch --share` propagates over SSH so the remote canopy switch does too.
- The Remote hosts tab `Enter` on a host rendered an error-colored info string instead of a real detail view. Replaced with `hostDetailMode` showing ssh-target, registered projects, version, last-seen, last-error.
- Remote `canopy rm` rejected on hanging work and the confirm modal gave no escape to `F`. The modal now offers both `y` and `F` for remote rows since the laptop can't run the safety preflight.
- After dismissing the post-add ssh-copy-id offer there was no way to retry. New `a` keybind on the Remote hosts tab opens the same modal for an existing host.
- `n` on a remote row from `$HOME` (outside any project) failed with "needs a project but you're not inside any". TUI now passes `--remote-cwd` resolved from the host registry.

### Removed

- `canopy host project` subcommand namespace (use top-level `canopy project` instead).
- The `f` keybind and `actionFocusProject` function.
- The huh-based subprocess wizard for `canopy host add --interactive` is still available from the CLI, but the TUI's `n` on Remote hosts opens the in-TUI form instead.

## [0.16.2.1] - 2026-05-11 — Pane-role contract hardening (defense-in-depth)

Quiet cleanup release. No user-facing behavior change in the happy path — these
are the safety nets behind the pane-role contract that v0.16.0 shipped. If you
have weird tmux configs, exotic layouts, or have ever seen "the agent badge is
on the wrong pane," this is the release that closes those gaps.

### Changed

- Pane-ID capture now structurally validates `%<digits>` format in `tmux.Create`
  and `tmux.SplitPane`. A user tmux hook that prints to stdout
  (`set-hook session-created 'display-message ...'`) can no longer poison the
  captured pane ID and silently propagate garbage into `SetRole` / `SelectPane`.
- `LookupPane` / `LookupAllPanes` now reject role globs with a leading or
  internal `*` (new `tmux.ErrInvalidGlob`). Previously `LookupPane("*:claude")`
  silently matched nothing — it read as "no such pane" when the real cause was
  a malformed pattern. Trailing `*` (the documented prefix-match form) still
  works.
- `BackfillRoles` returns no value. The function never produced an error in
  practice and every caller wrote `_ = BackfillRoles(...)`, which read as
  intentional error-swallow but wasn't.
- `TestRoles_PanesInOrder` now asserts the layout-tree traversal order
  (`[ide, agent, terminal:shell]`) and call-stability — the contract
  `BackfillRoles`' canonical-role mapping has always depended on. Previously
  the test only checked set-membership despite its name promising order.

### Added — backfill safeguards

`workspace.BackfillRoles` (which tags pre-v0.16 sessions on first attach) is
now harder to fool when the live layout doesn't match canopy's canonical shape:

- **Window-count gate.** Sessions with more than one window are skipped.
  Without this, three panes spread across multiple tmux windows passed the
  3-pane check (list-panes is session-scoped, not window-scoped) and
  positional inference would tag arbitrary panes from arbitrary windows.
- **Command-sniffing safeguard.** Reads `pane_current_command` and refuses to
  backfill if an editor or agent command is sitting in the wrong canonical
  slot (e.g., vim in the agent position). Shell-class commands stay
  permissive — "I quit claude to poke around" is still a valid intermediate
  state.
- **Concurrent-attach guard.** Skips backfill when a tmux client is already
  attached to the target session. Narrows the read-modify-write race window
  for the rare two-process case where two `canopy switch <ws>` invocations
  fire against the same v0.15-style session simultaneously.

Each new safeguard ships with both unit tests for the helper logic and an
integration test exercising the full path.

### Known limitations (documented, not yet fixed — captured in TODOS.md)

These are the residual gaps the safeguards above don't close. Acknowledged
explicitly in `BackfillRoles`' docstring so future work knows where to dig:

- **Already-but-wrong-tagged sessions** stay corrupted forever — backfill's
  early-exit only checks that role names exist, not that they're on the
  canonical panes.
- **launcherType vs observed-command divergence**: a pane actually running
  `codex` in a global flow (empty launcherType) gets tagged `agent:claude`
  because the canonical role uses the launcherType argument, not the sniffed
  command.
- **TOCTOU between `ListAllRoles` / `PanesInOrder` / `PaneCommands`**: a
  pathological multi-process race could still partially mistag despite the
  incongruence + command-sniff guards.
- **`HasClient` swallows all tmux errors** as "no client" — if the server
  fails non-trivially, the concurrent-attach guard silently disables itself.

### Behind the scenes

- New `tmux.PaneCommands` helper — single batched call for
  `pane_id → pane_current_command` mapping.
- New `tmux.HasClient` helper — `list-clients -t <session>` swallow-all wrapper
  intended only for the backfill polling use-case (documented in its
  docstring).
- New `tmux.WindowCount` helper — straightforward `list-windows` wrapper.

## [0.16.2.0] - 2026-05-11 — Spawn-with-task from the TUI

The prompt-driven workspace creation that landed as a CLI flag in v0.16.1 now has
a home in the TUI. Press `n`, pick "From a prompt", type (or paste) the task,
hit Ctrl+S — canopy spins up the workspace and hands claude its marching orders
in one move. Same trust-dialog state machine as the CLI, same defense against
typing the prompt into a crashed-to-shell pane.

### Added

- New TUI picker option: "From a prompt" (key `t`), sitting directly under
  "Fresh workspace" since it IS a fresh workspace with an opening message. The
  10-row textarea scrolls internally for long pastes; Ctrl+S submits, Enter
  inserts a newline, Esc steps back to the picker. After Create succeeds the
  prompt is delivered to the agent pane before auto-attach — the user lands in
  a workspace where claude is already thinking.
- Picker reorder so the most-used variants come first: fresh → prompt →
  pull request → issue → branch. Cursor + letter shortcuts updated to match.

### Changed

- `workspace.SendInitialPrompt` and `workspace.ErrPromptFailed` are now exported
  from `internal/workspace/initprompt.go`. Both the CLI (`canopy new --prompt`)
  and the TUI's new picker option share one implementation of the trust-dialog
  state machine — no copy-paste, no drift. `workspace.IsPromptFailed(err)` is
  the typed convenience for the `errors.As` check.

## [0.16.1.0] - 2026-05-10 — Background workspaces

Fire-and-forget claude. Spawn a workspace with `canopy new --prompt "..." --no-attach`,
walk away, come back, open the TUI, and SEE which one needs you. The first version
where you can run three claudes in parallel without staring at any of them.

### Added

- `canopy new --prompt "<text>"` and `--prompt-file <path>` — send an initial
  message to the agent pane right after workspace creation. Combine with the
  existing `--no-attach` for true fire-and-forget. Multi-line prompts via
  `--prompt-file` arrive at claude as one message (verified against claude's
  paste-buffer behavior). 32KB cap; oversized files are rejected, never
  silently truncated.
- TUI badge column showing per-workspace agent state, polled every 2 seconds:
  - ⚡ (cyan) — claude is thinking
  - 💤 (gray) — claude is idle, ready for your next message
  - ✋ (yellow) — claude is awaiting input (y/N or tool-permission popup blocking)
  - · (subtle) — workspace has no agent pane
- Press `?` for the legend.
- New exit code: `canopy new --prompt` exits 2 (not 1) when the workspace was
  created OK but the prompt couldn't be delivered. Scripts can now distinguish
  "workspace failed" from "workspace OK, agent didn't get its prompt".

### How it stays safe

- Trust-dialog state machine waits up to 10s for claude's first-launch
  trust prompt, dismisses it, then verifies claude is actually rendering
  before typing anything. If claude crashed and the pane fell back to a
  shell, the prompt is refused — no shell metacharacter execution from
  `--prompt "rm -rf /tmp"`.
- Verification looks at the bottom of the pane (where the live cursor is),
  not anywhere on screen, so stale claude scrollback after a crash can't
  fool the check.
- Every tmux call has a 500ms-2s timeout; a hung tmux server can't freeze
  the CLI or the TUI.

### Behind the scenes

- New `internal/agent` Detector classifies pane state from periodic content
  captures. Strips ANSI escapes, the spinner line, the auto-mode footer,
  and the input-prompt line before hashing — so user typing into claude
  doesn't read as "thinking".
- Polling skips dead workspaces automatically (only alive tmux sessions
  show up in `list-panes -a`). One batched tmux call per 2s tick across
  all workspaces, regardless of count.
- Generation token on the poll loop guarantees at most one tick in flight,
  even if Init re-fires.

## [0.16.0.0] - 2026-05-10 — Pane-role contract (internal refactor)

Canopy now addresses tmux panes by ROLE instead of by index. Every pane
canopy creates gets tagged with a `@canopy-role` tmux user-option (`ide`,
`agent:claude`, `terminal:shell`), and downstream code looks panes up by
role rather than remembering position. No user-visible behavior change in
this release — the refactor unblocks v0.16+ background workspaces, custom
layouts, and the kick-off-with-prompt feature without any of those needing
a second architectural change.

### Added

- `internal/tmux/roles.go`: `SetRole`, `LookupPane`, `LookupAllPanes`,
  `SelectPane`, `PaneCount`, `PanesInOrder`, `ListAllRoles` — the new
  role-addressing API. Uses tmux user-options (process-proof, persistent
  for the pane lifetime, queryable in one syscall via `list-panes -F`).
- `internal/workspace/backfill.go`: retroactively tags v0.15-style sessions
  on first attach. Conservative: only tags when the session has the
  canonical 3-pane layout AND no existing tags conflict with the canonical
  positional mapping. Skips with a warn line otherwise.
- `internal/agent/launchers.go::RoleForType`: produces `agent:<launcher>`
  role strings with empty-input defaulting to `agent:claude`.

### Changed

- `tmux.Create` and `tmux.SplitPane` now return `(paneID string, err error)`.
  Captured via `-P -F '#{pane_id}'` at creation time. Callers that don't
  need the ID (debug sessions) use `_, err := ...`.
- `internal/workspace/lifecycle.go::buildSession` and the resurrect path
  tag each pane after creation.
- `internal/workspace/main_session.go::buildMainSession` likewise; alive-
  branch attaches now run backfill so v0.15 main sessions get tagged on
  their first v0.16 attach.
- `cmd/canopy/switch.go` and `internal/ui/update.go::attachOrSwitch` hook
  backfill before every attach to a ready-status workspace.
- `cmd/canopy/main.go` and `cmd/canopy/new.go` help text: stale "4-pane"
  references corrected to "3-pane" (drive-by fix).

### Notes

- Layout-tree traversal vs creation order: a smoke test caught that
  `tmux list-panes` returns panes in layout-tree depth-first order, NOT
  creation order. Backfill maps positions accordingly. If `buildSession`'s
  split sequence ever changes, the canonical mapping in `backfill.go`
  must update to match the new traversal order.
- Adversarial review caught and fixed (pre-ship) a bug where backfill
  would overwrite an existing tag if it landed at a different canonical
  position. v0.16 only tags untagged panes; sessions with incongruent
  existing tags are skipped entirely.
- 6 follow-ups deferred to v0.16.x — see TODOS.md "Pane-role contract
  follow-ups" entry for the list.

### Tests

- 12 new tests in `internal/tmux/roles_test.go` (round-trip, glob, multi-
  match, cross-session contamination, etc.)
- 6 new tests in `internal/workspace/backfill_test.go` (canonical, partial,
  incongruent-skip, non-canonical-skip, empty-launcher, already-tagged)
- 1 new table-driven test for `agent.RoleForType`
- Existing `tmux.Create`/`SplitPane` tests updated for new signature

## [0.15.2.0] - 2026-05-09 — Prefix-less `Ctrl+Alt+c` summon chord

Pressing the tmux prefix to reach canopy is one chord too many when canopy
is the verb you reach for ten times a day. `canopy install tmux` now writes
a no-prefix `Ctrl+Alt+c` binding alongside the existing `<prefix>g`, so the
TUI is one chord away from any pane — same display-popup payload, same
project resolution, no behavior change for the prefix bind.

### Added

- `bind -n C-M-c display-popup -E "CANOPY_IN_POPUP=1 canopy"` is appended
  to the managed canopy block on every fresh install. Existing installs
  pick it up via `canopy install tmux --force` (the present-block guard
  refuses overwrites without `--force`, which stays the right call for
  hand-edited blocks).
- Success message updates to mention both chords so users discover the
  new alias right after running install.

### Notes

- Both the prefix bind and the no-prefix chord share one display-popup
  payload (`-d "#{pane_current_path}"`, `CANOPY_IN_POPUP=1`). Diverging
  the two would invite drift; a regression test enforces the shared
  shape.
- The chord is unclaimed by common shells/editors and forwards through
  Ghostty, Alacritty, kitty, and iTerm2. Terminals that swallow it can
  hand-edit the binding inside the marker block — re-runs without
  `--force` preserve hand edits.

## [0.15.1.0] - 2026-05-08 — `canopy rename --pin` / `--unpin` for power users

Opt out of branch auto-tracking on workspaces where you rebase frequently or
host multiple feature branches in a single worktree. The default 80% case
(one branch per workspace, rename branch on turn 1, never touch it again)
keeps working as before — pinning is purely additive.

### Added

- `canopy rename --pin` freezes the workspace's display label at the current
  branch. Subsequent `git checkout` / `git branch -m` calls inside the
  worktree no longer propagate to the tmux session name, statusline, or
  TUI rows. Idempotent: re-running `--pin` re-snapshots whatever branch is
  currently checked out.
- `canopy rename --unpin` releases the pin and re-syncs labels to the
  worktree's current branch in one shot.
- Plain `canopy rename` on a pinned workspace prints a friendly hint that
  names the unpin command instead of silently no-opping.

### Changed

- `internal/state.Workspace` gains a `pin_display_name` field. `omitempty`
  on the JSON tag means existing state.json files round-trip unchanged.

### Notes

Closes the v0.15 deferred-list entry `canopy rename --pin / --unpin`. The
pin check sits AFTER the legacy hyphen→slash session migration in
`SyncBranch` so a one-time format upgrade still happens for pinned
workspaces left over from pre-v0.16.

## [0.15.0.0] - 2026-05-06 — Workspace identity follows the branch you renamed

Every visible canopy surface now reflects the meaningful branch name instead
of the auto-generated workspace slug. Rename your branch with `git branch -m`
and within the next statusline tick (15 seconds) the tmux session name, the
status-right widget, the canopy TUI rows, and your terminal tab title all
update to match. No more glancing at `canopy-clever-jay` and wondering which
workspace you're in.

### Added

- `canopy rename [<workspace>]` — forces an immediate label refresh from the
  worktree's current branch. Useful right after `git branch -m` when you
  want the surfaces synced now instead of waiting for the next tick. With
  no argument, targets the workspace your shell is in (matches by tmux
  session if you're inside one, or by cwd-prefix if you're in a different
  terminal).
- `canopy use` listing now shows a BRANCH column alongside TARGET, so you
  can see at a glance which workspace is for which feature. The lookup also
  accepts the branch name directly: `canopy use clear-workspace-identity`
  resolves to the workspace whose branch matches.
- Statusline width-aware collapse: long branch names render in full when
  there's room, right-truncate with `…` in middle widths, collapse to
  initials (`clear-workspace-identity` → `cwi`) below 40 columns, and drop
  entirely below 30. Unicode-safe (East-Asian wide chars sized correctly).
- Terminal tab title forwarding via `set-titles on` in the canopy tmux
  block. Ghostty/iTerm tabs now show the same `canopy/branch` identity as
  the statusline. Run `canopy install tmux --force` to pick up the new
  config.

### Changed

- Tmux session name format: `canopy-<branch>` → `canopy/<branch>`. The
  slash visually parses as namespacing, matches the `<project>/<feature>`
  mental model. Existing sessions auto-migrate on the next statusline tick
  or `canopy rename` invocation; no manual cleanup needed.
- Tmux session window-name no longer duplicates the session prefix. Window
  list reads `1:<branch>` instead of `1:canopy-<branch>`. canopy also pins
  `automatic-rename off` on its windows so tmux's default process-name
  tracking doesn't fight the branch label.
- Workspace status-line and TUI rows now use the live git branch (live-
  synced from `git rev-parse` on every statusline tick) instead of the
  create-time cached value. The pre-existing Reconcile branch refresh keeps
  working; the new SyncBranch path makes the same logic available per-
  workspace without taking the full Reconcile flock.

### Removed

- `Workspace.Project` JSON field on state.json rows. Derived on-the-fly
  from `ProjectRoot` via `ProjectBasename()`. Old state files load fine;
  the field gets silently dropped on next save.
- `Workspace.TmuxSession` JSON field on state.json rows. Computed on-the-
  fly via `TmuxSessionName()` as `<project>/<branch>`. Same self-cleaning
  save behavior as above. External tools that grepped state.json for the
  field need to derive it from `project_root + branch`.

### Fixed

- `tmux install` now writes `set -g status-left-length 50` so long session
  names like `canopy/clear-workspace-identity` aren't truncated to garbage
  by tmux's stingy 10-char default.
- `internal/lifecycle/rename.go::gitCurrentBranch` was a duplicate of
  `internal/git/repo.go::CurrentBranch`. Dropped the local copy; all
  callers now use the canonical helper. Stale comments at `pr_status.go:67`
  and `lifecycle/rename.go:36` updated to reflect the new live-sync
  reality.

## [0.14.1.0] - 2026-05-01 — Feat: B opens the running app in a browser, P opens the PR

The TUI's PR-open shortcut moved from lowercase `p` to capital `P`, and a
new capital `B` opens the workspace's running app at
`http://localhost:<port>` via `xdg-open`. Lowercase `p` lives one row
below `k` (cursor up) and the user kept firing `gh pr view --web` by
accident on every misaimed nav stroke. Both new bindings now require
shift, matching the friction posture of `K` (kill tmux): destructive or
side-effecting verbs need a deliberate keypress.

### Added

- **`B` opens the running app in the browser.** Available on any row with
  a live tmux session and an allocated port — main rows and workspace
  rows both qualify, because `scripts.run` exposes a server on
  `CANOPY_PORT` in either context. Hidden when the cursor sits on a
  stopped row or one with no port, so the binding doesn't show up
  promising a 404. Linux-only handoff via `xdg-open`; errors (no handler,
  binary missing) surface on the status line instead of hanging the TUI.

### Changed

- **`P` (capital) is the new "open PR" key.** Lowercase `p` is unbound at
  the top level, so an accidental keypress is now a silent no-op instead
  of an external `gh` invocation. Help text and the long-form `?` panel
  are updated.

## [0.14.0.3] - 2026-05-01 — Fix: e2e test cleanup symmetric with workspace removal

`Client.Kill` (production workspace removal) walks `/proc` for processes
whose cwd matches a pane cwd, catching `nvim --embed` children that
deliberately detach on launch. `KillServerAndReap` (used by every e2e
test's `t.Cleanup`) was missing that cwd-scan, it only collected the
pane process tree. Result: every e2e run leaked one `nvim --embed`
orphan to systemd-user. Stragglers piled up to ~3.5 GiB of zombie RAM
across two days of dogfooding before anyone noticed. The two paths now
share `cwdScanForReap`, so they cannot diverge again.

### Fixed

- **`KillServerAndReap` now reaps detached children.** Extracted the
  /proc cwd-scan from `collectPanePIDs` into a package-level helper
  `cwdScanForReap`. Both `Client.Kill` and `KillServerAndReap` call it.
  Behavior of `Client.Kill` is unchanged; `KillServerAndReap` gains the
  reap of detach-on-launch children that the pane-tree enumeration
  misses. Test suite leaks 0 `nvim --embed` orphans per full e2e run
  (was ~12).
- **Regression test for the symmetric path.**
  `TestKillServerAndReap_ReapsDetachedNvimEmbed` mirrors the existing
  `TestKill_*` test so future divergence between the two reap paths
  fails in CI.

## [0.14.0.2] - 2026-05-01 — Fix: lazy port base on freshly-initialized projects

`canopy init` registers a project with `port_base: 0` and lets `canopy new`
allocate the real base on first use. The lazy lookup wasn't honoring that
contract — it returned the stored zero verbatim, so `port.Allocate` was
asked to find a free port in the privileged range `[10, 999]` and
predictably came back with "no ports available." Newly-onboarded projects
looked broken on their very first `canopy new`.

### Fixed

- **Empty project entries now allocate on first `canopy new`.**
  `state.EnsureProjectBase` treats `PortBase == 0` as "not yet allocated"
  and falls through to the allocation path instead of returning the zero.
  The zero is also excluded from the `used` set so it doesn't shadow a
  real candidate. Existing projects with valid bases are unaffected.

## [0.14.0.1] - 2026-05-01 — README polish for public launch

The repo went public, so the README needed to read like an open-source
project landing page instead of a private build log. Mostly a copy and
structure pass, plus the first hero screenshot.

### Added

- **Hero screenshot.** `docs/images/tui-global.png` shows the Global tab
  with workspaces across four projects, embedded under the README
  tagline so visitors see the product before they read about installing
  it.
- **"Why canopy?" section.** Two sentences framing the parallel-agent
  use case, with a link to `docs/landscape.md` for the longer take.
- **CHANGELOG link** in the Documentation section.

### Changed

- **Status banner updated.** Was "pre-v0.1, in active development. Not
  yet usable end-to-end" — out of date by ten minor releases. Now reads
  "v0.14, daily-driven by the author."
- **Section order.** Features now come before Install (you should know
  what canopy does before you install it). Install leads with the curl
  one-liner; per-platform prereqs collapsed into a bullet list.
- **"What works today" → "Features".** Same content, less apologetic
  framing for a public README.

### Fixed

- **3-pane vs 4-pane confusion.** The README claimed both. Reconciled to
  3-pane (nvim, claude, shell), matching what `internal/workspace/
  lifecycle.go` actually creates — `scripts.run` is launched on demand
  via `canopy run`, not auto-started.
- **Stale `scripts.run` claim.** "Reserved for a future on-demand
  invocation" was true at v0.7 and false ever since. Replaced with a
  pointer to the current `canopy run` / `<prefix>r` flow.
- **Version examples** bumped from v0.12 / v0.13 to v0.14 throughout
  the verify and upgrade sections.

### Removed

- **Cravd Inc. copyright.** Replaced with personal copyright in
  `LICENSE` (MIT requires a named holder); README license line now
  just links to `LICENSE`.

## [0.14.0.0] - 2026-04-30 — Workspace hints: render-precedence overhaul (v0.14 closeout)

Closes the v0.14 workspace-hints expansion designed in
`docs/design/v0.14-workspace-hints.md`. With Lanes A/B/C all in
(`⚠ conflict`, `⚠ rebasing` / `merging` / `pick` / `detached`,
`⇡N` / `⇅`), the hint column had grown to seven distinct badges and
some combinations were noisy. This release applies the precedence
rule the design called out as the post-lanes follow-up.

### Changed

- **`stuck_state` preempts `git_stats`.** When a row carries a
  stuck_state hint (mid-rebase, mid-merge, mid-cherry-pick, detached
  HEAD), the renderer now suppresses the `↑N ↓N *N` git_stats badge
  for that row. The ahead/behind/dirty numbers reflect git's
  transient internal state during these operations — rebase rewrites
  HEAD, merge holds a partial index — so they're not signals the
  user can act on. The actionable signal is "finish that op first,"
  which `⚠ rebasing` (etc.) already conveys; the numbers were just
  noise.
- **Other badges keep rendering during stuck states.** rename_suggested,
  mergeability, push_state, pr_status, and shipped describe distinct
  facts about the branch (its name, its mergeability against main,
  its relationship to origin/&lt;branch&gt;, its PR state) that don't move
  under git's feet during a rebase, so they stay visible. Only
  git_stats is suppressed because it's the only badge whose numbers
  are computed off the unstable HEAD/index pair.
- **Defensive guard.** An empty `stuck_state.Message` (the detector's
  "no signal" contract) does NOT trigger the preempt — git_stats
  keeps rendering. Locked in by a regression test alongside the
  precedence test.

### Why a 0.14.0.0 milestone bump

The three preceding ships (0.13.3.0 Lane A, 0.13.4.0 Lane B,
0.13.5.0 Lane C) plus this precedence pass complete the v0.14
design end-to-end. The minor bump signals the milestone closeout
in the version stream so users (and `canopy upgrade --status`) can
see at a glance which release the design landed under. No breaking
changes; the bump is purely narrative.

### NOT in this PR (deferred to follow-ups)

- **Width-aware truncation.** The design doc proposed dropping
  low-priority badges when the row exceeds terminal width. That's a
  bigger change (renderer needs terminal width + a priority list +
  drop logic) and ships separately as a v0.14.1+ ergonomics pass.
  Tracked in TODOS.md.
- **stuck preempts push_state / mergeability.** Same reasoning could
  argue for hiding push_state and mergeability under a stuck_state
  preempt; rejected for now because both surface stable refs
  (origin/&lt;branch&gt;, origin/&lt;default&gt;) that don't move during a
  local rebase. Re-evaluate if dogfooding shows otherwise.

## [0.13.5.0] - 2026-04-30 — Workspace hints: ⇡N / ⇅ push state (Lane C of 3)

Third and final detector slice of the workspace-hints expansion
designed in `docs/design/v0.14-workspace-hints.md`. Surfaces the
question users actually ask 20× a day — *"is my work backed up on
origin?"* — which the existing `↑N` (ahead-of-default) badge does
NOT answer. Closes the three-lane series; the render-precedence
overhaul now becomes the v0.14 follow-up.

### Added

- **`⇡N` / `⇅` push_state badges.** A new `push_state` detector
  resolves the local branch's upstream tracking ref via
  `git rev-parse --abbrev-ref @{upstream}` and counts commits in
  each direction. Three shapes:
  - `⇡N` — local has N commits the upstream doesn't (action:
    `git push`).
  - `⇅` — local and upstream have both diverged (action:
    `git fetch && git push --force-with-lease`).
  - `⇡N` *with no upstream configured* — falls back to counting
    commits past the default branch and emits a `git push -u
    origin <branch>` action so first-push wires upstream cleanly.
    Same glyph; the action carries the meaning so the badge column
    doesn't sprout a separate symbol.
- **Cyan + bold style** (`pushStateStyle`) — distinct from the
  orange "warning" tier (mergeability, stuck_state). Push-state is
  informational-but-actionable: nothing is broken, but the user
  almost certainly wants to push. Different hue lets the eye scan
  a row of badges and tell at a glance which are "fix this before
  continuing" vs "remember to push."

### Changed

- **`detect.RunFast` now spans 7 detectors instead of 6.** The new
  detector is purely-local (one `rev-parse @{upstream}` plus two
  `rev-list --count` invocations on the upstream-known path; one
  fewer call when no upstream is configured). Well under 30ms per
  workspace; runs in the same parallel goroutine pool as the
  others.
- **Badge order** in `RenderHintBadges`: stuck_state → rename →
  mergeability → git_stats → **push_state** → pr_status / shipped.
  push_state sits right of git_stats so the "ahead of main" count
  reads next to the "is my work on origin" answer — different axes
  (origin/&lt;default&gt; vs origin/&lt;branch&gt;) but related divergence
  story.

### Why this matters

A PR-ready workspace can show `↑5 *0` (five commits past main, clean
working tree) AND `⇡5` (those same five commits not yet on
origin/&lt;branch&gt;) at the same time. Without push_state, the
existing `↑5` falsely implies "the work is on origin"; the user
discovers their commits aren't backed up only when their laptop
crashes or a teammate asks for the branch. The new badge collapses
that gap.

The render-precedence overhaul (where stuck_state would also
preempt git_stats and width-aware truncation kicks in) is the
remaining item from the v0.14 design doc and ships separately.

## [0.13.4.1] - 2026-04-30 — UX: clearer wording for the `R` keybind

### Changed

- **"retry scripts.setup" → "re-run setup" everywhere a user reads it.**
  The old wording was opaque on its own ("retry what?") and the
  prefix `scripts.` leaked an internal config path into copy meant
  for newcomers. The keybind, the broken-row hint in the help legend,
  the inline hint shown on a broken row, and the error message you
  get when you press Enter on a broken workspace all now say "re-run
  setup." Behavior unchanged; the keybind is still `R`. The CLI
  subcommand `canopy retry` is unchanged so muscle memory and any
  scripts pointing at it keep working.

This is a precursor to the larger workspace-actions menu designed in
TODOS.md (v0.15+) — that's where re-runnable per-project actions
(reseed, migrate, tail logs) get a proper home. Until then the
clearer wording does the user-facing work.

## [0.13.4.0] - 2026-04-30 — Workspace hints: ⚠ stuck state (Lane B of 3)

Second slice of the workspace-hints expansion designed in
`docs/design/v0.14-workspace-hints.md`. Catches workspaces in
mid-rebase, mid-merge, mid-cherry-pick, or detached-HEAD state —
all easy to forget when juggling parallel canopy workspaces and
all surfaced now as a leftmost orange badge in the hint column.

### Added

- **`⚠ rebasing` / `⚠ merging` / `⚠ pick` / `⚠ detached` badges.**
  A new `stuck_state` detector resolves the per-worktree gitdir
  via `git rev-parse --git-dir` (necessary because `<ws.Path>/.git`
  is a *file* in canopy worktrees, not a directory) and stats
  marker files git leaves during in-progress operations:
  `rebase-merge/`, `rebase-apply/`, `MERGE_HEAD`, `CHERRY_PICK_HEAD`.
  HEAD detached separately via `rev-parse --symbolic-full-name`.
  First-match-wins precedence; rebase preempts detached so an
  interactive rebase parking HEAD detached surfaces the more
  actionable signal.
- **Action hints per stuck state.** Each badge carries a continue/
  switch command (`git rebase --continue`, `git merge --continue`,
  `git cherry-pick --continue`, `git switch <branch>`) so the
  AGENT.md briefing and any future hover/popover surface a
  ready-made fix.
- **Leftmost row position.** The stuck_state badge sits before
  rename / mergeability / git_stats so the "finish your in-flight
  git op first" signal is the first thing the user reads when
  scanning across parallel workspaces.

### Changed

- **`detect.RunFast` now spans 6 detectors instead of 5.** The new
  detector is purely-local (a few stat calls + one `git rev-parse`),
  well under 10ms per workspace, and runs in the same parallel
  goroutine pool as the others.

Lane C (`push_state`: unpushed-to-origin separated from
ahead-of-main) ships from its own canopy workspace and arrives in
a subsequent version. The render-precedence overhaul (where
stuck_state would also preempt git_stats) lands separately once
all three lanes are in.

## [0.13.3.0] - 2026-04-30 — Workspace hints: ⚠ conflict (Lane A of 3)

First slice of the workspace-hints expansion designed in
`docs/design/v0.14-workspace-hints.md`. The TUI's hint column now
warns you BEFORE you try to ship that your branch would conflict
with main, so you fix it on your own clock instead of discovering
it at the worst possible moment.

### Added

- **`⚠ conflict` hint badge.** Every workspace gets a new
  `mergeability` detector that simulates `git merge origin/<default>`
  via `git merge-tree --write-tree` (no working tree side effects)
  and emits an orange-bold badge if the merge would conflict.
  Singular form is `⚠ conflict`; multi-conflict shows the count
  (`⚠ 3 conflicts`). Sits between the rename and git_stats badges
  so the warning reads next to the divergence counts that explain
  why.
- **Action hint for the agent.** The hint carries a suggested
  command (`git fetch origin && git rebase origin/<default>`) so
  the AGENT.md briefing and any future hover/popover have a
  ready-made fix to point at.

### Changed

- **`detect.RunFast` now spans 5 detectors instead of 4.** The new
  detector runs in the same parallel goroutine pool as the others;
  wall-clock for the full hint pass stays bounded. In steady state,
  most workspaces hit a `merge-base --is-ancestor` short-circuit
  (~10ms) and never call `merge-tree` at all. Diverged workspaces
  add ~30-80ms but don't extend the parallel pool's wall time.

This is Lane A of a three-lane series. Lanes B (`stuck_state`:
mid-rebase / mid-merge / detached HEAD) and C (`push_state`:
unpushed-to-origin separated from ahead-of-main) ship from their
own canopy workspaces and arrive in subsequent versions.

## [0.13.2.1] - 2026-05-01 — Fix: pill no longer offers a downgrade

### Fixed

- **Don't offer a downgrade when the cache is stale.** If you
  `make install`-ed a newer canopy outside the upgrade flow (e.g.
  from your `~/Work/canopy` clone), the auto-check cache could lag
  6h before noticing. During that window the pill used string
  inequality (`!=`) to decide whether to fire, so it would happily
  show `v0.13.2.0 ⇑ v0.13.1.0` — i.e. an offer to downgrade.
  Switched to integer-component semver ordering: pill only fires
  when running is strictly less than cached latest. `--status`
  output explicitly names the "running is AHEAD of cached latest"
  state for self-diagnosis.

## [0.13.2.0] - 2026-05-01 — Upgrade UX polish: confirm gate, force-refresh, hints

Round of polish on the v0.13 upgrade flow surfaced by first-day
dogfooding.

### Added

- **`canopy upgrade` confirmation gate.** The CLI used to barrel
  through and run shell immediately after printing the changelog,
  making it impossible to actually read what was about to change.
  Now it prompts `Apply this upgrade? [Y/n]` after the changelog
  preview. Pass `--yes` (or `-y`) to skip; non-interactive stdin
  (CI, pipes, redirects) auto-confirms; `--force` continues to
  imply yes since forcing IS the confirmation.
- **`r` in the TUI also force-refreshes the auto-check cache.**
  The 6h TTL means that if you ship a release outside canopy
  (`make install` from `~/Work/canopy`), the pill won't notice
  until the next TTL window. Pressing `r` now busts the upgrade
  cache too — same intent as the existing "I want truth right
  now" semantic for workspace state.
- **`canopy upgrade --status` includes manual escape hatches.**
  The status output now ends with three hint lines documenting
  `canopy upgrade --check` (force refresh), `canopy upgrade
  --dismiss` (silence pill), and the in-TUI `r` / `D` keys. Makes
  `--status` the single answer to "why isn't the pill doing what
  I expect?"

### Infrastructure

- `RunUnifiedOptions` gains `RefreshOnInit bool`. The auto-check
  closure is now wired unconditionally (when not DEV) so the `r`
  key can use it; `RefreshOnInit` separately gates whether Init()
  fires it on launch. Caller (route.go) sets it from the same
  `needsRefresh` flag that previously gated wiring at all.

## [0.13.1.0] - 2026-05-01 — `canopy upgrade --status` diagnostic flag

Adds a debugging surface for the v0.13 auto-check feature. Useful for
answering "why isn't the pill showing?" or "did my dismissal take?"
without grepping JSON.

### Added

- **`canopy upgrade --status`** prints the auto-check cache contents:
  cache file path, running version, build mode (DEV vs release),
  cached `latest_version`, `dismissed_version`, time since last
  successful fetch, TTL remaining (or "expired"), and the resulting
  pill state with the suppression reason when relevant. Works on DEV
  builds (DEV doesn't auto-check, but inspecting the cache file is
  still useful diagnostically). Pure read — no network, no shell.

## [0.13.0.0] - 2026-04-30 — Upgrade UX overhaul

Canopy now tells you when there's a newer version, and lets you upgrade
without leaving the TUI.

### Added

- **Auto-check pill in the top bar.** When a newer canopy is available,
  the version pill mutates from `v0.12.3` to `v0.12.3 ⇑ v0.13.0` (yellow
  arrow). Synchronous read on TUI startup keeps launch zero-latency on
  cache hits; stale cache triggers an async refresh that lands as the
  TUI is already on screen, so the network never blocks rendering.
- **In-TUI upgrade flow (press `U`).** Four states: loading the
  changelog from GitHub, preview with the changelog rendered in a
  scrollable viewport (`j`/`k`/PgUp/PgDn/g/G), running with live-tailed
  `git pull` + `make install` output, done with success or stderr-tailed
  failure. Esc cancels pre-run; Ctrl-C cancels the running subprocess
  but stays in the flow so you can read what happened.
- **`canopy ls` hint line.** When an upgrade is available, plain `canopy
  ls` output ends with one dim line: `canopy v0.13.0 available — run
  canopy upgrade`. Pure cache read, never blocks.
- **Per-version dismissal.** Press `D` in the TUI or run `canopy upgrade
  --dismiss` to silence the pill until a newer version ships. Dismissal
  is automatic on the next release because the cache compares
  per-version, not by snooze timer.
- **Post-success restart hint.** The doneOK screen now reads "This
  canopy session is still running the old binary. Press q to quit, then
  re-run canopy to use v0.13.0." Linux/Mac keep the running inode alive
  after `make install` replaces the file; new invocations get the new
  binary, but the active TUI does not.
- **Global Ctrl-C in the upgrade flow.** Loading and preview states
  used to silently absorb Ctrl-C, breaking the convention that Ctrl-C
  always quits canopy. Now Ctrl-C quits from any pre-run state and
  cancels the subprocess from the running state.

### Changed

- `canopy upgrade --check` now writes the fetched VERSION into the
  auto-check cache so the next non-check invocation sees fresh data
  without re-fetching. Existing dismissed_version is preserved across
  --check.
- Successful `canopy upgrade` rewrites the cache to mark the running
  version as latest, with dismissal cleared. Pill disappears
  immediately instead of waiting for the next 6h refresh.

### Infrastructure

- New `~/.canopy/upgrade-check.json` cache (3 fields: `checked_at`,
  `latest_version`, `dismissed_version`), 6h TTL, atomic tempfile+rename
  writes. Mirrors the `state.json` pattern.
- New public types in `internal/ui` for the closure boundary between
  `cmd/canopy` and the TUI: `UpgradeRefreshFn`, `UpgradeChangelogFn`,
  `UpgradeShellFn`, `UpgradeDismissFn`. Keeps unexported types
  (`safeBuffer`) inside the package and lets `cmd/canopy` supply
  network/shell layers without import cycles.
- `RunUnified` signature gained 5 params for the upgrade closures
  (`initialUpgrade`, `refreshFn`, `changelogFn`, `shellFn`, `dismissFn`).

Design doc committed at `docs/design/v0.13-upgrade-ux.md`.

## [0.12.4] - 2026-04-30 — Revert org move: `oncactus/canopy` → `avinashjoshi/canopy`

Reverses v0.6's transfer of canopy to the `oncactus` org. Repo is back at
`github.com/avinashjoshi/canopy` for now while the brand strategy settles.
Mechanical change only — no behavior diff. Module path, every Go import,
README install one-liner + badges, install.sh, CHANGELOG compare links,
issue templates, and CLAUDE.md all repointed. Existing oncactus URLs keep
working via GitHub's transfer-redirect.

The historical migration plan at `docs/design/v0.6-org-move.md` and the
TODOS.md decision context are intentionally preserved verbatim — they
describe a past decision, not a current one. Contributor email
`avinash@oncactus.com` is also untouched.

### Changed
- Module path: `github.com/oncactus/canopy` → `github.com/avinashjoshi/canopy`.
  All Go imports, internal package references, and user-facing strings
  in `cmd/canopy/use.go` updated to match.
- README.md install one-liner, Go Reference badge, Go Report Card badge,
  Tests workflow badge, and `go install` examples now point at the
  avinashjoshi address.
- `install.sh` clones from `https://github.com/avinashjoshi/canopy`.
  `canopy upgrade` reads `VERSION` from
  `raw.githubusercontent.com/avinashjoshi/canopy/main/VERSION`.
- CHANGELOG.md compare links and release-tag links rewritten so the
  "see diff since last release" link in each entry now resolves.
- `.github/ISSUE_TEMPLATE/config.yml` + `feature.yml`, `CONTRIBUTING.md`,
  `docs/getting-started.md`, `docs/troubleshooting.md`, and `CLAUDE.md`
  all repointed.
- Local `git remote set-url origin` flipped on the source clone, which
  applies to every canopy worktree automatically.

## [0.12.3] - 2026-04-30 — Surface version in `canopy use` and `canopy --help`

Two small UX gaps closed: "which canopy am I running" should never
take an extra step. The release version now surfaces in both the
`canopy use` listing (per-target column) and at the top of `canopy
--help`, so the answer is always one command away — no more chasing
it through `canopy version`.

### Added
- `canopy use` listing gains a VERSION column. The release row execs
  `canopy.bin version` once and parses the version label; dev workspace
  rows show "DEV" without forking (make build is dev-by-convention).
  Missing binaries show "—". Capped at 2s per release exec so a wedged
  binary can't hang the listing.
- `canopy --help` leads with a one-line version banner — `canopy
  v0.12.2+abc1234` for releases, `canopy DEV (workspace-name)` for
  dev builds inside a known worktree, plain `canopy DEV` otherwise.
  Reuses the existing `versionDetails()` plumbing; one extra resolve
  at process start.

## [0.12.2] - 2026-04-30 — `make build` DEV banner restored + TODOs queued

The v0.12.1 squash merge dropped two commits that were pushed to the
PR after the squash. v0.12.2 lands them: the dev-sentinel preservation
fix (so `make build` binaries register as DEV again) plus the TODO
entries for the next round of UX work.

### Fixed
- `make build` binaries now correctly register as DEV. The v0.12.0
  versionDetails() BuildInfo fallback was overwriting the literal
  "dev" sentinel with Go's pseudo-version (e.g.
  "v0.0.0-20260430201846-6f65463") for in-repo builds, which tripped
  IsDev to false. That killed the cyan DEV pill in the TUI, killed
  the [DEV:branch] suffix in the tmux statusline, and made `canopy
  upgrade` cheerfully offer to upgrade dev binaries instead of
  refusing. Fixed by capturing rawVersion before the fallback runs;
  IsDev now reads from rawVersion while d.Version can still surface
  the pseudo for forensic display.

### Documentation
- TODOS.md gains two new entries: a P2 "surface version in canopy
  use and canopy --help" quick-win pair, and a P3 "upgrade UX
  overhaul" with sub-items for auto-check, TUI flow, and integration.
  Design questions are noted on the P3 sub-items because the right
  shape isn't obvious yet (cadence, storage, surface, dismissal).

## [0.12.1] - 2026-04-30 — `canopy upgrade` falls back to git for private repos

`canopy upgrade` was hitting `HTTP 404` on the VERSION fetch because
canopy's repo is private and raw.githubusercontent.com 404s anonymous
requests for private content. Fixed by adding a git-based fallback:
when HTTP fails, run `git fetch origin main && git show origin/main:VERSION`
in the local `~/.canopy/src` clone. The clone has working auth (that's
how install.sh succeeded), so the git path always works regardless of
repo visibility.

### Fixed
- `canopy upgrade` now works for private repos. The HTTP path stays as
  the fast-path for public repos; git is the fallback for private. When
  the repo eventually goes public, the HTTP path takes over automatically
  with no code changes needed — strictly faster, no git fetch overhead.
- Same fallback applies to the optional CHANGELOG preview before the
  pull, so users on private repos still see "What's new" diffs.

### Added
- `upgradeGitFetchFile` package-level var as the git path's testability
  seam (mirrors `upgradeFetchVersion`'s shape). Tests for both happy-
  path-via-git and dual-failure scenarios.

## [0.12.0] - 2026-04-30 — Source-based install, `canopy use` switcher, and visible release/DEV indicators

End-user install + dev-loop ergonomics. One curl line installs canopy from
source. `canopy upgrade` keeps it current. `canopy use` flips the active
binary between any feature build and the released one in milliseconds.
Three visual indicators (TUI top bar, tmux statusline suffix, `canopy
version`) make it impossible to forget which canopy is active.

### Added
- `install.sh` at the repo root, hosted via raw.githubusercontent.com.
  Detects OS via `uname`, checks for git/tmux 3.2+/Go 1.22+/make and
  prints the exact install command if any is missing, clones to
  `~/.canopy/src`, and runs `make install`. Idempotent: re-running on
  an already-installed machine prints "run canopy upgrade" and exits 0.
  One-liner: `curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh`.
- `canopy upgrade` — fetches `~/.canopy/src` to latest main and runs
  `make install`. Reads VERSION from raw.githubusercontent.com and
  string-compares with the running binary, prints CHANGELOG diff before
  the pull. `--check` compares without upgrading; `--force` runs even
  when versions match. Refuses cleanly on dev binaries (suggests
  `canopy use release` first) and on missing/corrupt source clones.
- `canopy use [target]` — single switcher subcommand for the active
  binary. With no args, lists current target + every workspace canopy
  knows about with built-or-not status (sorted alphabetically). With a
  target name, retargets `~/.local/bin/canopy` symlink atomically:
  `release` (alias: `main`) points at `canopy.bin`; a workspace name
  points at that worktree's `./canopy`. `--build` flag rebuilds the
  workspace binary first.
- `make dev` Makefile target — thin wrapper that builds this worktree's
  `./canopy` (no ldflags, so version stays "dev") and flips
  `~/.local/bin/canopy` at it. Works from any worktree.
- `make release` Makefile target — flips `~/.local/bin/canopy` back at
  `canopy.bin` from any worktree, with no rebuild. Refuses cleanly if
  `canopy.bin` is missing.
- VERSION file at the repo root holding the human-curated semver
  (matches gstack/gbrain convention). `canopy upgrade` and the
  Makefile's ldflags both read from it.
- TUI top-bar version pill — muted gray for release builds (`v0.12.0+abc1234`),
  cyan for dev builds (`DEV: <workspace>`). Suppressed when no version
  info is available so tests + bare invocations stay clean.
- `canopy statusline --format=current` appends `[DEV:<workspace>]` to
  the workspace segment when the running canopy is a dev build, regardless
  of which workspace's tmux session the user is in. The "I forgot I
  switched and this isn't the released canopy" reminder.
- `canopy version` now prints a structured multi-line block: version,
  commit, build date, resolved binary path (with symlink target), mode
  (release/DEV), and workspace name when running a dev build inside a
  known worktree.

### Changed
- **`canopy install tmux` now invokes bare `canopy` (PATH-resolved)
  instead of the absolute path of the binary that ran the install.**
  With `canopy use` swapping the symlink between release and dev
  binaries, bare `canopy` follows the symlink automatically — no
  `canopy install tmux --force` re-run on every binary swap.
- `make install` now writes to `~/.local/bin/canopy.bin` and symlinks
  `~/.local/bin/canopy` at it. `canopy use` flips the symlink. The
  symlink is the single source of truth for the active canopy on PATH;
  every other tool (tmux, shell aliases, `canopy upgrade`) follows it.
- `make install` injects ldflags from VERSION + git short-sha so
  `canopy version` produces a real version string for installed binaries
  without needing goreleaser. `make build` and `make dev` deliberately
  skip the ldflags so dev binaries surface as `version == "dev"` and
  the DEV banner fires.

### Documentation
- README: rewritten Install section (curl one-liner first), new Update
  section (`canopy upgrade`), new Dev workflow section (`canopy use`,
  `make dev`, `make release`, visual indicators), new Uninstall section.
- CLAUDE.md: new "Multi-workspace dogfooding" section instructing future
  Claudes to use `./canopy` or `/tmp/canopy-XX` per-workspace and never
  bare `canopy`, with rules for routing "make this active" requests to
  `canopy use` or `make dev`.
- Design doc: `docs/design/v0-install-and-dev-workflow.md` formalizes
  the source-based distribution decision (Approaches A/B/C compared),
  filesystem shape, switching state machine, and failure modes.

### Why source-based instead of goreleaser + binary releases
Canopy's audience is devs comfortable with shells; they have or can
install Go in one line. Source-based distribution matches gstack/gbrain
(the user's existing tooling), avoids `.goreleaser.yaml` + release
workflow + Gatekeeper notarization + cross-platform build matrix, and
already matches the user's "make install on main" dogfood habit. If
non-Go users ever materialize, goreleaser layers on top of this; doesn't
replace it.

## [0.11.3] - 2026-04-30 — Detach-and-remove now covers fullscreen + instant state-row drop

### Fixed
- Deleting the workspace you launched fullscreen `canopy` from no longer
  strands on-disk + state cleanup mid-Remove. The v0.11.2 detach +
  detached-subprocess shortcut was scoped to popup mode; in fullscreen,
  `escapeIfDeletingCurrent` would fall through to `busyMode + removeCmd`
  with canopy still hosted in a pane of the doomed session — when tmux
  Kill fired, canopy died and the rest of `Manager.Remove` (git worktree
  remove, branch delete, state-row drop) never ran. The path now flips
  on a (root, name) match against `currentWorkspace` regardless of mode,
  spawns the same detached `canopy rm --yes --force <name>` subprocess,
  runs `tmux detach-client`, and `tea.Quit`s. The user lands at the
  shell that started tmux; cleanup finishes in the background. When
  canopy was launched from a non-tmux shell, detach-client errors
  harmlessly and the rest of the sequence still works.
- `Manager.Remove` drops the state row up front (under the lock,
  immediately after snapshotting the row) instead of last. Reopening
  canopy in the gap between popup-close and the detached subprocess
  finishing no longer shows the just-deleted workspace as a stale row,
  and pressing enter on it no longer surfaces "tmux session not found."
  Steps 2-6 (archive, tmux kill, git remove, branch delete, log
  cleanup) work entirely off the in-memory snapshot so they don't need
  the state row to still exist. Failures after the drop leave residue
  on disk that `canopy reconcile` discovers — strictly better UX than
  a zombie row that lies for several seconds.

### Changed
- Renamed `deletingCurrentInPopup` → `deletingCurrentSession` and
  `popupDetachAndRemoveCmd` → `detachAndRemoveCmd` to reflect that the
  shortcut is no longer popup-specific.

## [0.11.2] - 2026-04-30 — Popup delete: detach instead of auto-switch

### Fixed
- Deleting the workspace you opened the popup from no longer auto-builds and
  switches your tmux client to the project main session — a flow the user
  perceived as "tmux loaded a random session" because EnsureMainSession spun
  up a fresh nvim+claude pane behind the popup. The popup now closes
  immediately, the tmux client detaches back to whatever shell started tmux,
  and the workspace cleanup runs in a detached `canopy rm --yes --force`
  subprocess (Setsid + Process.Release) so it survives the popup's exit.
  Other delete paths (deleting a different workspace from the popup, or
  deleting from fullscreen) keep using the existing busyMode + escape flow.
- `removeDoneMsg` now auto-dismisses busyMode on success instead of leaving
  the popup sitting at "Removed." waiting for a keypress. Mirrors
  `createDoneMsg`'s "drop the busy view as soon as the work is done"
  pattern. Failure path is unchanged — busyMode stays so the captured
  archive output and error are visible for diagnosis.

### Added
- `tmux.Client.DetachClient(ctx)` — wrapper around `tmux detach-client` for
  the popup-delete escape path.

## [0.11.1] - 2026-04-30 — TUI UX fixes round

### Fixed
- `o` (focus project) now works on the already-selected project — re-focusing
  is a harmless tab switch, no reason to disable the keybind. Removed the
  same-project guard. Also short-circuits the canopy.json reload in that
  case so a transiently unreadable config doesn't surface a spurious error.
- Deleting the workspace whose dir hosts the running canopy popup no longer
  strands the tmux client. Before kill-session runs, canopy ensures the
  project's main session is up and `switch-client`s the user there. Match is
  on (project root, name) so cross-project name collisions (A/foo and B/foo)
  don't trip the wrong escape target.
- Popup picker now pre-selects the workspace whose directory hosted the
  popup invocation, instead of always landing on row 0. The latch fires on
  the first non-empty refresh — early empty probes don't burn the preselect
  opportunity, and a missed match (target filtered out by tab) still latches
  so later refreshes don't yank the cursor.
- New workspaces (and resurrections) now land active focus on the Claude
  pane instead of the nvim pane — the agent is what you typed `n` to talk
  to, the cursor should be there waiting.
- `n` from the Global picker no longer panics with a nil pointer dereference.
  Three `attachCmd(m.mgr, ...)` callsites that crashed when `m.mgr` was nil
  (canopy launched outside any project) now route through the existing
  `attachOrSwitch` helper, which uses the always-set `m.tc` directly.

### Added
- "← here" indicator on the workspace row whose directory hosts the running
  canopy invocation. Cyan + bold so it stays visible even when you navigate
  the cursor away from the auto-preselected row.

## [0.11.0] - 2026-04-30 — Cross-project new workspace + picker chrome polish

`n` (new workspace) now works from the Global tab. Press `n` on any row and
canopy creates a workspace in that row's project, mirroring how `d`/`R`/`K`
already follow the cursor cross-project. The two-step `o` then `n` ritual is
gone; one keystroke does what one keystroke should.

To make cross-project intent unmissable, every screen in the new-workspace
flow (variant picker, fresh / PR / issue / branch sub-modals, busy view)
now leads with a "creating in `<project>`" banner — brand-violet chip,
repo path beside it. The chip carries through to the success line:
"Workspace created in cravd."

The new flow's chrome also got pulled into the rest of the TUI's visual
vocabulary. Variant picker letters render as the same key pill the help
line uses, dropping the red-error-coded brackets they used to wear.
PR/issue/branch sub-modals lost their bespoke filter input in favor of
the main TUI's `🔍 FILTER` pill with the same `▏` caret. Cursor caret
unified across the variant picker and all three sub-modals to match the
main workspace list's `❯` marker — one indicator per cursor position,
TUI-wide.

### Added

- **Cross-project `n` from the Global tab.** `availableNewWorkspace` now
  fires when the cursor row has a non-empty `ProjectRoot`. The action
  resolves the target project via `managerForRow(cursor)` (the same
  primitive `d`/`R`/`K` already use), surfaces config-load errors via
  `m.err` rather than panicking, and stores the resolved Manager + name
  + root in three new model fields (`newTargetMgr`, `newTargetRoot`,
  `newTargetName`) used by every submit + loader in the flow.
- **Project banner on every new-workspace screen.** A new
  `renderTargetBanner` helper emits "creating in" + brand-violet
  rounded chip + dim project root. Injected at the top of
  `renderNewPicker`, `renderNewFresh`, `renderNewPR`, `renderNewIssue`,
  `renderNewBranch`, and the busy view's create branch.
- **Project name in the create success line.** "Workspace created in
  `<project>`." closes the loop the banner opened. Empty project name
  falls back to the legacy generic line.

### Changed

- **Variant picker shortcut letters** render as `keyPillStyle` (the
  inverted-bg pill the help line uses), no longer wrapped in `[ ]` and
  no longer borrowing `brokenStyle`'s error red. Pill chrome implies
  "press this" — same convention the rest of the TUI already used.
- **PR / issue / branch sub-modal filters** replaced their bespoke
  "PR number or filter:" / "Filter:" labels with the main TUI's
  `🔍 FILTER` pill (`searchLabelStyle` + `searchInputStyle`).
  Per-modal guidance moves to a dim hint to the right of the pill,
  visible only when the value is empty. Caret renders as `▏` matching
  the main TUI's search line, since `bubbles/textinput.View()`'s block
  cursor fights canopy's vocabulary; we read `Value()` and compose the
  caret manually.
- **Cursor caret unified to `❯`** across the variant picker (was `>`)
  and all three sub-modals (was `●`), matching the main workspace
  list's selected-row glyph.
- **`branchInWorkspace`** scope check now follows the in-flight
  new-flow target when set, so the "(in workspace X)" tag in the
  PR/branch picker reflects the target project — not the launch
  context.

## [0.10.1] - 2026-04-30 — Fix: `r` now refreshes PR status

Pressing `r` (refresh) busted the in-memory RSS/CPU cache but silently
ignored the 10-minute pr_status cache, so users who just merged a PR
or pushed a review change kept seeing the stale "PR #142 awaiting
review" hint until the TTL expired. `r` now busts both — same intent,
both freshness gates.

Background ticks and reconcile keep the 10-minute TTL to stay inside
the GitHub API budget. Only deliberate user action (`r`) invalidates.

### Fixed

- **`r` now invalidates the pr_status cache.** `actionRefresh` calls
  the new exported `lifecycle.ResetPRStatusCache` alongside the
  existing `memCache.InvalidateAll`. Regression test in
  `internal/ui/effective_status_test.go` seeds the cache via
  `RunFast`, presses `r`, and asserts the cache is empty.

## [0.10.0] - 2026-04-30 — Workspace health, diagnostics, and machine-load visibility

A complete pass on workspace health and observability. The TUI list page
now shows tmux liveness, attached-client state, memory and CPU usage per
workspace, and per-row git stats (commits ahead/behind, dirty file count)
at a glance. Pressing `i` opens a read-only diagnostic drawer with the
process tree, recent log entries, and last setup-script output for the
selected workspace. `K` kills a workspace's tmux session in place.
Pressing Enter on a row whose tmux session was killed externally now
resurrects automatically instead of erroring.

The drawer is a drawer, not a dashboard. Read-only. No live tailing,
no editing. The scope cap is the load-bearing constraint.

### Added

- **List-page row glyphs** distinguish three session states at a glance:
  `⊙` (green) — tmux alive AND a client is attached, `○` (subtle grey)
  — tmux alive but no client attached, blank — no tmux session (status
  column says why).
- **Mem + CPU column** in the TUI list. Format `320M 12%` combining
  RSS summed across the process tree in every pane with summed CPU%
  (sum of `pcpu` per process; can exceed 100% on multi-core boxes).
  Heat-colored: amber > 500 MB or > 50% CPU, red > 2 GB or > 200% CPU.
  Auto-loads on first render via Bubbletea's async tea.Cmd — no `r`
  keypress required. Cached per-session for 5 s; invalidated on `K`
  (kill) so a just-killed row's column flips to `—` immediately rather
  than lagging the actual state by up to TTL seconds. CPU shown
  consistently (including `0%`) so the cell format never varies — the
  alternative "omit if < 1%" rule conflated "no data" with "idle."
- **`K` keybind** kills the highlighted workspace's tmux session with
  y/N confirm. State.json row, worktree dir, branch all survive —
  re-pressing Enter resurrects via `Manager.Resurrect`. Works on main
  rows too: kills the project's main session; Enter rebuilds via
  `EnsureMainSession`. Refused only on rows with no live session.
- **`i` keybind** opens a diagnostic detail drawer for the highlighted
  row. Shows: process tree with RSS/CPU per pane, last 20 entries
  from the per-workspace log, last `scripts.setup` output, env (port,
  paths, branch), tmux-alive + attached state. Read-only. `Esc`/`q`
  closes, `r` reloads, `b` opens a bare attach.
- **`b` keybind in the drawer** opens a one-pane shell at the row's
  directory with `CANOPY_*` env vars set, no auto-running claude/nvim,
  no `scripts.setup` rerun. For workspace rows: drops into the worktree
  path. For main rows: drops into the project root. Subsumes the v0.5
  `canopy debug` TODO. Hidden from the drawer footer except for
  broken workspaces and main rows (where it adds value over plain
  Enter); for running/stopped workspaces it's redundant clutter.
- **`Manager.BareAttach` and `Manager.BareAttachMain`** create a
  `<session>-debug` tmux session at the row's directory.
- **Per-workspace logs** at `~/.canopy/log/canopy-<workspace>.log` via
  a new `clog.fanoutHandler`. slog records carrying a `name` attribute
  are tee'd to both the global `canopy.log` and the workspace's own
  log. The drawer reads the per-workspace file directly. The handler
  shares a `*sinkRegistry` across all derived (`WithAttrs`/`WithGroup`)
  clones so there's exactly ONE lumberjack writer per workspace name
  regardless of how many `slog.With(...)` chains touch it.
- **Per-workspace setup logs** at `~/.canopy/log/setup-<workspace>.log`.
  `scripts.setup` output is captured to disk via `io.MultiWriter`
  alongside the existing live stream. The drawer surfaces it so a
  `broken` workspace tells you why on `i`.
- **Git stats badge** on every workspace row when non-zero: `↑3 ↓1 *5`
  for commits ahead of main / commits behind main / files with
  uncommitted changes. Shows alongside PR badges, not under them —
  they're complementary signals ("is this done?" vs "what's in flight
  now?"). Zero counts hidden so clean rows stay quiet.
- **Help legend (`?`)** documents every glyph and badge in one place:
  presence indicators, status column, Mem/CPU column, right-side
  badges. Reading the help once should be enough to scan rows without
  guessing.
- `tmux.PaneInfos`, `tmux.SessionLoad`, `tmux.SessionAttached`, and
  `tmux.AttachedSessions` for process-tree probing and tmux-attach
  detection.
- `state.MemCache` (caches both RSS and CPU per session, TTL +
  invalidate), `state.LoadProbe`, `state.GlobalRow` extended with
  `MemRSS`, `CPU`, and `Attached` fields.

### Fixed

- **`nvim --embed` orphan leak on every kill.** When `Tmux.Kill(session)`
  ran, the tmux session died but `nvim --embed` children — which
  interactive `nvim .` forks with deliberate session-detachment so
  they can outlive their launcher — got reparented to PID 1 and lived
  on forever, idle. Over a 4-day workstation uptime running canopy
  tests, this accumulated ~50 zombie processes eating ~1.5GB of RAM.
  `Tmux.Kill` now snapshots the pane process tree AND scans
  `/proc/*/cwd` for processes whose cwd matches a pane's cwd AND whose
  `comm` matches a known target list (currently just `nvim`), then
  SIGKILLs the union after `kill-session`. The image-name gate is
  load-bearing: a bare cwd match would SIGKILL anything happening to
  live at the workspace path, including canopy itself when launched
  from a workspace pane via the popup keybind. Regression test in
  `internal/tmux/reap_test.go`. Same reap added to
  `Tmux.KillServerAndReap` for test cleanup.
- **Stale-ready Enter bug.** Pressing Enter on a workspace row whose
  tmux session was killed externally (e.g., `tmux kill-session -t ...`
  from another terminal) used to fail with `tmux.AttachCmd(...): tmux:
  session not found`. The Enter handler now re-checks `row.Alive`
  freshly probed by `BuildGlobalRows` and routes stale-ready rows
  through `Resurrect` automatically. Regression test in
  `internal/ui/effective_status_test.go`.
- **`var log = clog.Pkg(...)` package-init pattern broke the per-workspace
  fan-out.** Go's package-init order runs `var log = clog.Pkg("name")`
  at file scope BEFORE `main()` runs `clog.Init()`. The previous
  implementation snapshotted `slog.Default()` at `Pkg()` call-time,
  freezing the binding to the pre-Init stderr handler — so no
  package's `log.Info(...)` ever reached the fan-out wired up later
  in `Init()`, and no `canopy-<ws>.log` files were ever created in
  production. `Pkg` now returns a forwarding handler that resolves
  `slog.Default()` on every record. Regression test verifies fan-out
  reachability when `Pkg` is called before `Init`.
- **`fanoutHandler.WithAttrs`/`WithGroup` writer duplication.** Each
  derived handler had its own `writers` map; two `slog.With(...)`
  chains for the same workspace would open two `lumberjack.Logger`
  instances at the same file path, racing on rotation and leaking file
  descriptors at teardown. Hoisted writers into a shared
  `*sinkRegistry` so all derived handlers see exactly one lumberjack
  per workspace name.
- **`shipped` detector false positive on fresh forks behind main.**
  When a workspace branch had zero unique commits and main had
  fast-forwarded past the branch's start, the merge-commit-style
  ancestor check returned true and the badge fired "✓ shipped (local)"
  on actively-in-progress branches. Detector now uses squash-merge
  detection only (via `git cherry`); the merge-commit case is
  ambiguous from current git state alone with the fresh-fork case, so
  we don't claim shipped without recording the branch base commit
  (future work). Squash-merge workflows (the default `gh PR merge`
  path) keep working. Regression test:
  `TestDetectShipped_FreshForkBehindMain`.
- **Row position shifted right on selection.** Selected rows had a
  `❯ ` caret + presence-glyph (4 chars before name) while non-selected
  rows had only the presence glyph (2 chars), so moving the cursor
  onto a row visually shifted everything 2 columns right. Both branches
  now share the same 4-char prefix; columns stay rock-still as the
  cursor moves. Regression test:
  `TestRender_NameColumnAlignmentDoesNotShiftOnSelection`.
- **`killDoneMsg` cache invalidation lied.** Comment claimed the load
  cache was invalidated for the killed session; code didn't actually
  call `Invalidate`. After K, a later resurrect on the same row
  served up to TTL seconds of stale RSS/CPU from the dead session.
  Now plumbs the session name through `killDoneMsg` and invalidates
  on receipt.
- **Drawer cross-project name collision.** The drawer's stale-load
  guard checked `forName` only, not `forRoot`. In the global TUI,
  opening drawers in quick succession on `feature-a` (project-A) →
  close → `feature-a` (project-B) could land project-A's data in
  project-B's drawer if the load arrived after the switch. Guard now
  matches on `(forName, forRoot)`.

### Changed

- **Status column vocabulary unified across workspace and main rows.**
  Workspace `ready` rows display as `running` (matching what main rows
  already said). Stale-ready rows (recorded `ready` but freshly-probed
  `Alive=false`) display as `stopped` instead of lying with `ready`.
  `setting_up` renders with a space (`setting up`). Pure display-layer
  changes — `state.Status` enum values are unchanged.
- **Status glyph helper now matches the displayed status.** Previously
  the row glyph was driven by raw `state.Status`, so stale-ready rows
  rendered as `(blank) stopped` (ready glyph + stopped text) — visually
  inconsistent. The new `displayGlyph(row)` mirrors `displayStatus(row)`
  so glyph and text always agree.
- **`shipped` badge label.** Renamed `✓ shipped (local)` → `✓ merged`
  to reflect what we actually detect (commits in the default branch
  via squash-merge), not "production deployment." The "(local)"
  qualifier was an implementation detail of the detector that didn't
  add user-facing value.
- **Main rows are first-class for lifecycle ops.** Reified the
  principle that `(main)` rows *are* workspaces for session-lifecycle
  operations (kill tmux, inspect, bare attach, attach) and *aren't*
  for identity operations (delete, retry-setup, new). `K`, `i`, and
  `b` now work on main rows. `d` (delete) and `R` (retry) stay refused
  on main — those operate on workspace identity, which main doesn't
  have.
- **`isErrSessionNotFound` uses `errors.Is`** instead of substring
  matching. The "import cycle risk" the comment alluded to never
  existed: the ui package already imports tmux for `*tmux.Client`.
  Substring matching would have silently broken if tmux ever rephrased
  the error or internationalized it.

### Cleanup

- `canopy rm <ws>` now removes the per-workspace log file
  (`canopy-<ws>.log`) and setup log (`setup-<ws>.log`) alongside the
  state row and worktree.

## [0.9.1] - 2026-04-29 — Force screen no longer fires after PR auto-merge

When GitHub auto-deletes the remote branch on PR merge (the default
"Automatically delete head branches" flow), `canopy rm` no longer shows
the red force-delete screen warning about "unpushed commits." The
local commits past `origin/main` are work that already landed in
squashed form before the branch was retired — flagging them as
data-loss risk was the bug. The new check also covers the case where
`gh` is unavailable, unauthenticated, or rate-limited, so a missing
gh signal alone can't trigger a false force-screen.

### Fixed
- **`canopy rm` no longer false-positives on auto-deleted branches.** The safety preflight now treats "upstream was tracked, remote-tracking ref is now gone" as a "merged" signal, alongside the existing `gh pr view → MERGED` check. Either signal is sufficient. Reads `branch.<name>.{remote,merge}` directly from `.git/config` rather than `@{u}`, since `@{u}` itself fails once the remote ref is pruned — which was hiding exactly the state we needed to detect.

## [0.9.0] - 2026-04-29 — TUI polish pass

A round of usability fixes and visual polish on top of the v0.8 unified TUI.
The popup and CLI verbs both correctly resolve project context when invoked
from inside a workspace dir, branch renames flow through to the TUI on the
next refresh, and pressing enter on a dead main row brings it up instead of
erroring. The list itself reads cleaner: branch icon, gray-styled main
branch, status column carries alive-state directly (no separate dot), and
selected rows wear a `❯` caret.

### Added
- **Branch icon (`⎇`)** prefixes every Branch column entry — single Unicode cell, no Nerd Font dependency.
- **Default branch shown on (main) rows.** `state.GlobalRow.Branch` for main rows now carries the project's actual default branch (origin/main or origin/master) instead of the `—` placeholder. Rendered in gray to signal "informational, not actionable."
- **Main row status reflects alive state.** Instead of always reading `main`, the (main) row's status column now shows `running` (green) when the main session is up and `not started` (gray) when it isn't. The standalone alive-dot column is gone — status carries that info.
- **Tab auto-focuses from Global view.** When canopy is launched outside any project (currentProject is empty) and the user presses Tab to switch to Local, the cursor row's project becomes the new Local context — Tab acts as "enter the project I'm looking at."
- **`p` keybind opens the workspace's PR.** Runs `gh pr view --web` from the workspace dir. Hidden when no `pr_status` hint exists, so the help line stays clean.
- **PR check status in the hint badge.** OPEN PRs now also surface CI status — `✓ checks`, `✗ checks`, or `… checks` next to the existing PR-state badge. Sourced from `gh pr view --json statusCheckRollup`.
- **Auto-detach other clients on attach.** Switching to a workspace from a second terminal now kicks the first terminal off the session by default, so two clients don't mirror keystrokes on the same tmux session. `CANOPY_NO_DETACH=1` opts back into tmux's default multi-client behavior.
- **`Manager.EnsureMainSession`.** Idempotently brings up the project's main tmux session. Used by both `canopy main` (CLI) and the TUI's enter-on-main-row path.
- **`git.CurrentBranch`** helper for reading a worktree's current branch.

### Changed
- **Selected-row caret `>` → `❯`.** Reads more cleanly at terminal weights.
- **Project headers + "add a project" flush-left.** Was indented at column 2; now at column 0 to outdent visually from the workspace rows below (column 2). Blank line between the "add a project" hint and the help line so they don't blur together.
- **Friendlier "no canopy.json" error.** Running `canopy main` or `canopy new` outside a project now reads `this directory isn't a canopy project yet — run` `canopy init` `here (or cd into one of your existing canopy projects)` instead of leaking the internal walk-up error verbatim.
- **Cobra usage dump suppressed on RunE errors.** `SilenceUsage: true` on the root command so failures land as a one-line error instead of a flag-table wall after the message.
- **Smarter delete safety.** `safetyChecks` now reads PR state once up front and skips the "unpushed commits" warning when the PR is `MERGED` — those commits are squashed/rebased into main, not lost work. Open-PR warning only fires for `OPEN` state.
- **Reconcile refreshes branch.** `Manager.Reconcile` now also re-reads `git branch --show-current` per workspace and persists the result, so a `git branch -m` rename surfaces in the TUI on the next refresh instead of staying frozen at workspace-create time.

### Fixed
- **Popup from workspace dir resolved to worktree path, not source repo.** `routeRoot` did `config.DiscoverAndLoad(cwd)` first, which found the worktree's checked-in `canopy.json` and set `cfg.ProjectRoot` to the worktree path. Every state.GlobalRow keys by the canonical source-repo ProjectRoot, so the Local tab matched zero rows. Now `workspace.ResolveCurrentProject` wins (workspace-path-prefix match first), with `DiscoverAndLoad` only as the fallback for unregistered cwd. Extracted into `resolveProjectContext` for testability.
- **Same bug in every CLI verb.** `loadManager` had the same `DiscoverAndLoad`-first shape, so `canopy reconcile`, `canopy switch`, `canopy rm`, etc. silently no-op'd when run from inside a workspace dir (Reconcile filtered every row out by ProjectRoot mismatch). Fixed via `resolveCfgForCwd` that mirrors the route-level resolver.
- **Enter on a dead main row errored instead of starting the session.** Used to print "main session not running — run `canopy main` in a terminal to start it", which a popup user can't even reach without first closing the popup. Now calls `Manager.EnsureMainSession` to build the session, then attaches/switch-clients in the same flow. `cmd/canopy main` was refactored to share the implementation.

### Removed
- **Alive-dot column.** Was redundant for `ready` (always `●`), `stopped` / `broken` / `orphaned` (always `○`), and only added information for the `setting_up` and `main` rows. Status column carries that signal directly now.

## [0.8.0] - 2026-04-29 — TUI unification

One TUI for every canopy invocation. The popup, the global view, and the
project view collapse into one model with two tabs (Local + Global). What
this means in practice: pressing `<prefix>g` from any tmux pane opens the
same screen `canopy` shows from a terminal. No more "different screen"
jolt when navigating to a project — every entry point lands on the same
unified view.

### Added
- **One unified TUI surface.** Three Bubbletea models (project Model, GlobalModel, GlobalModel.AsPopup) and two cobra subcommands (popup, popup-inner) collapsed into one Model + one entry point. Same screen everywhere: `canopy` from a terminal, `canopy` from inside a workspace, `<prefix>g` popup. Tab key switches between Local (current project) and Global (all projects).
- **Cross-project destructive verbs.** `d` (delete) and `R` (retry) now work on any row in the Global tab, not just the current project. The confirm modal shows the project name as a safety net. Mirrors `canopy d --project=foo workspace-bar` on the CLI.
- **Retry confirmation modal.** Pressing `R` on a healthy (non-broken) workspace now opens a y/N gate instead of silently erroring. Mirrors the CLI's `canopy retry --force` friction so a muscle-memory R-press doesn't clobber state.
- **Project focus from Global tab.** Press `o` on any Global-tab row to switch the unified TUI's focus to that project — Local tab pre-populates, `n` (new workspace) becomes available. No spawn, no nested canopy. The unified TUI carries the focus context, not the parent shell.
- **Popup launches from inside workspace panes.** When you press `<prefix>g` from a workspace's tmux pane, the resolver maps the worktree path back to the canonical source-repo ProjectRoot and Local tab pre-populates with that project's workspaces. Includes a fix for symlinked cwds (`filepath.EvalSymlinks` before path-prefix matching).
- **Lazyworktree-flavored chrome.** Brand pill `◆ canopy` + scope pill on the top bar, rounded-cap tab pills via powerline glyphs (Nerd Font required), keybind cheatsheet at the bottom with each binding as `[key] desc`. Selected row highlights as a single bold-white-on-grey band with a `>` caret prefix. Tab bar always renders; empty tabs dim and show context-specific onboarding text. Search has its own `🔍 SEARCH` pill in active mode, `🔍 query  (esc to clear)` in persistent-filter mode.
- **Onboarding hint on Global tab.** Subtle reminder above the keybind cheatsheet: "add a project: cd to its repo and run `canopy init`". Hidden on Local where the project is already known.

### Changed
- **`canopy install tmux` writes the new bind shape.** `bind g display-popup -E -w 80% -h 80% -d "#{pane_current_path}" "CANOPY_IN_POPUP=1 <bin>"` — replaces the old `bind g run-shell "<bin> popup"`. The `-d "#{pane_current_path}"` is load-bearing for the Local-tab resolver. `CANOPY_IN_POPUP=1` is the env-var flag that flips the unified TUI into popup-mode rendering (single-line tab bar + switch-client attach).
- **Bare `canopy` runs inside tmux without `CANOPY_ALLOW_NESTED=1`.** The unified TUI uses `tmux switch-client` for selecting workspaces (no nested attach), so it's safe to launch from any tmux pane. `canopy new`, `canopy rm`, `canopy retry` still hit the nested-tmux guard — those genuinely need a non-tmux shell.
- **Routing simplified.** `cmd/canopy/route.go` now picks the unified TUI for almost every invocation; init splash only fires for fresh git repos with zero registered projects. When `workspace.New` fails (e.g. v1/v2 state collision), routing falls back to global mode with a warning instead of refusing to launch — destructive verbs on that project are unavailable until the underlying state is reconciled, but the user can still see and act on other projects.
- **Lazy-loaded hint badges persist across tab/search mutations.** PR-status, shipped, and rename badges no longer disappear when you press tab or type in the search box. Hints merge into the unfiltered row store; tab/search filters re-derive the rendered set from there.
- **Canopy migration auto-resolves stale v1/v2 state collisions.** When state.json contains both a legacy basename key (`cravd`) and a v2 root-path key (`/home/avi/Work/cravd`) for the same project, the basename entry is now dropped during `MigrateLegacyProject` (PortBase salvaged to v2). Previously the basename-collision guard refused to construct a Manager, blocking the unified TUI from launching from inside that project.

### Removed
- **`canopy popup` and `canopy popup-inner` subcommands.** tmux's `display-popup -E` now invokes `canopy` directly. ~600 LOC of subcommand + spawning + signal-channel code deleted.
- **Spawn-a-child-canopy popup architecture.** The `goToProject` re-exec, `CANOPY_FROM_GLOBAL` / `CANOPY_FROM_POPUP` env vars, the `os.Exit(7)` popup-attach signal, and the `attachedFromPopup` field are all gone. Single-process popup hosting via `display-popup -E` is simpler and faster.
- **Three Bubbletea models and their tests.** `internal/ui/model_global.go` (906 LOC), `internal/ui/model_global_test.go` (748 LOC), and the popup test trio (`popup_test.go`, `popup_inner_test.go`, `popup_inner_resolve_test.go`) all deleted. Net: -1452 LOC across the merge commit alone.

### Fixed
- **Cross-project delete now matches by (Project, Name) pair.** Pre-fix: two projects each with a workspace named "foo" could be ambiguous on the Global tab — a refresh between modal-open and confirm could re-order rows so the wrong "foo" got deleted. P0 data-loss bug caught by the adversarial review during /ship; regression test added.
- **Popup-from-workspace-pane finds the right project.** Previously `<prefix>g` from inside `~/.canopy/workspaces/canopy/foo` would walk up canopy.json into the worktree dir, mismatch every state.GlobalRow's ProjectRoot, and the Local tab would silently empty. Resolver now does a workspace-path-prefix lookup first and returns the canonical source-repo ProjectRoot.
- **Lazy hint loading no longer breaks across tab switches.** rowHintsMsg now mutates the unfiltered store before re-pushing the filtered set to projectlist, so hints survive every SetRows that tab-switch and search-mutation trigger.

### Added (foundations)
- `config.LoadFrom(root)` — direct read of `<root>/canopy.json` without the walk-up that `DiscoverAndLoad` does. Used by the unified TUI's transient Manager construction (cross-project d/R) where the caller already knows the canonical project root from `state.GlobalRow.ProjectRoot`.
- `internal/workspace/projectroot.go` — the popup-pane → project-root resolver, ported from `cmd/canopy/popup_inner.go` and exposed as `workspace.ResolveCurrentProject`. Two-step lookup (workspace-path prefix match → registered-project canopy.json walk-up), with `filepath.EvalSymlinks` applied first for symlink resilience. Structured DEBUG logs at every resolution branch so `~/.canopy/log/canopy.log` answers the "why is my Local tab empty?" question without rerunning under `--debug`.
- `internal/ui/keymap.go` — listMode bindings as a `[]Binding` data table wrapping `bubbles/key.Binding` with an `Action` func. Replaces the giant switch statement in `update.go`. Each binding has an optional `Available(*Model) bool` predicate that gates both help-line rendering and dispatch — `n` is hidden on Global tab via this single source of truth.

### Added
- MIT license + README license badge.
- GitHub Actions CI: unit + e2e test runs on push/PR.
- `docs/landscape.md` — public-facing positioning ("where canopy fits").
- ASCII header in README for terminal-native vibes.
- Status badges now carry a 1-rune shape glyph (`…` setting_up, `⏸` stopped, `✗` broken, `!` orphaned) so the workspace state reads under protanopia and on monochrome terminals, not just by color. Healthy and main rows stay clean. The live `●`/`○` badge still conveys aliveness.
- MIT license + README license badge.
- GitHub Actions CI: unit + e2e test runs on push/PR.
- `docs/landscape.md` — public-facing positioning ("where canopy fits").
- ASCII header in README for terminal-native vibes.
- Status badges now carry a 1-rune shape glyph (`…` setting_up, `⏸` stopped, `✗` broken, `!` orphaned) so the workspace state reads under protanopia and on monochrome terminals, not just by color. Healthy and main rows stay clean. The live `●`/`○` badge still conveys aliveness.
- **Persistent TUI (v0.7).** Three new ways to reach canopy without leaving your work:
  - `canopy popup` — launches the global TUI in a tmux floating popup. Bound to `<prefix>g` by `canopy install tmux`. Picking a workspace switches your client to it and closes the popup, no detach round-trip.
  - `canopy statusline --format=current` — single-line tmux status-bar widget showing the current workspace's name, status glyph, and port. Panic-safe (defer-recover protects your status bar) and tmux-injection-safe (escapes `#` to `##`).
  - `canopy install tmux` — idempotent `~/.tmux.conf` writer with `--force`, `--dry-run`, marker-block detection, timestamped backup, and symlink-aware atomic write (preserves stow/chezmoi setups). Embeds the running binary's path so dogfood vs system install both work.
- **Local / Global tabs in popup.** Tab switches between current-project workspaces and all-projects view. Local pre-selected when launched from inside a project, Global otherwise. Resolution is workspace-path-aware so `<prefix>g` from inside a worktree finds its parent project correctly.
- **Fuzzy search in popup.** `/` enters search mode; query matches name, project, OR branch via subsequence. `Esc` clears, `Enter` exits search keeping the filter.
- `canopy run` — execs `scripts.run` from the nearest `canopy.json` via `syscall.Exec`. Inherits `CANOPY_PORT` and friends from the workspace tmux session. Bind it manually if you want a one-keystroke server start; default block stays out of conflict-prone keys.
- `canopy retry --force` — allow retry on `ready` / `stopped` workspaces (re-runs `scripts.setup` on a healthy workspace; useful for refreshing DB schema or agent briefings). Always refuses `setting_up` (concurrent setup hazard) and `orphaned` (no on-disk dir).

### Changed
- README install section reorganized: explicit Linux / macOS / Windows (WSL2) one-liners; nvim called out as a hard requirement.
- Design doc keymap (`docs/design/v0-canopy.md`) synced to the actual bindings: `g`/`G`/home/end for first/last, `R` for retry-broken, `b`/esc for back-to-global, `ctrl+c` quit. Added a one-liner about modal pickers in the new-workspace flow.
- `tmux attach` calls inside the project TUI now auto-route to `tmux switch-client` when running inside an existing tmux session (popup mode, or any nested invocation). Single helper (`attachVerbForCurrentEnv`) at the wrapper layer; every attach site benefits.
- Pressing `o` in popup-mode global TUI now opens the project TUI inside the popup (was previously unsupported — failed with "exit status 1" because the spawned canopy hit its own nested-tmux guard). The spawned canopy bypasses the guard via `CANOPY_ALLOW_NESTED=1`. Successful attach from within sends a popup-close signal (exit code 7) so the popup goes away cleanly without an extra `q`.

### Deprecated

### Removed

### Fixed
- Popup `o` (open project) no longer prints "exit status 1" — the spawned canopy now bypasses the nested-tmux guard cleanly.
- Resurrecting a stopped workspace from inside the popup actually works now — the `tmux attach` from project TUI auto-routes to `switch-client` because the popup pty is itself inside a tmux client.
- Popup launcher now passes the calling pane's cwd to `tmux display-popup -d`, so the Local tab finds the right project even when launched from inside a workspace dir (was inheriting tmux server's cwd).
- Popup statusline runs with `allow-in-tmux` annotation — was previously refused by the nested-tmux guard every time tmux re-ran `#(canopy statusline)`.

### Security
- Statusline output escapes `#` → `##` so a hostile branch name like `feat#[bg=red]gotcha` cannot inject style sequences into tmux's `status-right`. Cosmetic-to-misleading impact mitigated; not RCE.

## [0.1.0] — TBD (first public release)

The first canopy release with a public home. Highlights of what's in
this release (which has been accumulating across the pre-v0.1 dogfood
cycle):

### Workspace lifecycle
- `canopy init`, `canopy new`, `canopy ls`, `canopy switch`, `canopy rm`,
  `canopy reconcile`, `canopy main`, `canopy retry`, `canopy version`.
- Per-workspace git worktree under `~/.canopy/workspaces/<project>/<name>/`
  (centralized storage; source repo stays clean).
- Three lifecycle scripts in `canopy.json`: `setup` (once on create), `run`
  (reserved for on-demand `canopy run`), `archive` (on remove).
- Workspace state machine: `setting_up` → `ready` / `broken`; `ready` ↔
  `stopped` (resurrectable); `orphaned` for vanished dirs.

### TUI
- Bubbletea TUI launches when `canopy` is invoked with no subcommand.
- Three-pane workspace layout: nvim (top-left), claude (top-right), shell
  (full-width bottom).
- Global TUI when run from outside any project: every project + every
  workspace + every alive `<project>-main` session canopy knows about.
- Init splash when `canopy` is run in a fresh git repo with no `canopy.json`.

### Recovery + diagnosis
- `canopy retry <name>` re-runs `scripts.setup` against an existing broken
  workspace (preserves worktree, branch, port, claude conversation history).
- Auto-detect setup hints: known stderr signatures (Rails master key,
  database-already-exists, network blip, missing bundle, etc.) surface as
  one-line user-facing diagnoses.

### Safety
- Refuse to run from inside an existing tmux session by default. Carve-outs
  for `canopy version` and `CANOPY_ALLOW_NESTED=1`.
- Per-script env: `CANOPY_WORKSPACE_PATH`, `CANOPY_ROOT_PATH`, `CANOPY_PORT`.
  `CONDUCTOR_*` aliases for zero-friction Conductor migration.

### Docs
- `docs/getting-started.md`, `docs/landscape.md`, `docs/canopy-json.md`,
  `docs/migrate-from-conductor.md`, `docs/troubleshooting.md`,
  `docs/architecture.md`, `docs/design/v0-canopy.md`.

[Unreleased]: https://github.com/avinashjoshi/canopy/compare/v0.12.4...HEAD
[0.12.4]: https://github.com/avinashjoshi/canopy/releases/tag/v0.12.4
[0.8.0]: https://github.com/avinashjoshi/canopy/releases/tag/v0.8.0
[0.1.0]: https://github.com/avinashjoshi/canopy/releases/tag/v0.1.0
