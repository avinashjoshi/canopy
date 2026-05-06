package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDetectCanopyBlock covers the four-state classifier driving
// idempotency: absent / present / multiple / malformed.
func TestDetectCanopyBlock(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want canopyBlockState
	}{
		{"empty", "", canopyBlockAbsent},
		{
			"user_only",
			"set -g mouse on\nbind r source-file ~/.tmux.conf\n",
			canopyBlockAbsent,
		},
		{
			"single_well_formed",
			"set -g mouse on\n\n# canopy:start (managed)\nbind g run-shell foo\n# canopy:end\n",
			canopyBlockPresent,
		},
		{
			"two_starts",
			"# canopy:start (a)\n# canopy:end\n# canopy:start (b)\n# canopy:end\n",
			canopyBlockMultiple,
		},
		{
			"start_no_end",
			"# canopy:start\nbind g run-shell foo\n",
			canopyBlockMalformed,
		},
		{
			"end_no_start",
			"bind g run-shell foo\n# canopy:end\n",
			canopyBlockMalformed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectCanopyBlock(tc.in); got != tc.want {
				t.Errorf("detectCanopyBlock: got %d, want %d\ninput:\n%s",
					got, tc.want, tc.in)
			}
		})
	}
}

// TestApplyCanopyBlock_appendsToEmpty: empty input → just the block + nl.
func TestApplyCanopyBlock_appendsToEmpty(t *testing.T) {
	out := applyCanopyBlock("", canopyBlockBody())
	if !strings.HasPrefix(out, tmuxConfMarkerStart) {
		t.Errorf("empty input: result missing start marker at top:\n%s", out)
	}
	if !strings.Contains(out, tmuxConfMarkerEnd) {
		t.Errorf("empty input: result missing end marker:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("empty input: result must end with newline")
	}
}

// TestApplyCanopyBlock_appendsAfterExisting: user content stays intact,
// block separated by a blank line.
func TestApplyCanopyBlock_appendsAfterExisting(t *testing.T) {
	existing := "set -g mouse on\nbind r source-file ~/.tmux.conf\n"
	out := applyCanopyBlock(existing, canopyBlockBody())

	if !strings.HasPrefix(out, "set -g mouse on") {
		t.Errorf("user content lost; result starts: %.40q", out)
	}
	if !strings.Contains(out, tmuxConfMarkerStart) {
		t.Errorf("block missing start marker")
	}
	// Verify exactly one blank line between user content and our block.
	idx := strings.Index(out, tmuxConfMarkerStart)
	prefix := out[:idx]
	if !strings.HasSuffix(prefix, "\n\n") {
		t.Errorf("expected blank line before canopy block; prefix tail: %q",
			prefix[max(0, len(prefix)-10):])
	}
}

// TestApplyCanopyBlock_replacesExisting: existing block gets replaced
// in place; surrounding content preserved.
func TestApplyCanopyBlock_replacesExisting(t *testing.T) {
	existing := "set -g mouse on\n\n" +
		tmuxConfMarkerStart + "\nold-bind\n" + tmuxConfMarkerEnd + "\n\n" +
		"set -g status-bg blue\n"

	out := applyCanopyBlock(existing, canopyBlockBody())

	if strings.Contains(out, "old-bind") {
		t.Errorf("old block content survived replacement: %s", out)
	}
	if !strings.Contains(out, "bind g display-popup") {
		t.Errorf("new block content missing display-popup bind: %s", out)
	}
	if !strings.Contains(out, "set -g mouse on") {
		t.Errorf("user content before block lost: %s", out)
	}
	if !strings.Contains(out, "set -g status-bg blue") {
		t.Errorf("user content after block lost: %s", out)
	}
}

// TestCanopyBlockBody_popupBindShape: the generated block must include
// the v0.8 unified-TUI popup bind (display-popup -E with -d
// "#{pane_current_path}" and CANOPY_IN_POPUP=1 in env), and MUST NOT
// bind 'r' (collides with rename-window in many configs) or 'D' (the
// canopy-run binding was dropped pending design exploration). Also
// REGRESSION-CRIT: must NOT bind via the legacy "popup" subcommand,
// which is gone after unification.
func TestCanopyBlockBody_popupBindShape(t *testing.T) {
	body := canopyBlockBody()

	if !strings.Contains(body, "bind g display-popup") {
		t.Errorf("block missing 'bind g display-popup' (v0.8 unified TUI keybind):\n%s", body)
	}
	if !strings.Contains(body, `-d "#{pane_current_path}"`) {
		t.Errorf("block missing -d \"#{pane_current_path}\" (load-bearing for Local-tab cwd resolution):\n%s", body)
	}
	if !strings.Contains(body, "CANOPY_IN_POPUP=1") {
		t.Errorf("block missing CANOPY_IN_POPUP=1 env (popup-mode rendering toggle):\n%s", body)
	}
	if strings.Contains(body, "popup-inner") {
		t.Errorf("block references deleted popup-inner subcommand:\n%s", body)
	}
	if strings.Contains(body, `bind g run-shell`) {
		t.Errorf("block uses legacy run-shell shape; should be display-popup -E:\n%s", body)
	}
	if strings.Contains(body, "bind r ") {
		t.Errorf("block must NOT bind 'r' (collides with rename-window):\n%s", body)
	}
	if strings.Contains(body, "bind D ") {
		t.Errorf("block must NOT bind 'D' (canopy run keybind deferred):\n%s", body)
	}
}

// TestCanopyBlockBody_setsTitles asserts the block enables tmux's
// terminal-title forwarding (set-titles on) with #S as the title source.
// This is the load-bearing wire-up for "Ghostty tab strip shows the
// current branch": tmux emits OSC 0 with the session name, and canopy
// renames sessions to follow the branch via Manager.SyncBranch.
//
// Without these two lines, the rename pipeline updates everything EXCEPT
// the terminal tab — degraded experience even though the underlying
// session rename works.
func TestCanopyBlockBody_setsTitles(t *testing.T) {
	body := canopyBlockBody()

	if !strings.Contains(body, "set -g set-titles on") {
		t.Errorf("block missing 'set -g set-titles on' (terminal title forwarding):\n%s", body)
	}
	if !strings.Contains(body, "set -g set-titles-string '#S'") {
		t.Errorf("block missing 'set -g set-titles-string \\'#S\\'' (title source = session name):\n%s", body)
	}
}

// TestCanopyBlockBody_bareBinaryNoAbsolutePath asserts the block embeds
// bare `canopy` (PATH-resolved at tmux runtime) and never an absolute
// path. With `canopy use` swapping the ~/.local/bin/canopy symlink
// between release and dev binaries, an absolute path baked into the
// block would force `canopy install tmux --force` after every swap —
// which is exactly the pain this design eliminates.
//
// REGRESSION GUARD: the prior implementation resolved os.Executable()
// and wrote that path into the block. Any future change that brings
// path-baking back must also update the design doc and explicitly
// document why; this test fails first.
func TestCanopyBlockBody_bareBinaryNoAbsolutePath(t *testing.T) {
	body := canopyBlockBody()

	// The popup keybind line must end with `canopy"` (bare invocation),
	// not `/path/to/canopy"` or `'/path/to/canopy'"`.
	if !strings.Contains(body, `CANOPY_IN_POPUP=1 canopy"`) {
		t.Errorf("popup keybind missing bare `canopy` (expected `CANOPY_IN_POPUP=1 canopy\"`):\n%s", body)
	}

	// The statusline `set -ag` line must reference bare `canopy` too.
	if !strings.Contains(body, "#(canopy statusline --format=current)") {
		t.Errorf("statusline segment missing bare `canopy` (expected `#(canopy statusline ...)`):\n%s", body)
	}

	// Defense-in-depth: no absolute paths anywhere in the block. tmux
	// configs sometimes contain `/path/...` for unrelated reasons, so we
	// can't blanket-forbid all slashes — but `/canopy` followed by a
	// quote or end-of-line is the unambiguous signature of the prior
	// path-baking bug.
	for _, sig := range []string{`/canopy"`, `/canopy '`, `/canopy)`} {
		if strings.Contains(body, sig) {
			t.Errorf("block contains absolute-path canopy invocation %q (path-baking regression):\n%s", sig, body)
		}
	}
}

// TestCanopyBinForBlock asserts the helper returns bare `canopy` always.
// Belt-and-suspenders for the regression test above: if someone changes
// canopyBinForBlock to return os.Executable() again, this fires before
// the integration test does.
func TestCanopyBinForBlock(t *testing.T) {
	if got := canopyBinForBlock(); got != "canopy" {
		t.Errorf("canopyBinForBlock() = %q; want %q", got, "canopy")
	}
}

// TestApplyCanopyBlock_idempotent: applying twice produces same output.
// IRON RULE for install: re-runs must be safe.
func TestApplyCanopyBlock_idempotent(t *testing.T) {
	existing := "set -g mouse on\n"
	once := applyCanopyBlock(existing, canopyBlockBody())
	twice := applyCanopyBlock(once, canopyBlockBody())
	if once != twice {
		t.Errorf("apply twice changed output:\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}

// TestRunInstallTmux_freshFile: no existing tmux.conf → write one with
// the canopy block; no backup file (nothing to back up).
func TestRunInstallTmux_freshFile(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	cmd := newInstallTmuxCmd()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	confPath := filepath.Join(fakeHome, ".tmux.conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read %s: %v", confPath, err)
	}
	body := string(data)
	if !strings.Contains(body, tmuxConfMarkerStart) {
		t.Errorf("written file missing start marker:\n%s", body)
	}
	if !strings.Contains(body, "bind g display-popup") {
		t.Errorf("written file missing display-popup keybind:\n%s", body)
	}
	if !strings.Contains(body, "CANOPY_IN_POPUP=1") {
		t.Errorf("written file missing CANOPY_IN_POPUP=1 env:\n%s", body)
	}
	// Block invokes bare `canopy` (PATH-resolved) so it follows the
	// ~/.local/bin/canopy symlink across `canopy use` swaps.
	if !strings.Contains(body, `CANOPY_IN_POPUP=1 canopy"`) {
		t.Errorf("written file missing bare `canopy` invocation in popup keybind:\n%s", body)
	}
	if !strings.Contains(body, " statusline --format=current") {
		t.Errorf("written file missing 'statusline' subcommand reference:\n%s", body)
	}

	// No backup expected on a fresh file.
	matches, _ := filepath.Glob(confPath + ".canopy-backup-*")
	if len(matches) != 0 {
		t.Errorf("unexpected backup files on fresh install: %v", matches)
	}
}

// TestRunInstallTmux_existingFileNoBlock: pre-existing tmux.conf without
// canopy block → user content preserved, block appended, backup created.
func TestRunInstallTmux_existingFileNoBlock(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	confPath := filepath.Join(fakeHome, ".tmux.conf")
	original := "set -g mouse on\nset -g history-limit 50000\n"
	if err := os.WriteFile(confPath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed conf: %v", err)
	}

	cmd := newInstallTmuxCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// New conf has both user content and canopy block.
	data, _ := os.ReadFile(confPath)
	body := string(data)
	if !strings.Contains(body, "set -g mouse on") {
		t.Errorf("user content lost:\n%s", body)
	}
	if !strings.Contains(body, tmuxConfMarkerStart) {
		t.Errorf("canopy block missing:\n%s", body)
	}

	// Backup file exists with the original content.
	matches, _ := filepath.Glob(confPath + ".canopy-backup-*")
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 backup, got %d: %v", len(matches), matches)
	}
	backup, _ := os.ReadFile(matches[0])
	if string(backup) != original {
		t.Errorf("backup content mismatch:\nwant:\n%s\ngot:\n%s", original, backup)
	}
}

// TestRunInstallTmux_alreadyInstalledRefuses: re-running on a file that
// already has the canopy block (no --force) → no-op, exit 0, message says
// "already present".
//
// IRON RULE: idempotent re-run must not duplicate or clobber.
func TestRunInstallTmux_alreadyInstalledRefuses(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	confPath := filepath.Join(fakeHome, ".tmux.conf")
	preInstalled := "set -g mouse on\n\n" + canopyBlockBody() + "\n"
	if err := os.WriteFile(confPath, []byte(preInstalled), 0o644); err != nil {
		t.Fatalf("seed conf: %v", err)
	}

	cmd := newInstallTmuxCmd()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute (already-installed, no force): %v", err)
	}

	// File untouched.
	data, _ := os.ReadFile(confPath)
	if string(data) != preInstalled {
		t.Errorf("file modified despite no-force re-run:\nbefore:\n%s\nafter:\n%s",
			preInstalled, string(data))
	}

	// User-visible message points at --force.
	if !strings.Contains(out.String(), "--force") {
		t.Errorf("message missing --force hint:\n%s", out.String())
	}

	// No backup since we didn't write.
	matches, _ := filepath.Glob(confPath + ".canopy-backup-*")
	if len(matches) != 0 {
		t.Errorf("unexpected backup on no-op re-run: %v", matches)
	}
}

// TestRunInstallTmux_forceReplaces: --force on a pre-existing block
// replaces it in place; user content outside block is preserved.
func TestRunInstallTmux_forceReplaces(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	confPath := filepath.Join(fakeHome, ".tmux.conf")
	preInstalled := "set -g mouse on\n\n" +
		tmuxConfMarkerStart + " (old)\nbind g old-binding\n" + tmuxConfMarkerEnd + "\n\n" +
		"set -g status-bg blue\n"
	if err := os.WriteFile(confPath, []byte(preInstalled), 0o644); err != nil {
		t.Fatalf("seed conf: %v", err)
	}

	cmd := newInstallTmuxCmd()
	cmd.SetArgs([]string{"--force"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute --force: %v", err)
	}

	data, _ := os.ReadFile(confPath)
	body := string(data)
	if strings.Contains(body, "old-binding") {
		t.Errorf("old block content survived --force: %s", body)
	}
	if !strings.Contains(body, "bind g display-popup") {
		t.Errorf("new v0.8 unified-TUI block missing after --force: %s", body)
	}
	// Surrounding user content preserved.
	if !strings.Contains(body, "set -g mouse on") || !strings.Contains(body, "set -g status-bg blue") {
		t.Errorf("user content damaged by --force:\n%s", body)
	}
}

// TestRunInstallTmux_dryRunNoWrite: --dry-run prints planned change but
// leaves the file alone.
func TestRunInstallTmux_dryRunNoWrite(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	confPath := filepath.Join(fakeHome, ".tmux.conf")
	original := "set -g mouse on\n"
	if err := os.WriteFile(confPath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cmd := newInstallTmuxCmd()
	cmd.SetArgs([]string{"--dry-run"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute --dry-run: %v", err)
	}

	data, _ := os.ReadFile(confPath)
	if string(data) != original {
		t.Errorf("dry-run modified file:\nbefore: %q\nafter: %q", original, string(data))
	}
	if !strings.Contains(out.String(), "[dry-run]") {
		t.Errorf("dry-run output missing tag: %s", out.String())
	}
	matches, _ := filepath.Glob(confPath + ".canopy-backup-*")
	if len(matches) != 0 {
		t.Errorf("dry-run created backup: %v", matches)
	}
}

// TestShellQuote covers the POSIX-safe quoter that wraps the canopy
// binary path before it's embedded in the tmux display-popup command
// body. Bare-safe paths skip wrapping (cleaner generated config); paths
// with shell metacharacters get single-quoted with embedded single
// quotes escaped via the '\'' idiom.
func TestShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Bare-safe: no wrapping.
		{"canopy", "canopy"},
		{"/usr/local/bin/canopy", "/usr/local/bin/canopy"},
		{"/tmp/canopy-ss", "/tmp/canopy-ss"},
		{"./canopy", "./canopy"},
		{"v0.8.0+rc1", "v0.8.0+rc1"},
		// Non-bare-safe: single-quote wrap.
		{"", "''"},
		{"/path with space/canopy", "'/path with space/canopy'"},
		{"/has$dollar/canopy", "'/has$dollar/canopy'"},
		{"/has*glob/canopy", "'/has*glob/canopy'"},
		// Embedded single quote: the '\'' escape.
		{"/it's/canopy", `'/it'\''s/canopy'`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := shellQuote(tc.in); got != tc.want {
				t.Errorf("shellQuote(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsShellSafeBare covers the bare-quoting predicate. Hits both
// branches of every conditional in the alnum/punct allowlist.
func TestIsShellSafeBare(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false}, // empty is not bare-safe (we wrap to '')
		{"canopy", true},
		{"CANOPY", true},
		{"canopy123", true},
		{"/usr/bin/canopy", true},
		{"hyphen-name_under.dot+plus", true},
		// Disallowed chars.
		{"with space", false},
		{"with$dollar", false},
		{"with*glob", false},
		{"with'quote", false},
		{"with\"dquote", false},
		{"with;semi", false},
		{"with`tick", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isShellSafeBare(tc.in); got != tc.want {
				t.Errorf("isShellSafeBare(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAtomicWriteFile_basic exercises the tempfile+rename helper.
func TestAtomicWriteFile_basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	want := []byte("hello world\n")
	if err := atomicWriteFile(path, want, 0o644); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content mismatch: got %q, want %q", got, want)
	}
	// No leftover tmpfile in dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmux.conf.canopy-tmp-") {
			t.Errorf("leftover tempfile: %s", e.Name())
		}
	}
}
