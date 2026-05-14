package clipboard

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner records each invocation and returns canned bytes (or an
// error) keyed by the first argument after the binary name — that's the
// flag the Provider methods vary on. Lets each test case state "when
// wl-paste runs with --type image/png, return these bytes" without
// touching real wl-clipboard.
type fakeRunner struct {
	// responses[firstArg] → bytes returned, error returned.
	// firstArg "" matches calls with no args (the bare wl-paste / wl-copy).
	responses map[string]struct {
		out []byte
		err error
	}
	// calls is appended-to on each invocation, in call order. Tests
	// assert against this to verify the right args were passed.
	calls []fakeCall
}

type fakeCall struct {
	stdin []byte
	name  string
	args  []string
}

func (f *fakeRunner) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, fakeCall{stdin: append([]byte(nil), stdin...), name: name, args: append([]string(nil), args...)})
	key := ""
	if len(args) > 0 {
		key = args[0]
	}
	r, ok := f.responses[key]
	if !ok {
		return nil, errors.New("fakeRunner: no canned response for key " + key)
	}
	return r.out, r.err
}

func newWaylandWithFake(f *fakeRunner) *waylandProvider {
	return &waylandProvider{run: f.run}
}

func TestWayland_Name(t *testing.T) {
	p := newWaylandWithFake(&fakeRunner{})
	if got, want := p.Name(), "wayland"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

func TestWayland_ReadText_HappyPath(t *testing.T) {
	f := &fakeRunner{responses: map[string]struct {
		out []byte
		err error
	}{
		"--no-newline": {out: []byte("hello world"), err: nil},
	}}
	p := newWaylandWithFake(f)
	got, err := p.ReadText(context.Background())
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if got != "hello world" {
		t.Errorf("ReadText() = %q, want %q", got, "hello world")
	}
	if len(f.calls) != 1 || f.calls[0].name != "wl-paste" {
		t.Errorf("expected one wl-paste call, got %+v", f.calls)
	}
	if len(f.calls[0].args) != 1 || f.calls[0].args[0] != "--no-newline" {
		t.Errorf("expected wl-paste --no-newline, got args=%v", f.calls[0].args)
	}
}

func TestWayland_ReadText_PropagatesError(t *testing.T) {
	f := &fakeRunner{responses: map[string]struct {
		out []byte
		err error
	}{
		"--no-newline": {err: errors.New("wl-paste exit 1")},
	}}
	p := newWaylandWithFake(f)
	_, err := p.ReadText(context.Background())
	if err == nil {
		t.Fatal("ReadText should propagate underlying error")
	}
	if !strings.Contains(err.Error(), "wayland.ReadText") {
		t.Errorf("error should be wrapped with wayland.ReadText prefix, got %q", err.Error())
	}
}

func TestWayland_ReadImage_ReturnsBytesVerbatim(t *testing.T) {
	pngHeader := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00}
	f := &fakeRunner{responses: map[string]struct {
		out []byte
		err error
	}{
		"--type": {out: pngHeader, err: nil},
	}}
	p := newWaylandWithFake(f)
	got, err := p.ReadImage(context.Background(), "image/png")
	if err != nil {
		t.Fatalf("ReadImage: %v", err)
	}
	if !bytes.Equal(got, pngHeader) {
		t.Errorf("ReadImage returned mangled bytes: got %x, want %x", got, pngHeader)
	}
	if f.calls[0].args[0] != "--type" || f.calls[0].args[1] != "image/png" {
		t.Errorf("expected wl-paste --type image/png, got args=%v", f.calls[0].args)
	}
}

func TestWayland_ListTypes_ParsesNewlineDelimited(t *testing.T) {
	f := &fakeRunner{responses: map[string]struct {
		out []byte
		err error
	}{
		"--list-types": {out: []byte("text/plain;charset=utf-8\nimage/png\n"), err: nil},
	}}
	p := newWaylandWithFake(f)
	types, err := p.ListTypes(context.Background())
	if err != nil {
		t.Fatalf("ListTypes: %v", err)
	}
	want := []string{"text/plain;charset=utf-8", "image/png"}
	if len(types) != len(want) {
		t.Fatalf("ListTypes = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("ListTypes[%d] = %q, want %q", i, types[i], want[i])
		}
	}
}

func TestWayland_ListTypes_EmptyClipboard(t *testing.T) {
	// wl-paste --list-types on an empty clipboard returns just whitespace
	// (or empty output depending on version). The Provider must return
	// an empty slice, never nil, so the daemon's JSON serialization is
	// consistent.
	f := &fakeRunner{responses: map[string]struct {
		out []byte
		err error
	}{
		"--list-types": {out: []byte("   \n"), err: nil},
	}}
	p := newWaylandWithFake(f)
	types, err := p.ListTypes(context.Background())
	if err != nil {
		t.Fatalf("ListTypes: %v", err)
	}
	if types == nil {
		t.Error("ListTypes returned nil slice; want empty non-nil")
	}
	if len(types) != 0 {
		t.Errorf("ListTypes = %v, want empty", types)
	}
}

func TestWayland_Write_PipesStdin(t *testing.T) {
	f := &fakeRunner{responses: map[string]struct {
		out []byte
		err error
	}{
		"": {out: nil, err: nil},
	}}
	p := newWaylandWithFake(f)
	payload := []byte("paste me into the clipboard")
	if err := p.Write(context.Background(), payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0].name != "wl-copy" {
		t.Errorf("expected one wl-copy call, got %+v", f.calls)
	}
	if !bytes.Equal(f.calls[0].stdin, payload) {
		t.Errorf("Write didn't pipe stdin: got %q, want %q", f.calls[0].stdin, payload)
	}
}

func TestWayland_Write_PropagatesError(t *testing.T) {
	f := &fakeRunner{responses: map[string]struct {
		out []byte
		err error
	}{
		"": {err: errors.New("wl-copy died")},
	}}
	p := newWaylandWithFake(f)
	err := p.Write(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("Write should propagate underlying error")
	}
	if !strings.Contains(err.Error(), "wayland.Write") {
		t.Errorf("error should be wrapped with wayland.Write prefix, got %q", err.Error())
	}
}
