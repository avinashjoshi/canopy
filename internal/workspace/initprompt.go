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
	"os"
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
//  1. (≤promptPhaseBudget, default 5s) Poll capture-pane every 0.5s for
//     the trust dialog OR a claude-ready marker. If trust → dismiss
//     with Enter, advance to Phase 2. If ready → skip to Phase 3. If
//     timeout → fail. Budget is longer by default when
//     CANOPY_REMOTE_DISPATCH is set (see promptPhaseBudget) — the
//     whole process, including this poll loop, runs on the remote
//     host for `canopy new --on <host>`, and Claude reliably takes
//     longer than 5s to render there.
//
//  2. (≤promptPhaseBudget) Poll for the claude-ready marker after
//     trust dismiss. Same timeout semantics.
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
	if launcher != "claude" {
		return &ErrPromptFailed{
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
		return &ErrPromptFailed{Reason: "Phase 3 verify capture-pane failed: " + err.Error()}
	}
	if !agent.IsClaudeRendering(captured) {
		return &ErrPromptFailed{
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

// awaitClaudeReady runs Phase 1 + Phase 2 of the trust state machine.
// Returns nil when claude is verified rendering. Returns
// *ErrPromptFailed for either phase timeout.
func awaitClaudeReady(
	ctx context.Context,
	tx *tmux.Client,
	paneID string,
	progress io.Writer,
) error {
	const pollInterval = 500 * time.Millisecond
	budget := promptPhaseBudget()

	// Phase 1: race ready-OR-trust. awaitPaneOutput doesn't know about
	// trust dismissal — that's this function's concern, not the generic
	// poller's — so inspect the captured text it returns to decide which
	// condition actually matched.
	captured, err := awaitPaneOutput(ctx, tx, paneID, budget, pollInterval, progress,
		"Waiting for agent...",
		fmt.Sprintf("Phase 1 timeout: neither trust dialog nor claude ready marker appeared in %s", budget),
		func(s string) bool { return agent.IsClaudeRendering(s) || agent.IsTrustDialog(s) },
	)
	if err != nil {
		return err
	}
	if agent.IsClaudeRendering(captured) {
		return nil
	}
	// Trust dialog matched. Dismiss it, then fall through to Phase 2.
	dismissCtx, dismissCancel := context.WithTimeout(ctx, 2*time.Second)
	err = tx.SendKeyName(dismissCtx, paneID, "Enter")
	dismissCancel()
	if err != nil {
		return &ErrPromptFailed{
			Reason: "send-keys Enter to dismiss trust failed: " + err.Error(),
		}
	}

	// Phase 2: post-trust ready marker.
	_, err = awaitPaneOutput(ctx, tx, paneID, budget, pollInterval, progress,
		"Waiting for claude (post-trust)...",
		fmt.Sprintf("Phase 2 timeout: claude ready marker never appeared in %s after trust dismiss", budget),
		agent.IsClaudeRendering,
	)
	return err
}

// EnvRemoteDispatch is the env var buildRemoteScript (cmd/canopy/new.go)
// sets unconditionally on every `canopy new --on <host>` dispatch, and
// promptPhaseBudget reads to detect that this process is running on the
// remote host itself. Exported as a shared constant so the two sites
// can't drift apart on the literal string.
const EnvRemoteDispatch = "CANOPY_REMOTE_DISPATCH"

// Default prompt-phase budget tiers. See promptPhaseBudget.
const (
	defaultLocalBudget  = 5 * time.Second
	defaultRemoteBudget = 15 * time.Second
)

// promptPhaseBudget resolves the wall-clock budget for each phase of
// awaitClaudeReady. Three tiers, checked in order:
//
//  1. CANOPY_PROMPT_PHASE_BUDGET, an explicit override (e.g. "15s"),
//     parsed via time.ParseDuration. A malformed value, or a
//     zero/negative duration (ParseDuration happily accepts "0s"/"-1s"
//     — that's not "malformed" to it, but a zero-or-negative budget
//     would skip awaitPaneOutput's poll loop entirely and rely solely
//     on its one grace-period capture), falls through to the next tier
//     rather than being honored — this is a best-effort escape hatch,
//     not a strict config surface.
//  2. EnvRemoteDispatch: set unconditionally by buildRemoteScript on
//     every `canopy new --on <host>` dispatch (cmd/canopy/new.go),
//     since the entire `canopy new` process — including this poll
//     loop's tmux capture-pane calls — runs on the remote host itself.
//     Claude reliably takes longer than 5s to reach its ready marker
//     on a fresh remote install, so the default is longer here.
//  3. Otherwise: defaultLocalBudget, unchanged from the original
//     hardcoded local default (TUI's in-process "from a prompt" flow
//     never sets EnvRemoteDispatch, so it keeps failing fast as before).
func promptPhaseBudget() time.Duration {
	if v := os.Getenv("CANOPY_PROMPT_PHASE_BUDGET"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		} else if err != nil {
			promptLog.Warn("invalid CANOPY_PROMPT_PHASE_BUDGET, ignoring", "value", v, "err", err)
		} else {
			promptLog.Warn("CANOPY_PROMPT_PHASE_BUDGET must be positive, ignoring", "value", v)
		}
	}
	if os.Getenv(EnvRemoteDispatch) != "" {
		return defaultRemoteBudget
	}
	return defaultLocalBudget
}

// awaitPaneOutput polls capture-pane every pollInterval until
// match(captured) is true, ctx is done, or budget elapses. Reports
// "<label> <elapsed> / <budget>" to progress each tick. On budget
// exhaustion, does one grace-period re-check right at the deadline
// (guards against a flake-driven timeout when the match condition
// renders right as the deadline passes) before giving up.
//
// Returns the captured text on success. Returns
// *ErrPromptFailed{Reason: timeoutMsg} on timeout, or a plain error if
// ctx is cancelled/expires independently of budget.
func awaitPaneOutput(
	ctx context.Context,
	tx *tmux.Client,
	paneID string,
	budget, pollInterval time.Duration,
	progress io.Writer,
	label, timeoutMsg string,
	match func(captured string) bool,
) (string, error) {
	start := time.Now()
	deadline := start.Add(budget)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			clearProgress(progress)
			return "", ctx.Err()
		}
		captured, err := capturePaneTimeout(ctx, tx, paneID, pollInterval)
		if err == nil && match(captured) {
			clearProgress(progress)
			return captured, nil
		}
		elapsed := time.Since(start).Round(time.Second)
		fmt.Fprintf(progress, "\r%s %s / %s ", label, elapsed, budget)
		select {
		case <-ctx.Done():
			clearProgress(progress)
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	clearProgress(progress)
	// One more capture in case the match condition rendered right at the
	// deadline (avoids a flake-driven timeout).
	if captured, err := capturePaneTimeout(ctx, tx, paneID, pollInterval); err == nil && match(captured) {
		return captured, nil
	}
	return "", &ErrPromptFailed{Reason: timeoutMsg}
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
