package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/ui"
)

// TestAtomicSymlink_replacesExisting: a pre-existing symlink to one
// target gets atomically retargeted at another. Replicates the core
// switching primitive `canopy use` exists for.
func TestAtomicSymlink_replacesExisting(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "canopy")

	// First link → "canopy.bin"
	if err := atomicSymlink("canopy.bin", link); err != nil {
		t.Fatalf("first symlink: %v", err)
	}
	if got, _ := os.Readlink(link); got != "canopy.bin" {
		t.Fatalf("after first: got %q, want %q", got, "canopy.bin")
	}

	// Retarget → "/abs/path/canopy"
	target := filepath.Join(dir, "abs", "canopy")
	if err := atomicSymlink(target, link); err != nil {
		t.Fatalf("retarget: %v", err)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Errorf("after retarget: got %q, want %q", got, target)
	}

	// No leftover tempfiles in dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".canopy-symlink-tmp-") {
			t.Errorf("leftover tempfile after atomic replace: %s", e.Name())
		}
	}
}

// TestAtomicSymlink_createsParent: when the parent dir doesn't exist
// yet (fresh install, ~/.local/bin not created), atomicSymlink mkdirs
// before linking. install.sh and `make install` should never have to
// pre-create the dir.
func TestAtomicSymlink_createsParent(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "newdir", "subdir", "canopy")

	if err := atomicSymlink("canopy.bin", link); err != nil {
		t.Fatalf("symlink with missing parent: %v", err)
	}
	if got, _ := os.Readlink(link); got != "canopy.bin" {
		t.Errorf("got %q, want %q", got, "canopy.bin")
	}
}

// TestSwitchToRelease_missingBinary refuses cleanly when canopy.bin
// doesn't exist. The error must include the path AND the next step
// ("run make install on main") — users shouldn't have to think.
func TestSwitchToRelease_missingBinary(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "canopy")
	missing := filepath.Join(dir, "canopy.bin")

	var out bytes.Buffer
	err := switchToRelease(link, missing, &out)
	if err == nil {
		t.Fatal("expected error when canopy.bin is missing; got nil")
	}
	if !strings.Contains(err.Error(), "no release binary") {
		t.Errorf("error should mention 'no release binary'; got %v", err)
	}
	if !strings.Contains(err.Error(), "make install") {
		t.Errorf("error should suggest 'make install'; got %v", err)
	}
	// Symlink should NOT have been created.
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink should not exist on refusal; lstat err=%v", err)
	}
}

// TestSwitchToRelease_happyPath: with canopy.bin present, the symlink
// points at the relative target "canopy.bin" (not the absolute path)
// so moving ~/.local/bin doesn't break the link.
func TestSwitchToRelease_happyPath(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "canopy")
	real := filepath.Join(dir, "canopy.bin")
	if err := os.WriteFile(real, []byte("fake"), 0o755); err != nil {
		t.Fatalf("seed canopy.bin: %v", err)
	}

	var out bytes.Buffer
	if err := switchToRelease(link, real, &out); err != nil {
		t.Fatalf("switchToRelease: %v", err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "canopy.bin" {
		t.Errorf("symlink target: got %q, want relative %q", got, "canopy.bin")
	}
	for _, want := range []string{"Active:", "canopy.bin", "Mode:   release"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q; got:\n%s", want, out.String())
		}
	}
}

// TestSwitchToWorkspace_unknownName: an unknown workspace name returns
// an error listing the available alternatives. This is the "you typed
// it wrong" UX — the user shouldn't need to re-run `canopy use` to see
// what they could have meant. Suggestions are filtered to canopy
// source worktrees only — see TestErrUnknownWorkspace_filtersNonCanopy
// for the filter coverage.
func TestSwitchToWorkspace_unknownName(t *testing.T) {
	dir := t.TempDir()
	wtA := filepath.Join(dir, "A")
	wtB := filepath.Join(dir, "B")
	if err := os.MkdirAll(wtA, 0o755); err != nil {
		t.Fatalf("mkdir A: %v", err)
	}
	if err := os.MkdirAll(wtB, 0o755); err != nil {
		t.Fatalf("mkdir B: %v", err)
	}
	makeCanopyWorktree(t, wtA)
	makeCanopyWorktree(t, wtB)

	st := &state.State{
		Workspaces: []state.Workspace{
			{Name: "feature-A", Path: wtA},
			{Name: "feature-B", Path: wtB},
		},
	}
	err := errUnknownWorkspace("featuer-A", st) // typo
	if err == nil {
		t.Fatal("expected error for unknown name")
	}
	msg := err.Error()
	for _, want := range []string{"unknown target", "featuer-A", "feature-A", "feature-B", "release"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q; got %v", want, err)
		}
	}
}

