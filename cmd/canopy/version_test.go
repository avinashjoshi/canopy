package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFormatVersionDetails_release covers the release-mode rendering
// path: header line is "canopy <version>", mode line is "release", no
// workspace line. Binary path + commit + date all show when present.
func TestFormatVersionDetails_release(t *testing.T) {
	d := VersionDetails{
		Version:       "v0.12.0+abc1234",
		Commit:        "abc1234",
		Date:          "2026-04-30T12:34:56Z",
		BinaryPath:    "/home/avi/.local/bin/canopy",
		SymlinkTarget: "canopy.bin",
		IsDev:         false,
	}
	got := formatVersionDetails(d)

	for _, want := range []string{
		"canopy v0.12.0+abc1234\n",
		"  binary:    /home/avi/.local/bin/canopy -> canopy.bin\n",
		"  commit:    abc1234\n",
		"  built:     2026-04-30T12:34:56Z\n",
		"  mode:      release\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing line %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "DEV") {
		t.Errorf("release output must not contain DEV:\n%s", got)
	}
	if strings.Contains(got, "workspace:") {
		t.Errorf("release output must not contain workspace line:\n%s", got)
	}
}

// TestFormatVersionDetails_dev covers the DEV-mode rendering: header
// is "canopy DEV", workspace line is present, mode line is "DEV".
func TestFormatVersionDetails_dev(t *testing.T) {
	d := VersionDetails{
		Version:       "dev",
		Commit:        "abc1234",
		Date:          "unknown",
		BinaryPath:    "/home/avi/.local/bin/canopy",
		SymlinkTarget: "/home/avi/.canopy/workspaces/canopy/feature-A/canopy",
		IsDev:         true,
		DevWorkspace:  "feature-A",
	}
	got := formatVersionDetails(d)

	for _, want := range []string{
		"canopy DEV\n",
		"  workspace: feature-A\n",
		"  binary:    /home/avi/.local/bin/canopy -> /home/avi/.canopy/workspaces/canopy/feature-A/canopy\n",
		"  commit:    abc1234\n",
		"  mode:      DEV\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing line %q in:\n%s", want, got)
		}
	}
	// "unknown" date should be suppressed — it's a sentinel, not a
	// useful timestamp.
	if strings.Contains(got, "built:") {
		t.Errorf("dev output should suppress 'built: unknown':\n%s", got)
	}
	if strings.Contains(got, "release") {
		t.Errorf("dev output must not contain 'release':\n%s", got)
	}
}

// TestFormatVersionDetails_devUntracked: dev build with no resolvable
// workspace shows "(untracked)" in the workspace slot. This is the
// graceful-fallback path described in the design doc.
func TestFormatVersionDetails_devUntracked(t *testing.T) {
	d := VersionDetails{
		Version: "dev",
		IsDev:   true,
	}
	got := formatVersionDetails(d)
	if !strings.Contains(got, "  workspace: (untracked)\n") {
		t.Errorf("dev output with empty DevWorkspace should show '(untracked)':\n%s", got)
	}
}

// TestDevWorkspaceFromBinary_pathHeuristic exercises the path-based
// detection for binaries inside the canonical canopy worktree layout
// (~/.canopy/workspaces/<project>/<name>/canopy). No git fork needed.
func TestDevWorkspaceFromBinary_pathHeuristic(t *testing.T) {
	// Stub the git fallback so we know any positive result came from
	// the path heuristic, not git. If git gets called for any of these
	// inputs, the heuristic missed.
	prev := runGitBranchShowCurrent
	t.Cleanup(func() { runGitBranchShowCurrent = prev })
	runGitBranchShowCurrent = func(dir string) (string, error) {
		t.Errorf("path heuristic should have caught this; git fallback called for %q", dir)
		return "", errors.New("should not be called")
	}

	// Real-on-disk test: build a fake worktree layout under a temp dir
	// so EvalSymlinks can resolve a real path. The heuristic walks
	// three levels up looking for "workspaces" — any layout matching
	// .../workspaces/<anything>/<name>/canopy should yield <name>.
	tmp := t.TempDir()
	wsRoot := filepath.Join(tmp, ".canopy", "workspaces", "canopy", "feature-A")
	if err := mkdirAllForTest(t, wsRoot); err != nil {
		return
	}
	binPath := filepath.Join(wsRoot, "canopy")
	if err := writeEmptyFileForTest(t, binPath); err != nil {
		return
	}

	got := devWorkspaceFromBinary(binPath)
	if got != "feature-A" {
		t.Errorf("devWorkspaceFromBinary path heuristic: got %q, want %q", got, "feature-A")
	}
}

