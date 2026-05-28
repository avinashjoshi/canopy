// New-workspace flow (n keybind). Picker (Fresh / Prompt / PR / Issue
// / Branch) → sub-modal → busy view streaming Manager.Create output.
// Remote rows dispatch through `canopy new --on host` instead of the
// local Manager (Phase 1k). Carved out of update.go.

package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/ghx"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

// newPickerOptionCount is the number of options in the variant
// picker. Used to bound cursor nav. Update if newPickerOption is
// extended (in view.go).
const newPickerOptionCount = 5

// progressTickInterval controls how often the busy view refreshes
// during a long-running create. 150ms is invisible to the eye for
// streaming text and far below any practical script output rate;
// shorter intervals just burn redraw cycles for no gain.
const progressTickInterval = 150 * time.Millisecond

// openNewPicker resets state and opens the variant picker. Called
// from the listMode 'n' keypress and from sub-modal esc handlers
// (back-one-step). Idempotent; safe to call from any mode.
func (m *Model) openNewPicker() {
	m.mode = newPickerMode
	m.newPickerCursor = 0
	m.nameInput.Reset()
	m.nameInput.Blur()
}

// handleNewPickerKey is the keymap for the variant picker (step 1
// of the new-workspace flow). Single-letter shortcuts launch each
// sub-modal directly; arrow-then-enter is the keyboard-discoverable
// alternative for users who scan before they type.
//
// Esc returns to listMode (one step back). q is suppressed here so
// the user can't accidentally quit canopy from inside the picker;
// they have to esc back first. ctrl+c is the global escape hatch.
func (m *Model) handleNewPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = listMode
		m.clearNewTarget()
		return m, nil
	case "ctrl+c":
		return m, tea.Quit

	// Single-key shortcuts — launch the corresponding sub-modal.
	case "n", "f":
		// 'n' = "new (fresh)" — same letter as the keymap, no surprise.
		// 'f' is an alias if the user thinks "fresh".
		return m.openNewFresh(), textinputBlink()
	case "p":
		// PR/Issue/Branch variants work for both local and remote
		// targets — remote loaders SSH to the host's gh / git in the
		// project cwd, and submit handlers dispatch through
		// remoteCreateCmd so the remote canopy resolves the source.
		// v0.21 (parity with local).
		return m, m.openNewPR()
	case "i":
		return m, m.openNewIssue()
	case "b":
		return m, m.openNewBranch()
	case "t":
		// 't' for "task" — see newPickerOptions for the letter-choice
		// rationale (no good mnemonic for "prompt", and `p` is taken).
		return m.openNewPrompt()

	// Arrow nav for keyboard-discovery users. Cursor bounded by the
	// full picker option count for both local and remote targets
	// (PR/Issue/Branch are reachable for remote as of v0.21).
	case "up", "k":
		if m.newPickerCursor > 0 {
			m.newPickerCursor--
		}
		return m, nil
	case "down", "j":
		if m.newPickerCursor < newPickerOptionCount-1 {
			m.newPickerCursor++
		}
		return m, nil
	case "enter":
		// Same dispatch as the letter shortcuts, just keyed off cursor.
		// Indices follow newPickerOptions order: fresh, prompt, PR,
		// issue, branch.
		switch m.newPickerCursor {
		case 0:
			return m.openNewFresh(), textinputBlink()
		case 1:
			return m.openNewPrompt()
		case 2:
			return m, m.openNewPR()
		case 3:
			return m, m.openNewIssue()
		case 4:
			return m, m.openNewBranch()
		}
		return m, nil
	}
	return m, nil
}

// openNewFresh prepares the fresh-workspace sub-modal (step 2a).
// Reused by the picker's 'n'/'f'/enter-on-Fresh dispatch and any
// future direct-entry shortcut. Returns the model so the caller can
// chain the textinputBlink cmd.
func (m *Model) openNewFresh() *Model {
	m.mode = newFreshMode
	m.nameInput.Reset()
	m.nameInput.Focus()
	return m
}

