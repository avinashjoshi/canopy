package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// resolvedHost is what dispatchNewToRemote / dispatchSwitchToRemote
// need to know: the SSH target string and the resolved project path
// (where canopy will cd before running on the remote). Source carries
// a human-readable trace of how we got here for the dispatch log.
type resolvedHost struct {
	SSHTarget string
	RemoteCwd string // empty when caller will pass --remote-cwd explicitly
	Source    string // "registry:<host>/<project>" or "raw-target"
	HostName  string // empty if raw target
}

// resolveOnForNew turns the value of `--on` into a usable SSH target +
// project path for `canopy new --on <spec>`.
//
// localProject is the local cwd-derived project basename (canopy walks
// up from cwd, takes the dir name). For a raw SSH target we ignore it;
// for a registry name we use it to pick which project on the host to
// dispatch into (host.Projects[localProject]).
//
// explicitRemoteCwd, if non-empty, wins over registry lookup. That's
// the per-command escape hatch for "register a host but don't bother
// adding projects" and "dispatch to a one-off path."
//
// Errors are wrapped with host package sentinels (ErrHostNotFound,
// ErrProjectNotFound) so callers can branch on them.
func resolveOnForNew(spec, localProject, explicitRemoteCwd string) (resolvedHost, error) {
	if spec == "" {
		return resolvedHost{}, fmt.Errorf("resolveOn: empty --on value")
	}
	asRawTarget := func() (resolvedHost, error) {
		// Fail fast with a clear canopy-level error rather than relying
		// solely on ssh's own rejection of an option-shaped target —
		// see host.ValidateSSHTarget's doc comment.
		if err := host.ValidateSSHTarget(spec); err != nil {
			return resolvedHost{}, fmt.Errorf("resolveOn: %w", err)
		}
		// Caller must pass --remote-cwd (or accept that canopy on the
		// remote will fail to find canopy.json from $HOME).
		return resolvedHost{
			SSHTarget: spec,
			RemoteCwd: explicitRemoteCwd,
			Source:    "raw-target",
		}, nil
	}
	// A spec containing "@"/":" — user@host, host:port, or any other
	// form ssh's argv parser accepts — can never BE a valid registered
	// name (validateName forbids those characters), so it's always a
	// raw target; skip the registry lookup entirely.
	if strings.ContainsAny(spec, "@:") {
		return asRawTarget()
	}
	// Registry name lookup. A bare word that ISN'T a registered name
	// falls back to being used directly as a raw SSH target (a
	// ~/.ssh/config alias, /etc/hosts entry, etc.) rather than erroring
	// — matching how `ssh <bare-word>` would already work, without
	// requiring `canopy host add` first.
	reg, err := loadHostRegistry()
	if err != nil {
		return resolvedHost{}, fmt.Errorf("resolveOn: %w", err)
	}
	h, err := reg.Resolve(spec)
	if err != nil {
		if errors.Is(err, host.ErrHostNotFound) {
			return asRawTarget()
		}
		return resolvedHost{}, fmt.Errorf(
			"host %q not registered. Run `canopy host add %s <ssh-target>` or pass --on <ssh-target>: %w",
			spec, spec, err)
	}
	if h.Type != "ssh" {
		return resolvedHost{}, fmt.Errorf(
			"resolveOn: host %q has type %q; only \"ssh\" is supported in v0.17.0", spec, h.Type)
	}
	cwd := explicitRemoteCwd
	source := "registry:" + spec
	if cwd == "" {
		// Look up the project path. localProject comes from the
		// caller's cwd-walk; if it's empty (user not in a project),
		// we can't auto-resolve.
		if localProject == "" {
			return resolvedHost{}, fmt.Errorf(
				"resolveOn: --on %s needs a project but you're not inside any canopy project. cd into one or pass --remote-cwd <path>", spec)
		}
		path, perr := reg.GetProject(spec, localProject)
		if perr != nil {
			if errors.Is(perr, host.ErrProjectNotFound) {
				return resolvedHost{}, fmt.Errorf(
					"project %q not registered on host %q. Run: canopy project add %s <remote-path> --on %s",
					localProject, spec, localProject, spec)
			}
			return resolvedHost{}, perr
		}
		cwd = path
		source = "registry:" + spec + "/" + localProject
	}
	return resolvedHost{
		SSHTarget: h.SSHTarget,
		RemoteCwd: cwd,
		Source:    source,
		HostName:  spec,
	}, nil
}

