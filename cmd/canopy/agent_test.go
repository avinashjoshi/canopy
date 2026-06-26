package main

import "testing"

// TestIsInsideWorkspace pins the rel-classifying logic that
// findWorkspaceFromCwd uses to decide whether cwd lives inside a
// registered workspace. The codex review (P2 #3, 2026-06-25) caught a
// false-reject on dot-prefixed subdirectories — when cwd is one level
// inside .github or .config, filepath.Rel returns ".github" / ".config"
// and the old `rel[0] != '.'` test rejected them.
func TestIsInsideWorkspace(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
		why  string
	}{
		{".", true, "rel == . → cwd IS the workspace"},
		{"docs", true, "plain subdir"},
		{"docs/api/v1", true, "deep subdir"},
		{".github", true, "dot-prefixed subdir (the codex bug — .github used to false-reject)"},
		{".github/workflows", true, "deep inside dot-prefixed subdir"},
		{".config/canopy", true, "another dot-prefixed subdir we'd expect to land in"},
		{".gstack/tmp", true, "canopy's own .gstack subdir"},
		{"..", false, "rel == .. → cwd is the parent of wsPath"},
		{"../sibling", false, "rel == ../sibling → cwd is a sibling, outside"},
		{"../../other-project", false, "deeper outside"},
	}
	for _, c := range cases {
		t.Run(c.rel, func(t *testing.T) {
			got := isInsideWorkspace(c.rel)
			if got != c.want {
				t.Errorf("isInsideWorkspace(%q) = %v; want %v (%s)", c.rel, got, c.want, c.why)
			}
		})
	}
}
