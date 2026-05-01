// Package tmux wraps the subset of tmux commands canopy needs to manage
// per-workspace sessions.
//
// All operations go through a Client which optionally holds a named tmux
// socket (`tmux -L <name>`). Production code uses Client.New(), which talks
// to the user's default tmux server. Tests use Client.WithSocket("name") to
// scope to an isolated socket so they don't pollute the user's tmux state.
//
// This package is a leaf primitive: it knows how to start/stop/check tmux
// sessions, but not how canopy composes them into a 4-pane workspace
// layout. That orchestration lives in internal/workspace.
package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/avinashjoshi/canopy/internal/clog"
)

var log = clog.Pkg("tmux")

// SafeName turns an arbitrary identifier into one safe for use as a tmux
// session or window name. tmux's target syntax uses `:` and `.` as
// separators (`session:window.pane`), so neither character can appear
// unescaped in a name without breaking every subsequent target lookup.
//
// This is stricter than git.Sanitize — git allows dots in branch names
// (v1.2.3 stays v1.2.3) but tmux can't have them. Other unsafe characters
// collapse to a single `-`; alphanumerics, underscore, and hyphen pass
// through. Leading/trailing hyphens are trimmed.
//
//	SafeName("v1.2.3")            -> "v1-2-3"
//	SafeName("avi.tools")         -> "avi-tools"
//	SafeName("feature/oauth")     -> "feature-oauth"
//	SafeName("tmp.X-feat")        -> "tmp-X-feat"
func SafeName(s string) string {
	var b []byte
	prevDash := false
	for _, r := range s {
		safe := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-'
		if safe {
			b = append(b, byte(r))
			prevDash = (r == '-')
			continue
		}
		// Collapse runs of unsafe chars into a single hyphen.
		if !prevDash {
			b = append(b, '-')
			prevDash = true
		}
	}
	// Trim leading/trailing hyphens.
	start, end := 0, len(b)
	for start < end && b[start] == '-' {
		start++
	}
	for end > start && b[end-1] == '-' {
		end--
	}
	return string(b[start:end])
}

// Sentinel errors. Callers use errors.Is to distinguish "this is the
// 'doesn't exist' case" from genuine failures.
var (
	// ErrSessionExists is returned when Create is called for a session name
	// that's already alive on the server.
	ErrSessionExists = errors.New("tmux: session already exists")

	// ErrSessionNotFound is returned by Kill when the session isn't on the
	// server. Reconciliation treats this as a no-op.
	ErrSessionNotFound = errors.New("tmux: session not found")
)

// Client is a thin wrapper around the tmux CLI. The zero value is invalid;
// use New() or WithSocket() to construct one.
type Client struct {
	// socket is the tmux socket name passed via `tmux -L`. Empty means use
	// the user's default tmux server (no -L flag).
	socket string
}

// New returns a client that talks to the user's default tmux server.
func New() *Client { return &Client{} }

// WithSocket returns a client scoped to the named tmux socket. The socket
// is created on first use; tmux's own server-per-socket model means
// canopy-test sessions can't collide with the user's running tmux.
//
// Test code should always use WithSocket, never New.
func WithSocket(name string) *Client { return &Client{socket: name} }

// args prepends the -L flag if the client has a custom socket, then appends
// rest. Internal helper to keep the per-method exec.Command call sites short.
func (c *Client) args(rest ...string) []string {
	if c.socket == "" {
		return rest
	}
	out := make([]string, 0, len(rest)+2)
	out = append(out, "-L", c.socket)
	out = append(out, rest...)
	return out
}

// HasSession returns true if a session named name is alive on the server.
//
// tmux's exit codes here: 0 when the session exists, 1 when it doesn't (or
// when the server isn't running yet — both states map to "no session"
// from canopy's point of view).
func (c *Client) HasSession(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, "tmux", c.args("has-session", "-t", name)...)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("tmux.HasSession(%s): %w", name, err)
}

