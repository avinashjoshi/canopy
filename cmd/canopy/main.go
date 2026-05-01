// Command canopy is a TUI for managing git worktrees with paired tmux
// sessions. See docs/design/v0-canopy.md for the full design.
//
// Running `canopy` with no arguments opens the workspace TUI (once it's
// implemented). Until then, only `canopy version` and the standard cobra
// help output are wired up.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clog"
)

// debugFlag is the --debug switch on the root command. When true, the log
// level is bumped from INFO to DEBUG before any other canopy package runs.
var debugFlag bool

// version, commit, and date are injected via -ldflags by `make install`
// (see Makefile LDFLAGS). When canopy is built via `make build` /
// `make dev` / `go install`, these stay at their defaults — `version`
// stays "dev", which is what drives the DEV banner in the TUI status
// bar and the [DEV:branch] suffix in the tmux statusline.
//
// The "dev" sentinel is load-bearing: `versionDetails()` checks
// version == "dev" to decide whether the running binary is a dev build,
// and the UI uses the same signal to color the version pill.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	// Resolve once at startup so --help can lead with the active version
	// (matches the convention "$tool $version" most CLIs expose). Cheap
	// — versionDetails is a few syscalls plus one optional git fork for
	// dev builds.
	helpHeader := helpVersionLine(versionDetails()) + "\n\n"
	root := &cobra.Command{
		Use:   "canopy",
		Short: "TUI for managing git worktrees with paired tmux sessions and per-project setup hooks.",
		Long: helpHeader +
			"   _____\n" +
			"  / ____|\n" +
			" | |     __ _ _ __   ___  _ __  _   _\n" +
			" | |    / _` | '_ \\ / _ \\| '_ \\| | | |\n" +
			" | |___| (_| | | | | (_) | |_) | |_| |\n" +
			"  \\_____\\__,_|_| |_|\\___/| .__/ \\__, |\n" +
			"                         | |     __/ |\n" +
			"                         |_|    |___/\n" +
			"\n" +
			"Canopy creates per-branch git worktrees, runs configurable setup\n" +
			"and teardown scripts, and pairs each workspace with a 4-pane tmux\n" +
			"session (nvim / claude / shell / server). One TUI lets you switch\n" +
			"between workspaces and resurrect them after reboots.\n",
		// Bare `canopy` (no subcommand) launches the unified TUI, which
		// is safe to run from inside an existing tmux session — it
		// uses tmux switch-client (not nested attach) for selecting a
		// workspace, so no nesting risk. The guard still fires on
		// destructive subcommands (new, rm, retry) which DO spawn or
		// modify tmux sessions and shouldn't run nested.
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		// Suppress cobra's usage dump on RunE errors. RunE failures are
		// almost always "couldn't find canopy.json" / "tmux missing" /
		// state-related — surfacing the flag table for those is noise
		// that buries the real message. Errors still print (we don't
		// SilenceErrors); just no usage block under them.
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Guard first: if canopy is running inside an existing tmux
			// session (and isn't an explicitly-allowed subcommand like
			// `version`), refuse before doing any other work. Surfaces
			// to the user as a cobra error, which prints to stderr and
			// exits non-zero — clean and visible.
			if err := enforceNoNestedTmux(cmd); err != nil {
				return err
			}
			teardown, err := clog.Init(debugFlag)
			if err != nil {
				return err
			}
			cobra.OnFinalize(teardown)
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// `canopy` with no subcommand is context-sensitive (see
			// route.go for the full dispatch table):
			//   - inside a project   → project TUI
			//   - in a fresh git repo → init splash
			//   - anywhere else      → global TUI
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("canopy: getwd: %w", err)
			}
			return routeRoot(cmd.Context(), cwd, cmd.OutOrStdout())
		},
	}
	root.PersistentFlags().BoolVar(&debugFlag, "debug", false, "enable DEBUG-level logging to ~/.canopy/log/canopy.log")

	root.AddCommand(versionCmd())
	root.AddCommand(initCmd())
	root.AddCommand(newCmd())
	root.AddCommand(lsCmd())
	root.AddCommand(switchCmd())
	root.AddCommand(rmCmd())
	root.AddCommand(reconcileCmd())
	root.AddCommand(mainCmd())
	root.AddCommand(retryCmd())
	// popup + popup-inner removed in v0.8 (TUI unification): tmux
	// invokes `canopy` directly via display-popup -E with
	// CANOPY_IN_POPUP=1 in the env. See cmd/canopy/install_tmux.go.
	root.AddCommand(newStatuslineCmd())
	root.AddCommand(newInstallCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newUseCmd())
	root.AddCommand(newUpgradeCmd())

	if err := root.Execute(); err != nil {
		// cobra has already printed the error; just exit non-zero.
		os.Exit(1)
	}
}

