// Command canopy upgrade pulls the latest canopy from main and runs
// `make install` in the source clone. The end-user equivalent of "git
// pull && make install" without making the user remember where the
// source lives or what targets to invoke.
//
// Distribution model: source-based (matches gstack/gbrain). The
// install.sh one-liner clones github.com/avinashjoshi/canopy to
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
	"bufio"
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

	"github.com/avinashjoshi/canopy/internal/clog"
)

var upgradeLog = clog.Pkg("upgrade")

const (
	// upgradeVersionURL is where canopy upgrade reads the latest VERSION.
	// raw.githubusercontent.com is the no-infra hosting path: no domain
	// to set up, no CDN to maintain, just a stable URL backed by the
	// main branch of the repo.
	upgradeVersionURL = "https://raw.githubusercontent.com/avinashjoshi/canopy/main/VERSION"

	// upgradeChangelogURL is fetched (best-effort) so the user can see
	// what's new before the pull. A failed fetch here is non-fatal —
	// the upgrade still proceeds, just without the changelog preview.
	upgradeChangelogURL = "https://raw.githubusercontent.com/avinashjoshi/canopy/main/CHANGELOG.md"

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
	// --dismiss writes dismissed_version = latest_version into the
	// auto-check cache so the top-bar pill / canopy ls hint stops
	// nagging. Per-version dismissal: a new release un-dismisses
	// automatically because the field changes underneath. Refuses on
	// DEV (no auto-check fires on DEV) and refuses if no cached
	// latest exists (nothing to dismiss yet).
	cmd.Flags().Bool("dismiss", false, "stop showing 'upgrade available' until the next release")
	// --status prints the auto-check cache contents so users can
	// debug "why isn't the pill showing?" / "did dismiss take?" /
	// "when does the cache refresh?". Pure read, no network, no
	// shell. Works on DEV (DEV doesn't auto-check, but inspecting
	// any leftover cache file is still useful diagnostically).
	cmd.Flags().Bool("status", false, "print the auto-check cache contents (debugging aid)")
	// --yes / -y skips the confirmation prompt for non-interactive
	// invocations. Stdin auto-detection also skips the prompt when
	// stdin isn't a tty (CI, pipes), so this flag is explicit-only.
	// --force also implies --yes — forcing the upgrade IS the
	// confirmation in that case.
	cmd.Flags().BoolP("yes", "y", false, "skip the confirmation prompt (implied by --force)")
	return cmd
}

