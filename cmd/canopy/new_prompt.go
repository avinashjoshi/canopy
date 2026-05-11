// --prompt / --prompt-file glue for `canopy new`. Builds on the
// agent.Detector + agent.IsClaudeRendering / IsTrustDialog helpers and
// the tmux.SendKeysLiteral / LoadAndPasteBuffer / CapturePane helpers
// added for the v0.16.1 background-workspaces feature.
//
// The trust-dialog state machine + claude-rendering verify are the
// load-bearing pieces — without them, a freshly-created workspace's
// prompt would either get eaten by claude's first-launch trust dialog
// or get typed into the keepAlive shell when claude crashed.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

var promptLog = clog.Pkg("cmd-prompt")

// promptMaxBytes caps --prompt-file content to a defensible size. Per
// the v3 design's failure-modes table: REJECT (not truncate) — silent
// truncation would change task instructions invisibly.
//
// 32KB is an arbitrary defensible round number that's large enough for
// any realistic single-message prompt and small enough that tmux's
// paste-buffer handles it without trouble.
const promptMaxBytes = 32 * 1024

// errPromptFailed signals "workspace created OK but the prompt was not
// delivered." main.go inspects via errors.As and uses exit code 2 so
// scripts can distinguish "workspace creation failed" (exit 1) from
// "workspace OK, agent didn't get its prompt" (exit 2).
type errPromptFailed struct {
	Reason string
}

func (e *errPromptFailed) Error() string {
	return "prompt not sent: " + e.Reason
}

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

// sendInitialPrompt orchestrates the trust-dialog state machine and
// the actual send-keys for a freshly-created workspace.
//
// Phases (locked via codex review of the v3 design):
//
//  1. (≤5s) Poll capture-pane every 0.5s for the trust dialog OR a
//     claude-ready marker. If trust → dismiss with Enter, advance to
//     Phase 2. If ready → skip to Phase 3. If timeout → fail.
//
//  2. (≤5s) Poll for the claude-ready marker after trust dismiss.
//     Same timeout semantics.
//
//  3. (verify) Re-capture and confirm at least one claude-only marker
//     is visible. If only shell content (keepAlive fallback after
//     claude crash), abort BEFORE send-keys to defend against typing
//     the user's prompt into a shell that would interpret shell
//     metacharacters.
//
//  4. (send) Single-line prompts use send-keys -l (literal mode so
//     words like `Enter` aren't interpreted as keypresses).
//     Multi-line prompts use a named tmux buffer + paste-buffer to
//     avoid clobbering concurrent canopy new --prompt-file calls.
//     Either path finishes with a separate Enter keypress to submit.
//
// Returns nil on success. Returns *errPromptFailed (caller propagates
// as exit code 2) for "workspace OK, prompt skipped." Returns plain
// error only for unexpected tmux failures that aren't classified as
// prompt-send problems.
func sendInitialPrompt(
	ctx context.Context,
	tx *tmux.Client,
	sessionName, workspaceName, prompt string,
	progress io.Writer,
) error {
	agentPanes, err := tx.LookupAllPanes(ctx, sessionName, "agent:*")
	if err != nil {
		return fmt.Errorf("LookupAllPanes(%s, agent:*): %w", sessionName, err)
	}
	if len(agentPanes) == 0 {
		return &errPromptFailed{Reason: "no pane tagged agent:* in session " + sessionName}
	}
	agentPane := agentPanes[0]
	launcher := agent.LauncherFromRole(agentPane.Role)
	if launcher == "" {
		return &errPromptFailed{
			Reason: fmt.Sprintf("malformed @canopy-role tag %q (cannot derive launcher)", agentPane.Role),
		}
	}
	if launcher != "claude" {
		return &errPromptFailed{
			Reason: fmt.Sprintf(
				"--prompt is only supported for agent:claude in v0.16.1 (got agent:%s)",
				launcher),
		}
	}

	if err := awaitClaudeReady(ctx, tx, agentPane.ID, progress); err != nil {
		return err
	}

	// Phase 3: re-verify claude is rendering, not the keepAlive shell.
	// awaitClaudeReady already saw a claude marker once; this guards
	// against the rare race where claude crashed BETWEEN Phase 2 exit
	// and now.
	captured, err := capturePaneTimeout(ctx, tx, agentPane.ID, 500*time.Millisecond)
	if err != nil {
		return &errPromptFailed{Reason: "Phase 3 verify capture-pane failed: " + err.Error()}
	}
	if !agent.IsClaudeRendering(captured) {
		return &errPromptFailed{
			Reason: "agent pane is shell, not claude (claude may have crashed); refusing to send-keys to defend against command injection",
		}
	}

	// Phase 4: send the prompt body. Every tmux call gets a 2s
	// per-call timeout — long enough for paste-buffer of a 32KB file
	// to complete on a healthy server, short enough that a wedged
	// tmux server can't hang the canopy CLI.
	if strings.Contains(prompt, "\n") {
		bufName := tmux.SafeName("canopy-" + workspaceName + "-prompt")
		bufCtx, bufCancel := context.WithTimeout(ctx, 2*time.Second)
		err := tx.LoadAndPasteBuffer(bufCtx, agentPane.ID, bufName, prompt)
		bufCancel()
		if err != nil {
			return &errPromptFailed{Reason: "paste-buffer failed: " + err.Error()}
		}
	} else {
		sendCtx, sendCancel := context.WithTimeout(ctx, 2*time.Second)
		err := tx.SendKeysLiteral(sendCtx, agentPane.ID, prompt)
		sendCancel()
		if err != nil {
			return &errPromptFailed{Reason: "send-keys -l failed: " + err.Error()}
		}
	}
	// Submit with a separate Enter keypress.
	enterCtx, enterCancel := context.WithTimeout(ctx, 2*time.Second)
	err = tx.SendKeyName(enterCtx, agentPane.ID, "Enter")
	enterCancel()
	if err != nil {
		return &errPromptFailed{Reason: "send-keys Enter failed: " + err.Error()}
	}

	promptLog.Info("initial prompt sent",
		"session", sessionName, "pane", agentPane.ID, "bytes", len(prompt))
	return nil
}

