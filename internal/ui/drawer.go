// Diagnostic detail drawer. Opened with `i` from the list page; scope-
// capped to read-only diagnostics. See the CEO plan at
// ~/.gstack/projects/canopy/ceo-plans/2026-04-29-tmux-health-and-resurrect.md
// for the load-bearing scope cap rationale: this is a drawer, not a
// dashboard. No editing, no live tailing, no canopy.json mutation.
//
// Three data sources, loaded in one tea.Cmd that returns a single
// drawerLoadedMsg:
//
//   1. Process tree per pane (tmux list-panes + ps recursive walk)
//   2. Last 20 lines of the workspace's per-workspace log
//   3. Last setup-script output
//
// Errors per source are non-fatal. Each source falls back to a clean
// inline message ("no log captured", "ps failed: ...") rather than
// failing the whole drawer.

package ui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// actionInspect opens the diagnostic detail drawer for the cursor row.
// Works for both workspace rows AND main rows — the process tree, env,
// and tmux state are all useful regardless. Main rows skip the
// scripts.setup log section (main doesn't run setup) and disable the
// `b` bare-attach action (nothing to skip past).
func actionInspect(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	row, ok := m.list.CursorRow()
	if !ok || row.Loading {
		return m, nil
	}
	// v0.17.0: inspect probes the LOCAL tmux server for process tree,
	// logs, etc. Remote workspaces would need a parallel "remote
	// inspect" path that SSHes the probes. Not yet implemented.
	// Surface the limitation rather than show empty drawer panes.
	if row.Host != "" {
		m.err = fmt.Errorf(
			"remote inspect isn't supported yet — workspace lives on %s. SSH there and run `canopy inspect %s`, or attach (enter) and look around in tmux.",
			row.Host, row.Name)
		return m, nil
	}
	if row.TmuxSession == "" {
		// Defensive: every row should have a session name, but if a
		// future row type lacks one (e.g. a placeholder row) the
		// drawer can't probe anything useful.
		m.err = fmt.Errorf("inspect (i) needs a tmux session name; this row has none")
		return m, nil
	}
	m.mode = drawerMode
	m.drawerRow = row
	m.drawerProcInfo = ""
	m.drawerLogTail = ""
	m.drawerSetupLog = ""
	m.drawerErr = nil
	return m, drawerLoadCmd(m.tc, row)
}

// handleDrawerKey is the keymap while the inspect drawer is open.
//
//   - Esc / q       close drawer, refresh list
//   - r             reload drawer data
//   - b             bare attach (no scripts.setup rerun); subsumes the
//                   v0.5 `canopy debug` TODO. The detail view is the
//                   natural launch surface — when staring at a broken
//                   workspace, you want to drop into its dir to poke.
//   - anything else consumed (don't bleed into listMode bindings)
func (m *Model) handleDrawerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = listMode
		m.drawerRow = Row{}
		m.drawerProcInfo = ""
		m.drawerLogTail = ""
		m.drawerSetupLog = ""
		m.drawerErr = nil
		return m, m.refresh()
	case "r":
		m.drawerProcInfo = ""
		m.drawerLogTail = ""
		m.drawerSetupLog = ""
		m.drawerErr = nil
		return m, drawerLoadCmd(m.tc, m.drawerRow)
	case "b":
		// Bare attach: drop into a one-pane shell at the row's dir
		// with CANOPY_* env vars set, no auto-running claude/nvim.
		// Main rows take the project-root path; workspace rows take
		// the worktree path. Both share the "-debug" session-name
		// suffix so they don't collide with the row's main session.
		mgr, err := m.managerForRow(m.drawerRow)
		if err != nil {
			m.drawerErr = err
			return m, nil
		}
		if m.drawerRow.IsMain {
			return m, bareAttachMainCmd(mgr)
		}
		return m, bareAttachCmd(mgr, m.drawerRow)
	}
	return m, nil
}

// bareAttachMainCmd is the main-row counterpart of bareAttachCmd.
// Calls Manager.BareAttachMain (no name lookup) instead of BareAttach.
func bareAttachMainCmd(mgr *workspace.Manager) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		session, err := mgr.BareAttachMain(ctx)
		if err != nil {
			return drawerActionMsg{err: fmt.Errorf("bare attach main: %w", err)}
		}
		return drawerAttachAfterMsg{session: session, name: "(main)"}
	}
}

// drawerLoadedMsg carries the diagnostic data loaded for the open
// drawer back to Update. forName + forRoot guard against a stale
// load message arriving after the user closed and re-opened the
// drawer on a different row — both fields are required because
// workspace names are unique within a project but NOT across
// projects (the global TUI shows rows from many projects).
type drawerLoadedMsg struct {
	forName  string
	forRoot  string
	procInfo string
	logTail  string
	setupLog string
	err      error
}

// paneInspector is the slice of *tmux.Client drawerLoadCmd needs.
// Decoupled as an interface so unit tests can substitute a fake without
// spinning up a real tmux server.
type paneInspector interface {
	PaneInfos(ctx context.Context, session string) ([]tmux.PaneInfo, error)
}