// handleNewFreshKey is the keymap for the fresh-workspace name input
// (step 2a). Esc steps back to the picker. Enter submits with the
// typed name (or empty → namegen). Anything else falls through to
// the textinput.
func (m *Model) handleNewFreshKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil
	case "enter":
		name := m.nameInput.Value()
		spec := workspace.SourceSpec{} // fresh = zero spec
		m.busyOp = busyOpCreate
		m.busyTitle = newBusyTitle(name, spec)
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.mode = busyMode
		m.nameInput.Blur()
		if m.newTargetHost != "" {
			return m, remoteCreateCmd(m.canopyBinPath(), m.newTargetHost, m.newTargetRemoteCwd, name, spec, "")
		}
		return m, createCmd(m.newTargetMgr, name, spec)
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

// openNewPrompt prepares the "fresh + prompt" sub-modal (the 5th
// picker option). Mirrors openNewFresh but focuses the prompt
// textarea. Workspace name = namegen (no name input in this mode —
// the prompt is the user-facing primary; if they want an explicit
// name they can use the CLI's `canopy new --name foo --prompt ...`).
// Returns the cursor-blink cmd from textarea.Focus so the caret
// blinks immediately on open.
func (m *Model) openNewPrompt() (*Model, tea.Cmd) {
	m.mode = newPromptMode
	m.promptInput.Reset()
	blink := m.promptInput.Focus()
	return m, blink
}

// handleNewPromptKey is the keymap for the "fresh + prompt" sub-modal.
//
// Esc steps back to the picker. Ctrl+S submits when the prompt is
// non-empty (an empty Ctrl+S is a no-op — the placeholder already
// telegraphs the requirement, so an inline error would be noise).
// Enter inserts a newline because the prompt is a textarea, not a
// textinput — multi-line briefings are the whole point of the
// upgrade from single-line input.
//
// Anything else falls through to the textarea so the user can type
// normally.
func (m *Model) handleNewPromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil
	case "ctrl+s":
		prompt := strings.TrimSpace(m.promptInput.Value())
		if prompt == "" {
			return m, nil
		}
		// Stash the prompt on the model so createDoneMsg can pick it
		// up after Create succeeds. The actual send happens between
		// Create-success and attach.
		m.pendingPrompt = prompt
		spec := workspace.SourceSpec{} // fresh = zero spec
		m.busyOp = busyOpCreate
		m.busyTitle = "Creating workspace + prompting agent..."
		m.busyDone = false
		m.busyOutput = ""
		m.busyErr = nil
		m.mode = busyMode
		m.promptInput.Blur()
		if m.newTargetHost != "" {
			// Remote target: pass --prompt directly through canopy new
			// --on host (which already base64-encodes + tempfile-dances
			// per Phase 1f). The local pendingPrompt is consumed here,
			// not stashed for a post-attach send.
			m.pendingPrompt = ""
			return m, remoteCreateCmd(m.canopyBinPath(), m.newTargetHost, m.newTargetRemoteCwd, "", spec, prompt)
		}
		return m, createCmd(m.newTargetMgr, "", spec)
	}
	var cmd tea.Cmd
	m.promptInput, cmd = m.promptInput.Update(msg)
	return m, cmd
}

// openNewPR transitions to the PR picker sub-modal and kicks off the
// async loader. The loader returns prListLoadedMsg; until it arrives,
// the renderer shows a "Loading PRs..." state.
//
// For a remote target the loader SSHes `gh pr list` on the host inside
// the remote project cwd (v0.21 parity). For local it shells gh
// directly against newTargetRoot as before.
func (m *Model) openNewPR() tea.Cmd {
	m.mode = newPRMode
	m.listInput.Reset()
	m.listInput.Placeholder = "type a PR number, or arrow to a row below"
	m.listInput.Focus()
	m.listCursor = 0
	m.newLoading = true
	m.newLoadErr = nil
	m.newPRs = nil
	return tea.Batch(textinputBlink(), m.loadPRsForTarget())
}

