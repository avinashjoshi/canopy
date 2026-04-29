// Package agent owns canopy's launcher map and briefing assembly for the
// agent pane. It's the v0.6 agent-agnostic injection point: when canopy
// spawns the agent in a workspace's tmux pane, this package decides
// WHICH agent to spawn (based on canopy.json's agent.type) and HOW to
// hand it the assembled workspace briefing.
//
// Why a Go map and not config-driven scripts: the map is the single
// source of truth for "which agents canopy supports." Adding a new agent
// is one PR adding a map entry — internally consistent, type-safe, and
// always in sync with the canopy version that ships with it. Users who
// need full control still get it via canopy.json's `scripts.agent` (a
// path to their own launcher script that bypasses the map entirely).
//
// Briefing assembly + the hybrid fresh-vs-resume strategy live in
// briefing.go. This file only owns the launcher dispatch.
package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/oncactus/canopy/internal/clog"
)

var log = clog.Pkg("agent")

// ErrUnknownAgent is returned by Resolve when canopy.json's agent.type
// doesn't match any built-in launcher. The error message includes the
// list of known types so users can fix the typo without re-reading docs.
var ErrUnknownAgent = errors.New("agent: unknown agent type")

// BriefingMode describes how a launcher accepts the canopy-assembled
// briefing. Different agents have different conventions:
//
//   - BriefingInline:    pass the briefing text via a CLI flag value
//     (claude --append-system-prompt "...", codex --instructions "...").
//     Briefing is interpolated into the args slice
//     before exec.
//   - BriefingFile:      write the briefing to a temp file, pass the
//     path via a flag (aider --message-file <path>).
//     canopy creates the temp file and registers it
//     for cleanup after the agent exits.
//   - BriefingAgentsMd:  write the briefing to <worktree>/AGENTS.md
//     (opencode reads it natively from cwd). canopy
//     also adds AGENTS.md to .gitignore on init for
//     agents using this mode, so it doesn't leak into
//     the user's commits.
//
// New agents that need a different convention add a new mode here +
// the dispatch arm in workspace/lifecycle.go's launcher invocation.
type BriefingMode int

const (
	BriefingInline BriefingMode = iota
	BriefingFile
	BriefingAgentsMd
)

// Launcher is one entry in the built-in agent map. Cmd is the binary
// name (looked up via exec.LookPath at spawn time). Resume + Fresh are
// the argv tails for the two invocation contexts; the literal token
// "{{briefing}}" inside an arg is replaced with either the inline
// briefing string (BriefingInline) or the temp-file path (BriefingFile).
//
// The split between Resume and Fresh is for agents that have different
// flags for "continue prior session" vs "start new" (claude --continue
// vs no flag). Agents without a session-continuation concept set both
// to the same args.
type Launcher struct {
	Cmd          string
	Resume       []string
	Fresh        []string
	BriefingMode BriefingMode
}

// defaults is the registry of canopy-supported agents. Order is
// canonical: claude is the canonical default (Avi's primary agent;
// also the v0.5 hardcoded behavior).
//
// Adding a new agent:
//  1. Add an entry here.
//  2. If the agent uses a new BriefingMode, add the dispatch arm in
//     workspace/lifecycle.go where the launcher's argv gets assembled.
//  3. Add a smoke test in launchers_test.go.
//  4. Document the support in docs/canopy-json.md's agent matrix.
//
// Three things kept consistent across all entries:
//   - Cmd matches the agent's invocation binary as installed via the
//     agent's standard install path. We don't try to be clever about
//     finding the binary — exec.LookPath at spawn time gives a clean
//     error if it's not on PATH.
//   - Briefings are NEVER positional args. The "{{briefing}}" token
//     always appears as the value of a named flag — keeps the command
//     line robust against agents that add new positional args.
//   - Resume and Fresh end with the briefing argument so we can drop
//     it cleanly when the briefing is empty (resume + no hints).
var defaults = map[string]Launcher{
	"claude": {
		Cmd:          "claude",
		Resume:       []string{"--continue", "--append-system-prompt", "{{briefing}}"},
		Fresh:        []string{"--append-system-prompt", "{{briefing}}"},
		BriefingMode: BriefingInline,
	},
	"codex": {
		Cmd: "codex",
		// Codex's CLI is still moving; --instructions is the closest
		// equivalent to claude's --append-system-prompt as of 2026-04.
		// Update this entry when codex stabilizes its system-prompt
		// surface. No --resume distinction today — codex resume
		// requires a thread ID we don't track.
		Resume:       []string{"--instructions", "{{briefing}}"},
		Fresh:        []string{"--instructions", "{{briefing}}"},
		BriefingMode: BriefingInline,
	},
	"opencode": {
		Cmd:          "opencode",
		Resume:       []string{},
		Fresh:        []string{},
		BriefingMode: BriefingAgentsMd,
	},
	"aider": {
		Cmd:          "aider",
		Resume:       []string{"--restore-chat-history", "--message-file", "{{briefing}}"},
		Fresh:        []string{"--message-file", "{{briefing}}"},
		BriefingMode: BriefingFile,
	},
}

