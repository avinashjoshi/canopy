// Command canopy use switches the active canopy binary by retargeting
// the ~/.local/bin/canopy symlink. The whole point: from any worktree
// (or anywhere on PATH), you can flip between the released canopy and
// any in-flight feature build without rebuilding the released one.
//
// Three target kinds:
//
//   canopy use release            symlink -> canopy.bin (alias: main)
//   canopy use <workspace>        symlink -> workspace's ./canopy
//   canopy use --build <ws>       go build in <ws> first, then switch
//
// `canopy use` with no arg prints the active target plus a list of
// every workspace canopy knows about, with each row showing whether
// the worktree has been built and how long ago.
//
// The symlink swap is atomic: a tempfile-symlink + os.Rename, so a
// crash mid-swap can never leave ~/.local/bin/canopy in a "neither old
// nor new" state. `make dev` and `make release` are thin Makefile
// wrappers calling this subcommand — keep behavior here authoritative.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/state"
)

var useLog = clog.Pkg("use")

const (
	// useReleaseAlias is the user-facing name for the release target.
	// "main" is also accepted because Avi's mental model uses "main"
	// for the released-from-main-branch binary.
	useReleaseAlias     = "release"
	useReleaseAliasMain = "main"

	// canopyBinName / canopyRealBinName: the symlink and its target's
	// basename. Match the Makefile constants exactly — anyone editing
	// these must update Makefile in lockstep.
	canopyBinName     = "canopy"
	canopyRealBinName = "canopy.bin"
)

func newUseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "use [target]",
		Short: "Switch the active canopy binary by retargeting ~/.local/bin/canopy.",
		Long: `Targets:
  release           ~/.local/bin/canopy -> canopy.bin (the installed release binary).
                    Alias: main.
  <workspace>       ~/.local/bin/canopy -> <worktree>/canopy (a dev build).
                    Workspace must already have ./canopy built; pass --build to
                    rebuild it first.

With no argument, prints the active target and a list of every workspace
canopy knows about, with built-or-not status for each.

Switching is a symlink retarget, not a rebuild — flipping between dev
and release is fast (under 100ms) and the released canopy.bin is never
modified by 'canopy use'. Released binary is rebuilt only by 'make
install' on the main branch.

Examples:
  canopy use                       # show current + list available
  canopy use release               # back to released canopy
  canopy use feature-A             # spin workspace feature-A's binary
  canopy use --build feature-A     # build, then switch
`,
		// Allow running inside tmux: canopy use is the "from anywhere"
		// switcher; forcing the user to drop to a non-canopy shell
		// would defeat the whole UX.
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE:        runUse,
	}
	cmd.Flags().Bool("build", false, "build <workspace>'s ./canopy before switching (workspace targets only)")
	return cmd
}

func runUse(cmd *cobra.Command, args []string) error {
	build, _ := cmd.Flags().GetBool("build")
	out := cmd.OutOrStdout()

	binDir, err := canopyBinDir()
	if err != nil {
		return err
	}
	symlinkPath := filepath.Join(binDir, canopyBinName)
	releaseTargetPath := filepath.Join(binDir, canopyRealBinName)

	if len(args) == 0 {
		if build {
			return errors.New("canopy use --build requires a workspace target")
		}
		return printUseList(cmd.Context(), out, symlinkPath, releaseTargetPath)
	}

	target := args[0]
	if target == useReleaseAlias || target == useReleaseAliasMain {
		if build {
			return errors.New("canopy use --build is only valid with a workspace target, not 'release'")
		}
		return switchToRelease(symlinkPath, releaseTargetPath, out)
	}
	return switchToWorkspace(cmd.Context(), target, build, symlinkPath, out)
}

