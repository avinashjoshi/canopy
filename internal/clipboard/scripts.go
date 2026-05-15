package clipboard

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"text/template"
)

// scriptsFS embeds the wrapper scripts and SSH snippet template into
// the canopy binary. Pushing them to remote hosts is a stdin-pipe over
// SSH (cat > ~/.local/bin/wl-paste etc.) — same payload-delivery
// pattern internal/host.InstallScript already uses for the canopy
// installer.
//
//go:embed scripts/wl-paste.sh scripts/wl-copy.sh scripts/snippet.tmpl
var scriptsFS embed.FS

// WrapperScript names the wrapper file as the remote-side ~/.local/bin/
// name (without the .sh extension; we strip it before copying so
// Claude Code's exec.LookPath("wl-paste") resolves to the wrapper, not
// the real wl-clipboard binary).
type WrapperScript string

const (
	WrapperWlPaste WrapperScript = "wl-paste"
	WrapperWlCopy  WrapperScript = "wl-copy"
)

// sourceFile maps a WrapperScript to its embedded template file.
func (w WrapperScript) sourceFile() string {
	return "scripts/" + string(w) + ".sh"
}

// RemoteName is the path basename used when writing the wrapper into
// the remote's ~/.local/bin/. Strips the embedded .sh extension so
// exec.LookPath finds the wrapper instead of the real wl-clipboard
// binary.
func (w WrapperScript) RemoteName() string {
	return string(w)
}

// WrapperContent renders a wrapper script with the canopy version
// stamped into its header comment, then returns the rendered string
// and a sha256-12 hash for fast-skip comparison against the on-remote
// copy.
//
// The hash is computed AFTER template execution so two different
// canopy versions produce different hashes (the version stamp in the
// header changes). That gives reinstall a cheap "is the on-remote
// wrapper the same canopy version's wrapper?" check without parsing
// the header comment.
func WrapperContent(w WrapperScript, canopyVersion string) (content string, hash string, err error) {
	raw, err := scriptsFS.ReadFile(w.sourceFile())
	if err != nil {
		return "", "", fmt.Errorf("WrapperContent(%q): %w", w, err)
	}
	tmpl, err := template.New(string(w)).Parse(string(raw))
	if err != nil {
		return "", "", fmt.Errorf("WrapperContent(%q) parse: %w", w, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"Version": canopyVersion}); err != nil {
		return "", "", fmt.Errorf("WrapperContent(%q) execute: %w", w, err)
	}
	rendered := buf.String()
	sum := sha256.Sum256([]byte(rendered))
	return rendered, hex.EncodeToString(sum[:])[:12], nil
}

// SnippetData is what snippet.tmpl renders against. Kept as a named
// type (not map[string]any) so a missing field becomes a compile-time
// error instead of an empty-string template hole at runtime.
//
// HostName is canopy's internal name for the host (e.g., "tower"),
// used in the snippet's comment header AND the dedicated alias
// `Host canopy-tunnel-<HostName>` that the persistent SSH tunnel
// matches. Only one process (the systemd tunnel unit) ever ssh's
// to that alias, so the RemoteForward directives don't conflict
// with everyday ssh/mosh connections to the same machine.
//
// SSHHostname/SSHUser/SSHPort are the resolved pieces of the SSH
// target string (e.g., "tower.lan", "avi", "" from "avi@tower.lan").
// They land inside the `Host canopy-tunnel-...` block as
// HostName / User / Port directives so `ssh canopy-tunnel-tower`
// resolves to the real target without the tunnel command line
// needing to spell it out.
type SnippetData struct {
	HostName    string
	SSHHostname string
	SSHUser     string // optional — omitted if SSH target has no user@ prefix
	SSHPort     string // optional — omitted if SSH target has no :port suffix
	Version     string
	LocalUID    int
	RemoteUID   int
}

// SnippetContent renders the per-host SSH config snippet. The output
// is written to ~/.ssh/config.d/canopy/<HostName>.conf — picked up by
// the marker-block Include directive in ~/.ssh/config that
// `canopy install clipboard-bridge` wrote.
//
// Errors:
//   - empty HostName or SSHHostname: snippet would generate a `Host `
//     (whitespace) stanza which SSH treats as `Host *`. Catastrophic.
//   - non-positive UIDs: socket paths /run/user/<uid>/ would resolve
//     to /run/user/0/ which is root's runtime dir (won't exist for an
//     unprivileged daemon). Fail loudly.
func SnippetContent(d SnippetData) (string, error) {
	if d.HostName == "" {
		return "", fmt.Errorf("SnippetContent: empty HostName would break the snippet's comment + reinstall hint; refused")
	}
	if d.SSHHostname == "" {
		return "", fmt.Errorf("SnippetContent: empty SSHHostname would generate a `Host ` (== `Host *`) stanza, broadcasting RemoteForwards to every SSH target; refused")
	}
	if d.LocalUID <= 0 || d.RemoteUID <= 0 {
		return "", fmt.Errorf("SnippetContent: non-positive UID (local=%d, remote=%d) would point sockets at /run/user/0/", d.LocalUID, d.RemoteUID)
	}
	raw, err := scriptsFS.ReadFile("scripts/snippet.tmpl")
	if err != nil {
		return "", fmt.Errorf("SnippetContent: %w", err)
	}
	tmpl, err := template.New("snippet").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("SnippetContent parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("SnippetContent execute: %w", err)
	}
	return buf.String(), nil
}
