package host

import "fmt"

// InstallURL is the canonical raw VCS URL for install.sh on main. Same
// distribution shape as the README one-liner — keeping this constant
// in one place means a user who already trusts the curl|sh URL has no
// reason to distrust the in-canopy install paths (CLI + TUI).
const InstallURL = "https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh"

// InstallScript builds the bash script we pipe to a remote canopy
// host via SSH to install (or reinstall) canopy. Two surfaces share
// this builder:
//
//   - cmd/canopy host install <name>     — CLI surface
//   - internal/ui Hosts tab `I` keybind  — in-TUI surface
//
// The script always passes --yes to the remote install.sh because the
// SSH stdin is not a tty; install.sh's interactive prompts would skip
// silently otherwise. When reinstall is true, --reinstall is appended
// so the remote wipes ~/.canopy/src before re-cloning.
//
// The curl/wget fallback exists for Alpine-flavored boxes that don't
// pre-install curl. install.sh itself only needs git after the initial
// fetch, so the script doesn't need both tools — just one to bootstrap.
//
// Exposing this as a single string (rather than `bash -lc <multi-arg>`)
// is deliberate: SSH joins post-target argv with spaces and the remote
// shell re-parses, which would word-split control flow keywords. See
// internal/host/refresh.go for the canonical explanation.
func InstallScript(reinstall bool) string {
	flags := "--yes"
	if reinstall {
		flags += " --reinstall"
	}
	return fmt.Sprintf(`set -e
if command -v curl >/dev/null 2>&1; then
  curl -fsSL %s | bash -s -- %s
elif command -v wget >/dev/null 2>&1; then
  wget -qO- %s | bash -s -- %s
else
  echo "canopy host install: neither curl nor wget available on remote" >&2
  exit 1
fi`, InstallURL, flags, InstallURL, flags)
}
