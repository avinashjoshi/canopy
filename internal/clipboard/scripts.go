package clipboard

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"text/template"
)

// scriptsFS embeds the wrapper scripts into the canopy binary. Pushing
// them to remote hosts is a stdin-pipe over SSH (cat > ~/.local/bin/
// wl-paste etc.) — same payload-delivery pattern internal/host.InstallScript
// already uses for the canopy installer.
//
//go:embed scripts/wl-paste.sh scripts/wl-copy.sh
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
