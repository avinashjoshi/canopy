// Command `canopy clipboard-server` is the long-running daemon half of
// the v0.18 clipboard bridge. It listens on three Unix sockets in
// $XDG_RUNTIME_DIR/canopy/ and proxies clipboard reads/writes to the
// laptop's local clipboard tool (Wayland in Phase 1; X11/macOS in
// Phase 2).
//
// Normal invocation is by the systemd user unit
// (~/.config/systemd/user/canopy-clipboard.service) that
// `canopy install clipboard-bridge` writes. Running it by hand is
// supported and useful for Phase 0 dogfooding:
//
//	canopy clipboard-server          # blocks; SIGINT/SIGTERM shuts it down
//	canopy clipboard-server --debug  # bumps log level
//
// The subcommand lives next to install-tmux and the host verbs because
// it's a leaf piece of canopy's CLI surface; the bulk of the daemon
// logic lives in internal/clipboard.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clipboard"
)

// newClipboardServerCmd returns the `canopy clipboard-server` cobra
// subcommand. Allowed inside an existing tmux session because it never
// spawns or modifies tmux state — it just opens sockets and waits.
func newClipboardServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clipboard-server",
		Short: "Run the clipboard-bridge daemon (long-running; managed by systemd in production).",
		Long: "Listens on three Unix sockets in $XDG_RUNTIME_DIR/canopy/ and proxies\n" +
			"clipboard reads and writes to the local clipboard tool. Used by the\n" +
			"v0.18 clipboard-bridge feature; see docs/design/v0.18-clipboard-bridge.md.\n\n" +
			"Normally run by the systemd user unit that\n" +
			"`canopy install clipboard-bridge` sets up. Running by hand is supported\n" +
			"for Phase 0 dogfooding. Shuts down cleanly on SIGINT or SIGTERM.",
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		// No flags in Phase 0. --debug comes from the persistent root
		// flag so logging-level tweaks work the same way as every other
		// canopy subcommand.
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClipboardServer(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

// runClipboardServer is the body of the clipboard-server subcommand.
// Extracted from the cobra wrapper so the systemd unit (when wired up
// in Phase 1) can call the same code path without rebuilding cobra
// state in tests.
func runClipboardServer(ctx context.Context, out, errOut interface {
	Write(p []byte) (n int, err error)
}) error {
	provider, err := clipboard.Detect()
	if err != nil {
		fmt.Fprintf(errOut, "canopy clipboard-server: %v\n", err)
		fmt.Fprintln(errOut, "  Phase 1 supports Wayland only. Make sure WAYLAND_DISPLAY is set")
		fmt.Fprintln(errOut, "  and wl-clipboard is installed (`pacman -S wl-clipboard` / `apt install wl-clipboard`).")
		return err
	}

	sockDir, err := clipboardSocketDir()
	if err != nil {
		return err
	}

	srv := clipboard.NewServer(provider, sockDir)
	fmt.Fprintf(out, "canopy clipboard-server: provider=%s, sockets=%s\n", provider.Name(), sockDir)

	// SIGINT/SIGTERM trigger ctx cancellation; ListenAndServe returns
	// when the cleanup goroutine inside the Server closes the
	// listeners. systemd uses SIGTERM for graceful stop, so this is
	// the production path.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return srv.ListenAndServe(ctx)
}

// clipboardSocketDir picks the directory the three sockets live in.
// Order:
//
//  1. $XDG_RUNTIME_DIR/canopy/  — canonical on every modern Linux
//     desktop (locked-in call in the v0.18 design doc).
//  2. /tmp/canopy-<uid>/         — last-resort fallback when
//     $XDG_RUNTIME_DIR is unset (containers, minimal cron envs).
//
// Server.ListenAndServe creates the dir; we just compute the path here.
func clipboardSocketDir() (string, error) {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "canopy"), nil
	}
	uid := os.Getuid()
	if uid < 0 {
		// Windows or some other UID-less env; the design doc is Linux-
		// only, but fail loudly rather than silently writing to /tmp/canopy--1.
		return "", fmt.Errorf("clipboardSocketDir: $XDG_RUNTIME_DIR unset and uid is unavailable")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("canopy-%d", uid)), nil
}