// loadPRsForTarget picks the local or remote loader based on whether
// the current new-workspace target is remote. Returns a tea.Cmd that
// emits prListLoadedMsg either way so the receiving Update handler
// doesn't need to branch on host. Falls back to a synchronous
// errored loader when the SSH target can't be resolved (host vanished
// from registry mid-flow) — the picker surfaces the error inline.
func (m *Model) loadPRsForTarget() tea.Cmd {
	if m.newTargetHost == "" {
		return loadPRsCmd(m.newTargetRoot)
	}
	target, err := m.resolveHostForExec(m.newTargetHost)
	if err != nil {
		return func() tea.Msg {
			return prListLoadedMsg{err: err}
		}
	}
	return loadPRsRemoteCmd(target, m.newTargetRemoteCwd)
}

// prListLoadedMsg carries the result of an async ghx.ListPRs call.
// Update on receipt: clear loading, populate newPRs, surface any
// error inline.
type prListLoadedMsg struct {
	prs []ghx.PRSummary
	err error
}

// loadPRsCmd dispatches ghx.ListPRs in a goroutine. Limit 20 keeps
// the picker scannable; users with > 20 open PRs can still type the
// number directly.
func loadPRsCmd(projectRoot string) tea.Cmd {
	return func() tea.Msg {
		prs, err := ghx.ListPRs(context.Background(), projectRoot, 20)
		return prListLoadedMsg{prs: prs, err: err}
	}
}

// loadPRsRemoteCmd is the remote-host analog of loadPRsCmd. SSHes
// `gh pr list` on sshTarget inside remoteCwd. Same prListLoadedMsg
// payload so the picker render path is identical to the local case.
func loadPRsRemoteCmd(sshTarget, remoteCwd string) tea.Cmd {
	return func() tea.Msg {
		prs, err := ghx.RemoteListPRs(context.Background(), sshTarget, remoteCwd, 20)
		return prListLoadedMsg{prs: prs, err: err}
	}
}

// handleNewPRKey is the keymap for the PR picker sub-modal. Two
// dispatch shapes:
//
//   - User types a number: enter creates a workspace from PR #<num>
//     directly (works even when the list is empty / unloaded — covers
//     the "I know the number, just go" power-user path).
//   - User arrows into the loaded list: enter creates from the
//     selected PR (recognition path — see the PR title before
//     committing).
//
// Esc returns to the picker (back-one-step).
func (m *Model) handleNewPRKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil

	case "up", "ctrl+k":
		// Arrow nav on the list. Doesn't consume the textinput's
		// own up-arrow (we don't bind that in textinput) so users
		// can scan without losing typed-in number.
		if m.listCursor > 0 {
			m.listCursor--
		}
		return m, nil
	case "down", "ctrl+j":
		// Bound by FILTERED length so the cursor can't drift past
		// what's visible in the picker.
		if m.listCursor < len(filterPRs(m.newPRs, m.listInput.Value()))-1 {
			m.listCursor++
		}
		return m, nil

	case "enter":
		// Two paths: typed-number wins if the input parses as an
		// integer. Otherwise, fall back to the cursor's selection
		// in the FILTERED list.
		if num, ok := parsePositiveInt(m.listInput.Value()); ok {
			return m.submitNewPR(num)
		}
		filtered := filterPRs(m.newPRs, m.listInput.Value())
		if len(filtered) > 0 && m.listCursor < len(filtered) {
			return m.submitNewPR(filtered[m.listCursor].Number)
		}
		// Nothing typed, no list — surface a hint.
		m.newLoadErr = fmt.Errorf("type a PR number or wait for the list to load")
		return m, nil
	}

	// Forward to textinput. Reset cursor when filter changes so
	// the highlighted row doesn't drift past the visible list.
	prevValue := m.listInput.Value()
	var cmd tea.Cmd
	m.listInput, cmd = m.listInput.Update(msg)
	if m.listInput.Value() != prevValue {
		m.listCursor = 0
		m.newLoadErr = nil
	}
	return m, cmd
}