// awaitClaudeReady runs Phase 1 + Phase 2 of the trust state machine.
// Returns nil when claude is verified rendering. Returns
// *errPromptFailed for either phase timeout.
func awaitClaudeReady(
	ctx context.Context,
	tx *tmux.Client,
	paneID string,
	progress io.Writer,
) error {
	const (
		pollInterval = 500 * time.Millisecond
		phaseBudget  = 5 * time.Second
	)

	// Phase 1: trust OR ready.
	phase1Start := time.Now()
	phase1Deadline := phase1Start.Add(phaseBudget)
	var trustSeen bool
	for time.Now().Before(phase1Deadline) {
		captured, err := capturePaneTimeout(ctx, tx, paneID, pollInterval)
		if err == nil {
			if agent.IsClaudeRendering(captured) {
				clearProgress(progress)
				return nil
			}
			if agent.IsTrustDialog(captured) {
				trustSeen = true
				dismissCtx, dismissCancel := context.WithTimeout(ctx, 2*time.Second)
				err := tx.SendKeyName(dismissCtx, paneID, "Enter")
				dismissCancel()
				if err != nil {
					clearProgress(progress)
					return &errPromptFailed{
						Reason: "send-keys Enter to dismiss trust failed: " + err.Error(),
					}
				}
				break
			}
		}
		elapsed := time.Since(phase1Start).Round(time.Second)
		fmt.Fprintf(progress, "\rWaiting for agent... %s / %s ", elapsed, phaseBudget)
		time.Sleep(pollInterval)
	}
	clearProgress(progress)
	if !trustSeen {
		// One more capture in case the ready marker rendered right at
		// the deadline (avoids a flake-driven timeout).
		if captured, err := capturePaneTimeout(ctx, tx, paneID, pollInterval); err == nil &&
			agent.IsClaudeRendering(captured) {
			return nil
		}
		return &errPromptFailed{
			Reason: "Phase 1 timeout: neither trust dialog nor claude ready marker appeared in 5s",
		}
	}

	// Phase 2: post-trust ready marker.
	phase2Start := time.Now()
	phase2Deadline := phase2Start.Add(phaseBudget)
	for time.Now().Before(phase2Deadline) {
		captured, err := capturePaneTimeout(ctx, tx, paneID, pollInterval)
		if err == nil && agent.IsClaudeRendering(captured) {
			clearProgress(progress)
			return nil
		}
		elapsed := time.Since(phase2Start).Round(time.Second)
		fmt.Fprintf(progress, "\rWaiting for claude (post-trust)... %s / %s ", elapsed, phaseBudget)
		time.Sleep(pollInterval)
	}
	clearProgress(progress)
	return &errPromptFailed{
		Reason: "Phase 2 timeout: claude ready marker never appeared in 5s after trust dismiss",
	}
}

// clearProgress overwrites the carriage-return progress line with
// spaces so subsequent output starts at column 0 of a clean line.
func clearProgress(w io.Writer) {
	fmt.Fprint(w, "\r"+strings.Repeat(" ", 70)+"\r")
}

// capturePaneTimeout wraps tx.CapturePane with a per-call timeout. The
// codex review insists every tmux call has a deadline so a hung tmux
// server can't wedge the caller (M14).
func capturePaneTimeout(
	parent context.Context,
	tx *tmux.Client,
	paneID string,
	d time.Duration,
) (string, error) {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	return tx.CapturePane(ctx, paneID)
}
