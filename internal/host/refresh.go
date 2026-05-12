package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Refresher fans out per-host `canopy ls --json` queries with bounded
// concurrency + per-host deadlines. Phase 1b architecture:
//
//	TUI tick (every 2s)
//	     │
//	     ▼
//	Refresher.Tick(ctx, hosts) ──fan-out──┐
//	     ▲                                │
//	     │ join (3s deadline)             │ per-host
//	     │                                │ goroutine
//	     │                                ▼
//	     │                          ssh tower canopy ls --json
//	     │                                │
//	     └────────────────────────────────┘
//	            ▼
//	     map[host]Result
//
// One slow/offline host can't freeze the TUI: each goroutine has its
// own context.WithTimeout(3s), they all join via a sync.WaitGroup, and
// the Tick caller sees a Result for every host (with a non-nil Err for
// the ones that timed out / failed). This is the D8 decision from
// /plan-eng-review.
type Refresher struct {
	// Timeout is per-host, default 3 * time.Second. Caller may override
	// for slower networks (mobile tether, congested wifi) or for tests.
	Timeout time.Duration
}

// Result is the per-host outcome of one refresh tick.
type Result struct {
	HostName string
	// Workspaces is the workspace listing from `canopy ls --json` on
	// the remote. Nil when Err != nil (we don't pretend to have rows
	// for an offline host; the cache layer keeps the LAST-known rows
	// visible via its own merge).
	Workspaces []RemoteWorkspace
	// CanopyVersion is whatever the remote canopy reports about itself.
	// Empty if Err != nil or schema didn't include it.
	CanopyVersion string
	// LastSeen is set to the moment the tick succeeded. Zero on Err.
	LastSeen time.Time
	// RTT is the wall-clock duration the goroutine spent on this host
	// (SSH connect + remote command + parse). Useful for the Hosts tab
	// "RTT" column (Phase 1c).
	RTT time.Duration
	// Err is non-nil when the refresh failed: connection refused, auth
	// failed, ssh timeout, remote canopy crashed, malformed JSON, etc.
	// The TUI surfaces this as a per-host status pill but doesn't drop
	// the last-known rows from cache.
	Err error
}

// RemoteWorkspace is the parsed shape of one entry in the remote's
// `canopy ls --json` output. Mirrors LsJSONWorkspace in cmd/canopy/ls.go
// — kept here in the host package to avoid an import cycle between
// internal/host and cmd/canopy. If the wire format gains fields later
// they're additive; older clients ignore them.
type RemoteWorkspace struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	Branch      string `json:"branch"`
	Status      string `json:"status"`
	Port        int    `json:"port,omitempty"`
	TmuxSession string `json:"tmux_session"`
	Alive       bool   `json:"alive"`
}

// remoteLsResponse mirrors LsJSONOutput on the cmd/canopy side. Kept
// internal — we only need the workspace listing + version for the
// refresher's purposes.
type remoteLsResponse struct {
	SchemaVersion int               `json:"schema_version"`
	CanopyVersion string            `json:"canopy_version"`
	Workspaces    []RemoteWorkspace `json:"workspaces"`
}

// Tick runs one refresh pass across all `hosts`, returning a slice of
// Results — one per host, in the same order as the input. Blocks until
// every host has either returned or timed out (3s deadline by default,
// bounded so the TUI tick stays snappy regardless of network state).
//
// Callers (model.go) wrap this in a tea.Cmd so it runs off the UI
// thread.
//
// Implementation: spawns one goroutine per host with its own
// context.WithTimeout. Each goroutine builds an SSH command using
// host.SSHCmd (which sets ControlMaster flags so the second+ ticks
// reuse the SSH connection from the first one — that's where the
// 50-300ms-per-host handshake cost disappears for steady-state
// refreshes).
func (r *Refresher) Tick(ctx context.Context, hosts []Host) []Result {
	if len(hosts) == 0 {
		return nil
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	results := make([]Result, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(idx int, host Host) {
			defer wg.Done()
			results[idx] = refreshOneHost(ctx, host, timeout)
		}(i, h)
	}
	wg.Wait()
	return results
}

// refreshOneHost is the per-host worker. Builds the SSH command,
// invokes it under a context.WithTimeout, parses the JSON response.
// Errors at any step land in Result.Err and the cache layer (caller's
// responsibility) decides whether to display them.
func refreshOneHost(parent context.Context, h Host, timeout time.Duration) Result {
	res := Result{HostName: h.Name}
	if h.Type != "ssh" {
		res.Err = fmt.Errorf("refreshOneHost: unsupported host type %q (only ssh in v0.17.0)", h.Type)
		return res
	}
	if h.SSHTarget == "" {
		res.Err = fmt.Errorf("refreshOneHost: host %q has no ssh_target", h.Name)
		return res
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	start := time.Now()
	// `canopy ls --json` doesn't need a project cwd (uses global state)
	// so no cd dance. PATH-prepending matches the dispatch path.
	// SSHCmdBatch (NOT SSHCmd): BatchMode=yes prevents password prompts
	// from hanging this goroutine. Without it, a host that doesn't have
	// SSH key auth set up would hang the refresh forever AND corrupt
	// the Bubbletea TUI render (SSH writes password prompts to /dev/tty
	// directly, bypassing our captured stdout/stderr).
	remoteCmd := `export PATH="$HOME/.local/bin:$PATH"; exec canopy ls --json --all`
	cmd := SSHCmdBatch(ctx, h.SSHTarget, "bash", "-lc", remoteCmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Don't include full stderr in the wrapped error — keeps the
		// per-row error pill in the TUI readable. Caller can pull
		// detailed diagnostics from the canopy log.
		stderrSnippet := stderr.String()
		if len(stderrSnippet) > 120 {
			stderrSnippet = stderrSnippet[:120] + "…"
		}
		if stderrSnippet != "" {
			res.Err = fmt.Errorf("ssh %s canopy ls --json: %w (stderr: %s)", h.Name, err, stderrSnippet)
		} else {
			res.Err = fmt.Errorf("ssh %s canopy ls --json: %w", h.Name, err)
		}
		log.Debug("host.refresh.failed", "host", h.Name, "err", res.Err.Error())
		return res
	}

	var parsed remoteLsResponse
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		res.Err = fmt.Errorf("host %s: parse canopy ls --json output: %w", h.Name, err)
		return res
	}

	res.Workspaces = parsed.Workspaces
	res.CanopyVersion = parsed.CanopyVersion
	res.LastSeen = time.Now()
	res.RTT = time.Since(start)
	log.Debug("host.refresh.ok", "host", h.Name, "workspaces", len(res.Workspaces), "rtt_ms", res.RTT.Milliseconds())
	return res
}
