# Clipboard bridge

v0.18 makes the laptop's clipboard available inside any remote canopy
workspace. Paste a screenshot you copied on your laptop into Claude
Code running on a remote host. Copy text from a remote tmux session
straight to your local clipboard. None of it requires per-session
setup once installed.

Design doc: [`docs/design/v0.18-clipboard-bridge.md`](design/v0.18-clipboard-bridge.md).

**Zero-setup with `canopy --remote <host>` (v0.23.0.0).** If you're using the
thin-client `--remote` mode (see
[`remote-workspaces.md`](remote-workspaces.md#thin-client-mode-zero-setup-with-canopy---remote-host)),
skip the Setup section below entirely — the first successful connection
installs the bridge unattended (laptop-side daemon/SSH config, then the
per-host wrappers and tunnel), with a one-line status notice instead of the
manual flow's full transcript. The manual steps below still apply if you're
using the registered `--on <host>` flow, or want to install ahead of time.

## What works (verified end-to-end)

| Direction | Mechanism | Verified |
|---|---|---|
| Local clipboard image → Claude Code on remote (Ctrl-V) | `wl-paste --type image/png` wrapper → SSH `RemoteForward` → laptop daemon → local Wayland | ✅ |
| Local clipboard text → Claude Code on remote (Ctrl-V) | Same path as image | ✅ |
| Local clipboard text → remote terminal (Ctrl-V in shell) | Terminal pty bracketed-paste; no bridge needed | ✅ |
| Remote `wl-copy "foo"` → local Wayland clipboard | `wl-copy` wrapper → `clip-copy.sock` → SSH tunnel → laptop daemon | ✅ |
| Remote tmux copy-mode `y` / `Enter` → local clipboard | tmux `copy-pipe-and-cancel "wl-copy"` (auto-configured by install) | ✅ |
| Remote tmux copy-mode mouse-drag → local clipboard | tmux `MouseDragEnd1Pane` binding (auto-configured) | ✅ |
| Remote tmux copy-mode `Ctrl+Shift+C` → local clipboard | tmux `C-S-c` binding — works over SSH-attach; see Caveats for mosh | ⚠ |
| Remote nvim yank → local clipboard | `set clipboard=unnamedplus` (manual; install prints the hint) | ⚠ |

## Setup

Two commands. The first is per-laptop and one-time; the second is per
remote host.

### 1. One-time per laptop

```bash
canopy install clipboard-bridge
```

This writes:

- `~/.config/systemd/user/canopy-clipboard.service` — the local daemon
  unit; launches `canopy clipboard-server` and supervises it across
  session restarts. ExecStart points at `~/.local/bin/canopy` (the
  symlink that `canopy use` swaps), so dev-vs-release binary switches
  don't break the daemon.
- `~/.ssh/config.d/canopy/` — directory for per-host SSH snippets.
- `~/.ssh/config` — adds a marker-bounded block that `Include`s the
  snippets above. Wrapped in `Host *` so it loads regardless of where
  in your existing `~/.ssh/config` the marker block ends up.

Then enables + starts the daemon. If your shell's `WAYLAND_DISPLAY`
is set, the install also runs `systemctl --user import-environment
WAYLAND_DISPLAY ...` so the daemon's environment carries the Wayland
session info on first start.

The daemon listens on three Unix sockets in `$XDG_RUNTIME_DIR/canopy/`:

- `clip-text.sock` — serves clipboard text on read
- `clip-image.sock` — serves clipboard image bytes on read
- `clip-copy.sock` — receives bytes on write, sets clipboard

### 2. Per remote host

```bash
canopy host clipboard <host-name>
```

…or press `c` on the host's row in the Hosts tab. As of v0.23.0.0, `<host-name>`
doesn't have to be registered — it resolves the same way `--remote`/`--on` do
(a registered `hosts.json` name, or a raw SSH target used directly), so you
can install against a box you haven't run `canopy host add` on.

What this does, in order:

1. SSHes to the remote and detects the user's UID via `id -u`.
2. `mkdir -p /run/user/<uid>/canopy` on the remote, mode `0700`.
   sshd binds RemoteForward sockets here later; the directory must
   exist or `bind()` returns `ENOENT`.
3. Pushes the canopy wrapper scripts to `~/.local/bin/wl-paste` and
   `~/.local/bin/wl-copy` on the remote. They take precedence over
   the system wl-clipboard binaries when `~/.local/bin` is first on
   `$PATH`.
4. Writes `~/.ssh/config.d/canopy/<host-name>.conf` on your laptop
   with a dedicated `Host canopy-tunnel-<host-name>` alias and three
   `RemoteForward` directives pointing at the daemon's sockets.
5. Writes `~/.config/systemd/user/canopy-clipboard-tunnel-<host-name>.service`
   — a systemd user unit that holds an `ssh -N canopy-tunnel-<host-name>`
   tunnel open persistently and supervises restart on failure.
6. Splices the tmux copy-mode bindings into the remote's
   `~/.tmux.conf` between marker comments. Re-running rewrites the
   block; nothing outside the markers is touched.
7. Reloads tmux on the remote via `tmux source-file ~/.tmux.conf`
   (best-effort; silent if no tmux server is running there).
8. Verifies the chain end-to-end: tunnel unit active, wrapper
   round-trips `text/plain` over a fresh SSH, PATH on the remote
   puts `~/.local/bin/wl-paste` ahead of `/usr/bin/wl-paste`. If
   PATH precedence fails, install completes with a warning + the
   one-line fix instruction.

Idempotent. Re-running on an already-installed host refreshes every
artifact in place.

### Manual nvim step (optional)

To make nvim's yank land in the laptop clipboard, on the remote add
to your nvim init:

```lua
vim.opt.clipboard = "unnamedplus"   -- init.lua
```

or

```vim
set clipboard+=unnamedplus           " init.vim
```

After this, `yy`, `yiw`, `y$` etc. all reach the local clipboard.

## Caveats

### Phase 1 supports Wayland local + Linux remote only

- Local must be running a Wayland compositor with `wl-clipboard`
  installed. The daemon's `Detect()` keys on `WAYLAND_DISPLAY` and
  refuses to start without it.
- Remote must be Linux with `socat` available (or installable via
  the system package manager when the install runs).
- X11 (`xclip`) local support and macOS (`pbpaste`/`pbcopy`) local
  support are Phase 2; the `Provider` interface is in place so each
  is a single-file addition when there's a user who needs it.
- macOS as a remote target is not supported. v0.17.x's remote
  workspaces feature is Linux-only on the remote; v0.18 inherits that.

### Tailscale SSH must be off for the bridged hostname

Tailscale's embedded SSH server (`tailscale set --ssh=true`) doesn't
support Unix-socket forwarding. If your remote host runs Tailscale
SSH, every connection routes through Tailscale's ssh implementation
instead of OpenSSH, the `RemoteForward` directives are silently
dropped, and the bridge fails with no sockets on the remote.

To check:

```bash
ssh -v <host> true 2>&1 | grep "remote software"
# OpenSSH_X.Y → fine
# Tailscale → blocking the bridge
```

To fix on the remote:

```bash
sudo tailscale set --ssh=false
```

(You can still use Tailscale for the network path; this only disables
Tailscale's in-band SSH server in favor of OpenSSH on the same host.)

### Mosh has exactly ONE limitation: `Ctrl+Shift+C` in tmux copy-mode

The bridge is **attach-method-agnostic**. The persistent SSH tunnel
(the systemd unit) holds the RemoteForward sockets open continuously
on the remote. When you mosh OR ssh to the host, Claude/nvim/shell
all reach the wrapper through the same live sockets — image paste,
text paste, command-line `wl-copy`, tmux copy-mode `y` / `Enter` /
mouse-drag-release, nvim yank — every one of these works identically
over both attach methods.

The single thing that differs is **`Ctrl+Shift+C` in tmux copy-mode**.
Mosh's wire protocol normalizes input — it doesn't forward the CSI-u /
modifyOtherKeys escape sequences that tmux 3.2+ uses to distinguish
`Ctrl+Shift+C` from plain `Ctrl+C`. Your terminal generates the right
sequence, but mosh boils it down to `Ctrl+C` by the time tmux sees it.
The canopy `bind-key -T copy-mode-vi C-S-c …` we install doesn't match
because tmux only ever sees `C-c` over mosh.

Workarounds:

- Use `y` or `Enter` in tmux copy-mode — both bypass extended-keys
  entirely and always work.
- Hold **Shift** while dragging to bypass tmux mouse mode; the
  terminal app handles the selection natively, and its own
  `Ctrl+Shift+C` keybinding copies via the local clipboard with no
  bridge involved.
- SSH-attach instead of mosh-attach if `Ctrl+Shift+C` muscle memory
  is non-negotiable. Plain SSH passes the escape sequence through
  faithfully and the binding fires.

**Everything else about the bridge works fine over mosh.** Don't
switch attach methods preemptively — mosh's resilience to network
churn is a real benefit; only ssh-attach when `Ctrl+Shift+C`
specifically matters.

### Terminal emulator may eat `Ctrl+Shift+C` before tmux sees it

Even outside mosh, some terminal apps (alacritty, kitty, ghostty,
foot, ptyxis) bind `Ctrl+Shift+C` at the terminal level to "copy
the terminal's own selection to system clipboard" and consume the
keystroke before it reaches the running program. If your terminal
does that AND you're SSH-attached AND tmux's copy-mode binding still
doesn't fire, rebind the terminal's copy action to a different key
(commonly `Ctrl+Shift+Y`) so `Ctrl+Shift+C` passes through to tmux.

This is rare in practice — most terminals only "copy" on
`Ctrl+Shift+C` when text is visually selected; with no selection,
they pass the keystroke through. But if your terminal is more
aggressive, this is the fix.

### Single laptop per host at a time

Two laptops both bridging to the same remote host will fight over the
RemoteForward sockets. SSH's `StreamLocalBindUnlink yes` makes the
*second* tunnel win — its sockets replace the first laptop's. From
the user's perspective: "I pasted on laptop A, the image came from
laptop B's clipboard."

Multi-laptop arbitration (with explicit ownership tokens or per-laptop
namespacing) is parked for v0.19+.

### sshd config requirements on the remote

The remote's sshd must allow Unix-socket forwarding:

```
# In /etc/ssh/sshd_config (or sshd_config.d/), if explicitly set:
AllowStreamLocalForwarding yes
```

This is the **default** on every OpenSSH 6.7+ install, so unless your
host has a hardening config that explicitly flipped it to `no` or
`local`, you don't have to do anything. To check:

```bash
ssh <host> 'sudo sshd -T 2>&1 | grep -i streamlocal'
# expect: allowstreamlocalforwarding yes
```

If your config has it set to `no` or `local`, add to
`/etc/ssh/sshd_config.d/99-canopy-clipboard.conf` on the remote:

```
AllowStreamLocalForwarding yes
```

Then `sudo systemctl reload sshd`.

### Daemon doesn't auto-start on tty1 / virtual console

The systemd user unit's `WantedBy=graphical-session.target` ties the
daemon's lifecycle to the Wayland compositor's session. If you log
into a TTY (`tty1`, `tty2`, etc.) without starting a graphical
session, the daemon stays inactive. That's correct behavior — there's
no clipboard for it to bridge. But if you SSH back into your own
laptop and try `canopy clipboard-server` from a non-graphical context,
you'll see:

```
canopy clipboard-server: clipboard.Detect: no clipboard provider
detected on this system. Phase 1 supports Wayland only. ...
```

Not a bug — the daemon refuses to start where there's no clipboard
backend. Run the bridge from your normal graphical session.

### canopy switch defaults to mosh

`canopy switch` (and the TUI's Enter on a remote row) attach via
mosh by default. Combined with the mosh+extended-keys caveat above,
mosh-attached sessions get the tmux bindings *except* `Ctrl+Shift+C`.
`y` and `Enter` cover most copy-mode workflows.

If you want SSH-attach instead (for `Ctrl+Shift+C` fidelity, or
because you're on a network where UDP is awkward), pass `--no-mosh`
(v0.23.0.0):

```bash
canopy switch --on tower fix-the-bug --no-mosh
```

This attaches over an ssh reconnect-loop instead of mosh — it re-dials
with backoff on a dropped connection rather than kicking you back to a
shell. The same flag works on `canopy --remote <host> --no-mosh` for the
thin-client mode. If mosh isn't installed locally at all, canopy now falls
back to this path automatically, so you don't have to remember the flag
just because a box is missing mosh.

## Troubleshooting

### Daemon doesn't start

```bash
systemctl --user status canopy-clipboard
journalctl --user -u canopy-clipboard -n 20
```

Common causes:

- **WAYLAND_DISPLAY not in systemd user manager env** — happens when
  the compositor doesn't auto-import the env. Fix:
  ```bash
  systemctl --user import-environment WAYLAND_DISPLAY XDG_RUNTIME_DIR
  systemctl --user restart canopy-clipboard
  ```
  For permanence (Hyprland users), add to `~/.config/hypr/hyprland.conf`:
  ```
  exec-once = dbus-update-activation-environment --systemd WAYLAND_DISPLAY XDG_CURRENT_DESKTOP
  ```
  omarchy's default Hyprland config does this already.

- **wl-clipboard not installed** — `pacman -S wl-clipboard` (Arch) /
  `apt install wl-clipboard` (Debian) / equivalent.

### Tunnel unit failing

```bash
systemctl --user status canopy-clipboard-tunnel-<host>
journalctl --user -u canopy-clipboard-tunnel-<host> -n 30
```

Common causes:

- **Tailscale SSH active** — see Caveats above.
- **sshd `AllowStreamLocalForwarding no`** — see Caveats above.
- **Stale sockets** — install's `ExecStartPre` should clean them, but
  if you've been mid-debugging:
  ```bash
  ssh <host> 'rm -f /run/user/$(id -u)/canopy/clip-*.sock'
  systemctl --user restart canopy-clipboard-tunnel-<host>
  ```

### `wl-paste --list-types` returns nothing useful

On the remote:

```bash
which wl-paste                             # should be ~/.local/bin/wl-paste
wl-paste --list-types
# expect: text/plain;charset=utf-8 (always) + image/png (if image in local clipboard)
```

If `which` reports `/usr/bin/wl-paste`:

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
# then re-attach the session so the new shell picks up PATH
```

If `which` is correct but `--list-types` is empty / errors out:

```bash
# socket exists?
ls -la /run/user/$(id -u)/canopy/
# bridge alive? (on local)
systemctl --user status canopy-clipboard-tunnel-<host>
```

### "Pasted text" instead of "[Image attached]" in Claude on remote

The bridge is working in the text direction but not the image. Two
checks:

```bash
# 1. on local — does your clipboard ACTUALLY have a PNG?
wl-paste --list-types
# need: image/png in the output. If not, re-screenshot.

# 2. on remote — does the wrapper get the image?
wl-paste --type image/png | file -
# expect: /dev/stdin: PNG image data, ...
```

If #2 shows PNG bytes, the bridge is fine and Claude Code itself
needs a restart (it may have cached an earlier failure).

### Diagnostic checklist (one-liners)

```bash
# Local side
systemctl --user is-active canopy-clipboard
systemctl --user is-active canopy-clipboard-tunnel-<host>
ls -la /run/user/$(id -u)/canopy/
grep "canopy:start" ~/.ssh/config
cat ~/.ssh/config.d/canopy/<host>.conf | grep Host

# Remote side
ssh <host> 'which wl-paste'
ssh <host> 'ls -la /run/user/$(id -u)/canopy/'
ssh <host> 'grep "canopy:start" ~/.tmux.conf'

# End-to-end
ssh <host> '$HOME/.local/bin/wl-paste --list-types'
```

## What's not supported

| Feature | Status | Reason |
|---|---|---|
| X11 local | Phase 2 | Single-file Provider addition; not load-bearing for daily users |
| macOS local | Phase 2 | Same as X11 |
| macOS remote | Out of scope | v0.17.x is Linux-only on remote |
| Windows local | Out of scope | WSL counts as Linux; native Windows is out of scope project-wide |
| Multi-laptop arbitration | v0.19+ | Last writer wins; tracked as a known limitation |
| Clipboard content sync (continuous) | Out of scope | Bridge is request/response; continuous sync needs a different security model |
| Selection clipboard (X11 PRIMARY) | Out of scope | Wayland-only Phase 1 doesn't have a notion of selection |
| Audit logging of clipboard content | Out of scope | Compliance feature; needs separate security review |
| Drag-drop image from browser to remote terminal | Out of scope | Terminal protocols don't carry image-drop today |
| Mobile clipboard (phone → remote) | Out of scope | Different transport entirely (ntfy / Tailscale Funnel) |
| Auto-detection of "no PATH precedence" + auto-fix | v0.18.x | Install warns; auto-fix is fiddly across shells |

## How it works (one-paragraph version)

A local Go daemon (`canopy clipboard-server`) holds three Unix sockets
in `$XDG_RUNTIME_DIR/canopy/`. SSH's `RemoteForward` directive
forwards those sockets to identical paths on the remote host via a
persistent `ssh -N` connection that a systemd user unit supervises.
On the remote, the canopy wrapper scripts at `~/.local/bin/wl-paste`
and `~/.local/bin/wl-copy` use `socat` to connect to those
SSH-forwarded sockets — every read/write proxies back to the laptop's
real Wayland clipboard via the daemon. Tmux's copy-mode is wired to
the wrappers via auto-installed `bind-key copy-pipe-and-cancel
"wl-copy"` lines in the remote's `~/.tmux.conf`. Claude Code, nvim,
and anything else on the remote that calls `wl-paste` / `wl-copy`
flows through this chain transparently — no application changes
required.

For the full architecture and the design-decision history, see
[`docs/design/v0.18-clipboard-bridge.md`](design/v0.18-clipboard-bridge.md).