// Create starts a new detached tmux session named name with cwd as the
// initial working directory. If shellCmd is non-empty, the first pane runs
// "sh -c <shellCmd>" so multi-arg shell expressions work (e.g.
// "rm -rf .overmind.sock && bin/dev"); otherwise the pane runs the user's
// default shell.
//
// Returns ErrSessionExists if a session with that name is already alive.
//
// Callers that want canopy's standard 4-pane workspace layout call this
// to seed the session, then SplitPane for each additional pane. That
// orchestration lives in internal/workspace, not here.
//
// env contains "KEY=VALUE" entries that set session-level environment
// variables (via tmux's -e flag), inherited by every pane in the session,
// including future panes the user creates with prefix-c. Use this for
// CANOPY_PORT and friends so commands typed in the shell pane (like
// `bin/dev`) can read them.
func (c *Client) Create(ctx context.Context, name, cwd, shellCmd string, env ...string) error {
	log.Info("tmux.create", "name", name, "cwd", cwd, "cmd", shellCmd, "env_count", len(env))

	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return fmt.Errorf("tmux.Create(%s): %w", name, err)
	}
	if exists {
		return fmt.Errorf("tmux.Create(%s): %w", name, ErrSessionExists)
	}

	args := c.args("new-session", "-d", "-s", name, "-c", cwd)
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	if shellCmd != "" {
		// `sh -c "<expr>"` so any shell metachars (&&, |, $VAR) work.
		// Single-command cases (just "nvim") run via sh too; the extra
		// process is microseconds and not worth the API split.
		args = append(args, "sh", "-c", shellCmd)
	}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.Create(%s): %w (stderr: %s)", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SplitDirection picks how SplitPane carves up the target pane. tmux uses
// "-h" for a side-by-side (horizontal split = vertical divider line) and
// "-v" for stacked (vertical split = horizontal divider line). The naming
// has historically tripped people up; the constants below match tmux.
type SplitDirection string

const (
	// SplitHorizontal places the new pane to the RIGHT of target (vertical line).
	SplitHorizontal SplitDirection = "-h"

	// SplitVertical places the new pane BELOW target (horizontal line).
	SplitVertical SplitDirection = "-v"
)