// switchToRelease retargets symlinkPath at canopy.bin (relative — the
// symlink lives in the same dir as the target, so a relative target is
// stable across moves of the bin dir). Refuses if canopy.bin is missing
// because flipping to a phantom would silently break tmux popup +
// `canopy` invocations everywhere.
func switchToRelease(symlinkPath, releaseTargetPath string, out io.Writer) error {
	if _, err := os.Stat(releaseTargetPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"canopy use release: no release binary at %s\n"+
				"  Run 'make install' on the main branch first to populate it,\n"+
				"  then 'canopy use release' will flip the symlink back here from any worktree.",
			releaseTargetPath)
	} else if err != nil {
		return fmt.Errorf("canopy use release: stat %s: %w", releaseTargetPath, err)
	}

	if err := atomicSymlink(canopyRealBinName, symlinkPath); err != nil {
		return fmt.Errorf("canopy use release: %w", err)
	}
	useLog.Info("use.release", "symlink", symlinkPath, "target", canopyRealBinName)
	fmt.Fprintf(out, "Active: %s -> %s\n", symlinkPath, canopyRealBinName)
	fmt.Fprintln(out, "Mode:   release")
	return nil
}

// switchToWorkspace looks up <name> in state.json, locates its
// ./canopy, and retargets symlinkPath at it. With --build, a fresh
// `go build` runs in the worktree first.
//
// Failure modes (each returns a clear error message):
//   - state.json missing or unreadable
//   - workspace name not in registry (lists known names so the user
//     sees what they could have meant)
//   - dev binary missing (suggests --build)
func switchToWorkspace(ctx context.Context, name string, build bool, symlinkPath string, out io.Writer) error {
	st, err := loadStateForUse()
	if err != nil {
		return err
	}

	ws := findWorkspaceByName(st, name)
	if ws == nil {
		return errUnknownWorkspace(name, st)
	}
	// Refuse non-canopy worktrees with a clearer message than "no
	// dev binary at <path>". The latter would leave the user thinking
	// they need to run `make build` somewhere — but they can't,
	// because the worktree isn't canopy source.
	if !isCanopyWorktree(ws.Path) {
		return fmt.Errorf(
			"canopy use %s: workspace %q exists but isn't a canopy source worktree.\n"+
				"  Project: %s\n"+
				"  Path:    %s\n"+
				"  canopy use only works with worktrees of github.com/avinashjoshi/canopy itself.\n"+
				"  Run 'canopy use' (no args) to see canopy worktrees you can switch to.",
			name, name, ws.Project, ws.Path)
	}

	devBin := filepath.Join(ws.Path, "canopy")

	if build {
		fmt.Fprintf(out, "Building %s in %s ...\n", canopyBinName, ws.Path)
		if err := goBuildInWorktree(ctx, ws.Path); err != nil {
			return fmt.Errorf("canopy use --build %s: %w", name, err)
		}
	}

	if _, err := os.Stat(devBin); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"canopy use %s: no dev binary at %s\n"+
				"  Run 'make build' in that worktree first, or pass --build to do it now:\n"+
				"    canopy use --build %s",
			name, devBin, name)
	} else if err != nil {
		return fmt.Errorf("canopy use %s: stat %s: %w", name, devBin, err)
	}

	if err := atomicSymlink(devBin, symlinkPath); err != nil {
		return fmt.Errorf("canopy use %s: %w", name, err)
	}
	useLog.Info("use.workspace", "symlink", symlinkPath, "target", devBin, "workspace", name)
	fmt.Fprintf(out, "Active: %s -> %s\n", symlinkPath, devBin)
	fmt.Fprintln(out, "Mode:   DEV")
	return nil
}