// submitNewPR is the shared "go fetch this PR and create the
// workspace" path used by both enter-with-number and enter-on-row.
// Flips to busyMode and dispatches the appropriate create command:
// local createCmd for an in-process Manager, or remoteCreateCmd which
// spawns `canopy new --on <host> --pr <num>` so the remote canopy
// resolves the PR via its own gh + git (v0.21 parity).
func (m *Model) submitNewPR(num int) (tea.Model, tea.Cmd) {
	spec := workspace.SourceSpec{PR: num}
	m.busyOp = busyOpCreate
	m.busyTitle = newBusyTitle("", spec)
	m.busyDone = false
	m.busyOutput = ""
	m.busyErr = nil
	m.mode = busyMode
	m.listInput.Blur()
	if m.newTargetHost != "" {
		return m, remoteCreateCmd(m.canopyBinPath(), m.newTargetHost, m.newTargetRemoteCwd, "", spec, "")
	}
	return m, createCmd(m.newTargetMgr, "", spec)
}

// openNewIssue is the issue-picker analog of openNewPR. Same shape,
// different data type: ghx.IssueSummary instead of PRSummary.
//
// Routes to the remote loader for remote targets (v0.21 parity).
func (m *Model) openNewIssue() tea.Cmd {
	m.mode = newIssueMode
	m.listInput.Reset()
	m.listInput.Placeholder = "type an issue number, or arrow to a row below"
	m.listInput.Focus()
	m.listCursor = 0
	m.newLoading = true
	m.newLoadErr = nil
	m.newIssues = nil
	return tea.Batch(textinputBlink(), m.loadIssuesForTarget())
}

// loadIssuesForTarget mirrors loadPRsForTarget for issues.
func (m *Model) loadIssuesForTarget() tea.Cmd {
	if m.newTargetHost == "" {
		return loadIssuesCmd(m.newTargetRoot)
	}
	target, err := m.resolveHostForExec(m.newTargetHost)
	if err != nil {
		return func() tea.Msg {
			return issueListLoadedMsg{err: err}
		}
	}
	return loadIssuesRemoteCmd(target, m.newTargetRemoteCwd)
}

// issueListLoadedMsg is the issue analog of prListLoadedMsg.
type issueListLoadedMsg struct {
	issues []ghx.IssueSummary
	err    error
}

// loadIssuesCmd dispatches ghx.ListIssues in a goroutine.
func loadIssuesCmd(projectRoot string) tea.Cmd {
	return func() tea.Msg {
		issues, err := ghx.ListIssues(context.Background(), projectRoot, 20)
		return issueListLoadedMsg{issues: issues, err: err}
	}
}

// loadIssuesRemoteCmd is the remote-host analog of loadIssuesCmd.
func loadIssuesRemoteCmd(sshTarget, remoteCwd string) tea.Cmd {
	return func() tea.Msg {
		issues, err := ghx.RemoteListIssues(context.Background(), sshTarget, remoteCwd, 20)
		return issueListLoadedMsg{issues: issues, err: err}
	}
}

// handleNewIssueKey mirrors handleNewPRKey for issues. Two enter
// dispatch shapes: typed-number → fetch by ID; arrow-then-enter →
// use cursor's selection. Esc returns to picker.
func (m *Model) handleNewIssueKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil

	case "up", "ctrl+k":
		if m.listCursor > 0 {
			m.listCursor--
		}
		return m, nil
	case "down", "ctrl+j":
		if m.listCursor < len(filterIssues(m.newIssues, m.listInput.Value()))-1 {
			m.listCursor++
		}
		return m, nil

	case "enter":
		if num, ok := parsePositiveInt(m.listInput.Value()); ok {
			return m.submitNewIssue(num)
		}
		filtered := filterIssues(m.newIssues, m.listInput.Value())
		if len(filtered) > 0 && m.listCursor < len(filtered) {
			return m.submitNewIssue(filtered[m.listCursor].Number)
		}
		m.newLoadErr = fmt.Errorf("type an issue number or wait for the list to load")
		return m, nil
	}

	prev := m.listInput.Value()
	var cmd tea.Cmd
	m.listInput, cmd = m.listInput.Update(msg)
	if m.listInput.Value() != prev {
		m.listCursor = 0
		m.newLoadErr = nil
	}
	return m, cmd
}

