// Command canopy install tmux writes canopy's tmux integration (popup
// keybind + statusline interpolation) into ~/.tmux.conf, between marker
// comments so we can find our own block on re-runs.
//
// Idempotency is the load-bearing property: re-running must not duplicate
// blocks, must not silently overwrite user edits, must surface clearly
// when our block is already present.
//
// Out of scope for v0.7:
//   - `--uninstall` (remove the block). Trivial follow-up if the install
//     pattern catches on.
//   - TPM / include awareness. The marker block makes intent obvious to
//     anyone reading the file; mixing with TPM hasn't shown problems
//     in dogfood yet.
//   - User-bind conflict detection (warn if they had `bind g` already).
//     Marker block isolates ours from theirs; users see the diff plainly.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

const (
	tmuxConfMarkerStart = "# canopy:start"
	tmuxConfMarkerEnd   = "# canopy:end"

	// tmuxInstallMinVersion mirrors popupMinTmuxVersion. install must
	// refuse to write bindings the user's tmux can't run, so the bar
	// is the same: 3.2 (display-popup support).
	// 3.2 (October 2021) is the first tmux release that supports
	// display-popup. Older tmux returns "unknown command" for
	// display-popup -E "...", which would surface as a confusing
	// failure deep inside the popup keybind path. Refuse with a
	// clear message at install time instead.
	tmuxInstallMinVersion = "3.2"
)

var installTmuxLog = clog.Pkg("install-tmux")

// newInstallCmd is the parent group for `canopy install <target>`.
// Ships one target today (tmux); future targets (hypr-sidebar, etc.)
// plug in via AddCommand. The clipboard-bridge laptop-side bootstrap
// target was removed in v0.24.x — OSC 52 (see internal/clipboard's
// wl-copy.sh/wl-paste.sh) needs no laptop-side daemon or SSH config to
// install; `canopy host clipboard <name>` alone is enough.
func newInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install canopy integrations into your environment.",
		Long: `Targets:
  canopy install tmux   Wires canopy popup + statusline into ~/.tmux.conf`,
	}
	cmd.AddCommand(newInstallTmuxCmd())
	return cmd
}

func newInstallTmuxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tmux",
		Short: "Add canopy popup + statusline bindings to ~/.tmux.conf.",
		Long: `Writes a managed block to ~/.tmux.conf between marker comments:

  # canopy:start ...
  bind g run-shell "canopy popup"
  set -ag status-right " #(canopy statusline --format=current) "
  # canopy:end

Idempotent. Backs up ~/.tmux.conf to ~/.tmux.conf.canopy-backup-<timestamp>
before writing. Refuses if the marker block already exists; pass --force
to replace it in place.

Requires tmux 3.2+ (display-popup support).
`,
		// Allow inside tmux: this command writes a config file, doesn't
		// manipulate tmux server state. Users naturally invoke it from
		// inside their working tmux session.
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE:        runInstallTmux,
	}
	cmd.Flags().Bool("force", false, "replace existing canopy block if present (default: refuse and exit)")
	cmd.Flags().Bool("dry-run", false, "show what would be written without modifying ~/.tmux.conf")
	return cmd
}

