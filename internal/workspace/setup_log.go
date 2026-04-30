// Per-workspace setup-script log capture. Setup output is the most
// load-bearing diagnostic when a workspace fails: it shows exactly
// which command in scripts.setup failed and what it printed before
// dying. Capturing it to disk per-workspace lets the inspect drawer
// (`i` in the TUI) show it without forcing the user to scroll back
// through a global log.
//
// Files live at ~/.canopy/log/setup-<name>.log. Removed by `canopy rm`.

package workspace

import (
	"io"
	"os"
	"path/filepath"
)

// openSetupLog opens (truncating) ~/.canopy/log/setup-<name>.log for
// the given workspace. Returns the file as an io.Writer for use in
// io.MultiWriter, plus a close function safe to defer. Either return
// can be nil — callers must check both before using the writer.
//
// Why truncate, not append: each setup run is a complete attempt to
// build the workspace; the previous attempt's output is rarely useful
// once a new run starts. Truncate keeps the file small and matches the
// "last setup output" framing in the drawer. If we want history later,
// adding a rotation policy here is straightforward.
//
// Errors are silent: setup-log capture is a quality-of-life feature,
// not a correctness one. A failed open logs once via the global slog
// but does not block the setup hook from running. The hook still
// streams to the original stdout/stderr the caller passed.
func openSetupLog(name string) (io.Writer, func()) {
	if name == "" {
		return nil, func() {}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Warn("setup-log.no-home", "name", name, "err", err.Error())
		return nil, func() {}
	}
	dir := filepath.Join(home, ".canopy", "log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warn("setup-log.mkdir-failed", "name", name, "dir", dir, "err", err.Error())
		return nil, func() {}
	}
	path := filepath.Join(dir, "setup-"+name+".log")
	f, err := os.Create(path) // truncate any prior run
	if err != nil {
		log.Warn("setup-log.open-failed", "name", name, "path", path, "err", err.Error())
		return nil, func() {}
	}
	return f, func() { _ = f.Close() }
}

// removeSetupLog deletes ~/.canopy/log/setup-<name>.log. Called by
// Manager.Remove so the log doesn't outlive the workspace it belongs
// to. Best-effort: missing file is not an error.
func removeSetupLog(name string) {
	if name == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".canopy", "log", "setup-"+name+".log")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Warn("setup-log.remove-failed", "name", name, "path", path, "err", err.Error())
	}
}
