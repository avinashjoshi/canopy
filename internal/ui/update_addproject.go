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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/canopyinit"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/host"
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
	m.addProjectTargets = buildAddProjectTargets(m.hostList)
	m.addProjectTargetIdx = 0 // default to local
	return textinputBlink()
}

// buildAddProjectTargets returns the dispatch-target list the Tab key
// cycles through: empty string (local) first, then registered host
// names in sorted order. Sorted so cycling order is deterministic
// across sessions (no host-order surprises after a re-add).
func buildAddProjectTargets(hosts []host.Host) []string {
	out := []string{""} // "" = local
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	sort.Strings(names)
	return append(out, names...)
}

// currentAddProjectTarget returns the target the user has cycled to.
// Empty string means local canopy. Safe to call even when targets
// haven't been initialized — falls back to local.
func (m *Model) currentAddProjectTarget() string {
	if len(m.addProjectTargets) == 0 {
		return ""
	}
	idx := m.addProjectTargetIdx
	if idx < 0 || idx >= len(m.addProjectTargets) {
		idx = 0
	}
	return m.addProjectTargets[idx]
}

// cycleAddProjectTarget advances the target cursor by delta (+1 for
// Tab, -1 for Shift+Tab), wrapping at both ends so the user can
// cycle in either direction without thinking about bounds.
func (m *Model) cycleAddProjectTarget(delta int) {
	if len(m.addProjectTargets) <= 1 {
		return // only local; nothing to cycle
	}
	n := len(m.addProjectTargets)
	m.addProjectTargetIdx = ((m.addProjectTargetIdx + delta) % n + n) % n
	// Clear error so any "remote requires URL" message from a previous
	// submit doesn't linger across target changes.
	m.addProjectError = ""
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
	case "tab":
		if m.addProjectEditingSourceRoot {
			return m, nil // tab is a no-op inside the source-root editor
		}
		m.cycleAddProjectTarget(+1)
		return m, nil
	case "shift+tab":
		if m.addProjectEditingSourceRoot {
			return m, nil
		}
		m.cycleAddProjectTarget(-1)
		return m, nil
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
// input. Classifies the value AND consults the current dispatch
// target (local vs remote host) to pick the right flow.
//
// Target matrix:
//
//	            local                  remote host
//	  ─────────┼──────────────────────┼─────────────────────────────
//	  empty    │ inline error          │ inline error
//	  path     │ sync runInit          │ refuse (paths are local-only)
//	  URL      │ tea.ExecProcess git   │ tea.ExecProcess ssh dispatch
//	  ─────────┴──────────────────────┴─────────────────────────────
func (m *Model) submitAddProject() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(m.addProjectInput.Value())
	target := m.currentAddProjectTarget()

	// Empty input on Global tab → error. Splash uses a different
	// model (see model_splash.go) which treats empty as "init cwd";
	// inside the main TUI's addProjectFormMode, there is no
	// meaningful "cwd" — the TUI is cross-project.
	if value == "" {
		m.addProjectError = "✗ Type a path or URL."
		return m, nil
	}

	isURL := canopyinit.LooksLikeGitURL(value)

	// Remote target: only URLs make sense. A path is a string on the
	// local filesystem; we can't validate it against a different
	// machine's disk. Refuse with a clear error rather than dispatching
	// something that'll fail confusingly on the remote.
	if target != "" {
		if !isURL {
			m.addProjectError = fmt.Sprintf("✗ Target %s is remote — only git URLs are allowed (paths can't be resolved on another machine).", target)
			return m, nil
		}
		return m.submitAddProjectRemote(value, target)
	}

	// Local target: split path vs URL.
	if !isURL {
		return m.submitAddProjectPath(value)
	}
	return m.submitAddProjectURL(value)
}