// submitNewIssue is the shared "go fetch this issue and create the
// workspace" path. Same shape as submitNewPR — routes through
// remoteCreateCmd for a remote target.
func (m *Model) submitNewIssue(num int) (tea.Model, tea.Cmd) {
	spec := workspace.SourceSpec{Issue: num}
	m.busyOp = busyOpCreate
	m.busyTitle = newBusyTitle("", spec)
	m.busyDone = false
	m.busyOutput = ""
	m.busyErr = nil
	m.mode = busyMode
	m.listInput.Blur()
	if m.newTargetHost != "" {
		return m, remoteCreateCmd(m.canopyBinPath(), m.newTargetHost, m.newTargetRemoteCwd, "", spec, "")
	}
	return m, createCmd(m.newTargetMgr, "", spec)
}

// openNewBranch is the branch-picker analog. Doesn't need gh —
// `git for-each-ref` is fast enough that we can load synchronously
// in the open path. Loading state is kept for parity with PR/issue
// pickers and to handle the (rare) slow-disk case.
//
// Routes to the remote loader for remote targets (v0.21 parity).
func (m *Model) openNewBranch() tea.Cmd {
	m.mode = newBranchMode
	m.listInput.Reset()
	m.listInput.Placeholder = "type to filter, e.g. `feat`"
	m.listInput.Focus()
	m.listCursor = 0
	m.newLoading = true
	m.newLoadErr = nil
	m.newBranches = nil
	return tea.Batch(textinputBlink(), m.loadBranchesForTarget())
}

// loadBranchesForTarget mirrors loadPRsForTarget for branches.
func (m *Model) loadBranchesForTarget() tea.Cmd {
	if m.newTargetHost == "" {
		return loadBranchesCmd(m.newTargetRoot)
	}
	target, err := m.resolveHostForExec(m.newTargetHost)
	if err != nil {
		return func() tea.Msg {
			return branchListLoadedMsg{err: err}
		}
	}
	return loadBranchesRemoteCmd(target, m.newTargetRemoteCwd)
}

// branchListLoadedMsg carries the result of an async git
// for-each-ref. Same shape as the PR/issue load messages.
type branchListLoadedMsg struct {
	branches []string
	err      error
}

// loadBranchesCmd dispatches git.ListBranches in a goroutine. Even
// though git is fast, putting it in a goroutine keeps the open
// path consistent (bubbletea Cmd → Msg) and avoids blocking the
// initial render.
func loadBranchesCmd(projectRoot string) tea.Cmd {
	return func() tea.Msg {
		branches, err := git.ListBranches(context.Background(), projectRoot)
		return branchListLoadedMsg{branches: branches, err: err}
	}
}

// loadBranchesRemoteCmd is the remote-host analog of loadBranchesCmd.
func loadBranchesRemoteCmd(sshTarget, remoteCwd string) tea.Cmd {
	return func() tea.Msg {
		branches, err := git.RemoteListBranches(context.Background(), sshTarget, remoteCwd)
		return branchListLoadedMsg{branches: branches, err: err}
	}
}