// TestFindWorkspaceByName: linear lookup must match exactly; partial
// matches and case mismatches return nil so we never auto-pick a
// neighbor. Hits both branches (found / not found) and the nil-state
// guard.
func TestFindWorkspaceByName(t *testing.T) {
	st := &state.State{
		Workspaces: []state.Workspace{
			{Name: "feature-A", Path: "/A"},
			{Name: "feature-B", Path: "/B"},
		},
	}
	cases := []struct {
		name      string
		input     string
		wantFound bool
	}{
		{"exact", "feature-A", true},
		{"second", "feature-B", true},
		{"unknown", "feature-C", false},
		{"empty", "", false},
		{"case_mismatch", "Feature-A", false},
		{"prefix_only", "feature", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := findWorkspaceByName(st, tc.input)
			if (ws != nil) != tc.wantFound {
				t.Errorf("findWorkspaceByName(%q): got %v, wantFound=%v", tc.input, ws, tc.wantFound)
			}
		})
	}

	// Defensive: nil state must not panic.
	if got := findWorkspaceByName(nil, "anything"); got != nil {
		t.Errorf("nil state should return nil; got %v", got)
	}
}

// TestFindWorkspaceByName_BranchFallback covers the "user types the
// branch instead of the workspace slug" convenience path. Name lookup
// wins on direct match; branch lookup runs only if no name matches.
func TestFindWorkspaceByName_BranchFallback(t *testing.T) {
	st := &state.State{
		Workspaces: []state.Workspace{
			{Name: "clever-jay", Path: "/cj", Branch: "clear-workspace-identity"},
			{Name: "another-slug", Path: "/another", Branch: "feat-foo"},
			{Name: "branch-equals-name", Path: "/ben", Branch: "branch-equals-name"},
		},
	}
	cases := []struct {
		name        string
		input       string
		wantFoundAt string // workspace.Name we expect to match (or "" for nil)
	}{
		{"name match wins", "clever-jay", "clever-jay"},
		{"branch fallback", "clear-workspace-identity", "clever-jay"},
		{"second branch", "feat-foo", "another-slug"},
		{"name == branch picks via name path", "branch-equals-name", "branch-equals-name"},
		{"unknown still nil", "nope", ""},
		{"empty branch never matches", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := findWorkspaceByName(st, tc.input)
			if tc.wantFoundAt == "" {
				if ws != nil {
					t.Errorf("got %+v; want nil", ws)
				}
				return
			}
			if ws == nil || ws.Name != tc.wantFoundAt {
				t.Errorf("got %+v; want workspace named %q", ws, tc.wantFoundAt)
			}
		})
	}
}