// TestDevWorkspaceFromBinary_gitFallback exercises the git fallback for
// binaries that DON'T live in the canonical workspaces layout (e.g.,
// a contributor's source clone at ~/Code/canopy/canopy).
func TestDevWorkspaceFromBinary_gitFallback(t *testing.T) {
	prev := runGitBranchShowCurrent
	t.Cleanup(func() { runGitBranchShowCurrent = prev })

	gitCalled := false
	runGitBranchShowCurrent = func(dir string) (string, error) {
		gitCalled = true
		return "my-feature-branch", nil
	}

	// Layout: /tmp/somewhere-not-canopy-workspaces/canopy
	// Path heuristic should miss; git fallback should fire.
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "Code", "canopy")
	if err := mkdirAllForTest(t, srcDir); err != nil {
		return
	}
	binPath := filepath.Join(srcDir, "canopy")
	if err := writeEmptyFileForTest(t, binPath); err != nil {
		return
	}

	got := devWorkspaceFromBinary(binPath)
	if !gitCalled {
		t.Error("git fallback was never invoked despite path heuristic miss")
	}
	if got != "my-feature-branch" {
		t.Errorf("devWorkspaceFromBinary git fallback: got %q, want %q", got, "my-feature-branch")
	}
}

// TestDevWorkspaceFromBinary_gitFails: when the binary lives outside
// the canonical layout AND git fails (not a worktree, or unrelated
// dir), we return "" — UI surfaces "(untracked)" / bare "[DEV]".
func TestDevWorkspaceFromBinary_gitFails(t *testing.T) {
	prev := runGitBranchShowCurrent
	t.Cleanup(func() { runGitBranchShowCurrent = prev })
	runGitBranchShowCurrent = func(dir string) (string, error) {
		return "", errors.New("not a git repository")
	}

	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "canopy")
	if err := writeEmptyFileForTest(t, binPath); err != nil {
		return
	}

	got := devWorkspaceFromBinary(binPath)
	if got != "" {
		t.Errorf("git failure should yield empty workspace, got %q", got)
	}
}

// TestDevWorkspaceFromBinary_emptyPath: defensive — an empty path
// argument must return empty without panicking. UI calls this on
// every render so robustness matters.
func TestDevWorkspaceFromBinary_emptyPath(t *testing.T) {
	prev := runGitBranchShowCurrent
	t.Cleanup(func() { runGitBranchShowCurrent = prev })
	runGitBranchShowCurrent = func(dir string) (string, error) {
		t.Error("git fallback should not be invoked for empty path")
		return "", nil
	}

	if got := devWorkspaceFromBinary(""); got != "" {
		t.Errorf("empty path should yield empty workspace, got %q", got)
	}
}

// TestVersionInfo_compatTuple: the legacy versionInfo() wrapper still
// returns (version, commit, date) consistent with versionDetails().
// Some external callers may exist; this guards the contract.
func TestVersionInfo_compatTuple(t *testing.T) {
	v, c, dt := versionInfo()
	d := versionDetails()
	if v != d.Version {
		t.Errorf("versionInfo version mismatch: tuple=%q details=%q", v, d.Version)
	}
	if c != d.Commit {
		t.Errorf("versionInfo commit mismatch: tuple=%q details=%q", c, d.Commit)
	}
	if dt != d.Date {
		t.Errorf("versionInfo date mismatch: tuple=%q details=%q", dt, d.Date)
	}
}

// TestVersionDetails_devSentinelSurvivesBuildInfoFallback covers the
// regression that v0.12.0 shipped with: BuildInfo would overwrite a
// literal "dev" version with a pseudo-version like
// "v0.0.0-20260430-6f65463" for `make build` binaries (in-repo go
// build), which tripped IsDev to false. That made the TUI DEV pill
// disappear, the statusline DEV suffix vanish, and `canopy upgrade`
// stop refusing dev binaries — defeating the whole release/dev
// distinction the design depends on.
//
// Fix: rawVersion is captured before the fallback runs, and IsDev
// reads from rawVersion. d.Version can still surface the pseudo for
// forensic display, but the dev/release classification stays honest.
func TestVersionDetails_devSentinelSurvivesBuildInfoFallback(t *testing.T) {
	prev := version
	t.Cleanup(func() { version = prev })
	version = "dev"

	d := versionDetails()

	// IsDev must be true regardless of what BuildInfo did to d.Version.
	// The Go test harness builds binaries with ldflags-less go build,
	// so BuildInfo here typically returns a real Main.Version (the
	// test binary's pseudo). That's fine — we just need IsDev right.
	if !d.IsDev {
		t.Errorf("dev sentinel must keep IsDev=true after BuildInfo fallback; got Version=%q IsDev=%v", d.Version, d.IsDev)
	}
}

// TestVersionDetails_releaseSentinelStaysRelease: the inverse case.
// When ldflags inject a real version (the `make install` path),
// IsDev must be false. Defends against an over-correction where
// rawVersion-based IsDev computation might drift from intent.
func TestVersionDetails_releaseSentinelStaysRelease(t *testing.T) {
	prev := version
	t.Cleanup(func() { version = prev })
	version = "v0.12.1+abc1234"

	d := versionDetails()

	if d.IsDev {
		t.Errorf("ldflags-injected version must keep IsDev=false; got Version=%q IsDev=%v", d.Version, d.IsDev)
	}
}

// --- test helpers (package-private, used only by version_test.go) ---

func mkdirAllForTest(t *testing.T, dir string) error {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
		return err
	}
	return nil
}

func writeEmptyFileForTest(t *testing.T, path string) error {
	t.Helper()
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
		return err
	}
	return nil
}
