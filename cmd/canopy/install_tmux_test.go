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
	if !strings.Contains(out, "bind g run-shell") {
		t.Errorf("new block content missing: %s", out)
	}
	if !strings.Contains(out, "set -g mouse on") {
		t.Errorf("user content before block lost: %s", out)
	}
	if !strings.Contains(out, "set -g status-bg blue") {
		t.Errorf("user content after block lost: %s", out)
	}
}

// TestCanopyBlockBody_popupBindOnly: the generated block must include
// the popup bind (g) and MUST NOT bind 'r' (collides with rename-window
// in many configs) or 'D' (the canopy-run binding was dropped pending
// design exploration of the right shape).
func TestCanopyBlockBody_popupBindOnly(t *testing.T) {
	body := canopyBlockBody()

	if !strings.Contains(body, "bind g run-shell") {
		t.Errorf("block missing 'bind g run-shell' (canopy popup keybind):\n%s", body)
	}
	if !strings.Contains(body, " popup") {
		t.Errorf("block missing 'popup' subcommand reference:\n%s", body)
	}
	// Defensive: the block must NOT bind lowercase r (collides with
	// tmux's common rename-window binding). Catches the regression
	// where v0.7.1 first shipped lowercase r and broke users' configs.
	if strings.Contains(body, "bind r ") {
		t.Errorf("block must NOT bind 'r' (collides with rename-window):\n%s", body)
	}
	// canopy run keybind is intentionally NOT in the default block —
	// the user wants to revisit shape (popup vs send-keys vs pane) in
	// a separate PR. Block must NOT bind D for now.
	if strings.Contains(body, "bind D ") {
		t.Errorf("block must NOT bind 'D' (canopy run keybind deferred):\n%s", body)
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
	if !strings.Contains(body, "bind g run-shell") {
		t.Errorf("written file missing popup keybind:\n%s", body)
	}
	// Binary path is resolved at install time from os.Executable() so
	// the path embedded varies per test run (it's the test harness's
	// path). Match on the subcommand names instead.
	if !strings.Contains(body, " popup\"") {
		t.Errorf("written file missing 'popup' subcommand reference:\n%s", body)
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
	if !strings.Contains(body, " popup\"") {
		t.Errorf("new block missing after --force: %s", body)
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
