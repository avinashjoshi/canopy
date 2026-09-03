package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/avinashjoshi/canopy/internal/host"
)

// TestResolveRemoteHost covers resolveRemoteHost's branches: empty spec,
// a raw SSH target (both `user@host` and `host:port` shapes), a
// registered host name, and an unregistered name. Mirrors how
// resolveOnForNew/resolveOnForSwitch split raw-target-vs-registry, but
// resolveRemoteHost also reports selfHeal (whether the resolved host
// has a registry entry to attach auto-discovered project registrations
// to — see buildRemoteRowsMsg's selfHeal parameter in internal/ui).
func TestResolveRemoteHost(t *testing.T) {
	t.Run("empty spec errors", func(t *testing.T) {
		_, _, err := resolveRemoteHost("")
		if err == nil {
			t.Fatal("resolveRemoteHost(\"\") = nil error; want an error")
		}
	})

	t.Run("raw target with @ bypasses the registry", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home) // no hosts.json at all — must not be consulted

		h, selfHeal, err := resolveRemoteHost("user@tower.tail-abc.ts.net")
		if err != nil {
			t.Fatalf("resolveRemoteHost: %v", err)
		}
		if h.SSHTarget != "user@tower.tail-abc.ts.net" {
			t.Errorf("SSHTarget = %q; want the raw spec", h.SSHTarget)
		}
		if h.Name != "user@tower.tail-abc.ts.net" {
			t.Errorf("Name = %q; want the raw spec (no registry name to use instead)", h.Name)
		}
		if selfHeal {
			t.Error("selfHeal = true; want false for a raw target (no registry entry)")
		}
	})

	t.Run("dash-prefixed raw target is rejected", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		// Contains "@" so it takes the raw-target branch, but the
		// leading "-" makes it option-injection-shaped -- must be
		// rejected before it ever reaches ssh/mosh's argv.
		_, _, err := resolveRemoteHost("-oProxyCommand=touch /tmp/x@evil")
		if err == nil {
			t.Fatal("resolveRemoteHost(dash-prefixed target) = nil error; want an error")
		}
		if !errors.Is(err, host.ErrSSHTargetInvalid) {
			t.Errorf("error = %v; want it to wrap host.ErrSSHTargetInvalid", err)
		}
	})

	t.Run("raw target with : bypasses the registry", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		h, selfHeal, err := resolveRemoteHost("tower.example.com:2222")
		if err != nil {
			t.Fatalf("resolveRemoteHost: %v", err)
		}
		if h.SSHTarget != "tower.example.com:2222" {
			t.Errorf("SSHTarget = %q; want the raw spec", h.SSHTarget)
		}
		if selfHeal {
			t.Error("selfHeal = true; want false for a raw target")
		}
	})

	t.Run("registered name resolves via the registry", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if err := seedHostRegistryForTest(t, "tower", "user@tower-real-target"); err != nil {
			t.Fatalf("seed registry: %v", err)
		}

		h, selfHeal, err := resolveRemoteHost("tower")
		if err != nil {
			t.Fatalf("resolveRemoteHost: %v", err)
		}
		if h.Name != "tower" {
			t.Errorf("Name = %q; want %q", h.Name, "tower")
		}
		if h.SSHTarget != "user@tower-real-target" {
			t.Errorf("SSHTarget = %q; want %q", h.SSHTarget, "user@tower-real-target")
		}
		if !selfHeal {
			t.Error("selfHeal = false; want true for a registry-resolved host")
		}
	})

	t.Run("unregistered bare name falls back to a raw target", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home) // no hosts.json at all

		// A bare word with no "@"/":" isn't necessarily a canopy
		// registry name — it's exactly the shape of a ~/.ssh/config
		// Host alias (e.g. `--remote tower` where `ssh tower` already
		// works). Failing "not registered" here would break the "just
		// log in, no host add" promise for the single most common case.
		// Registry lookup still happens first (this spec isn't
		// registered, so it falls through) — only THEN does it become
		// a raw target.
		h, selfHeal, err := resolveRemoteHost("tower")
		if err != nil {
			t.Fatalf("resolveRemoteHost(unregistered bare name): %v; want it to fall back to a raw target, not error", err)
		}
		if h.SSHTarget != "tower" {
			t.Errorf("SSHTarget = %q; want %q", h.SSHTarget, "tower")
		}
		if h.Name != "tower" {
			t.Errorf("Name = %q; want %q", h.Name, "tower")
		}
		if selfHeal {
			t.Error("selfHeal = true; want false — no registry entry exists for this fallback")
		}
	})

	t.Run("registered bare name still wins over the raw-target fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if err := seedHostRegistryForTest(t, "tower", "avi@tower-real.tail.ts.net"); err != nil {
			t.Fatalf("seed registry: %v", err)
		}

		h, selfHeal, err := resolveRemoteHost("tower")
		if err != nil {
			t.Fatalf("resolveRemoteHost: %v", err)
		}
		if h.SSHTarget != "avi@tower-real.tail.ts.net" {
			t.Errorf("SSHTarget = %q; want the registry's SSHTarget, not the bare spec itself", h.SSHTarget)
		}
		if !selfHeal {
			t.Error("selfHeal = false; want true — this spec IS a registered name")
		}
	})

	t.Run("registered non-ssh host type errors", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		reg, err := loadHostRegistry()
		if err != nil {
			t.Fatalf("loadHostRegistry: %v", err)
		}
		// v0.17.0 only supports Type "ssh"; a future canopy.cloud-style
		// Type value must be rejected explicitly rather than silently
		// treated as SSH.
		if err := reg.Add("cloudhost", host.Host{Type: "canopy-cloud"}); err != nil {
			t.Fatalf("seed registry: %v", err)
		}

		_, _, err = resolveRemoteHost("cloudhost")
		if err == nil {
			t.Fatal("resolveRemoteHost(non-ssh host) = nil error; want an error")
		}
		if !strings.Contains(err.Error(), "canopy-cloud") {
			t.Errorf("error = %q; want it to mention the unsupported type", err.Error())
		}
	})
}

