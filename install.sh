#!/bin/sh
# canopy installer: downloads the release binary for this machine, verifies it, installs it.
#
#   curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh
#
# Options (environment variables):
#   CANOPY_VERSION=v1.2.3   install a specific release (default: latest)
#   CANOPY_BIN_DIR=/path    where to put the binary (default: ~/.local/bin)
#   CANOPY_REPO=owner/name  GitHub repository to download from
#   CANOPY_DOWNLOAD_BASE=URL  take assets from URL/<asset> instead of GitHub (for testing)
#
# Re-running the script upgrades in place. `canopy upgrade` does the same from inside canopy.
set -eu

REPO="${CANOPY_REPO:-avinashjoshi/canopy}"
BIN_DIR="${CANOPY_BIN_DIR:-$HOME/.local/bin}"
VERSION="${CANOPY_VERSION:-latest}"

say()  { printf '%s\n' "$*" >&2; }
fail() { say "canopy install: $*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have curl || fail "curl is required"
have tar  || fail "tar is required"

# ---- which build -----------------------------------------------------------------------
os=$(uname -s)
arch=$(uname -m)
case "$os/$arch" in
  Linux/x86_64)                 target=x86_64-unknown-linux-musl ;;
  Linux/aarch64|Linux/arm64)    target=aarch64-unknown-linux-musl ;;
  Darwin/x86_64)                target=x86_64-apple-darwin ;;
  Darwin/arm64)                 target=aarch64-apple-darwin ;;
  *) fail "no prebuilt binary for $os/$arch. Build from source: cargo install --git https://github.com/$REPO canopy" ;;
esac

# ---- which version ---------------------------------------------------------------------
if [ -n "${CANOPY_DOWNLOAD_BASE:-}" ]; then
  [ "$VERSION" != "latest" ] || fail "CANOPY_DOWNLOAD_BASE needs CANOPY_VERSION too"
  tag=$VERSION
  case "$tag" in v*) ;; *) tag="v$tag" ;; esac
elif [ "$VERSION" = "latest" ]; then
  # The /releases/latest redirect carries the tag of the newest stable release; no API call,
  # no rate limit. It skips pre-releases, so fall back to the API when only those exist.
  location=$(curl -fsSI "https://github.com/$REPO/releases/latest" | tr -d '\r' | awk 'tolower($1)=="location:" {print $2}' | tail -n1)
  tag=${location##*/tag/}
  if [ -z "$tag" ] || [ "$tag" = "$location" ]; then
    tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=1" | grep -o '"tag_name": *"[^"]*"' | head -n1 | cut -d'"' -f4)
  fi
  [ -n "$tag" ] || fail "could not find a release of $REPO (none published yet?)"
else
  tag=$VERSION
  case "$tag" in v*) ;; *) tag="v$tag" ;; esac
fi

asset="canopy-$tag-$target.tar.gz"
base="${CANOPY_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download/$tag}"

# ---- download + verify -----------------------------------------------------------------
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t canopy)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading canopy $tag ($target)…"
curl -fsSL --retry 3 -o "$tmp/$asset" "$base/$asset" || fail "download failed: $base/$asset"
curl -fsSL --retry 3 -o "$tmp/$asset.sha256" "$base/$asset.sha256" || fail "checksum download failed"

expected=$(awk '{print $1}' "$tmp/$asset.sha256")
if have sha256sum; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
elif have shasum; then
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
else
  say "warning: no sha256sum/shasum on this machine; skipping checksum verification"
  actual=$expected
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $asset (expected $expected, got $actual)"

tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/canopy" ] || fail "archive did not contain a canopy binary"

# ---- install ---------------------------------------------------------------------------
mkdir -p "$BIN_DIR"
chmod 755 "$tmp/canopy"
# Rename over the old binary: atomic, and running canopy processes keep their old inode.
mv -f "$tmp/canopy" "$BIN_DIR/canopy.new"
mv -f "$BIN_DIR/canopy.new" "$BIN_DIR/canopy"
say "installed $("$BIN_DIR/canopy" --version) to $BIN_DIR/canopy"

# An older server keeps running until told otherwise; stop it so the new one takes over
# on the next command. (The new binary also does this itself when it notices the mismatch.)
if have pgrep && pgrep -f 'canopy server$' >/dev/null 2>&1; then
  "$BIN_DIR/canopy" server stop >/dev/null 2>&1 || true
  say "stopped the old canopy server; the new one starts on your next command (sessions are untouched)"
fi

# ---- requirements ----------------------------------------------------------------------
missing=""
have tmux || missing="$missing tmux"
have git  || missing="$missing git"
if [ -n "$missing" ]; then
  say ""
  say "canopy needs:$missing"
  if have pacman; then say "  sudo pacman -S$missing"
  elif have apt-get; then say "  sudo apt-get install -y$missing"
  elif have dnf; then say "  sudo dnf install -y$missing"
  elif have brew; then say "  brew install$missing"
  fi
fi
have gh || say "optional: install gh (GitHub CLI) for the PR and issue pickers"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say ""; say "add $BIN_DIR to your PATH, e.g. in ~/.bashrc or ~/.zshrc:"; say "  export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac

say ""
say "next:  canopy doctor            # check tmux, git, agents, server"
say "       cd <your repo> && canopy init --with-scripts && canopy new"
