// Command `canopy host clipboard <name>` is the per-host installer for
// the v0.18 clipboard bridge. SSHes to the registered host, detects
// its UID, pushes the wl-paste / wl-copy wrappers, writes the SSH
// snippet for this host into ~/.ssh/config.d/canopy/<name>.conf, and
// verifies the wrapper round-trips text/plain.
//
// Re-runs are idempotent. The TUI `c` keybind on the Hosts tab is the
// other surface that drives the same code path (Lane C.4).
//
// See docs/design/v0.18-clipboard-bridge.md.
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/clipboard"
)

// hostClipboardCmd returns the `canopy host clipboard <name>` cobra
// subcommand. Registered alongside the existing `canopy host install`
// verb by hostCmd().
//
// --reinstall is currently a no-op flag (the install is already
// idempotent); reserved for future "force re-detect UID + re-push"
// semantics if the simple idempotency stops being good enough.
func hostClipboardCmd() *cobra.Command {
	var reinstall bool
	c := &cobra.Command{
		Use:   "clipboard <name>",
		Short: "Set up the v0.18 clipboard bridge on a registered remote host",
		Long: "Configures a registered host so the laptop's clipboard is\n" +
			"available inside any remote canopy workspace on it. Four steps:\n\n" +
			"  1. SSH `id -u` on the remote to detect its UID (baked into\n" +
			"     the SSH RemoteForward socket paths).\n" +
			"  2. Push canopy's wl-paste and wl-copy wrappers into\n" +
			"     ~/.local/bin on the remote.\n" +
			"  3. Write ~/.ssh/config.d/canopy/<name>.conf locally with\n" +
			"     three RemoteForward directives for the Unix sockets the\n" +
			"     local daemon listens on.\n" +
			"  4. Verify the wrapper round-trips text/plain over SSH.\n\n" +
			"Idempotent — re-run safely. Prerequisites:\n" +
			"  - `canopy install clipboard-bridge` (one-time per laptop)\n" +
			"  - `socat` on the remote (Phase 1 doesn't auto-install yet)",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := loadHostRegistry()
			if err != nil {
				return err
			}
			h, err := reg.Resolve(args[0])
			if err != nil {
				return err
			}
			if h.SSHTarget == "" {
				return fmt.Errorf("canopy host clipboard %s: host has no ssh_target — re-register with `canopy host add %s <user@host>`", h.Name, h.Name)
			}

			installer, err := clipboard.NewHostInstaller(canopyVersionInfo)
			if err != nil {
				return err
			}
			return installer.InstallOnHost(cmd.Context(), h.Name, h.SSHTarget, cmd.OutOrStdout())
		},
	}
	c.Flags().BoolVar(&reinstall, "reinstall", false,
		"force re-detect of remote UID and re-push wrappers (currently a no-op — install is idempotent)")
	return c
}
