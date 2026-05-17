# Canopy — Makefile for the day-to-day dev loop.
#
# Two install shapes coexist:
#
#   make install   builds with -ldflags-injected version, writes to
#                  $(BIN_DIR)/canopy.bin, then symlinks $(BIN_DIR)/canopy
#                  -> canopy.bin. Use this on main after every pull/merge
#                  to dogfood the latest released code. NEVER from feature
#                  branches (parallel agents racing the install slot
#                  produces confusing dogfood signal).
#
#   make dev       builds the worktree's local canopy with NO ldflags
#                  (so version stays "dev" and the DEV banner fires) and
#                  points $(BIN_DIR)/canopy at it. Lets you flip between
#                  released and an in-flight feature build with one
#                  command, no rebuild on the way back.
#
#   make release   flips the symlink back to canopy.bin. Doesn't rebuild
#                  — canopy.bin is whatever was last installed from main.
#                  Use after make dev to return to the released binary
#                  from any worktree.
#
# Both dev and release are thin wrappers; cmd/canopy/use.go is the
# source of truth and works from anywhere on PATH. The Makefile targets
# exist for muscle memory and for fresh installs that haven't built
# `canopy use` yet.

PREFIX  ?= $(HOME)/.local
BIN_DIR := $(PREFIX)/bin
BIN     := $(BIN_DIR)/canopy
BIN_REAL := $(BIN_DIR)/canopy.bin

# Version + commit injected via -ldflags so `canopy version` produces a
# real string for `make install`-built binaries (no goreleaser needed).
# VERSION is the human-curated semver in the repo root; COMMIT is the
# short sha of HEAD; DATE is the build timestamp in UTC.
#
# `make build` and `make dev` deliberately omit these ldflags so dev-built
# binaries surface as `version == "dev"` and the TUI/statusline DEV
# banner fires. That's the one signal that distinguishes a worktree
# build from an installed one — keep it pristine.
VERSION := $(shell cat VERSION 2>/dev/null || echo "0.0.0-novers")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "nogit")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=v$(VERSION)+$(COMMIT) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.DEFAULT_GOAL := build

# Build a local ./canopy in the worktree root. NO ldflags — we want
# `version = "dev"` to drive the DEV banner. `make dev` chains here.
.PHONY: build
build:
	@go build -o canopy ./cmd/canopy
	@echo "Built: ./canopy"

# Build with ldflags then install to $(BIN_REAL). The symlink at $(BIN)
# is updated in place so a parallel `make dev` from another worktree
# doesn't get clobbered by an install — the symlink target changes,
# the file at $(BIN_REAL) gets refreshed, both states coexist.
#
# Pre-flight: probe whether $(BIN_DIR) is writable BEFORE go build
# writes a partial artifact. The classic failure mode is a prior
# install that ran as root (sudo) and left $(BIN_REAL) owned by root,
# so the current user can't overwrite it on the next upgrade. Catching
# it here means we surface "fix permissions on ~/.local/bin" instead
# of the muddier "go: open ...canopy.bin: permission denied".
.PHONY: install
install:
	@mkdir -p $(BIN_DIR) || { \
		echo "ERROR: could not create $(BIN_DIR)."; \
		echo "  Check that the parent directory exists and is writable:"; \
		echo "    ls -ld $$(dirname $(BIN_DIR))"; \
		exit 1; \
	}
	@if ! ( touch $(BIN_DIR)/.canopy-install-probe 2>/dev/null && rm -f $(BIN_DIR)/.canopy-install-probe ); then \
		echo "ERROR: $(BIN_DIR) is not writable by $$(whoami)."; \
		echo "  This usually means a previous install ran as root via sudo."; \
		echo "  Recover with:"; \
		echo "    sudo chown -R $$(whoami) $(BIN_DIR)"; \
		echo "  Then re-run 'make install' (or 'canopy upgrade')."; \
		exit 1; \
	fi
	@if [ -e $(BIN_REAL) ] && [ ! -w $(BIN_REAL) ]; then \
		echo "ERROR: $(BIN_REAL) exists but is not writable by $$(whoami)."; \
		echo "  A previous install likely ran as root via sudo."; \
		echo "  Recover with:"; \
		echo "    sudo chown $$(whoami) $(BIN_REAL)"; \
		echo "  Then re-run 'make install' (or 'canopy upgrade')."; \
		exit 1; \
	fi
	@go build -ldflags='$(LDFLAGS)' -o $(BIN_REAL) ./cmd/canopy
	@chmod 0755 $(BIN_REAL)
	@ln -sfn canopy.bin $(BIN)
	@echo "Installed: $(BIN_REAL)"
	@echo "Active:    $(BIN) -> canopy.bin"
	@$(BIN) version 2>/dev/null | head -1 || true
	@case ":$$PATH:" in *":$(BIN_DIR):"*) ;; *) \
		echo ""; \
		echo "WARNING: $(BIN_DIR) is not on your PATH."; \
		echo "Add this to your shell profile (~/.bashrc or ~/.zshrc):"; \
		echo "  export PATH=\"$(BIN_DIR):\$$PATH\""; \
		;; esac

# `make dev` builds this worktree's canopy and points $(BIN) at it.
# One command in any worktree to flip from released to dev. No rebuild
# needed to switch back — see `make release`.
.PHONY: dev
dev: build
	@mkdir -p $(BIN_DIR)
	@ln -sfn $(CURDIR)/canopy $(BIN)
	@echo "Active: $(BIN) -> $(CURDIR)/canopy"
	@echo "Mode:   DEV"
	@$(BIN) version 2>/dev/null | head -1 || true

# `make release` flips $(BIN) back to canopy.bin. Refuses if canopy.bin
# is missing — that means the user has only ever done `make dev` and
# there's no release binary to fall back to.
.PHONY: release
release:
	@if [ ! -f $(BIN_REAL) ]; then \
		echo "No release binary at $(BIN_REAL)."; \
		echo "Run 'make install' on the main branch first to populate it,"; \
		echo "then 'make release' will flip $(BIN) back here from any worktree."; \
		exit 1; \
	fi
	@ln -sfn canopy.bin $(BIN)
	@echo "Active: $(BIN) -> canopy.bin"
	@echo "Mode:   release"
	@$(BIN) version 2>/dev/null | head -1 || true

# Remove the installed binary AND the symlink. Local ./canopy survives.
.PHONY: uninstall
uninstall:
	@rm -f $(BIN) $(BIN_REAL)
	@echo "Removed: $(BIN) and $(BIN_REAL)"

# Standard go test pass. -tags=e2e runs the slow E2E pair (real tmux + scratch
# repo). Defaults to fast unit tests only.
.PHONY: test
test:
	@go test ./...

.PHONY: test-e2e
test-e2e:
	@go test -tags=e2e ./...

# Lint shortcut. Doesn't fail the build if golangci-lint isn't installed yet.
.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || \
		echo "golangci-lint not installed; skipping (install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)"

# Strip the local build artifact. ~/.local/bin install survives.
.PHONY: clean
clean:
	@rm -f canopy
	@echo "Cleaned: ./canopy"