// printUseList renders the no-args output: the active symlink target
// followed by a tab-aligned list of every workspace canopy knows about.
// Always exits 0 — a missing state file or unreadable worktree is shown
// as "(not built)" rather than failing the whole listing.
func printUseList(ctx context.Context, out io.Writer, symlinkPath, releaseTargetPath string) error {
	// Header: where the symlink points right now.
	if currentTarget, err := os.Readlink(symlinkPath); err == nil {
		fmt.Fprintf(out, "Active: %s -> %s\n\n", symlinkPath, currentTarget)
	} else if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "Active: %s (not installed; run install.sh or `make install` first)\n\n", symlinkPath)
	} else {
		// File exists but isn't a symlink — uncommon, surface plainly.
		fmt.Fprintf(out, "Active: %s (not a symlink; canopy expects %s -> canopy.bin)\n\n", symlinkPath, symlinkPath)
	}

	st, _ := loadStateForUse() // ignore err; empty list is a valid view

	fmt.Fprintln(out, "Available targets:")
	tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "  TARGET\tPATH\tVERSION\tBUILT")
	fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
		useReleaseAlias,
		releaseTargetPath,
		releaseVersionLabel(ctx, releaseTargetPath),
		builtAgo(releaseTargetPath))

	canopyOnly := 0
	skipped := 0
	if st != nil {
		// Sort workspaces alphabetically for stable output. Without sort
		// the order depends on JSON load order, which is itself unstable.
		// Only include canopy source worktrees — rows from other projects
		// (Rails, Python, etc. registered in the same canopy state.json)
		// can't have ./canopy and would just be noise in the listing.
		names := make([]string, 0, len(st.Workspaces))
		for _, ws := range st.Workspaces {
			if isCanopyWorktree(ws.Path) {
				names = append(names, ws.Name)
			} else {
				skipped++
			}
		}
		sort.Strings(names)
		for _, n := range names {
			ws := findWorkspaceByName(st, n)
			devBin := filepath.Join(ws.Path, canopyBinName)
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				ws.Name, devBin, devVersionLabel(devBin), builtAgo(devBin))
			canopyOnly++
		}
	}
	tw.Flush()

	// One-line footer when we filtered anything out, so the user knows
	// the listing is canopy-only on purpose. Without this, someone with
	// a workspace named "feature-X" in a non-canopy project might think
	// `canopy use` lost it.
	if skipped > 0 {
		fmt.Fprintf(out, "\n  (%d workspace(s) from other projects skipped — canopy use only works with canopy source worktrees)\n", skipped)
	}
	if canopyOnly == 0 && st != nil && len(st.Workspaces) > 0 {
		fmt.Fprintln(out, "  (no canopy source worktrees registered)")
	}
	return nil
}

// releaseVersionLabel runs `<binPath> version` and parses the version
// out of the first line ("canopy v0.12.2+abc1234" → "v0.12.2+abc1234").
// On any failure (missing binary, exec error, parse miss) it returns
// "—" so the row still renders cleanly. Capped at 2s so a wedged
// release binary can't hang `canopy use`.
//
// Dev rows go through devVersionLabel instead — make build is dev-by-
// convention and an exec there would just confirm what we already know.
//
// var-typed for the same reason as goBuildInWorktree: lets tests stub
// the exec without spawning a real subprocess.
var releaseVersionLabel = func(ctx context.Context, binPath string) string {
	if _, err := os.Stat(binPath); err != nil {
		return "—"
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, binPath, "version").Output()
	if err != nil {
		return "—"
	}
	if v := parseVersionLine(string(out)); v != "" {
		return v
	}
	return "—"
}

// devVersionLabel returns "DEV" if the dev binary exists, "—" if it
// doesn't. Keeps the column scannable without forking the binary —
// `make build` produces dev binaries by convention.
func devVersionLabel(devBin string) string {
	if _, err := os.Stat(devBin); err != nil {
		return "—"
	}
	return "DEV"
}

