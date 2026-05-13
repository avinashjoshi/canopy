#!/usr/bin/env bash
#
# canopy installer — clones the source to ~/.canopy/src, builds via
# make install, and reports the next step. Designed to be invoked as:
#
#   curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh
#
# Source-based distribution (matches gstack/gbrain). Audience is devs
# comfortable with shells; we expect git + tmux 3.2+ + Go 1.22+ to be
# present — but if they're not, we now offer to install them via the
# system package manager (with a y/N prompt, or auto-yes under --yes).
#
# Flags (must come after `sh -s --` when piped):
#   --yes, -y    Non-interactive. Auto-confirm dep installs and skip
#                any other prompts. Required for SSH'd remote install
#                so the script doesn't hang on a y/N waiting for input.
#   --reinstall  Wipe ~/.canopy/src and re-clone fresh. Default behavior
#                short-circuits when the clone already exists; --reinstall
#                forces a clean install. Use for recovery after a broken
#                clone, or to roll back to whatever main is right now.
#
# Idempotent: re-running without --reinstall on a machine that already
# has ~/.canopy/src prints "looks like canopy is already installed, run
# canopy upgrade instead" and exits 0.
#
# Errors abort the script (set -e) and print the failing command, so
# `curl ... | sh` doesn't silently leave a half-installed state.

set -euo pipefail

REPO_URL="https://github.com/avinashjoshi/canopy.git"
SRC_DIR="$HOME/.canopy/src"
PREFIX="${PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"
MIN_TMUX="3.2"
MIN_GO_MAJOR=1
MIN_GO_MINOR=22

# Populated by parse_args. ASSUME_YES drives both prompt-skipping AND
# whether we pass -y to apt/dnf/etc when running dep installs.
ASSUME_YES=0
REINSTALL=0

# ─── Helpers ─────────────────────────────────────────────────────────

die() {
  echo "" >&2
  echo "canopy install: $*" >&2
  exit 1
}

info() { echo "$*"; }

have() { command -v "$1" >/dev/null 2>&1; }

# parse_args is intentionally tiny — two flags, both boolean. No
# positionals. Unknown flags fail fast so a typo doesn't silently
# become a noop.
parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      -y|--yes)        ASSUME_YES=1 ;;
      --reinstall)     REINSTALL=1 ;;
      -h|--help)
        cat <<EOF
canopy installer — clones to ~/.canopy/src and runs make install.

Usage:
  curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh
  curl -fsSL ... | sh -s -- --yes              # non-interactive
  curl -fsSL ... | sh -s -- --reinstall --yes  # wipe + reinstall fresh

Flags:
  -y, --yes       Non-interactive. Auto-confirm dep installs.
  --reinstall     Wipe ~/.canopy/src and re-clone fresh.
  -h, --help      Show this help.
EOF
        exit 0
        ;;
      *)
        die "unknown flag: $1 (run with --help for usage)"
        ;;
    esac
    shift
  done
}

# detect_pkg_install picks the right install command for the user's
# distro. Returns the bare command string (e.g., "sudo apt-get install git").
# Used both for printing copy-pasteable hints AND for actually running
# the install when the user (or --yes) opts in.
#
# macOS brew runs without sudo; everything else does. We honor that
# distinction so the prompt accurately reflects what's about to happen.
detect_pkg_install() {
  local pkg="$1"
  case "$(uname -s)" in
    Darwin)
      echo "brew install $pkg"
      ;;
    Linux)
      if   have apt-get; then echo "sudo apt-get install -y $pkg"
      elif have dnf;     then echo "sudo dnf install -y $pkg"
      elif have pacman;  then echo "sudo pacman -S --noconfirm $pkg"
      elif have apk;     then echo "sudo apk add $pkg"
      else                    echo ""  # signals "no auto-install available"
      fi
      ;;
    *)
      echo ""  # signals "no auto-install available"
      ;;
  esac
}

# version_ge compares two dotted version strings. version_ge "3.2" "3.2"
# returns 0 (true), version_ge "3.1" "3.2" returns 1 (false). Pure shell;
# avoids a dependency on Python or Perl just for one comparison.
version_ge() {
  printf '%s\n%s\n' "$2" "$1" | sort -V -C
}

# prompt_yes_no asks the user to confirm an action. Returns 0 on yes,
# 1 on no. Under --yes / -y, returns 0 immediately (no prompt). When
# stdin isn't a tty (canopy ran install.sh over SSH without --yes),
# we treat it as "no" with a clear error so the user understands why
# the auto-install was skipped — the alternative (default to yes
# under a pipe) would silently run sudo without consent.
prompt_yes_no() {
  local question="$1"
  if [ "$ASSUME_YES" -eq 1 ]; then
    info "$question [auto-yes via --yes]"
    return 0
  fi
  if [ ! -t 0 ]; then
    info "$question [SKIPPED: stdin is not a terminal; re-run with --yes to auto-confirm]"
    return 1
  fi
  local reply
  printf "%s [y/N] " "$question"
  read -r reply
  case "$reply" in
    y|Y|yes|YES) return 0 ;;
    *)           return 1 ;;
  esac
}