// handleNewBranchKey is the keymap for the branch picker. Filter
// behavior matches PR/issue pickers (case-insensitive substring),
// but enter takes a STRING (the branch name) instead of a number.
// No "type by name and submit" fast path because branch names can
// contain slashes that conflict with the filter — the user is
// expected to filter then pick.
func (m *Model) handleNewBranchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.openNewPicker()
		return m, nil

	case "up", "ctrl+k":
		if m.listCursor > 0 {
			m.listCursor--
		}
		return m, nil
	case "down", "ctrl+j":
		// Bound by the filtered length, not the raw length, so the
		// cursor doesn't drift past visible rows.
		if m.listCursor < len(filterBranches(m.newBranches, m.listInput.Value()))-1 {
			m.listCursor++
		}
		return m, nil

	case "enter":
		filtered := filterBranches(m.newBranches, m.listInput.Value())
		if len(filtered) == 0 {
			m.newLoadErr = fmt.Errorf("no branches match — adjust filter or check your remote")
			return m, nil
		}
		idx := m.listCursor
		if idx >= len(filtered) {
			idx = len(filtered) - 1
		}
		// Strip the "origin/" prefix if present so the resolver's
		// origin-vs-local logic sees a bare branch name. The branch
		// resolver already handles both routes; passing the bare
		// name is the unambiguous form.
		ref := filtered[idx]
		bare := strings.TrimPrefix(ref, "origin/")
		// AllowLocal flips on when the picked entry is a local-only
		// branch (no origin/<name> alongside it). Detect that by
		// checking whether the matching origin/<bare> exists in the
		// list.
		spec := workspace.SourceSpec{Branch: bare}
		if !strings.HasPrefix(ref, "origin/") {
			// Local-only entry. The resolver requires --allow-local
			// when the branch isn't on origin.
			if !branchHasOrigin(m.newBranches, bare) {
				spec.AllowLocal = true
			}
		}
		return m.submitNewBranch(spec)
	}

	prev := m.listInput.Value()
	var cmd tea.Cmd
	m.listInput, cmd = m.listInput.Update(msg)
	if m.listInput.Value() != prev {
		m.listCursor = 0
		m.newLoadErr = nil
	}
	return m, cmd
}

// submitNewBranch flips to busyMode with a SourceSpec for the
// chosen branch. Allows the SourceSpec to carry AllowLocal for
// local-only branches. Routes through remoteCreateCmd for a remote
// target (v0.21 parity).
func (m *Model) submitNewBranch(spec workspace.SourceSpec) (tea.Model, tea.Cmd) {
	m.busyOp = busyOpCreate
	m.busyTitle = newBusyTitle("", spec)
	m.busyDone = false
	m.busyOutput = ""
	m.busyErr = nil
	m.mode = busyMode
	m.listInput.Blur()
	if m.newTargetHost != "" {
		return m, remoteCreateCmd(m.canopyBinPath(), m.newTargetHost, m.newTargetRemoteCwd, "", spec, "")
	}
	return m, createCmd(m.newTargetMgr, "", spec)
}

// branchHasOrigin returns true when the ListBranches output contains
// "origin/<bare>" alongside the local "<bare>". Used by the branch
// picker to decide whether AllowLocal should be set on the spec.
func branchHasOrigin(branches []string, bare string) bool {
	target := "origin/" + bare
	for _, b := range branches {
		if b == target {
			return true
		}
	}
	return false
}

// filterBranches narrows the loaded branch list. Case-insensitive
// substring match against the full ref (so "origin/feat" can match
// "origin/feat/oauth" and a typed "feat" matches both local + remote).
func filterBranches(branches []string, filter string) []string {
	filter = strings.TrimSpace(strings.ToLower(filter))
	if filter == "" {
		return branches
	}
	out := make([]string, 0, len(branches))
	for _, b := range branches {
		if strings.Contains(strings.ToLower(b), filter) {
			out = append(out, b)
		}
	}
	return out
}

// parsePositiveInt is the shared "is this a PR/issue number" check
// for the picker enter handlers. Returns (n, true) only for integers
// > 0; "0" / "-1" / "abc" / "" all return (_, false).
func parsePositiveInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// newBusyTitle picks the busy-mode title shown while a Create
// operation is in flight. Customized per source variant so the user
// sees "Checking out PR #1234..." instead of a generic spinner —
// useful because the gh + git fetch can take a few seconds before
// scripts.setup even starts.
func newBusyTitle(name string, spec workspace.SourceSpec) string {
	switch {
	case spec.PR > 0:
		return fmt.Sprintf("Checking out PR #%d...", spec.PR)
	case spec.Issue > 0:
		return fmt.Sprintf("Setting up workspace for issue #%d...", spec.Issue)
	case spec.Branch != "":
		return fmt.Sprintf("Checking out branch %q...", spec.Branch)
	}
	if name != "" {
		return fmt.Sprintf("Creating workspace %q...", name)
	}
	return "Creating workspace..."
}

