package clipboard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeProvider is the testable stand-in for a real clipboard tool. It
// records every call (so handler dispatch is verifiable) and returns
// whatever bytes the test set up. Thread-safe — the daemon serves
// connections on multiple goroutines and concurrent fakeProvider calls
// are routine.
type fakeProvider struct {
	mu        sync.Mutex
	textOut   string
	imageOut  []byte
	textErr   error
	imageErr  error
	writeErr  error
	writeData []byte // last payload passed to Write
	calls     []string
}

func (f *fakeProvider) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) ReadText(_ context.Context) (string, error) {
	f.record("ReadText")
	return f.textOut, f.textErr
}

func (f *fakeProvider) ReadImage(_ context.Context, _ string) ([]byte, error) {
	f.record("ReadImage")
	return f.imageOut, f.imageErr
}

func (f *fakeProvider) ListTypes(_ context.Context) ([]string, error) {
	f.record("ListTypes")
	return nil, nil
}

func (f *fakeProvider) Write(_ context.Context, data []byte) error {
	f.record("Write")
	f.mu.Lock()
	f.writeData = append([]byte(nil), data...)
	f.mu.Unlock()
	return f.writeErr
}

// startServer brings up a Server on a temp sockDir, returns the path
// and a cancel func the test calls to shut it down. Waits for all
// three sockets to be listening before returning so subsequent dials
// don't race the accept loop.
func startServer(t *testing.T, p Provider) (sockDir string, cancel func()) {
	t.Helper()
	sockDir = t.TempDir()
	srv := NewServer(p, sockDir)
	srv.SetHandlerTimeout(500 * time.Millisecond)

	ctx, cancelCtx := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.ListenAndServe(ctx)
	}()

	// Wait until all three socket FILES exist — bounded so a busted
	// listen fails the test promptly. Statting the file (rather than
	// dialing) avoids spawning handler invocations during warm-up;
	// dialing the copy socket would trigger Provider.Write with empty
	// data and race subsequent test writes.
	deadline := time.Now().Add(2 * time.Second)
	paths := []string{
		filepath.Join(sockDir, SocketText),
		filepath.Join(sockDir, SocketImage),
		filepath.Join(sockDir, SocketCopy),
	}
	for time.Now().Before(deadline) {
		allExist := true
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				allExist = false
				break
			}
		}
		if allExist {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel = func() {
		cancelCtx()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server didn't shut down within 2s")
		}
	}
	t.Cleanup(cancel)
	return sockDir, cancel
}

func TestServer_TextRoundTrip(t *testing.T) {
	p := &fakeProvider{textOut: "hello from the laptop"}
	sockDir, _ := startServer(t, p)

	conn, err := net.Dial("unix", filepath.Join(sockDir, SocketText))
	if err != nil {
		t.Fatalf("dial text socket: %v", err)
	}
	defer conn.Close()

	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read text socket: %v", err)
	}
	if string(data) != "hello from the laptop" {
		t.Errorf("got %q, want %q", data, "hello from the laptop")
	}
}

func TestServer_ImageRoundTrip(t *testing.T) {
	want := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x99, 0x88}
	p := &fakeProvider{imageOut: want}
	sockDir, _ := startServer(t, p)

	conn, err := net.Dial("unix", filepath.Join(sockDir, SocketImage))
	if err != nil {
		t.Fatalf("dial image socket: %v", err)
	}
	defer conn.Close()

	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read image socket: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("image bytes mangled: got %x, want %x", data, want)
	}
}

func TestServer_CopyRoundTrip(t *testing.T) {
	p := &fakeProvider{}
	sockDir, _ := startServer(t, p)

	conn, err := net.Dial("unix", filepath.Join(sockDir, SocketCopy))
	if err != nil {
		t.Fatalf("dial copy socket: %v", err)
	}
	payload := []byte("paste me into the laptop clipboard")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write copy socket: %v", err)
	}
	conn.Close() // signal EOF so handler completes

	// Provider.Write happens on the daemon's goroutine — poll briefly.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := append([]byte(nil), p.writeData...)
		p.mu.Unlock()
		if bytes.Equal(got, payload) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("Provider.Write payload didn't arrive: got %q, want %q", p.writeData, payload)
}

func TestServer_ProviderErrorClosesConnectionCleanly(t *testing.T) {
	// A Provider that errors must not crash the daemon or hang the
	// client — the client should just see an empty / EOF response and
	// move on.
	p := &fakeProvider{textErr: errors.New("wl-paste blew up")}
	sockDir, _ := startServer(t, p)

	conn, err := net.Dial("unix", filepath.Join(sockDir, SocketText))
	if err != nil {
		t.Fatalf("dial text socket: %v", err)
	}
	defer conn.Close()

	// Bounded read — if the daemon hangs instead of closing, this fails fast.
	conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	data, err := io.ReadAll(conn)
	if err != nil {
		// A read deadline error means the daemon didn't close → bug.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("daemon hung on Provider error; should have closed conn")
		}
	}
	if len(data) != 0 {
		t.Errorf("expected empty response on Provider error, got %q", data)
	}
}

func TestServer_StaleSocketCleanup(t *testing.T) {
	// Drop a stale socket file in place before the daemon starts; it
	// should be cleaned up and the daemon should listen on the same path.
	sockDir := t.TempDir()
	stale := filepath.Join(sockDir, SocketText)
	if err := writeFile(stale, "junk"); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}

	srv := NewServer(&fakeProvider{textOut: "ok"}, sockDir)
	srv.SetHandlerTimeout(500 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	// Wait for socket to become a real listener.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", stale)
		if err == nil {
			data, _ := io.ReadAll(conn)
			conn.Close()
			if string(data) == "ok" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon didn't replace stale socket within 2s")
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