// drawerLoadCmd loads the drawer's three data sources sequentially in
// one tea.Cmd. Sequential because the data sources are tiny (process
// tree: ~5ms, file reads: <1ms each); parallel would add complexity
// without measurable gain at this scale.
func drawerLoadCmd(tc paneInspector, row Row) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		msg := drawerLoadedMsg{forName: row.Name, forRoot: row.ProjectRoot}

		if row.Alive && row.TmuxSession != "" {
			infos, err := tc.PaneInfos(ctx, row.TmuxSession)
			if err != nil {
				msg.procInfo = fmt.Sprintf("  (process tree unavailable: %v)\n", err)
			} else {
				msg.procInfo = renderPaneInfos(infos)
			}
		} else {
			msg.procInfo = "  (no live tmux session — press Enter on the row to resurrect)\n"
		}

		// Per-workspace log + setup log are workspace-only: clog's
		// fan-out keys on the `name` attribute, which workspace
		// lifecycle code sets to the workspace name. Main rows don't
		// log with that key, so there's no per-main log file. The
		// drawer's log section just shows a hint for main rows.
		if !row.IsMain {
			if tail, err := tailFile(workspaceLogPath(row.Name), 20); err == nil {
				msg.logTail = tail
			} else if !os.IsNotExist(err) {
				msg.logTail = fmt.Sprintf("  (log read failed: %v)\n", err)
			}

			if data, err := readBoundedFile(setupLogPath(row.Name), 8*1024); err == nil {
				msg.setupLog = string(data)
			} else if !os.IsNotExist(err) {
				msg.setupLog = fmt.Sprintf("  (setup log read failed: %v)\n", err)
			}
		}

		return msg
	}
}

// renderPaneInfos formats a tmux session's PaneInfos into the drawer's
// process tree section. One block per pane: header line with index +
// title + total RSS, then each process indented under it.
func renderPaneInfos(infos []tmux.PaneInfo) string {
	if len(infos) == 0 {
		return "  (no panes)\n"
	}
	var b strings.Builder
	for _, p := range infos {
		title := p.Title
		if title == "" {
			title = "(no title)"
		}
		fmt.Fprintf(&b, "  pane %d  %s   total: %s\n", p.Index, title, humanBytes(p.TotalRSS))
		for _, proc := range p.Tree {
			fmt.Fprintf(&b, "    %5d  %5s%%  %8s  %s\n",
				proc.PID, proc.CPU, humanBytes(proc.RSS), proc.Comm)
		}
	}
	return b.String()
}

// humanBytes renders a byte count as KB/MB/GB. The drawer needs to
// scan-friendly numbers, not exact byte counts.
func humanBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1fG", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%dM", n/mb)
	case n >= kb:
		return fmt.Sprintf("%dK", n/kb)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// workspaceLogPath returns the per-workspace log path created by the
// clog fan-out handler. ~/.canopy/log/canopy-<name>.log.
func workspaceLogPath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".canopy", "log", "canopy-"+name+".log")
}

// setupLogPath returns the per-workspace setup-script log path written
// by the lifecycle's setup tee. ~/.canopy/log/setup-<name>.log.
func setupLogPath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".canopy", "log", "setup-"+name+".log")
}

// tailFile returns the last n lines of the file at path. Returns the
// underlying os error untouched — caller decides whether to suppress
// IsNotExist (no log captured yet) or surface (read permission denied).
//
// Implementation: bufio.Scanner ring buffer. Bounded memory regardless
// of file size; ~20 lines is well under any reasonable line length, so
// total bytes is small.
func tailFile(path string, n int) (string, error) {
	if path == "" {
		return "", os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	// Larger buffer for JSON log lines that can exceed default 64KB
	// when a slog record carries a long error message.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(ring) < n {
			ring = append(ring, sc.Text())
		} else {
			copy(ring, ring[1:])
			ring[n-1] = sc.Text()
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return strings.Join(ring, "\n") + "\n", nil
}

// readBoundedFile reads at most max bytes from path. Returns the same
// shape as os.ReadFile but without unbounded memory growth — setup
// logs across a long-running project can be megabytes.
func readBoundedFile(path string, max int64) ([]byte, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// If the file is larger than max, seek to (size - max) and read
	// the tail. Truncating the head is the right call — a long-running
	// setup is more likely to have its useful failure context near the
	// end.
	if stat.Size() > max {
		if _, err := f.Seek(stat.Size()-max, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}

// bareAttachCmd dispatches a tea.ExecProcess that drops the user into
// the workspace's dir without re-running scripts.setup. Subsumes the
// v0.5 `canopy debug` TODO. After detach, refreshCmd reloads the list.
//
// In popup mode, switch-client semantics don't apply here (we're
// launching a fresh tmux session, not switching to an existing one),
// so we just refresh.
func bareAttachCmd(mgr *workspace.Manager, row Row) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		session, err := mgr.BareAttach(ctx, row.Name)
		if err != nil {
			return drawerActionMsg{err: fmt.Errorf("bare attach %s: %w", row.Name, err)}
		}
		return drawerAttachAfterMsg{session: session, name: row.Name}
	}
}

// drawerActionMsg surfaces the result of a drawer-initiated action
// (currently bare attach) when there's nothing more to do — the message
// just reports the error (or success with no follow-on).
type drawerActionMsg struct {
	err error
}

// drawerAttachAfterMsg is the bridge between bareAttachCmd's async
// session-creation and the synchronous tea.ExecProcess attach. Update
// catches it and dispatches the actual attach.
type drawerAttachAfterMsg struct {
	session string
	name    string
}