// submitAddProjectRemote dispatches the URL clone+init to a registered
// remote canopy via SSH. Identical mechanism to the CLI's
// `canopy init <url> --on <host>` — re-uses host.SSHCmd's
// ControlMaster plumbing and inherits the user's tty via
// tea.ExecProcess so git auth prompts on the remote come back to the
// local terminal.
//
// We don't follow up with RunInitFunc — the remote canopy did its
// own init. We DO emit refreshAllMsg so the Global tab pulls the new
// project into the remote-rows snapshot.
func (m *Model) submitAddProjectRemote(rawURL, hostName string) (tea.Model, tea.Cmd) {
	canopyHome, err := canopyHomeDir()
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	reg, err := host.NewRegistry(canopyHome)
	if err != nil {
		m.addProjectError = "✗ " + err.Error()
		return m, nil
	}
	h, err := reg.Resolve(hostName)
	if err != nil {
		m.addProjectError = fmt.Sprintf("✗ %v. Register the host: canopy host add %s <ssh-target>", err, hostName)
		return m, nil
	}

	// Shell-quote the URL so metacharacters can't be reinterpreted on
	// the remote shell. SSHRunUser wraps in `bash -lc` so the user's
	// PATH (incl. ~/.local/bin where canopy typically lives) is set,
	// and allocates a pty so git auth prompts can read /dev/tty.
	//
	// Prepend CANOPY_INIT_RESULT_FILE=<remote-temp> so the remote
	// canopy writes its canonical project root to a known path. After
	// the SSH dispatch returns, we fetch + parse + auto-register the
	// project in the laptop's hosts.json. Without this step the next
	// `canopy new` on the just-added project errors with "host has
	// no projects registered" (resolveOnForNew can't find the path).
	resultFile, err := unpredictableRemoteResultPath()
	if err != nil {
		m.addProjectError = "✗ generate result path: " + err.Error()
		return m, nil
	}
	remote := fmt.Sprintf("CANOPY_INIT_RESULT_FILE=%s canopy init %s",
		shellQuoteUI(resultFile), shellQuoteUI(rawURL))
	ctx := context.Background()
	sshCmd := host.SSHRunUser(ctx, h.SSHTarget, remote)

	// Wrap ssh in a tiny shell preamble that prints a "connecting"
	// line BEFORE ssh starts its silent handshake. Without this the
	// user sees several seconds of dead terminal between the form
	// submit and the first remote output — easy to mistake for a hang.
	//
	// Implementation: `sh -c 'echo $1; shift; exec "$@"' -- <preamble> <ssh argv>`
	// passes the ssh argv as positional args so its exact shape is
	// preserved (no fragile re-quoting of paths-with-% characters
	// like ssh's ControlPath). On subsequent dispatches the
	// ControlMaster socket reuses the connection — handshake is
	// instant — so the user mostly sees the line and immediately
	// sees ssh's output. On first dispatch they see the line and
	// then a few seconds of wait, which is honest signal.
	preamble := fmt.Sprintf("Connecting to %s (%s)... (first dispatch may take a few seconds)", h.Name, h.SSHTarget)
	wrapArgs := append([]string{"-c", `echo "$1"; shift; exec "$@"`, "--", preamble}, sshCmd.Args...)
	cmd := exec.CommandContext(ctx, "sh", wrapArgs...)
	cmd.Env = os.Environ()
	// Capture sshTarget + resultFile for the post-exec callback so
	// the laptop can SSH-fetch the path the remote canopy wrote.
	sshTarget := h.SSHTarget
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return addProjectRemoteDoneMsg{
			hostName:   hostName,
			sshTarget:  sshTarget,
			rawURL:     rawURL,
			resultFile: resultFile,
			err:        err,
		}
	})
}

// addProjectRemoteDoneMsg is the result of an SSH dispatch via
// tea.ExecProcess. Distinct from the local addProjectCloneDoneMsg
// because the post-success action is different: no RunInitFunc to
// run (remote canopy already did it), but we DO want to (a) fetch
// the canonical project root from the remote-side result file so we
// can auto-register the project in the laptop's hosts.json, and (b)
// refresh the remote-rows cache so the new project appears in the
// Global tab.
type addProjectRemoteDoneMsg struct {
	hostName   string
	sshTarget  string
	rawURL     string
	resultFile string // remote-side path written by `canopy init`
	err        error
}

// handleAddProjectRemoteDone surfaces success/failure of an SSH
// dispatch into the form. Mirrors handleAddProjectCloneDone but
// triggers a remote refresh instead of a local runInit. Also
// auto-registers the new project in the laptop's hosts.json so
// follow-up `canopy new` against this row can resolve --remote-cwd.
func (m *Model) handleAddProjectRemoteDone(msg addProjectRemoteDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.mode = addProjectFormMode
		m.addProjectError = fmt.Sprintf("✗ remote init failed on %s: %v", msg.hostName, msg.err)
		m.addProjectInput.Focus()
		return m, textinputBlink()
	}
	// Fetch the remote-side result file and AddProject locally. If
	// this step fails we still toast success — the project IS
	// initialized on the remote — but we warn the user via a logged
	// message so they know to register manually before `canopy new`.
	registerRemoteAddProject(msg.hostName, msg.sshTarget, msg.resultFile)

	// Derive a friendly name for the toast. Best-effort: the URL
	// basename is what the remote canopy clones into.
	name := msg.hostName
	if base, derr := canopyinit.DeriveBasename(msg.rawURL); derr == nil {
		name = base + " on " + msg.hostName
	}
	return m, m.showAddProjectToast(name, msg.hostName)
}

