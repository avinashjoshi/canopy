package clipboard

import (
	"strings"
	"testing"
)

func TestTunnelUnitName_PrefixIsStable(t *testing.T) {
	// The wire name is referenced by both EnsureTunnelUnit (writing
	// the file) and verifyBridge (calling systemctl is-active). Drift
	// between those two callers would break verify silently.
	if got, want := TunnelUnitName("tower"), "canopy-clipboard-tunnel-tower"; got != want {
		t.Errorf("TunnelUnitName(tower) = %q, want %q", got, want)
	}
}

func TestTunnelUnitContent_RendersExpectedDirectives(t *testing.T) {
	body, err := TunnelUnitContent(TunnelUnitData{
		HostName:  "tower",
		SSHTarget: "avi@tower.lan",
		RemoteUID: 1000,
		SSHPath:   "/usr/bin/ssh",
		Version:   "v0.18.0+test",
	})
	if err != nil {
		t.Fatalf("TunnelUnitContent: %v", err)
	}
	for _, must := range []string{
		"Description=Canopy clipboard bridge SSH tunnel to tower",
		"After=network-online.target canopy-clipboard.service",
		"ExecStartPre=-/usr/bin/ssh",
		"avi@tower.lan rm -f /run/user/1000/canopy/clip-text.sock",
		"ExecStart=/usr/bin/ssh -N canopy-tunnel-tower",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("unit body missing %q\nbody:\n%s", must, body)
		}
	}
}

func TestTunnelUnitContent_RefusesInvalidFields(t *testing.T) {
	good := TunnelUnitData{
		HostName:  "tower",
		SSHTarget: "avi@tower.lan",
		RemoteUID: 1000,
		SSHPath:   "/usr/bin/ssh",
	}
	cases := []struct {
		name string
		mut  func(d *TunnelUnitData)
	}{
		{"empty HostName", func(d *TunnelUnitData) { d.HostName = "" }},
		{"empty SSHTarget", func(d *TunnelUnitData) { d.SSHTarget = "" }},
		{"zero RemoteUID", func(d *TunnelUnitData) { d.RemoteUID = 0 }},
		{"negative RemoteUID", func(d *TunnelUnitData) { d.RemoteUID = -1 }},
		{"empty SSHPath", func(d *TunnelUnitData) { d.SSHPath = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := good
			c.mut(&d)
			if _, err := TunnelUnitContent(d); err == nil {
				t.Errorf("TunnelUnitContent should reject %s; got nil error", c.name)
			}
		})
	}
}

func TestParseSSHTarget(t *testing.T) {
	cases := []struct {
		target               string
		user, host, port     string
	}{
		{"tower.lan", "", "tower.lan", ""},
		{"avi@tower.lan", "avi", "tower.lan", ""},
		{"avi@tower.lan:22", "avi", "tower.lan", "22"},
		{"tower.lan:2222", "", "tower.lan", "2222"},
		{"cassy@a10i-tower.geep-carat.ts.net", "cassy", "a10i-tower.geep-carat.ts.net", ""},
		// LastIndex defends against weird user@user@host patterns
		{"weird@user@host.example.com", "weird@user", "host.example.com", ""},
		// Non-digit suffix isn't a port — leave attached
		{"host:notaport", "", "host:notaport", ""},
	}
	for _, c := range cases {
		u, h, p := parseSSHTarget(c.target)
		if u != c.user || h != c.host || p != c.port {
			t.Errorf("parseSSHTarget(%q) = (%q, %q, %q); want (%q, %q, %q)",
				c.target, u, h, p, c.user, c.host, c.port)
		}
	}
}