func runUpgrade(cmd *cobra.Command, _ []string) error {
	checkOnly, _ := cmd.Flags().GetBool("check")
	force, _ := cmd.Flags().GetBool("force")
	dismiss, _ := cmd.Flags().GetBool("dismiss")
	status, _ := cmd.Flags().GetBool("status")
	yes, _ := cmd.Flags().GetBool("yes")
	out := cmd.OutOrStdout()

	// --status is a pure cache read — no network, no shell, no DEV
	// gate. Handle it before any of the upgrade-flavored guards so
	// users can inspect the cache regardless of binary state.
	if status {
		return printUpgradeStatus(out)
	}

	// Refuse on dev binaries. Upgrading from "dev" doesn't have a
	// sensible target — you're already running uncommitted local code.
	// The dismiss path also refuses: dismissing the auto-check pill
	// for a DEV binary is meaningless because DEV binaries never
	// trigger auto-check in the first place.
	d := versionDetails()
	if d.IsDev {
		return errors.New(
			"canopy upgrade: you're running a dev binary.\n" +
				"  Switch to the released canopy first:\n" +
				"    canopy use release\n" +
				"  Then re-run canopy upgrade.")
	}

	// --dismiss is a pure cache write; no network, no shell, no src
	// dir requirement. Handle it before the other guards so users
	// can dismiss without having a working ~/.canopy/src.
	if dismiss {
		latest, err := dismissUpgradeCheck()
		if err != nil {
			return fmt.Errorf("canopy upgrade --dismiss: %w", err)
		}
		fmt.Fprintf(out, "Dismissed v%s. Pill will reappear when a newer version ships.\n", latest)
		return nil
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
	//
	// Fetch path: HTTP first (fast, no fork), git fallback (works for
	// private repos where raw.githubusercontent.com 404s anonymous
	// requests). The src clone always has working git auth — that's
	// how install.sh succeeded — so the git fallback is dependable.
	ctx, cancel := context.WithTimeout(cmd.Context(), upgradeFetchTimeout)
	defer cancel()
	remote, err := fetchRemoteFile(ctx, srcDir, upgradeVersionURL, "VERSION")
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
		// Opportunistically refresh the cache from the value we
		// already fetched. Avoids a double network call. Preserves
		// any existing dismissed_version so --check doesn't undo a
		// previous dismiss.
		if perr := writeCachedRemote(remote); perr != nil {
			upgradeLog.Warn("upgrade.check_cache_write_failed", "err", perr)
		}
		fmt.Fprintf(out, "\nUpgrade available: %s -> v%s\n", current, remote)
		fmt.Fprintln(out, "Run 'canopy upgrade' (without --check) to apply.")
		return nil
	}

	// Best-effort CHANGELOG preview. Failure is non-fatal — we don't
	// want a slow CDN, a 404 (private repo), or a stalled git fetch
	// to block the upgrade.
	if changelog, err := fetchRemoteFile(ctx, srcDir, upgradeChangelogURL, "CHANGELOG.md"); err == nil {
		preview := changelogSlice(changelog, current, remote)
		if preview != "" {
			fmt.Fprintln(out, "\nWhat's new:")
			fmt.Fprint(out, preview)
		}
	}

	// Confirmation gate. The TUI flow (U key) has an explicit "Enter
	// to upgrade, Esc to cancel" preview state; the CLI used to just
	// barrel through and run shell after printing the changelog.
	// That made it impossible to actually READ the changelog before
	// the upgrade started.
	//
	// Skip the prompt when:
	//   - --yes / -y was passed (explicit non-interactive intent)
	//   - --force was passed (force IS the confirmation)
	//   - stdin isn't a tty (CI, scripts, pipes — a prompt would
	//     either hang on Read or read piped garbage)
	if !yes && !force && upgradeIsTerminal(os.Stdin) {
		if !upgradePromptYesNo(cmd.InOrStdin(), out) {
			fmt.Fprintln(out, "Cancelled. Run `canopy upgrade --yes` to skip the prompt next time.")
			return nil
		}
	}

	fmt.Fprintln(out, "\nUpgrading...")
	if err := upgradeRunShell(cmd.Context(), srcDir); err != nil {
		return fmt.Errorf("canopy upgrade: %w", err)
	}
	upgradeLog.Info("upgrade.success", "from", current, "to", remote, "src", srcDir)
	// Rewrite the auto-check cache so the pill / ls hint disappears
	// immediately instead of waiting for the next 6h refresh. Failure
	// here is non-fatal: the upgrade itself succeeded; a stale pill
	// for one cycle is acceptable. Log so it's debuggable if it
	// becomes a pattern.
	if err := clearUpgradeCheck(remote); err != nil {
		upgradeLog.Warn("upgrade.cache_clear_failed", "err", err)
	}
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
				"    curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh",
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
				"    curl -fsSL https://raw.githubusercontent.com/avinashjoshi/canopy/main/install.sh | sh",
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

// fetchRemoteFile reads a file from the canopy repo's main branch.
// Tries HTTP first (raw.githubusercontent.com, fast, no fork). On any
// failure — including 404 from private-repo anonymous access — falls
// back to git: `git fetch origin main && git show origin/main:<path>`
// in the local src clone. The clone has working auth (otherwise
// install.sh wouldn't have succeeded), so git always works regardless
// of repo visibility.
//
// gitPath is the path of the file relative to the repo root (e.g.
// "VERSION", "CHANGELOG.md"). url is the raw.githubusercontent.com
// URL for the same file. The HTTP path is exposed so when the repo
// goes public, the upgrade check is one HTTP GET instead of a git
// fetch — strictly faster.
//
// Both paths are tested via the upgradeFetchVersion + upgradeGitFetchFile
// stubs (each independently injectable so tests can simulate "HTTP
// works", "HTTP fails, git works", "both fail").
func fetchRemoteFile(ctx context.Context, srcDir, url, gitPath string) (string, error) {
	// HTTP first.
	body, httpErr := upgradeFetchVersion(ctx, url)
	if httpErr == nil {
		return body, nil
	}
	// HTTP failed (404, network, timeout). Fall back to git.
	body, gitErr := upgradeGitFetchFile(ctx, srcDir, gitPath)
	if gitErr == nil {
		return body, nil
	}
	// Both failed — surface the original HTTP error since that's
	// the canonical first attempt; mention git fallback context too
	// so the user knows we tried.
	return "", fmt.Errorf("HTTP: %w; git fallback also failed: %v", httpErr, gitErr)
}

// upgradeGitFetchFile reads <gitPath> from origin/main of srcDir.
// Runs `git fetch --quiet origin main` first so the read sees the
// latest remote state. Both commands respect the context's deadline.
//
// Exposed as a package-level var so tests can stub the git path
// without forking real git.
var upgradeGitFetchFile = func(ctx context.Context, srcDir, gitPath string) (string, error) {
	fetch := exec.CommandContext(ctx, "git", "-C", srcDir, "fetch", "--quiet", "origin", "main")
	if err := fetch.Run(); err != nil {
		return "", fmt.Errorf("git fetch origin main in %s: %w", srcDir, err)
	}
	show := exec.CommandContext(ctx, "git", "-C", srcDir, "show", "origin/main:"+gitPath)
	out, err := show.Output()
	if err != nil {
		return "", fmt.Errorf("git show origin/main:%s: %w", gitPath, err)
	}
	return string(out), nil
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

// upgradeIsTerminal reports whether the given file is connected to
// an interactive terminal (vs a pipe, file, or other non-tty source).
// Used to gate the confirmation prompt: when stdin is piped or
// redirected, we auto-confirm rather than blocking on a Read that
// would either hang or consume piped data the user didn't mean as
// a yes/no answer.
//
// Stdlib-only: stat the fd and check ModeCharDevice. Avoids pulling
// golang.org/x/term for one syscall. Exposed as a package-level var
// so tests can stub it without setting up real ttys.
var upgradeIsTerminal = func(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// upgradePromptYesNo prints "Apply this upgrade? [Y/n]:" and reads
// one line from in. Returns true on yes (Y, y, yes, empty/Enter —
// because the [Y/n] capitalization signals Y as the default), false
// on no (N, n, no). Anything else re-prompts up to 3 times before
// giving up and returning false.
//
// Exposed as a package-level var so tests can stub the prompt with
// a canned input source instead of hooking up real stdin.
var upgradePromptYesNo = func(in io.Reader, out io.Writer) bool {
	reader := bufio.NewReader(in)
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprint(out, "\nApply this upgrade? [Y/n]: ")
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			// EOF or read error with no input — treat as cancel
			// rather than guessing. Keeps the contract honest.
			return false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return true
		case "n", "no":
			return false
		default:
			fmt.Fprintln(out, "Please answer y or n.")
		}
	}
	return false
}

// upgradeRunShellStreaming is the io.Writer variant of upgradeRunShell
// for the in-TUI flow. Same two-step shell (`git pull --ff-only` then
// `make install`) with the same --ff-only invariant, but stdout and
// stderr both pipe into the caller-supplied writer so the TUI can
// surface live progress via safeBuffer + progressTick.
//
// The error message on failure includes the same hint structure as
// upgradeRunShell — by the time we've streamed the output, the
// returned error just needs to identify which step failed; the
// detailed stderr is already in the user's view via the writer.
//
// Exposed as a package-level var so tests can stub the streaming
// path independently of the CLI's upgradeRunShell.
var upgradeRunShellStreaming = func(ctx context.Context, srcDir string, w io.Writer) error {
	// Tee output into the caller's writer AND into a local buffer so the
	// post-failure error message can surface a permission-denied hint
	// without making the user scroll back through the streamed output.
	//
	// CRITICAL: build ONE io.Writer and assign it to both Stdout and
	// Stderr. exec.Cmd's stdout/stderr goroutines are only serialized
	// when `Stdout == Stderr` (interface equality); two distinct
	// `io.MultiWriter(...)` calls produce different *multiWriter values,
	// which would spawn two goroutines racing on the same
	// strings.Builder. Single value + single goroutine = no race. The
	// underlying Builder is unsafe for concurrent use; the safeBuffer
	// the caller passes (`w`) IS safe, but the path through this buf
	// must stay single-threaded by construction.
	pullBuf := &strings.Builder{}
	pullTee := io.MultiWriter(w, pullBuf)
	pull := exec.CommandContext(ctx, "git", "-C", srcDir, "pull", "--ff-only")
	pull.Stdout = pullTee
	pull.Stderr = pullTee
	if err := pull.Run(); err != nil {
		return wrapPullErr(srcDir, err, pullBuf.String(), "", false)
	}

	makeBuf := &strings.Builder{}
	makeTee := io.MultiWriter(w, makeBuf)
	makeInstall := exec.CommandContext(ctx, "make", "-C", srcDir, "install")
	makeInstall.Stdout = makeTee
	makeInstall.Stderr = makeTee
	if err := makeInstall.Run(); err != nil {
		return wrapMakeErr(srcDir, err, makeBuf.String(), false)
	}
	return nil
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
//
// Permission-denied detection: both steps can fail on a host whose
// ~/.canopy/src or ~/.local/bin holds files owned by another user
// (the classic case is a prior install that ran as root via sudo).
// Surfacing the generic "local commits" / "build failed" hint there
// is misleading — we sniff for "Permission denied" in stderr and
// emit a targeted recovery hint instead.
var upgradeRunShell = func(ctx context.Context, srcDir string) error {
	pull := exec.CommandContext(ctx, "git", "-C", srcDir, "pull", "--ff-only")
	var pullStderr strings.Builder
	pull.Stderr = &pullStderr
	if out, err := pull.Output(); err != nil {
		return wrapPullErr(srcDir, err, pullStderr.String(), string(out), true)
	}

	makeInstall := exec.CommandContext(ctx, "make", "-C", srcDir, "install")
	var makeStderr strings.Builder
	makeInstall.Stderr = &makeStderr
	makeInstall.Stdout = io.Discard
	if err := makeInstall.Run(); err != nil {
		return wrapMakeErr(srcDir, err, makeStderr.String(), true)
	}
	return nil
}

// wrapPullErr classifies a `git pull --ff-only` failure into a
// user-facing error. The permission-denied path emits a chown-recovery
// hint; the fallback preserves the legacy "local commits" wording.
//
// includeOutput=true folds the captured stderr (and stdout for the
// generic branch) into the message — that's the CLI flow, where the
// user hasn't already seen the streamed bytes. =false omits them — the
// streaming flow already wrote both straight to the user's view and
// re-printing would just clutter the error.
//
// Pure function (no I/O, no goroutines, no globals) so unit tests can
// pin both branches against both verbosities directly. The package's
// existing convention is to expose I/O-bearing functions like
// upgradeRunShell as `var` for stubbing; classification is pulled out
// here precisely because it doesn't need that escape hatch.
func wrapPullErr(srcDir string, runErr error, stderr, stdout string, includeOutput bool) error {
	if isPermissionDeniedStderr(stderr) {
		if includeOutput {
			return fmt.Errorf(
				"git pull --ff-only failed in %s: %w\n  stderr: %s\n"+
					"  Looks like a file in %s isn't writable by the current user.\n"+
					"  This usually means a previous install ran as root via sudo.\n"+
					"  Recover with:\n"+
					"    sudo chown -R $(whoami) %s\n"+
					"  Then re-run canopy upgrade.",
				srcDir, runErr, stderr, srcDir, srcDir)
		}
		return fmt.Errorf(
			"git pull --ff-only failed in %s: %w\n  "+
				"Hint: a file in %s isn't writable by the current user; "+
				"fix with `sudo chown -R $(whoami) %s` and re-run.",
			srcDir, runErr, srcDir, srcDir)
	}
	if includeOutput {
		return fmt.Errorf(
			"git pull --ff-only failed in %s: %w\n  stderr: %s\n  stdout: %s\n"+
				"  This usually means there are local commits in the source clone.\n"+
				"  Resolve manually (cd %s && git status) or remove the clone and re-run install.sh.",
			srcDir, runErr, stderr, stdout, srcDir)
	}
	return fmt.Errorf("git pull --ff-only failed in %s: %w", srcDir, runErr)
}

// wrapMakeErr is wrapPullErr's sibling for `make install` failures.
// Same shape: permission-denied steers the hint toward chown,
// fallback keeps the existing "build failed" wording.
//
// The hint references "the install target shown in stderr above"
// rather than hardcoding ~/.local/bin — the Makefile's $(BIN_DIR)
// honors $PREFIX, so a user who set PREFIX=/opt/canopy would get
// misleading recovery instructions if we baked the path in.
func wrapMakeErr(srcDir string, runErr error, stderr string, includeOutput bool) error {
	if isPermissionDeniedStderr(stderr) {
		if includeOutput {
			return fmt.Errorf(
				"make install failed in %s: %w\n  stderr: %s\n"+
					"  Looks like the install target isn't writable by the current user.\n"+
					"  This usually means a previous install ran as root via sudo,\n"+
					"  leaving binaries the current user can't replace.\n"+
					"  Recover by chowning the path shown in the stderr above, e.g.:\n"+
					"    sudo chown $(whoami) <path-from-stderr>\n"+
					"  Then re-run canopy upgrade.",
				srcDir, runErr, stderr)
		}
		return fmt.Errorf(
			"make install failed in %s: %w\n  "+
				"Hint: the install target isn't writable (prior install likely ran as root). "+
				"Chown the path shown in the streamed output above, then re-run.",
			srcDir, runErr)
	}
	if includeOutput {
		return fmt.Errorf(
			"make install failed in %s: %w\n  stderr: %s\n"+
				"  The git pull succeeded, so source is up to date — only the build failed.\n"+
				"  Inspect the error above and re-run 'make install' manually in %s.",
			srcDir, runErr, stderr, srcDir)
	}
	return fmt.Errorf("make install failed in %s: %w", srcDir, runErr)
}

// isPermissionDeniedStderr reports whether stderr from a child process
// indicates a *filesystem* permission failure (the kind a `chown`
// recovery can fix). Both git and make pass through the underlying
// open()/write() errno text, so the canonical strings ("Permission
// denied", "permission denied") match across distros. Lowercase the
// haystack before Contains so "PERMISSION DENIED" from oddball locales
// also matches.
//
// Bias: false NEGATIVES over false positives. The chown hint is
// destructive (sudo + recursive ownership change); steering a user
// toward it for an unrelated failure is far worse than missing the
// hint for a real filesystem permission failure. Two filters apply:
//
//  1. Explicit deny: SSH/network forms of "Permission denied" must
//     never trigger the hint. These include "(publickey)", "(password)",
//     "(keyboard-interactive)", "(none)", "please try again", and any
//     "ssh:" prefix (covers "ssh: connect to host …: Permission denied"
//     from firewall blocks).
//
//  2. Filesystem-signal requirement: alongside the phrase, stderr must
//     contain at least one filesystem-shaped token (an errno-attached
//     syscall name, "cannot"/"unable to" git wording, or a path
//     fragment). Without it, the cause is ambiguous and the generic
//     error message (which doesn't push chown) is the safer default.
func isPermissionDeniedStderr(stderr string) bool {
	s := strings.ToLower(stderr)
	if !strings.Contains(s, "permission denied") {
		return false
	}
	// Filter 1: explicit network/auth deny.
	denyMarkers := []string{
		"permission denied (publickey",
		"permission denied (password",
		"permission denied (keyboard-interactive",
		"permission denied (none",
		"permission denied, please try again",
		"ssh:",
	}
	for _, m := range denyMarkers {
		if strings.Contains(s, m) {
			return false
		}
	}
	// Filter 2: filesystem-signal requirement. At least one of these
	// must co-occur with "permission denied" for the hint to fire.
	fsMarkers := []string{
		"open ", "openat ", "mkdir ", "write ", "rename ", "unlink ",
		"cannot ", "unable to ",
		".git/", ".bin", ".canopy/", "/.local/bin", "/home/", "/root/", "/var/",
	}
	for _, m := range fsMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
