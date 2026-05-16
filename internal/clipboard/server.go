package clipboard

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Default file names for the three sockets. Kept as constants because
// the wrapper scripts on the remote (and the SSH config snippet
// templates) hard-code them — change in one place and you must change
// in three.
const (
	SocketText  = "clip-text.sock"
	SocketImage = "clip-image.sock"
	SocketCopy  = "clip-copy.sock"
)

// Server runs the clipboard daemon: three Unix-socket listeners, one
// per direction (text-read, image-read, write). Each connection invokes
// the Provider once, then closes — request/response, no long-lived
// streams.
//
// Three-socket design (Premise 3 in docs/design/v0.18-clipboard-bridge.md):
// separating the read-paths lets the remote wrapper pick the right
// socket from the wl-paste invocation's --type flag, instead of
// negotiating MIME type over a single socket. Cheap and clear.
type Server struct {
	provider Provider
	sockDir  string

	// handlerTimeout bounds how long a single connection can keep the
	// Provider busy. Defaults to 5s in NewServer; tests override via
	// SetHandlerTimeout for faster failure assertions.
	handlerTimeout time.Duration
}

// NewServer constructs a Server. sockDir is the directory under which
// the three sockets live — the daemon's caller picks this (typically
// $XDG_RUNTIME_DIR/canopy). Creation of sockDir is deferred to
// ListenAndServe so a failed daemon start surfaces the mkdir error
// alongside the listen failure rather than at construction time.
func NewServer(provider Provider, sockDir string) *Server {
	return &Server{
		provider:       provider,
		sockDir:        sockDir,
		handlerTimeout: 5 * time.Second,
	}
}

// SetHandlerTimeout overrides the per-connection Provider timeout.
// Tests use this to assert fast-fail behavior without waiting the
// production 5 seconds.
func (s *Server) SetHandlerTimeout(d time.Duration) {
	s.handlerTimeout = d
}

// ListenAndServe blocks until ctx is canceled, serving all three
// sockets in parallel. The first listen/chmod failure aborts the
// daemon with a wrapped error; partial-up states are torn down before
// the function returns so a failed start leaves no dangling sockets.
//
// On clean shutdown (ctx canceled), all listeners are closed, accept
// loops exit, and ListenAndServe returns nil. Per-connection handler
// errors are logged at WARN but never propagated — one client glitching
// must not kill the daemon.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := os.MkdirAll(s.sockDir, 0o700); err != nil {
		return fmt.Errorf("Server.ListenAndServe: mkdir %s: %w", s.sockDir, err)
	}

	type spec struct {
		name    string
		path    string
		handler func(net.Conn)
	}
	specs := []spec{
		{"text", filepath.Join(s.sockDir, SocketText), s.handleText},
		{"image", filepath.Join(s.sockDir, SocketImage), s.handleImage},
		{"copy", filepath.Join(s.sockDir, SocketCopy), s.handleCopy},
	}

	var listeners []net.Listener
	cleanup := func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}

	for _, sp := range specs {
		// Best-effort remove of a stale socket from a prior daemon run.
		// Real sockets aren't recreated by Open — Listen fails with
		// "address already in use" if one exists — so this is required
		// for crash-restart idempotency.
		_ = os.Remove(sp.path)

		ln, err := net.Listen("unix", sp.path)
		if err != nil {
			cleanup()
			return fmt.Errorf("Server.ListenAndServe: listen %s: %w", sp.path, err)
		}
		// Mode 0600: only the running user can connect. Premise 5 in the
		// design doc says the bridge is per-user; this enforces it.
		if err := os.Chmod(sp.path, 0o600); err != nil {
			cleanup()
			return fmt.Errorf("Server.ListenAndServe: chmod %s: %w", sp.path, err)
		}
		listeners = append(listeners, ln)
		log.Info("clipboard.server.listen", "socket", sp.name, "path", sp.path)
	}

	// Close listeners when ctx is canceled — this unblocks the accept
	// loops below with a net.ErrClosed, which they translate to a clean
	// exit. Without this, accept would block forever and Wait() would
	// hang the shutdown path.
	go func() {
		<-ctx.Done()
		cleanup()
	}()

	var wg sync.WaitGroup
	for i, sp := range specs {
		wg.Add(1)
		go func(name string, ln net.Listener, handler func(net.Conn)) {
			defer wg.Done()
			s.acceptLoop(ctx, ln, name, handler)
		}(sp.name, listeners[i], sp.handler)
	}

	wg.Wait()
	log.Info("clipboard.server.shutdown")
	return nil
}

// acceptLoop accepts connections until the listener is closed. Each
// accepted connection is handled on its own goroutine so a slow client
// can't block subsequent reads. After ctx is canceled the listener is
// closed by the cleanup goroutine; Accept returns net.ErrClosed and
// we exit.
func (s *Server) acceptLoop(ctx context.Context, ln net.Listener, name string, handler func(net.Conn)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown path — listener was closed by cleanup goroutine.
				return
			}
			log.Warn("clipboard.server.accept", "socket", name, "err", err)
			return
		}
		go handler(conn)
	}
}

// handleText: client connects → we read clipboard text via Provider →
// write bytes to socket → close. Per-connection ctx with handlerTimeout
// so a wedged wl-paste can't hang the daemon's accept loop.
func (s *Server) handleText(conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), s.handlerTimeout)
	defer cancel()
	txt, err := s.provider.ReadText(ctx)
	if err != nil {
		log.Warn("clipboard.server.text.read", "err", err)
		return
	}
	if _, err := conn.Write([]byte(txt)); err != nil {
		log.Warn("clipboard.server.text.write", "err", err)
	}
}

// handleImage: same shape as handleText, but reads image/png bytes.
// The remote wrapper's `wl-paste --type image/png` connects to this
// socket; whatever bytes Provider returns flow back over SSH verbatim.
func (s *Server) handleImage(conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), s.handlerTimeout)
	defer cancel()
	img, err := s.provider.ReadImage(ctx, "image/png")
	if err != nil {
		log.Warn("clipboard.server.image.read", "err", err)
		return
	}
	if _, err := conn.Write(img); err != nil {
		log.Warn("clipboard.server.image.write", "err", err)
	}
}

// handleCopy: read stdin from the client until EOF, hand it to
// Provider.Write. Used when the remote wants to push something into
// the laptop's clipboard (the `wl-copy` wrapper).
func (s *Server) handleCopy(conn net.Conn) {
	defer conn.Close()
	// io.ReadAll on the connection blocks until the client closes its
	// write end. SSH-forwarded sockets propagate close correctly, so
	// this is the canonical pattern.
	data, err := io.ReadAll(conn)
	if err != nil {
		log.Warn("clipboard.server.copy.read", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.handlerTimeout)
	defer cancel()
	if err := s.provider.Write(ctx, data); err != nil {
		log.Warn("clipboard.server.copy.write", "err", err)
	}
}