func runInstallTmux(cmd *cobra.Command, _ []string) error {
	force, _ := cmd.Flags().GetBool("force")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	out := cmd.OutOrStdout()

	// Refuse on pre-3.2 tmux. Writing display-popup bindings to a tmux
	// that doesn't understand them turns the user's status bar into an
	// error message every refresh.
	t := tmux.New()
	ver, err := t.Version(cmd.Context())
	if err != nil {
		return fmt.Errorf("install tmux: tmux version check failed: %w", err)
	}
	ok, err := tmux.CompareVersions(ver, tmuxInstallMinVersion)
	if err != nil {
		return fmt.Errorf("install tmux: cannot parse tmux version %q: %w", ver, err)
	}
	if !ok {
		return fmt.Errorf(
			"install tmux: requires tmux %s+ (display-popup support).\n"+
				"  You have tmux %s. Upgrade tmux first.",
			tmuxInstallMinVersion, ver)
	}

	confPath, err := tmuxConfPath()
	if err != nil {
		return err
	}

	// Read current contents (empty if missing).
	existing, err := readTmuxConf(confPath)
	if err != nil {
		return fmt.Errorf("install tmux: read %s: %w", confPath, err)
	}

	state := detectCanopyBlock(existing)
	switch state {
	case canopyBlockMultiple:
		return fmt.Errorf(
			"install tmux: %s contains multiple canopy:start/end blocks.\n"+
				"  Hand-edit to a single block and re-run, or pass --force to overwrite all.",
			confPath)

	case canopyBlockMalformed:
		return fmt.Errorf(
			"install tmux: %s has a canopy:start without matching canopy:end (or vice versa).\n"+
				"  Hand-edit to fix the markers, or pass --force to overwrite.",
			confPath)

	case canopyBlockPresent:
		if !force {
			fmt.Fprintf(out,
				"canopy block already present in %s. Re-run with --force to replace it.\n",
				confPath)
			return nil
		}
		// Fall through to replace.
	}

	newConf := applyCanopyBlock(existing, canopyBlockBody())

	if dryRun {
		fmt.Fprintf(out, "[dry-run] would write %d bytes to %s\n", len(newConf), confPath)
		fmt.Fprintln(out, "[dry-run] new canopy block:")
		fmt.Fprintln(out, canopyBlockBody())
		return nil
	}

	// Backup BEFORE any write. Skip if the file didn't exist (nothing
	// to back up — a missing tmux.conf is a valid first-run state).
	backupPath := ""
	if existing != "" {
		backupPath, err = backupTmuxConf(confPath, existing)
		if err != nil {
			return fmt.Errorf("install tmux: backup: %w", err)
		}
	}

	// Atomic write: tempfile + rename.
	if err := atomicWriteFile(confPath, []byte(newConf), 0o644); err != nil {
		return fmt.Errorf("install tmux: write %s: %w", confPath, err)
	}

	installTmuxLog.Info("install_tmux.success",
		"path", confPath, "backup", backupPath, "force", force, "tmux_version", ver)

	fmt.Fprintf(out, "Wrote canopy block to %s.\n", confPath)
	if backupPath != "" {
		fmt.Fprintf(out, "Backup saved to %s.\n", backupPath)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Apply now without restarting tmux:")
	fmt.Fprintln(out, "  tmux source-file ~/.tmux.conf")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Then press <prefix>g (or Ctrl+Alt+c, no prefix) to open the canopy")
	fmt.Fprintln(out, "popup. The statusline shows the current workspace when you're attached")
	fmt.Fprintln(out, "to a canopy-managed session.")
	return nil
}

type canopyBlockState int

const (
	canopyBlockAbsent canopyBlockState = iota
	canopyBlockPresent
	canopyBlockMultiple  // >1 start markers — operator hand-edit territory
	canopyBlockMalformed // start without end, or vice versa
)

// detectCanopyBlock classifies the current state of marker comments in
// the user's tmux.conf. The matrix:
//
//	0 start, 0 end       → Absent
//	1 start, 1 end       → Present (good case)
//	2+ starts, anything  → Multiple (operator edit needed)
//	N starts, M ends, N≠M → Malformed (count mismatch)
func detectCanopyBlock(content string) canopyBlockState {
	starts := strings.Count(content, tmuxConfMarkerStart)
	ends := strings.Count(content, tmuxConfMarkerEnd)
	switch {
	case starts == 0 && ends == 0:
		return canopyBlockAbsent
	case starts == 1 && ends == 1:
		return canopyBlockPresent
	case starts > 1:
		return canopyBlockMultiple
	default:
		return canopyBlockMalformed
	}
}

// canopyBlockBody is the literal text we write between markers. The
// block invokes bare `canopy` (PATH-resolved at tmux runtime) rather
// than baking an absolute path. With `canopy use` swapping the
// ~/.local/bin/canopy symlink between release and dev binaries, bare
// `canopy` follows the symlink automatically — no `install tmux --force`
// re-run on every binary swap. PATH lookup is also what end users
// expect: tmux config doesn't pin to the install-time location of the
// binary, which can break later moves.
//
// Reload safety: tmux's `set -ag` (append) accumulates on every
// source-file invocation — running `tmux source-file ~/.tmux.conf`
// twice would put two canopy statusline segments in status-right.
// The block uses `run-shell` + sed to strip any pre-existing canopy
// statusline segment BEFORE appending, so reloads are idempotent.
// The strip pattern matches any `#(...statusline...)` segment regardless
// of binary path, which also handles the "old install left a stale
// entry pointing at a different binary" case.
func canopyBlockBody() string {
	bin := canopyBinForBlock()
	quoted := shellQuote(bin)
	return tmuxConfMarkerStart + ` (managed by ` + "`canopy install tmux`" + ` — edit only outside markers)
# Default keybind:
#   <prefix>g  open the unified canopy TUI as a tmux popup (workspace switcher).
#
# v0.8 unification: there is no separate "popup" subcommand anymore.
# tmux invokes ` + "`canopy`" + ` directly via display-popup -E with
# CANOPY_IN_POPUP=1 in the env; the unified TUI flips to popup-mode
# rendering (single-line tab bar, switch-client + tea.Quit on attach).
#
# -d "#{pane_current_path}" is load-bearing: it gives the popup body
# the user's pane cwd so the workspace.ResolveCurrentProject lookup
# finds the right project for the Local-tab filter. Without it, the
# popup walks up from the tmux server's cwd (typically $HOME) and the
# Local tab is empty even from inside a workspace.
#
# canopy run is shipped as a subcommand but NOT keybound by default —
# the right shape (popup vs send-keys vs spawn-pane) is still being
# explored. Bind manually if you want it; e.g.:
#   bind X display-popup -E -d "#{pane_current_path}" "canopy run"
bind g display-popup -E -w 80% -h 80% -d "#{pane_current_path}" "CANOPY_IN_POPUP=1 ` + quoted + `"
# Prefix-less alias: Ctrl+Alt+c summons canopy from any pane without
# needing the tmux prefix. -n = no prefix. The chord is unclaimed by
# common shells/editors and most terminals (Ghostty, Alacritty, kitty,
# iTerm2) forward it through to tmux unmodified. If your terminal
# swallows it, edit this binding to taste — the marker block is
# preserved across re-runs except for full --force replacement.
bind -n C-M-c display-popup -E -w 80% -h 80% -d "#{pane_current_path}" "CANOPY_IN_POPUP=1 ` + quoted + `"
# Statusline: strip any pre-existing canopy segment, then append fresh.
# The strip+append pattern keeps reloads idempotent (set -ag alone would
# accumulate duplicates on every source-file invocation). Pattern is
# anchored on "canopy" so unrelated statusline tools (e.g., a hypothetical
# user-installed #(my-statusline-tool) segment) survive untouched.
run-shell 'tmux set -gq status-right "$(tmux show -gv status-right | sed -E "s| *#\([^)]*canopy[^)]*statusline[^)]*\)||g")"'
set -ag status-right " #(` + bin + ` statusline --format=current) "
# Terminal title: tmux emits OSC 0 to the host terminal. set-titles on
# turns the feature on; set-titles-string '#S' makes the title reflect
# the tmux session name. Since canopy renames sessions to follow the
# git branch (Manager.SyncBranch), the Ghostty/iTerm/etc. tab/window
# title stays in sync with the workspace identity automatically.
set -g set-titles on
set -g set-titles-string '#S'
# status-left-length: tmux's default cap (10) truncates long canopy
# session names ("canopy-clear-workspace-identity" etc.) to garbage.
# Bump to 50 so even hyphenated branch names render in full. Doesn't
# push other segments off the line — tmux flexes status-left and
# status-right independently of these caps.
set -g status-left-length 50
` + tmuxConfMarkerEnd
}

// shellQuote wraps a path for safe inclusion in a shell command (used in
// the tmux display-popup -E body, which the user's default shell parses).
// Single-quote-wrap with escape for embedded single quotes — POSIX-safe
// across bash, zsh, dash. Bare-safe paths skip wrapping for cleaner
// generated config.
func shellQuote(s string) string {
	if isShellSafeBare(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isShellSafeBare reports whether s contains only characters that don't
// need shell quoting: alnum, slash, dash, underscore, dot, plus.
func isShellSafeBare(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '/' || c == '-' || c == '_' || c == '.' || c == '+':
		default:
			return false
		}
	}
	return true
}

// canopyBinForBlock returns the binary name embedded in the generated
// tmux config. Always bare `canopy` so the tmux popup keybind follows
// whatever ~/.local/bin/canopy currently points at. `canopy use` swaps
// that symlink between release and dev builds; with bare `canopy` here
// the swap is invisible to tmux. Embedding os.Executable() (the prior
// behavior) baked the install-time binary path into ~/.tmux.conf, which
// forced a `canopy install tmux --force` re-run on every binary swap.
//
// Kept as a function (not a constant) so the block-generation surface
// stays consistent with shellQuote and to leave a clear seam if we
// ever want to re-introduce path-baking behind a flag.
func canopyBinForBlock() string {
	return "canopy"
}

// applyCanopyBlock returns existing with the canopy block applied. If a
// block is present (single, well-formed), it's replaced in place; if
// absent, the new block is appended (with a separating blank line if
// the file is non-empty).
//
// Multiple-blocks and malformed states are operator-hand-edit territory
// and shouldn't reach this function — callers gate on detectCanopyBlock.
func applyCanopyBlock(existing, block string) string {
	state := detectCanopyBlock(existing)
	switch state {
	case canopyBlockAbsent:
		return appendCanopyBlock(existing, block)
	case canopyBlockPresent:
		return replaceCanopyBlock(existing, block)
	}
	// Defense in depth: shouldn't be reached; preserve existing as-is
	// rather than scribble over a malformed file.
	return existing
}

func appendCanopyBlock(existing, block string) string {
	if existing == "" {
		return block + "\n"
	}
	// Ensure exactly one blank line between user content and our block.
	trimmed := strings.TrimRight(existing, "\n")
	return trimmed + "\n\n" + block + "\n"
}

func replaceCanopyBlock(existing, block string) string {
	startIdx := strings.Index(existing, tmuxConfMarkerStart)
	endIdx := strings.Index(existing, tmuxConfMarkerEnd)
	if startIdx < 0 || endIdx < 0 || endIdx < startIdx {
		// Inconsistent with detectCanopyBlock contract; bail safely.
		return existing
	}
	// Find end-of-line for the end marker so we replace the full line.
	endOfBlock := endIdx + len(tmuxConfMarkerEnd)
	for endOfBlock < len(existing) && existing[endOfBlock] != '\n' {
		endOfBlock++
	}
	return existing[:startIdx] + block + existing[endOfBlock:]
}

// tmuxConfPath returns the canonical ~/.tmux.conf path. We do NOT honor
// $XDG_CONFIG_HOME / ~/.config/tmux/tmux.conf yet — most tmux setups in
// the wild use ~/.tmux.conf, and our marker block lets the user wire it
// in elsewhere with `source-file` if they prefer XDG layout.
func tmuxConfPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("install tmux: home dir: %w", err)
	}
	return filepath.Join(home, ".tmux.conf"), nil
}

