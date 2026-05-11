package workspace

import "testing"

// Pure unit tests for the backfill command-sniffing helpers. No tmux
// needed — these are package-level helpers in backfill.go.

func TestClassifyCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"", ""},
		{"nvim", "ide"},
		{"vim", "ide"},
		{"hx", "ide"},
		{"emacs", "ide"},
		{"bash", "shell"},
		{"zsh", "shell"},
		{"fish", "shell"},
		{"claude", "agent"},
		{"codex", "agent"},
		{"aider", "agent"},
		{"opencode", "agent"},
		// unknown — neither editor nor shell nor known agent
		{"htop", ""},
		{"python", ""},
		{"go", ""},
	}
	for _, tc := range cases {
		if got := classifyCommand(tc.cmd); got != tc.want {
			t.Errorf("classifyCommand(%q) = %q; want %q", tc.cmd, got, tc.want)
		}
	}
}

func TestRoleClassOf(t *testing.T) {
	cases := []struct {
		role string
		want string
	}{
		{"ide", "ide"},
		{"terminal:shell", "shell"},
		{"agent:claude", "agent"},
		{"agent:codex", "agent"},
		{"agent:something-future", "agent"},
		// unrecognized
		{"", ""},
		{"agent:", ""},
		{"terminal", ""},
		{"random", ""},
	}
	for _, tc := range cases {
		if got := roleClassOf(tc.role); got != tc.want {
			t.Errorf("roleClassOf(%q) = %q; want %q", tc.role, got, tc.want)
		}
	}
}

// TestCommandConflictsWithRole covers the four branches of the
// safeguard:
//   - clear conflict (nvim in shell slot) → true
//   - matching class (any agent in agent slot) → false
//   - empty / unknown command → false (don't refuse on ambiguity)
//   - unrecognized role → false (don't second-guess)
func TestCommandConflictsWithRole(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		role string
		want bool
	}{
		// real conflicts — ide or agent landing in the wrong slot
		{"nvim in shell slot", "nvim", "terminal:shell", true},
		{"nvim in agent slot", "nvim", "agent:claude", true},
		{"claude in ide slot", "claude", "ide", true},
		{"claude in shell slot", "claude", "terminal:shell", true},
		{"codex in ide slot", "codex", "ide", true},

		// no conflict — same class
		{"nvim in ide slot", "nvim", "ide", false},
		{"vim in ide slot", "vim", "ide", false},
		{"zsh in shell slot", "zsh", "terminal:shell", false},
		{"claude in agent:claude slot", "claude", "agent:claude", false},
		// user switched agent: codex now in slot canonically assigned to claude.
		// Class matches; no conflict (don't refuse just because launcher diverged).
		{"codex in agent:claude slot", "codex", "agent:claude", false},

		// shell-class commands are PERMISSIVE in any slot — the agent
		// or editor may simply not be running yet. Refusing here would
		// break every "I quit claude to poke around the shell" case.
		{"bash in agent slot", "bash", "agent:claude", false},
		{"bash in ide slot", "bash", "ide", false},
		{"zsh in agent slot", "zsh", "agent:claude", false},

		// ambiguous — don't refuse
		{"empty command, any slot", "", "agent:claude", false},
		{"unknown command in shell slot", "htop", "terminal:shell", false},
		{"unknown command in agent slot", "python", "agent:claude", false},

		// unrecognized role — don't second-guess
		{"known cmd, unknown role", "nvim", "custom-role", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandConflictsWithRole(tc.cmd, tc.role); got != tc.want {
				t.Errorf("commandConflictsWithRole(%q, %q) = %v; want %v",
					tc.cmd, tc.role, got, tc.want)
			}
		})
	}
}
