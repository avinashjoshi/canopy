package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// PaneInfo describes one pane's process tree.
type PaneInfo struct {
	// PaneID is tmux's pane identifier (e.g. "%23"). Stable for the
	// life of the pane; useful for cross-referencing with `tmux
	// display-message`.
	PaneID string
	// Index is the pane's position in the window (0-based or 1-based
	// depending on the user's tmux base-index, typically 0). Used as
	// a human-friendly label in the drawer.
	Index int
	// PID is the pane's shell PID. Children of this PID are the
	// actual workload (claude, nvim, dev server, etc.) — the shell
	// itself is usually small.
	PID int
	// Title is the pane's tmux title (#{pane_title}), often the
	// command name like "claude" or "nvim" once a long-running
	// process attaches. Empty when no title set.
	Title string
	// Tree is the recursive process tree under PID, including PID
	// itself. The list is sorted by RSS desc so the heaviest process
	// in the pane is first.
	Tree []ProcInfo
	// TotalRSS is the sum of RSS across Tree, in bytes.
	TotalRSS int64
}

// ProcInfo describes one process in a pane's tree.
type ProcInfo struct {
	PID  int
	PPID int
	RSS  int64  // resident set size in bytes (kB from ps × 1024)
	CPU  string // human-readable percentage from ps -o pcpu (e.g. "12.3")
	Comm string // command name (process basename)
}

// PaneInfos returns one PaneInfo per pane in the named tmux session.
// Each PaneInfo carries a process tree rooted at the pane's PID with
// RSS/CPU per process.
//
// Implementation: `tmux list-panes -t <session> -F ...` to get the pane
// PIDs, then `ps -o pid,ppid,rss,pcpu,comm` over the relevant PID set
// to enumerate descendants. ps is invoked once per pane (not once per
// PID) to keep subprocess count bounded — typical canopy workspace has
// 4 panes and 5-15 processes total, well within ps's argv limits.
//
// Empty session (no panes) returns ([], nil). tmux failure returns the
// error verbatim — caller decides whether to surface or fall back.
//
// Cross-platform: ps -o is the most stable Unix interface there is;
// works on Linux + macOS unchanged. Linux's ps shows kB in RSS column
// by default; macOS too. We multiply by 1024 to get bytes.
func (c *Client) PaneInfos(ctx context.Context, session string) ([]PaneInfo, error) {
	exists, err := c.HasSession(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("tmux.PaneInfos(%s): %w", session, err)
	}
	if !exists {
		return nil, fmt.Errorf("tmux.PaneInfos(%s): %w", session, ErrSessionNotFound)
	}

	// tmux's -F format string: pane index, pane id, pid, title.
	// Tabs as separators since titles can contain spaces but never tabs
	// (tmux strips them).
	args := c.args("list-panes", "-t", session, "-F", "#{pane_index}\t#{pane_id}\t#{pane_pid}\t#{pane_title}")
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tmux.PaneInfos(%s): list-panes failed: %w (stderr: %s)",
			session, err, strings.TrimSpace(stderr.String()))
	}

	var infos []PaneInfo
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 3 {
			log.Warn("tmux.list-panes.malformed-line", "line", line)
			continue
		}
		idx, err := strconv.Atoi(parts[0])
		if err != nil {
			log.Warn("tmux.list-panes.bad-index", "raw", parts[0], "err", err)
			continue
		}
		pid, err := strconv.Atoi(parts[2])
		if err != nil {
			log.Warn("tmux.list-panes.bad-pid", "raw", parts[2], "err", err)
			continue
		}
		title := ""
		if len(parts) == 4 {
			title = parts[3]
		}
		info := PaneInfo{
			PaneID: parts[1],
			Index:  idx,
			PID:    pid,
			Title:  title,
		}
		infos = append(infos, info)
	}
	if len(infos) == 0 {
		return infos, nil
	}

	// One ps -A snapshot for the whole session, walked per-pane below.
	// Previously this was one `ps -A` per pane; for a 4-pane workspace
	// that's 4× the subprocess cost for the same data. With a snapshot
	// it's one ps regardless of pane count.
	snap, err := psSnapshot(ctx)
	if err != nil {
		log.Warn("tmux.PaneInfos.ps-failed", "session", session, "err", err)
		// Leave each pane's Tree empty + TotalRSS=0; drawer renders
		// "(process tree unavailable)" with the underlying error.
		return infos, nil
	}
	for i := range infos {
		tree, total := psTreeFrom(snap, infos[i].PID)
		infos[i].Tree = tree
		infos[i].TotalRSS = total
	}
	return infos, nil
}