// SplitPane creates a new pane by splitting the session's *active* pane.
// We target the session by name (`-t session`) rather than a specific
// pane index because window/pane base indices are user-configurable
// (many configs set `base-index 1`), and the orchestrator always wants
// to split the most recently created pane anyway — that's always the
// active one immediately after a previous split.
//
// cwd becomes the new pane's working directory; shellCmd is run via
// sh -c (or the default shell if empty), same semantics as Create.
//
// sizePercent is variadic for backward-compat — pass nothing for an
// even split (default 50/50), or pass a single integer 1-99 to size
// the NEW pane to that percentage of the parent. Used for the tdl-style
// layout where the bottom shell is 15% of the window and the right-side
// AI pane is 30% of the top.
//
// Layout note: chained splits produce a tree, not a balanced grid.
// SelectLayout can rearrange tiled grids, but for fixed proportional
// layouts (like tdl), use sizePercent on each split to set the geometry
// at creation time.
func (c *Client) SplitPane(ctx context.Context, session, cwd, shellCmd string, dir SplitDirection, sizePercent ...int) error {
	log.Info("tmux.split-pane", "session", session, "cwd", cwd, "cmd", shellCmd, "dir", dir)

	args := c.args("split-window", "-d", string(dir), "-t", session, "-c", cwd)
	if len(sizePercent) > 0 && sizePercent[0] > 0 && sizePercent[0] < 100 {
		// `-l <N>%` is the modern tmux size syntax (the deprecated form is
		// `-p <N>`). Sizes the NEW pane to N% of the parent pane.
		args = append(args, "-l", fmt.Sprintf("%d%%", sizePercent[0]))
	}
	if shellCmd != "" {
		args = append(args, "sh", "-c", shellCmd)
	}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.SplitPane(%s, %s): %w (stderr: %s)", session, dir, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SelectLayout applies a named tmux layout preset to the session's
// active window. The most useful preset for canopy's 4-pane workspace
// is "tiled", which arranges N panes in a clean grid regardless of
// split history. Other presets: "main-horizontal", "main-vertical",
// "even-horizontal", "even-vertical".
func (c *Client) SelectLayout(ctx context.Context, session, layout string) error {
	cmd := exec.CommandContext(ctx, "tmux", c.args("select-layout", "-t", session, layout)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.SelectLayout(%s, %s): %w (stderr: %s)", session, layout, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SelectPaneDirection moves the active-pane focus in the given
// compass direction relative to the currently-active pane. dir is one
// of "U", "D", "L", "R" (up/down/left/right).
//
// We use direction rather than absolute pane index because tmux's
// pane-base-index is user-configurable — `select-pane -t session:.2`
// would target the wrong pane on configs with `pane-base-index 1`.
// Direction-relative selection is base-index-agnostic.
//
// Errors are non-fatal at the call site: failure to select a pane
// shouldn't tear down the workspace build, just leaves the active
// pane on whatever was previously active.
func (c *Client) SelectPaneDirection(ctx context.Context, session, dir string) error {
	flag := "-" + dir
	cmd := exec.CommandContext(ctx, "tmux", c.args("select-pane", "-t", session, flag)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.SelectPaneDirection(%s, %s): %w (stderr: %s)",
			session, dir, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Kill terminates the named session. Returns ErrSessionNotFound if the
// session doesn't exist; reconciliation can ignore that.
//
// Important: also reaps the pane process tree before kill-session to
// catch processes that detach from their tmux pane on launch (notably
// `nvim --embed`, which interactive `nvim .` forks as its editor
// backend with deliberate session-detachment so it can outlive its
// launcher). Without explicit reaping, every Kill leaves one nvim
// --embed orphaned to PID 1, accumulating across `K` keypresses and
// test runs into gigabytes of zombie RAM.
//
// The reap is best-effort: enumeration failures don't block kill-
// session. Production callers (Manager.Remove) run scripts.archive
// before Kill, so workloads have already had their chance to shut
// down gracefully — the reap here is a safety net for processes that
// refused to die or never received the cascade.
func (c *Client) Kill(ctx context.Context, name string) error {
	log.Info("tmux.kill", "name", name)

	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return fmt.Errorf("tmux.Kill(%s): %w", name, err)
	}
	if !exists {
		return fmt.Errorf("tmux.Kill(%s): %w", name, ErrSessionNotFound)
	}

	// Snapshot pane PIDs BEFORE kill-session — the server going down
	// loses our enumeration handle. Errors here are non-fatal.
	pidsToReap := c.collectPanePIDs(ctx, name)

	cmd := exec.CommandContext(ctx, "tmux", c.args("kill-session", "-t", name)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux.Kill(%s): %w (stderr: %s)", name, err, strings.TrimSpace(stderr.String()))
	}

	// SIGKILL anything from the snapshot still alive. SIGKILL not
	// SIGTERM: these are processes that didn't already die from the
	// kill-session cascade, so polite signals won't help.
	for _, pid := range pidsToReap {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}

// isReapableComm returns true if /proc/<pid>/comm names a process
// known to detach from its launching tmux pane. Currently just nvim
// (whose --embed child does the deliberate detach). Add more entries
// here only when you have evidence a program leaks orphaned children
// under canopy's kill flow — gate liberally; the cwd-only filter
// already SIGKILLed canopy itself when run from a workspace pane.
func isReapableComm(pidStr string) bool {
	commBytes, err := os.ReadFile("/proc/" + pidStr + "/comm")
	if err != nil {
		return false
	}
	comm := strings.TrimSpace(string(commBytes))
	switch comm {
	case "nvim":
		return true
	}
	return false
}

// collectPanePIDs walks a session's pane tree and returns every PID
// to kill, using PaneInfos PLUS a CWD-based scan to catch processes
// that detached from their pane on launch (notably nvim --embed,
// which deliberately detaches its session so it can outlive its
// launcher).
//
// Strategy: enumerate panes, take each pane's cwd as a probe, then
// walk /proc/*/cwd to find any process — regardless of parent —
// whose working directory matches a pane cwd. The pane's normal
// process tree is also collected via PaneInfos. Union of both is
// returned. Best-effort: enumeration failures return an empty
// slice rather than an error.
func (c *Client) collectPanePIDs(ctx context.Context, session string) []int {
	infos, err := c.PaneInfos(ctx, session)
	if err != nil {
		return nil
	}
	pidSet := map[int]struct{}{}
	cwds := map[string]struct{}{}
	for _, p := range infos {
		for _, proc := range p.Tree {
			pidSet[proc.PID] = struct{}{}
		}
		// Resolve the pane's working directory so we can match
		// detached children below.
		args := c.args("display-message", "-t", session+"."+strconv.Itoa(p.Index), "-p", "#{pane_current_path}")
		out, err := exec.CommandContext(ctx, "tmux", args...).Output()
		if err == nil {
			cwd := strings.TrimSpace(string(out))
			if cwd != "" {
				cwds[cwd] = struct{}{}
			}
		}
	}

	// Scan /proc for processes whose cwd matches any pane cwd AND
	// whose comm matches a known target list. The motivating case is
	// `nvim --embed` (interactive nvim's editor backend), which
	// detaches from its pane on launch — the regular pane process
	// tree misses it.
	//
	// Image-name gating is load-bearing: a bare cwd match would
	// SIGKILL anything happening to live at the workspace path.
	// Specifically, `canopy` itself (when launched from a workspace
	// pane via the popup keybind) has cwd=workspace-path and would
	// kill itself mid-keystroke if not filtered out. Only known
	// detach-on-launch programs go on this list.
	if len(cwds) > 0 {
		entries, err := os.ReadDir("/proc")
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				pid, err := strconv.Atoi(entry.Name())
				if err != nil {
					continue
				}
				cwd, err := os.Readlink("/proc/" + entry.Name() + "/cwd")
				if err != nil {
					continue
				}
				// Strip the " (deleted)" suffix the kernel appends
				// when the cwd's underlying inode has been unlinked
				// — happens when a test's t.TempDir cleanup runs
				// before the process exits.
				cwd = strings.TrimSuffix(cwd, " (deleted)")
				if _, ok := cwds[cwd]; !ok {
					continue
				}
				if !isReapableComm(entry.Name()) {
					continue
				}
				pidSet[pid] = struct{}{}
			}
		}
	}

	out := make([]int, 0, len(pidSet))
	for pid := range pidSet {
		out = append(out, pid)
	}
	return out
}

// Attach hands the current process off to `tmux attach -t <name>`. On
// success, syscall.Exec replaces the canopy process image with tmux —
// this function never returns. When the user detaches with prefix-d,
// they end up back at their original shell, not in canopy.
//
// This is the right shape for CLI subcommands (`canopy switch`,
// `canopy new` after setup completes). The Bubbletea TUI uses
// AttachCmd instead, which returns the prepared exec.Cmd so
// tea.ExecProcess can hand off + return control after detach.
//
// On failure (session doesn't exist, tmux missing), Attach returns an
// error and does NOT exec — the canopy process stays alive and can
// surface the error to the user.
func (c *Client) Attach(ctx context.Context, name string) error {
	verb := attachVerbForCurrentEnv()
	log.Info("tmux.attach", "name", name, "verb", verb)

	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return fmt.Errorf("tmux.Attach(%s): %w", name, err)
	}
	if !exists {
		return fmt.Errorf("tmux.Attach(%s): %w", name, ErrSessionNotFound)
	}

	c.detachOtherClients(ctx, name, verb)

	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux.Attach: tmux not on PATH: %w", err)
	}

	args := []string{"tmux"}
	args = append(args, c.args(verb, "-t", name)...)
	// attach-session supports `-d` to detach other clients on the target
	// session before our client attaches. switch-client doesn't take
	// `-d` (different verb shape), so detachOtherClients above handles
	// the inside-tmux path explicitly.
	if verb == "attach" && shouldDetachOthers() {
		args = append(args, "-d")
	}
	return syscall.Exec(tmuxPath, args, os.Environ())
}

// AttachCmd returns a prepared exec.Cmd for `tmux attach -t <name>`
// without running it. The Bubbletea TUI passes this to tea.ExecProcess
// to hand the terminal to tmux temporarily; when the user detaches
// (prefix-d), tmux exits cleanly and Bubbletea reclaims the terminal
// to redraw the TUI.
//
// Pre-flight: returns ErrSessionNotFound if the session doesn't exist
// at call time. Caller should still handle exec errors from running
// the returned Cmd (terminal reset issues, tmux server crashed mid-attach).
func (c *Client) AttachCmd(ctx context.Context, name string) (*exec.Cmd, error) {
	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("tmux.AttachCmd(%s): %w", name, err)
	}
	if !exists {
		return nil, fmt.Errorf("tmux.AttachCmd(%s): %w", name, ErrSessionNotFound)
	}
	verb := attachVerbForCurrentEnv()
	c.detachOtherClients(ctx, name, verb)

	args := c.args(verb, "-t", name)
	if verb == "attach" && shouldDetachOthers() {
		args = append(args, "-d")
	}
	return exec.CommandContext(ctx, "tmux", args...), nil
}

