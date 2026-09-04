package clipboard

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// SanitizeArtifactName derives a filesystem- and systemd-unit-safe key
// from a host identifier. HostInstaller.InstallOnHost uses it (via its
// hostName parameter) to key the legacy-artifact cleanup lookups in
// cleanupLegacyArtifacts: the pre-OSC52 canopy-clipboard-tunnel-<name>.service
// unit name and the pre-OSC52 ~/.ssh/config.d/canopy/<name>.conf
// snippet filename — both were named this way when they were written,
// so cleanup must derive the same name to find them again.
//
// A REGISTERED host name is already restricted to this shape —
// internal/host/registry.go's validateName forbids @, :, /, and
// whitespace. A raw --remote/--on target used without `canopy host
// add` (the whole point of wiring clipboard-bridge auto-setup into
// `--remote` thin-client mode — see
// docs/design/v0.18-clipboard-bridge.md) can be shaped like
// `user@host:port`, which is NOT safe to use verbatim: "/" would
// escape the config.d directory, and systemd unit names have their own
// reserved-character rules. Any character outside [A-Za-z0-9._-]
// becomes '-'.
//
// A clean input (no substitution needed) passes through byte-for-byte
// — this matters because it's the common case (registered host names,
// and the bare ~/.ssh/config-alias shape of most raw --remote targets,
// e.g. "tower"), and existing on-disk artifact filenames for
// already-bridged hosts must never shift under them on a canopy
// upgrade. Only when a character actually gets substituted (the messy
// "user@host:port" shape) is an 8-hex-char suffix of sha256(spec)
// appended, because substitution is lossy and otherwise-distinct specs
// can collide — "tower:1" and "tower-1" would both sanitize to
// "tower-1" without it, silently overwriting one host's legacy
// artifacts with the other's during cleanup.
func SanitizeArtifactName(spec string) string {
	var b strings.Builder
	b.Grow(len(spec))
	changed := false
	for _, r := range spec {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
			changed = true
		}
	}
	out := b.String()
	if out == "" {
		return "host"
	}
	if !changed {
		return out
	}
	sum := sha256.Sum256([]byte(spec))
	return out + "-" + hex.EncodeToString(sum[:])[:8]
}