// createDoneMsg carries the result of a Manager.Create call back to
// Update. Output is whatever Create wrote to its stdout/stderr writers.
// tmuxSession is the new workspace's session name on success — Update
// uses it to dispatch an immediate attachCmd so the user lands in the
// running session right after `n` instead of bouncing back to the list.
type createDoneMsg struct {
	output      string
	tmuxSession string
	err         error
	// remote is true when this createDoneMsg came from a
	// remoteCreateCmd dispatch. Update's createDoneMsg handler skips
	// the auto-attach path for remote — we can't local-attach to a
	// session on tower; user presses Enter on the new row to mosh in.
	// v0.17 Phase 1k.
	remote bool
}

// promptSentMsg fires after workspace.SendInitialPrompt finishes
// (either successfully delivered the prompt, or failed with an
// ErrPromptFailed). Carries the session name so the createDoneMsg
// follow-up can dispatch the attach without re-deriving it. err
// is non-nil only when the prompt didn't get delivered — the
// workspace itself is alive either way.
type promptSentMsg struct {
	session string
	err     error
}

// sendPromptCmd dispatches workspace.SendInitialPrompt in a
// goroutine, then emits promptSentMsg with the result. mgr.Tmux
// is the tmux client that knows about the freshly-created session.
// io.Discard for the progress writer because the TUI doesn't render
// that carriage-return progress line — the busyMode "Sending prompt..."
// title is the equivalent feedback.
func sendPromptCmd(mgr *workspace.Manager, session, prompt string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := workspace.SendInitialPrompt(ctx, mgr.Tmux, session, session, prompt, io.Discard)
		return promptSentMsg{session: session, err: err}
	}
}

// createCmd kicks off Manager.Create asynchronously and streams its
// stdout/stderr to the busy view as it runs. spec drives the source
// variant (zero spec = fresh workspace; populated spec = pr/issue/
// branch). The gh shellouts + git fetches happen inside ResolveSource,
// then mgr.Create runs scripts.setup which is the slow, output-y
// part — that's what the user wants to see scroll past in real time.
//
// Mechanism:
//
//   - A safeBuffer captures everything written to the
//     stdout/stderr writers passed to mgr.Create.
//   - Goroutine runs the actual work (resolve + create) and pushes
//     the final result onto a `done` chan.
//   - Returned tea.Batch has TWO cmds:
//     1. progressTickCmd — re-fires every 150ms, drains the
//     buffer, emits progressTickMsg with the new chunk. The
//     tick re-schedules itself in Update until busyDone.
//     2. waitDoneCmd — blocks reading from `done`, emits
//     createDoneMsg when the goroutine finishes.
//
// Both cmds run concurrently under tea.Batch. Update appends ticks
// to m.busyOutput live, then on createDoneMsg appends any final
// bytes the last tick missed.
func createCmd(mgr *workspace.Manager, name string, spec workspace.SourceSpec) tea.Cmd {
	// Lazy spawn: the goroutine + buffer + chan are constructed
	// inside the returned closure, NOT at createCmd's call site.
	// That keeps the cmd value cheap to construct and lets unit
	// tests inspect the returned cmd without accidentally kicking
	// off real work against a nil-mgr fixture.
	//
	// Update sees createStartedMsg first and dispatches the
	// streaming + done cmds via tea.Batch from there.
	return func() tea.Msg {
		buf := &safeBuffer{}
		done := make(chan createDoneMsg, 1)
		go func() {
			ctx := context.Background()
			opts, suggestedName, err := mgr.ResolveSource(ctx, spec)
			if err != nil {
				done <- createDoneMsg{output: buf.Drain(), err: err}
				return
			}
			// Explicit name beats source-derived suggestion beats namegen.
			if name == "" {
				name = suggestedName
			}
			ws, err := mgr.Create(ctx, name, opts, buf, buf)
			// Drain after Create returns so any trailing bytes
			// (the last "Workspace ready" line, etc.) end up in
			// the final createDoneMsg, not stranded in the buffer
			// if the tick timing missed them.
			msg := createDoneMsg{output: buf.Drain(), err: err}
			if err == nil && ws != nil {
				msg.tmuxSession = ws.TmuxSessionName()
			}
			done <- msg
		}()
		return createStartedMsg{buf: buf, done: done}
	}
}