// detachOtherClients runs `tmux detach-client -s <name>` to kick any
// clients currently attached to the target session. Solo-dev workflow
// default: only one terminal should display a workspace at a time —
// without this, switching to a workspace from a second terminal leaves
// both terminals attached to the same session, mirroring keystrokes
// and confusing the user about which is "real."
//
// The attach-session verb has a `-d` flag for the same behavior, but
// switch-client (used when canopy is invoked from inside tmux) does
// not, so we run an explicit detach-client beforehand for that path.
// We call this for both verbs for symmetry and because detach-client
// is a no-op when the target has no clients.
//
// We deliberately do NOT pass `-E` to inject a notice. The `-E`
// command only fires when the kicked-out tmux client fully exits
// (which can be much later — the TUI lives inside a pane), and the
// notice then accumulates on whatever shell prompt eventually shows.
// Worse, it can pile up across multiple detaches. Better to let tmux
// show its standard "[detached]" and call it done. The initiating
// TUI is a more natural place to surface a "took over from <client>"
// message if we want one in the future.
//
// Errors are intentionally ignored: detach-client returns non-zero on
// "no current client" and similar harmless states. Failure to detach
// shouldn't block attach. Caller's calling client (the one running
// canopy) is on a different session at this point, so it doesn't kick
// itself.
//
// Skip entirely when CANOPY_NO_DETACH=1 is set — escape hatch for
// pair-programming or "second terminal mirroring this for reference."
func (c *Client) detachOtherClients(ctx context.Context, name, verb string) {
	if !shouldDetachOthers() {
		return
	}
	args := c.args("detach-client", "-s", name)
	cmd := exec.CommandContext(ctx, "tmux", args...)
	_ = cmd.Run()
	log.Debug("tmux.detach-others", "session", name, "verb", verb)
}

