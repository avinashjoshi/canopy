package clog

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsSafeWorkspaceName_Accepts: typical canopy workspace names
// (already sanitized by git.Sanitize at creation) pass.
func TestIsSafeWorkspaceName_Accepts(t *testing.T) {
	for _, name := range []string{
		"misty-marsh",
		"bold-falcon",
		"feat-oauth",
		"v1-2-3",
		"abc",
		"_underscore-ok",
		"UPPERCASE-also-ok",
	} {
		if !isSafeWorkspaceName(name) {
			t.Errorf("isSafeWorkspaceName(%q) = false; want true", name)
		}
	}
}

// TestIsSafeWorkspaceName_Rejects: anything that could traverse the
// filesystem or otherwise smuggle path segments must be rejected.
// Belt-and-suspenders — workspace names are already sanitized at
// creation, but slog records can carry `name` from anywhere.
func TestIsSafeWorkspaceName_Rejects(t *testing.T) {
	for _, name := range []string{
		"",
		".",
		"..",
		"../etc/passwd",
		"foo/bar",
		"foo\\bar",
		"foo.log",
		"with spaces",
		"with\x00null",
		strings.Repeat("a", 65), // exceeds 64-char cap
	} {
		if isSafeWorkspaceName(name) {
			t.Errorf("isSafeWorkspaceName(%q) = true; want false", name)
		}
	}
}

// TestFanout_RecordWithName_WritesBoth: a record carrying a `name`
// attribute lands in both the inner handler's stream AND a per-
// workspace file. This is the load-bearing claim of the fan-out.
func TestFanout_RecordWithName_WritesBoth(t *testing.T) {
	dir := t.TempDir()
	var globalBuf bytes.Buffer
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	inner := slog.NewJSONHandler(&globalBuf, &slog.HandlerOptions{Level: level})
	h := NewFanoutHandler(inner, dir, level)
	defer h.Close()

	logger := slog.New(h)
	logger.Info("workspace.event", "name", "misty-marsh", "x", 1)

	// Global stream: contains the record.
	if !strings.Contains(globalBuf.String(), "workspace.event") {
		t.Errorf("global stream missing event; got %q", globalBuf.String())
	}

	// Per-workspace file: created at canopy-misty-marsh.log with the same record.
	wsPath := filepath.Join(dir, "canopy-misty-marsh.log")
	wsData, err := os.ReadFile(wsPath)
	if err != nil {
		t.Fatalf("expected per-workspace log at %s: %v", wsPath, err)
	}
	if !strings.Contains(string(wsData), "workspace.event") {
		t.Errorf("per-workspace log missing event; got %q", wsData)
	}
}

// TestFanout_RecordWithoutName_GlobalOnly: a record with no `name`
// attribute writes only to the global stream — no stray per-workspace
// files for events that aren't workspace-scoped.
func TestFanout_RecordWithoutName_GlobalOnly(t *testing.T) {
	dir := t.TempDir()
	var globalBuf bytes.Buffer
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	h := NewFanoutHandler(slog.NewJSONHandler(&globalBuf, &slog.HandlerOptions{Level: level}), dir, level)
	defer h.Close()

	slog.New(h).Info("startup", "version", "v0.9.0")

	if !strings.Contains(globalBuf.String(), "startup") {
		t.Error("global stream missing event")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "canopy-") {
			t.Errorf("unexpected per-workspace file %q created for record without name attr", e.Name())
		}
	}
}

// TestFanout_UnsafeName_NoFile: a record whose `name` would be a path-
// traversal must NOT create a file. Defense-in-depth: the workspace
// name was sanitized at creation, but log records can carry `name`
// from anywhere.
func TestFanout_UnsafeName_NoFile(t *testing.T) {
	dir := t.TempDir()
	var globalBuf bytes.Buffer
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	h := NewFanoutHandler(slog.NewJSONHandler(&globalBuf, &slog.HandlerOptions{Level: level}), dir, level)
	defer h.Close()

	slog.New(h).Info("event", "name", "../etc/passwd")

	// Global stream: still gets the record.
	if !strings.Contains(globalBuf.String(), "event") {
		t.Error("global stream missing event for unsafe-name record")
	}
	// No file with that name (or anywhere outside dir).
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("unexpected file %q created from unsafe-name record", e.Name())
	}
	// Defense-in-depth: also check parent dir didn't get touched.
	if _, err := os.Stat(filepath.Join(dir, "..", "etc")); err == nil {
		t.Error("path-traversal succeeded — fanout wrote outside dir")
	}
}

// TestFanout_WithAttrs: WithAttrs returns a new handler whose records
// (in BOTH inner and per-workspace streams) carry the added attributes.
// Important: canopy's per-package loggers use `slog.With("pkg", ...)`
// at module init time — without WithAttrs propagation, per-workspace
// logs would lose the pkg tag.
func TestFanout_WithAttrs(t *testing.T) {
	dir := t.TempDir()
	var globalBuf bytes.Buffer
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	h := NewFanoutHandler(slog.NewJSONHandler(&globalBuf, &slog.HandlerOptions{Level: level}), dir, level)
	defer h.Close()

	logger := slog.New(h.WithAttrs([]slog.Attr{slog.String("pkg", "tmux")}))
	logger.Info("session.create", "name", "test-ws")

	// Inner: pkg attribute present.
	if !strings.Contains(globalBuf.String(), `"pkg":"tmux"`) {
		t.Errorf("global stream missing pkg attr; got %q", globalBuf.String())
	}
	// Per-workspace: same attribute present.
	wsData, err := os.ReadFile(filepath.Join(dir, "canopy-test-ws.log"))
	if err != nil {
		t.Fatalf("read per-workspace log: %v", err)
	}
	if !strings.Contains(string(wsData), `"pkg":"tmux"`) {
		t.Errorf("per-workspace log missing pkg attr; got %q", wsData)
	}
}

// TestFanout_HandleRespectsContext: the inner handler's context-aware
// behavior (filtering, propagation) is preserved by the wrapper.
func TestFanout_HandleRespectsContext(t *testing.T) {
	dir := t.TempDir()
	var globalBuf bytes.Buffer
	level := new(slog.LevelVar)
	level.Set(slog.LevelWarn) // INFO records get filtered
	h := NewFanoutHandler(slog.NewJSONHandler(&globalBuf, &slog.HandlerOptions{Level: level}), dir, level)
	defer h.Close()

	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(LevelInfo) = true; want false (level set to Warn)")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("Enabled(LevelWarn) = false; want true")
	}
}

// TestRemoveWorkspaceLog_Missing_NoError: removing a workspace's log
// when the file doesn't exist is a no-op (canopy rm runs unconditionally
// regardless of whether the workspace ever logged).
func TestRemoveWorkspaceLog_Missing_NoError(t *testing.T) {
	if err := RemoveWorkspaceLog("nonexistent-ws-12345"); err != nil {
		t.Errorf("RemoveWorkspaceLog on missing file = %v; want nil", err)
	}
}

// TestRemoveWorkspaceLog_RejectsUnsafeName: defense-in-depth.
func TestRemoveWorkspaceLog_RejectsUnsafeName(t *testing.T) {
	if err := RemoveWorkspaceLog("../etc/passwd"); err == nil {
		t.Error("RemoveWorkspaceLog with unsafe name = nil; want error")
	}
}