func TestBranchLabelForUse(t *testing.T) {
	cases := []struct {
		name string
		ws   *state.Workspace
		want string
	}{
		{"nil workspace", nil, "—"},
		{"no branch set", &state.Workspace{Name: "x"}, "—"},
		{"branch == name (dedupe)", &state.Workspace{Name: "x", Branch: "x"}, "—"},
		{"meaningful branch", &state.Workspace{Name: "clever-jay", Branch: "clear-workspace-identity"}, "clear-workspace-identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := branchLabelForUse(tc.ws); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

// makeCanopyWorktree creates a fake canopy source worktree at dir.
// Adds cmd/canopy/main.go so isCanopyWorktree returns true. Test
// helper used by every test that needs a "this is a canopy worktree"
// state row.
func makeCanopyWorktree(t *testing.T, dir string) {
	t.Helper()
	canopyDir := filepath.Join(dir, "cmd", "canopy")
	if err := os.MkdirAll(canopyDir, 0o755); err != nil {
		t.Fatalf("mkdir cmd/canopy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(canopyDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
}

// TestSwitchToWorkspace_missingDevBin: workspace exists in state but
// ./canopy hasn't been built. Refuse with a message that suggests both
// `make build` AND `canopy use --build`.
func TestSwitchToWorkspace_missingDevBin(t *testing.T) {
	dir := t.TempDir()
	worktree := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeCanopyWorktree(t, worktree)
	canopyHome := filepath.Join(dir, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir canopy home: %v", err)
	}
	t.Setenv("HOME", dir)

	store, _ := state.NewStore(canopyHome)
	if err := store.Save(&state.State{
		Workspaces: []state.Workspace{
			{Name: "feature-X", Path: worktree},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	link := filepath.Join(dir, ".local", "bin", "canopy")
	var out bytes.Buffer
	err := switchToWorkspace(context.Background(), "feature-X", false, link, &out)
	if err == nil {
		t.Fatal("expected error when ./canopy missing")
	}
	for _, want := range []string{"no dev binary", "make build", "canopy use --build feature-X"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q; got %v", want, err)
		}
	}
	// Symlink should NOT exist.
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink should not be created on failure")
	}
}

// TestIsCanopyWorktree covers the source-worktree detection: present
// when cmd/canopy/main.go exists, absent otherwise. Defensive empty-
// path guard returns false.
func TestIsCanopyWorktree(t *testing.T) {
	dir := t.TempDir()

	// Empty path → false (defensive).
	if isCanopyWorktree("") {
		t.Error("empty path should return false")
	}

	// Plain dir, no cmd/canopy/main.go → false.
	plain := filepath.Join(dir, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if isCanopyWorktree(plain) {
		t.Error("plain dir without cmd/canopy/main.go should return false")
	}

	// Has cmd/canopy/main.go → true.
	canopy := filepath.Join(dir, "canopy")
	if err := os.MkdirAll(canopy, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeCanopyWorktree(t, canopy)
	if !isCanopyWorktree(canopy) {
		t.Error("canopy worktree with cmd/canopy/main.go should return true")
	}
}

// TestSwitchToWorkspace_notACanopyWorktree refuses with a clear error
// when the user tries to `canopy use` a workspace that exists but
// isn't a canopy source worktree (e.g., a Rails workspace in cravd).
// The error message must explain WHY (not a canopy source worktree)
// and point at the no-args listing for valid alternatives.
func TestSwitchToWorkspace_notACanopyWorktree(t *testing.T) {
	dir := t.TempDir()
	rails := filepath.Join(dir, "rails-worktree")
	if err := os.MkdirAll(rails, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Note: no cmd/canopy/main.go — this is a non-canopy worktree.

	canopyHome := filepath.Join(dir, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("HOME", dir)

	store, _ := state.NewStore(canopyHome)
	if err := store.Save(&state.State{
		Workspaces: []state.Workspace{
			{Name: "crisp-badger", Path: rails, ProjectRoot: "/tmp/cravd"},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	link := filepath.Join(dir, ".local", "bin", "canopy")
	var out bytes.Buffer
	err := switchToWorkspace(context.Background(), "crisp-badger", false, link, &out)
	if err == nil {
		t.Fatal("expected refusal for non-canopy worktree")
	}
	for _, want := range []string{"isn't a canopy source worktree", "Project: cravd", "github.com/avinashjoshi/canopy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q; got %v", want, err)
		}
	}
	// Symlink must NOT have been created.
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink should not be created on refusal")
	}
}

// TestPrintUseList_filtersNonCanopyWorktrees: the listing must skip
// rows from other projects. The footer must mention how many were
// skipped so the user knows the listing is intentionally scoped.
func TestPrintUseList_filtersNonCanopyWorktrees(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	binReal := filepath.Join(binDir, "canopy.bin")
	if err := os.WriteFile(binReal, []byte("rel"), 0o755); err != nil {
		t.Fatalf("seed canopy.bin: %v", err)
	}
	link := filepath.Join(binDir, "canopy")
	if err := os.Symlink("canopy.bin", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	canopyHome := filepath.Join(dir, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("HOME", dir)

	canopyWS := filepath.Join(dir, "canopy-ws")
	if err := os.MkdirAll(canopyWS, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeCanopyWorktree(t, canopyWS)

	railsWS := filepath.Join(dir, "rails-ws")
	if err := os.MkdirAll(railsWS, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// rails-ws deliberately has no cmd/canopy/main.go — should be filtered.

	store, _ := state.NewStore(canopyHome)
	if err := store.Save(&state.State{
		Workspaces: []state.Workspace{
			{Name: "smooth-fawn", Path: canopyWS, ProjectRoot: "/tmp/canopy"},
			{Name: "crisp-badger", Path: railsWS, ProjectRoot: "/tmp/cravd"},
			{Name: "fierce-salmon", Path: railsWS, ProjectRoot: "/tmp/cravd"},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	var out bytes.Buffer
	if err := printUseList(context.Background(), &out, link, binReal); err != nil {
		t.Fatalf("printUseList: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "smooth-fawn") {
		t.Errorf("canopy worktree should appear; got:\n%s", got)
	}
	if strings.Contains(got, "crisp-badger") || strings.Contains(got, "fierce-salmon") {
		t.Errorf("non-canopy worktrees should be filtered; got:\n%s", got)
	}
	if !strings.Contains(got, "2 workspace(s) from other projects skipped") {
		t.Errorf("footer should mention 2 skipped; got:\n%s", got)
	}
}

// TestErrUnknownWorkspace_filtersNonCanopy: typo recovery suggestion
// list must NOT include cravd workspaces. Pointing the user at a
// workspace they can't actually use is worse than not suggesting
// anything.
func TestErrUnknownWorkspace_filtersNonCanopy(t *testing.T) {
	dir := t.TempDir()
	canopyWS := filepath.Join(dir, "canopy-ws")
	if err := os.MkdirAll(canopyWS, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeCanopyWorktree(t, canopyWS)

	railsWS := filepath.Join(dir, "rails-ws")
	if err := os.MkdirAll(railsWS, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	st := &state.State{
		Workspaces: []state.Workspace{
			{Name: "smooth-fawn", Path: canopyWS},
			{Name: "crisp-badger", Path: railsWS},
		},
	}
	err := errUnknownWorkspace("smoothfawn", st)
	msg := err.Error()
	if !strings.Contains(msg, "smooth-fawn") {
		t.Errorf("canopy worktree must appear in suggestions; got %v", err)
	}
	if strings.Contains(msg, "crisp-badger") {
		t.Errorf("non-canopy worktree must NOT appear in suggestions; got %v", err)
	}
	if !strings.Contains(msg, "release") {
		t.Errorf("release alias must appear in suggestions; got %v", err)
	}
}

// TestSwitchToWorkspace_happyPath: workspace exists, ./canopy built →
// symlink points at the absolute dev binary path.
func TestSwitchToWorkspace_happyPath(t *testing.T) {
	dir := t.TempDir()
	worktree := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeCanopyWorktree(t, worktree)
	devBin := filepath.Join(worktree, "canopy")
	if err := os.WriteFile(devBin, []byte("fake"), 0o755); err != nil {
		t.Fatalf("seed dev binary: %v", err)
	}
	canopyHome := filepath.Join(dir, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir canopy home: %v", err)
	}
	t.Setenv("HOME", dir)

	store, _ := state.NewStore(canopyHome)
	if err := store.Save(&state.State{
		Workspaces: []state.Workspace{
			{Name: "feature-X", Path: worktree},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	link := filepath.Join(dir, ".local", "bin", "canopy")
	var out bytes.Buffer
	if err := switchToWorkspace(context.Background(), "feature-X", false, link, &out); err != nil {
		t.Fatalf("switchToWorkspace: %v", err)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != devBin {
		t.Errorf("symlink target: got %q, want %q", target, devBin)
	}
	if !strings.Contains(out.String(), "Mode:   DEV") {
		t.Errorf("output missing 'Mode:   DEV'; got:\n%s", out.String())
	}
}

// TestSwitchToWorkspace_buildFlag exercises --build. Stubs the build
// function so the test stays unit-scoped (no real go-toolchain fork).
// The goBuildInWorktree var pattern is the testability seam.
func TestSwitchToWorkspace_buildFlag(t *testing.T) {
	prev := goBuildInWorktree
	t.Cleanup(func() { goBuildInWorktree = prev })

	buildCalls := 0
	dir := t.TempDir()
	worktree := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeCanopyWorktree(t, worktree)
	devBin := filepath.Join(worktree, "canopy")

	// Stub: when "build" is invoked, write the binary file. This
	// simulates a real go build without running go.
	goBuildInWorktree = func(ctx context.Context, d string) error {
		if d != worktree {
			t.Errorf("build called with wrong dir: got %q, want %q", d, worktree)
		}
		buildCalls++
		return os.WriteFile(devBin, []byte("fake-built"), 0o755)
	}

	canopyHome := filepath.Join(dir, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir canopy home: %v", err)
	}
	t.Setenv("HOME", dir)
	store, _ := state.NewStore(canopyHome)
	if err := store.Save(&state.State{
		Workspaces: []state.Workspace{
			{Name: "feature-X", Path: worktree},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	link := filepath.Join(dir, ".local", "bin", "canopy")
	var out bytes.Buffer
	if err := switchToWorkspace(context.Background(), "feature-X", true, link, &out); err != nil {
		t.Fatalf("switchToWorkspace --build: %v", err)
	}
	if buildCalls != 1 {
		t.Errorf("expected 1 build call, got %d", buildCalls)
	}
	if !strings.Contains(out.String(), "Building canopy") {
		t.Errorf("output missing 'Building canopy' progress; got:\n%s", out.String())
	}
	if got, _ := os.Readlink(link); got != devBin {
		t.Errorf("symlink target: got %q, want %q", got, devBin)
	}
}

// TestPrintUseList_lists releases and workspaces, with built-status.
// Covers: active line, header, release row with timestamp, workspace
// rows with built-or-not, sorted alphabetically.
func TestPrintUseList(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	// Seed canopy.bin with mtime ~2h ago so "built 2h ago" surfaces.
	binReal := filepath.Join(binDir, "canopy.bin")
	if err := os.WriteFile(binReal, []byte("rel"), 0o755); err != nil {
		t.Fatalf("seed canopy.bin: %v", err)
	}
	twoHoursAgo := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(binReal, twoHoursAgo, twoHoursAgo); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	link := filepath.Join(binDir, "canopy")
	if err := os.Symlink("canopy.bin", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	canopyHome := filepath.Join(dir, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir canopy home: %v", err)
	}
	t.Setenv("HOME", dir)

	// One workspace built (./canopy exists), one not built.
	// Both must look like canopy source worktrees so the filter
	// doesn't drop them — that's tested separately in
	// TestPrintUseList_filtersNonCanopyWorktrees.
	wtA := filepath.Join(dir, "wsA")
	wtB := filepath.Join(dir, "wsB")
	if err := os.MkdirAll(wtA, 0o755); err != nil {
		t.Fatalf("mkdir A: %v", err)
	}
	if err := os.MkdirAll(wtB, 0o755); err != nil {
		t.Fatalf("mkdir B: %v", err)
	}
	makeCanopyWorktree(t, wtA)
	makeCanopyWorktree(t, wtB)
	if err := os.WriteFile(filepath.Join(wtA, "canopy"), []byte("dev"), 0o755); err != nil {
		t.Fatalf("seed A canopy: %v", err)
	}

	store, _ := state.NewStore(canopyHome)
	if err := store.Save(&state.State{
		Workspaces: []state.Workspace{
			// Saved out of alphabetical order to verify sort.
			{Name: "feature-B", Path: wtB},
			{Name: "feature-A", Path: wtA},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	var out bytes.Buffer
	if err := printUseList(context.Background(), &out, link, binReal); err != nil {
		t.Fatalf("printUseList: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Active:",
		"-> canopy.bin",
		"Available targets:",
		"release",
		"feature-A",
		"feature-B",
		"(not built)", // wsB has no ./canopy
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}

	// Alphabetical: feature-A appears before feature-B.
	idxA := strings.Index(got, "feature-A")
	idxB := strings.Index(got, "feature-B")
	if idxA == -1 || idxB == -1 || idxA > idxB {
		t.Errorf("workspaces not sorted alphabetically; A@%d B@%d:\n%s", idxA, idxB, got)
	}

	// New columns shape: BRANCH replaces PATH.
	if !strings.Contains(got, "BRANCH") {
		t.Errorf("BRANCH column missing from header:\n%s", got)
	}
	// Tip line so users discover branch-name lookup.
	if !strings.Contains(got, "OR its branch") {
		t.Errorf("missing tip explaining branch-name lookup:\n%s", got)
	}
}

// TestBuiltAgo covers all four bucket boundaries (just-now, minutes,
// hours, days) plus the "(not built)" path. The user's status display
// reads this directly, so each bucket is a user-visible string.
func TestBuiltAgo(t *testing.T) {
	dir := t.TempDir()

	// not built
	if got := builtAgo(filepath.Join(dir, "missing")); got != "(not built)" {
		t.Errorf("missing file: got %q, want %q", got, "(not built)")
	}

	cases := []struct {
		name       string
		offset     time.Duration
		wantPrefix string
	}{
		{"just_now", -10 * time.Second, "built just now"},
		{"minutes", -5 * time.Minute, "built 5m ago"},
		{"hours", -3 * time.Hour, "built 3h ago"},
		{"days", -49 * time.Hour, "built 2d ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			ts := time.Now().Add(tc.offset)
			if err := os.Chtimes(path, ts, ts); err != nil {
				t.Fatalf("chtimes: %v", err)
			}
			got := builtAgo(path)
			if got != tc.wantPrefix {
				t.Errorf("got %q, want %q", got, tc.wantPrefix)
			}
		})
	}
}

// TestParseVersionLine covers the four shapes of `canopy version` first
// lines we expect, plus the malformed-input fallback. Drives both the
// release row's display string and the future stability of the column
// — if formatVersionDetails ever drops the "canopy " prefix this test
// fails loudly.
func TestParseVersionLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"release_full", "canopy v0.12.2+abc1234\n  binary: ...", "v0.12.2+abc1234"},
		{"dev_label", "canopy DEV\n  workspace: feature-A", "DEV"},
		{"single_line_no_newline", "canopy v0.12.2+abc1234", "v0.12.2+abc1234"},
		{"empty", "", ""},
		{"missing_prefix", "Canopy v0.12.2", ""},
		{"only_prefix", "canopy ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVersionLine(tc.in); got != tc.want {
				t.Errorf("parseVersionLine(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDevVersionLabel: built dev binary → "DEV", missing → "—".
// Both branches matter for the canopy use listing.
func TestDevVersionLabel(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent")
	if got := devVersionLabel(missing); got != "—" {
		t.Errorf("missing devBin: got %q, want %q", got, "—")
	}

	present := filepath.Join(dir, "canopy")
	if err := os.WriteFile(present, []byte("dev"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := devVersionLabel(present); got != "DEV" {
		t.Errorf("present devBin: got %q, want %q", got, "DEV")
	}
}

// TestPrintUseList_versionColumn verifies the listing surfaces the
// VERSION column in both the header and each row. Stubs
// releaseVersionLabel so the test doesn't fork a real binary.
func TestPrintUseList_versionColumn(t *testing.T) {
	prev := releaseVersionLabel
	t.Cleanup(func() { releaseVersionLabel = prev })
	releaseVersionLabel = func(ctx context.Context, binPath string) string {
		return "v9.9.9+test123"
	}

	dir := t.TempDir()
	binDir := filepath.Join(dir, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	binReal := filepath.Join(binDir, "canopy.bin")
	if err := os.WriteFile(binReal, []byte("rel"), 0o755); err != nil {
		t.Fatalf("seed canopy.bin: %v", err)
	}
	link := filepath.Join(binDir, "canopy")
	if err := os.Symlink("canopy.bin", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	canopyHome := filepath.Join(dir, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("HOME", dir)

	// One workspace built (./canopy exists → "DEV"), one not built
	// (./canopy missing → "—").
	wtBuilt := filepath.Join(dir, "ws-built")
	wtUnbuilt := filepath.Join(dir, "ws-unbuilt")
	for _, p := range []string{wtBuilt, wtUnbuilt} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		makeCanopyWorktree(t, p)
	}
	if err := os.WriteFile(filepath.Join(wtBuilt, "canopy"), []byte("dev"), 0o755); err != nil {
		t.Fatalf("seed dev bin: %v", err)
	}

	store, _ := state.NewStore(canopyHome)
	if err := store.Save(&state.State{
		Workspaces: []state.Workspace{
			{Name: "ws-built", Path: wtBuilt},
			{Name: "ws-unbuilt", Path: wtUnbuilt},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	var out bytes.Buffer
	if err := printUseList(context.Background(), &out, link, binReal); err != nil {
		t.Fatalf("printUseList: %v", err)
	}
	got := out.String()
	for _, want := range []string{"VERSION", "v9.9.9+test123", "DEV", "—"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

// TestReleaseVersionLabel_missingBinary: stat fails → returns "—". This
// is the "no canopy.bin yet" path that hits a fresh install where the
// release symlink isn't populated.
func TestReleaseVersionLabel_missingBinary(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "canopy.bin")
	if got := releaseVersionLabel(context.Background(), missing); got != "—" {
		t.Errorf("missing binary: got %q, want %q", got, "—")
	}
}

// TestHelpVersionLine covers all three branches of the --help banner:
// release version, dev with workspace, dev without workspace.
func TestHelpVersionLine(t *testing.T) {
	cases := []struct {
		name string
		d    VersionDetails
		want string
	}{
		{
			"release",
			VersionDetails{Version: "v0.12.2+abc1234", IsDev: false},
			"canopy v0.12.2+abc1234",
		},
		{
			"dev_with_workspace",
			VersionDetails{Version: "dev", IsDev: true, DevWorkspace: "calm-firefly"},
			"canopy DEV (calm-firefly)",
		},
		{
			"dev_untracked",
			VersionDetails{Version: "dev", IsDev: true},
			"canopy DEV",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := helpVersionLine(tc.d); got != tc.want {
				t.Errorf("helpVersionLine: got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunUse_buildWithoutTarget: --build without a workspace argument
// is a usage error. The flag only makes sense paired with a target.
func TestRunUse_buildWithoutTarget(t *testing.T) {
	cmd := newUseCmd()
	cmd.SetArgs([]string{"--build"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --build with no target")
	}
	if !strings.Contains(err.Error(), "requires a workspace target") {
		t.Errorf("error should explain --build needs a target; got %v", err)
	}
}

// TestRunUse_buildWithRelease: --build release is also a usage error;
// you don't rebuild the released binary via this path. (Use `make
// install` on main for that.)
func TestRunUse_buildWithRelease(t *testing.T) {
	cmd := newUseCmd()
	cmd.SetArgs([]string{"--build", "release"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --build release")
	}
	if !strings.Contains(err.Error(), "only valid with a workspace target") {
		t.Errorf("error should refuse --build release; got %v", err)
	}
}

// setupUseHome wires a $HOME for a use-flow test: builds ~/.local/bin
// with a canopy.bin + symlink, plus a ~/.canopy state store. Returns
// the symlinkPath and releaseTargetPath, which mirror canopyBinDir()
// resolution against the temp HOME. Used by the TTY-routing tests
// below.
func setupUseHome(t *testing.T) (symlinkPath, releaseTargetPath string) {
	t.Helper()
	dir := t.TempDir()
	binDir := filepath.Join(dir, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	releaseTargetPath = filepath.Join(binDir, "canopy.bin")
	if err := os.WriteFile(releaseTargetPath, []byte("rel"), 0o755); err != nil {
		t.Fatalf("seed canopy.bin: %v", err)
	}
	symlinkPath = filepath.Join(binDir, "canopy")
	if err := os.Symlink("canopy.bin", symlinkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	canopyHome := filepath.Join(dir, ".canopy")
	if err := os.MkdirAll(canopyHome, 0o755); err != nil {
		t.Fatalf("mkdir canopy home: %v", err)
	}
	t.Setenv("HOME", dir)
	return symlinkPath, releaseTargetPath
}

// stubPicker captures invocations so tests can assert the picker was
// or wasn't called and inspect the rows handed in. Returns a restore
// func suitable for `defer`.
type pickerCall struct {
	called          bool
	rowsTargets     []string
	activeText      string
	returnTarget    string
	returnBuild     bool
	returnErr       error
}

func stubPicker(t *testing.T, ret string, retBuild bool, retErr error) *pickerCall {
	t.Helper()
	pc := &pickerCall{returnTarget: ret, returnBuild: retBuild, returnErr: retErr}
	prev := runUsePickerFn
	runUsePickerFn = func(rows []ui.UseRow, activeText string) (string, bool, error) {
		pc.called = true
		pc.activeText = activeText
		pc.rowsTargets = make([]string, 0, len(rows))
		for _, r := range rows {
			pc.rowsTargets = append(pc.rowsTargets, r.Target)
		}
		return pc.returnTarget, pc.returnBuild, pc.returnErr
	}
	t.Cleanup(func() { runUsePickerFn = prev })
	return pc
}

// stubUseIsTerminal flips the TTY-detection branch. defer-restored.
func stubUseIsTerminal(t *testing.T, isTTY bool) {
	t.Helper()
	prev := useIsTerminal
	useIsTerminal = func(*os.File) bool { return isTTY }
	t.Cleanup(func() { useIsTerminal = prev })
}

// TestRunUse_TTYBranch_LaunchesPicker: stdin is a tty, no args, no
// --list → picker fires. Picker returns "release" → caller switches
// to release. End-to-end routing assertion.
func TestRunUse_TTYBranch_LaunchesPicker(t *testing.T) {
	setupUseHome(t)
	stubUseIsTerminal(t, true)
	pc := stubPicker(t, "release", false, nil)

	cmd := newUseCmd()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !pc.called {
		t.Fatal("picker was not invoked on TTY + no-args")
	}
	if !strings.Contains(out.String(), "Mode:   release") {
		t.Errorf("expected release switch output; got:\n%s", out.String())
	}
}

// TestRunUse_NotTTY_FallsBackToList: piped/non-TTY stdin keeps the
// existing tabular behavior. Locks the "scripts and CI keep working"
// contract that the picker is gated behind.
func TestRunUse_NotTTY_FallsBackToList(t *testing.T) {
	setupUseHome(t)
	stubUseIsTerminal(t, false)
	pc := stubPicker(t, "", false, nil)

	cmd := newUseCmd()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if pc.called {
		t.Error("picker should not be invoked when stdin is not a TTY")
	}
	for _, want := range []string{"Active:", "Available targets:", "TARGET", "release"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("tabular output missing %q; got:\n%s", want, out.String())
		}
	}
}

// TestRunUse_ListFlag_BypassesPickerOnTTY: --list forces tabular
// output even on an interactive terminal. The documented escape
// hatch for screen recordings, debugging, or just preference.
func TestRunUse_ListFlag_BypassesPickerOnTTY(t *testing.T) {
	setupUseHome(t)
	stubUseIsTerminal(t, true)
	pc := stubPicker(t, "release", false, nil)

	cmd := newUseCmd()
	cmd.SetArgs([]string{"--list"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if pc.called {
		t.Error("--list should bypass the picker, but it was invoked")
	}
	if !strings.Contains(out.String(), "Available targets:") {
		t.Errorf("expected tabular output with --list; got:\n%s", out.String())
	}
}

// TestRunUse_PickerCancel_NoSwitch_Exit0: picker returns ("", false,
// nil) → silent exit 0, no error, no switch. Ensures Esc/q/^c on the
// picker behaves like every other "press q" dismissal in canopy.
func TestRunUse_PickerCancel_NoSwitch_Exit0(t *testing.T) {
	symlinkPath, _ := setupUseHome(t)
	stubUseIsTerminal(t, true)
	pc := stubPicker(t, "", false, nil)

	// Capture the symlink target before — must be unchanged after.
	pre, err := os.Readlink(symlinkPath)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}

	cmd := newUseCmd()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute on cancel should be nil err; got %v", err)
	}
	if !pc.called {
		t.Error("picker should have been called")
	}
	if out.String() != "" {
		t.Errorf("cancel path should be silent; got:\n%s", out.String())
	}
	post, _ := os.Readlink(symlinkPath)
	if post != pre {
		t.Errorf("symlink changed on cancel: pre=%q post=%q", pre, post)
	}
}

// TestRunUse_PickerError_Propagates: a Bubbletea program failure
// flows up through RunE → cobra exit 1 with a useful error.
func TestRunUse_PickerError_Propagates(t *testing.T) {
	setupUseHome(t)
	stubUseIsTerminal(t, true)
	stubPicker(t, "", false, errors.New("tea borked"))

	cmd := newUseCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from failing picker")
	}
	if !strings.Contains(err.Error(), "tea borked") {
		t.Errorf("error chain should preserve picker err; got %v", err)
	}
}

// TestRunUse_PickerWorkspaceWithBuild_CallsSwitchWithBuild: picker
// returns a workspace name and withBuild=true → switchToWorkspace
// runs with build=true. Asserted indirectly via the build hook stub:
// goBuildInWorktree fires (means build=true path was taken).
func TestRunUse_PickerWorkspaceWithBuild_CallsSwitchWithBuild(t *testing.T) {
	symlinkPath, _ := setupUseHome(t)
	stubUseIsTerminal(t, true)

	// Seed a canopy worktree with a dev binary so switchToWorkspace
	// will succeed past the --build stage.
	wsDir := filepath.Join(filepath.Dir(filepath.Dir(symlinkPath)), "..", "ws")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	makeCanopyWorktree(t, wsDir)
	devBin := filepath.Join(wsDir, "canopy")
	if err := os.WriteFile(devBin, []byte("dev"), 0o755); err != nil {
		t.Fatalf("seed devbin: %v", err)
	}

	canopyHome := filepath.Join(filepath.Dir(filepath.Dir(symlinkPath)), "..", ".canopy")
	store, _ := state.NewStore(canopyHome)
	if err := store.Save(&state.State{
		Workspaces: []state.Workspace{{Name: "feature-A", Path: wsDir}},
	}); err != nil {
		t.Fatalf("save state: %v", err)
	}

	stubPicker(t, "feature-A", true, nil)

	// Stub the build hook so we can detect it was called.
	buildCalled := false
	prevBuild := goBuildInWorktree
	t.Cleanup(func() { goBuildInWorktree = prevBuild })
	goBuildInWorktree = func(ctx context.Context, dir string) error {
		buildCalled = true
		return nil
	}

	cmd := newUseCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !buildCalled {
		t.Error("withBuild=true from picker should have triggered goBuildInWorktree")
	}
}

// TestUseRows_ActiveFlag_TracksSymlink: useRows reads the current
// symlink target and marks the matching row Active=true. This is the
// data driving the ▶ marker in the picker.
func TestUseRows_ActiveFlag_TracksSymlink(t *testing.T) {
	symlinkPath, releaseTargetPath := setupUseHome(t)
	rows := useRows(context.Background(), symlinkPath, releaseTargetPath)
	if len(rows) == 0 {
		t.Fatal("useRows returned no rows")
	}
	if !rows[0].IsRelease {
		t.Fatalf("first row should be release; got %q", rows[0].Target)
	}
	if !rows[0].Active {
		t.Error("release row should be Active=true; symlink points at canopy.bin")
	}
}

// TestUseRows_HasBinary_ReflectsDisk: HasBinary flag tracks whether
// BinaryPath exists. Workspace rows with missing ./canopy must report
// HasBinary=false so the picker can mute them.
func TestUseRows_HasBinary_ReflectsDisk(t *testing.T) {
	symlinkPath, releaseTargetPath := setupUseHome(t)

	// Add a workspace with NO ./canopy.
	homeDir := filepath.Dir(filepath.Dir(filepath.Dir(symlinkPath)))
	wsDir := filepath.Join(homeDir, "ws-nobin")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	makeCanopyWorktree(t, wsDir)

	canopyHome := filepath.Join(homeDir, ".canopy")
	store, _ := state.NewStore(canopyHome)
	if err := store.Save(&state.State{
		Workspaces: []state.Workspace{{Name: "ws-nobin", Path: wsDir}},
	}); err != nil {
		t.Fatalf("save state: %v", err)
	}

	rows := useRows(context.Background(), symlinkPath, releaseTargetPath)
	var wsRow *ui.UseRow
	for i := range rows {
		if rows[i].Target == "ws-nobin" {
			wsRow = &rows[i]
			break
		}
	}
	if wsRow == nil {
		t.Fatalf("ws-nobin row missing; rows: %+v", rows)
	}
	if wsRow.HasBinary {
		t.Errorf("ws-nobin has no ./canopy; HasBinary should be false")
	}
}