// SessionAttached returns true if at least one tmux client is
// currently attached to the named session. Distinct from HasSession:
// a session can be alive (the server has it) without any client
// looking at it (detached). Used by the TUI to show which workspace
// the user is "currently in" so they don't accidentally K it.
//
// Implementation: `tmux display-message -t <name> -p '#{session_attached}'`
// returns "0" or a positive integer ("1", "2" if multiple clients).
// Empty / unknown maps to false. ErrSessionNotFound when the session
// doesn't exist on the server.
func (c *Client) SessionAttached(ctx context.Context, name string) (bool, error) {
	exists, err := c.HasSession(ctx, name)
	if err != nil {
		return false, fmt.Errorf("tmux.SessionAttached(%s): %w", name, err)
	}
	if !exists {
		return false, nil
	}
	args := c.args("display-message", "-t", name, "-p", "#{session_attached}")
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("tmux.SessionAttached(%s): %w", name, err)
	}
	v := strings.TrimSpace(string(out))
	return v != "" && v != "0", nil
}

// AttachedSessions returns the set of session names with at least one
// client attached, in a single tmux call. Cheaper than calling
// SessionAttached per session — `tmux list-sessions -F` is one
// round-trip regardless of session count. Returns an empty map (not
// nil) when no sessions are attached so callers can do
// `if attached["foo"]` without a nil check.
//
// Returns an empty map + nil error when no tmux server is running
// (no sessions to enumerate). Other failures bubble up.
func (c *Client) AttachedSessions(ctx context.Context) (map[string]bool, error) {
	args := c.args("list-sessions", "-F", "#{session_name}\t#{session_attached}")
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.Output()
	if err != nil {
		// "no server running" is the expected error before any session
		// has been created — treat as empty, not as error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("tmux.AttachedSessions: %w", err)
	}
	out2 := strings.TrimRight(string(out), "\n")
	result := make(map[string]bool)
	if out2 == "" {
		return result, nil
	}
	for _, line := range strings.Split(out2, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		if parts[1] != "" && parts[1] != "0" {
			result[parts[0]] = true
		}
	}
	return result, nil
}

// SessionLoad describes a session's aggregate resource use across
// every process in every pane. RSS is bytes; CPU is summed pcpu
// (single-core normalized; can exceed 100 on multi-core).
type SessionLoad struct {
	RSS int64
	CPU float64
}

// SessionLoad returns the aggregate RSS+CPU across every process in
// every pane of the named tmux session. Used to populate the Mem/CPU
// cell on the TUI list page. Built on PaneInfos so the heavy lifting
// (tmux list-panes + ps -A) lives in one place.
//
// ErrSessionNotFound bubbles up untouched so the caller can treat
// dead sessions as zero/"—" without spending a ps call.
func (c *Client) SessionLoad(ctx context.Context, session string) (SessionLoad, error) {
	infos, err := c.PaneInfos(ctx, session)
	if err != nil {
		return SessionLoad{}, err
	}
	var load SessionLoad
	for _, p := range infos {
		load.RSS += p.TotalRSS
		for _, proc := range p.Tree {
			if cpu, err := strconv.ParseFloat(proc.CPU, 64); err == nil {
				load.CPU += cpu
			}
		}
	}
	return load, nil
}