// remoteCreateCmd mirrors createCmd's lazy-spawn + streaming-buffer
// shape, but instead of running mgr.Create it spawns `<canopy> new
// --on <host> --remote-cwd <path> [flags] [<name>]` as a child
// process and pipes its combined stdout/stderr into the safeBuffer
// the TUI's busy view reads. v0.17 Phase 1k.
//
// Reuses createDoneMsg / createStartedMsg / progressTickMsg so the
// Update handler doesn't need a parallel pipeline — only the goroutine
// body differs. Sets done.remote = true so the post-create handler
// knows to skip the auto-attach path (we can't local-attach to a
// session on tower; user presses Enter on the new row to mosh).
//
// promptText is base64'd into a --prompt-file via the existing
// Phase 1f mechanism on the canopy new side; we just pass --prompt
// or --prompt-file via the local flag and let dispatchNewToRemote
// handle the heredoc + tempfile dance.
func remoteCreateCmd(canopyBin, host, remoteCwd, name string, spec workspace.SourceSpec, promptText string) tea.Cmd {
	return func() tea.Msg {
		buf := &safeBuffer{}
		done := make(chan createDoneMsg, 1)

		args := []string{"new", "--on", host}
		if remoteCwd != "" {
			args = append(args, "--remote-cwd", remoteCwd)
		}
		if name != "" {
			args = append(args, "--name", name)
		}
		if spec.PR != 0 {
			args = append(args, "--pr", fmt.Sprintf("%d", spec.PR))
		}
		if spec.Issue != 0 {
			args = append(args, "--issue", fmt.Sprintf("%d", spec.Issue))
		}
		if spec.Branch != "" {
			args = append(args, "--branch", spec.Branch)
		}
		if spec.AllowLocal {
			args = append(args, "--allow-local")
		}
		if promptText != "" {
			args = append(args, "--prompt", promptText)
		}

		go func() {
			cmd := exec.Command(canopyBin, args...)
			cmd.Stdout = buf
			cmd.Stderr = buf
			cmd.Env = append(os.Environ(), "CANOPY_ALLOW_NESTED=1")
			err := cmd.Run()
			msg := createDoneMsg{output: buf.Drain(), err: err, remote: true}
			done <- msg
		}()
		return createStartedMsg{buf: buf, done: done}
	}
}

// createStartedMsg is the bridge between createCmd's lazy spawn and
// the streaming machinery. Update receives it once and dispatches
// the per-tick + wait-done cmds as a batch. Carries the buffer +
// done-chan so the dispatched cmds have what they need.
type createStartedMsg struct {
	buf  *safeBuffer
	done <-chan createDoneMsg
}

// progressTickMsg fires every progressTickInterval while a Create is
// in flight. Carries the freshly-drained chunk and a back-reference
// to the buffer so Update can keep ticking without holding state.
type progressTickMsg struct {
	chunk string
	buf   *safeBuffer
}

// progressTickCmd builds the tick command. The drain happens at
// schedule time (inside the closure) so each tick fetches whatever
// the goroutine has written between this tick and the previous one.
func progressTickCmd(buf *safeBuffer) tea.Cmd {
	return tea.Tick(progressTickInterval, func(time.Time) tea.Msg {
		return progressTickMsg{chunk: buf.Drain(), buf: buf}
	})
}

// waitDoneCmd blocks on the done channel and emits the createDoneMsg
// when the goroutine finishes. Single-shot — only fires once per
// create flow.
func waitDoneCmd(done <-chan createDoneMsg) tea.Cmd {
	return func() tea.Msg {
		return <-done
	}
}

// textinputBlink dispatches the cursor blink command for the textinput.
// Wrapper kept so the modal-open code reads cleanly.
func textinputBlink() tea.Cmd {
	return textinput.Blink
}
