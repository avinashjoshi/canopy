package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestUpgradeCheckPath resolves under HOME and ends with the expected
// file name. Single-line of logic but the constant is the contract
// surface for both readers and writers.
func TestUpgradeCheckPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	got, err := upgradeCheckPath()
	if err != nil {
		t.Fatalf("upgradeCheckPath: %v", err)
	}
	want := filepath.Join(tmp, ".canopy", "upgrade-check.json")
	if got != want {
		t.Errorf("upgradeCheckPath = %q, want %q", got, want)
	}
}

// TestReadUpgradeCheck_missing covers the "first run, no cache yet"
// branch. Returning (nil, nil) lets callers branch cleanly without
// parsing error strings.
func TestReadUpgradeCheck_missing(t *testing.T) {
	tmp := t.TempDir()
	got, err := readUpgradeCheck(filepath.Join(tmp, "nope.json"))
	if err != nil {
		t.Fatalf("missing file should NOT be an error; got %v", err)
	}
	if got != nil {
		t.Errorf("missing file should return nil cache; got %+v", got)
	}
}

// TestReadUpgradeCheck_valid round-trips a written cache through read.
// All three fields (CheckedAt, LatestVersion, DismissedVersion) must
// survive marshal+unmarshal.
func TestReadUpgradeCheck_valid(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "u.json")
	now := time.Date(2026, 4, 30, 12, 34, 56, 0, time.UTC)
	want := &upgradeCheck{
		CheckedAt:        now,
		LatestVersion:    "0.13.0",
		DismissedVersion: "0.12.3",
	}
	if err := writeUpgradeCheck(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readUpgradeCheck(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil {
		t.Fatal("read returned nil for present file")
	}
	if !got.CheckedAt.Equal(want.CheckedAt) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, want.CheckedAt)
	}
	if got.LatestVersion != want.LatestVersion {
		t.Errorf("LatestVersion = %q, want %q", got.LatestVersion, want.LatestVersion)
	}
	if got.DismissedVersion != want.DismissedVersion {
		t.Errorf("DismissedVersion = %q, want %q", got.DismissedVersion, want.DismissedVersion)
	}
}

// TestReadUpgradeCheck_malformed surfaces a parse error so the caller
// can decide to log-and-continue. The function does NOT swallow the
// error — that decision belongs to the caller (most just treat
// malformed as missing and overwrite on next fetch).
func TestReadUpgradeCheck_malformed(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readUpgradeCheck(path)
	if err == nil {
		t.Fatal("malformed JSON should return an error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure; got %v", err)
	}
}

// TestWriteUpgradeCheck_atomic verifies the tmpfile+rename pattern
// leaves no .tmp file behind on success and produces a readable
// result on disk. Mirrors state.Save's atomicity guarantee.
func TestWriteUpgradeCheck_atomic(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".canopy", "u.json")
	u := &upgradeCheck{
		CheckedAt:     time.Now().UTC(),
		LatestVersion: "0.13.0",
	}
	if err := writeUpgradeCheck(path, u); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("output file should exist; got %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".tmp file should be cleaned up after rename; stat = %v", err)
	}
}

// TestWriteUpgradeCheck_createsParent: first run, ~/.canopy may not
// exist yet. The write must mkdir -p so the cache gets created
// regardless of how canopy was invoked first.
func TestWriteUpgradeCheck_createsParent(t *testing.T) {
	tmp := t.TempDir()
	// Deeply nested path that doesn't exist at all.
	path := filepath.Join(tmp, "a", "b", "c", "u.json")
	u := &upgradeCheck{LatestVersion: "0.13.0"}
	if err := writeUpgradeCheck(path, u); err != nil {
		t.Fatalf("write should mkdir parent; got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should exist after write; got %v", err)
	}
}

