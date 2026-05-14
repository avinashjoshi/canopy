#!/usr/bin/env bash
# Canopy clipboard-bridge wrapper for wl-copy.
# Managed by canopy {{.Version}}. Do not edit; reinstall via
#   canopy host clipboard <name> --reinstall
#
# Forwards wl-copy data through the SSH-forwarded clip-copy.sock so
# the bytes land in the laptop's clipboard via the local daemon's
# Provider.Write call.
#
# Supports the two invocation forms wl-copy itself supports:
#   wl-copy "some text"   (argv)
#   wl-copy < file        (stdin)
#
# Flags we silently accept then ignore — they're meaningful to real
# wl-copy but don't survive the SSH boundary cleanly:
#   -n / --trim-newline      we never appended one in the first place
#   -o / --paste-once        no daemon state to make "single-use"
#   -p / --primary           bridge serves one clipboard bucket
#   -c / --clear             use a separate `clear` action if needed
#   -f / --foreground        we don't fork
#   -t / --type              clipboard payload is opaque bytes here
#
# Flags that error out fast: --watch (not applicable to wl-copy
# anyway; included for parity with wl-paste wrapper).

set -euo pipefail

CLIP_DIR="${XDG_RUNTIME_DIR:-/tmp}/canopy"

while [ $# -gt 0 ]; do
  case "$1" in
    -n|--trim-newline|-o|--paste-once|-p|--primary|-c|--clear|-f|--foreground)
      shift ;;
    -t|--type) shift 2 ;;
    --type=*)  shift ;;
    -h|--help)
      echo "canopy wl-copy wrapper (managed by canopy)" >&2
      exit 0 ;;
    --) shift; break ;;
    *) break ;;
  esac
done

# pipe_to_socket — write stdin to the copy socket with a 2s timeout.
# Bash's pipefail makes a timeout-induced socat exit a non-zero overall
# exit, which is what Claude Code (or whatever invoked wl-copy) needs
# to see when the daemon is down.
pipe_to_socket() {
  timeout 2 socat - "UNIX-CONNECT:$CLIP_DIR/clip-copy.sock"
}

if [ $# -gt 0 ]; then
  # Argument form: emit the args verbatim (joined by spaces) to the
  # socket. No trailing newline — matches wl-copy's `--trim-newline`
  # default; the user can add one in their content if they want.
  printf '%s' "$*" | pipe_to_socket
else
  # Stdin form: stream bytes through. Works for any payload including
  # binary (image data) because we never decode along the way.
  pipe_to_socket
fi
