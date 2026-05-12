package main

import (
	"fmt"
	"strings"

	"github.com/avinashjoshi/canopy/internal/host"
)

// resolvedHost is what dispatchNewToRemote / dispatchSwitchToRemote
// need to know: the SSH target string and the default project path
// (if registered). Either field may be empty for raw-SSH-target use.
type resolvedHost struct {
	SSHTarget   string
	ProjectPath string // empty if --on was a raw target, or registry entry had no project_path
	Source      string // "registry:<name>" or "raw-target" — for friendly error/log messages
}

// resolveOn turns the value of `--on` into a usable SSH target.
//
// Heuristic: if the value contains @ or :, it's already a fully-qualified
// SSH target (user@host or host:port). Otherwise it's a registry name
// to look up in ~/.canopy/hosts.json.
//
// This preserves Phase 0 back-compat: existing scripts that pass
// `--on cassy@tower.tail.ts.net` keep working without re-registration.
// The new ergonomic case `--on tower` just adds a lookup step on top.
//
// Returns a clear "did you forget canopy host add?" error when a bare
// name isn't in the registry — much better than the silent SSH failure
// you'd get from passing a bare hostname to ssh.
func resolveOn(spec string) (resolvedHost, error) {
	if spec == "" {
		return resolvedHost{}, fmt.Errorf("resolveOn: empty --on value")
	}
	// Raw SSH target — looks like `user@host`, `host:port`, or any
	// other form ssh's argv parser accepts. We don't try to validate;
	// ssh will reject malformed targets at exec time with a clear
	// error.
	if strings.ContainsAny(spec, "@:") {
		return resolvedHost{
			SSHTarget: spec,
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
		// Specific helpful error when name isn't registered.
		// Stays compatible with errors.Is(err, host.ErrHostNotFound)
		// so callers can branch on it if needed later.
		return resolvedHost{}, fmt.Errorf(
			"host %q not registered in ~/.canopy/hosts.json. Run `canopy host add %s <ssh-target>` or pass --on <ssh-target> directly: %w",
			spec, spec, err)
	}
	if h.Type != "ssh" {
		return resolvedHost{}, fmt.Errorf(
			"resolveOn: host %q has type %q; only \"ssh\" is supported in v0.17.0",
			spec, h.Type)
	}
	return resolvedHost{
		SSHTarget:   h.SSHTarget,
		ProjectPath: h.ProjectPath,
		Source:      "registry:" + spec,
	}, nil
}

// ensureHostPkgLinked silences the unused-import linter when the
// host package would otherwise be referenced only via errors.Is.
// Removed when callers actually use the package symbol.
var _ = host.ErrHostNotFound