// VersionDetails captures everything the version subcommand and the UI
// status bar need to surface at runtime: which binary is active, where
// it lives, whether it's a dev or release build, and (when dev) which
// workspace it came from. Fields are filled in by versionDetails() on
// demand; populating once per process is cheap.
type VersionDetails struct {
	// Version is "dev" for worktree builds, or "v<semver>+<sha>" for
	// `make install`-built binaries. The literal "dev" string is the
	// signal that drives the DEV banner — keep it stable.
	Version string

	// Commit is the short HEAD sha at build time. Empty for `go install`
	// builds (BuildInfo fills it in there), set by ldflags otherwise.
	Commit string

	// Date is the UTC build timestamp (RFC3339-ish). Same provenance
	// as Commit.
	Date string

	// BinaryPath is the absolute path of the running binary, with one
	// level of symlink resolution. For `~/.local/bin/canopy` →
	// `~/.local/bin/canopy.bin`, both paths are surfaced via the
	// SymlinkTarget field.
	BinaryPath string

	// SymlinkTarget is the resolved target of BinaryPath if it's a
	// symlink, otherwise empty. Lets `canopy version` show
	//   binary: ~/.local/bin/canopy -> canopy.bin
	// without doing the resolution itself.
	SymlinkTarget string

	// IsDev is shorthand for Version == "dev". True when the running
	// binary was built without ldflags (worktree build via `make build`
	// / `make dev`, or a raw `go build`).
	IsDev bool

	// DevWorkspace is the canopy workspace name when IsDev is true and
	// the binary lives inside a known canopy worktree. Empty when not
	// resolvable (binary in ~/Work/canopy directly, or some other dir).
	// Used by the TUI DEV pill and the statusline [DEV:branch] suffix.
	DevWorkspace string
}

// versionCmd resolves canopy's version using runtime/debug.ReadBuildInfo
// when ldflags didn't run (e.g., `go install` or `make build`), and
// surfaces the result as a multi-line block so the user can answer
// "which canopy am I running" without guessing. The block always
// includes the binary path so symlink confusion is debuggable.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print canopy's version, commit, build date, and active binary path.",
		// version is the one subcommand we let users run from inside an
		// existing tmux session — it's the canonical "is canopy
		// installed?" probe and must answer regardless of context. See
		// enforceNoNestedTmux + allowInTmuxAnnotation in guard.go.
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		Run: func(cmd *cobra.Command, args []string) {
			d := versionDetails()
			fmt.Fprint(cmd.OutOrStdout(), formatVersionDetails(d))
		},
	}
}

// versionInfo is a thin compat wrapper for code that only wants the
// classic (version, commit, date) tuple. Newer call sites should use
// versionDetails() directly so the binary path + dev workspace stay
// available without re-resolving.
func versionInfo() (string, string, string) {
	d := versionDetails()
	return d.Version, d.Commit, d.Date
}

// versionDetails populates a VersionDetails for the running process.
// Cheap (a few syscalls + one git fork in the dev case); safe to call
// multiple times. Always returns a usable struct — every field has a
// graceful fallback so a partial filesystem failure can't break
// `canopy version` for the user.
func versionDetails() VersionDetails {
	// Capture the original ldflags-injected (or default) version BEFORE
	// any BuildInfo fallback. The fallback below may overwrite a literal
	// "dev" with a pseudo-version (useful display for `go install`
	// users), but IsDev must still be driven by the original sentinel
	// — otherwise `make build` binaries appear as releases, the DEV
	// banner doesn't fire, and `canopy upgrade` doesn't refuse them.
	rawVersion := version

	d := VersionDetails{
		Version: version,
		Commit:  commit,
		Date:    date,
	}

	// `go install` and `make build` don't run ldflags, so commit/date
	// are empty. Fall back to BuildInfo so they still surface useful
	// values for users who installed via go install.
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if d.Commit == "" {
					if len(s.Value) >= 7 {
						d.Commit = s.Value[:7]
					} else {
						d.Commit = s.Value
					}
				}
			case "vcs.time":
				if d.Date == "" {
					d.Date = s.Value
				}
			}
		}
		// Module version is the `go install`-time pseudo-version; only
		// useful when ldflags are absent AND module info is real. We
		// surface it for forensic display but rawVersion still drives
		// IsDev below.
		if d.Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			d.Version = info.Main.Version
		}
	}

	if d.Commit == "" {
		d.Commit = "unknown"
	}
	if d.Date == "" {
		d.Date = "unknown"
	}

	d.IsDev = rawVersion == "dev"

	if bin, err := os.Executable(); err == nil {
		d.BinaryPath = bin
		// One level of symlink resolution: enough to surface "canopy
		// -> canopy.bin" without traversing the target's own chain
		// (which is rarely useful and clutters output).
		if target, lerr := os.Readlink(bin); lerr == nil {
			d.SymlinkTarget = target
		}
	}

	if d.IsDev {
		d.DevWorkspace = devWorkspaceFromBinary(d.BinaryPath)
	}

	return d
}