// parseVersionLine extracts the version label from the first line of
// `canopy version` output. The first line is always "canopy <label>"
// (see formatVersionDetails) — for releases that's the version string,
// for dev builds it's literally "DEV".
//
// Returns "" if the output doesn't start with "canopy " — caller maps
// that to "—" so a future format change degrades to a missing column,
// not a misleading row.
func parseVersionLine(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	line = strings.TrimSpace(line)
	if rest, ok := strings.CutPrefix(line, "canopy "); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

// builtAgo returns a human-friendly "built Xh ago" string for path's
// mtime, or "(not built)" if the file doesn't exist. Keeps output
// scannable without committing to a real stale-detection heuristic.
func builtAgo(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "(not built)"
	}
	d := time.Since(info.ModTime())
	switch {
	case d < time.Minute:
		return "built just now"
	case d < time.Hour:
		return fmt.Sprintf("built %dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("built %dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("built %dd ago", int(d.Hours()/24))
	}
}

// atomicSymlink retargets symlinkPath at target without ever leaving
// the link in a half-replaced state. POSIX rename is atomic within a
// filesystem; we put the temp symlink in the same dir as the
// destination to keep the rename on one fs.
//
// Why this matters: tmux invokes bare `canopy` every status-interval
// (~15s) plus on every popup keybind press. A non-atomic replace could
// land between syscalls and the popup would launch nothing for one
// tick.
func atomicSymlink(target, symlinkPath string) error {
	dir := filepath.Dir(symlinkPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// Tempfile name uses pid + nanos to dodge collisions when two
	// canopy use invocations race. Worst case: one of them overwrites
	// the other's tmp link before rename — both then succeed, and the
	// last writer wins as expected.
	tmp := filepath.Join(dir, fmt.Sprintf(".canopy-symlink-tmp-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", tmp, target, err)
	}
	if err := os.Rename(tmp, symlinkPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, symlinkPath, err)
	}
	return nil
}

// goBuildInWorktree runs `go build -o canopy ./cmd/canopy` in dir.
// Captures stderr so build failures surface with full compiler output
// — no point hiding "undefined: foo" behind a generic "build failed."
//
// Deliberately runs without -ldflags. Dev binaries should keep
// version="dev" so the DEV banner fires; injecting a fake version here
// would break the visual indicator that's the whole point of `make dev`.
var goBuildInWorktree = func(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", canopyBinName, "./cmd/canopy")
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build in %s: %w\n%s", dir, err, stderr.String())
	}
	return nil
}

// canopyBinDir returns ~/.local/bin (where install.sh and the Makefile
// place canopy + canopy.bin). Single source of truth so install.sh,
// Makefile, and `canopy use` all agree.
func canopyBinDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("canopy use: home dir: %w", err)
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// loadStateForUse opens canopy's state.json. Read-only; no flock.
// Brief stale window (<1 status-interval) is fine — `canopy use` is
// interactive, not part of a hot loop, and even a stale row points at
// a worktree that hasn't moved.
func loadStateForUse() (*state.State, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("canopy use: home dir: %w", err)
	}
	store, err := state.NewStore(filepath.Join(home, ".canopy"))
	if err != nil {
		return nil, fmt.Errorf("canopy use: open store: %w", err)
	}
	st, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("canopy use: load state: %w", err)
	}
	return st, nil
}

// findWorkspaceByName returns the first workspace whose Name matches.
// O(n) linear scan over state.Workspaces is fine — registries are
// dozens-not-thousands sized; a map index would just add deserialization
// cost.
func findWorkspaceByName(st *state.State, name string) *state.Workspace {
	if st == nil || name == "" {
		return nil
	}
	for i := range st.Workspaces {
		if st.Workspaces[i].Name == name {
			return &st.Workspaces[i]
		}
	}
	return nil
}

// isCanopyWorktree reports whether a workspace dir is a canopy source
// worktree. Test: does it have cmd/canopy/main.go? That file is the
// canopy entry point and is unique to this repo — a Rails worktree
// (e.g., a cravd workspace registered in the same canopy state.json)
// won't have it.
//
// Filter is applied at both list time (skip non-canopy rows in the
// `canopy use` output) and at switch time (refuse with a clearer
// error than "no dev binary"). The state.json mixes workspaces from
// every project canopy manages, so without this filter the list shows
// rows the user can't possibly `canopy use` against.
func isCanopyWorktree(workspacePath string) bool {
	if workspacePath == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(workspacePath, "cmd", "canopy", "main.go"))
	return err == nil
}

// errUnknownWorkspace formats the "unknown workspace" error with the
// list of names the user could have meant. The fast diagnostic — they
// usually mistyped a hyphen or capitalized a letter, and seeing the
// real names side-by-side is faster than re-running `canopy use` for
// the listing.
//
// Suggestions are filtered to canopy worktrees only — listing a
// cravd workspace as a "did you mean" suggestion when canopy use
// can't actually use it would mislead.
func errUnknownWorkspace(name string, st *state.State) error {
	available := []string{useReleaseAlias}
	if st != nil {
		for _, ws := range st.Workspaces {
			if isCanopyWorktree(ws.Path) {
				available = append(available, ws.Name)
			}
		}
	}
	sort.Strings(available)
	return fmt.Errorf("canopy use: unknown target %q\n  Available: %s", name, strings.Join(available, ", "))
}