// TestUpgradeCheckFresh_TTL covers both branches of the freshness
// gate using the upgradeCheckNow seam to simulate elapsed time
// without sleeping. 5h59m = fresh; 6h01m = stale.
func TestUpgradeCheckFresh_TTL(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	prevNow := upgradeCheckNow
	t.Cleanup(func() { upgradeCheckNow = prevNow })
	upgradeCheckNow = func() time.Time { return now }

	cases := []struct {
		name      string
		checkedAt time.Time
		wantFresh bool
	}{
		{"zero -> stale", time.Time{}, false},
		{"5h59m -> fresh", now.Add(-5*time.Hour - 59*time.Minute), true},
		{"6h01m -> stale", now.Add(-6*time.Hour - 1*time.Minute), false},
		{"24h -> stale", now.Add(-24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &upgradeCheck{CheckedAt: tc.checkedAt, LatestVersion: "0.13.0"}
			if got := u.fresh(); got != tc.wantFresh {
				t.Errorf("fresh() = %v, want %v", got, tc.wantFresh)
			}
		})
	}

	// Nil receiver: defensive, treat as stale.
	if (*upgradeCheck)(nil).fresh() {
		t.Error("nil receiver should be stale")
	}
}

// TestUpgradeCheckDismissed: dismissal is per-version. v0.13 dismissed
// suppresses v0.13 but not v0.14 (because the field changes
// underneath).
func TestUpgradeCheckDismissed(t *testing.T) {
	cases := []struct {
		name string
		u    *upgradeCheck
		want bool
	}{
		{"nil -> not dismissed", nil, false},
		{"empty dismissed -> not dismissed", &upgradeCheck{LatestVersion: "0.13.0"}, false},
		{"matches latest -> dismissed", &upgradeCheck{LatestVersion: "0.13.0", DismissedVersion: "0.13.0"}, true},
		{"differs from latest -> not dismissed", &upgradeCheck{LatestVersion: "0.14.0", DismissedVersion: "0.13.0"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.dismissed(); got != tc.want {
				t.Errorf("dismissed() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUpgradeAvailable covers all four gates of the surface decision:
// DEV-binary suppression, missing cache, dismissed version, semver
// equality. Every other surface (pill, ls hint, U-key) reads through
// this single helper so it's the load-bearing one to test.
func TestUpgradeAvailable(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name    string
		running string
		isDev   bool
		cache   *upgradeCheck
		want    bool
	}{
		{"dev binary always false", "v0.12.3", true, &upgradeCheck{LatestVersion: "0.13.0", CheckedAt: now}, false},
		{"nil cache false", "v0.12.3", false, nil, false},
		{"empty latest false", "v0.12.3", false, &upgradeCheck{CheckedAt: now}, false},
		{"dismissed false", "v0.12.3", false, &upgradeCheck{LatestVersion: "0.13.0", DismissedVersion: "0.13.0", CheckedAt: now}, false},
		{"same as running false", "v0.13.0", false, &upgradeCheck{LatestVersion: "0.13.0", CheckedAt: now}, false},
		{"running with v prefix false", "v0.13.0+abc", false, &upgradeCheck{LatestVersion: "0.13.0", CheckedAt: now}, false},
		{"newer available true", "v0.12.3", false, &upgradeCheck{LatestVersion: "0.13.0", CheckedAt: now}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upgradeAvailable(tc.running, tc.isDev, tc.cache); got != tc.want {
				t.Errorf("upgradeAvailable(%q, dev=%v, %+v) = %v, want %v",
					tc.running, tc.isDev, tc.cache, got, tc.want)
			}
		})
	}
}

// TestCachedRemoteVersion_freshHit short-circuits the network. The
// cache must be returned verbatim and upgradeFetchVersion must NOT
// be called.
func TestCachedRemoteVersion_freshHit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	prevFetch := upgradeFetchVersion
	t.Cleanup(func() { upgradeFetchVersion = prevFetch })
	upgradeFetchVersion = func(ctx context.Context, url string) (string, error) {
		t.Error("fresh cache hit should NOT trigger network fetch")
		return "", nil
	}

	now := time.Now().UTC()
	prevNow := upgradeCheckNow
	t.Cleanup(func() { upgradeCheckNow = prevNow })
	upgradeCheckNow = func() time.Time { return now }

	path, _ := upgradeCheckPath()
	if err := writeUpgradeCheck(path, &upgradeCheck{
		CheckedAt:        now.Add(-1 * time.Hour),
		LatestVersion:    "0.13.0",
		DismissedVersion: "0.12.0",
	}); err != nil {
		t.Fatal(err)
	}

	latest, dismissed, fetched, err := cachedRemoteVersion(context.Background(), tmp)
	if err != nil {
		t.Fatalf("cachedRemoteVersion: %v", err)
	}
	if latest != "0.13.0" || dismissed != "0.12.0" {
		t.Errorf("got (%q, %q), want (0.13.0, 0.12.0)", latest, dismissed)
	}
	if fetched {
		t.Error("fetched should be false on cache hit")
	}
}

// TestCachedRemoteVersion_staleRefetch: cache exists but is older
// than TTL → fetch fresh value, write back, preserve dismissed.
func TestCachedRemoteVersion_staleRefetch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	prevFetch := upgradeFetchVersion
	t.Cleanup(func() { upgradeFetchVersion = prevFetch })
	fetchCalls := 0
	upgradeFetchVersion = func(ctx context.Context, url string) (string, error) {
		fetchCalls++
		return "0.14.0", nil
	}

	now := time.Now().UTC()
	prevNow := upgradeCheckNow
	t.Cleanup(func() { upgradeCheckNow = prevNow })
	upgradeCheckNow = func() time.Time { return now }

	path, _ := upgradeCheckPath()
	if err := writeUpgradeCheck(path, &upgradeCheck{
		CheckedAt:        now.Add(-7 * time.Hour), // stale
		LatestVersion:    "0.13.0",
		DismissedVersion: "0.12.0",
	}); err != nil {
		t.Fatal(err)
	}

	latest, dismissed, fetched, err := cachedRemoteVersion(context.Background(), tmp)
	if err != nil {
		t.Fatalf("cachedRemoteVersion: %v", err)
	}
	if fetchCalls != 1 {
		t.Errorf("fetchCalls = %d, want 1", fetchCalls)
	}
	if latest != "0.14.0" {
		t.Errorf("latest = %q, want 0.14.0", latest)
	}
	if dismissed != "0.12.0" {
		t.Errorf("dismissed = %q, want 0.12.0 (preserved across refresh)", dismissed)
	}
	if !fetched {
		t.Error("fetched should be true after refresh")
	}

	// Cache file on disk must reflect the new state.
	got, err := readUpgradeCheck(path)
	if err != nil || got == nil {
		t.Fatalf("readback: err=%v cache=%v", err, got)
	}
	if got.LatestVersion != "0.14.0" {
		t.Errorf("disk LatestVersion = %q, want 0.14.0", got.LatestVersion)
	}
	if got.DismissedVersion != "0.12.0" {
		t.Errorf("disk DismissedVersion = %q, want 0.12.0", got.DismissedVersion)
	}
	if !got.CheckedAt.Equal(now) {
		t.Errorf("disk CheckedAt = %v, want %v", got.CheckedAt, now)
	}
}

// TestCachedRemoteVersion_missingFetch: first-run path. No cache,
// fetch succeeds, cache gets written.
func TestCachedRemoteVersion_missingFetch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	prevFetch := upgradeFetchVersion
	t.Cleanup(func() { upgradeFetchVersion = prevFetch })
	upgradeFetchVersion = func(ctx context.Context, url string) (string, error) {
		return "0.13.0", nil
	}

	latest, dismissed, fetched, err := cachedRemoteVersion(context.Background(), tmp)
	if err != nil {
		t.Fatalf("cachedRemoteVersion: %v", err)
	}
	if latest != "0.13.0" || dismissed != "" || !fetched {
		t.Errorf("got (%q, %q, fetched=%v), want (0.13.0, \"\", true)",
			latest, dismissed, fetched)
	}
}

// TestCachedRemoteVersion_fetchFailsWithStaleCache: degrade
// gracefully. Return whatever we had cached so the pill keeps showing
// the last-known version even when offline.
func TestCachedRemoteVersion_fetchFailsWithStaleCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	prevFetch := upgradeFetchVersion
	prevGitFetch := upgradeGitFetchFile
	t.Cleanup(func() {
		upgradeFetchVersion = prevFetch
		upgradeGitFetchFile = prevGitFetch
	})
	upgradeFetchVersion = func(ctx context.Context, url string) (string, error) {
		return "", errors.New("no network")
	}
	upgradeGitFetchFile = func(ctx context.Context, srcDir, gitPath string) (string, error) {
		return "", errors.New("git: no network")
	}

	now := time.Now().UTC()
	prevNow := upgradeCheckNow
	t.Cleanup(func() { upgradeCheckNow = prevNow })
	upgradeCheckNow = func() time.Time { return now }

	path, _ := upgradeCheckPath()
	if err := writeUpgradeCheck(path, &upgradeCheck{
		CheckedAt:     now.Add(-7 * time.Hour),
		LatestVersion: "0.13.0",
	}); err != nil {
		t.Fatal(err)
	}

	latest, _, fetched, err := cachedRemoteVersion(context.Background(), tmp)
	if err == nil {
		t.Error("expected error from failed fetch")
	}
	if latest != "0.13.0" {
		t.Errorf("latest = %q, want 0.13.0 (cached fallback)", latest)
	}
	if fetched {
		t.Error("fetched should be false when fetch failed")
	}

	// Cache file should be UNCHANGED — failed fetch must not
	// corrupt the previous good value.
	got, err := readUpgradeCheck(path)
	if err != nil || got == nil {
		t.Fatalf("readback: %v %v", err, got)
	}
	if got.LatestVersion != "0.13.0" {
		t.Errorf("disk LatestVersion changed to %q after failed fetch", got.LatestVersion)
	}
}

