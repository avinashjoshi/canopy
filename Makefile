# Canopy — Makefile for the day-to-day dev loop.
#
# The most-used target is `make install`: builds canopy and drops the binary
# into ~/.local/bin so `canopy` works from any directory. Run it after every
# `git pull` or local change to dogfood the latest.

PREFIX ?= $(HOME)/.local
BIN_DIR := $(PREFIX)/bin
BIN := $(BIN_DIR)/canopy

.DEFAULT_GOAL := build

# Build a local ./canopy in the repo root. Useful for one-off testing without
# touching ~/.local/bin.
.PHONY: build
build:
	@go build -o canopy ./cmd/canopy
	@echo "Built: ./canopy ($$(./canopy version))"

# Build + install to $(BIN_DIR). The PATH check warns once if ~/.local/bin
# isn't on PATH so newly-installed canopy commands actually resolve.
.PHONY: install
install: build
	@mkdir -p $(BIN_DIR)
	@install -m 0755 canopy $(BIN)
	@echo "Installed: $(BIN) ($$($(BIN) version))"
	@case ":$$PATH:" in *":$(BIN_DIR):"*) ;; *) \
		echo ""; \
		echo "WARNING: $(BIN_DIR) is not on your PATH."; \
		echo "Add this to your shell profile (~/.bashrc or ~/.zshrc):"; \
		echo "  export PATH=\"$(BIN_DIR):\$$PATH\""; \
		;; esac

# Remove the installed binary. Local ./canopy survives.
.PHONY: uninstall
uninstall:
	@rm -f $(BIN)
	@echo "Removed: $(BIN)"

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