// devWorkspaceFromBinary returns the canopy workspace name for a
// binary that lives inside a canopy-managed worktree, or "" if not
// resolvable.
//
// Resolution order:
//  1. Path heuristic. ~/.canopy/workspaces/<project>/<name>/canopy is
//     canopy's canonical worktree shape; the workspace name IS the
//     parent dir basename. Cheap, no fork.
//  2. Git fallback. For binaries built outside that layout (e.g.,
//     ~/Work/canopy/canopy from a contributor's source clone), shell
//     out to `git -C <bin-dir> branch --show-current`. Adds one fork
//     but only fires for dev builds and only when path heuristic
//     misses, so the cost is bounded.
//
// A failure at either step returns "" — the UI degrades to plain "DEV"
// rather than crashing or showing a wrong name.
func devWorkspaceFromBinary(binaryPath string) string {
	if binaryPath == "" {
		return ""
	}
	// Resolve symlinks once: the heuristic and the git fallback both
	// need the on-disk location of the binary, not the symlink path.
	resolved, err := filepath.EvalSymlinks(binaryPath)
	if err != nil {
		resolved = binaryPath
	}
	parent := filepath.Dir(resolved)

	// Heuristic: parent's parent's parent should be "workspaces" in
	// canopy's canonical layout. Walk three levels up cheaply rather
	// than calling os.Stat on each.
	pp := filepath.Dir(parent)
	ppp := filepath.Dir(pp)
	if filepath.Base(ppp) == "workspaces" {
		return filepath.Base(parent)
	}

	// Git fallback. We don't log here — devWorkspace is best-effort and
	// a missing branch shouldn't pollute canopy.log every time the user
	// types `canopy version`.
	out, err := runGitBranchShowCurrent(parent)
	if err != nil || out == "" {
		return ""
	}
	return out
}

// runGitBranchShowCurrent is split out so tests can stub it via a
// package-level var. Returns the trimmed branch name on success, or
// ("", err) if git fails or the directory isn't a worktree.
var runGitBranchShowCurrent = func(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// formatVersionDetails renders a VersionDetails as the multi-line block
// users see from `canopy version`. Layout:
//
//	canopy v0.12.0+abc1234            (release)
//	  commit:  abc1234
//	  built:   2026-04-30T12:34:56Z
//	  binary:  /home/avi/.local/bin/canopy -> canopy.bin
//	  mode:    release
//
// or, for a dev build:
//
//	canopy DEV
//	  workspace: install-and-dev-workflow
//	  binary:    /home/avi/.local/bin/canopy -> /home/avi/.canopy/.../canopy
//	  commit:    abc1234
//	  mode:      DEV
func formatVersionDetails(d VersionDetails) string {
	var b strings.Builder
	if d.IsDev {
		b.WriteString("canopy DEV\n")
		ws := d.DevWorkspace
		if ws == "" {
			ws = "(untracked)"
		}
		fmt.Fprintf(&b, "  workspace: %s\n", ws)
	} else {
		fmt.Fprintf(&b, "canopy %s\n", d.Version)
	}

	if d.BinaryPath != "" {
		if d.SymlinkTarget != "" {
			fmt.Fprintf(&b, "  binary:    %s -> %s\n", d.BinaryPath, d.SymlinkTarget)
		} else {
			fmt.Fprintf(&b, "  binary:    %s\n", d.BinaryPath)
		}
	}
	if d.Commit != "" && d.Commit != "unknown" {
		fmt.Fprintf(&b, "  commit:    %s\n", d.Commit)
	}
	if d.Date != "" && d.Date != "unknown" {
		fmt.Fprintf(&b, "  built:     %s\n", d.Date)
	}
	if d.IsDev {
		b.WriteString("  mode:      DEV\n")
	} else {
		b.WriteString("  mode:      release\n")
	}
	return b.String()
}

// helpVersionLine renders the one-line version label that leads
// `canopy --help`. Format mirrors the convention `$tool $version`:
//
//	canopy v0.12.2+abc1234        (release)
//	canopy DEV (calm-firefly)     (dev build inside a known worktree)
//	canopy DEV                    (dev build, workspace not resolvable)
//
// Kept distinct from formatVersionDetails — that one is the multi-line
// `canopy version` block; this one is a single line for the help banner.
func helpVersionLine(d VersionDetails) string {
	if d.IsDev {
		if d.DevWorkspace != "" {
			return fmt.Sprintf("canopy DEV (%s)", d.DevWorkspace)
		}
		return "canopy DEV"
	}
	return "canopy " + d.Version
}
