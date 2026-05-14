// config.go — `canopy config` subcommand for user-level preferences.
//
// Mirrors the shape of `git config`: small set of well-known keys, each
// settable, gettable, listable, and unsettable. Persists to
// ~/.canopy/config.json via internal/config.UserStore (flock-protected).
//
// Precedence for effective values (highest wins):
//
//	1. $CANOPY_SOURCE_ROOT (env)
//	2. ~/.canopy/config.json (config)
//	3. built-in default
//
// `canopy config get` and `list` show the source annotation so users know
// where the value came from and how to override it.
//
// Known keys (extend by adding to knownConfigKeys + handling in the
// set/get/unset switches):
//
//	source-root  Directory under which `canopy init <git-url>` clones.
//	             Default: ~/.canopy/sources.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/avinashjoshi/canopy/internal/config"
)

// configKey is the user-facing name on the CLI (e.g. "source-root").
// Matches the JSON tag in UserConfig so the on-disk file and the CLI
// surface use the same vocabulary.
type configKey string

const (
	keySourceRoot configKey = "source-root"
)

// knownConfigKeys is the closed set of keys `canopy config` accepts.
// Adding a key: append here, extend setValue/getValue/unsetValue.
var knownConfigKeys = []configKey{keySourceRoot}

// configCmd returns the parent `canopy config` subcommand.
//
// No standalone behavior — the parent prints help. Real work happens in
// the four subcommands. Same shape as `canopy host` and `canopy project`.
func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage user-level canopy settings (~/.canopy/config.json)",
		Long: "Persist per-user preferences across projects, like the directory where\n" +
			"`canopy init <git-url>` clones repos by default (source-root).\n\n" +
			"Precedence (highest wins): env var > config file > built-in default.\n\n" +
			"Known keys:\n" +
			"  source-root  Directory canopy clones into (default: ~/.canopy/sources).\n" +
			"               Override via env: CANOPY_SOURCE_ROOT=/path canopy init <url>",
	}
	cmd.AddCommand(configSetCmd())
	cmd.AddCommand(configGetCmd())
	cmd.AddCommand(configListCmd())
	cmd.AddCommand(configUnsetCmd())
	return cmd
}

// configSetCmd handles `canopy config set <key> <value>`.
//
// Validates the key is known and the value is non-empty, then takes the
// flock on config.json and writes. Atomic via tmpfile + rename inside
// the lock window.
//
// Does NOT create the value on disk (lazy mkdir): setting source-root
// to a dir that doesn't exist is fine — canopy will create it just
// before the first clone that needs it. This matches `git config` UX
// where you can set arbitrary paths and only the consumer enforces
// existence.
func configSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a config value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := configKey(args[0]), args[1]
			if err := validateKey(key); err != nil {
				return err
			}
			if value == "" {
				return fmt.Errorf("canopy config set: empty value for %q. To clear, use `canopy config unset %s`", key, key)
			}
			store, err := openUserStore()
			if err != nil {
				return err
			}
			if err := store.WithLock(func(c *config.UserConfig) error {
				return setValue(c, key, value)
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Set %s = %s\n", key, value)
			return nil
		},
	}
}

// configGetCmd handles `canopy config get <key>`.
//
// Prints the effective value with its source annotation:
//
//	$ canopy config get source-root
//	/home/avi/Work  (config)
//
//	$ CANOPY_SOURCE_ROOT=/tmp/srcs canopy config get source-root
//	/tmp/srcs  (env)
//
//	$ canopy config get source-root  # nothing set
//	/home/avi/.canopy/sources  (default)
//
// The source token is whitespace-delimited so scripts can do:
//
//	canopy config get source-root | awk '{print $1}'  # value only
//	canopy config get source-root | grep -q '(env)'    # source check
func configGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print the effective value of a config key, with its source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := configKey(args[0])
			if err := validateKey(key); err != nil {
				return err
			}
			value, source, err := getValue(key)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s  (%s)\n", value, source)
			return nil
		},
	}
}

