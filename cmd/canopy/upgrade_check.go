// Auto-check cache for `canopy upgrade`. Backs the top-bar pill, the
// canopy ls hint line, and the in-TUI U-key flow. Cache lives at
// ~/.canopy/upgrade-check.json so it sits alongside state.json without
// mixing schemas (state.json is the workspace registry; this is CLI
// process metadata with a different write cadence and blast radius).
//
// Three operations:
//   - readUpgradeCheck:  load cache, treat missing file as nil (not error)
//   - writeUpgradeCheck: atomic tempfile+rename, mirrors state.Save
//   - cachedRemoteVersion: cache-first synchronous read; if cache is
//     stale (>upgradeCheckTTL), refetch via fetchRemoteFile (HTTP +
//     git fallback already in upgrade.go) and rewrite the cache
//
// The TTL is 6h, deliberately shorter than the npm/cargo/gh consensus
// of 24h. Canopy ships intra-day during active development; 24h would
// mean other workspaces don't see a new release until the next day.
// The async refresh is non-blocking, so a shorter TTL is effectively
// free.
//
// Time source is exposed as upgradeCheckNow so tests can simulate
// fresh / stale / dismissed states without touching the wall clock.
// The HTTP and git fetch seams are reused from upgrade.go.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// upgradeCheckFile is the cache file name under ~/.canopy/. Changing
// this is a one-time migration: drop the old file, the new one writes
// fresh on next startup.
const upgradeCheckFile = "upgrade-check.json"

// upgradeCheckTTL bounds how stale a cached LatestVersion is allowed
// to be before cachedRemoteVersion considers it expired and triggers
// a refetch. Six hours is the dogfooding sweet spot: catches same-day
// releases by lunchtime, ~4 fetches/day worst case (well under any
// rate limit), and the network call is async so shorter has no UI
// cost.
const upgradeCheckTTL = 6 * time.Hour

// upgradeCheckNow is the time source. Tests stub it to simulate
// fresh/stale cache without sleeping or modifying file mtimes.
var upgradeCheckNow = time.Now

// upgradeCheck is the on-disk schema. Three fields, JSON-encoded with
// indentation for human readability (the cache file is rare-access and
// debuggability matters more than byte count). Forward-compatible:
// extra fields on disk are preserved by round-trip via json.RawMessage
// only if we add explicit handling later; for v0.13 we treat unknown
// fields as ignorable (json.Unmarshal default).
type upgradeCheck struct {
	// CheckedAt is when LatestVersion was last successfully fetched
	// from upstream. Drives the TTL gate. RFC3339 UTC on disk.
	CheckedAt time.Time `json:"checked_at"`

	// LatestVersion is the bare semver from VERSION on origin/main
	// at CheckedAt. No "v" prefix — matches the file format used by
	// `canopy upgrade`. Empty when the cache has never been
	// successfully populated.
	LatestVersion string `json:"latest_version"`

	// DismissedVersion is the bare semver the user said "stop
	// nagging me about" (via `canopy upgrade --dismiss` or the TUI
	// D key). The pill / ls hint is suppressed when this equals
	// LatestVersion. A new release un-suppresses automatically
	// because the field changes underneath.
	DismissedVersion string `json:"dismissed_version,omitempty"`
}

// fresh reports whether the cache is within the TTL window. A fresh
// cache hit means cachedRemoteVersion returns immediately with no
// network call.
func (u *upgradeCheck) fresh() bool {
	if u == nil || u.CheckedAt.IsZero() {
		return false
	}
	return upgradeCheckNow().Sub(u.CheckedAt) < upgradeCheckTTL
}

// dismissed reports whether the user has explicitly silenced the
// current LatestVersion. Suppresses the pill / ls hint.
func (u *upgradeCheck) dismissed() bool {
	if u == nil {
		return false
	}
	return u.DismissedVersion != "" && u.DismissedVersion == u.LatestVersion
}

// upgradeCheckPath resolves to ~/.canopy/upgrade-check.json. Single
// source of truth for the location; both readers and writers go
// through here.
func upgradeCheckPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("upgrade-check: home dir: %w", err)
	}
	return filepath.Join(home, ".canopy", upgradeCheckFile), nil
}

// readUpgradeCheck loads the cache from disk. A missing file is NOT
// an error — first-run users have no cache yet, and that's expected.
// Returns (nil, nil) in that case so callers can branch on "no
// cache" vs "cache exists" without parsing error strings.
//
// Malformed JSON returns an error. Callers may choose to log-and-
// continue (treating malformed as missing) — the cache is rebuilt on
// the next successful fetch either way.
func readUpgradeCheck(path string) (*upgradeCheck, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("upgrade-check: read %s: %w", path, err)
	}
	var u upgradeCheck
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("upgrade-check: parse %s: %w", path, err)
	}
	return &u, nil
}

// writeUpgradeCheck saves the cache atomically via tempfile + rename.
// POSIX rename within the same filesystem is atomic, so a reader
// either sees the previous file or the new one — never a partial.
// Mirrors state.Save's pattern exactly.
//
// Creates the parent directory if missing (first-run case where
// ~/.canopy itself doesn't exist yet).
func writeUpgradeCheck(path string, u *upgradeCheck) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("upgrade-check: mkdir parent: %w", err)
	}
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return fmt.Errorf("upgrade-check: marshal: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("upgrade-check: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("upgrade-check: rename: %w", err)
	}
	return nil
}