// TestResolveOnForNew_UnregisteredBareNameFallsBackToRawTarget is the
// regression test for the same class of bug fixed in resolveRemoteHost,
// applied here to `canopy new --on <spec>`: a bare word that isn't a
// registered name must be usable directly as an SSH target (a
// ~/.ssh/config alias, etc.), not rejected outright.
func TestResolveOnForNew_UnregisteredBareNameFallsBackToRawTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no hosts.json at all

	got, err := resolveOnForNew("tower", "myproject", "/remote/path")
	if err != nil {
		t.Fatalf("resolveOnForNew(unregistered bare name): %v; want it to fall back to a raw target", err)
	}
	if got.SSHTarget != "tower" {
		t.Errorf("SSHTarget = %q; want %q", got.SSHTarget, "tower")
	}
	if got.Source != "raw-target" {
		t.Errorf("Source = %q; want %q", got.Source, "raw-target")
	}
	if got.HostName != "" {
		t.Errorf("HostName = %q; want empty (no registry entry backs this fallback)", got.HostName)
	}
	// explicitRemoteCwd was passed, so it must carry through even on
	// the fallback path (same as the pre-existing raw-target branch).
	if got.RemoteCwd != "/remote/path" {
		t.Errorf("RemoteCwd = %q; want %q", got.RemoteCwd, "/remote/path")
	}
}

// TestResolveOnForNew_RegisteredBareNameStillUsesRegistry proves the
// registry still wins over the fallback when the name IS registered —
// the fallback only fires on ErrHostNotFound, not unconditionally.
func TestResolveOnForNew_RegisteredBareNameStillUsesRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := seedHostRegistryForTest(t, "tower", "avi@tower-real.tail.ts.net"); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	reg, err := loadHostRegistry()
	if err != nil {
		t.Fatalf("loadHostRegistry: %v", err)
	}
	if err := reg.AddProject("tower", "myproject", "/home/avi/myproject"); err != nil {
		t.Fatalf("AddProject: %v", err)
	}

	got, err := resolveOnForNew("tower", "myproject", "")
	if err != nil {
		t.Fatalf("resolveOnForNew: %v", err)
	}
	if got.SSHTarget != "avi@tower-real.tail.ts.net" {
		t.Errorf("SSHTarget = %q; want the registry's SSHTarget", got.SSHTarget)
	}
	if got.HostName != "tower" {
		t.Errorf("HostName = %q; want %q (registry-resolved)", got.HostName, "tower")
	}
}

// TestResolveOnForSwitch_UnregisteredBareNameFallsBackToRawTarget is
// the direct regression test for the bug hand-testing surfaced: `enter`
// on a workspace row in a --remote-pinned session (an unregistered bare
// host) dispatches `canopy switch --on <row.Host>`, which used to error
// "not registered" even though the pinned session was already
// successfully talking to that exact host.
func TestResolveOnForSwitch_UnregisteredBareNameFallsBackToRawTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := resolveOnForSwitch("tower", "", "")
	if err != nil {
		t.Fatalf("resolveOnForSwitch(unregistered bare name): %v; want it to fall back to a raw target", err)
	}
	if got.SSHTarget != "tower" {
		t.Errorf("SSHTarget = %q; want %q", got.SSHTarget, "tower")
	}
	if got.Source != "raw-target" {
		t.Errorf("Source = %q; want %q", got.Source, "raw-target")
	}
}

// TestResolveOnForSwitch_DashPrefixedBareNameRejected proves the
// fallback still runs the option-injection guard — a dash-prefixed
// spec must not silently become a raw target.
func TestResolveOnForSwitch_DashPrefixedBareNameRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := resolveOnForSwitch("-oProxyCommand=touch /tmp/x", "", "")
	if err == nil {
		t.Fatal("resolveOnForSwitch(dash-prefixed spec) = nil error; want an error")
	}
	if !errors.Is(err, host.ErrSSHTargetInvalid) {
		t.Errorf("error = %v; want it to wrap host.ErrSSHTargetInvalid", err)
	}
}

// seedHostRegistryForTest writes a host directly into ~/.canopy/hosts.json
// via the host package rather than shelling out to the CLI, so the test
// doesn't depend on cobra command wiring or the nested-tmux guard.
func seedHostRegistryForTest(t *testing.T, name, sshTarget string) error {
	t.Helper()
	reg, err := loadHostRegistry()
	if err != nil {
		return err
	}
	return reg.Add(name, host.Host{Type: "ssh", SSHTarget: sshTarget})
}
