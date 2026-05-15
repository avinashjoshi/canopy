package clipboard

import (
	"bytes"
	"fmt"
	"text/template"
)

// TunnelUnitName returns the systemd user-unit name for the
// persistent-SSH-tunnel service for a given canopy host. One unit
// per registered bridged host; units don't share state.
//
// Service-file name (the .service suffix is appended by callers as
// needed): canopy-clipboard-tunnel-<hostName>.service
func TunnelUnitName(hostName string) string {
	return "canopy-clipboard-tunnel-" + hostName
}

// TunnelUnitData drives the systemd unit-file template. SSHPath is
// the absolute path to the ssh binary on the laptop (resolved at
// install time via exec.LookPath) — systemd user services don't
// inherit a useful PATH, so baking the absolute path here is the
// reliable form.
type TunnelUnitData struct {
	HostName  string
	SSHTarget string
	RemoteUID int
	SSHPath   string
	Version   string
}

// tunnelUnitTemplate is the systemd user-unit body. Key design
// choices documented in the unit's own comments since users may
// `cat` the file directly when debugging.
//
// Loadbearing pieces:
//   - ExecStartPre with `-` prefix → systemd ignores failure
//     (covers the "nothing to clean" case on first start).
//   - Pre-clean rm -f works around the observed bug where
//     StreamLocalBindUnlink yes isn't honored by sshd; sshd
//     refuses bind() on top of a stale socket file even when our
//     snippet asks it to unlink first.
//   - ssh -N (no command) + ExitOnForwardFailure so a bind failure
//     surfaces as a non-zero exit, prompting systemd's Restart.
//   - After=canopy-clipboard.service so the local daemon comes up
//     first; otherwise the tunnel's RemoteForward sockets would
//     point at a dead listener on the laptop side.
//   - WantedBy=default.target so the unit starts at login.
const tunnelUnitTemplate = `[Unit]
Description=Canopy clipboard bridge SSH tunnel to {{.HostName}}
Documentation=https://github.com/avinashjoshi/canopy/blob/main/docs/design/v0.18-clipboard-bridge.md
After=network-online.target canopy-clipboard.service
Wants=network-online.target canopy-clipboard.service
PartOf=canopy-clipboard.service
StartLimitBurst=10
StartLimitIntervalSec=300

[Service]
# Pre-clean stale RemoteForward sockets on the remote. StreamLocalBindUnlink
# yes in the SSH snippet is supposed to do this, but isn't reliably honored
# (the observed failure mode is sshd refusing bind() with "Address already
# in use" on top of files from a prior session). The leading dash on
# ExecStartPre tells systemd to ignore failure of the pre-step itself
# (e.g., when the dir is already clean, or when the first ssh fails
# because of bind-failed warnings — the rm still runs and succeeds).
ExecStartPre=-{{.SSHPath}} -o BatchMode=yes -o ServerAliveInterval=30 {{.SSHTarget}} rm -f /run/user/{{.RemoteUID}}/canopy/clip-text.sock /run/user/{{.RemoteUID}}/canopy/clip-image.sock /run/user/{{.RemoteUID}}/canopy/clip-copy.sock

# Persistent SSH tunnel. -N means "no remote command" — the connection's
# whole purpose is to hold the RemoteForward sockets alive. The alias
# canopy-tunnel-{{.HostName}} is defined in the matching
# ~/.ssh/config.d/canopy/{{.HostName}}.conf snippet.
ExecStart={{.SSHPath}} -N canopy-tunnel-{{.HostName}}

Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

// TunnelUnitContent renders the unit body. Validates every field —
// a typo'd ssh path or zero-UID would produce a unit that systemd
// quietly accepts but that fails in confusing ways at runtime.
func TunnelUnitContent(d TunnelUnitData) (string, error) {
	if d.HostName == "" {
		return "", fmt.Errorf("TunnelUnitContent: empty HostName")
	}
	if d.SSHTarget == "" {
		return "", fmt.Errorf("TunnelUnitContent: empty SSHTarget")
	}
	if d.RemoteUID <= 0 {
		return "", fmt.Errorf("TunnelUnitContent: non-positive RemoteUID (%d) would point pre-clean rm at /run/user/0/", d.RemoteUID)
	}
	if d.SSHPath == "" {
		return "", fmt.Errorf("TunnelUnitContent: empty SSHPath — must be absolute (systemd user services don't inherit a useful PATH)")
	}
	tmpl, err := template.New("tunnel-unit").Parse(tunnelUnitTemplate)
	if err != nil {
		return "", fmt.Errorf("TunnelUnitContent parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("TunnelUnitContent execute: %w", err)
	}
	return buf.String(), nil
}
