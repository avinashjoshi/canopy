// Initial-prompt delivery for freshly-created workspaces. Shared by
// the `canopy new --prompt` CLI path (cmd/canopy/new.go) and the
// TUI's "From a prompt" picker option (internal/ui).
//
// The trust-dialog state machine + claude-rendering verify are the
// load-bearing pieces — without them, a freshly-created workspace's
// prompt would either get eaten by claude's first-launch trust dialog
// or get typed into the keepAlive shell when claude crashed.
//
// History: this code lived in cmd/canopy/new_prompt.go through v0.16.1
// and moved here when the TUI grew its own "new from prompt" flow —
// both call sites needed the same state machine, so the function got
// promoted to the workspace package (the same package that owns
// Manager.Create, the operation it complements).
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/tmux"
)

var promptLog = clog.Pkg("workspace-prompt")

// ErrPromptFailed signals "workspace created OK but the prompt was not
// delivered." Callers inspect via errors.As to distinguish "workspace
// creation failed" from "workspace OK, agent didn't get its prompt."
// cmd/canopy/main.go uses this distinction to pick exit code 2 (prompt
// failed) vs 1 (general failure).
type ErrPromptFailed struct {
	Reason string
}

func (e *ErrPromptFailed) Error() string {
	return "prompt not sent: " + e.Reason
}

// SendInitialPrompt orchestrates the trust-dialog state machine and
// the actual send-keys for a freshly-created workspace's agent pane.
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
// Returns nil on success. Returns *ErrPromptFailed (caller propagates
// as exit code 2) for "workspace OK, prompt skipped." Returns plain
// error only for unexpected tmux failures that aren't classified as
// prompt-send problems.
//
// progress is the writer used for the "Waiting for agent..." carriage-
// return progress line. Pass io.Discard from non-TTY callers.
func SendInitialPrompt(
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
		return &ErrPromptFailed{Reason: "no pane tagged agent:* in session " + sessionName}
	}
	agentPane := agentPanes[0]
	launcher := agent.LauncherFromRole(agentPane.Role)
	if launcher == "" {
		return &ErrPromptFailed{
			Reason: fmt.Sprintf("malformed @canopy-role tag %q (cannot derive launcher)", agentPane.Role),
		}
	}
	// v0.22: dispatch by classifier instead of the v0.16-era launcher==
	// "claude" gate. Each launcher's Classifier provides IsRendering /
	// IsTrustDialog; --prompt now works for any launcher with
	// registered patterns (claude, codex). Stub launchers (opencode,
	// aider) return false from both helpers, so the Phase-1 trust-loop
	// times out without sending anything — same observable outcome as
	// the old gate but without the special case.
	classifier := agent.ClassifierFor(launcher)

	if err := awaitAgentReady(ctx, tx, agentPane.ID, classifier, progress); err != nil {
		return err
	}

	// Phase 3: re-verify the agent is rendering, not the keepAlive
	// shell. awaitAgentReady already saw an agent marker once; this
	// guards against the rare race where the agent crashed BETWEEN
	// Phase 2 exit and now.
	captured, err := capturePaneTimeout(ctx, tx, agentPane.ID, 500*time.Millisecond)
	if err != nil {
		return &ErrPromptFailed{Reason: "Phase 3 verify capture-pane failed: " + err.Error()}
	}
	if !classifier.IsRendering(captured) {
		return &ErrPromptFailed{
			Reason: fmt.Sprintf(
				"agent pane is shell, not %s (agent may have crashed); refusing to send-keys to defend against command injection",
				launcher),
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
			return &ErrPromptFailed{Reason: "paste-buffer failed: " + err.Error()}
		}
	} else {
		sendCtx, sendCancel := context.WithTimeout(ctx, 2*time.Second)
		err := tx.SendKeysLiteral(sendCtx, agentPane.ID, prompt)
		sendCancel()
		if err != nil {
			return &ErrPromptFailed{Reason: "send-keys -l failed: " + err.Error()}
		}
	}
	// Submit with a separate Enter keypress.
	enterCtx, enterCancel := context.WithTimeout(ctx, 2*time.Second)
	err = tx.SendKeyName(enterCtx, agentPane.ID, "Enter")
	enterCancel()
	if err != nil {
		return &ErrPromptFailed{Reason: "send-keys Enter failed: " + err.Error()}
	}

	promptLog.Info("initial prompt sent",
		"session", sessionName, "pane", agentPane.ID, "bytes", len(prompt))
	return nil
}

// IsPromptFailed is a typed convenience for the errors.As pattern.
// Returns (ErrPromptFailed, true) when err (or any error it wraps) is
// a *ErrPromptFailed. Lets call sites avoid threading the variable
// declaration through their own type-assertion boilerplate.
func IsPromptFailed(err error) (*ErrPromptFailed, bool) {
	var pf *ErrPromptFailed
	if errors.As(err, &pf) {
		return pf, true
	}
	return nil, false
}

// awaitAgentReady runs Phase 1 + Phase 2 of the trust state machine.
// Returns nil when the agent is verified rendering. Returns
// *ErrPromptFailed for either phase timeout.
//
// classifier is the per-launcher Classifier (from
// agent.ClassifierFor(launcher)); its IsRendering / IsTrustDialog drive
// the dispatch. For stub launchers (opencode/aider), both return
// false, so the trust-dialog branch never fires and the ready-marker
// branch never matches — Phase 1 times out cleanly. The pre-v0.22
// behavior was a hardcoded gate against `launcher != "claude"`; the
// timeout-with-classifier-stub is observationally equivalent.
func awaitAgentReady(
	ctx context.Context,
	tx *tmux.Client,
	paneID string,
	classifier agent.Classifier,
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
			if classifier.IsRendering(captured) {
				clearProgress(progress)
				return nil
			}
			if classifier.IsTrustDialog(captured) {
				trustSeen = true
				dismissCtx, dismissCancel := context.WithTimeout(ctx, 2*time.Second)
				err := tx.SendKeyName(dismissCtx, paneID, "Enter")
				dismissCancel()
				if err != nil {
					clearProgress(progress)
					return &ErrPromptFailed{
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
			classifier.IsRendering(captured) {
			return nil
		}
		return &ErrPromptFailed{
			Reason: "Phase 1 timeout: neither trust dialog nor agent ready marker appeared in 5s",
		}
	}

	// Phase 2: post-trust ready marker.
	phase2Start := time.Now()
	phase2Deadline := phase2Start.Add(phaseBudget)
	for time.Now().Before(phase2Deadline) {
		captured, err := capturePaneTimeout(ctx, tx, paneID, pollInterval)
		if err == nil && classifier.IsRendering(captured) {
			clearProgress(progress)
			return nil
		}
		elapsed := time.Since(phase2Start).Round(time.Second)
		fmt.Fprintf(progress, "\rWaiting for agent (post-trust)... %s / %s ", elapsed, phaseBudget)
		time.Sleep(pollInterval)
	}
	clearProgress(progress)
	return &ErrPromptFailed{
		Reason: "Phase 2 timeout: agent ready marker never appeared in 5s after trust dismiss",
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
