# Clipboard bridge

Makes the laptop's clipboard available inside any remote canopy workspace,
for **text** in both directions — copy on the remote, it lands on your
laptop; paste on your laptop, it shows up on the remote. Copy text from a
remote tmux session straight to your local clipboard. Paste text into
Claude Code running on a remote host. No daemon, no persistent tunnel, no
per-session setup once installed.

Design doc: [`docs/design/v0.18-clipboard-bridge.md`](design/v0.18-clipboard-bridge.md)
(see its OSC52 follow-up section for the mechanism described below —
the doc's main body describes the original daemon/tunnel design, which
this replaced).

> **Images are not supported.** The bridge moved from a socket-forwarding
> daemon to OSC 52 terminal escape sequences, which can't carry
> screenshot-sized payloads. Text only. See "How it works" below.

**Zero-setup with `canopy --remote <host>`.** If you're using the
thin-client `--remote` mode (see
[`remote-workspaces.md`](remote-workspaces.md#thin-client-mode-zero-setup-with-canopy---remote-host)),
skip the Setup section below entirely — the first successful connection
installs the bridge unattended, with a one-line status notice instead of
the manual flow's full transcript. The manual steps below still apply if
you're using the registered `--on <host>` flow, or want to install ahead
of time.

## What works

| Direction | Mechanism | Notes |
|---|---|---|
| Remote `wl-copy "foo"` → local clipboard | `wl-copy` wrapper writes an OSC 52 "set clipboard" sequence to `/dev/tty` | Requires an attached terminal session (SSH or mosh both fine) |
| Remote `wl-paste` → reads local clipboard | `wl-paste` wrapper writes an OSC 52 query sequence and reads the terminal's reply | Requires the terminal to support OSC 52 *read* — see Caveats |
| Remote tmux copy-mode `y` / `Enter` → local clipboard | tmux `copy-pipe-and-cancel "wl-copy"` (auto-configured by install) | Needs `allow-passthrough on` (auto-configured) |
| Remote tmux copy-mode mouse-drag → local clipboard | tmux `MouseDragEnd1Pane` binding (auto-configured) | Same |
| Remote tmux copy-mode `Ctrl+Shift+C` → local clipboard | tmux `C-S-c` binding | Works over SSH-attach; see Caveats for mosh |
| Local clipboard text → remote terminal (Ctrl-V in shell) | Terminal pty bracketed-paste; no bridge needed | Always worked, unrelated to this bridge |
| Remote nvim yank → local clipboard | `set clipboard=unnamedplus` (manual; install prints the hint) | |
| Image paste (local → remote, or remote → local) | **Not supported** | OSC 52 payloads are too size-constrained for images; would need a different transport (tracked as a future PTY-proxy rewrite) |

## Setup

One command, per remote host:

```bash
canopy host clipboard <host-name>
```

…or press `c` on the host's row in the Hosts tab. `<host-name>` doesn't
have to be registered — it resolves the same way `--remote`/`--on` do (a
registered `hosts.json` name, or a raw SSH target used directly), so you
can install against a box you haven't run `canopy host add` on. There is
no separate one-time laptop-side step — OSC 52 needs no daemon or SSH
config on your laptop.

What this does, in order:

1. Pushes the canopy wrapper scripts to `~/.local/bin/wl-paste` and
   `~/.local/bin/wl-copy` on the remote. They take precedence over the
   system wl-clipboard binaries when `~/.local/bin` is first on `$PATH`.
2. Splices the tmux copy-mode bindings into the remote's `~/.tmux.conf`
   between marker comments, including `set -g allow-passthrough on`
   (required on tmux 3.3+, which defaults it off — without it, tmux
   silently eats the escape sequences the wrappers emit). Re-running
   rewrites the block; nothing outside the markers is touched.
3. Reloads tmux on the remote via `tmux source-file ~/.tmux.conf`
   (best-effort; silent if no tmux server is running there).
4. Removes any pre-OSC52 artifacts an older canopy version left on
   *this* laptop: the `canopy-clipboard-tunnel-<host-name>.service` and
   `canopy-clipboard.service` systemd user units, and the per-host SSH
   snippet at `~/.ssh/config.d/canopy/<host-name>.conf`. Silent no-op if
   none of these exist.
5. Confirms the wrapper resolves on the remote's login-shell PATH
   (`command -v wl-paste`). If PATH precedence is wrong, install
   completes with a warning + the one-line fix instruction.

Idempotent. Re-running on an already-installed host refreshes every
artifact in place.

Note what step 5 does *not* do: it doesn't attempt to verify OSC 52
actually round-trips. That check would need a real attached terminal
(tty), and the install runs over a non-interactive SSH connection with
no tty at all. See "How it works" below for why, and what that means for
the Hosts tab's `📋 bridged` pill.

### Manual nvim step (optional)

To make nvim's yank land in the laptop clipboard, on the remote add to
your nvim init:

```lua
vim.opt.clipboard = "unnamedplus"   -- init.lua
```

or

```vim
set clipboard+=unnamedplus           " init.vim
```

After this, `yy`, `yiw`, `y$` etc. all reach the local clipboard.

## Caveats

### Your terminal must support OSC 52

The bridge is only as good as the terminal emulator you're actually
sitting in front of — OSC 52 is handled by the *outer* terminal, not by
canopy or by SSH/mosh. Most modern terminals support the *write*
direction (remote → local clipboard) by default; the *read* direction
(local → remote, i.e. `wl-paste`) is disabled by default in some
terminals for security reasons (a malicious program could otherwise
silently read your clipboard).

- **foot** (the reference terminal this bridge was built against):
  `osc52` config option, defaults to `enabled` (both directions).
- Other terminals: check their docs for an OSC 52 / clipboard setting.
  If `wl-paste` on the remote times out with "local terminal did not
  respond to the OSC 52 clipboard query," the read direction is
  disabled or unsupported in your terminal — `wl-copy` (write) may
  still work fine.

### tmux must have `allow-passthrough on`

Install configures this automatically (step 2 above). If you edit
`~/.tmux.conf` by hand afterward and remove or shadow that line, the
bridge silently stops working — tmux 3.3+ eats the DCS-wrapped escape
sequences instead of relaying them to the outer terminal, and there's no
error, just no clipboard traffic. To check:

```bash
tmux show -g allow-passthrough
# expect: allow-passthrough on
```

### Mosh has exactly ONE limitation: `Ctrl+Shift+C` in tmux copy-mode

The OSC 52 mechanism itself is attach-method-agnostic — it's just bytes
written to and read from the pty, which both SSH and mosh carry
transparently. `wl-copy`, `wl-paste`, tmux copy-mode `y` / `Enter` /
mouse-drag-release, nvim yank — every one of these works identically
over both attach methods.

The single thing that differs is **`Ctrl+Shift+C` in tmux copy-mode**.
Mosh's wire protocol normalizes input — it doesn't forward the CSI-u /
modifyOtherKeys escape sequences that tmux 3.2+ uses to distinguish
`Ctrl+Shift+C` from plain `Ctrl+C`. Your terminal generates the right
sequence, but mosh boils it down to `Ctrl+C` by the time tmux sees it.
The canopy `bind-key -T copy-mode-vi C-S-c …` we install doesn't match
because tmux only ever sees `C-c` over mosh. (This caveat predates the
OSC 52 rewrite and is unrelated to it — it's about the keybinding never
firing, not about what happens once it does.)

Workarounds:

- Use `y` or `Enter` in tmux copy-mode — both bypass extended-keys
  entirely and always work.
- Hold **Shift** while dragging to bypass tmux mouse mode; the terminal
  app handles the selection natively, and its own `Ctrl+Shift+C`
  keybinding copies via the local clipboard with no bridge involved.
- SSH-attach instead of mosh-attach if `Ctrl+Shift+C` muscle memory is
  non-negotiable. Plain SSH passes the escape sequence through
  faithfully and the binding fires.

**Everything else about the bridge works fine over mosh.** Don't switch
attach methods preemptively — mosh's resilience to network churn is a
real benefit; only ssh-attach when `Ctrl+Shift+C` specifically matters.

### Terminal emulator may eat `Ctrl+Shift+C` before tmux sees it

Even outside mosh, some terminal apps (alacritty, kitty, ghostty, foot,
ptyxis) bind `Ctrl+Shift+C` at the terminal level to "copy the
terminal's own selection to system clipboard" and consume the keystroke
before it reaches the running program. If your terminal does that AND
you're SSH-attached AND tmux's copy-mode binding still doesn't fire,
rebind the terminal's copy action to a different key (commonly
`Ctrl+Shift+Y`) so `Ctrl+Shift+C` passes through to tmux.

This is rare in practice — most terminals only "copy" on
`Ctrl+Shift+C` when text is visually selected; with no selection, they
pass the keystroke through. But if your terminal is more aggressive,
this is the fix.

### canopy switch defaults to mosh

`canopy switch` (and the TUI's Enter on a remote row) attach via mosh by
default. Combined with the mosh+extended-keys caveat above, mosh-attached
sessions get the tmux bindings *except* `Ctrl+Shift+C`. `y` and `Enter`
cover most copy-mode workflows.

If you want SSH-attach instead (for `Ctrl+Shift+C` fidelity, or because
you're on a network where UDP is awkward), pass `--no-mosh`:

```bash
canopy switch --on tower fix-the-bug --no-mosh
```

This attaches over an ssh reconnect-loop instead of mosh — it re-dials
with backoff on a dropped connection rather than kicking you back to a
shell. The same flag works on `canopy --remote <host> --no-mosh` for the
thin-client mode. If mosh isn't installed locally at all, canopy now
falls back to this path automatically, so you don't have to remember the
flag just because a box is missing mosh.

## Troubleshooting

### `wl-copy` runs but nothing lands on the laptop clipboard

On the remote:

```bash
which wl-copy                              # should be ~/.local/bin/wl-copy
echo -n "hello from remote" | wl-copy
```

If `which` reports `/usr/bin/wl-copy`:

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
# then re-attach the session so the new shell picks up PATH
```

If `which` is correct but the paste never shows up on the laptop:

- Confirm you're inside a real attached terminal session (not, say, a
  cron job or a canopy background probe) — OSC 52 needs a tty.
- If you're inside tmux: `tmux show -g allow-passthrough` — must say
  `on`.
- Try the same command *outside* tmux (`tmux detach` or a fresh SSH
  session) to isolate whether tmux passthrough or the terminal itself
  is the problem.
- Check your terminal's OSC 52 setting (see Caveats above).

### `wl-paste` times out or returns empty

```bash
wl-paste
# expect: your laptop clipboard's text content
```

A timeout ("local terminal did not respond to the OSC 52 clipboard
query") almost always means your terminal has OSC 52 *read* disabled —
this is a common default for security reasons. Check your terminal's
docs; foot enables it by default (`osc52=enabled`). Writing (`wl-copy`)
can still work even when reading doesn't, since terminals treat the two
directions as separate risk levels.

### Hosts tab shows `📋 bridged` but it doesn't actually work

This is expected in one specific case: the `📋 bridged` pill means "the
wrapper scripts are installed on this host," not "OSC 52 is confirmed
working right now." canopy's background health probe (`canopy ls
--json`, refreshed every ~2s) runs over a non-interactive SSH connection
with no tty — there's no terminal on the other end to answer an OSC 52
query, so liveness genuinely can't be checked from there. The wrapper
scripts themselves fail loudly (non-zero exit, clear stderr) the moment
you actually try to use them from a real attached session — that's
where to look if something's wrong, not the pill.

### Diagnostic checklist (one-liners)

```bash
# Remote side
ssh <host> 'which wl-paste; which wl-copy'
ssh <host> 'grep "canopy:start" ~/.tmux.conf'
ssh <host> 'tmux show -g allow-passthrough' # run from inside an attached tmux session on the remote, not over batch ssh

# End-to-end, from an actual attached session on the remote (not batch ssh)
echo -n "canopy clipboard test" | wl-copy
wl-paste
```

## What's not supported

| Feature | Status | Reason |
|---|---|---|
| Image paste (either direction) | Not supported | OSC 52 payloads are too size-constrained for screenshots; needs a different transport — tracked as a future PTY-proxy rewrite |
| X11 selection clipboard (PRIMARY) | Out of scope | OSC 52 has no notion of a separate selection buffer |
| Continuous clipboard sync | Out of scope | The bridge is request/response (an explicit copy/paste), not a live sync; a different security model entirely |
| Audit logging of clipboard content | Out of scope | Compliance feature; needs separate security review |
| Mobile clipboard (phone → remote) | Out of scope | Different transport entirely (ntfy / Tailscale Funnel) |
| Auto-detection of "no PATH precedence" + auto-fix | Not yet | Install warns; auto-fix is fiddly across shells |
| Verifying OSC 52 liveness from the background health probe | Not possible | Requires a real attached tty; the probe runs over batch SSH — see "Hosts tab shows bridged but it doesn't work" above |

## How it works

Each wrapper script (`~/.local/bin/wl-copy`, `~/.local/bin/wl-paste`) is
just a shell script that talks directly to the terminal, not to a
daemon:

- **`wl-copy`** base64-encodes its input and writes an OSC 52 "set
  clipboard" escape sequence — `\033]52;c;<base64>\033\\` — straight to
  `/dev/tty`. The outer terminal emulator intercepts this sequence and
  sets its own system clipboard. No network hop, no socket, no laptop
  process involved.
- **`wl-paste`** writes an OSC 52 *query* sequence (`\033]52;c;?\033\\`)
  to `/dev/tty`, then reads the terminal's base64-encoded reply back off
  stdin.
- **Inside tmux**, both directions wrap the sequence in tmux's DCS
  passthrough envelope (`\033Ptmux;...\033\\`) so it reaches the *outer*
  terminal instead of being consumed by tmux itself — this requires
  `allow-passthrough on`, which install configures automatically (tmux
  3.3+ defaults it off).

Because the terminal emulator does the actual clipboard work, this needs
no daemon, no persistent SSH tunnel, no forwarded sockets, and no `socat`
on the remote — a meaningful simplification over the original
daemon/tunnel design (still described in
[`docs/design/v0.18-clipboard-bridge.md`](design/v0.18-clipboard-bridge.md)'s
main body). The tradeoff is the one covered throughout this doc: OSC 52
payloads are size-limited (no images) and liveness depends entirely on
the terminal you're actually sitting in front of.
