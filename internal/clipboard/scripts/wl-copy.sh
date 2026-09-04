#!/usr/bin/env bash
# Canopy clipboard-bridge wrapper for wl-copy (OSC 52).
# Managed by canopy {{.Version}}. Do not edit; reinstall via
#   canopy host clipboard <name> --reinstall
#
# Writes an OSC 52 "set clipboard" escape sequence to the tty instead
# of proxying through a persistent SSH tunnel + laptop daemon. The
# LOCAL terminal emulator (foot, kitty, alacritty, wezterm, iTerm2,
# ...) intercepts the sequence and writes to the real system
# clipboard — it round-trips through ssh/mosh + tmux passthrough
# exactly like any other terminal escape sequence, so there's nothing
# for canopy to install, keep alive, or go stale. Same mechanism
# tmux-yank-osc52, neovim's OSC52 clipboard provider, and standalone
# tools like theimpostor/osc already rely on.
#
# Supports the two invocation forms wl-copy itself supports:
#   wl-copy "some text"   (argv)
#   wl-copy < file         (stdin)
#
# Flags we silently accept then ignore — they're meaningful to real
# wl-copy but don't apply to an OSC 52 "set clipboard, once" write:
#   -n / --trim-newline      we never appended one in the first place
#   -o / --paste-once        no daemon state to make "single-use"
#   -p / --primary           OSC 52 "c" target is the system clipboard;
#                             there's no separate PRIMARY selection to
#                             address over this channel
#   -c / --clear             use a separate `clear` action if needed
#   -f / --foreground        we don't fork
#   -t / --type              clipboard payload is opaque bytes here
#
# Requires the local terminal to have OSC 52 write access enabled
# (foot: `osc52=enabled` or `copy-enabled`, both defaults in recent
# foot; other terminals vary — see docs/clipboard-bridge.md).

set -euo pipefail

while [ $# -gt 0 ]; do
  case "$1" in
    -n|--trim-newline|-o|--paste-once|-p|--primary|-c|--clear|-f|--foreground)
      shift ;;
    -t|--type) shift 2 ;;
    --type=*)  shift ;;
    -h|--help)
      echo "canopy wl-copy wrapper (managed by canopy; OSC 52)" >&2
      exit 0 ;;
    --) shift; break ;;
    *) break ;;
  esac
done

if [ $# -gt 0 ]; then
  payload="$*"
else
  payload=$(cat)
fi

# base64 -w0 isn't portable (BSD base64 lacks -w); `tr -d '\n'` strips
# whatever line-wrapping the local base64 implementation applies,
# which matters here because an embedded newline would split the OSC
# sequence and corrupt it.
b64=$(printf '%s' "$payload" | base64 | tr -d '\n')

osc=$'\033]52;c;'"${b64}"$'\033\\'
if [ -n "${TMUX:-}" ]; then
  # Running inside tmux: a raw OSC sequence written by a program
  # inside a pane is consumed by tmux itself, not forwarded to the
  # OUTER terminal tmux is running in. tmux's DCS passthrough
  # (`\033Ptmux;...\033\\`) tells tmux "relay this to the real
  # terminal instead" — but every literal ESC (\033) inside the
  # payload must be doubled, or tmux's own DCS parser terminates the
  # passthrough early at the first embedded ESC.
  osc=$'\033Ptmux;'"$(printf '%s' "$osc" | sed $'s/\033/\033\033/g')"$'\033\\'
fi
printf '%s' "$osc" > /dev/tty
