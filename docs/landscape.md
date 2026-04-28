# Where canopy fits

Canopy lives at the intersection of three older categories: **git worktrees**, **terminal multiplexers**, and **AI coding agents**. Plenty of good tools exist in each one. Canopy's bet is that the *combination* — one workspace per branch, one tmux session per workspace, one AI agent attached to each, all persistent across reboots — is its own category.

This doc helps you locate canopy relative to tools you already know.

## If you came from Conductor

[Conductor.build](https://conductor.build/) is the inspiration. If you used it on macOS and miss it on Linux, canopy is the closest thing. The mental model is the same:

| Conductor | Canopy |
|---|---|
| One row per workspace, click to attach | `canopy ls` / TUI list, `enter` to attach |
| Setup script runs on workspace create | `scripts.setup` in `canopy.json` |
| Server pane runs your dev command | `scripts.run` (on-demand in v0; per-workspace port via `CANOPY_PORT`) |
| Archive script on workspace delete | `scripts.archive` |
| AI pane resumes prior conversation | `claude --continue \|\| claude` (or any agent CLI you configure) |
| Persistent across reboots | tmux sessions resurrect via `canopy switch` |

The biggest differences:

- **Terminal, not GUI.** Canopy is a Bubbletea TUI plus CLI subcommands. No window chrome, no system tray — it lives in your tmux/terminal stack like nvim or lazygit do.
- **Linux-first.** macOS works (the build is platform-agnostic Go); the dogfood loop runs on Arch/Hyprland.
- **Open source.** Apache-2.0 (planned at public release). Conductor is closed-source freeware.

The Conductor-compatibility shim is real: `canopy init` detects an existing `conductor.json` and adopts its script paths; `CONDUCTOR_*` env vars are exported alongside `CANOPY_*` ones so existing setup scripts keep working without edits. See [`docs/migrate-from-conductor.md`](migrate-from-conductor.md).

## If you came from tmuxinator / sesh / smug / zellij sessions

Tmux session managers are great at "save and restore my window/pane layout." Canopy ships *above* that layer:

- A tmuxinator-style YAML describes panes; canopy describes a **workspace** — a worktree, a branch, a port allocation, a setup script that ran once, an archive script that runs on cleanup, an agent's per-cwd conversation history.
- A session manager attaches you to an existing layout. Canopy *creates* the worktree + branch + tmux session as a single atomic operation, runs your `scripts.setup` once, and tracks the result in a state registry.
- Killing a tmuxinator session is just a tmux kill. `canopy rm` runs `scripts.archive` (drop the per-workspace DB, kill the dev server, etc.), removes the git worktree, deletes the underlying branch, and drops the state row.

If you're happy with tmuxinator and don't need worktrees or AI panes, canopy isn't a meaningful upgrade. If you've ever had three branches checked out under three sibling worktrees with three different `bin/dev` ports and three different in-progress claude conversations and lost track of which is which, that's the gap canopy fills.

## If you came from raw `git worktree`

Canopy uses `git worktree add` under the hood. What it adds on top:

- **Centralized storage.** Workspaces live at `~/.canopy/workspaces/<project>/<name>/` instead of polluting the source repo with sibling dirs. Your source tree stays clean.
- **Port allocation.** Each workspace gets a deterministic, stable TCP port via `CANOPY_PORT`. No more "wait, was branch X on 3001 or 3002?"
- **Lifecycle scripts.** `scripts.setup` runs on create, `scripts.archive` runs on remove. Idiomatic place for `db:create`/`db:drop`, dependency installs, secrets symlinks.
- **Tmux sessions per workspace.** Three-pane layout: nvim, agent (claude / aider / codex), shell. Persistent and resurrectable after reboots.
- **A registry.** `canopy ls` shows every workspace at a glance, with status (ready, stopped, broken, orphaned). No more `git worktree list | grep | awk` dances.

If your project doesn't need any of that — straight `git worktree add` is genuinely fine and one less dependency to think about. Canopy gets useful as soon as workspaces have non-trivial lifecycle (databases, ports, AI sessions).

## If you came from helmor / multi-agent IDEs

[helmor.ai](https://helmor.ai/) and similar local-first multi-agent IDEs are adjacent peers. The shape is different — they're full IDEs that orchestrate agents through review/test/merge phases; canopy is a terminal tool that handles the workspace setup + session persistence half. They'll likely co-exist for users who want both: helmor for the post-generation IDE loop, canopy for the per-workspace shell environment underneath.

If you live in your editor and want a single GUI for everything, helmor's shape probably fits better. If you live in tmux and your editor is one pane of three, canopy is shaped for you.

## What canopy hosts (not competes with)

Canopy puts *other people's tools* in panes. These are explicitly cooperative, not competitive:

### AI coding-agent CLIs

The agent pane runs whichever AI CLI you configure. Today defaults to `claude` (Claude Code); future versions make this per-project via `scripts.agent`. The agent's "resume prior conversation" semantics determine how clean canopy's resurrection feels — agents with per-directory storage and a non-interactive resume flag get the full magic; ones without get a fresh session each time.

| Tool | Resurrection support |
|---|---|
| [Claude Code](https://docs.claude.com/en/docs/claude-code) | Full — `claude --continue` resumes per-cwd |
| [aider](https://aider.chat/) | Full — `aider --restore-chat-history` |
| [Codex CLI](https://github.com/openai/codex) | Degraded — needs explicit thread ID; wrapper recommended |
| [opencode](https://opencode.ai/) | TBD — verify before relying on it |

### Editors

Top-left pane is `nvim` by default. Vim, helix, kakoune, or `$EDITOR` will work too — the layout config is becoming swappable. If you don't use a TUI editor, canopy isn't your tool.

### Multiplexer

Tmux for now. Pluggable backend (zellij, kitty session protocol, future Ghostty session persistence) is a v1 design goal — see `TODOS.md`.

### Runtime version managers

`scripts.setup` runs in your shell environment, so [mise](https://mise.jdx.dev/), [asdf](https://asdf-vm.com/), rbenv, nvm, etc. all work. Canopy doesn't replace these; it invokes scripts that use them.

### Procfile runners

`scripts.run` typically wraps [Foreman](https://github.com/ddollar/foreman) / [Overmind](https://github.com/DarthSim/overmind) / [Hivemind](https://github.com/DarthSim/hivemind) / `bin/dev` for projects that need multi-process dev servers. Canopy passes `CANOPY_PORT` through; the Procfile picks it up.

## What canopy explicitly isn't

- **Not a Kubernetes devloop.** [Tilt](https://tilt.dev/), Skaffold, and friends solve cluster-shaped problems. Canopy is workspace-shaped: one branch, one process group, one local port.
- **Not a code-generation harness.** Canopy doesn't generate code, run agents non-interactively, or orchestrate multi-step plans. It hosts the shell environment where you run those agents yourself.
- **Not a CI tool.** No remote runners, no scheduled jobs. The lifecycle scripts run on your laptop, attached to your terminal.
- **Not opinionated about your editor or shell.** It is opinionated about tmux and `git worktree`. If you've already chosen something else for either, canopy isn't a fit.

## Further reading

- [`docs/getting-started.md`](getting-started.md) — 5-minute install + first workspace
- [`docs/canopy-json.md`](canopy-json.md) — schema for `canopy.json`
- [`docs/migrate-from-conductor.md`](migrate-from-conductor.md) — for projects already on Conductor
- [`docs/architecture.md`](architecture.md) — how the codebase is laid out
