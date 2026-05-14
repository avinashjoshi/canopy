#!/usr/bin/env bash
# Canopy clipboard-bridge wrapper for wl-paste.
# Managed by canopy {{.Version}}. Do not edit; reinstall via
#   canopy host clipboard <name> --reinstall
#
# Routes wl-paste invocations to the SSH-forwarded Unix sockets that
# canopy's daemon on the laptop listens on. The wrapper handles the
# subset of wl-paste flags Claude Code uses for clipboard reads:
#   wl-paste                       text, with trailing newline
#   wl-paste --no-newline          text, no trailing newline
#   wl-paste --type image/png      image bytes
#   wl-paste --type text/plain     text
#   wl-paste --list-types          newline-delimited mime types
#
# Each socat call is wrapped with `timeout 2` so a daemon-down state
# fails fast (Claude Code sees the non-zero exit and treats as "no
# clipboard") instead of hanging the user's Ctrl+V.

set -euo pipefail

CLIP_DIR="${XDG_RUNTIME_DIR:-/tmp}/canopy"

list_types=0
type_arg=""
no_newline=0

while [ $# -gt 0 ]; do
  case "$1" in
    -l|--list-types) list_types=1; shift ;;
    -t|--type)       type_arg="${2:-}"; shift 2 ;;
    --type=*)        type_arg="${1#--type=}"; shift ;;
    -n|--no-newline) no_newline=1; shift ;;
    -p|--primary)    shift ;;  # ignored — bridge serves the main clipboard only
    -w|--watch)
      echo "canopy wl-paste wrapper: --watch is not supported over the bridge" >&2
      exit 2 ;;
    -c|--clear)
      echo "canopy wl-paste wrapper: --clear is not supported (use wl-copy to overwrite)" >&2
      exit 2 ;;
    *) shift ;;
  esac
done

# pipe_socket SOCKET_PATH — opens the unix socket, writes stdout
# verbatim, exits non-zero if the daemon is unreachable within 2s.
pipe_socket() {
  timeout 2 socat - "UNIX-CONNECT:$1" </dev/null
}

if [ "$list_types" -eq 1 ]; then
  # Always offer text/plain — matches every realistic state where the
  # local clipboard has SOMETHING. Real wl-paste would omit text/plain
  # when the clipboard is truly image-only; for Claude Code's use case
  # the answer it actually keys on is whether image/png is in the list.
  echo "text/plain;charset=utf-8"

  # Probe clip-image.sock: read the first eight bytes and check the PNG
  # signature. head -c 8 closes our end early so we don't pay for the
  # full image transfer just to determine presence.
  head_bytes=$(pipe_socket "$CLIP_DIR/clip-image.sock" 2>/dev/null \
    | head -c 8 \
    | od -An -tx1 \
    | tr -d ' \n' || true)
  if [ "${head_bytes:-}" = "89504e470d0a1a0a" ]; then
    echo "image/png"
  fi
  exit 0
fi

case "$type_arg" in
  image/*)
    pipe_socket "$CLIP_DIR/clip-image.sock"
    ;;
  text/*|"")
    pipe_socket "$CLIP_DIR/clip-text.sock"
    # Real wl-paste emits a trailing newline by default; --no-newline
    # suppresses it.
    [ "$no_newline" -eq 1 ] || echo
    ;;
  *)
    echo "canopy wl-paste wrapper: unsupported type ${type_arg}" >&2
    exit 2 ;;
esac