// resolveOnForSwitch is the attach-path counterpart to resolveOnForNew.
//
// switch's purpose is to find a workspace BY NAME on the remote (global
// lookup), then attach. The remote canopy switch still needs to be cd'd
// inside SOME project to load its config — but it doesn't matter WHICH
// project, because the workspace lookup is global. So we use the local
// project name as a preferred hint, fall back to any registered project,
// and only error if the host has no projects registered at all.
//
// preferredProject may be empty (user isn't in a local project — that's
// fine for switch, unlike new).
func resolveOnForSwitch(spec, preferredProject, explicitRemoteCwd string) (resolvedHost, error) {
	if spec == "" {
		return resolvedHost{}, fmt.Errorf("resolveOn: empty --on value")
	}
	asRawTarget := func() (resolvedHost, error) {
		if err := host.ValidateSSHTarget(spec); err != nil {
			return resolvedHost{}, fmt.Errorf("resolveOn: %w", err)
		}
		return resolvedHost{
			SSHTarget: spec,
			RemoteCwd: explicitRemoteCwd,
			Source:    "raw-target",
		}, nil
	}
	// See resolveOnForNew's identical comment: "@"/":" can never be a
	// valid registered name, so skip the registry lookup entirely.
	if strings.ContainsAny(spec, "@:") {
		return asRawTarget()
	}
	reg, err := loadHostRegistry()
	if err != nil {
		return resolvedHost{}, fmt.Errorf("resolveOn: %w", err)
	}
	h, err := reg.Resolve(spec)
	if err != nil {
		// A bare word that isn't a registered name falls back to a raw
		// target — this is what fixes `enter` on a --remote-pinned row
		// for an unregistered host: the TUI's attach flow dispatches
		// `canopy switch --on <row.Host>`, and row.Host is exactly the
		// spec the user originally typed (registered name or not).
		if errors.Is(err, host.ErrHostNotFound) {
			return asRawTarget()
		}
		return resolvedHost{}, fmt.Errorf(
			"host %q not registered. Run `canopy host add %s <ssh-target>` or pass --on <ssh-target>: %w",
			spec, spec, err)
	}
	if h.Type != "ssh" {
		return resolvedHost{}, fmt.Errorf(
			"resolveOn: host %q has type %q; only \"ssh\" is supported in v0.17.0", spec, h.Type)
	}
	cwd := explicitRemoteCwd
	source := "registry:" + spec
	if cwd == "" {
		// Try preferred (local) project first.
		if preferredProject != "" {
			if path, perr := reg.GetProject(spec, preferredProject); perr == nil {
				cwd = path
				source = "registry:" + spec + "/" + preferredProject
			}
		}
		// Fall back to first registered project on this host.
		if cwd == "" {
			projs, lerr := reg.ListProjects(spec)
			if lerr == nil && len(projs) > 0 {
				cwd = projs[0].Path
				source = "registry:" + spec + "/" + projs[0].Name + " (fallback)"
			}
		}
		// Still nothing? Host has no projects registered.
		if cwd == "" {
			return resolvedHost{}, fmt.Errorf(
				"host %q has no projects registered. Run: canopy project add <project-name> <remote-path> --on %s (or pass --remote-cwd)",
				spec, spec)
		}
	}
	return resolvedHost{
		SSHTarget: h.SSHTarget,
		RemoteCwd: cwd,
		Source:    source,
		HostName:  spec,
	}, nil
}

// resolveRemoteHost turns the value of `--remote` into a usable
// host.Host for the `canopy --remote <spec>` thin-client mode (v0.22,
// see routeRemote).
//
// Unlike --on's resolveOnForNew/resolveOnForSwitch (which decide raw-
// target-vs-registry-name purely from spec's shape — present of "@" or
// ":"), resolveRemoteHost tries the registry FIRST regardless of shape,
// and only falls back to treating spec as a raw SSH target if it isn't
// a registered name. The shape-only heuristic breaks for the single
// most common case of "just log in, no host add": a bare SSH config
// alias like `--remote tower` (no "@", no ":", resolved by the user's
// own ~/.ssh/config, not by canopy) — under the old shape-only rule
// that fell into the registry-name branch and errored "not registered"
// even though `ssh tower` works fine. Try-then-fall-back handles both:
// a spec containing "@"/":" can never BE a valid registry name anyway
// (validateName forbids those characters), so registry lookup is
// skipped for those as a pure optimization, not a correctness split.
//
// selfHeal reports whether the resolved host has a real registry entry
// to attach auto-discovered project registrations to (see
// buildRemoteRowsMsg's selfHeal parameter in internal/ui) — true only
// when spec resolved via the registry.
//
// Unlike resolveOnForNew/resolveOnForSwitch, there's no "which project"
// step here: --remote lists every project on the host (`canopy ls
// --json --all`), it doesn't dispatch into one.
func resolveRemoteHost(spec string) (h host.Host, selfHeal bool, err error) {
	if spec == "" {
		return host.Host{}, false, fmt.Errorf("resolveRemoteHost: empty --remote value")
	}
	asRawTarget := func() (host.Host, bool, error) {
		// Fail fast with a clear canopy-level error rather than relying
		// solely on ssh's own rejection of an option-shaped target
		// (which the sink-side "--" fix in internal/host/ssh.go already
		// guards against, but a dedicated check here gives a much
		// clearer message than "hostname contains invalid characters").
		if err := host.ValidateSSHTarget(spec); err != nil {
			return host.Host{}, false, fmt.Errorf("resolveRemoteHost: %w", err)
		}
		return host.Host{Name: spec, Type: "ssh", SSHTarget: spec}, false, nil
	}
	if strings.ContainsAny(spec, "@:") {
		// Can never be a registered name (validateName forbids @/:) —
		// skip the registry lookup entirely.
		return asRawTarget()
	}
	reg, regErr := loadHostRegistry()
	if regErr != nil {
		return host.Host{}, false, fmt.Errorf("resolveRemoteHost: %w", regErr)
	}
	resolved, resolveErr := reg.Resolve(spec)
	if resolveErr != nil {
		if errors.Is(resolveErr, host.ErrHostNotFound) {
			// Not a registry name — fall back to trying it as a bare
			// SSH target (a ~/.ssh/config alias, /etc/hosts entry, or
			// resolvable hostname/mDNS name with no "@"/":").
			return asRawTarget()
		}
		return host.Host{}, false, fmt.Errorf("resolveRemoteHost: %w", resolveErr)
	}
	if resolved.Type != "ssh" {
		return host.Host{}, false, fmt.Errorf(
			"resolveRemoteHost: host %q has type %q; only \"ssh\" is supported", spec, resolved.Type)
	}
	return resolved, true, nil
}

