// update_addproject.go — Bubbletea state machine for the v0.20
// Add Project form. Mirrors the addHostFormMode pattern in
// update_host.go: single mode, single textinput, Enter dispatches.
//
// The form is reachable from two surfaces:
//
//   - Global tab: `a` keybind on listMode opens the form (decision #11
//     in v0.20-add-project.md).
//   - Splash screen: openAddProjectForm fires on startup when no
//     projects are registered (the user's first canopy run; the splash
//     model re-uses the same Bubbletea Update + View flow).
//
// On Enter, the input is classified:
//
//   - Empty + Global tab → inline error "✗ Type a path or URL." (decision #11
//     forbids "empty = init cwd" on Global; that semantics only makes
//     sense for the splash, where the cwd is meaningful).
//   - Local path → m.RunInitFunc(path) synchronously, then close form
//     with success toast.
//   - Git URL → resolve dest, pre-clone safety checks, then
//     tea.ExecProcess(git clone) so the user can answer SSH passphrase
//     / HTTPS credential prompts on the real terminal. After the exec
//     returns, the callback emits addProjectCloneDoneMsg; Update then
//     invokes RunInitFunc on the cloned dir and shows the success toast.
//
// Errors land in m.addProjectError and render below the input in red.
// The error clears on the next keystroke.

package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/canopyinit"
	"github.com/avinashjoshi/canopy/internal/config"
)

// actionAddProject is the binding-table entry for the `a` keybind on
// Local/Global tabs. Delegates to openAddProjectForm.
func actionAddProject(m *Model, _ tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m, m.openAddProjectForm()
}

// openAddProjectForm transitions to addProjectFormMode. Resets the
// textinput so a previous attempt's value doesn't haunt the new one.
// Returns the blink Cmd so the caret animates from the moment the
// form appears (consistent with addHostFormMode).
//
// Disabled cleanly when m.RunInitFunc is nil — the caller (`a` keybind
// handler) should check before invoking. Belt-and-suspenders: this
// function also no-ops if RunInitFunc is missing, so a stray call
// can't put the TUI into an unrecoverable form state.
func (m *Model) openAddProjectForm() tea.Cmd {
	if m.RunInitFunc == nil {
		m.err = fmt.Errorf("Add Project unavailable in this build")
		return nil
	}
	m.mode = addProjectFormMode
	m.addProjectInput.Reset()
	m.addProjectInput.Focus()
	m.addProjectError = ""
	m.addProjectToast = ""
	m.addProjectToastFor = time.Time{}
	m.addProjectEditingSourceRoot = false
	m.addProjectSavedInput = ""
	return textinputBlink()
}

// closeAddProjectForm returns to listMode, blurring the input and
// clearing form-only state. Called on Esc and after a successful add.
func (m *Model) closeAddProjectForm() {
	m.mode = listMode
	m.addProjectInput.Blur()
	m.addProjectInput.Reset()
	m.addProjectError = ""
	m.addProjectToast = ""
	m.addProjectToastFor = time.Time{}
	m.addProjectEditingSourceRoot = false
	m.addProjectSavedInput = ""
}

// handleAddProjectFormKey is the per-mode key router for the Add
// Project form. Esc cancels; Enter submits; ctrl+s enters inline
// source-root edit mode; anything else forwards to the textinput.
//
// Error-clear-on-keystroke: any non-Enter, non-ctrl+s key clears
// addProjectError so the user sees feedback only while their input
// is still the problem.
func (m *Model) handleAddProjectFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.addProjectEditingSourceRoot {
			// Editing source-root: cancel back to the URL/path field.
			m.restorePrimaryInput()
			return m, textinputBlink()
		}
		m.closeAddProjectForm()
		return m, nil
	case "ctrl+s":
		return m.handleAddProjectSettingsKey()
	case "enter":
		if m.addProjectEditingSourceRoot {
			return m.submitSourceRootEdit()
		}
		return m.submitAddProject()
	}
	// Any other key clears a stale error and forwards to the input.
	if m.addProjectError != "" {
		m.addProjectError = ""
	}
	var cmd tea.Cmd
	m.addProjectInput, cmd = m.addProjectInput.Update(msg)
	return m, cmd
}