// shouldDetachOthers gates the auto-detach behavior on CANOPY_NO_DETACH.
// Default is "yes, detach others"; set CANOPY_NO_DETACH=1 to opt back
// into tmux's default of allowing multiple clients per session.
func shouldDetachOthers() bool {
	return os.Getenv("CANOPY_NO_DETACH") != "1"
}

// attachVerbForCurrentEnv returns the right tmux verb for "make this
// client follow that session," chosen by whether the calling process
// is itself inside a tmux client (TMUX env set):
//
//   - Inside tmux  → "switch-client" (the calling tmux client switches
//                   to the target session). `attach` would fail with
//                   "sessions should be nested with care" or similar.
//
//   - Outside tmux → "attach" (a fresh tmux client attaches to the
//                   target). switch-client requires an existing client
//                   to switch.
//
// This handles popup mode (popup pty IS a tmux client) and any future
// nested invocation. The CLI subcommands that today are gated by
// enforceNoNestedTmux still benefit if a user opts into nesting via
// CANOPY_ALLOW_NESTED.
func attachVerbForCurrentEnv() string {
	if os.Getenv("TMUX") != "" {
		return "switch-client"
	}
	return "attach"
}

// KillServer shuts down the tmux server bound to this client's socket.
// Used by tests to clean up in t.Cleanup; rarely useful in production.
//
// Important: kill-server alone doesn't always reap children. Specifically,
// `nvim --embed` (which interactive `nvim .` forks as its editor backend)
// deliberately detaches its session so it can outlive the launcher — a
// feature for "embed nvim in another tool" use cases. Without explicit
// reaping, every test that spawns an `nvim .` pane leaks one nvim --embed
// process to PID 1; over hundreds of test runs that adds up to gigabytes
// of RAM held by zombie test fixtures. KillServerAndReap below is the
// fix.
func (c *Client) KillServer(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "tmux", c.args("kill-server")...)
	// kill-server exits non-zero if no server was running; treat that as success.
	_ = cmd.Run()
	return nil
}

// KillServerAndReap is KillServer plus aggressive orphan cleanup.
// Before killing the server, it enumerates every session's pane
// process tree via PaneInfos and collects the descendant PIDs.
// After the server dies, it SIGKILLs anything from that PID set
// still alive — catching nvim --embed and any other process that
// detached from its tmux pane.
//
// Use this instead of KillServer in test cleanup. Production code
// shouldn't need it: real workspace lifecycle uses Manager.Remove
// which runs scripts.archive (the user's chance to gracefully shut
// down dev servers etc.) before kill-session.
//
// Best-effort: errors enumerating panes don't block the kill. The
// tmux server going down is the load-bearing part; the reap is
// belt-and-suspenders.
func (c *Client) KillServerAndReap(ctx context.Context) error {
	// Collect every PID under every pane in every session BEFORE
	// killing the server (the server dying loses our enumeration handle).
	var pidsToReap []int
	sessionsCmd := exec.CommandContext(ctx, "tmux", c.args("list-sessions", "-F", "#{session_name}")...)
	if out, err := sessionsCmd.Output(); err == nil {
		for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if name == "" {
				continue
			}
			infos, err := c.PaneInfos(ctx, name)
			if err != nil {
				continue
			}
			for _, p := range infos {
				for _, proc := range p.Tree {
					pidsToReap = append(pidsToReap, proc.PID)
				}
			}
		}
	}

	// Now kill the server.
	_ = exec.CommandContext(ctx, "tmux", c.args("kill-server")...).Run()

	// SIGKILL anything from the collected PID set that survived.
	// Use SIGKILL not SIGTERM — these are zombies from a forced server
	// shutdown; we want them gone immediately, not given a chance to
	// "gracefully exit" (which is what got us here in the first place).
	for _, pid := range pidsToReap {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}
