package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// envAllowNested is the escape hatch for users who genuinely need to
// run canopy from inside an existing tmux session — typically tests,
// or future tmux-status-line integrations that call `canopy <some-readonly>`
// to populate a status segment. Default behavior is to refuse: nesting
// canopy inside its own tmux server confuses attach/detach plumbing
// (tmux balks at `attach-session` while you're already attached) and
// nesting canopy inside an existing canopy workspace is the failure
// mode this guard exists to prevent in the first place.
const envAllowNested = "CANOPY_ALLOW_NESTED"

// allowInTmuxAnnotation is the cobra annotation key that opts a
// subcommand out of the no-nesting guard. Today only `version` carries
// it — version is the canonical "is canopy installed?" probe and must
// answer cleanly regardless of where it runs. Annotation value of
// "true" enables the carve-out.
const allowInTmuxAnnotation = "allow-in-tmux"

// enforceNoNestedTmux returns a non-nil error when canopy is invoked
// from inside an existing tmux session unless the command opts out
// (annotation) or the user opts out (env var).
//
// Detection signal: `$TMUX` is set by tmux itself for every process
// running inside an attached session. Both "user is inside ambient
// tmux" and "user is inside a canopy workspace" set TMUX, so one
// check covers both flavors. The error message tells them apart via
// `$CANOPY_WORKSPACE_PATH` (canopy sets this on every workspace
// session) so the message can be specific.
func enforceNoNestedTmux(cmd *cobra.Command) error {
	if os.Getenv("TMUX") == "" {
		return nil
	}
	if os.Getenv(envAllowNested) == "1" {
		return nil
	}
	if cmd != nil && cmd.Annotations[allowInTmuxAnnotation] == "true" {
		return nil
	}

	location := "inside a tmux session"
	if os.Getenv("CANOPY_WORKSPACE_PATH") != "" {
		location = "inside a canopy workspace (which is itself a tmux session)"
	}

	return fmt.Errorf(
		"canopy refuses to run %s.\n\n"+
			"  Canopy launches its own tmux sessions, so nesting it confuses tmux's\n"+
			"  attach/detach plumbing — and nesting canopy inside a canopy workspace\n"+
			"  is almost always a mistake (you'd lose track of which workspace you're\n"+
			"  acting on).\n\n"+
			"  Detach the current tmux session first (prefix-d on your tmux binding),\n"+
			"  then run canopy from the outer terminal.\n\n"+
			"  If you really need to bypass this (testing, scripting), set %s=1.",
		location, envAllowNested)
}
