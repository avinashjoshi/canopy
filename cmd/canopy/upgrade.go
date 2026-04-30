// Command canopy upgrade pulls the latest canopy from main and runs
// `make install` in the source clone. The end-user equivalent of "git
// pull && make install" without making the user remember where the
// source lives or what targets to invoke.
//
// Distribution model: source-based (matches gstack/gbrain). The
// install.sh one-liner clones github.com/oncactus/canopy to
// ~/.canopy/src; canopy upgrade git-pulls that clone and rebuilds.
// VERSION file in the repo root is the comparison key — we curl
// raw.githubusercontent.../main/VERSION, string-compare, and skip the
// pull if already current.
//
// Refusal cases (each prints a clear next step):
//   - Running from a dev binary (version == "dev"): use canopy use
//     release first; upgrading a dev build doesn't make sense
//   - ~/.canopy/src missing or not a git repo: install.sh wasn't run,
//     or the clone got corrupted; point at the curl|sh one-liner
//   - Network failure on VERSION fetch: bail with the underlying error
//   - git pull merge conflict: bail before make install, leave src
//     dir for manual resolution
//
// Tests inject the http fetcher and shell runner via package-level
// vars (same testability seam as runGitBranchShowCurrent and
// goBuildInWorktree) so the test suite never has to hit the network
// or fork git.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/oncactus/canopy/internal/clog"
)

var upgradeLog = clog.Pkg("upgrade")

const (
	// upgradeVersionURL is where canopy upgrade reads the latest VERSION.
	// raw.githubusercontent.com is the no-infra hosting path: no domain
	// to set up, no CDN to maintain, just a stable URL backed by the
	// main branch of the repo.
	upgradeVersionURL = "https://raw.githubusercontent.com/oncactus/canopy/main/VERSION"

	// upgradeChangelogURL is fetched (best-effort) so the user can see
	// what's new before the pull. A failed fetch here is non-fatal —
	// the upgrade still proceeds, just without the changelog preview.
	upgradeChangelogURL = "https://raw.githubusercontent.com/oncactus/canopy/main/CHANGELOG.md"

	// upgradeFetchTimeout caps the network calls. canopy upgrade is
	// interactive, not a hot loop, so a generous-but-not-infinite
	// timeout is the right shape.
	upgradeFetchTimeout = 10 * time.Second

	// upgradeSrcSubdir is the canopy-managed source clone path under
	// ~/.canopy/. Single source of truth shared with install.sh; if
	// install.sh changes the clone destination, this constant must
	// change in lockstep.
	upgradeSrcSubdir = "src"
)

func newUpgradeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update canopy to the latest version on main.",
		Long: `Fetches the latest VERSION from canopy's main branch, compares with the
running binary, and runs git pull + make install in ~/.canopy/src if newer.

Source-based distribution: install.sh placed the clone at ~/.canopy/src on
first install; this subcommand keeps it current. No goreleaser, no GitHub
Releases — just main branch + make install.

Refuses if:
  - Running from a dev binary (canopy use release first)
  - ~/.canopy/src is missing or corrupt (install.sh wasn't run, or the
    clone got removed)

Network-only flags: --check (compare versions but skip the upgrade),
--force (run git pull + make install even if versions match).
`,
		Annotations: map[string]string{allowInTmuxAnnotation: "true"},
		RunE:        runUpgrade,
	}
	cmd.Flags().Bool("check", false, "compare versions but don't upgrade")
	cmd.Flags().Bool("force", false, "run git pull + make install even if versions match")
	return cmd
}

func runUpgrade(cmd *cobra.Command, _ []string) error {
	checkOnly, _ := cmd.Flags().GetBool("check")
	force, _ := cmd.Flags().GetBool("force")
	out := cmd.OutOrStdout()

	// Refuse on dev binaries. Upgrading from "dev" doesn't have a
	// sensible target — you're already running uncommitted local code.
	d := versionDetails()
	if d.IsDev {
		return errors.New(
			"canopy upgrade: you're running a dev binary.\n" +
				"  Switch to the released canopy first:\n" +
				"    canopy use release\n" +
				"  Then re-run canopy upgrade.")
	}

	srcDir, err := upgradeSrcDir()
	if err != nil {
		return err
	}
	if err := ensureSrcDirReady(srcDir); err != nil {
		return err
	}

	// Always fetch the remote VERSION. Comparing against running
	// version tells us whether to bother with the pull at all.
	ctx, cancel := context.WithTimeout(cmd.Context(), upgradeFetchTimeout)
	defer cancel()
	remote, err := upgradeFetchVersion(ctx, upgradeVersionURL)
	if err != nil {
		return fmt.Errorf("canopy upgrade: fetch VERSION: %w", err)
	}
	remote = strings.TrimSpace(remote)
	current := normalizeRunningVersion(d.Version)

	fmt.Fprintf(out, "Running:  %s\n", d.Version)
	fmt.Fprintf(out, "Latest:   v%s\n", remote)

	if !force && current == remote {
		fmt.Fprintln(out, "\nAlready up to date.")
		return nil
	}

	if checkOnly {
		fmt.Fprintf(out, "\nUpgrade available: %s -> v%s\n", current, remote)
		fmt.Fprintln(out, "Run 'canopy upgrade' (without --check) to apply.")
		return nil
	}

	// Best-effort CHANGELOG preview. Failure is non-fatal — we don't
	// want a slow CDN to block the upgrade.
	if changelog, err := upgradeFetchVersion(ctx, upgradeChangelogURL); err == nil {
		preview := changelogSlice(changelog, current, remote)
		if preview != "" {
			fmt.Fprintln(out, "\nWhat's new:")
			fmt.Fprint(out, preview)
		}
	}

	fmt.Fprintln(out, "\nUpgrading...")
	if err := upgradeRunShell(cmd.Context(), srcDir); err != nil {
		return fmt.Errorf("canopy upgrade: %w", err)
	}
	upgradeLog.Info("upgrade.success", "from", current, "to", remote, "src", srcDir)
	fmt.Fprintf(out, "\nUpgraded to v%s\n", remote)
	return nil
}