// configListCmd handles `canopy config list`. Prints every known key
// alongside its effective value and source in a tab-aligned table so
// users can see at a glance which keys are set vs default.
func configListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all known config keys with their effective values and sources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE")
			// Sorted for stable scripting output, even though there's
			// only one key today.
			keys := append([]configKey{}, knownConfigKeys...)
			sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
			for _, k := range keys {
				value, source, err := getValue(k)
				if err != nil {
					return err
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", k, value, source)
			}
			return tw.Flush()
		},
	}
}

// configUnsetCmd handles `canopy config unset <key>`. Clears the key
// from config.json. After unset, `get` returns the env (if set) or the
// default. A no-op unset (key wasn't set) succeeds silently — `unset`
// describes intent, not state.
func configUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <key>",
		Short: "Remove a config key (falls back to env var or default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := configKey(args[0])
			if err := validateKey(key); err != nil {
				return err
			}
			store, err := openUserStore()
			if err != nil {
				return err
			}
			if err := store.WithLock(func(c *config.UserConfig) error {
				unsetValue(c, key)
				return nil
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Unset %s\n", key)
			return nil
		},
	}
}

// validateKey returns a user-friendly error if key isn't one of the
// known keys. Listing the valid keys in the error is what makes typo
// recovery painless — no need to dig out --help.
func validateKey(key configKey) error {
	for _, k := range knownConfigKeys {
		if k == key {
			return nil
		}
	}
	names := make([]string, len(knownConfigKeys))
	for i, k := range knownConfigKeys {
		names[i] = string(k)
	}
	return fmt.Errorf("canopy config: unknown key %q. Known keys: %v", key, names)
}

// setValue applies a set on the in-flock UserConfig. The switch is the
// single place that knows which struct field each key maps to.
func setValue(c *config.UserConfig, key configKey, value string) error {
	switch key {
	case keySourceRoot:
		c.SourceRoot = value
		return nil
	}
	// Unreachable if validateKey ran first.
	return fmt.Errorf("canopy config set: unhandled key %q (internal error)", key)
}

// unsetValue clears the key from the in-flock UserConfig.
func unsetValue(c *config.UserConfig, key configKey) {
	switch key {
	case keySourceRoot:
		c.SourceRoot = ""
	}
}

// getValue resolves the effective value + source for a key. Reads
// outside the lock (a snapshot is fine — see UserStore.Load doc).
//
// The canopyHome arg is implicit: source-root's default lives under
// ~/.canopy/sources, so we need the home dir to assemble the default.
// Other future keys may not need a home — they'd just ignore it.
func getValue(key configKey) (value string, source string, err error) {
	switch key {
	case keySourceRoot:
		home, err := canopyHomeDir()
		if err != nil {
			return "", "", err
		}
		store, err := config.NewUserStore(home)
		if err != nil {
			return "", "", err
		}
		c, err := store.Load()
		if err != nil {
			return "", "", err
		}
		v, src := config.ResolveSourceRoot(c, home)
		return v, string(src), nil
	}
	return "", "", fmt.Errorf("canopy config get: unhandled key %q (internal error)", key)
}

// openUserStore returns a UserStore rooted at ~/.canopy. Caller-local
// helper (mirrors init.go's openStateForInit) so each subcommand reads
// the same way. Creates the ~/.canopy dir if missing so a fresh user's
// first `canopy config set` succeeds without a separate setup step.
func openUserStore() (*config.UserStore, error) {
	home, err := canopyHomeDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("canopy config: create %s: %w", home, err)
	}
	return config.NewUserStore(home)
}

// canopyHomeDir returns the absolute path to ~/.canopy. Sibling helpers
// in cmd/canopy use os.UserHomeDir directly — extracted here so the
// config code path has a single named entry that future tests can swap.
func canopyHomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("canopy config: home dir: %w", err)
	}
	return filepath.Join(home, ".canopy"), nil
}

// Sanity-checks: keep io and errors imported for tests/future expansion;
// silences "imported and not used" if a refactor drops a call site.
var _ io.Writer = os.Stdout
var _ = errors.New
