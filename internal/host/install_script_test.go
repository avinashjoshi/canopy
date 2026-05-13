package host

import (
	"strings"
	"testing"
)

// TestInstallScript covers both branches of the --reinstall conditional
// and confirms the script wires curl AND wget into the fetch fallback
// (Alpine boxes don't ship curl by default; we need wget to work even
// though the install.sh README assumes curl).
func TestInstallScript(t *testing.T) {
	t.Run("default install (no reinstall)", func(t *testing.T) {
		s := InstallScript(false)
		if !strings.Contains(s, "--yes") {
			t.Errorf("script must always pass --yes (non-interactive); got:\n%s", s)
		}
		if strings.Contains(s, "--reinstall") {
			t.Errorf("script must NOT include --reinstall when flag is false; got:\n%s", s)
		}
		if !strings.Contains(s, "curl -fsSL") {
			t.Errorf("script must include curl fallback; got:\n%s", s)
		}
		if !strings.Contains(s, "wget -qO-") {
			t.Errorf("script must include wget fallback; got:\n%s", s)
		}
		if !strings.Contains(s, InstallURL) {
			t.Errorf("script must fetch from InstallURL=%q; got:\n%s", InstallURL, s)
		}
		if !strings.Contains(s, "neither curl nor wget") {
			t.Errorf("script must surface a clear error when neither fetcher is available; got:\n%s", s)
		}
	})

	t.Run("reinstall mode", func(t *testing.T) {
		s := InstallScript(true)
		if !strings.Contains(s, "--reinstall") {
			t.Errorf("script must include --reinstall when flag is true; got:\n%s", s)
		}
		// --yes is still required so the remote doesn't hang on prompts.
		if !strings.Contains(s, "--yes") {
			t.Errorf("script must still include --yes alongside --reinstall; got:\n%s", s)
		}
	})
}
