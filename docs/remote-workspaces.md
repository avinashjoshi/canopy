# Remote workspaces (v0.17, thin-client mode added v0.22)

Canopy workspaces don't have to live on your laptop. Register an SSH-reachable host once, register a project path on that host, then run `canopy new --on tower` from anywhere. The heavy work — `scripts.setup`, `scripts.run`, the agent panes — happens on the host. The laptop becomes a thin client. One unified TUI views every workspace across every host.

## When to use it

- **Heavy builds / heavy dependencies.** Your laptop fans don't have to be the bottleneck. A 32-core tower does the bundle/install/migrate dance and you keep typing.
- **Long-running agents.** A `canopy new --on tower --prompt "..." --no-attach` keeps claude grinding even when you close your laptop.
- **Shared review boxes.** A team SSH box can host workspaces for everybody; each developer keeps their own laptop-side state.
- **OS isolation.** Macs and Linuxes for the same project, without you switching machines.

## Prerequisites

| Tool | Where | Why |
|---|---|---|
| Canopy (v0.17.0+) | Laptop AND each remote host | Both ends speak the same wire protocol (`canopy ls --json` schema v3). |
| SSH (OpenSSH 8.x+) | Laptop | ControlMaster reuse keeps refresh fast. |
| SSH key auth to each host | Laptop ↔ host | Passwords break the BatchMode no-prompt contract. The `--interactive` add flow can run `ssh-copy-id` for you. |
| mosh (1.4+) | Both ends | Used by `canopy switch --on <host>` for suspend-tolerant attach. Optional as of v0.23.0.0 — falls back automatically to an ssh reconnect-loop if mosh isn't installed locally, or opt into that path explicitly with `--no-mosh` even when mosh IS installed (e.g. its UDP transport is blocked by a firewall/VPN). |

Install canopy on the remote with the same one-liner you used locally:

```bash
ssh tower
curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh
exit
```

## Thin-client mode: zero-setup with `canopy --remote <host>`

Everything below this point assumes you've registered a host and a project on it. If you just want
to check on a box right now — no `canopy host add`, no `canopy project add`, nothing written to
`~/.canopy/hosts.json` — use the thin-client root flag instead (v0.22):

```bash
canopy --remote tower                          # registered name, OR a bare ~/.ssh/config alias
canopy --remote avi@tower.tail-abc.ts.net      # raw SSH target, zero prior setup
```

Run from anywhere — you don't need to be inside a project, or even have `canopy.json` on your
laptop at all. `<host>` is resolved by trying the registry first: if it matches a name in
`~/.canopy/hosts.json`, that entry's SSH target is used; otherwise `<host>` is used directly as a
raw SSH target (never persisted, never touches `hosts.json`) — so a bare `~/.ssh/config` alias like
`tower` works exactly like `ssh tower` does, with no registration required. This mirrors how
herdr's `--remote <host>` works — point it at a box and go, no registration ceremony.