// registerRemoteAddProject fetches the canonical project root from
// the remote-side result file (written by canopy init when
// CANOPY_INIT_RESULT_FILE was set) and registers it in the laptop's
// hosts.json. Best-effort — failures are logged but don't bubble up.
// The project is still successfully initialized on the remote;
// missing auto-registration just means the user has to run
// `canopy project add <name> <path> --on <host>` manually before
// `canopy new --on <host>` works.
func registerRemoteAddProject(hostName, sshTarget, resultFile string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use the batch (non-interactive) variant — no pty needed, we
	// just want cat output. ControlMaster socket from the previous
	// dispatch is reused → handshake is instant.
	fetch := host.SSHCmdBatch(ctx, sshTarget, "bash", "-c",
		fmt.Sprintf("cat %s 2>/dev/null && rm -f %s", shellQuoteUI(resultFile), shellQuoteUI(resultFile)))
	out, err := fetch.Output()
	if err != nil {
		log.Warn("ui.addproject.fetch-result-failed", "host", hostName, "err", err)
		return
	}
	canonicalRoot := strings.TrimSpace(string(out))
	if canonicalRoot == "" {
		log.Warn("ui.addproject.remote-result-empty",
			"host", hostName,
			"hint", "remote canopy may be pre-v0.20 — upgrade with `canopy host upgrade`")
		return
	}
	// The remote could be compromised, on an older canopy, or the
	// temp file could have been raced. Validate before writing into
	// the laptop's hosts.json.
	if err := validateRemoteResultPath(canonicalRoot); err != nil {
		log.Warn("ui.addproject.remote-result-invalid",
			"host", hostName,
			"err", err,
			"hint", "remote returned path that failed safety checks; not auto-registering")
		return
	}

	projectName := filepath.Base(canonicalRoot)
	home, err := os.UserHomeDir()
	if err != nil {
		log.Warn("ui.addproject.home-failed", "err", err)
		return
	}
	reg, err := host.NewRegistry(filepath.Join(home, ".canopy"))
	if err != nil {
		log.Warn("ui.addproject.registry-failed", "err", err)
		return
	}
	if err := reg.AddProject(hostName, projectName, canonicalRoot); err != nil {
		// ErrProjectExists is fine — idempotent re-run.
		if !errors.Is(err, host.ErrProjectExists) {
			log.Warn("ui.addproject.local-register-failed",
				"host", hostName,
				"project", projectName,
				"path", canonicalRoot,
				"err", err)
		}
	}
}

// shellQuoteUI is the ui-package twin of cmd/canopy.shellQuote in
// install_tmux.go. Same logic; duplicated so internal/ui can avoid a
// cross-package import (cmd → ui, not ui → cmd, per CLAUDE.md).
//
// Wraps s in single quotes; escapes embedded quotes via the standard
// close-escape-reopen dance.
func shellQuoteUI(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// unpredictableRemoteResultPath returns a /tmp path with 128 bits of
// entropy in the suffix. Random naming stops an attacker on the
// remote host from pre-creating the file as a symlink before the
// remote canopy writes to it. Same mechanism as cmd/canopy's
// unpredictableResultPath — duplicated to keep ui leaf-up.
func unpredictableRemoteResultPath() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "/tmp/canopy-init-" + hex.EncodeToString(buf) + ".txt", nil
}

// validateRemoteResultPath enforces the same safety contract on the
// remote-supplied project root as cmd/canopy/init_remote.go does for
// the CLI flow. Duplicated for the leaf-up rule.
func validateRemoteResultPath(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	if len(p) > 1024 {
		return fmt.Errorf("path too long (%d > 1024)", len(p))
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("path is not absolute: %q", p)
	}
	if !utf8.ValidString(p) {
		return errors.New("path is not valid UTF-8")
	}
	for i, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path contains control character at byte %d", i)
		}
	}
	return nil
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
