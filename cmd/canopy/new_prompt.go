// --prompt / --prompt-file flag glue for `canopy new`. The actual
// trust-dialog state machine + send-keys orchestration lives in
// internal/workspace/initprompt.go so the TUI ("From a prompt" picker
// option) can call the same machinery — both surfaces share one
// implementation of the only-claude-can-accept-this contract.
package main

import (
	"errors"
	"fmt"
	"os"
)

// promptMaxBytes caps --prompt-file content to a defensible size. Per
// the v3 design's failure-modes table: REJECT (not truncate) — silent
// truncation would change task instructions invisibly.
//
// 32KB is an arbitrary defensible round number that's large enough for
// any realistic single-message prompt and small enough that tmux's
// paste-buffer handles it without trouble.
const promptMaxBytes = 32 * 1024

// loadPrompt resolves --prompt and --prompt-file into a single string.
// Returns "" when no prompt was requested (caller skips the send path
// entirely). Returns a non-nil error for hard failures: mutually
// exclusive flags, unreadable file, oversized file. These all happen
// BEFORE workspace creation so the caller propagates as exit code 1
// (workspace not created).
func loadPrompt(promptFlag, promptFileFlag string) (string, error) {
	switch {
	case promptFlag != "" && promptFileFlag != "":
		return "", errors.New("--prompt and --prompt-file are mutually exclusive")
	case promptFlag != "":
		return promptFlag, nil
	case promptFileFlag != "":
		data, err := os.ReadFile(promptFileFlag)
		if err != nil {
			return "", fmt.Errorf("--prompt-file: %w", err)
		}
		if len(data) > promptMaxBytes {
			return "", fmt.Errorf(
				"prompt file too large (%d bytes; max %d). Split into multiple workspaces.",
				len(data), promptMaxBytes)
		}
		return string(data), nil
	default:
		return "", nil
	}
}