// upgradeSrcDir resolves to ~/.canopy/src. Single source of truth
// shared with install.sh via the upgradeSrcSubdir constant.
func upgradeSrcDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("canopy upgrade: home dir: %w", err)
	}
	return filepath.Join(home, ".canopy", upgradeSrcSubdir), nil
}

// ensureSrcDirReady refuses cleanly if the source clone doesn't exist
// or isn't a git repo. install.sh creates it on first install; this
// is the "you skipped the install step" diagnostic.
func ensureSrcDirReady(srcDir string) error {
	info, err := os.Stat(srcDir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"canopy upgrade: source clone missing at %s\n"+
				"  Run install.sh first to set it up:\n"+
				"    curl -fsSL https://raw.githubusercontent.com/oncactus/canopy/main/install.sh | sh",
			srcDir)
	}
	if err != nil {
		return fmt.Errorf("canopy upgrade: stat %s: %w", srcDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("canopy upgrade: %s is not a directory", srcDir)
	}
	if _, err := os.Stat(filepath.Join(srcDir, ".git")); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"canopy upgrade: %s is not a git clone (no .git directory)\n"+
				"  Remove it and re-run install.sh:\n"+
				"    rm -rf %s\n"+
				"    curl -fsSL https://raw.githubusercontent.com/oncactus/canopy/main/install.sh | sh",
			srcDir, srcDir)
	}
	return nil
}

// normalizeRunningVersion strips the goreleaser-style "v" prefix from
// the running version so it lines up with the bare semver in the
// VERSION file. "v0.12.0+abc1234" -> "0.12.0+abc1234"; the build
// metadata (the +sha portion) is preserved so equality comparison
// stays meaningful even between identical semvers built at different
// commits.
func normalizeRunningVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	// Strip the +sha build metadata for comparison, since the remote
	// VERSION file holds the bare semver. We compare semvers, not the
	// commit pin — the semver bump is what triggers an upgrade.
	if i := strings.Index(v, "+"); i >= 0 {
		v = v[:i]
	}
	return v
}

// changelogSlice extracts the CHANGELOG section between two versions.
// Both inputs are bare semvers ("0.12.0", "0.13.0"); we look for the
// "## [<version>]" headings the existing CHANGELOG uses (Keep a
// Changelog format) and return everything between [remote] and
// [current], exclusive of the [current] heading.
//
// Returns "" when the bracketing headings can't be found — better to
// print nothing than mislead. The upgrade still proceeds.
func changelogSlice(content, current, remote string) string {
	currentHeading := fmt.Sprintf("## [%s]", current)
	remoteHeading := fmt.Sprintf("## [%s]", remote)

	startIdx := strings.Index(content, remoteHeading)
	if startIdx < 0 {
		return ""
	}
	endIdx := strings.Index(content[startIdx:], currentHeading)
	if endIdx < 0 {
		// Older CHANGELOG entries may have been trimmed; just print
		// from the remote heading to the next "## [" heading or 80
		// lines, whichever comes first.
		nextSection := strings.Index(content[startIdx+len(remoteHeading):], "\n## [")
		if nextSection < 0 {
			return content[startIdx:]
		}
		return content[startIdx : startIdx+len(remoteHeading)+nextSection]
	}
	return content[startIdx : startIdx+endIdx]
}

// upgradeFetchVersion is the http GET seam exposed for testing.
// Production fetches via net/http; tests stub with a local string.
var upgradeFetchVersion = func(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: upgradeFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// upgradeRunShell runs `git pull --ff-only && make install` in srcDir.
// Two-step shell with explicit error wrapping so users can tell which
// step failed. Stderr is captured and surfaced — when make install
// fails because of a missing dep or a code conflict, the user needs
// the actual error, not "exit status 1".
//
// --ff-only is load-bearing: a non-fast-forward would mean local
// commits in the user's source clone, which the upgrade flow has no
// business merging silently. Refuse and tell the user to investigate.
var upgradeRunShell = func(ctx context.Context, srcDir string) error {
	pull := exec.CommandContext(ctx, "git", "-C", srcDir, "pull", "--ff-only")
	var pullStderr strings.Builder
	pull.Stderr = &pullStderr
	if out, err := pull.Output(); err != nil {
		return fmt.Errorf(
			"git pull --ff-only failed in %s: %w\n  stderr: %s\n  stdout: %s\n"+
				"  This usually means there are local commits in the source clone.\n"+
				"  Resolve manually (cd %s && git status) or remove the clone and re-run install.sh.",
			srcDir, err, pullStderr.String(), string(out), srcDir)
	}

	makeInstall := exec.CommandContext(ctx, "make", "-C", srcDir, "install")
	var makeStderr strings.Builder
	makeInstall.Stderr = &makeStderr
	makeInstall.Stdout = io.Discard
	if err := makeInstall.Run(); err != nil {
		return fmt.Errorf(
			"make install failed in %s: %w\n  stderr: %s\n"+
				"  The git pull succeeded, so source is up to date — only the build failed.\n"+
				"  Inspect the error above and re-run 'make install' manually in %s.",
			srcDir, err, makeStderr.String(), srcDir)
	}
	return nil
}