// KnownAgents returns the sorted list of built-in agent type names.
// Used by config.validate's error messages and `canopy init --with-scripts
// --agent <foo>` to list valid choices.
func KnownAgents() []string {
	out := make([]string, 0, len(defaults))
	for k := range defaults {
		out = append(out, k)
	}
	// Sort manually instead of importing sort: tiny slice, deterministic
	// iteration order matters more than asymptotic perf.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Resolve returns the Launcher for the given agent type. Returns
// ErrUnknownAgent (wrapped with the known-types list for the user's
// benefit) when the type isn't in the registry.
//
// Empty agentType is treated as "claude" for backwards compat — every
// canopy.json that predates v0.6 has no agent block, and they all
// implicitly target claude. config.validate also defaults the field, so
// callers normally don't hit the empty case here, but this function is
// the canonical fallback in case some path skipped validate.
func Resolve(agentType string) (Launcher, error) {
	if agentType == "" {
		agentType = "claude"
	}
	l, ok := defaults[agentType]
	if !ok {
		return Launcher{}, fmt.Errorf("%w: %q (known: %s)",
			ErrUnknownAgent, agentType, strings.Join(KnownAgents(), ", "))
	}
	return l, nil
}

// VerifyInstalled checks that the launcher's command is available on
// PATH. Called at spawn time so users get a clean error ("claude not
// found on PATH; install: <link>") instead of a cryptic exec failure
// from inside the tmux pane.
//
// Returns nil on success; an error including the binary name and a
// suggested install command on failure. The install hints are best-effort
// — we don't try to detect the user's package manager or platform; the
// hint is one canonical install link per agent.
func (l Launcher) VerifyInstalled() error {
	if _, err := exec.LookPath(l.Cmd); err != nil {
		return fmt.Errorf("agent: %q not found on PATH (%w). %s",
			l.Cmd, err, installHint(l.Cmd))
	}
	return nil
}

// BriefingPlan is what canopy.workspace.lifecycle needs to spawn the
// agent in a tmux pane. It tells the caller two things:
//
//  1. ShellCommand: the literal command string to pass to tmux pane spawn
//     (via tmux send-keys / new-window / split-window). Already shell-
//     safe — uses $(cat <path>) substitution to inject the briefing
//     content rather than trying to shell-escape arbitrary markdown.
//
//  2. PreRun: a shell command that must run BEFORE ShellCommand starts.
//     Used by BriefingAgentsMd mode (opencode) to copy the briefing
//     into <worktree>/AGENTS.md. Empty string for other modes.
//
// The briefing temp file is the caller's responsibility — workspace.lifecycle
// writes it to ~/.canopy/tmp/agent-briefing-<random>.md before calling
// PlanLaunch, and persists for the agent's lifetime (tiny files, system
// tmp cleanup handles eviction).
type BriefingPlan struct {
	// ShellCommand is the command tmux pane spawn invokes. It includes
	// any --append-system-prompt / --message-file / etc flags assembled
	// per the launcher's BriefingMode. Empty briefingPath produces a
	// command without the briefing flag (the "resume + no hints" case
	// from the hybrid strategy).
	ShellCommand string

	// PreRun runs once before ShellCommand. Empty string for all modes
	// except BriefingAgentsMd. When set, the caller is expected to
	// chain: `<PreRun> && <ShellCommand>` in the tmux pane invocation.
	PreRun string
}

// PlanLaunch builds a BriefingPlan for spawning this launcher's agent
// in a tmux pane. briefingPath is the absolute path to a temp file
// containing the briefing (or "" to skip the briefing entirely — used
// for "resume + no hints" launches).
//
// resume picks the Resume vs Fresh argv tails. When resume is true AND
// briefingPath is empty, the briefing flag (and its preceding flag-name)
// is dropped — for claude this means `claude --continue`, no
// --append-system-prompt at all.
//
// worktreePath is required for BriefingAgentsMd (opencode writes
// AGENTS.md to the worktree's cwd). Other modes ignore it.
//
// All command strings are shell-safe: paths are not embedded raw, and
// the briefing content is read via $(cat ...) substitution rather than
// inlined as a shell-escaped string.
func (l Launcher) PlanLaunch(briefingPath string, resume bool, worktreePath string) BriefingPlan {
	tail := l.Fresh
	if resume {
		tail = l.Resume
	}

	// Walk the argv tail. For each "{{briefing}}" token, replace it
	// according to BriefingMode. When briefingPath is empty AND the
	// token would have been a flag value, drop both the token and the
	// preceding flag name.
	parts := []string{shellQuote(l.Cmd)}
	for i := 0; i < len(tail); i++ {
		arg := tail[i]
		if arg != "{{briefing}}" {
			parts = append(parts, shellQuote(arg))
			continue
		}
		// Token at position i.
		if briefingPath == "" {
			// Drop this token AND the preceding flag, if any.
			if len(parts) > 1 {
				parts = parts[:len(parts)-1]
			}
			continue
		}
		// Replace per mode.
		switch l.BriefingMode {
		case BriefingInline:
			// Shell substitution: $(cat <path>). The path itself is
			// shell-quoted so spaces / special chars in path are safe.
			// The cat'd content goes verbatim into the arg — no further
			// escaping needed even for markdown with backticks etc.
			parts = append(parts, fmt.Sprintf(`"$(cat %s)"`, shellQuote(briefingPath)))
		case BriefingFile:
			// Pass the path as the flag value directly. aider reads it.
			parts = append(parts, shellQuote(briefingPath))
		case BriefingAgentsMd:
			// No replacement — the briefing was already cp'd to
			// AGENTS.md by PreRun. This branch shouldn't normally fire
			// because BriefingAgentsMd launchers don't use the
			// {{briefing}} token in their argv. Defensive no-op.
		}
	}

	plan := BriefingPlan{
		ShellCommand: strings.Join(parts, " "),
	}

	// BriefingAgentsMd mode: pre-copy the briefing into the worktree's
	// AGENTS.md. opencode reads AGENTS.md from cwd at startup; we need
	// it in place before opencode runs.
	//
	// When briefingPath is empty (resume + no hints), we skip the cp —
	// the agent uses whatever AGENTS.md already exists in the worktree
	// from a prior launch (or none, if this is the first time).
	if l.BriefingMode == BriefingAgentsMd && briefingPath != "" && worktreePath != "" {
		plan.PreRun = fmt.Sprintf("cp %s %s",
			shellQuote(briefingPath),
			shellQuote(worktreePath+"/AGENTS.md"))
	}

	return plan
}

// shellQuote wraps a string in single quotes and escapes any embedded
// single quotes via the standard '\” trick. Safe for any string —
// single-quoted POSIX shell strings have NO escape sequences except
// for the close-quote-escape-quote-reopen pattern.
//
// Empty string becomes ”. Used when assembling shell commands from
// trusted-but-arbitrary inputs (file paths, briefing paths).
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// If the string is alphanum + safe punctuation only, no quoting
	// needed. Cuts noise in the resulting shell command for the common
	// case of agent binary names like "claude".
	safe := true
	for _, r := range s {
		if !isShellSafe(r) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isShellSafe reports whether r is a character that needs no shell
// quoting in a POSIX shell command. Conservative — only allows
// alphanumerics, underscore, hyphen, dot, slash, comma, equals.
// Anything else triggers single-quoting.
func isShellSafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z',
		r >= 'A' && r <= 'Z',
		r >= '0' && r <= '9':
		return true
	case r == '_', r == '-', r == '.', r == '/', r == ',', r == '=', r == '+', r == ':':
		return true
	}
	return false
}

