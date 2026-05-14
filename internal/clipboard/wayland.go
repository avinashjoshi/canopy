package clipboard

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// waylandProvider drives wl-clipboard (wl-paste and wl-copy).
//
// The run field is the seam tests use: replacing it with a fake lets
// us exercise the Provider's logic — argument shape, output parsing,
// stdin piping — without needing wl-clipboard installed in CI.
type waylandProvider struct {
	run cmdRunner
}

// cmdRunner is the swap point for tests. Default impl shells out via
// exec.CommandContext; tests substitute fakeRunner.
//
// stdin is optional: nil means the command doesn't read stdin (wl-paste
// invocations); non-nil bytes are written to the command's stdin before
// it starts (wl-copy invocations).
type cmdRunner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

// defaultRunner is the production cmdRunner. Kept package-private so
// every caller goes through Provider methods, not the runner directly.
func defaultRunner(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// wl-paste exits non-zero when the clipboard is empty for the
		// requested type — surface stderr so the caller can distinguish
		// "nothing to read" from "binary missing".
		return out, fmt.Errorf("%s %v: %w (stderr: %s)", name, args, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// newWaylandProvider constructs the Wayland Provider. Fails fast if
// either wl-paste or wl-copy is missing — that's installation-level
// breakage and the daemon shouldn't start in a half-functional state.
func newWaylandProvider() (*waylandProvider, error) {
	for _, bin := range []string{"wl-paste", "wl-copy"} {
		if _, err := exec.LookPath(bin); err != nil {
			return nil, fmt.Errorf("wayland provider: %s not on PATH (install wl-clipboard): %w", bin, err)
		}
	}
	return &waylandProvider{run: defaultRunner}, nil
}

func (w *waylandProvider) Name() string { return "wayland" }

func (w *waylandProvider) ReadText(ctx context.Context) (string, error) {
	out, err := w.run(ctx, nil, "wl-paste", "--no-newline")
	if err != nil {
		return "", fmt.Errorf("wayland.ReadText: %w", err)
	}
	return string(out), nil
}

func (w *waylandProvider) ReadImage(ctx context.Context, mime string) ([]byte, error) {
	out, err := w.run(ctx, nil, "wl-paste", "--type", mime)
	if err != nil {
		return nil, fmt.Errorf("wayland.ReadImage(%q): %w", mime, err)
	}
	return out, nil
}

func (w *waylandProvider) ListTypes(ctx context.Context) ([]string, error) {
	out, err := w.run(ctx, nil, "wl-paste", "--list-types")
	if err != nil {
		return nil, fmt.Errorf("wayland.ListTypes: %w", err)
	}
	types := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			types = append(types, line)
		}
	}
	return types, nil
}

func (w *waylandProvider) Write(ctx context.Context, data []byte) error {
	if _, err := w.run(ctx, data, "wl-copy"); err != nil {
		return fmt.Errorf("wayland.Write: %w", err)
	}
	return nil
}
