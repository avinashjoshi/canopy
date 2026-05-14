// Package clipboard implements canopy's clipboard bridge — the daemon
// that exposes the laptop's local clipboard over SSH-forwarded Unix
// sockets so remote canopy workspaces can read and write it as if it
// were their own.
//
// See docs/design/v0.18-clipboard-bridge.md for the full design.
//
// Phase 1 ships a single Wayland provider. The Provider interface exists
// so X11 (xclip) and macOS (pbpaste/pbcopy) providers can land in
// Phase 2 as one-file additions without touching Server or the daemon
// wiring.
package clipboard

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("clipboard")

// ErrNoProvider is returned by Detect when the current environment has
// no supported clipboard tool. Callers use errors.Is to distinguish
// "the user is on a server with no graphical session" from "the
// provider's tool blew up at runtime".
var ErrNoProvider = errors.New("no clipboard provider detected on this system")

// Provider is the operations every clipboard backend must support.
// Methods take a context so the daemon can cancel a slow shell-out
// when a client disconnects mid-read.
//
// Empty results vs errors: a clipboard with nothing in it is not an
// error — ReadText returns ("", nil), ReadImage returns (nil, nil),
// ListTypes returns ([]string{}, nil). Errors mean the underlying
// tool failed (binary missing, permission denied, killed).
type Provider interface {
	// Name returns a short identifier ("wayland", "x11", "darwin") for
	// log lines and the TUI status display.
	Name() string

	// ReadText returns the current clipboard contents as a string. The
	// daemon serves this on clip-text.sock.
	ReadText(ctx context.Context) (string, error)

	// ReadImage returns the current clipboard contents in the requested
	// MIME type (typically "image/png"). The daemon serves this on
	// clip-image.sock. Empty []byte means no image is on the clipboard;
	// not an error.
	ReadImage(ctx context.Context, mime string) ([]byte, error)

	// ListTypes returns the MIME types currently offered by the
	// clipboard. The wrapper script's `wl-paste --list-types` pass-
	// through relies on this so Claude Code can detect when an image
	// is available before asking for bytes.
	ListTypes(ctx context.Context) ([]string, error)

	// Write replaces the clipboard contents with the given bytes. The
	// daemon invokes this when a remote sends data on clip-copy.sock.
	Write(ctx context.Context, data []byte) error
}

// Detect picks a Provider based on the current environment.
//
// Phase 1: Wayland only — heuristic is the presence of $WAYLAND_DISPLAY,
// which is the canonical signal every Wayland compositor sets and every
// Wayland client looks for.
//
// Future phases land X11 (heuristic: $DISPLAY without $WAYLAND_DISPLAY)
// and macOS (heuristic: runtime.GOOS == "darwin") here as additional
// branches. Order matters: Wayland-on-Linux often has both $WAYLAND_DISPLAY
// and $DISPLAY (XWayland) set, and we prefer the native protocol.
func Detect() (Provider, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		p, err := newWaylandProvider()
		if err != nil {
			return nil, fmt.Errorf("clipboard.Detect: %w", err)
		}
		log.Debug("clipboard.detect.wayland")
		return p, nil
	}
	return nil, fmt.Errorf("clipboard.Detect: %w", ErrNoProvider)
}