// probeRemoteCwd is a fast `test -d` check over SSH. Used before
// exec'ing mosh in dispatchSwitchToRemote so a missing remote project
// path surfaces in the laptop's terminal — not silently inside the
// mosh child shell, where it tears down without leaving anything on
// screen for the TUI to relay. Reuses the ControlMaster socket, so on
// a warmed-up canopy session this is sub-100ms.
//
// Returns nil when the path exists, non-nil on any failure (missing
// path, ssh transport error, timeout). The caller distinguishes "path
// missing" from "host offline" by checking the exit code via
// *exec.ExitError; non-zero = test failed = path missing.
func probeRemoteCwd(ctx context.Context, sshTarget, remotePath string) error {
	cmd := host.SSHCmd(ctx, sshTarget, "test", "-d", remotePath)
	return cmd.Run()
}

// dispatchVerbToRemote runs an arbitrary canopy verb on a remote host
// via SSH. Used by --on-aware CLI subcommands (rm, retry, etc.) that
// don't need the prompt-file dance dispatchNewToRemote does for `new`.
//
// The script is piped via SSH stdin to `bash -l` on the remote, with
// PATH-prepending for ~/.local/bin and optional cwd change. Stdout/
// stderr stream back to the caller's writers so the CLI feels native.
//
// resolved.RemoteCwd takes the per-command --remote-cwd override
// already; pass an empty string when not applicable.
func dispatchVerbToRemote(ctx context.Context, resolved resolvedHost, verb string, args []string, stdout, stderr io.Writer) error {
	canopyArgs := append([]string{"canopy", verb}, args...)
	script := buildRemoteScript(resolved.RemoteCwd, canopyArgs, "")
	fmt.Fprintf(stderr, "Dispatching to %s (%s):\n%s", resolved.SSHTarget, resolved.Source, indent(script, "  "))

	cmd := host.SSHCmd(ctx, resolved.SSHTarget, "bash", "-l")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = strings.NewReader(script)
	if err := cmd.Run(); err != nil {
		// Exit 7 — buildRemoteScript's dir-existence pre-check fired.
		// Surface the actionable remediation instead of bash's terse cd
		// error. resolved.HostName is set when --on was a registry name
		// (the common path through the TUI); empty for raw ssh-targets.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 7 {
			spec := resolved.HostName
			if spec == "" {
				spec = resolved.SSHTarget
			}
			return remotePathMissingErr(spec, resolved.RemoteCwd, resolved.HostName)
		}
		return fmt.Errorf("remote canopy %s failed: %w", verb, err)
	}
	return nil
}

// localProjectBasename returns the basename of the current canopy
// project's *source repo* root directory. Critically, when cwd is a
// workspace's worktree under ~/.canopy/workspaces/<project>/<name>,
// we resolve back to the SOURCE project (not the worktree basename
// "eager-pine") so the registry lookup uses the right project name.
//
// Resolution order:
//  1. Try workspace.ResolveCurrentProject(cwd, state.json) — matches
//     the manager.go logic. Maps any cwd inside a known workspace
//     back to its source-repo root.
//  2. Walk up looking for canopy.json. Catches "user is in their
//     source repo for a project not registered in state.json yet."
//  3. Return empty — caller decides whether that's an error.
func localProjectBasename(cwd string) string {
	if cwd == "" {
		return ""
	}
	// (1) state.json-backed workspace-to-source mapping.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if store, sErr := state.NewStore(filepath.Join(home, ".canopy")); sErr == nil {
			if st, lErr := store.Load(); lErr == nil {
				if root := workspace.ResolveCurrentProject(cwd, st); root != "" {
					return filepath.Base(root)
				}
			}
		}
	}
	// (2) Walk up looking for canopy.json (source-repo case).
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "canopy.json")); err == nil {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
