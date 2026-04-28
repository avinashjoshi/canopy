// Migration helpers for the v1 → v2 schema change (basename-keyed projects
// → canonical-root-path-keyed projects). The actual data shape evolution
// is described in state.go's SchemaVersion constant; this file holds the
// pure helpers that lifecycle code uses to migrate state lazily.
//
// Lazy migration model: when canopy.json is first discovered for a project
// during a project-scoped command (canopy new / switch / rm / ls / TUI
// project mode), workspace.New holds the state lock and calls
// MigrateLegacyProject(basename, canonicalRoot). Idempotent — running it
// twice on the same state is a no-op the second time.
//
// FindBasenameCollision is the gate that enforces the v0.5 invariant
// "no two projects share a basename." Called by canopy init before
// writing canopy.json, and defensively by workspace.New before allocating
// a port. The gate doesn't try to repair pre-existing collisions; it just
// refuses to add a new one.

package state

import "path/filepath"

// MigrateLegacyProject moves a v1 (basename-keyed) project entry to v2
// (canonical-root-path-keyed) and backfills ProjectRoot on legacy Workspace
// rows belonging to the same project.
//
// Idempotency: safe to call repeatedly. Once migration has run for this
// (basename, canonicalRoot) pair, subsequent calls find no work to do and
// return cleanly.
//
// Migration steps:
//
//  1. If s.Projects has an entry keyed by basename AND no entry yet at
//     canonicalRoot: move the meta to the canonical key, set meta.Root,
//     delete the basename key. PortBase rides along untouched.
//  2. If s.Projects already has an entry at canonicalRoot but its Root
//     field is empty (could happen on a partial migration): backfill
//     meta.Root.
//  3. For every Workspace where Project == basename and ProjectRoot is
//     empty: set ProjectRoot = canonicalRoot. Project (legacy basename)
//     is preserved through v0.5 for backward compat with tools that
//     grep state.json.
//  4. If at least one entry was migrated and SchemaVersion < 2, bump it
//     to 2.
//
// The caller must hold the state lock; this mutates s in place.
func (s *State) MigrateLegacyProject(basename, canonicalRoot string) {
	if s.Projects == nil {
		s.Projects = map[string]ProjectMeta{}
	}

	migrated := false

	// Step 1: move basename-keyed Projects entry to root-keyed.
	if meta, ok := s.Projects[basename]; ok {
		if _, alreadyAtRoot := s.Projects[canonicalRoot]; !alreadyAtRoot {
			meta.Root = canonicalRoot
			s.Projects[canonicalRoot] = meta
			delete(s.Projects, basename)
			migrated = true
		}
	}

	// Step 2: self-heal a root-keyed entry whose Root field is empty.
	if meta, ok := s.Projects[canonicalRoot]; ok && meta.Root == "" {
		meta.Root = canonicalRoot
		s.Projects[canonicalRoot] = meta
		migrated = true
	}

	// Step 3: backfill ProjectRoot on legacy Workspace rows. We match by
	// the legacy Project basename; any row whose ProjectRoot is already
	// set is left alone (it's either already migrated or belongs to a
	// different project that happens to share the basename — though
	// FindBasenameCollision should prevent that case from existing).
	for i := range s.Workspaces {
		w := &s.Workspaces[i]
		if w.Project == basename && w.ProjectRoot == "" {
			w.ProjectRoot = canonicalRoot
			migrated = true
		}
	}

	// Step 4: version bump.
	if migrated && s.SchemaVersion < SchemaVersion {
		s.SchemaVersion = SchemaVersion
	}
}

// FindBasenameCollision returns the canonical root path of any *other*
// project in s.Projects whose basename matches canonicalRoot's basename,
// or "" if no collision exists. Used as the gate for v0.5+'s "no two
// projects share a basename" invariant.
//
// "Other" means: the same canonicalRoot is not a self-collision. If
// s.Projects already has an entry at canonicalRoot, this returns "" for
// that case (the caller is reconfirming an existing project, not creating
// a new one).
//
// Pure function: no state mutation. Safe to call without holding the lock,
// though callers that need to act on the result (refuse init, error out
// of workspace.New) should hold the lock for the check + write window.
//
// O(n) over s.Projects entries; n is the number of registered projects,
// typically <10. No optimization needed.
func (s *State) FindBasenameCollision(canonicalRoot string) string {
	if s.Projects == nil {
		return ""
	}
	target := filepath.Base(canonicalRoot)
	for root := range s.Projects {
		if root == canonicalRoot {
			continue
		}
		if filepath.Base(root) == target {
			return root
		}
	}
	return ""
}