func readTmuxConf(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// backupTmuxConf writes content to ~/.tmux.conf.canopy-backup-<RFC3339-ish>.
// Returns the backup path on success. Timestamp is local-time formatted
// for human readability (matches the rest of canopy's filename conventions).
func backupTmuxConf(confPath, content string) (string, error) {
	stamp := time.Now().Format("20060102-150405")
	backupPath := confPath + ".canopy-backup-" + stamp
	if err := os.WriteFile(backupPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	return backupPath, nil
}

// atomicWriteFile writes data to path via tempfile + rename, so a crash
// mid-write can't leave a half-written tmux.conf that breaks tmux. POSIX
// guarantees rename(2) is atomic within the same filesystem; we put the
// tempfile next to the target to stay on one fs.
//
// Symlink handling: if path is a symlink (common for stow/chezmoi
// users with ~/.tmux.conf -> ~/dotfiles/tmux.conf), follow the link
// and write to the resolved target. Without this, os.Rename would
// replace the symlink itself with the new file, severing the user's
// dotfile-management setup.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if resolved, err := os.Readlink(path); err == nil {
		// path is a symlink — write to its target instead. Resolve
		// relative symlinks against the symlink's directory.
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(path), resolved)
		}
		path = resolved
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmux.conf.canopy-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
