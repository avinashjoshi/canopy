package clog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"
)

// fanoutHandler wraps the global slog.Handler and additionally tees
// records carrying a `name` attribute to a per-workspace log file at
// ~/.canopy/log/canopy-<name>.log. The fan-out is opt-in by attribute:
// only records explicitly tagged with `name` create or write to a
// per-workspace file.
//
// Why fan out via slog.Handler rather than a separate logger
// per workspace: every package in canopy already calls
// slog.With("pkg", "<name>").Info("event", "name", w.Name, ...) — there's
// no central object that knows when a workspace is "active." Routing in
// the handler means existing log calls keep working unchanged; the file
// fan-out happens transparently. Adding a workspace logger object
// would force every call site to plumb it through.
//
// Concurrency: per-workspace writers are created lazily in writers and
// guarded by mu. slog.Handler.Handle is allowed to be called from
// multiple goroutines per the slog contract.
type fanoutHandler struct {
	// inner is the global handler that always receives every record.
	// In practice this is the JSON handler over ~/.canopy/log/canopy.log.
	inner slog.Handler

	// dir is the directory per-workspace log files live in (typically
	// ~/.canopy/log). Captured at construction so we don't re-resolve
	// $HOME on every record.
	dir string

	// level is shared with the global handler so the per-workspace
	// handlers respect the same INFO/DEBUG toggle. Pointer so the
	// runtime SetDebug flip propagates without re-wiring.
	level *slog.LevelVar

	// sinks is shared by pointer across the entire handler tree.
	// WithAttrs/WithGroup create derived handlers but they all see the
	// same writers map, so there's exactly ONE lumberjack.Logger per
	// workspace name regardless of which derived handler opens it.
	// Without this, two slog.With() calls for the same workspace would
	// open two lumberjacks at the same path → rotation race + fd leak.
	sinks *sinkRegistry

	// preAttrs are accumulated via WithAttrs so the per-workspace
	// handler created lazily inherits the same pkg/etc tags the inner
	// handler already has. slog.Handler.WithAttrs returns a NEW handler;
	// the fan-out needs to mirror that into the per-workspace path.
	preAttrs []slog.Attr
	preGroup string
}

// sinkRegistry holds the per-workspace writers shared across all
// derived fanoutHandler instances (WithAttrs/WithGroup clones). One
// lumberjack per workspace name, period.
type sinkRegistry struct {
	mu      sync.Mutex
	writers map[string]*workspaceSink
}

// workspaceSink is a per-workspace writer + handler pair. Cached in
// fanoutHandler.writers so we don't open/close a file on every record.
type workspaceSink struct {
	writer  *lumberjack.Logger
	handler slog.Handler
}

// NewFanoutHandler wraps the inner handler with per-workspace fan-out.
// Records that carry a `name` attribute matching a filename-safe
// workspace name are tee'd to ~/.canopy/log/canopy-<name>.log.
//
// dir is the directory the per-workspace logs live in (created lazily
// per workspace). level should be the same LevelVar the inner handler
// uses so debug-toggle state stays consistent across files.
func NewFanoutHandler(inner slog.Handler, dir string, level *slog.LevelVar) *fanoutHandler {
	return &fanoutHandler{
		inner: inner,
		dir:   dir,
		level: level,
		sinks: &sinkRegistry{writers: make(map[string]*workspaceSink)},
	}
}

// Enabled defers to the inner handler. The per-workspace handlers share
// the same LevelVar so a record enabled for the inner is enabled for
// the fan-out too.
func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle writes the record to the inner handler, then if the record
// carries a `name` attribute, ALSO writes it to that workspace's log.
//
// Errors from the inner handler are returned (they're load-bearing —
// dropping a log to canopy.log is a real problem worth surfacing).
// Errors from the per-workspace handler are logged-and-swallowed via
// the inner handler — fan-out is a best-effort enhancement, not a
// correctness gate.
func (h *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	if err := h.inner.Handle(ctx, r); err != nil {
		return err
	}
	name := extractName(r)
	if name == "" {
		return nil
	}
	if !isSafeWorkspaceName(name) {
		return nil
	}
	sink, err := h.sinkFor(name)
	if err != nil {
		// Route the warning through the inner handler directly so we
		// never recurse through this fanout (slog.Warn would route
		// through Default, which IS this handler in production).
		_ = h.inner.Handle(ctx, slog.NewRecord(r.Time, slog.LevelWarn, "clog.fanout.sink-failed", 0))
		return nil
	}
	// Apply this derived handler's accumulated attrs/group on top of
	// the cached base handler. Doing it per-call lets the cache key
	// stay "name only" — multiple slog.With() chains for the same
	// workspace all hit the same lumberjack writer.
	target := sink.handler
	for _, g := range []string{h.preGroup} {
		if g != "" {
			target = target.WithGroup(g)
		}
	}
	if len(h.preAttrs) > 0 {
		target = target.WithAttrs(h.preAttrs)
	}
	if err := target.Handle(ctx, r); err != nil {
		// Route the warning through the inner handler directly so we
		// never recurse through this fanout (which would happen if
		// the warning carried a `name` attribute matching a workspace).
		_ = h.inner.Handle(ctx, slog.NewRecord(r.Time, slog.LevelWarn, "clog.fanout.handle-failed", 0))
	}
	return nil
}