// cachedRemoteVersion is the synchronous-first entry point used by
// CLI commands (canopy ls, canopy upgrade --dismiss). Returns the
// effective latest + dismissed values without ever blocking on the
// network for fresh cache hits.
//
// Behavior:
//   - cache fresh                 → return cached values, fetched=false
//   - cache missing or stale      → fetch from upstream, write cache,
//                                   return new values, fetched=true
//   - fetch fails (network, etc.) → return cached values (possibly
//                                   empty) with the fetch error,
//                                   fetched=false
//
// The fetched bool lets callers decide whether to print "(refreshed)"
// vs "(from cache)" diagnostics if they want to. Most callers can
// ignore it.
//
// Network failures are non-fatal: the function still returns a usable
// (possibly empty) latest/dismissed pair plus the error. CLI callers
// typically silence the error (no upgrade hint is better than a noisy
// "couldn't check for updates" line on every invocation).
func cachedRemoteVersion(ctx context.Context, srcDir string) (latest, dismissed string, fetched bool, err error) {
	path, perr := upgradeCheckPath()
	if perr != nil {
		return "", "", false, perr
	}
	cache, rerr := readUpgradeCheck(path)
	if rerr != nil {
		// Treat malformed cache as missing — the next successful
		// fetch overwrites it. Surface the error to the caller in
		// case they want to log it.
		err = rerr
		cache = nil
	}
	if cache != nil && cache.fresh() {
		return cache.LatestVersion, cache.DismissedVersion, false, nil
	}

	// Stale or missing: try to refresh.
	remote, ferr := refreshUpgradeCheckCache(ctx, srcDir, cache)
	if ferr != nil {
		// Network failure: return whatever we had cached (may be
		// empty) so the caller degrades gracefully. The cache file
		// is left untouched; next invocation retries.
		if cache != nil {
			return cache.LatestVersion, cache.DismissedVersion, false, ferr
		}
		return "", "", false, ferr
	}
	return remote.LatestVersion, remote.DismissedVersion, true, nil
}

// refreshUpgradeCheckCache fetches the latest VERSION from upstream,
// merges it with any preserved fields from the previous cache (so the
// dismissed_version survives a refresh), and atomically rewrites the
// file. Used by both the synchronous cachedRemoteVersion path and the
// async TUI refresh tea.Cmd.
//
// Pure function over the cache file: idempotent within the same TTL
// window (well, almost — CheckedAt updates every call, but that's
// harmless).
//
// The previous cache is passed in (rather than re-read) so the caller
// controls when the read happens. Callers that don't have a previous
// cache pass nil; the dismissed_version simply starts empty in that
// case.
func refreshUpgradeCheckCache(ctx context.Context, srcDir string, prev *upgradeCheck) (*upgradeCheck, error) {
	body, err := fetchRemoteFile(ctx, srcDir, upgradeVersionURL, "VERSION")
	if err != nil {
		return nil, err
	}
	next := &upgradeCheck{
		CheckedAt:     upgradeCheckNow().UTC(),
		LatestVersion: strings.TrimSpace(body),
	}
	if prev != nil {
		// Preserve dismissal, but only if it still makes sense:
		// dismissing v0.13.0 should NOT silence v0.14.0. We keep
		// the field set even when LatestVersion changes — the
		// dismissed() helper compares the two and decides to
		// suppress or not — so the field can stay verbatim here.
		next.DismissedVersion = prev.DismissedVersion
	}
	path, perr := upgradeCheckPath()
	if perr != nil {
		return nil, perr
	}
	if werr := writeUpgradeCheck(path, next); werr != nil {
		return nil, werr
	}
	return next, nil
}

// dismissUpgradeCheck writes dismissed_version = latest_version into
// the cache. Used by `canopy upgrade --dismiss` and the TUI D key.
//
// Refuses (returns an error) when there's no cached LatestVersion to
// dismiss — nothing to silence. Callers surface the error to the user
// with a "run canopy upgrade --check first" hint.
func dismissUpgradeCheck() (latest string, err error) {
	path, perr := upgradeCheckPath()
	if perr != nil {
		return "", perr
	}
	cache, rerr := readUpgradeCheck(path)
	if rerr != nil {
		return "", rerr
	}
	if cache == nil || cache.LatestVersion == "" {
		return "", errors.New("no cached upgrade information; run `canopy upgrade --check` first")
	}
	cache.DismissedVersion = cache.LatestVersion
	if werr := writeUpgradeCheck(path, cache); werr != nil {
		return "", werr
	}
	return cache.LatestVersion, nil
}

// clearUpgradeCheck rewrites the cache to mark the running version as
// the latest, with dismissal cleared. Called at the end of a
// successful `canopy upgrade` so the pill disappears immediately
// instead of waiting for the next 6h refresh.
//
// The current arg is the bare semver of the version that was just
// installed (i.e., the one we just upgraded TO). If the file write
// fails, the function returns the error — callers typically log-and-
// continue since the upgrade itself already succeeded; the pill just
// disappears one cycle late.
func clearUpgradeCheck(current string) error {
	path, perr := upgradeCheckPath()
	if perr != nil {
		return perr
	}
	next := &upgradeCheck{
		CheckedAt:     upgradeCheckNow().UTC(),
		LatestVersion: strings.TrimSpace(current),
	}
	return writeUpgradeCheck(path, next)
}

// upgradeAvailable is the single decision function used by every
// surface (pill, ls hint, U-key gate). Returns true when there's a
// genuine new version the user hasn't dismissed.
//
// All four conditions must hold:
//   - cache exists and has a non-empty LatestVersion
//   - LatestVersion differs from the running normalized version
//   - LatestVersion is NOT the dismissed version
//   - the running binary is NOT a DEV build (DEV is exempt — canopy
//     upgrade refuses on DEV, so showing the pill would be misleading)
func upgradeAvailable(running string, isDev bool, cache *upgradeCheck) bool {
	if isDev || cache == nil || cache.LatestVersion == "" {
		return false
	}
	if cache.dismissed() {
		return false
	}
	return normalizeRunningVersion(running) != cache.LatestVersion
}
