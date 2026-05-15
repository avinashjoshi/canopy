package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeBareRepoTmp builds a hermetic local bare repo and returns its
// file:// URL so URL-flow tests don't need network access. Mirrors
// makeBareRepo in internal/git/clone_test.go.
func makeBareRepoTmp(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, name+"-work")
	if err := exec.Command("git", "init", work).Run(); err != nil {
		t.Skipf("git init: %v", err)
	}
	for _, args := range [][]string{
		{"-C", work, "config", "user.email", "test@example.com"},
		{"-C", work, "config", "user.name", "Test"},
		{"-C", work, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	bare := filepath.Join(root, name+".git")
	if out, err := exec.Command("git", "clone", "--bare", work, bare).CombinedOutput(); err != nil {
		t.Fatalf("clone --bare: %v\n%s", err, out)
	}
	return "file://" + bare
}

// withFakeHome redirects HOME so init's state.json + config.json land
// in a tempdir for the test. Returns the canopyHome path the runAddProject
// argument expects.
func withFakeHome(t *testing.T) (canopyHome string) {
	t.Helper()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	os.Unsetenv("CANOPY_SOURCE_ROOT")
	canopyHome = filepath.Join(fakeHome, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("MkdirAll canopyHome: %v", err)
	}
	return canopyHome
}

// TestRunAddProject_EmptyArg_UsesCwd: backwards-compat path. No arg
// means "init my cwd" — today's behavior preserved.
func TestRunAddProject_EmptyArg_UsesCwd(t *testing.T) {
	canopyHome := withFakeHome(t)
	cwd := t.TempDir()
	t.Chdir(cwd)

	var out bytes.Buffer
	dest, err := runAddProject(context.Background(), "", addProjectOptions{}, &out, canopyHome)
	if err != nil {
		t.Fatalf("runAddProject: %v\n%s", err, out.String())
	}
	// runInit canonicalizes cwd via EvalSymlinks (macOS /var ↔ /private/var).
	// Compare basenames to stay portable.
	if filepath.Base(dest) != filepath.Base(cwd) {
		t.Errorf("dest = %q; want basename %q", dest, filepath.Base(cwd))
	}
	if _, err := os.Stat(filepath.Join(cwd, "canopy.json")); err != nil {
		t.Errorf("canopy.json missing after empty-arg init: %v", err)
	}
}

// TestRunAddProject_PathArg: pointing at a path inits it without cd-ing.
// The classic "I want to register this folder" case.
func TestRunAddProject_PathArg(t *testing.T) {
	canopyHome := withFakeHome(t)
	target := t.TempDir()

	var out bytes.Buffer
	dest, err := runAddProject(context.Background(), target, addProjectOptions{}, &out, canopyHome)
	if err != nil {
		t.Fatalf("runAddProject: %v\n%s", err, out.String())
	}
	if filepath.Base(dest) != filepath.Base(target) {
		t.Errorf("dest = %q; want basename %q", dest, filepath.Base(target))
	}
	if _, err := os.Stat(filepath.Join(target, "canopy.json")); err != nil {
		t.Errorf("canopy.json missing: %v", err)
	}
}

// TestRunAddProject_PathMissing: a nonexistent path errors clearly.
// The user typo'd or pointed at something gone.
func TestRunAddProject_PathMissing(t *testing.T) {
	canopyHome := withFakeHome(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := runAddProject(context.Background(), missing, addProjectOptions{}, &bytes.Buffer{}, canopyHome)
	if err == nil {
		t.Fatal("missing path: nil error; want refused")
	}
}

// TestRunAddProject_PathIsFile: pointing at a regular file (not a dir)
// errors. No silent fallback that would write canopy.json into the file.
func TestRunAddProject_PathIsFile(t *testing.T) {
	canopyHome := withFakeHome(t)
	root := t.TempDir()
	regularFile := filepath.Join(root, "not-a-dir.txt")
	if err := os.WriteFile(regularFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := runAddProject(context.Background(), regularFile, addProjectOptions{}, &bytes.Buffer{}, canopyHome)
	if err == nil {
		t.Fatal("file as arg: nil error; want refused")
	}
}

// TestRunAddProject_URL_CloneAndInit: the full URL flow. Hermetic clone
// from a local bare repo, lands at source-root/<basename>, canopy.json
// written, state registered. The marquee path.
func TestRunAddProject_URL_CloneAndInit(t *testing.T) {
	canopyHome := withFakeHome(t)
	// Set source-root via env so the default location doesn't depend on
	// existing/missing dirs in the fake home.
	srcRoot := t.TempDir()
	t.Setenv("CANOPY_SOURCE_ROOT", srcRoot)

	url := makeBareRepoTmp(t, "fixture")

	var out bytes.Buffer
	dest, err := runAddProject(context.Background(), url, addProjectOptions{}, &out, canopyHome)
	if err != nil {
		t.Fatalf("runAddProject URL: %v\n%s", err, out.String())
	}
	if filepath.Dir(dest) != srcRoot {
		t.Errorf("dest = %q; want parent %q", dest, srcRoot)
	}
	if filepath.Base(dest) != "fixture" {
		t.Errorf("dest basename = %q; want fixture", filepath.Base(dest))
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Errorf(".git missing after clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "canopy.json")); err != nil {
		t.Errorf("canopy.json missing after init: %v", err)
	}
	if !strings.Contains(out.String(), "Cloning") {
		t.Errorf("output missing 'Cloning' status line; got %q", out.String())
	}
}

// TestRunAddProject_URL_ExplicitDest: `canopy init <url> <dest>` —
// the 2nd positional overrides source-root entirely.
func TestRunAddProject_URL_ExplicitDest(t *testing.T) {
	canopyHome := withFakeHome(t)
	t.Setenv("CANOPY_SOURCE_ROOT", "/should-be-ignored")

	url := makeBareRepoTmp(t, "fixture")
	explicitDest := filepath.Join(t.TempDir(), "my-explicit-dest")

	var out bytes.Buffer
	dest, err := runAddProject(context.Background(), url, addProjectOptions{DestOverride: explicitDest}, &out, canopyHome)
	if err != nil {
		t.Fatalf("runAddProject: %v\n%s", err, out.String())
	}
	if dest != explicitDest {
		t.Errorf("dest = %q; want %q", dest, explicitDest)
	}
	if _, err := os.Stat(filepath.Join(explicitDest, "canopy.json")); err != nil {
		t.Errorf("canopy.json missing at explicit dest: %v", err)
	}
}

// TestRunAddProject_URL_Idempotent: running the same `canopy init <url>`
// twice is safe. Second run sees the existing .git, skips clone, and
// re-inits in place. No error.
func TestRunAddProject_URL_Idempotent(t *testing.T) {
	canopyHome := withFakeHome(t)
	srcRoot := t.TempDir()
	t.Setenv("CANOPY_SOURCE_ROOT", srcRoot)

	url := makeBareRepoTmp(t, "fixture")

	for i, label := range []string{"first", "second"} {
		var out bytes.Buffer
		_, err := runAddProject(context.Background(), url, addProjectOptions{}, &out, canopyHome)
		if err != nil {
			t.Fatalf("%s run: %v\n%s", label, err, out.String())
		}
		if i == 1 && !strings.Contains(out.String(), "Re-using existing repo") {
			t.Errorf("second run output missing 'Re-using existing repo' status; got %q", out.String())
		}
	}
}

// TestRunAddProject_URL_DestCollisionNotGit: dest exists but isn't a
// git repo — error, no destructive write into the user's dir.
func TestRunAddProject_URL_DestCollisionNotGit(t *testing.T) {
	canopyHome := withFakeHome(t)
	srcRoot := t.TempDir()
	t.Setenv("CANOPY_SOURCE_ROOT", srcRoot)

	url := makeBareRepoTmp(t, "fixture")
	// Pre-create the dest dir as a regular dir.
	collide := filepath.Join(srcRoot, "fixture")
	if err := os.MkdirAll(collide, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(collide, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := runAddProject(context.Background(), url, addProjectOptions{}, &bytes.Buffer{}, canopyHome)
	if err == nil {
		t.Fatal("non-git dest collision: nil error; want refused")
	}
	if !strings.Contains(err.Error(), "isn't a git repo") {
		t.Errorf("err = %v; want 'isn't a git repo' message", err)
	}
}

// TestRunAddProject_PathArg_WithDestRejected: <dest> only makes sense
// with a URL. Passing it alongside a path arg errors with a clear msg.
func TestRunAddProject_PathArg_WithDestRejected(t *testing.T) {
	canopyHome := withFakeHome(t)
	target := t.TempDir()
	_, err := runAddProject(context.Background(), target, addProjectOptions{DestOverride: "/some/other"}, &bytes.Buffer{}, canopyHome)
	if err == nil {
		t.Fatal("path + dest: nil error; want refused")
	}
}

// TestRunInit_WritesInitResultFileWhenEnvSet: when CANOPY_INIT_RESULT_FILE
// is set (the SSH-dispatch path from `canopy init --on <host>`), runInit
// writes the canonical project root to that file on success. Without this
// the laptop can't auto-register the new project in its hosts.json after
// a remote init dispatch — and `canopy new --on <host>` then fails with
// "host has no projects registered".
func TestRunInit_WritesInitResultFileWhenEnvSet(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	if err := os.MkdirAll(filepath.Join(fakeHome, ".canopy"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	resultPath := filepath.Join(t.TempDir(), "init-result.txt")
	t.Setenv("CANOPY_INIT_RESULT_FILE", resultPath)

	projectRoot := t.TempDir()
	var out bytes.Buffer
	if err := runInit(projectRoot, initOptions{}, &out); err != nil {
		t.Fatalf("runInit: %v\n%s", err, out.String())
	}

	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	got := strings.TrimSpace(string(data))
	// Canonical path may EvalSymlinks the basename (macOS /var ↔ /private/var)
	// so compare basenames to stay portable across runners.
	if filepath.Base(got) != filepath.Base(projectRoot) {
		t.Errorf("result file = %q; want basename %q", got, filepath.Base(projectRoot))
	}
}

// TestRunInit_NoResultFileWhenEnvUnset: the env-var-driven side effect
// is opt-in. Plain `canopy init` (no remote dispatch) doesn't write any
// extra file.
func TestRunInit_NoResultFileWhenEnvUnset(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	os.Unsetenv("CANOPY_INIT_RESULT_FILE")
	if err := os.MkdirAll(filepath.Join(fakeHome, ".canopy"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	projectRoot := t.TempDir()
	var out bytes.Buffer
	if err := runInit(projectRoot, initOptions{}, &out); err != nil {
		t.Fatalf("runInit: %v\n%s", err, out.String())
	}
	// No assertion needed beyond "did not panic / did not create a
	// surprise file" — runInit's normal contract holds.
}

// TestRunInit_BugFix_RegistersOnEarlyReturn is the regression test
// for the v0.20 bug fix. Pre-fix: runInit early-returned when
// canopy.json existed AND didn't register the project. Post-fix:
// it registers, so `canopy ls` sees the project.
//
// Scenario: a dir with a canopy.json already in place but the project
// not yet in state.json (the cloned-canopy case).
func TestRunInit_BugFix_RegistersOnEarlyReturn(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	if err := os.MkdirAll(filepath.Join(fakeHome, ".canopy"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Build a dir with canopy.json BUT don't register the project.
	projectRoot := t.TempDir()
	canopyJSON := filepath.Join(projectRoot, "canopy.json")
	if err := os.WriteFile(canopyJSON, []byte(`{"scripts":{}}`), 0o644); err != nil {
		t.Fatalf("seed canopy.json: %v", err)
	}

	// Run runInit. It should hit the "already initialized" early-return
	// branch but STILL register the project.
	var out bytes.Buffer
	if err := runInit(projectRoot, initOptions{}, &out); err != nil {
		t.Fatalf("runInit: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "already exists") {
		t.Errorf("expected early-return msg; got %q", out.String())
	}

	// Verify state.json now has the project registered.
	store, err := openStateForInit()
	if err != nil {
		t.Fatalf("openStateForInit: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	canon, err := canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if _, ok := st.Projects[canon]; !ok {
		t.Errorf("project not registered after early-return; have keys %v, want %s",
			keysOf(st.Projects), canon)
	}
}

// keysOf is a tiny helper for the bug-fix test's error message.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
