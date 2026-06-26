package workspace

// This file is _test.go-suffixed so its exports are visible ONLY to
// the workspace_test package (Go's convention for "internal test
// helpers without polluting the production API"). Production code
// can't see RunMigrateCurrentAgentsForTest, so the migration entry
// point stays unexported in the binary.

// RunMigrateCurrentAgentsForTest exposes migrateCurrentAgents for test
// code that constructs a *Manager manually (bypassing New) to exercise
// the migration without setting up a fake ~/.canopy. Production callers
// hit migrateCurrentAgents through New() instead.
func RunMigrateCurrentAgentsForTest(m *Manager) error {
	return m.migrateCurrentAgents()
}
