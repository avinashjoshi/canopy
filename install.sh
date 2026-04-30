#!/usr/bin/env bash
#
# canopy installer — clones the source to ~/.canopy/src, builds via
# make install, and reports the next step. Designed to be invoked as:
#
#   curl -fsSL https://raw.githubusercontent.com/oncactus/canopy/main/install.sh | sh
#
# Source-based distribution (matches gstack/gbrain). Audience is devs
# comfortable with shells; we expect git + tmux 3.2+ + Go 1.22+ to be
# present, and we tell users exactly what to run if they're not.
#
# Idempotent: re-running on a machine that already has ~/.canopy/src
# prints "looks like canopy is already installed, run canopy upgrade
# instead" and exits 0.
#
# Errors abort the script (set -e) and print the failing command, so
# `curl ... | sh` doesn't silently leave a half-installed state.

set -euo pipefail

REPO_URL="https://github.com/oncactus/canopy.git"
SRC_DIR="$HOME/.canopy/src"
PREFIX="${PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"
MIN_TMUX="3.2"
MIN_GO_MAJOR=1
MIN_GO_MINOR=22

# ─── Helpers ─────────────────────────────────────────────────────────

die() {
  echo "" >&2
  echo "canopy install: $*" >&2
  exit 1
}

info() { echo "$*"; }

have() { command -v "$1" >/dev/null 2>&1; }

# detect_pkg_install picks the right install command for the user's
# distro. We don't run it for them — we just print the line so they
# can copy/paste with full understanding of what's about to land in
# /usr/bin.
detect_pkg_install() {
  local pkg="$1"
  case "$(uname -s)" in
    Darwin)
      echo "  brew install $pkg"
      ;;
    Linux)
      if   have apt-get; then echo "  sudo apt-get install $pkg"
      elif have dnf;     then echo "  sudo dnf install $pkg"
      elif have pacman;  then echo "  sudo pacman -S $pkg"
      elif have apk;     then echo "  sudo apk add $pkg"
      else                    echo "  (install $pkg via your distro's package manager)"
      fi
      ;;
    *)
      echo "  (install $pkg for your platform)"
      ;;
  esac
}

# version_ge compares two dotted version strings. version_ge "3.2" "3.2"
# returns 0 (true), version_ge "3.1" "3.2" returns 1 (false). Pure shell;
# avoids a dependency on Python or Perl just for one comparison.
version_ge() {
  printf '%s\n%s\n' "$2" "$1" | sort -V -C
}

# ─── Prereq checks ───────────────────────────────────────────────────

check_git() {
  if ! have git; then
    die "git is required but not installed.
  Install it first:
$(detect_pkg_install git)"
  fi
}

check_tmux() {
  if ! have tmux; then
    die "tmux is required but not installed.
  canopy uses tmux's display-popup (added in 3.2).
  Install it first:
$(detect_pkg_install tmux)"
  fi
  local tmux_version
  tmux_version=$(tmux -V 2>/dev/null | awk '{print $2}' | sed 's/[a-z]*$//')
  if [ -z "$tmux_version" ]; then
    info "WARNING: could not detect tmux version; assuming it's recent enough."
    return 0
  fi
  if ! version_ge "$tmux_version" "$MIN_TMUX"; then
    die "tmux $MIN_TMUX+ is required (found $tmux_version).
  canopy's popup integration uses tmux display-popup, added in 3.2.
  Upgrade tmux:
$(detect_pkg_install tmux)"
  fi
}

check_go() {
  if ! have go; then
    die "Go is required but not installed.
  canopy is built from source (no pre-built binaries).
  Install Go ${MIN_GO_MAJOR}.${MIN_GO_MINOR}+:
$(detect_pkg_install golang-go)
  Or download from https://go.dev/dl/"
  fi
  # Go reports "go version go1.22.3 linux/amd64" — we want "1.22.3".
  local go_version
  go_version=$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')
  if [ -z "$go_version" ]; then
    info "WARNING: could not detect Go version; assuming it's recent enough."
    return 0
  fi
  local min_version="${MIN_GO_MAJOR}.${MIN_GO_MINOR}"
  if ! version_ge "$go_version" "$min_version"; then
    die "Go $min_version+ is required (found $go_version).
  Upgrade Go:
$(detect_pkg_install golang-go)
  Or download from https://go.dev/dl/"
  fi
}

check_make() {
  if ! have make; then
    die "make is required but not installed.
  canopy's install path uses 'make install' from the source clone.
  Install build essentials:
$(detect_pkg_install make)"
  fi
}

# ─── Clone + build ───────────────────────────────────────────────────

clone_or_skip() {
  if [ -e "$SRC_DIR" ]; then
    if [ -d "$SRC_DIR/.git" ]; then
      info ""
      info "canopy source already at $SRC_DIR."
      info "Looks like canopy is already installed. To upgrade, run:"
      info "  canopy upgrade"
      info ""
      info "(Or if the existing clone is broken, remove it and re-run install.sh:"
      info "  rm -rf $SRC_DIR && curl -fsSL https://raw.githubusercontent.com/oncactus/canopy/main/install.sh | sh)"
      exit 0
    fi
    die "$SRC_DIR exists but is not a git clone.
  Remove it and re-run install.sh:
    rm -rf $SRC_DIR
    curl -fsSL https://raw.githubusercontent.com/oncactus/canopy/main/install.sh | sh"
  fi
  info "Cloning canopy to $SRC_DIR..."
  mkdir -p "$(dirname "$SRC_DIR")"
  git clone --quiet "$REPO_URL" "$SRC_DIR"
}

build_and_install() {
  info "Building and installing canopy..."
  if ! make -C "$SRC_DIR" install >/dev/null; then
    die "make install failed.
  Inspect the output above and re-run from the source clone:
    cd $SRC_DIR && make install"
  fi
}

print_path_hint() {
  case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *)
      info ""
      info "WARNING: $BIN_DIR is not on your PATH."
      info "Add this to your shell profile (~/.bashrc or ~/.zshrc):"
      info "  export PATH=\"$BIN_DIR:\$PATH\""
      info ""
      info "Then reload your shell or run: export PATH=\"$BIN_DIR:\$PATH\""
      ;;
  esac
}

# ─── Run ─────────────────────────────────────────────────────────────

main() {
  info "canopy install"
  info "  source:    $REPO_URL"
  info "  clone to:  $SRC_DIR"
  info "  install:   $BIN_DIR/canopy.bin (symlinked as $BIN_DIR/canopy)"
  info ""
  info "Checking prerequisites..."
  check_git
  check_tmux
  check_go
  check_make
  info "  ok: git, tmux, Go, make"
  info ""

  clone_or_skip
  build_and_install
  print_path_hint

  info ""
  info "Installed. Try it:"
  info "  canopy version"
  info "  canopy install tmux    # wire popup keybind into ~/.tmux.conf"
  info "  canopy upgrade         # update later"
}

main "$@"