// WithAttrs returns a new handler with the given attrs added to every
// record. The derived handler shares the SAME sinks registry (by
// pointer) as the parent so all clones agree on which lumberjack.Logger
// owns each workspace's file — opening two writers at the same path
// would race on rotation and leak fds.
func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &fanoutHandler{
		inner:    h.inner.WithAttrs(attrs),
		dir:      h.dir,
		level:    h.level,
		sinks:    h.sinks, // shared registry — DO NOT clone
		preAttrs: append(append([]slog.Attr(nil), h.preAttrs...), attrs...),
		preGroup: h.preGroup,
	}
}

// WithGroup returns a new handler that opens a group on every record.
// Same shared-sinks contract as WithAttrs.
func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &fanoutHandler{
		inner:    h.inner.WithGroup(name),
		dir:      h.dir,
		level:    h.level,
		sinks:    h.sinks, // shared registry — DO NOT clone
		preAttrs: append([]slog.Attr(nil), h.preAttrs...),
		preGroup: name,
	}
}

// Close flushes and closes every per-workspace writer in the shared
// sinks registry. Called by the teardown closure returned from Init.
// Safe to call multiple times.
func (h *fanoutHandler) Close() {
	h.sinks.mu.Lock()
	defer h.sinks.mu.Unlock()
	for _, sink := range h.sinks.writers {
		_ = sink.writer.Close()
	}
	h.sinks.writers = make(map[string]*workspaceSink)
}

// sinkFor returns the cached per-workspace sink for name, creating
// it on first access. The shared sinks registry guarantees a single
// lumberjack per workspace name — derived handlers (WithAttrs clones)
// see the same writer; the per-derived attrs are applied to the
// returned record (in Handle) rather than baked into the cached
// handler so the cache key stays "name only."
func (h *fanoutHandler) sinkFor(name string) (*workspaceSink, error) {
	h.sinks.mu.Lock()
	defer h.sinks.mu.Unlock()
	if sink, ok := h.sinks.writers[name]; ok {
		return sink, nil
	}
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", h.dir, err)
	}
	writer := &lumberjack.Logger{
		Filename:   filepath.Join(h.dir, "canopy-"+name+".log"),
		MaxSize:    maxSizeMB,
		MaxBackups: maxBackups,
		MaxAge:     maxAgeDays,
		Compress:   true,
	}
	// Cached handler is the BASE JSON handler. Per-derived attrs and
	// group are applied per-record in Handle, NOT baked into the
	// cached object — that way the cache lookup is "name only" and
	// a slog.With() chain doesn't fragment the registry.
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: h.level})
	sink := &workspaceSink{writer: writer, handler: handler}
	h.sinks.writers[name] = sink
	return sink, nil
}

// extractName scans the record's top-level attrs for a key="name"
// string and returns its value. Returns "" if no name attribute exists
// or it's the wrong type. Walks attrs once — slog.Record.Attrs is the
// canonical visitor pattern.
func extractName(r slog.Record) string {
	var name string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "name" && a.Value.Kind() == slog.KindString {
			name = a.Value.String()
			return false
		}
		return true
	})
	return name
}

// isSafeWorkspaceName guards the file path against path-traversal and
// other foot-guns. Workspace names are already sanitized at creation
// (git.Sanitize collapses unsafe characters), but slog records carrying
// `name` aren't necessarily workspace names — could be a tmux session
// name, a branch name, etc. Belt-and-suspenders check.
func isSafeWorkspaceName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	if strings.ContainsAny(name, "/\\.\x00") {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		safe := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-'
		if !safe {
			return false
		}
	}
	return true
}

// RemoveWorkspaceLog deletes the per-workspace log file (and any
// rotated backups) for the named workspace. Called by `canopy rm` so
// the log file doesn't outlive the workspace it belongs to.
//
// Best-effort: missing files are not errors. Other I/O errors are
// returned so the caller can log them but should not fail the rm.
func RemoveWorkspaceLog(name string) error {
	if !isSafeWorkspaceName(name) {
		return fmt.Errorf("clog.RemoveWorkspaceLog: unsafe name %q", name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("clog.RemoveWorkspaceLog: home dir: %w", err)
	}
	dir := filepath.Join(home, ".canopy", "log")
	prefix := "canopy-" + name + ".log"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("clog.RemoveWorkspaceLog: read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Match the active log file plus any rotated files
		// (canopy-<name>.log, canopy-<name>.log-<timestamp>.gz, etc.).
		n := entry.Name()
		if n != prefix && !strings.HasPrefix(n, prefix+"-") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			return fmt.Errorf("clog.RemoveWorkspaceLog: remove %s: %w", n, err)
		}
	}
	return nil
}
