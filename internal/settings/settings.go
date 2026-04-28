// Package settings holds canopy's per-machine global configuration —
// distinct from internal/config which loads per-project canopy.json.
//
// Settings live at ~/.canopy/config.json. The file is optional; if it
// doesn't exist, Default() values apply. Anything in the file is merged
// over the defaults so users can override only the fields they care
// about and leave the rest at the recommended values.
//
// Today the only setting is port allocation strategy (base port,
// project stride, workspace stride). Future additions (default editor,
// pane layout overrides, AI tool choice) will land here as new fields.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileName is the on-disk filename canopy looks for under canopyHome.
const FileName = "config.json"

// Settings is the parsed content of ~/.canopy/config.json plus the
// defaults for any missing fields.
type Settings struct {
	Ports PortPlan `json:"ports"`
}

// PortPlan controls how canopy allocates TCP ports across projects and
// workspaces.
//
// The model: each project gets a base port (Base + N×ProjectStride for
// the N-th project canopy has seen). Workspaces within a project use
// project_base + M×WorkspaceStride for the M-th workspace, where M is
// chosen as the smallest free slot inside the project's range.
//
// With defaults (Base=3000, ProjectStride=1000, WorkspaceStride=10):
//
//	cravd  workspace 1 -> 3000   workspace 2 -> 3010   workspace 3 -> 3020
//	brain  workspace 1 -> 4000   workspace 2 -> 4010
//	hey    workspace 1 -> 5000
//
// WorkspaceStride > 1 leaves room for projects whose dev server uses
// adjacent ports (e.g. Rails on 3000 + Sidekiq on 3001 + Redis on 3002).
// ProjectStride = 1000 means each project has 100 workspace slots before
// it bumps into the next project — far more than any real workflow.
type PortPlan struct {
	Base            int `json:"base"`
	ProjectStride   int `json:"project_stride"`
	WorkspaceStride int `json:"workspace_stride"`
}

// Default returns the recommended settings. Used when no config.json
// exists, and as the merge baseline for partial configs.
//
// Base=40000 deliberately avoids the 3000-9000 range where webapps
// commonly live (Rails, Next.js, Django, Flask, generic http servers).
// 40000 is well above the k8s NodePort range (30000-32767) and below
// the IANA ephemeral range (49152+), so it almost never collides with
// other tooling on a developer machine. With ProjectStride=1000, the
// realistic 50-project ceiling fits comfortably in 40000-90000.
func Default() Settings {
	return Settings{
		Ports: PortPlan{
			Base:            40000,
			ProjectStride:   1000,
			WorkspaceStride: 10,
		},
	}
}

// Load reads ~/.canopy/config.json (rooted at canopyHome). Missing file
// returns Default() with nil error — config is optional.
//
// The JSON is unmarshaled OVER the default values, so partial configs
// that only set one field keep the defaults for the others. That means
// "Base":3500 in the file but no project_stride still gives a sensible
// project_stride of 1000.
func Load(canopyHome string) (Settings, error) {
	path := filepath.Join(canopyHome, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Default(), fmt.Errorf("settings.Load: read %s: %w", path, err)
	}

	s := Default()
	if err := json.Unmarshal(data, &s); err != nil {
		return Default(), fmt.Errorf("settings.Load: parse %s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return Default(), fmt.Errorf("settings.Load(%s): %w", path, err)
	}
	return s, nil
}

// validate sanity-checks the loaded settings. Returns descriptive errors
// for misconfigurations rather than letting the workspace orchestrator
// fail with a confusing port-allocation message later.
func (s Settings) validate() error {
	if s.Ports.Base < 1024 || s.Ports.Base > 65535 {
		return fmt.Errorf("ports.base %d out of valid TCP range 1024-65535", s.Ports.Base)
	}
	if s.Ports.ProjectStride <= 0 {
		return fmt.Errorf("ports.project_stride %d must be > 0", s.Ports.ProjectStride)
	}
	if s.Ports.WorkspaceStride <= 0 {
		return fmt.Errorf("ports.workspace_stride %d must be > 0", s.Ports.WorkspaceStride)
	}
	if s.Ports.WorkspaceStride > s.Ports.ProjectStride {
		return fmt.Errorf("ports.workspace_stride (%d) cannot exceed ports.project_stride (%d)",
			s.Ports.WorkspaceStride, s.Ports.ProjectStride)
	}
	return nil
}