// handleAddProjectSettingsKey toggles into the inline source-root
// editor (decision #18). Saves the current input, swaps in the
// resolved source-root value, marks the editor active. Pressing
// ctrl+s again is a no-op while already editing.
func (m *Model) handleAddProjectSettingsKey() (tea.Model, tea.Cmd) {
	if m.addProjectEditingSourceRoot {
		return m, nil // already editing; no-op
	}
	root, _, err := resolveCurrentSourceRoot()
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	m.addProjectSavedInput = m.addProjectInput.Value()
	m.addProjectInput.SetValue(root)
	m.addProjectInput.CursorEnd()
	m.addProjectEditingSourceRoot = true
	m.addProjectError = ""
	return m, textinputBlink()
}

// submitSourceRootEdit writes the new source-root to
// ~/.canopy/config.json and returns to the primary input. Empty value
// is treated as "unset" — falls back to env / default per the config
// precedence rules.
func (m *Model) submitSourceRootEdit() (tea.Model, tea.Cmd) {
	newRoot := strings.TrimSpace(m.addProjectInput.Value())
	canopyHome, err := canopyHomeDir()
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	store, err := config.NewUserStore(canopyHome)
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	err = store.WithLock(func(c *config.UserConfig) error {
		c.SourceRoot = newRoot
		return nil
	})
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	m.restorePrimaryInput()
	return m, textinputBlink()
}

// restorePrimaryInput swaps the URL/path input back in after a
// source-root edit (success or cancel).
func (m *Model) restorePrimaryInput() {
	m.addProjectInput.SetValue(m.addProjectSavedInput)
	m.addProjectInput.CursorEnd()
	m.addProjectSavedInput = ""
	m.addProjectEditingSourceRoot = false
}

// submitAddProject is the Enter handler for the primary URL/path
// input. Classifies the value and dispatches to either the
// path-init or URL-clone path.
func (m *Model) submitAddProject() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(m.addProjectInput.Value())

	// Empty input on Global tab → error. Splash uses a different
	// model (see model_splash.go) which treats empty as "init cwd";
	// inside the main TUI's addProjectFormMode, there is no
	// meaningful "cwd" — the TUI is cross-project.
	if value == "" {
		m.addProjectError = "✗ Type a path or URL."
		return m, nil
	}

	// Local path → sync runInit. Fast (<100ms), no network.
	if !canopyinit.LooksLikeGitURL(value) {
		return m.submitAddProjectPath(value)
	}

	// Git URL → resolve dest, run safety checks, then tea.ExecProcess.
	return m.submitAddProjectURL(value)
}

// submitAddProjectPath handles the local-path branch. Validates the
// path exists and is a directory, calls RunInitFunc, shows toast.
func (m *Model) submitAddProjectPath(path string) (tea.Model, tea.Cmd) {
	abs, err := filepath.Abs(path)
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	if !info.IsDir() {
		m.addProjectError = fmt.Sprintf("✗ %s is not a directory.", abs)
		return m, nil
	}
	if err := m.RunInitFunc(abs, false, false); err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	return m, m.showAddProjectToast(filepath.Base(abs), abs)
}

// submitAddProjectURL runs all pre-clone checks (basename collision,
// dest path safety, resolution) then drops out of altscreen via
// tea.ExecProcess to run git clone with full tty access. On exec
// completion, the callback emits addProjectCloneDoneMsg.
func (m *Model) submitAddProjectURL(rawURL string) (tea.Model, tea.Cmd) {
	canopyHome, err := canopyHomeDir()
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	store, err := config.NewUserStore(canopyHome)
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	userCfg, err := store.Load()
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	dest, _, err := canopyinit.ResolveCloneDest(rawURL, "", userCfg, canopyHome)
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}

	// Pre-clone basename collision via state.json.
	st, err := m.store.Load()
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	if other := st.FindBasenameCollision(dest); other != "" {
		m.addProjectError = fmt.Sprintf(
			"✗ Project %q is already registered at %s. Edit ~/.canopy/state.json or pick a different URL.",
			filepath.Base(dest), other)
		return m, nil
	}
	if err := canopyinit.ValidateDestNotInsideWorkspace(dest, st); err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}

	// Three sub-cases mirror cmd/canopy/addproject.go: dest exists
	// with .git (skip clone), dest exists without .git (error), dest
	// missing (clone).
	skipClone := false
	if info, err := os.Stat(dest); err == nil {
		if !info.IsDir() {
			m.addProjectError = fmt.Sprintf("✗ %s exists and isn't a directory.", dest)
			return m, nil
		}
		if _, gerr := os.Stat(filepath.Join(dest, ".git")); gerr == nil {
			skipClone = true
		} else {
			m.addProjectError = fmt.Sprintf("✗ %s exists and isn't a git repo.", dest)
			return m, nil
		}
	} else if err := canopyinit.EnsureSourceRoot(dest); err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}

	if skipClone {
		// Idempotent rerun: skip exec, go straight to init.
		if err := m.RunInitFunc(dest, false, false); err != nil {
			m.addProjectError = "✗ " + err.Error()
			return m, nil
		}
		return m, m.showAddProjectToast(filepath.Base(dest), dest)
	}

	// Build the git clone command. inherit env so SSH agent / git
	// credential helpers find their config. Stdout/stderr/stdin are
	// inherited from the real tty by tea.ExecProcess automatically —
	// that's the whole point of using it instead of a captured Cmd.
	cmd := exec.Command("git", "clone", rawURL, dest)
	cmd.Env = os.Environ()
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return addProjectCloneDoneMsg{dest: dest, rawURL: rawURL, err: err}
	})
}