// SessionRSS is the legacy single-value form of SessionLoad. Kept as a
// thin wrapper for any external callers that only need RSS; internal
// code uses SessionLoad to get CPU at the same time without a second
// ps call.
func (c *Client) SessionRSS(ctx context.Context, session string) (int64, error) {
	load, err := c.SessionLoad(ctx, session)
	if err != nil {
		return 0, err
	}
	return load.RSS, nil
}

// psTree returns the process tree rooted at root (root + all descendants
// recursively), each with RSS and CPU. Sorted by RSS desc.
//
// Strategy: one `ps -A -o pid,ppid,rss,pcpu,comm` call to get the whole
// process table, then filter in-memory by walking the parent-child
// graph from root. Single-process-walk is much cheaper than recursive
// `ps --ppid` calls per node, and ps's full-system listing is ~5ms even
// on busy machines.
//
// Public-facing wrapper: a one-shot ps that does its own snapshot per
// call. PaneInfos uses psTreeFrom (no internal-ps version) instead so
// it can share one snapshot across multiple panes in the same session
// — for a 4-pane workspace that's 4× fewer ps invocations.
func psTree(ctx context.Context, root int) ([]ProcInfo, int64, error) {
	snap, err := psSnapshot(ctx)
	if err != nil {
		return nil, 0, err
	}
	tree, total := psTreeFrom(snap, root)
	return tree, total, nil
}

// psSnapshotData holds a parsed `ps -A` snapshot: every process keyed
// by PID, plus a parent→children index. Reused across multiple
// psTreeFrom calls in the same enumeration.
type psSnapshotData struct {
	all      map[int]ProcInfo
	children map[int][]int
}

// psSnapshot runs ps once and parses the result into a reusable
// snapshot. The snapshot is read-only after construction so callers
// can share it without locking.
func psSnapshot(ctx context.Context) (psSnapshotData, error) {
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,rss=,pcpu=,comm=")
	out, err := cmd.Output()
	if err != nil {
		return psSnapshotData{}, fmt.Errorf("ps: %w", err)
	}
	snap := psSnapshotData{
		all:      map[int]ProcInfo{},
		children: map[int][]int{},
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		rssKB, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			continue
		}
		cpu := fields[3]
		// comm may contain spaces (rare but possible in macOS); join
		// the rest of the fields back.
		comm := strings.Join(fields[4:], " ")
		snap.all[pid] = ProcInfo{
			PID:  pid,
			PPID: ppid,
			RSS:  rssKB * 1024,
			CPU:  cpu,
			Comm: comm,
		}
		snap.children[ppid] = append(snap.children[ppid], pid)
	}
	return snap, nil
}

// psTreeFrom walks a pre-built snapshot from root and returns the
// sorted process tree. Cheap (just hash lookups + BFS); meant to be
// called many times with one snapshot when enumerating panes.
func psTreeFrom(snap psSnapshotData, root int) ([]ProcInfo, int64) {
	// BFS from root, collect every descendant + root itself.
	var tree []ProcInfo
	var total int64
	if p, ok := snap.all[root]; ok {
		tree = append(tree, p)
		total += p.RSS
	}
	frontier := []int{root}
	for len(frontier) > 0 {
		var next []int
		for _, pid := range frontier {
			for _, child := range snap.children[pid] {
				if cp, ok := snap.all[child]; ok {
					tree = append(tree, cp)
					total += cp.RSS
					next = append(next, child)
				}
			}
		}
		frontier = next
	}
	// Sort by RSS desc — heaviest first so the user immediately sees
	// the offender. Stable sort is overkill; insertion-sort on a
	// 5-15-element slice is fine and avoids importing sort just for this.
	for i := 1; i < len(tree); i++ {
		for j := i; j > 0 && tree[j].RSS > tree[j-1].RSS; j-- {
			tree[j], tree[j-1] = tree[j-1], tree[j]
		}
	}
	return tree, total
}