The TUI that opens is pinned to that one host: only the Global tab is shown (no Local tab — there's
no local project; no Hosts tab — fleet management doesn't apply to a single pinned box), and it
lists exactly that host's workspaces, fetched the same way the Global tab already fans out over
SSH for registered hosts. It's not read-only: `n` opens the same new-workspace picker as a
registered remote row (Fresh / Prompt / PR / Issue / Branch), and `enter` / `d` / `K` / `R` all work
the same as they do against any other remote row on the Global tab.

Use `--remote` for the "just log in and look" case; use the registered `--on <host>` flow (below)
when you want the host to also show up folded into your regular local + Global workspace view, or
when you're scripting against it (`canopy new --on tower`, `canopy rm --on tower`, etc.).

**Clipboard bridge auto-setup.** The first time a `--remote` session confirms the host is reachable,
canopy automatically runs the same install the Hosts tab's `c` key (and `canopy host clipboard
<name>`) would — pushing the wrapper scripts and tmux config to the remote — so copying/pasting
text with an agent on the pinned host works without any manual setup step. It's silent on success
(a one-line "clipboard bridge ready" notice appears briefly) and non-fatal on failure. Unlike the
original daemon-based design, this has no laptop-OS requirement — the bridge uses OSC 52 terminal
escape sequences, handled by whatever terminal you're sitting in front of, not by the laptop's OS —
so it runs on macOS and Windows laptops too, not just Linux. Text only; images aren't supported. See
[`clipboard-bridge.md`](clipboard-bridge.md) for the mechanism and
[`design/v0.18-clipboard-bridge.md`](design/v0.18-clipboard-bridge.md) for the design history.

Add `--no-mosh` to skip mosh for every attach dispatched from this pinned session and use the ssh
reconnect-loop instead (`canopy --remote tower --no-mosh`) — see "Attach" below for when that's
useful. Missing mosh already falls back to the same path automatically.

## End-to-end walkthrough

### 1. Register a host

```bash
# Quick form (positional args, no probe)
canopy host add tower cassy@tower.tail.ts.net

# Guided form — probes connectivity, offers ssh-copy-id if key auth fails,
# verifies the remote canopy version
canopy host add --interactive
```

Verify:

```bash
canopy host ls
```

```
NAME   SSH TARGET                                PROJECTS
pi     jarvis@a10i-pi-gateway.geep-carat.ts.net  — (run `canopy project add ... --on pi`)
tower  cassy@a10i-tower.geep-carat.ts.net        1 (canopy)
```

The host registry lives at `~/.canopy/hosts.json`. Hand-edits round-trip cleanly.

### 2. Register a project on the host

```bash
canopy project add cravd /home/cassy/Work/cravd --on tower
```

`canopy project ls` shows local + remote in one view:

```
HOST   PROJECT   PATH
local  brain     /home/avi/Work/brain
local  canopy    /home/avi/Work/canopy
local  cravd     /home/avi/Work/cravd
tower  canopy    /home/cassy/Work/canopy
tower  cravd     /home/cassy/Work/cravd
```

**Naming convention:** the project name on the remote should match the local project's directory basename so cwd-driven dispatch works without typing paths. (When you run `canopy new --on tower` from inside `~/Work/cravd`, canopy resolves `cravd` against the host's registry.)

### 3. Spawn a remote workspace

```bash
# From anywhere — uses the project path from the registry
canopy new --on tower

# With an opening prompt for the agent (fire-and-forget)
canopy new --on tower --prompt "fix the timezone bug" --no-attach

# One-off path override when you don't want to register a project
canopy new --on tower --remote-cwd /tmp/scratch-repo
```

The prompt travels safely: base64 encoded into a heredoc → umask-077 temp file on the remote → cleaned up on script exit. It never appears in `ps aux` on either machine.

### 4. Attach

```bash
canopy switch --on tower fix-the-bug
```

Under the hood that's `mosh --ssh="ssh ..." -- tmux attach -t canopy/<branch>`. Mosh's UDP transport tolerates laptop suspend, flaky wifi, and roaming between networks.

If mosh isn't installed locally, canopy falls back automatically (v0.23.0.0) to an ssh reconnect-loop: it attaches over plain `ssh -t`, and if the connection drops (ssh exits 255 — its own "transport failed" signal), canopy re-dials with exponential backoff instead of dropping you back to a shell. It's not as seamless as mosh's UDP roaming, but it survives a flaky connection the way a bare one-shot `ssh -t tmux attach` never would. You can also opt into the same path explicitly even when mosh IS installed:

```bash
canopy switch --on tower fix-the-bug --no-mosh
```

useful when mosh's UDP port range (see below) is blocked but ssh still works.

Once attached, the tmux statusline prefixes the workspace segment with a yellow `@<host>` pill (using the registered host nickname from `hosts.json`, not the remote's `hostname`). Two long-running sessions side by side — one local, one on `tower` — now look distinct at a glance, no more "wait, which box am I typing into?". The pill clears automatically when you re-attach to that same session locally (canopy unsets the tmux session env on local attach, so the pill never lies).

For multi-attach (you want to watch the session from two machines):

```bash
canopy switch --on tower fix-the-bug --share
```

That sets `CANOPY_NO_DETACH=1` over SSH, so the remote canopy switch doesn't kick the existing client.

### 5. Manage

Everything you can do locally works with `--on`:

```bash
canopy ls --on tower                    # workspaces on the host
canopy rm --on tower fix-the-bug        # remote teardown
canopy retry --on tower broken-one      # re-run scripts.setup remotely
```

## The TUI: Remote hosts tab

`canopy` (no args) → press `Tab` to land on **Remote hosts**.

The tab lists every registered host with:

- Current canopy version (probed every 2 seconds via `canopy ls --json`) with a yellow `⇑` badge when the host is behind your laptop (or `⇓` when it's ahead) — silent when matched
- Last-seen timestamp
- Workspace count + per-host CPU / memory hints
- Agent badges for each remote workspace (the laptop now shows `💤` / `✋` / `⚡` for remote rows, not just local)
- Last error if a fetch failed (auth, network, version mismatch)

On first load — before any host has a cached snapshot — never-refreshed hosts animate a Braille spinner (`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`, ~120 ms cadence) until their fan-out result lands, instead of showing a static `· (never refreshed)` for the full 3-second SSH window. Hosts with a cached snapshot keep their previous status. The tick loop stops itself once the fan-out settles (v0.21.1.0).

The same spinner glyph now also decorates each remote host's section header on the **Workspaces tab** during a refresh — the Hosts tab spinner only covered hosts without cached rows yet, so re-refreshing a host that already had rows looked silent. Spinners on both tabs animate from the same frame counter and clear together when results land (v0.21.5.0).

Verbs while on the tab:

| Key | Action |
|---|---|
| `enter` | Open host detail drawer (ssh-target, projects, last-error) |
| `n` | Open the in-TUI add-workspace picker (Fresh / Prompt / PR / Issue / Branch — full parity with local; loaders SSH `gh pr list`, `gh issue list`, and `git for-each-ref` against the remote project cwd) |
| `s` | Open an interactive `ssh` shell on the host (y/N confirm first; canopy refreshes when you `exit`) |
| `U` | Run `canopy upgrade --yes` on the host; output streams into the TUI |
| `S` | Run `canopy use release` on the host (recovery if it's running a DEV binary that refuses `upgrade`) |
| `a` | Re-offer ssh-copy-id (useful if the host lost key auth) |
| `d` | Remove the host from the registry |
| `f` | Narrow Global tab to this host's rows |

The **Global** tab mixes local and remote workspaces. Remote rows show a `↗` glyph in the TMUX column. Workspace verbs (`d`, `K`, `B`, `R`, `i`, `enter`) all work; some delegate over SSH transparently:

- `enter` → `canopy switch --on <host> <name>` (mosh attach)
- `d` → `canopy rm --on <host> <name>` (with `F` for force, no local safety preflight)
- `K` → `ssh <host> tmux kill-session -t canopy/<branch>`
- `B` → auto-creates an `ssh -L <port>:localhost:<port>` forward and runs `xdg-open` locally — your laptop browser opens the remote dev server
- `R` → `canopy retry --on <host> <name>`
- `i` → diagnostic drawer fetched over SSH

If you press `enter` on a session another client is already attached to, the TUI now pops a y/N confirm before sharing/stealing. Skips the prompt when re-attaching to your own launching session.

## Upgrading a remote

Two flows, both on the **Remote hosts** tab:

- **`U`** — `canopy upgrade --yes` on the host. Confirms first (no surprise SSH), then streams `git pull && make install` output into the TUI. Errors stay on-screen instead of disappearing inside an alt-screen suspend. When the failure is a permission problem on `~/.canopy/src` or `~/.local/bin` (typical when a previous install ran as `sudo`), the error names the offending directory and tells you to `sudo chown -R $(whoami) …` it — the misleading "local commits in source clone" hint is gone (v0.21.1.0). The remote shell snippet prepends `~/.local/bin`, `/usr/local/go/bin`, `~/go/bin` to PATH and activates `mise` / `asdf` directly, so `make install` finds `go` even when the host's mise/asdf is wired through `~/.bashrc` (which a non-interactive `bash -l` skips) — fixes the `make: go: No such file or directory` failure on Arch/Omarchy/Ubuntu defaults (v0.21.7.0).
- **`S`** — `canopy use release` on the host. The recovery path when a host is running a dev binary and `canopy upgrade` refuses with "Switch to the released canopy first."

`U` is hidden on hosts already running a DEV binary (it would error); `S` is hidden on hosts reporting a real semver (it would no-op). Both keys show up on hosts reporting `(unknown)` — that's the legacy state for hosts older than v0.17.1 that didn't emit version over the wire.

Hosts that are behind your laptop's canopy show a yellow `⇑` next to their version in the row; the host detail drawer (press `enter`) spells the same out as `⇑ upgrade available (your local: vX.Y.Z) — press U`. Hosts ahead of your laptop render `⇓`. Comparison suppresses silently when either side reports `dev` or `(unknown)`.

## How it works under the hood

- **Wire protocol.** `canopy ls --json` schema v3 carries `mem_rss`, `cpu`, `hints`, `last_error_hint`, `agent_state`, and `canopy_version` per workspace row. Backwards-compatible: older laptop parsers ignore unknown fields.
- **Refresher.** The TUI's 2s tick fans out per-host goroutines with 3s deadlines, so one slow host can't block the others. SSH ControlMaster reuse keeps subsequent fetches near-zero latency. `BatchMode=yes` prevents password prompts from hanging the Bubbletea render.
- **Agent classification.** `agent.ClassifyOneShot` runs on the remote side of `canopy ls --json`, so the 💤 / ✋ / ⚡ badges are accurate for remote rows without the laptop having to read the remote tmux pane.
- **Main rows.** Remote `(main)` rows show real `running` / `not started` status and the actual default branch, because `IsMain=true` and `fillMainBranches` now both happen on the remote side of `canopy ls --json`.

## Limitations and gotchas

- **Agent must be installed on the remote.** Canopy can't ship `claude` for you. The remote needs `claude` (or whatever `scripts.agent` points at) on PATH.
- **mosh ports.** Mosh uses UDP ports 60000–61000 by default. If you're behind a firewall, either open the range or pass `--no-mosh` to use the ssh reconnect-loop attach path instead (see "Attach" above).
- **Single-laptop assumption.** The host registry is laptop-local. If you want the same registry on a second machine, copy `~/.canopy/hosts.json` over.

## Troubleshooting

- **`canopy host add` probe fails with auth error.** Run the probe with `--interactive` — it offers `ssh-copy-id` automatically. Or `ssh-copy-id <ssh-target>` by hand and re-add.
- **TUI shows `(unknown)` version for a remote.** That host is on canopy < v0.17.1.0 (older versions didn't emit `canopy_version` over the wire). `U` and `S` both show on those rows so you can recover.
- **Refresh hangs the TUI on launch.** Pre-v0.17.0.1 bug; upgrade your laptop canopy. The refresher now does all I/O inside the returned tea.Cmd closure.
- **Remote row shows the wrong agent badge.** Single-shot classification looks at the remote tmux pane on every refresh. If `claude` crashed to a shell, the badge correctly becomes `·`. Press `enter` to re-attach and start it.
- **`canopy new --on tower` from `$HOME` fails with "needs a project".** The TUI passes `--remote-cwd` resolved from the host registry; the CLI doesn't. Either `cd` into a project dir first or pass `--remote-cwd /path/on/remote` explicitly.

See [`troubleshooting.md`](troubleshooting.md) for the wider catalogue.

## See also

- [`getting-started.md`](getting-started.md) — local-first install and first workspace
- [`canopy-json.md`](canopy-json.md) — schema (same on local and remote)
- [`landscape.md`](landscape.md) — where remote dispatch sits next to mosh, sshfs, and friends