# offer_install_pkg is the interactive variant of detect_pkg_install.
# Prints the proposed command, asks for confirmation, runs it if
# approved. Returns 0 if the package is now installed, 1 if the
# user declined or we can't auto-install on this platform.
#
# The shellcheck-cmd argument is the binary we expect to land in PATH
# after the install (e.g., "git", "tmux") — used to verify the install
# actually worked. Some package names don't match the binary name
# (e.g., golang-go installs `go`), hence the separate arg.
offer_install_pkg() {
  local pkg="$1"
  local check_cmd="$2"
  local install_cmd
  install_cmd="$(detect_pkg_install "$pkg")"
  if [ -z "$install_cmd" ]; then
    info "  (no automatic installer for $pkg on this platform — install manually)"
    return 1
  fi
  info ""
  info "Would install $pkg via:"
  info "  $install_cmd"
  if ! prompt_yes_no "Run this now?"; then
    info "  Skipped. Install $pkg manually and re-run install.sh."
    return 1
  fi
  info "Running: $install_cmd"
  # shellcheck disable=SC2086
  if ! sh -c "$install_cmd"; then
    info "  Install failed. Inspect the output above."
    return 1
  fi
  if ! have "$check_cmd"; then
    info "  $check_cmd is still not on PATH after install — investigate."
    return 1
  fi
  info "  ✓ $check_cmd installed."
  return 0
}

# ─── Prereq checks ───────────────────────────────────────────────────

check_git() {
  if have git; then
    return 0
  fi
  info ""
  info "git is required but not installed. canopy uses git to clone and"
  info "manage worktrees."
  if ! offer_install_pkg git git; then
    die "git is required. Install it and re-run install.sh."
  fi
}

check_tmux() {
  if ! have tmux; then
    info ""
    info "tmux is required but not installed."
    info "canopy uses tmux's display-popup (added in 3.2)."
    if ! offer_install_pkg tmux tmux; then
      die "tmux is required. Install it and re-run install.sh."
    fi
  fi
  local tmux_version
  tmux_version=$(tmux -V 2>/dev/null | awk '{print $2}' | sed 's/[a-z]*$//')
  if [ -z "$tmux_version" ]; then
    info "WARNING: could not detect tmux version; assuming it's recent enough."
    return 0
  fi
  if ! version_ge "$tmux_version" "$MIN_TMUX"; then
    # We don't try to auto-upgrade tmux — distro repos often have
    # old versions, and the right fix is distro-specific (PPA, brew
    # upgrade, build from source). Print the hint and bail.
    local hint
    hint="$(detect_pkg_install tmux)"
    die "tmux $MIN_TMUX+ is required (found $tmux_version).
  canopy's popup integration uses tmux display-popup, added in 3.2.
  Upgrade tmux (distro may not have a recent enough version):
    ${hint:-(install tmux for your platform)}"
  fi
}

check_go() {
  if ! have go; then
    info ""
    info "Go is required but not installed. canopy is built from source"
    info "(no pre-built binaries)."
    # On Linux, the Go package name varies by distro: golang-go on
    # Debian/Ubuntu, go on Arch, golang on Fedora/Alpine. detect_pkg_install
    # is fed the right name for the right distro inline below.
    local pkg="golang-go"
    if [ "$(uname -s)" = "Linux" ]; then
      if have pacman; then pkg="go"
      elif have dnf || have apk; then pkg="golang"
      fi
    elif [ "$(uname -s)" = "Darwin" ]; then
      pkg="go"
    fi
    if ! offer_install_pkg "$pkg" go; then
      die "Go is required. Install Go ${MIN_GO_MAJOR}.${MIN_GO_MINOR}+ and re-run.
  Or download from https://go.dev/dl/"
    fi
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
  Distro Go is often outdated. Either upgrade via your package manager,
  or download a recent Go from https://go.dev/dl/"
  fi
}

check_make() {
  if have make; then
    return 0
  fi
  info ""
  info "make is required but not installed."
  info "canopy's install path uses 'make install' from the source clone."
  # Debian/Ubuntu: build-essential pulls make + gcc together. Other
  # distros: bare make is fine.
  local pkg="make"
  if have apt-get; then pkg="build-essential"
  fi
  if ! offer_install_pkg "$pkg" make; then
    die "make is required. Install build essentials and re-run install.sh."
  fi
}

# ─── Clone + build ───────────────────────────────────────────────────

# wipe_src_for_reinstall removes ~/.canopy/src when --reinstall is set.
# Only touches the src clone — state.json, workspaces, hosts.json, and
# everything else under ~/.canopy/ are preserved. This is the
# "fresh-clone reinstall" semantics chosen for v0.17.x.
wipe_src_for_reinstall() {
  if [ "$REINSTALL" -ne 1 ]; then
    return 0
  fi
  if [ ! -e "$SRC_DIR" ]; then
    return 0
  fi
  info "--reinstall: removing existing source clone at $SRC_DIR"
  rm -rf "$SRC_DIR"
}

clone_or_skip() {
  if [ -e "$SRC_DIR" ]; then
    if [ -d "$SRC_DIR/.git" ]; then
      info ""
      info "canopy source already at $SRC_DIR."
      info "Looks like canopy is already installed. To upgrade, run:"
      info "  canopy upgrade"
      info ""
      info "Or to wipe and re-clone fresh, re-run install.sh with --reinstall:"
      info "  curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh -s -- --reinstall"
      exit 0
    fi
    die "$SRC_DIR exists but is not a git clone.
  Remove it and re-run install.sh:
    rm -rf $SRC_DIR
    curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh"
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
  parse_args "$@"

  info "canopy install"
  info "  source:    $REPO_URL"
  info "  clone to:  $SRC_DIR"
  info "  install:   $BIN_DIR/canopy.bin (symlinked as $BIN_DIR/canopy)"
  if [ "$REINSTALL" -eq 1 ]; then
    info "  mode:      --reinstall (will wipe existing $SRC_DIR)"
  fi
  if [ "$ASSUME_YES" -eq 1 ]; then
    info "  mode:      --yes (auto-confirm dep installs)"
  fi
  info ""
  info "Checking prerequisites..."
  check_git
  check_tmux
  check_go
  check_make
  info "  ok: git, tmux, Go, make"
  info ""

  wipe_src_for_reinstall
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
