// Remote-dispatch helpers shared across the rm / retry / kill flows
// (and used by `n` for cwd resolution). Each helper is "given a host
// name, do an SSH thing"; the workspace-action wrappers live next to
// the keybind they implement (update_kill.go, update_retry.go, etc).
// Carved out of update.go.

package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// parseRemoteWorkspaceName scrapes the workspace name from a streamed
// `canopy new` log. The canonical success line — emitted by the remote
// canopy at cmd/canopy/new.go — is `Workspace ready: <name>` followed
// by a newline. Returns "" when the line isn't present (e.g., partial
// output on failure), which the caller treats as "fall back to manual
// attach hint."
//
// Picks the LAST occurrence: a setup hook that echoes the marker line,
// or a prompt containing it, shouldn't be able to redirect the
// auto-attach. The remote canopy emits this line once at the very end
// of a successful create — taking the last match makes the parser
// robust against injection-shaped output earlier in the stream.
func parseRemoteWorkspaceName(output string) string {
	const prefix = "Workspace ready: "
	var got string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			got = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return got
}

// execRemoteVerb runs `<canopy-bin> <verb> --on <host> <args...>` as a
// subprocess via tea.ExecProcess. Used for rm and retry. v0.17.0 Phase 1.
//
// force, when true, appends --force to the remote canopy invocation.
// Used by the delete handler when the user pressed F on hanging-work
// confirmation (mirrors the local --force path).
func (m *Model) execRemoteVerb(hostName, verb string, args []string, force bool) tea.Cmd {
	canopyBin, err := os.Executable()
	if err != nil || canopyBin == "" {
		canopyBin = os.Args[0]
	}
	cmdArgs := []string{verb, "--on", hostName}
	cmdArgs = append(cmdArgs, args...)
	if force {
		cmdArgs = append(cmdArgs, "--force")
	}
	cmd := exec.Command(canopyBin, cmdArgs...)
	cmd.Env = append(os.Environ(), "CANOPY_ALLOW_NESTED=1")
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			log.Warn("ui.remote-verb.failed", "verb", verb, "host", hostName, "err", err)
		}
		// Refresh BOTH local and remote so the row updates / disappears
		// as appropriate. The remote-rows fan-out is what reflects the
		// rm/retry side effect we just dispatched. v0.17 Phase 1h.
		// The refreshAllMsg handler in Update clears remoteRefreshing
		// on the Bubbletea goroutine before re-dispatching refresh, so
		// we deliberately don't touch m from this exec callback.
		return refreshAllMsg{}
	})
}

// canopyBinPath returns the absolute path to this canopy binary so
// subprocess invocations resolve to the same build. Falls back to
// os.Args[0] when os.Executable() fails (rare; e.g. binary deleted
// after launch). v0.17 Phase 1k.
func (m *Model) canopyBinPath() string {
	bin, err := os.Executable()
	if err != nil || bin == "" {
		return os.Args[0]
	}
	return bin
}

// remoteCwdForRow looks up the remote project path for (host, project)
// from the in-memory host registry snapshot. Returns "" when the
// registry doesn't know that pair (lets the caller fall back to
// canopy's own resolution logic + error). v0.17 Phase 1i.
func (m *Model) remoteCwdForRow(hostName, projectName string) string {
	if hostName == "" || projectName == "" {
		return ""
	}
	for _, h := range m.hostList {
		if h.Name != hostName {
			continue
		}
		return h.Projects[projectName]
	}
	return ""
}

// remoteCwdArg returns the `--remote-cwd <path>` suffix to thread into
// a remote canopy dispatch, or nil when the host registry doesn't know
// the path. Pinning the project keeps cmd/canopy's resolveOnForSwitch
// out of its "first project on host" fallback (the path that prints
// `(fallback)` in the dispatch source line) so a verb dispatched for a
// workspace under project A can't accidentally land in project B on a
// multi-project host. Returning nil leaves the caller in the legacy
// shape (no flag appended) — that's still correct for hosts the
// registry hasn't fully mapped yet; only the diagnostic gets worse.
func (m *Model) remoteCwdArg(hostName, projectName string) []string {
	if path := m.remoteCwdForRow(hostName, projectName); path != "" {
		return []string{"--remote-cwd", path}
	}
	return nil
}

// execRemoteKill kills a workspace's tmux session on a remote host by
// SSHing `tmux kill-session -t <session>` directly. Doesn't go through
// canopy on the remote because there's no `canopy kill` verb — kill is
// a tmux operation that leaves the workspace's worktree + state intact,
// transitioning it to stopped (the existing canopy convention).
func (m *Model) execRemoteKill(hostName, sessionName string) tea.Cmd {
	// Inline the SSH via a one-shot subprocess. We use ssh directly here
	// (not via host.SSHCmd) because we're already in the parent process —
	// no need to re-resolve through cmd/canopy. The TUI's host registry
	// is the source of truth.
	resolved, err := m.resolveHostForExec(hostName)
	if err != nil {
		return func() tea.Msg {
			log.Warn("ui.remote-kill.resolve-failed", "host", hostName, "err", err)
			return refreshAllMsg{}
		}
	}
	cmd := exec.Command("ssh",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(os.Getenv("HOME"), ".canopy", "ssh-%C.sock"),
		"-o", "ControlPersist=300",
		"-o", "BatchMode=yes",
		"-o", "NumberOfPasswordPrompts=0",
		// "--" before resolved: without it, an SSHTarget value shaped
		// like an ssh option (e.g. "-oProxyCommand=...") is parsed as a
		// FLAG, not a hostname — confirmed by PoC to achieve local
		// arbitrary command execution. resolved comes from hosts.json
		// (m.hostList), which nothing validates the content of beyond
		// the host NAME's charset — see internal/host/ssh.go's
		// sshCmdInternal for the same fix applied to canopy's other ssh
		// call sites.
		"--", resolved,
		"tmux", "kill-session", "-t", sessionName,
	)
	return func() tea.Msg {
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Warn("ui.remote-kill.failed", "host", hostName, "session", sessionName, "err", err, "out", string(out))
		}
		// remoteRefreshing is cleared in the refreshAllMsg handler in
		// Update — never from a tea.Cmd goroutine. Touching m here
		// would race the View+spinner read path.
		return refreshAllMsg{}
	}
}

// resolveHostForExec looks up the SSH target for a host name from the
// in-memory registry snapshot the TUI already has (m.hostList).
// Avoids re-loading hosts.json every time the user kills/deletes a row.
func (m *Model) resolveHostForExec(name string) (string, error) {
	for _, h := range m.hostList {
		if h.Name == name {
			return h.SSHTarget, nil
		}
	}
	return "", fmt.Errorf("host %q not found in registry snapshot", name)
}
