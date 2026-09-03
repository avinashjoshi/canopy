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

	t.Run("unregistered name errors clearly", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		_, _, err := resolveRemoteHost("nonexistent-host")
		if err == nil {
			t.Fatal("resolveRemoteHost(unregistered) = nil error; want an error")
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Errorf("error = %q; want it to mention the host isn't registered", err.Error())
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
