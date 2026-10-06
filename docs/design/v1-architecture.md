# canopy v1 — Rust rebuild: architecture

Status: D-1..D-3 approved 2026-10-05 (tmux-first). Phase 0 and Phase 1 core landed the same day; see §10 for what is in and out.

## 0. One paragraph

canopy v1 keeps everything people liked about canopy v0 (Go): a *project* is a repo with a
`canopy.json`, a *workspace* is a git worktree with a stable port, lifecycle scripts, a paired
terminal session with fixed pane roles, an agent that resumes where it left off, health and
agent-state badges, and one TUI that sees every workspace on every host. It replaces v0's
*runtime* with a server model: a long-lived **server** that owns state and terminals, a
**thin client** (TUI and CLI) that talks to it over a Unix socket with a newline-JSON API plus an
event stream, and **remote hosts** reached by bridging that same protocol over OpenSSH stdio
instead of exec-replacing into `ssh … tmux attach`. Written in Rust, one binary.

## 1. What stays, what changes

| Concern | Decision |
|---|---|
| Project / workspace / port / scripts model | Kept from v0: `canopy.json`, `CANOPY_*` env, strided port plan, setup/run/archive, retry, resurrect, rename-follows-branch |
| Agent briefing + launcher table | Kept from v0: full vs delta briefing, resume handling, rename-first nudge, data-not-instructions wrapping |
| Badges / hints / TUI information design | Kept from v0: `⚡ 💤 ✋`, `⚠ conflict`, `↑N ↓N *N`, `↗ rename-suggested`, PR state, idle-project collapse |
| Run scripts | Extended: `scripts.run` as a table (`command`, `args`, `options.cwd`, `default`, `hide`), ten-port block per workspace, SIGTERM then SIGKILL after a grace period. Nothing tool-specific is built in: `canopy init --from <file>` (or `[init] adopt_from` in the user config) seeds the script table from any JSON/TOML with the same shape, and `env_prefixes` (project) / `[env] prefixes` (user) mirror the core env vars under extra names |
| Branch-first flow | Kept: branch from `origin/<default>` after a fetch; agent told to rename the branch in the first message. A branch may be checked out in one workspace only (git enforces it; canopy surfaces a clear error) |
| Merge-readiness | Kept + extended: git status and PR state hints, now with CI/status-check state. Todos, deployments, review threads: not planned |
| Runtime | New: server + thin client, auto-spawned daemon; `canopy` with no args probes the socket, spawns `canopy server`, attaches |
| API | New: newline-JSON API + typed events + `events.subscribe`, so agents and scripts can drive canopy; `schemars` schema published |
| Agent state | New: hook reports (`pane.report_agent`) are authoritative, screen rules are the fallback; replaces v0's SHA-of-capture-pane heuristic |
| Remote | New: OpenSSH stdio bridge with ControlMaster, zero registration; no socket forwarding, no mosh dependency for the dashboard |
| Persistence | New: debounced atomic writes, snapshots, backups: `state.json` v3 + `snapshots/` |
| Server-owned PTYs + terminal emulation | **Phase 3** (see D-2); tmux stays as a backend |

Not in scope: a plugin system, theme engine, mobile/cloud endpoints, live handoff between
server versions (maybe later), Windows support. canopy v0's clipboard daemon + SSH RemoteForward
design is retired; OSC 52 for text and client-side paste bridging for images replace it.

Alternative considered and rejected: build canopy as a layer over an existing terminal
workspace manager. It would reuse that multiplexer for free, but canopy would then be a thin
layer over someone else's product and protocol churn, and the ask was a Rust rebuild with its
own server.

## 2. Process model

```
 laptop                                               tower (remote host)
 ┌───────────────────────────┐                        ┌───────────────────────────┐
 │ canopy (TUI client)       │                        │ canopy server             │
 │ canopy new/ls/rm (CLI)    │── ~/.canopy/canopy.sock│  state · ports · scripts  │
 │                           │   (NDJSON API + events)│  backend: tmux | native   │
 │ canopy server ◄───────────┘                        │  agent detection          │
 │  (auto-spawned, setsid)   │                        └───────────▲───────────────┘
 └───────────▲───────────────┘                                    │ stdio ↔ socket
             │ canopy --remote tower ─── ssh -T tower 'canopy bridge' ┘
```

