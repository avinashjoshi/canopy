// Command canopy install shell prints a wrapper function that lets the
// user's shell change directory based on canopy's `o` (focus) keybind.
//
// Why a printed wrapper instead of direct shell-rc modification:
// shell-rc files (.bashrc, .zshrc, config.fish, etc.) are intensely
// personal and load-order-sensitive. Writing into them silently is
// invasive; printing the snippet for the user to paste is the universal
// pattern (lazygit, zoxide, autojump, fzf all do this).
//
// The wrapper protocol is the lazygit one: set $CANOPY_NEW_DIR_FILE
// before running canopy; canopy writes the focused project root there
// when the user presses `o`; the wrapper cds on exit if the file is
// non-empty, then cleans up.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newInstallShellCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell [bash|zsh|fish]",
		Short: "Print a shell wrapper that cds to the focused project on canopy exit.",
		Long: `Prints a shell-specific wrapper function that lets canopy's 'o' (focus
project) keybind change your shell's cwd to that project's root when you
quit canopy.

How it works:

  1. Wrapper sets $CANOPY_NEW_DIR_FILE to a temp file before running canopy.
  2. Pressing 'o' on a Global-tab row writes the focused project's root
     to that file.
  3. On canopy exit, the wrapper cds your shell into the path if the
     file is non-empty, then cleans up.

Without the wrapper, 'o' still focuses the project inside canopy's TUI —
the wrapper just adds the shell-cd hand-off.

Pipe into your shell-rc:

  canopy install shell zsh >> ~/.zshrc
  canopy install shell bash >> ~/.bashrc
  canopy install shell fish >> ~/.config/fish/functions/canopy.fish

Or print without writing:

  canopy install shell zsh

Auto-detects from $SHELL when the shell name is omitted.

Allowed inside tmux: this command writes nothing — it just prints the
snippet to stdout.
`,
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		Args:        cobra.MaximumNArgs(1),
		RunE:        runInstallShell,
	}
	return cmd
}

func runInstallShell(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()

	shell := ""
	if len(args) == 1 {
		shell = args[0]
	} else {
		shell = detectShellFromEnv()
	}

	switch shell {
	case "bash", "zsh":
		fmt.Fprintln(out, posixWrapper())
	case "fish":
		fmt.Fprintln(out, fishWrapper())
	default:
		return fmt.Errorf(
			"install shell: unknown shell %q.\n"+
				"  Supported: bash, zsh, fish.\n"+
				"  Pass the shell name explicitly: canopy install shell zsh",
			shell)
	}
	return nil
}

// detectShellFromEnv reads $SHELL and returns the basename. Most shells
// set $SHELL to their absolute path; we just want "bash" / "zsh" / "fish".
// Empty string when $SHELL is unset or unrecognized — caller surfaces a
// helpful error.
func detectShellFromEnv() string {
	// Inline path-base extraction; no need to import path/filepath for
	// one shell name. $SHELL values are always absolute paths in
	// practice, but defensively handle bare names too.
	envShell := envGet("SHELL")
	for i := len(envShell) - 1; i >= 0; i-- {
		if envShell[i] == '/' {
			return envShell[i+1:]
		}
	}
	return envShell
}

// envGet wraps os.Getenv with a tiny indirection so tests can stub it
// without monkey-patching the os package. Currently a thin wrapper —
// tests pass the shell explicitly via args, so detection isn't on the
// test path.
func envGet(key string) string {
	return getenvFunc(key)
}

var getenvFunc = os.Getenv

// posixWrapper returns the bash/zsh wrapper. mktemp is portable across
// macOS + Linux (different default templates, but both accept -t).
// Falls back gracefully when canopy isn't on PATH (the wrapper is named
// `canopy` so `command canopy` finds the underlying binary regardless).
func posixWrapper() string {
	return `# canopy shell-cd wrapper (managed by ` + "`canopy install shell`" + ` — paste into ~/.bashrc or ~/.zshrc)
# Lets canopy's 'o' keybind cd your shell into the focused project on exit.
canopy() {
    local _canopy_dir_file
    _canopy_dir_file="$(mktemp -t canopy-cwd.XXXXXX)"
    CANOPY_NEW_DIR_FILE="$_canopy_dir_file" command canopy "$@"
    if [ -s "$_canopy_dir_file" ]; then
        cd "$(cat "$_canopy_dir_file")" || true
    fi
    rm -f "$_canopy_dir_file"
}`
}

// fishWrapper returns the fish-flavored equivalent. fish syntax differs
// enough from POSIX that a separate template is cleaner than a porting
// layer.
func fishWrapper() string {
	return `# canopy shell-cd wrapper (managed by ` + "`canopy install shell`" + ` — paste into ~/.config/fish/functions/canopy.fish)
# Lets canopy's 'o' keybind cd your shell into the focused project on exit.
function canopy
    set -l _canopy_dir_file (mktemp -t canopy-cwd.XXXXXX)
    CANOPY_NEW_DIR_FILE=$_canopy_dir_file command canopy $argv
    if test -s $_canopy_dir_file
        cd (cat $_canopy_dir_file)
    end
    rm -f $_canopy_dir_file
end`
}