// installHint returns a one-line install pointer for known agents.
// Empty string for unknown commands (we just say "install it" generically).
func installHint(cmd string) string {
	switch cmd {
	case "claude":
		return "Install: https://docs.anthropic.com/en/docs/claude-code/quickstart"
	case "codex":
		return "Install: npm install -g @openai/codex"
	case "opencode":
		return "Install: https://opencode.ai/docs/install"
	case "aider":
		return "Install: pip install aider-chat"
	}
	return "Install the agent CLI before launching this workspace."
}

// BuildArgv assembles the argv slice for the given launcher invocation.
// resume picks Resume vs Fresh argv tails. briefing is the canopy-assembled
// briefing string (for BriefingInline) or the temp-file path (for
// BriefingFile/BriefingAgentsMd). When briefing is empty, the "{{briefing}}"
// token is dropped along with its preceding flag arg — useful for the
// "resume + no hints" branch where we don't pass --append-system-prompt
// at all.
//
// Returns the full argv: [cmd, ...args]. Caller passes this to
// exec.Cmd directly.
func (l Launcher) BuildArgv(resume bool, briefing string) []string {
	tail := l.Fresh
	if resume {
		tail = l.Resume
	}

	// Walk the tail. For each "{{briefing}}" token: if briefing is
	// non-empty, replace inline and keep the preceding flag. If empty,
	// drop both the flag and the token (the prior arg is assumed to be
	// the flag this token is the value for).
	out := make([]string, 0, len(tail)+1)
	out = append(out, l.Cmd)
	for i := 0; i < len(tail); i++ {
		arg := tail[i]
		if arg != "{{briefing}}" {
			out = append(out, arg)
			continue
		}
		// Token at position i. If briefing is empty, drop it AND the
		// preceding flag (out's last element).
		if briefing == "" {
			if len(out) > 1 {
				out = out[:len(out)-1] // pop the preceding flag
			}
			continue
		}
		out = append(out, briefing)
	}
	log.Debug("agent.argv", "cmd", l.Cmd, "resume", resume, "argc", len(out))
	return out
}