// TestDismissUpgradeCheck_noCache refuses cleanly when there's
// nothing to dismiss. The error message points at --check.
func TestDismissUpgradeCheck_noCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	_, err := dismissUpgradeCheck()
	if err == nil {
		t.Fatal("expected error when no cached latest")
	}
	if !strings.Contains(err.Error(), "--check") {
		t.Errorf("error should suggest --check; got %v", err)
	}
}

// TestDismissUpgradeCheck_writes the dismissal field on top of an
// existing cache. Other fields preserved.
func TestDismissUpgradeCheck_writes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	path, _ := upgradeCheckPath()
	if err := writeUpgradeCheck(path, &upgradeCheck{
		CheckedAt:     now,
		LatestVersion: "0.13.0",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := dismissUpgradeCheck()
	if err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if got != "0.13.0" {
		t.Errorf("returned dismissed version = %q, want 0.13.0", got)
	}
	cache, err := readUpgradeCheck(path)
	if err != nil || cache == nil {
		t.Fatalf("readback: %v %v", err, cache)
	}
	if cache.DismissedVersion != "0.13.0" {
		t.Errorf("disk DismissedVersion = %q, want 0.13.0", cache.DismissedVersion)
	}
	if !cache.CheckedAt.Equal(now) {
		t.Errorf("CheckedAt should not change on dismiss; got %v", cache.CheckedAt)
	}
}

// TestClearUpgradeCheck overwrites the cache to mark the running
// version as latest with empty dismissal — used at the end of a
// successful upgrade so the pill disappears immediately.
func TestClearUpgradeCheck(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	prevNow := upgradeCheckNow
	t.Cleanup(func() { upgradeCheckNow = prevNow })
	upgradeCheckNow = func() time.Time { return now }

	path, _ := upgradeCheckPath()
	// Pre-existing cache with a dismissal that should be cleared.
	if err := writeUpgradeCheck(path, &upgradeCheck{
		CheckedAt:        now.Add(-7 * time.Hour),
		LatestVersion:    "0.12.3",
		DismissedVersion: "0.12.3",
	}); err != nil {
		t.Fatal(err)
	}

	if err := clearUpgradeCheck("0.13.0"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := readUpgradeCheck(path)
	if err != nil || got == nil {
		t.Fatalf("readback: %v %v", err, got)
	}
	if got.LatestVersion != "0.13.0" {
		t.Errorf("LatestVersion = %q, want 0.13.0", got.LatestVersion)
	}
	if got.DismissedVersion != "" {
		t.Errorf("DismissedVersion = %q, want empty (cleared post-upgrade)", got.DismissedVersion)
	}
	if !got.CheckedAt.Equal(now) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, now)
	}
}
