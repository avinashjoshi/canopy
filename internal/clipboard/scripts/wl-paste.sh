#!/usr/bin/env bash
# Canopy clipboard-bridge wrapper for wl-paste (OSC 52).
# Managed by canopy {{.Version}}. Do not edit; reinstall via
#   canopy host clipboard <name> --reinstall
#
# Queries the LOCAL terminal's clipboard via an OSC 52 "get clipboard"
# escape sequence instead of proxying through a persistent SSH tunnel
# + laptop daemon. Round-trips through ssh/mosh + tmux passthrough:
# this wrapper writes the query to the tty, the local terminal
# emulator (foot, kitty, alacritty, wezterm, iTerm2, ...) intercepts
# it, reads the REAL system clipboard, and writes an OSC 52 reply back
# down the same tty — which this wrapper reads in raw mode. Nothing
# persistent to install, keep alive, or go stale.
#
# OSC 52 "get" (reading the local clipboard from a remote program) is
# the more security-sensitive direction — many terminals disable it by
# default even when "set" (copy) is enabled. foot defaults to
# `osc52=enabled` (both directions); see docs/clipboard-bridge.md for
# other terminals. If the local terminal doesn't answer within the
# timeout, every command below fails closed (non-zero exit, no
# fabricated output) rather than hanging or silently returning empty.
#
# Handles the subset of wl-paste flags Claude Code uses for clipboard
# reads:
#   wl-paste                       text, with trailing newline
#   wl-paste --no-newline          text, no trailing newline
#   wl-paste --type text/plain     text
#   wl-paste --list-types          newline-delimited mime types
#
# Image support (--type image/png) is NOT implemented over OSC 52 —
# terminals cap OSC 52 payload size well below what a screenshot
# needs, which is also why herdr bridges images via a temp file over
# its control channel instead of terminal escape sequences. Canopy's
# image-paste bridge is tracked as separate, later work; for now
# --type image/png fails clearly rather than silently returning
# nothing.

set -euo pipefail

# osc52_timeout bounds how long we wait for the terminal's reply.
# Local escape-sequence round-trips are effectively instant once they
# reach the terminal (the only latency is the existing ssh/mosh link,
# not a new network hop), so a short timeout is enough to distinguish
# "the terminal doesn't support/allow OSC 52 read" from "still
# thinking" without making every unsupported-terminal paste hang.
osc52_timeout=2

list_types=0
type_arg=""
no_newline=0

while [ $# -gt 0 ]; do
  case "$1" in
    -l|--list-types) list_types=1; shift ;;
    -t|--type)       type_arg="${2:-}"; shift 2 ;;
    --type=*)        type_arg="${1#--type=}"; shift ;;
    -n|--no-newline) no_newline=1; shift ;;
    -p|--primary)    shift ;;  # ignored — OSC 52 "c" target is the system clipboard
    -w|--watch)
      echo "canopy wl-paste wrapper: --watch is not supported over OSC 52" >&2
      exit 2 ;;
    -c|--clear)
      echo "canopy wl-paste wrapper: --clear is not supported (use wl-copy to overwrite)" >&2
      exit 2 ;;
    *) shift ;;
  esac
done

# osc52_wrap SEQUENCE — applies the same tmux DCS passthrough wrapping
# wl-copy.sh uses, so the query actually reaches the outer terminal
# instead of being consumed by tmux itself.
osc52_wrap() {
  local seq="$1"
  if [ -n "${TMUX:-}" ]; then
    printf '%s' $'\033Ptmux;'"$(printf '%s' "$seq" | sed $'s/\033/\033\033/g')"$'\033\\'
  else
    printf '%s' "$seq"
  fi
}

# osc52_query — sends the OSC 52 "get clipboard" request and reads the
# reply from the tty in raw mode (so the tty driver doesn't line-
# buffer or echo the response), one character at a time until a
# terminator (BEL, or ST = ESC \\) shows up or osc52_timeout elapses
# with no further input. Prints the decoded payload on success;
# returns non-zero if the terminal never replied at all (distinct from
# replying with an empty clipboard, which still produces a valid
# zero-length payload).
osc52_query() {
  local old_stty buf ch got_reply=0
  old_stty=$(stty -g < /dev/tty)
  stty raw -echo < /dev/tty
  # trap ensures the tty is never left in raw mode if we exit early
  # (error, signal) — a wedged raw tty would break the user's shell.
  trap 'stty "$old_stty" < /dev/tty 2>/dev/null || true' RETURN

  osc52_wrap $'\033]52;c;?\033\\' > /dev/tty

  buf=""
  while IFS= read -rsn 1 -t "$osc52_timeout" ch < /dev/tty; do
    got_reply=1
    buf+="$ch"
    case "$ch" in
      $'\a') break ;;  # BEL terminator
    esac
    case "$buf" in
      *$'\033\\') break ;;  # ST terminator (ESC \)
    esac
  done

  if [ "$got_reply" -eq 0 ]; then
    return 1
  fi

  # Strip everything up to and including the OSC 52 header, then the
  # terminator, leaving just the base64 payload.
  local payload="${buf#*]52;c;}"
  payload="${payload%$'\033\\'}"
  payload="${payload%$'\a'}"
  printf '%s' "$payload" | base64 -d 2>/dev/null || true
}

if [ "$list_types" -eq 1 ]; then
  # Claim text/plain only after a real OSC 52 round-trip confirms the
  # local terminal actually answers — never unconditionally. This
  # mirrors the fix applied to the tunnel-based wrapper after it was
  # found lying about bridge health in production (see
  # internal/clipboard/probe.go's ProbeBridgeStatus and this repo's
  # commit 5e27f1b): claiming success without verifying connectivity
  # made a dead bridge report "bridged" forever.
  if osc52_query >/dev/null; then
    echo "text/plain;charset=utf-8"
  fi
  # image/png is intentionally never listed — see the file header
  # comment on why images aren't bridged over OSC 52.
  exit 0
fi

case "$type_arg" in
  image/*)
    echo "canopy wl-paste wrapper: image paste is not supported over OSC 52 (payload too large for terminal escape sequences)" >&2
    exit 2
    ;;
  text/*|"")
    if ! result=$(osc52_query); then
      echo "canopy wl-paste wrapper: local terminal did not respond to the OSC 52 clipboard query within ${osc52_timeout}s (OSC 52 read may be disabled — see docs/clipboard-bridge.md)" >&2
      exit 1
    fi
    printf '%s' "$result"
    [ "$no_newline" -eq 1 ] || echo
    ;;
  *)
    echo "canopy wl-paste wrapper: unsupported type ${type_arg}" >&2
    exit 2 ;;
esac
