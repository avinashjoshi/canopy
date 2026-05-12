package main

import (
	"errors"
	"fmt"
	"os"
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
	SSHTarget  string
	RemoteCwd  string // empty when caller will pass --remote-cwd explicitly
	Source     string // "registry:<host>/<project>" or "raw-target"
	HostName   string // empty if raw target
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
	// Raw SSH target — looks like `user@host`, `host:port`, or any
	// other form ssh's argv parser accepts. Caller must pass
	// --remote-cwd (or accept that canopy on the remote will fail to
	// find canopy.json from $HOME).
	if strings.ContainsAny(spec, "@:") {
		return resolvedHost{
			SSHTarget: spec,
			RemoteCwd: explicitRemoteCwd,
			Source:    "raw-target",
		}, nil
	}
	// Registry name lookup.
	reg, err := loadHostRegistry()
	if err != nil {
		return resolvedHost{}, fmt.Errorf("resolveOn: %w", err)
	}
	h, err := reg.Resolve(spec)
	if err != nil {
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
					"project %q not registered on host %q. Run: canopy host project add %s %s <remote-path>",
					localProject, spec, spec, localProject)
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
	if strings.ContainsAny(spec, "@:") {
		return resolvedHost{
			SSHTarget: spec,
			RemoteCwd: explicitRemoteCwd,
			Source:    "raw-target",
		}, nil
	}
	reg, err := loadHostRegistry()
	if err != nil {
		return resolvedHost{}, fmt.Errorf("resolveOn: %w", err)
	}
	h, err := reg.Resolve(spec)
	if err != nil {
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
				"host %q has no projects registered. Run: canopy host project add %s <project-name> <remote-path> (or pass --remote-cwd)",
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
