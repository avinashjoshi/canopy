// Command `canopy install clipboard-bridge` is the one-time per-laptop
// bootstrap for the v0.18 clipboard bridge. Writes the systemd user
// unit that supervises `canopy clipboard-server`, adds the
// `Include ~/.ssh/config.d/canopy/*.conf` directive to ~/.ssh/config
// (creating the file if missing), and ensures ~/.ssh/config.d/canopy/
// exists for per-host snippets.
//
// Idempotent — safe to re-run. Per-host setup is a separate verb,
// `canopy host clipboard <name>` (Lane C).
//
// See docs/design/v0.18-clipboard-bridge.md for the full feature design.
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clipboard"
)

// newInstallClipboardBridgeCmd returns the `canopy install
// clipboard-bridge` cobra subcommand. Registered alongside the existing
// tmux target by newInstallCmd() in install_tmux.go.
//
// Allowed inside an existing tmux session because it only edits config
// files in ~/.config/ and ~/.ssh/ — no tmux server state is touched.
func newInstallClipboardBridgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clipboard-bridge",
		Short: "Set up the laptop-side clipboard bridge (systemd user unit + SSH config Include)",
		Long: "One-time per laptop. Performs four actions:\n\n" +
			"  1. Writes ~/.config/systemd/user/canopy-clipboard.service that\n" +
			"     supervises `canopy clipboard-server`. ExecStart uses the\n" +
			"     ~/.local/bin/canopy symlink so `canopy use` swaps survive\n" +
			"     without reinstall.\n" +
			"  2. Creates ~/.ssh/config.d/canopy/ for per-host SSH snippets.\n" +
			"  3. Adds `Include ~/.ssh/config.d/canopy/*.conf` to ~/.ssh/config\n" +
			"     between `# canopy:start clipboard-bridge` markers — the\n" +
			"     same marker-block pattern `canopy install tmux` uses.\n" +
			"  4. systemctl --user daemon-reload && enable --now\n" +
			"     canopy-clipboard.service.\n\n" +
			"Idempotent — re-running is safe. After this, per-host setup\n" +
			"is `canopy host clipboard <name>` (or the `c` key on the Hosts\n" +
			"tab).",
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			installer, err := clipboard.NewLocalInstaller()
			if err != nil {
				return err
			}
			if err := installer.Install(cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "")
				fmt.Fprintln(cmd.ErrOrStderr(), "Install failed. Common causes:")
				fmt.Fprintln(cmd.ErrOrStderr(), "  - No user systemd session (running over plain SSH without --user lingering)")
				fmt.Fprintln(cmd.ErrOrStderr(), "    Fix: enable lingering with `loginctl enable-linger $USER`")
				fmt.Fprintln(cmd.ErrOrStderr(), "  - $HOME/.local/bin/canopy is not the active symlink (run `canopy use release` or `canopy use <workspace>`)")
				return err
			}
			return nil
		},
	}
}