**D-1. One server per machine per user**, started on demand by the first client
(`Command::new(current_exe).arg("server")`, `setsid`, stdio to `/dev/null`), detached from the
client. Socket `$XDG_RUNTIME_DIR/canopy/canopy.sock` when set, else `~/.canopy/canopy.sock`,
mode 0600, stale-socket probe (ECONNREFUSED ⇒ unlink). `canopy server stop` via API. The server
survives client exit; it does **not** need to survive reboot — workspaces are persistent on disk
and the server reconciles on start (exactly v0's `reconcile`).

Why a server at all, given v0 managed without one: remote polling, agent-state tracking, port
locking, PR-status polling and event delivery all want a process that outlives the TUI. v0 bolted
these onto TUI goroutines and flock'd JSON; every concurrency bug in the v0 CHANGELOG lives there.

## 3. Session backend (the big decision)

**D-2. `SessionBackend` trait with two implementations, shipped in this order:**

1. **tmux backend (Phase 1).** The server drives tmux exactly like v0 did (session
   `<project>/<branch>`, three panes tagged with `@canopy-role`, `tmux -e CANOPY_*`). Attach is
   `tmux attach` (local) or `ssh -t … tmux attach` / mosh (remote). This gets full v0 parity on the
   new runtime fast, and keeps tmux users (the current ones) happy forever.
2. **native backend (Phase 3).** The server owns PTYs (`portable-pty`) and emulates terminals
   (`alacritty_terminal` behind a small `Vt` trait so libghostty-vt can be swapped in later for
   Kitty graphics). The client attaches over the same socket/bridge and receives pane surfaces;
   paste, clipboard and keybindings are local. This is what makes `--remote` a real thin client
   instead of an exec into ssh.

The trait is small on purpose: `create_session`, `kill_session`, `session_alive`, `attached_clients`,
`spawn_pane(role, cmd, env)`, `panes()`, `send_text`, `read_screen(pane, bottom_lines)`, `attach_target()`.
Both backends are known in advance, so the "interface for one backend is wrong" risk is low.

Everything *above* the trait — state, ports, scripts, briefing, hooks, API, TUI dashboard, remote
bridge — is shared and is most of the code.

## 4. Domain model

```
Host ─┬─ Project (root, name, port_base, canopy.json)
      │     └─ Workspace (name, branch, path, port, status, source, owner, agent, launches)
      │           └─ Session (backend id) ─ Pane{role: ide | agent:<kind> | terminal:shell | run:<name>}
      └─ Main session per project (port_base + 0)
```

Statuses unchanged: `setting_up | ready | stopped | broken | orphaned` (+ synthetic `main`).
Invariant kept: branch name == path basename == session suffix, unless `pinned`.
Derived data (hints, agent state, PR status) is never persisted. Workspace ids are stable random
public ids (`w7K`) so renames don't break API clients; names remain the human handle.

### Files

```
~/.canopy/
  config.toml           user settings (ports, editor, layout, agents, hosts defaults)   [new: TOML]
  state.json            projects + workspaces, schema_version 3, debounced atomic write
  snapshots/            bounded state history (48 max, ≥15 min apart)
  hosts.json            saved machines (name, ssh target, enabled)
  canopy.sock           API socket (or $XDG_RUNTIME_DIR/canopy/)
  workspaces/<project>/<name>/
  log/{server,client}.log
<repo>/canopy.json      project config (unchanged, JSON, committed; legacy workspace-tool configs adopted on init)
```

### canopy.json v1

```jsonc
{
  "scripts": {
    "setup":   "bin/canopy-setup",
    "run":     "bin/dev",                              // string, or table of named scripts:
    // "run": { "web": {"command":"bin/dev","default":true}, "jobs": {"command":"bin/jobs"} },
    "archive": "bin/canopy-archive",
    "agent":   "",                                     // custom launcher override (kept)
    "timeouts": { "setup": 600, "archive": 120 }       // NEW; default 600/120 s, kill process group
  },
  "agent": { "type": "claude", "briefing": "", "briefing_file": "" },   // kept
  "agents": ["claude", "codex"],                       // allowlist for --agent / canopy ask (kept from design)
  "layout": "default",                                 // NEW: named pane layout from config.toml
  "env_prefixes": ["OLDTOOL"]                          // NEW: mirror core env vars under extra prefixes
}
```

Env passed to scripts and sessions: `CANOPY_WORKSPACE_PATH`, `CANOPY_ROOT_PATH`, `CANOPY_PORT`,
`CANOPY_WORKSPACE_NAME`, `CANOPY_BRANCH`, `CANOPY_PROJECT`, mirrored under any configured
prefixes, plus runtime vars `CANOPY_SOCKET_PATH`, `CANOPY_WORKSPACE_ID`, `CANOPY_PANE_ID` so hooks can report in.
Port block: `CANOPY_PORT .. CANOPY_PORT+9` is reserved per workspace (stride 10, as before).

## 5. API

Newline-delimited JSON over the socket. `{"id","method","params"}` →
`{"id","result":{"type":…}}` | `{"id","error":{"code","message"}}`. One request per connection
except `events.subscribe`. Schema generated with `schemars`, printed by `canopy api schema`.

Method families (Phase 1 set):

- `ping`, `server.{stop,reload_config,status}`
- `project.{list,get,add,remove,init}`
- `workspace.{list,get,create,remove,retry,log,rename,pin,reconcile,resurrect,attach_target,run,stop_run}`
  (`workspace.log` returns a slice of the setup log by byte offset so clients can tail it live;
  rows carry a `progress` line while setting up)
- `pane.{list,read,send_text,send_keys,report_agent,report_agent_session}`
- `agent.{list,get,wait,prompt}`
- `host.{list,add,remove,status}` (Phase 2)
- `events.{subscribe,wait}` — `workspace_{created,updated,removed}`, `agent_status_changed`,
  `run_{started,exited}`, `host_{online,offline}`; 512-entry ring, `events_lost` on lag.

The CLI is a thin mapping onto these (`canopy new` ⇒ `workspace.create`), so the whole CLI is
also the automation API. `canopy ls --json` stays as a stable read for scripts.

## 6. Agent integration

**D-3. Hooks first, screen second.** `canopy integration install claude` writes a managed hook
script and edits `~/.claude/settings.json` (`SessionStart`, `UserPromptSubmit→working`,
`Stop→idle`, `Notification→blocked`, `SessionEnd→release`); the script posts
`pane.report_agent` to `CANOPY_SOCKET_PATH`. Same for codex/opencode/gemini where hooks exist.
Fallback: TOML screen manifests (regex rules over the bottom N lines) ported from v0's claude
patterns, so an agent without hooks still gets a badge. States: `idle|working|blocked|done|unknown`.

Launcher table stays code, not config (one PR per agent), with `fresh`, `resume`, `exec`
(one-shot, for `canopy ask`) and `briefing_mode` per launcher. Session ids reported through hooks
enable real `claude --resume <id>` on resurrect instead of `--continue`.

## 7. Remote hosts

Shipped 2026-10-05 in its zero-setup form (Avi: "without doing any of the host add dance"):
`canopy --remote <ssh-target>` and `canopy --remote <target> <verb>`. No registry. The client
probes the host (`uname`, `canopy version`), refuses protocol mismatches, and with `--install`
copies its own binary when the architecture matches. Each request is one `ssh -T` running
`canopy bridge` (stdio <-> local socket) over a ControlMaster connection in `~/.canopy/ssh/`;
`events.subscribe` is one long-lived ssh. Attach converts `Tmux` to `RemoteTmux` and execs
mosh (or `ssh -t` with `--ssh`). A saved-hosts registry and multi-host aggregation remain
future work; the original plan follows.

Original Phase 2 plan: `canopy host add tower cassy@tower`. The client runs `ssh -T tower 'exec canopy bridge'`
with a managed ControlMaster socket; the far side connects to its local `canopy.sock` and pipes
stdio, so the laptop speaks the normal API to the remote server. The dashboard aggregates all
hosts; `canopy --remote tower` pins to one. Remote verbs (`new --on tower`) are plain API calls over
the bridge — no more shell heredocs with base64 prompts. Attach under the tmux backend still execs
`ssh -t/mosh … tmux attach`; under the native backend attach is the bridged surface stream.
Protocol has a version and a capability list in `ping`; skew is tolerated, generation bumps are not.

Clipboard over `--remote` (2026-10-05): the client no longer execs into mosh/ssh; it spawns the
attach as a child and stays resident (`remote::attach_supervised`), polling the local clipboard
(macOS: NSPasteboard changeCount via JXA + `clipboard info`/`pbpaste`; Linux: wl-paste/xclip)
and pushing changes to the remote: text via `clipboard.set_text`, PNGs via scp +
`clipboard.set_file`. The server writes them to its own clipboard (`server/clipboard.rs`:
system `wl-copy` with `WAYLAND_DISPLAY` discovered from the runtime dir, else xclip/pbcopy), so
`Ctrl+V` in a remote agent pastes a local screenshot. Remote → local text uses OSC 52 through
tmux `set-clipboard`. This replaces v0's tunnel + wrapper-script design entirely.

## 8. TUI

Two surfaces, both ratatui + crossterm, both stateless (they render server snapshots and
refresh on events):

- **Sidebar** (`canopy sidebar`, added 2026-10-05 at Avi's request for a sidebar-plus-tabs layout): a
  full-height pane at the left of every session, role `sidebar`, width from
  `[sidebar] width`. Collapsible project tree, workspace rows with badges, and the current
  workspace's tmux windows listed as tabs. Switching is `tmux switch-client` / `select-window`
  through the server API. `q` collapses it to a 3-column strip (canopy mark, ✋/⚡ counts);
  `prefix+b` cycles expand / focus / collapse via `session.sidebar_toggle`. "Collapsed" is
  derived from the pane's width by everyone involved (server resizes, sidebar re-renders), so
  no process-to-process messaging is needed, and new tabs inherit the state. Open/closed,
  folded projects and expanded workspace tab lists are one shared state in
  `~/.canopy/sidebar.json`, polled by mtime by every sidebar, so all sessions agree (Avi:
  "keep it open until its closed"; focus-following exists behind `[sidebar] auto_collapse`,
  off by default, because it flickered on every switch).
- **Dashboard** (`canopy`, or `prefix+g` popup): the sidebar's bigger sibling. Projects as
  header rows (path, port base, counts), `main` first under each, then workspaces with the
  full badge set and hints; folds are shared with the sidebar (`shared::SharedUi`). Title bar
  and key-cap footer match the sidebar; dialogs use the shared frame. First run with no
  projects shows a welcome card (`a` to add a project, or `canopy init`).

Tabs are tmux windows. **No install step** (Avi, 2026-10-05: "everything should be automated
and super simple"): the server configures tmux at runtime through the backend
(`ensure_server_config` + `decorate_session`), never by editing `~/.tmux.conf`. Keybinds
(`prefix+b`, `prefix+g`) are bound only where the user has nothing bound (the no-prefix
`Ctrl+Alt+c` switcher was retired once the sidebar existed; an old canopy binding on it is unbound); tab
styling is applied globally only where tmux defaults are still in place; status position,
renumbering, titles and the statusline segment are session options on canopy sessions. The
pollers re-apply every 10 s so a restarted tmux server picks them up again. The same policy
covers agent hooks: the server auto-installs Claude Code hooks when `claude` is on PATH
(`[integrations] auto_install`, default on, settings file backed up). The first window of a
workspace session is named `work` with automatic-rename off; run scripts open `run:<name>`.

Both surfaces list the project's **main** session (repo root, port = project base) as a row
under the project (`project.main` to attach/create, `project.stop_main` to kill; facts come
from `ProjectRow.main_*`). Avi, 2026-10-05: "also have the main branch / main project folder".

From the sidebar, `n` and `a` open the forms as full-screen `tmux display-popup`s running
`canopy ui new|add-project` (Avi: "within sidebar is weird and squished"); the popup switches
the client to the new session on success. Both surfaces share the new-workspace form
(`client/newform.rs`): Fresh / PR / Issue / Branch
with lists fetched via `project.pull_requests|issues|branches` (server runs `gh` / `git`, so
it works over `--remote`), and an add-project prompt backed by `project.init` (local path or
git URL cloned into `source_root`, default `~/.canopy/sources`). `project.remove` forgets an
empty project. Avi, 2026-10-05: "from the UI" — CLI flags exist but the UI is the primary path.

Under the native backend (Phase 3) the sidebar component is rendered by the client itself and
tabs become native windows; the tree model and keymap carry over unchanged.

## 9. Crate layout

```
Cargo.toml                 workspace
crates/
  canopy-core/             domain: project, workspace, status, ports, namegen, canopy.json, state store,
                           script runner, git plumbing, briefing. No I/O to sockets, no TUI.
  canopy-proto/            API request/response/event types + schema. Shared by server and client.
  canopy-server/           daemon: API listener, app loop, SessionBackend{tmux,native}, agent detection,
                           reconcile, pollers, persistence.
  canopy-client/           TUI (ratatui) + remote bridge client.
  canopy/                  the binary: clap CLI, auto-spawn, routes to server/client/bridge.
```

Dependency direction is leaf-up (`core ← proto ← server/client ← bin`), same discipline as v0.

Crates: `tokio`, `clap`, `serde`/`serde_json`, `toml`, `schemars`, `ratatui`, `crossterm`,
`tracing`, `regex`, `nix` (flock/setsid), `portable-pty` + `alacritty_terminal` (Phase 3). Git and
ssh are shelled out, as in both predecessors.

## 10. Phases

| Phase | Deliverable | Done when |
|---|---|---|
| 0 | Design doc, workspace skeleton, core domain with tests | done 2026-10-05 |
| 1 | Server + CLI + TUI parity on tmux backend | mostly done 2026-10-05: server, API, events, create/rm/retry/resurrect/rename/reconcile/main/run/stop, script timeouts, briefing, launcher table, claude hook integration + screen fallback, git hints, PR status, TUI (list, new, delete, kill, retry, inspect, filter, popup mode), `install tmux`, statusline, v0 state import. **Open:** `canopy ask`, `canopy config`, upgrade/`use`, owner editing in TUI |
| 2 | Remote hosts over stdio bridge | `--remote`, `--on`, hosts tab, aggregated dashboard, `canopy host install` |
| 3 | Native backend | server-owned PTYs, pane surfaces to client, OSC52 + image paste bridge, backend selectable per machine |
| 4 | Extras | `canopy ask`, skills-as-briefing, doctor, upgrade channels, state snapshots UI |

## 11. Open questions for review

1. **Backend order (D-2):** tmux-first then native, or native from day one and skip tmux? tmux-first
   gives a usable tool in weeks; native-first is months before anything is daily-drivable.
2. **Terminal emulator for Phase 3:** `alacritty_terminal` (pure Rust, no Kitty graphics) vs a
   C/Zig VT library over FFI (build-time toolchain cost, has graphics). Proposal: alacritty
   behind a trait.
3. **Config format:** `~/.canopy/config.toml` for user settings while `canopy.json` stays JSON
   for project config and legacy compatibility. OK to have two formats?
4. **Server lifetime:** on-demand per user (proposal) vs systemd user unit. On-demand is simpler;
   a unit can come later.
5. **Compatibility with v0 state:** import `~/.canopy/state.json` v2 on first run so existing
   workspaces are adopted, then write v3. Proposal: yes, one-way.