// addProjectCloneDoneMsg is the message Update receives after
// tea.ExecProcess returns from the git-clone subprocess. err is the
// clone outcome; on success, the dest dir contains a fresh repo and
// we run RunInitFunc to register it.
type addProjectCloneDoneMsg struct {
	dest   string
	rawURL string
	err    error
}

// handleAddProjectCloneDone runs RunInitFunc on the cloned dir and
// surfaces success / failure into the form. Wired from Update's
// outermost type switch (added in the keymap-and-update wiring step).
func (m *Model) handleAddProjectCloneDone(msg addProjectCloneDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.mode = addProjectFormMode
		m.addProjectError = fmt.Sprintf("✗ git clone failed: %v", msg.err)
		m.addProjectInput.Focus()
		return m, textinputBlink()
	}
	if err := m.RunInitFunc(msg.dest, false, false); err != nil {
		m.mode = addProjectFormMode
		m.addProjectError = "✗ " + err.Error()
		m.addProjectInput.Focus()
		return m, textinputBlink()
	}
	return m, m.showAddProjectToast(filepath.Base(msg.dest), msg.dest)
}

// addProjectToastExpireMsg fires when a success toast's display window
// elapses. Update closes the form on receipt.
type addProjectToastExpireMsg struct{}

// showAddProjectToast sets the success line and schedules an auto-close
// after 3 seconds (decision #14). Returns a Cmd batch: a refresh so
// the new project appears in the list, and a tick that emits
// addProjectToastExpireMsg.
func (m *Model) showAddProjectToast(name, path string) tea.Cmd {
	m.addProjectToast = fmt.Sprintf("✓ Added %s at %s", name, path)
	m.addProjectError = ""
	m.addProjectToastFor = time.Now().Add(3 * time.Second)
	return tea.Batch(
		func() tea.Msg { return refreshAllMsg{} },
		tea.Tick(3*time.Second, func(time.Time) tea.Msg { return addProjectToastExpireMsg{} }),
	)
}

// handleAddProjectToastExpire closes the form after the success toast
// has been visible long enough for the user to read it.
func (m *Model) handleAddProjectToastExpire() (tea.Model, tea.Cmd) {
	if m.mode != addProjectFormMode {
		return m, nil // user pressed Esc before the tick fired
	}
	m.closeAddProjectForm()
	return m, nil
}

// canopyHomeDir returns the path to ~/.canopy. Local helper duplicated
// from cmd/canopy/config.go because internal/ui can't import cmd/canopy
// (leaf-up dep rule). Same logic — the only "right" home is the user's.
func canopyHomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("canopy home: %w", err)
	}
	return filepath.Join(home, ".canopy"), nil
}

// resolveCurrentSourceRoot returns the current effective source-root
// + its source label. Used to seed the inline source-root editor and
// to display the status line in the form. Reads outside the lock — a
// snapshot is fine for display purposes.
func resolveCurrentSourceRoot() (path, source string, err error) {
	canopyHome, err := canopyHomeDir()
	if err != nil {
		return "", "", err
	}
	store, err := config.NewUserStore(canopyHome)
	if err != nil {
		return "", "", err
	}
	c, err := store.Load()
	if err != nil {
		return "", "", err
	}
	p, s := config.ResolveSourceRoot(c, canopyHome)
	return p, string(s), nil
}
