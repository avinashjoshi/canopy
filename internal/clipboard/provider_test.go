package clipboard

import (
	"errors"
	"os/exec"
	"testing"
)

func TestDetect_WaylandWhenEnvSet(t *testing.T) {
	// Skip when wl-paste isn't installed in the test environment — Detect
	// fails the LookPath check before reaching the env-based dispatch
	// result, and there's nothing meaningful to assert about that failure
	// shape here (it's exercised in TestNewWaylandProvider_MissingBinary
	// instead).
	if _, err := exec.LookPath("wl-paste"); err != nil {
		t.Skip("wl-paste not on PATH; skipping (Phase 1 dev-machine prerequisite)")
	}
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	p, err := Detect()
	if err != nil {
		t.Fatalf("Detect() with WAYLAND_DISPLAY set: %v", err)
	}
	if got, want := p.Name(), "wayland"; got != want {
		t.Errorf("Provider.Name() = %q, want %q", got, want)
	}
}

func TestDetect_NoProviderWhenEnvUnset(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "")
	_, err := Detect()
	if err == nil {
		t.Fatal("Detect() with no WAYLAND_DISPLAY should return ErrNoProvider")
	}
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("Detect() error = %v, want errors.Is(...) == ErrNoProvider", err)
	}
}
