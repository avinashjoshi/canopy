// Package ui hosts canopy's Bubbletea TUI. The TUI is the front door
// when `canopy` is invoked with no subcommand — it shows the list of
// workspaces (plus the main session if alive), lets the user navigate,
// attach, create, and remove without leaving the visual surface.
//
// Architecture: the Model holds a snapshot of state (loaded via
// workspace.Manager.List + the main-session check) and a cursor. Every
// keypress maps to an Update that mutates the Model, possibly returning
// a tea.Cmd to fetch fresh data or hand off to tmux. The View renders
// the Model into a styled table via lipgloss.
//
// We deliberately keep the Manager + state.Store wiring outside this
// package — Model takes a *workspace.Manager and dispatches to it. That
// keeps internal/ui from owning the lifecycle, mirroring how
// cmd/canopy/* subcommands work.
package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/avinashjoshi/canopy/internal/agent"
	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/config"
	"github.com/avinashjoshi/canopy/internal/ghx"
	"github.com/avinashjoshi/canopy/internal/git"
	"github.com/avinashjoshi/canopy/internal/host"
	"github.com/avinashjoshi/canopy/internal/lifecycle"
	"github.com/avinashjoshi/canopy/internal/state"
	"github.com/avinashjoshi/canopy/internal/tmux"
	"github.com/avinashjoshi/canopy/internal/ui/projectlist"
	"github.com/avinashjoshi/canopy/internal/workspace"
)

var log = clog.Pkg("ui")

// Row is a back-compat alias for state.GlobalRow. v0.8 unification
// promoted state.GlobalRow to the canonical row shape (it's what the
// projectlist sub-component renders) and ui.Row went away. Tests in
// the package still write `ui.Row{...}` literals; the alias keeps
// those compiling without rewriting every test.
type Row = state.GlobalRow

// viewMode tracks which screen the TUI is showing.
//
// listMode is the default table.
//
// The new-workspace flow is a two-step picker (canopy convention,
// lazygit-flavored):
//
//   - newPickerMode: variant chooser. Single-key shortcuts pick a
//     branch direction (Fresh / PR / Issue / Branch). Self-evident,
//     no syntax to recall.
//   - newFreshMode: name input for the "blank workspace" path.
//   - newPRMode / newIssueMode / newBranchMode: per-variant sub-modals
//     that load live data (gh / git) and let the user pick from a
//     filtered list.
//   - newPromptMode: prompt input for the "fresh workspace + send the
//     agent an initial task on launch" path. Mirrors the CLI's
//     `canopy new --prompt` flow. The prompt is delivered to the
//     agent pane via workspace.SendInitialPrompt after Create returns
//     and before the auto-attach.
//
// Esc steps back one level; from a sub-modal back to the picker, from
// the picker back to listMode. Never exits canopy outright.
//
// confirmDeleteMode is the y/N prompt before tearing down a workspace;
// busyMode is the wait/output screen during a long-running operation.
type viewMode int

const (
	listMode viewMode = iota
	newPickerMode
	newFreshMode
	newPRMode
	newIssueMode
	newBranchMode
	newPromptMode
	confirmDeleteMode
	// confirmRetryMode is the y/N gate for `R` on a non-broken workspace
	// (D3/CP1). Mirrors the CLI's `canopy retry --force` friction so a
	// muscle-memory R-press doesn't accidentally re-run scripts.setup on
	// a healthy workspace and clobber whatever state setup mutates
	// (db reseed, env regen, etc.).
	confirmRetryMode
	// confirmKillMode is the y/N gate before `K` tears down a workspace's
	// tmux session. Distinct from confirmDeleteMode because K is much
	// less destructive: state.json + worktree dir + branch all survive,
	// only the tmux session goes. Re-pressing Enter after kill resurrects
	// the workspace cleanly. The friction here is "did you really mean
	// to drop this session?", not "are you sure you want to lose work?".
	confirmKillMode
	// confirmAttachMode is the y/N gate before attaching to a session
	// that already has another client connected. v0.17 Phase 1j —
	// surfaces the "this workspace is open in another terminal/window"
	// case so the user doesn't accidentally share/steal a live agent
	// session. Skipped when the target is the workspace canopy was
	// launched from (re-attaching your own session is the normal flow).
	confirmAttachMode
	// confirmHostRemoveMode is the y/N gate before deleting a host
	// from the registry. v0.17 Phase 1l. Same shape as
	// confirmDeleteMode but scoped to hosts.json instead of state.json.
	confirmHostRemoveMode
	// addHostFormMode is the in-TUI add-host form. Single mode with
	// two textinputs (name + ssh-target); Tab switches focus, Enter
	// submits when both are non-empty. Submit writes the registry.
	// v0.17 Phase 1l — replaces the subprocess wizard handoff.
	addHostFormMode
	// hostDetailMode renders read-only detail for the selected host:
	// ssh-target, type, registered projects, version, last seen,
	// last error. Esc back to listMode. v0.17 Phase 1l polish.
	hostDetailMode
	// confirmSSHCopyIDMode is the post-Add prompt offering to run
	// ssh-copy-id when the connection probe came back AuthFailed.
	// y/Y → tea.ExecProcess into ssh-copy-id (which prompts for the
	// remote password); anything else → keep the host registered as-is.
	confirmSSHCopyIDMode
	// confirmHostSSHMode is the y/N gate before `s` execs an interactive
	// ssh into the cursor's host. Light-friction confirmation: SSH is
	// not destructive but does drop the user into a different shell,
	// and we want a deliberate keypress so a stray `s` doesn't bounce
	// them out of the TUI unexpectedly.
	confirmHostSSHMode
	// confirmHostClipboardMode is the y/N gate before `c` runs
	// `canopy host clipboard <name>` against the cursor's host. The
	// command exec's interactively (the user sees the install
	// transcript). v0.18 Lane C.4.
	confirmHostClipboardMode
	// drawerMode is the diagnostic detail drawer (opened with `i`).
	// Read-only view of one workspace's process tree, recent logs, env,
	// status history, and last setup log. The drawer is opt-in (no
	// auto-open) and scope-capped to diagnostics — no editing, no live
	// dev-server tailing, no canopy.json mutation. See the CEO plan at
	// ~/.gstack/projects/canopy/ceo-plans/2026-04-29-tmux-health-and-resurrect.md
	// for the load-bearing scope cap rationale.
	drawerMode
	busyMode
	// upgradeMode is the in-TUI canopy-upgrade flow. Reachable via
	// the `U` key from listMode when the auto-check pill is showing.
	// Owns the screen end-to-end while the upgrade runs (no top-bar
	// pills) and dispatches to its own four-state sub-flow:
	// loading → preview → running → doneOK/doneError. See
	// internal/ui/upgrade.go for the state machine and key handling.
	upgradeMode
	// hostUpgradeMode is the in-TUI flow for upgrading canopy on a
	// remote host (U on the Hosts tab). Mirrors upgradeMode's
	// confirm → running → doneOK/doneError state machine but runs
	// `canopy upgrade --yes` over SSH and streams the output into a
	// captured buffer (no tty pass-through; no flicker). See
	// internal/ui/update_host_upgrade.go for the state machine.
	hostUpgradeMode
	// settingsFormMode is the standalone settings modal reachable from
	// any tab via `,` (v0.20). Currently exposes a single key:
	// source-root. Reuses the same inline-edit textinput + WithLock
	// save flow as Add Project's ctrl+s shortcut, just without the
	// surrounding form chrome. Esc returns to listMode without saving.
	settingsFormMode
	// addProjectFormMode is the in-TUI "Add Project" form (v0.20).
	// Single textinput; Enter classifies the value as path or URL
	// and dispatches:
	//   - path → sync runInit via the m.RunInitFunc callback
	//   - URL  → tea.ExecProcess git clone (drops altscreen so SSH
	//            passphrase / HTTPS credential helpers work natively),
	//            then runInit on the cloned dir
	// Esc cancels back to listMode. ctrl+s opens inline source-root
	// edit (decision #18 in v0.20-add-project.md). Reachable from
	// the Global tab via `a`; same flow drives the splash screen.
	addProjectFormMode
	// ownerFormMode is the single-textinput modal for editing a
	// workspace's owner (the `o` keybind). Enter sets the typed login;
	// ctrl+d clears back to "mine"; Esc cancels. Submit dispatches to
	// Manager.SetOwner for local rows or `canopy set-owner --on <host>`
	// for remote rows. v0.22 distinguish-my-workspaces.
	ownerFormMode
	// agentSwapPickerMode is the v0.22 picker that opens on `A` from
	// the workspaces tab. Lists the cursor row's project canopy.json
	// `agents:` allowlist; arrow-nav + Enter dispatches into
	// Manager.SwapAgent. Esc cancels back to listMode. agentSwapBusy
	// flips true while the swap is in flight; the view renders
	// "Swapping..." then the result message before auto-returning
	// to listMode. See internal/ui/update_agent_swap.go.
	agentSwapPickerMode
	// askPickerMode + askInputMode are the v0.22 "quick second opinion"
	// flow. `Q` opens askPickerMode → user picks the target agent →
	// askInputMode (textarea for the question) → Ctrl+S submits → the
	// TUI writes the question to ~/.canopy/tmp/ask-*.md and spawns
	// `canopy ask <agent> --file <path>` inside a tmux display-popup.
	// The answer renders in the popup; when the popup closes the TUI
	// returns to listMode. See internal/ui/update_ask.go.
	askPickerMode
	askInputMode
)

// inNewFlow reports whether the current mode is any step of the
// new-workspace flow. Used by Update to route messages to the right
// per-mode handler without listing all five constants every time.
func (m viewMode) inNewFlow() bool {
	return m == newPickerMode ||
		m == newFreshMode ||
		m == newPRMode ||
		m == newIssueMode ||
		m == newBranchMode ||
		m == newPromptMode
}

// busyOpKind identifies which long-running operation is currently in
// busyMode. The View uses this to render the right success message
// ("Workspace created" vs "Workspace removed" vs "Workspace recovered")
// and decides what to do after dismiss (e.g., retry's success could
// offer to attach automatically).
type busyOpKind int

const (
	busyOpNone busyOpKind = iota
	busyOpCreate
	busyOpRemove
	busyOpRetry
)

// Model is the Bubbletea state. Constructed via New() (project mode,
// mgr non-nil) or NewUnified() (mgr-optional, used for popup + global
// invocations). Updated via Update(), rendered via View().
//
// v0.8 unification: the same Model serves three contexts — project TUI
// (mgr non-nil, single-project rows), global TUI (mgr nil, cross-project
// rows), and popup mode (CANOPY_IN_POPUP=1, single-line tab bar +
// switch-client attach). The viewMode + popup* fields drive the runtime
// dispatch.
type Model struct {
	// mgr is the current-project Manager. Non-nil when canopy was
	// invoked from inside a project (cwd has canopy.json walk-up).
	// Nil when invoked outside any project (global TUI startup) —
	// rows still populate from state.BuildGlobalRows, but the `n`
	// keybind is hidden and Local tab shows onboarding text.
	mgr *workspace.Manager
	tc  *tmux.Client

	// store is the state.Store the unified model uses for cross-project
	// row aggregation (state.BuildGlobalRows) and transient Manager
	// construction (cross-project d/R). Always set, even when mgr is
	// non-nil — mgr.Store and store point to the same on-disk file
	// in that case.
	store *state.Store

	// list is the embedded projectlist sub-component that owns row
	// rendering + cursor state. The unified TUI delegates rendering
	// to projectlist (the same sub-component the popup used in v0.7),
	// matching the popup's grouped-by-project visual treatment.
	list projectlist.Model

	width  int
	height int

	// Toggles + ephemeral UI state.
	mode     viewMode
	showHelp bool
	err      error // last operational error to surface; cleared on next refresh

	// New-workspace flow state.
	//
	// Step 1 (newPickerMode): newPickerCursor selects which variant
	// to launch. 0..3 maps to fresh / pr / issue / branch.
	//
	// Step 2 (newFreshMode): nameInput captures the optional workspace
	// name. Empty → namegen picks a random one.
	//
	// Step 2b/c (newPRMode, newIssueMode): list-with-filter pickers.
	// listInput is the number-or-filter input; listCursor is the
	// arrow-selected index into the (filtered) list. newPRs / newIssues
	// hold the live data once the async loader returns.
	newPickerCursor int
	nameInput       textinput.Model

	// promptInput captures the textarea for newPromptMode — the "fresh
	// workspace + send the agent an initial task" path. Multi-line
	// (Enter = newline, Ctrl+S = submit) so users can type a real
	// task brief without hopping out to the CLI. The CLI's
	// --prompt-file path is still the right escape hatch for prompts
	// pulled from disk. Empty Value() blocks submit (the whole point
	// of the mode is the prompt content).
	promptInput textarea.Model

	// pendingPrompt is the prompt text captured from newPromptMode that
	// must be sent to the agent pane AFTER mgr.Create succeeds and
	// BEFORE auto-attach. Carries the value across the picker → submit
	// → busy → done → attach handoff; cleared by clearNewTarget.
	// Empty means "no prompt to send" — the createDoneMsg handler
	// branches on it to decide whether to dispatch sendPromptCmd.
	pendingPrompt string

	listInput   textinput.Model
	listCursor  int
	newLoading  bool
	newLoadErr  error
	newPRs      []ghx.PRSummary
	newIssues   []ghx.IssueSummary
	newBranches []string // local + remote branches; remote prefixed "origin/"

	// In-flight new-workspace target. Set by actionNewWorkspace before
	// the picker opens, used by every submit/load handler in the flow,
	// cleared when the flow exits (esc back to listMode, or busy dismiss).
	//
	// Decoupled from m.mgr because the Global tab's cursor may point at a
	// project DIFFERENT from the launch-context project (m.mgr). On Local
	// tab these all collapse to m.mgr / its config; on Global tab they're
	// resolved from the cursor row's ProjectRoot via managerForRow.
	//
	// The picker, sub-modal headers, busy title, and success line all
	// render newTargetName so the user sees which project they're
	// creating in — load-bearing for cross-project intent clarity.
	newTargetMgr  *workspace.Manager
	newTargetRoot string // ProjectRoot of the target project
	newTargetName string // display name (Cfg.Project) for the chip + headers

	// newTargetHost + newTargetRemoteCwd: v0.17 Phase 1k introduced the
	// remote-row branch; v0.21 brought PR/Issue/Branch to parity. When
	// the user presses n on a REMOTE row, the picker opens with these
	// set (newTargetMgr stays nil — there's no local Manager for a
	// project that lives on tower). Submit handlers branch on
	// newTargetHost being non-empty to dispatch `canopy new --on <host>
	// --remote-cwd <path>` as a subprocess (captured to safeBuffer like
	// createCmd). Loaders for PR/Issue/Branch SSH `gh` / `git` on the
	// host inside the remote project cwd so the picker populates the
	// same lists the local flow does.
	newTargetHost      string
	newTargetRemoteCwd string

	// Confirm-delete modal (mode == confirmDeleteMode).
	deleteTarget string // workspace name pending removal
	// deleteTargetRoot scopes deleteTarget to a specific project. Without
	// this, `handleConfirmDeleteKey.resolveTargetMgr` would match by Name
	// only — and on the Global tab, two projects each with a workspace
	// named "foo" would be ambiguous: a refresh between modal-open and
	// confirm could re-order rows so the FIRST `foo` in filteredRows is
	// project B's even though the user pressed d on project A's. Storing
	// the project root snapshots the user's intent and forces an exact
	// (Project, Name) match at confirm time.
	deleteTargetRoot string
	deleteHangs      []string // v0.6 safety check results — populated when 'd' is pressed; non-empty triggers the force-required path in renderConfirmDelete + handleConfirmDeleteKey

	// Agent-swap picker (mode == agentSwapPickerMode). Snapshotted at
	// modal-open time so a refresh between open and Enter doesn't
	// re-roll the (workspace, project, agent list) the user saw.
	// Same scoping rationale as deleteTarget + deleteTargetRoot.
	agentSwapTarget     string   // workspace name
	agentSwapTargetRoot string   // workspace's ProjectRoot
	agentSwapCurrent    string   // workspace's current agent at open time (dimmed in picker)
	agentSwapList       []string // snapshot of Cfg.Agents at open time
	agentSwapCursor     int      // index into agentSwapList
	agentSwapBusy       bool     // SwapAgent call in flight; suppress further keypresses + render "Swapping..."
	agentSwapResult     string   // post-swap message ("Swapped to codex." or error); shown until any key returns to listMode

	// Ask (second-opinion popup) state. Same snapshot rationale as
	// the swap picker: row context captured at open time. Spans two
	// modes (picker → input) so we share the state between them.
	askTarget     string         // workspace name (for prefix)
	askTargetRoot string         // workspace's ProjectRoot
	askList       []string       // snapshot of Cfg.Agents at open time
	askCursor     int            // index into askList during picker
	askAgent      string         // chosen agent after picker → input transition
	askInput      textarea.Model // multi-line question textarea (Ctrl+S submits)
	askErr        string         // last error from temp-file write or popup spawn

	// attachTarget snapshots the row the user pressed Enter on when its
	// session already has another client connected. confirmAttachMode
	// reads it to render the "already attached" prompt; y/Enter proceeds
	// with the original attach. v0.17 Phase 1j.
	attachTarget Row

	// Long-running operation in progress (mode == busyMode). Reused by
	// Create, Remove, and Retry flows.
	busyOp     busyOpKind // distinguishes the success message + post-action
	busyTitle  string     // e.g. "Creating workspace 'bold-falcon'..." / "Removing 'foo'..."
	busyOutput string     // captured stdout/stderr after the goroutine returns
	busyDone   bool       // true once the goroutine completes
	busyErr    error      // the goroutine's error if any (separate from m.err)

	// Loaded once at startup, used in title rendering.
	projectName string

	// ─── v0.8 unification fields ─────────────────────────────────────
	// inPopup is true when CANOPY_IN_POPUP=1 was set by the tmux
	// display-popup -E invocation. Toggles single-line tab bar,
	// switch-client attach (via tea.QuitMsg after tmux switch-client),
	// and the compact help line. Determined once at New time and
	// immutable for the program's lifetime.
	inPopup bool

	// currentProject is the canonical ProjectRoot that the Local tab
	// filters to. Resolved at startup via workspace.ResolveCurrentProject.
	// Empty when the user is outside any registered project — Local tab
	// is then shown but empty (with onboarding text).
	currentProject string

	// currentWorkspace is the registered workspace name whose Path
	// matches cwd at startup. Set when cwd is inside a workspace dir;
	// used to pre-select that workspace in the list on the first
	// rowsLoadedMsg. Empty otherwise (popup launched from main session
	// or outside any workspace — fall back to row 0).
	currentWorkspace string

	// currentWorkspaceRoot is the ProjectRoot of currentWorkspace.
	// Tracked alongside the name so escape/preselect logic disambiguates
	// across projects with same-named workspaces — e.g. project A and
	// B both have a "foo" workspace, and from A/foo deleting B/foo on
	// the Global tab must not trigger an escape switch to B's main.
	currentWorkspaceRoot string

	// initialCursorPlaced flips to true once the first rowsLoadedMsg
	// has been used to position the cursor on currentWorkspace. Without
	// this latch, every subsequent refresh would yank the cursor back
	// to currentWorkspace mid-session, losing whatever the user was
	// hovering on.
	initialCursorPlaced bool

	// tab tracks which top-level tab is active. tabLocal filters to
	// rows whose ProjectRoot matches currentProject; tabGlobal shows
	// every row from state.BuildGlobalRows.
	tab tabKind

	// allRows is the unfiltered row set (cross-project when mgr is nil
	// or global tab is selected). The tab + searchQuery filter
	// projects allRows down to what list renders.
	allRows []state.GlobalRow

	// remoteRows is the last result of host.Refresher.Tick, merged
	// into the rendered listing AFTER allRows (local). v0.17.0 Phase 1b.
	// Each row has its Host field set to the registered host name so
	// the projectlist's render path groups it under a host section header.
	remoteRows []state.GlobalRow

	// remoteRefreshing is true between dispatching a remote refresh Cmd
	// and receiving the result. Used to gate concurrent remote refresh
	// attempts so we don't fan-out twice on overlapping TUI ticks.
	remoteRefreshing bool

	// hostsSpinnerFrame indexes the Braille frame for every host row
	// currently in hosts.StatusLoading. Advanced by hostsSpinnerTickMsg
	// while remoteRefreshing is true; held steady otherwise so the row
	// doesn't flicker after a single host comes back. Reset to 0 on
	// each fresh remote-refresh dispatch so the animation starts at
	// frame 0 rather than mid-rotation.
	hostsSpinnerFrame int

	// hostsSpinnerActive prevents stacking ticks. refresh() flips this
	// true when it dispatches a remote tick + the spinner tick; the
	// tick handler clears it once remoteRefreshing settles back to
	// false. Without the latch, every refresh would queue another
	// independent tick loop and the frame counter would advance N×
	// faster after N refreshes.
	hostsSpinnerActive bool

	// hostList is the snapshot of registered hosts at the most recent
	// refresh. Drives the Hosts tab. Repopulated as part of every
	// remote refresh so the Hosts tab stays in sync without a separate
	// load path. v0.17.0 Phase 1c.
	hostList []host.Host

	// remoteSnaps is the in-memory mirror of remotes-cache.json,
	// keyed by host name. Used by the Hosts tab to render status
	// pills + last-seen. v0.17.0 Phase 1c.
	remoteSnaps map[string]*state.RemoteHostSnapshot

	// hostsCursor indexes the Hosts tab's selected row. Separate from
	// the workspace-list cursor (projectlist owns its own) so the two
	// tabs navigate independently. v0.17 Phase 1l.
	hostsCursor int

	// hostRemoveTarget is the host name pending removal via the
	// confirmHostRemoveMode modal. Cleared on dismiss.
	hostRemoveTarget string

	// hostAddName + hostAddTarget are the form fields for the in-TUI
	// add-host flow. hostAddFocus toggles 0 (name) ↔ 1 (target). Tab
	// cycles focus. v0.17 Phase 1l.
	hostAddName    string
	hostAddTarget  string
	hostAddFocus   int // 0 = name input focused, 1 = target input focused
	// targetInput is the second textinput for the form (ssh-target).
	// We need a dedicated field because nameInput is shared with the
	// workspace-name flow.
	targetInput textinput.Model

	// hostDetailTarget is the host name being viewed in hostDetailMode.
	hostDetailTarget string

	// pendingProbeHost stores the host name we just registered and
	// are awaiting a connectivity probe for. On AuthFailed, used to
	// build the ssh-copy-id command. v0.17 Phase 1l.
	pendingProbeHost   string
	pendingProbeTarget string

	// hostSSHName / hostSSHTarget stash the cursor host's identity
	// across the confirmHostSSHMode modal so the y/N handler can exec
	// ssh against the right target even if the cursor moves underneath
	// (e.g. a remote-refresh tick between modal-open and confirm).
	hostSSHName   string
	hostSSHTarget string

	// hostClipboardName / hostClipboardTarget stash the cursor host's
	// identity across confirmHostClipboardMode. Same pattern as the
	// SSH modal pair above. v0.21 clipboard bridge.
	hostClipboardName   string
	hostClipboardTarget string

	// Add Project (v0.20) form fields.
	//
	// addProjectInput captures the URL/path the user types. Reset on
	// open via openAddProjectForm. Kept separate from nameInput so the
	// Add Project form and the new-workspace flow can coexist without
	// fighting over textinput state.
	addProjectInput textinput.Model

	// Owner-edit form (ownerFormMode, `o` keybind). ownerInput holds the
	// login the user types; ownerError renders an inline rejection (e.g.
	// empty submit); ownerTarget is the row being edited so submit knows
	// the name + whether to dispatch locally or to a remote host. v0.22.
	ownerInput  textinput.Model
	ownerError  string
	ownerTarget Row

	// hideReviewing, when true, drops rows the user is only reviewing
	// (someone else's work) from the list so it collapses to their own
	// workspaces. Toggled by the `m` keybind. v0.22.
	hideReviewing bool

	// reviewHiddenCount is how many rows the hideReviewing filter
	// dropped on the last projection. Surfaced as a banner below the
	// list so a hidden row reads as "filtered", not "missing data".
	// Recomputed every filteredRows() call. v0.22.
	reviewHiddenCount int

	// addProjectError renders below the input in errorStyle when
	// validation or the orchestrator returns an error. Cleared on the
	// next keystroke so the user sees feedback only while their input
	// is still the problem.
	addProjectError string

	// addProjectToast is the post-success line (e.g. "✓ Added bar at
	// ~/Work/bar"). Set when an add succeeds; cleared by a tick after
	// addProjectToastFor expires so the form auto-closes.
	addProjectToast    string
	addProjectToastFor time.Time

	// addProjectEditingSourceRoot is true while the user is editing
	// source-root inline (ctrl+s on the form). The textinput is
	// reused — addProjectInput becomes the source-root editor for
	// the duration. On Enter we write to ~/.canopy/config.json and
	// restore the previous input value.
	addProjectEditingSourceRoot bool
	// addProjectSavedInput holds the URL/path the user was typing
	// when they hit ctrl+s. Restored to addProjectInput when they
	// finish editing source-root.
	addProjectSavedInput string

	// addProjectTargets is the ordered list of dispatch targets the
	// form's Tab key cycles through. Index 0 is always "" (local
	// canopy); subsequent entries are registered host names in
	// sorted order. Populated by openAddProjectForm so a host
	// registered mid-session shows up next time the form opens.
	addProjectTargets []string

	// addProjectTargetIdx is the cursor into addProjectTargets. 0
	// means local; >0 means a remote host. Tab increments mod len;
	// Shift+Tab decrements.
	addProjectTargetIdx int

	// RunInitFunc is the post-clone init callback. main() injects a
	// closure that calls runInit (and registers the project in
	// state.json) on the given absolute path. nil disables Add Project.
	//
	// withScripts and force mirror `canopy init` flags. The TUI never
	// sets force=true today; --with-scripts is a future toggle.
	RunInitFunc func(absPath string, withScripts, force bool) error

	// searchMode is true while the user is typing in the fuzzy-search
	// box (entered via /). Captures keystrokes into searchQuery
	// instead of forwarding to the listMode keymap.
	searchMode bool
	// searchQuery is the current fuzzy-search filter string. Empty
	// means no filter. Subsequence match (fzf-style) against
	// row name + project + branch.
	searchQuery string

	// confirmRetryNonBroken triggers a y/N modal before R re-runs
	// scripts.setup on a non-broken workspace (D3/CP1). Mirrors the
	// CLI's --force friction. The pending row name is held in
	// retryTarget; the modal is the busyMode parent waiting for input.
	retryTarget string

	// memCache caches per-session RSS+CPU values for the load column
	// with a 5s TTL. Without caching, every refresh tick would spawn
	// a `ps -A` per workspace row. Invalidated on K (kill) so the
	// just-killed row flips to "—" immediately rather than lagging
	// the actual state by up to TTL seconds.
	memCache *state.MemCache

	// detector classifies agent-pane state for the badge column.
	// Owns per-pane history; ticked every agentPollInterval. Single
	// instance shared across the TUI lifetime; pruned every tick to
	// drop history for panes that no longer exist.
	detector *agent.Detector

	// agentStates is the latest poll's session-name → state map.
	// Snapshot pushed to projectlist via SetAgentStates after each
	// successful tick. Keyed by tmux session name so Global-tab rows
	// from different projects don't collide.
	agentStates map[string]agent.State

	// agentPollGen is the generation token for the agent-state poll
	// loop (codex review v3-B3). Incremented on Init; in-flight ticks
	// captured a value at schedule time and drop themselves if they
	// see a newer generation. Only the accepted tick reschedules, so
	// at most one tick is ever in flight regardless of how many times
	// Init is re-entered.
	agentPollGen uint64

	// Confirm-kill modal (mode == confirmKillMode). K kills the
	// workspace's tmux session; the y/N gate prevents accidental
	// keypress. killTargetRoot scopes by ProjectRoot for the same
	// reason deleteTargetRoot does — see comment on deleteTargetRoot.
	// killTargetHost disambiguates synthetic-name rows (notably
	// "(main)") across local + remote: a local-tab project and a remote
	// host can both have a "(main)" row, and ProjectRoot alone isn't
	// enough (remote rows leave it empty). killTargetProject
	// disambiguates further when the SAME host has two registered
	// projects — both emit "(main)" rows with the same Host, so we
	// also key on Project to pick the right one.
	killTarget        string
	killTargetRoot    string
	killTargetHost    string
	killTargetProject string

	// Drawer state (mode == drawerMode). The drawer snapshots the row
	// it was opened against and the loaded diagnostic data so re-renders
	// during async refreshes don't pull data from underneath the user.
	drawerRow      Row
	drawerProcInfo string // pre-rendered process tree text, "" while loading
	drawerLogTail  string // last N lines of ~/.canopy/log/canopy-<ws>.log
	drawerSetupLog string // last setup run output, or "no setup log captured"
	drawerErr      error  // non-fatal load error to surface in-drawer

	// Version pill state. Set by SetVersionInfo from cmd/canopy on
	// startup so the top bar can surface "release vs DEV, and which
	// workspace if dev" at a glance. Both fields empty means "no
	// version info available" — the pill is omitted from the top bar
	// and the existing chrome is unchanged.
	//
	// versionLabel is the human-friendly version string ("v0.12.0",
	// "main-abc1234", or "dev"). Shown muted gray for release builds.
	//
	// devWorkspace is the canopy workspace name when the running
	// canopy is a dev build inside a known worktree, or "" when not
	// detectable. Non-empty triggers the cyan DEV pill regardless of
	// versionLabel; empty + versionLabel == "dev" still renders DEV
	// (untracked); empty + versionLabel != "dev" renders the version
	// pill normally.
	versionLabel string
	devWorkspace string

	// upgradeAvailable is the bare semver of a newer canopy release
	// that's available, or "" when no upgrade is available / has
	// been dismissed / running on a DEV build. Mutates the version
	// pill from "v0.12.3" to "v0.12.3 ⇑ v0.13.0" (yellow arrow).
	// Set by SetUpgradeAvailable on startup (sync read from the
	// auto-check cache) and updated mid-session by the async
	// refresh tea.Cmd when the cache was stale at startup.
	upgradeAvailable string

	// upgradeRefreshFn is the closure that performs the async
	// network fetch. Wired unconditionally by route.go (when not
	// DEV) so the `r` key can force a refresh regardless of whether
	// the cache was fresh at startup. Init() decides whether to
	// fire it on launch via upgradeRefreshOnInit.
	upgradeRefreshFn UpgradeRefreshFn

	// upgradeRefreshOnInit gates whether Init() fires the refresh
	// closure on TUI startup. True when the auto-check cache was
	// missing or stale (TTL expired) at construction. False when
	// the cache was fresh — we trust the cached value at startup
	// and only refresh on explicit user action (`r` key).
	upgradeRefreshOnInit bool

	// In-TUI upgrade flow state. Active only when mode == upgradeMode.
	// Reset to zero on dismiss (resetUpgradeMode). Lives in upgrade.go
	// alongside the state machine.
	upgradeState         upgradeState
	upgradeChangelog     string
	upgradeChangelogVP   viewport.Model // scrollable preview pane
	upgradeChangelogInit bool           // viewport sized + content set
	upgradeShipped       string         // version that just installed (for doneOK message)
	upgradeOutput        string
	upgradeErr           error
	upgradeBuf           *safeBuffer
	upgradeCancel        context.CancelFunc
	upgradeChangelogFn   UpgradeChangelogFn
	upgradeShellFn       UpgradeShellFn
	upgradeDismissFn     UpgradeDismissFn

	// Host-upgrade flow state. Active only when mode == hostUpgradeMode.
	// Reset to zero on dismiss (resetHostUpgradeMode). State machine lives
	// in update_host_upgrade.go; parallel to upgradeMode but for a remote
	// canopy installation reached over SSH.
	//
	// The same state machine handles `canopy upgrade` AND `canopy use
	// release` because they share the shape (confirm → run → done) and
	// rendering chrome — they differ only in title, action verb, and
	// the remote command. Carrying the variants here keeps callers
	// from threading per-flow plumbing through every msg type.
	hostUpgradeState     hostUpgradeState
	hostUpgradeHost      string // selected host name
	hostUpgradeTarget    string // resolved ssh_target
	hostUpgradeVersion   string // remote's current canopy_version at action time
	hostUpgradeAction    string // short label: "upgrade", "use release"
	hostUpgradeVerb      string // present-continuous: "Upgrading", "Switching to release"
	hostUpgradeSuccess   string // doneOK headline: "Upgrade complete", "Switched to release"
	hostUpgradeRemoteCmd string // shell-parseable command to run over SSH
	hostUpgradeOutput    string
	hostUpgradeErr       error
	hostUpgradeBuf       *safeBuffer
	hostUpgradeCancel    context.CancelFunc
}

// UpgradeRefreshFn performs the async cache refresh: fetches the
// latest VERSION from upstream, writes the cache, returns the
// upgrade-available semver (or "" when the user is up to date or
// has dismissed the latest). Network errors surface here so the
// caller (the UI) can log; the UI does NOT change pill state on
// error — the existing cached value stays visible.
type UpgradeRefreshFn func(ctx context.Context) (latest string, err error)

// SetVersionInfo records the running binary's version surface for the
// top-bar pill. Called by cmd/canopy after constructing the model so
// the UI never has to know about ldflags or BuildInfo — it just renders
// the strings it's given. Safe to call with all-empty arguments to
// suppress the pill (e.g., in tests that don't care about chrome).
func (m *Model) SetVersionInfo(versionLabel, devWorkspace string) {
	m.versionLabel = versionLabel
	m.devWorkspace = devWorkspace
}

// SetUpgradeAvailable records the bare semver of an available newer
// canopy release. Empty string suppresses the upgrade-arrow branch on
// the version pill. Caller is responsible for the gating logic
// (DEV-binary check, dismissal, version equality) — this setter just
// stores the value the renderer should display.
//
// Called by RunUnified on startup (sync read from
// ~/.canopy/upgrade-check.json) and updated mid-session by the
// upgradeCheckedMsg handler when the async refresh lands.
func (m *Model) SetUpgradeAvailable(latest string) {
	m.upgradeAvailable = latest
}

// hostReferenceVersion picks the bare semver each remote host's
// canopy_version is compared against on the Hosts tab. Returns "" when
// no meaningful comparison is possible (the renderer falls back to
// DriftUnknown / no badge).
//
// Priority:
//
//  1. Laptop is a release build — use the laptop's own version. The
//     intent is "show me hosts that don't match my local," which is
//     the most common dogfood workflow (you just upgraded local, now
//     visit the Hosts tab to find which hosts to U-key).
//
//  2. Laptop is a dev build with a known upstream-latest — fall back
//     to that. The dev case is exactly when canopy contributors are
//     reaching out to dev fleets, and "compared to the public release"
//     is the only number that's meaningful to compare against.
//
//  3. Otherwise (dev with no cache, e.g. offline first-run) — return
//     "" to suppress the badge entirely.
func (m *Model) hostReferenceVersion() string {
	if m.devWorkspace == "" && m.versionLabel != "" && m.versionLabel != "dev" {
		return m.versionLabel
	}
	return m.upgradeAvailable
}

// SetUpgradeRefreshFn wires the async refresh closure that fires
// from Init() when the auto-check cache was missing or stale at
// startup. Pass nil to skip refresh entirely (tests, popup mode
// where we want minimal startup work, etc.).
func (m *Model) SetUpgradeRefreshFn(fn UpgradeRefreshFn) {
	m.upgradeRefreshFn = fn
}

// upgradeCheckedMsg lands when the async refresh started by Init
// completes. Carries the new latest_version (or "" if up-to-date).
// Errors from the refresh are not propagated — they're logged at
// the closure layer; the UI just doesn't update pill state on
// failure.
type upgradeCheckedMsg struct {
	latest string
}

// upgradeRefreshCmd wraps the closure in a tea.Cmd. Returns nil when
// no refresh fn is wired (test path, or the caller intentionally
// suppressed it). Uses a 10s timeout so a stalled HTTP/git fetch
// can't hang Bubbletea forever.
func upgradeRefreshCmd(fn UpgradeRefreshFn) tea.Cmd {
	if fn == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		latest, _ := fn(ctx)
		return upgradeCheckedMsg{latest: latest}
	}
}

// tabKind identifies which top-level tab is active in the unified TUI.
type tabKind int

const (
	// tabLocal shows only rows whose ProjectRoot matches m.currentProject.
	// Pre-selected when canopy was invoked from inside a project; the
	// "scope is what I'm working on right now" view.
	tabLocal tabKind = iota
	// tabGlobal shows every workspace canopy knows about across all
	// projects. Pre-selected when canopy was invoked from outside any
	// project; the "give me everything" view.
	tabGlobal
	// tabHosts shows the fleet of registered remote canopy hosts —
	// status, project + workspace counts, version, last-seen. v0.17.0
	// Phase 1c. Empty section when no hosts are registered (the user
	// just sees `n: add host`). Tab cycles through Project → Global →
	// Hosts.
	tabHosts
)

// managerForRow returns a *workspace.Manager scoped to the row's project.
// When the row's ProjectRoot matches the current Manager's project root,
// returns m.mgr directly (no construction cost). When the row is in a
// different project (cross-project d/R from the Global tab), constructs
// a transient Manager via config.LoadFrom + workspace.New.
//
// Returns an error when the row's canopy.json is missing/parse-broke or
// Manager construction fails. Caller surfaces via m.err so the user
// sees a status-line hint instead of a panic.
//
// Why no caching: per-action canopy.json reads are <1ms; caching would
// add staleness bugs (project's canopy.json edited mid-session) for
// negligible perf gain. Boring choice.
func (m *Model) managerForRow(row Row) (*workspace.Manager, error) {
	if m.mgr != nil && row.ProjectRoot == m.mgr.Cfg.ProjectRoot {
		return m.mgr, nil
	}
	if row.ProjectRoot == "" {
		return nil, fmt.Errorf("row %q has no ProjectRoot — can't resolve project Manager", row.Name)
	}
	cfg, err := config.LoadFrom(row.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("project config unavailable at %s: %w", row.ProjectRoot, err)
	}
	mgr, err := workspace.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("manager construction failed for %s: %w", cfg.Project, err)
	}
	return mgr, nil
}

// branchInWorkspace reports whether a branch name is currently
// checked out by an existing canopy workspace in this project. Used
// by the PR + branch pickers to tag conflicting rows so the user
// doesn't try to create a duplicate (which would fail at git-
// worktree-add time anyway with a confusing error).
//
// Match is exact-string. Caller is responsible for normalizing the
// branch name (stripping "origin/" prefix etc.) before passing it
// in. The main row is excluded — its branch is "—" sentinel and
// shouldn't shadow real workspaces.
func (m *Model) branchInWorkspace(branch string) (string, bool) {
	if branch == "" {
		return "", false
	}
	for _, r := range m.allRows {
		if r.IsMain {
			continue
		}
		// Project-context check: only match against rows in the current
		// project's source repo. Cross-project branch collisions are
		// not the same conflict (each project has its own git tree).
		// Rows with empty ProjectRoot pass through (legacy project-mode
		// rows + tests that don't set the field). Scope check uses the
		// in-flight new-flow target when set (cross-project from Global
		// tab); falls back to m.mgr for the Local-tab path and any
		// caller outside the new flow.
		scopeRoot := ""
		switch {
		case m.newTargetRoot != "":
			scopeRoot = m.newTargetRoot
		case m.mgr != nil:
			scopeRoot = m.mgr.Cfg.ProjectRoot
		}
		if scopeRoot != "" && r.ProjectRoot != "" && r.ProjectRoot != scopeRoot {
			continue
		}
		if r.Branch == branch {
			return r.Name, true
		}
	}
	return "", false
}

// New constructs a project-mode Model. mgr is required. Used by the
// project TUI entry path (when canopy.json walk-up succeeds).
//
// Wraps NewUnified with the project-mode defaults: tabLocal pre-selected,
// currentProject = mgr.Cfg.ProjectRoot.
func New(mgr *workspace.Manager) *Model {
	return NewUnified(mgr, mgr.Store, mgr.Tmux, mgr.Cfg.ProjectRoot, "", "")
}

// NewUnified is the v0.8 unified-TUI constructor. Single entry point for
// every canopy invocation: project, global, popup. mgr is optional —
// nil when the user invoked canopy from outside any registered project
// or from a popup whose host pane isn't in a known project.
//
// currentProject is the canonical ProjectRoot for the Local tab filter
// (resolved upstream by workspace.ResolveCurrentProject); empty disables
// Local-tab filtering.
//
// Popup-mode rendering is detected via CANOPY_IN_POPUP=1 (set by the
// tmux display-popup -E invocation in install_tmux.go). Single source of
// truth: the env var is what flips chrome from fullscreen to popup.
func NewUnified(mgr *workspace.Manager, store *state.Store, tc *tmux.Client, currentProject, currentWorkspaceRoot, currentWorkspace string) *Model {
	ti := textinput.New()
	ti.Placeholder = "leave blank for a random name"
	ti.CharLimit = 60
	ti.Width = 40

	li := textinput.New()
	li.Placeholder = "type to filter, or a number to fetch by ID"
	li.CharLimit = 80
	li.Width = 60

	// targetInput backs the add-host form's ssh-target field. Kept
	// separate from nameInput so both can render side-by-side and
	// Tab can switch focus without juggling one widget's state.
	tgti := textinput.New()
	tgti.Placeholder = "user@host or host.tail.ts.net"
	tgti.CharLimit = 200
	tgti.Width = 40

	// addProjectInput backs the Add Project form's URL/path field
	// (v0.20). CharLimit large enough for a long GitHub Enterprise URL.
	api := textinput.New()
	api.Placeholder = "https://github.com/foo/bar.git or ~/code/foo"
	api.CharLimit = 1024
	api.Width = 60

	// ownerInput backs the owner-edit form (`o`). A GitHub login or a
	// person's name; capped well above GitHub's 39-char login limit to
	// allow free-form names.
	owi := textinput.New()
	owi.Placeholder = "github login or name"
	owi.CharLimit = 64
	owi.Width = 40

	// Multi-line textarea: Enter inserts newline, Ctrl+S submits
	// (intercepted by handleNewPromptKey before this widget sees the
	// key — see internal/ui/update.go). CharLimit 8KB caps the
	// total prompt size (CLI's --prompt-file path stays at 32KB —
	// users with bigger prompts should drop to that surface).
	//
	// Sizing: SetHeight(10) gives a generous initial viewport; the
	// textarea scrolls internally when content exceeds it, so a
	// paste of 100 lines fits — the user can arrow up/down to
	// review. MaxHeight stays at the bubbles default (99) so the
	// textarea's own scroll machinery is the only limit. The
	// renderNewPrompt() wrapper draws a single outer border around
	// the entire View() output (cleaner than the per-line Base-style
	// border, which left a visual gap at the top corner).
	pi := textarea.New()
	pi.Placeholder = "task to send the agent (e.g. add OAuth login)"
	pi.CharLimit = 8 * 1024
	pi.SetWidth(60)
	pi.SetHeight(10)
	pi.ShowLineNumbers = false
	// Visual cleanup vs the bubbles defaults:
	//   - Prompt "" drops the per-line "│ " glyph (it stacks down
	//     every row including empty ones — looks like a column of
	//     stutter on a half-filled textarea).
	//   - CursorLine cleared so the placeholder line isn't painted
	//     with a contrasting background block (the default looks
	//     like a selection highlight on an empty input — confusing).
	// The outer rounded border is applied by renderNewPrompt rather
	// than via Base style; rendering it through the textarea's
	// per-line draw produces a broken top-edge under certain widths.
	pi.Prompt = ""
	pi.FocusedStyle.Prompt = lipgloss.NewStyle()
	pi.BlurredStyle.Prompt = lipgloss.NewStyle()
	pi.FocusedStyle.CursorLine = lipgloss.NewStyle()
	pi.BlurredStyle.CursorLine = lipgloss.NewStyle()

	// Tab pre-selection: the project-scoped tab when canopy was
	// launched inside a project (so the user lands on their own
	// workspaces), Projects (Global) otherwise. v0.17 Phase 1h.
	defaultTab := tabLocal
	if currentProject == "" {
		defaultTab = tabGlobal
	}

	projectName := ""
	if mgr != nil {
		projectName = mgr.Cfg.Project
	}

	m := &Model{
		mgr:                  mgr,
		tc:                   tc,
		store:                store,
		projectName:          projectName,
		nameInput:            ti,
		listInput:            li,
		targetInput:          tgti,
		addProjectInput:      api,
		ownerInput:           owi,
		promptInput:          pi,
		mode:                 listMode,
		inPopup:              os.Getenv("CANOPY_IN_POPUP") == "1",
		currentProject:       currentProject,
		currentWorkspace:     currentWorkspace,
		currentWorkspaceRoot: currentWorkspaceRoot,
		tab:                  defaultTab,
	}
	// projectlist owns row rendering + cursor. We supply nil callbacks
	// because the unified TUI's bindings table dispatches activate /
	// goToProject / refresh — projectlist's own keymap (up/down/enter)
	// fires through Update but the activate path is handled by the
	// parent's `enter` binding (actionAttach), not projectlist's
	// OnActivate. SetRows happens after each refresh.
	m.list = projectlist.New(projectlist.Options{})
	m.list.SetCurrent(currentWorkspaceRoot, currentWorkspace)

	// Preload the host registry synchronously so the Workspaces tab can
	// render host-loading placeholders on the very first frame — without
	// this, m.hostList stays empty until the first SSH fan-out lands
	// (~3s) and registered hosts are silently missing from the listing
	// during that window. Failure is non-fatal: the refresh path still
	// populates hostList from its own reg.List() result. v0.22.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if reg, err := host.NewRegistry(filepath.Join(home, ".canopy")); err == nil {
			if hosts, err := reg.List(); err == nil {
				m.hostList = hosts
			}
		}
	}
	return m
}

// RunUnifiedOptions groups the optional knobs passed into RunUnified.
// All fields are optional; the zero value gives the bare TUI with no
// version pill, no auto-check, no in-TUI upgrade flow.
//
// Lives next to RunUnified rather than being separately documented
// because it exists solely as RunUnified's options bag — when the
// next field is added, it lands here and RunUnified's call sites
// don't shift positionally.
type RunUnifiedOptions struct {
	// VersionLabel is the human-friendly version string for the
	// top-bar pill ("v0.13.0+abc1234"). Empty suppresses the pill.
	VersionLabel string

	// DevWorkspace is the canopy workspace name when the running
	// canopy is a DEV build inside a known worktree. Non-empty
	// triggers the cyan DEV pill regardless of VersionLabel.
	DevWorkspace string

	// InitialUpgrade is the bare semver of an available newer
	// canopy release, read synchronously from the auto-check cache
	// at startup. Empty when no upgrade is available, the cache
	// is missing, the user has dismissed, or running on DEV.
	InitialUpgrade string

	// RefreshFn is the async closure that performs the network
	// fetch + cache write. Result lands as upgradeCheckedMsg and
	// updates the pill mid-session. Wired unconditionally so the
	// `r` key can force a refresh; Init() only fires it on launch
	// when RefreshOnInit is also true. Nil disables refresh.
	RefreshFn UpgradeRefreshFn

	// RefreshOnInit gates whether Init() fires RefreshFn at TUI
	// launch. True when the auto-check cache was stale or missing
	// at construction (caller derived this from initialUpgradeForUI).
	// False when the cache was fresh — startup uses the cached
	// value and skips the network call. The `r` key fires RefreshFn
	// regardless of this flag.
	RefreshOnInit bool

	// ChangelogFn fetches the CHANGELOG slice for the in-TUI
	// upgrade flow's preview state. Nil disables the U key.
	ChangelogFn UpgradeChangelogFn

	// ShellFn runs git pull + make install for the in-TUI upgrade
	// flow's running state. Nil disables the U key.
	ShellFn UpgradeShellFn

	// DismissFn writes dismissed_version into the auto-check cache
	// for the D key. Nil disables D.
	DismissFn UpgradeDismissFn

	// RunInitFunc is the v0.20 Add Project callback. Wires the TUI's
	// addProjectFormMode to cmd/canopy/runInit so the form can finish
	// the init half of the add-project flow without internal/ui
	// importing cmd/canopy. Nil disables the `a` keybind cleanly
	// (the binding is gated on this being non-nil).
	RunInitFunc func(absPath string, withScripts, force bool) error
}

// RunUnified is the v0.8 public entry point used by cmd/canopy/route.go.
// Single bubbletea program for every canopy invocation: project, global,
// popup. mgr is optional — nil when invoked from outside a registered
// project. currentProject is the resolved Local-tab filter root.
//
// Optional knobs (version pill, auto-check, in-TUI upgrade flow) live
// in RunUnifiedOptions to keep the positional signature short and
// guard against argument-order bugs as more features land. Pass the
// zero value for the bare TUI.
//
// In popup mode (CANOPY_IN_POPUP=1) we omit MouseCellMotion since the
// popup is keyboard-driven and mouse handling adds latency.
func RunUnified(mgr *workspace.Manager, store *state.Store, tc *tmux.Client, currentProject, currentWorkspaceRoot, currentWorkspace string, opts RunUnifiedOptions) error {
	m := NewUnified(mgr, store, tc, currentProject, currentWorkspaceRoot, currentWorkspace)
	m.RunInitFunc = opts.RunInitFunc
	m.SetVersionInfo(opts.VersionLabel, opts.DevWorkspace)
	m.SetUpgradeAvailable(opts.InitialUpgrade)
	m.SetUpgradeRefreshFn(opts.RefreshFn)
	m.upgradeRefreshOnInit = opts.RefreshOnInit
	m.SetUpgradeChangelogFn(opts.ChangelogFn)
	m.SetUpgradeShellFn(opts.ShellFn)
	m.SetUpgradeDismissFn(opts.DismissFn)
	teaOpts := []tea.ProgramOption{tea.WithAltScreen()}
	if !m.inPopup {
		teaOpts = append(teaOpts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, teaOpts...)
	_, err := p.Run()
	return err
}

// Run is the legacy project-mode entry point. RunUnified is the v0.8
// unified entry; Run is preserved as a thin wrapper for any external
// callers (none today) and the e2e tests in the workspace package.
//
// Pre-v0.8 Run had an exit-7 signal channel for the popup-from-project
// nested-canopy flow. That flow is gone — popup hosts the unified TUI
// directly, no nested spawn — so this is now a straightforward Bubbletea
// run-loop wrapper.
func Run(mgr *workspace.Manager) error {
	m := New(mgr)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// Init implements tea.Model. Returns the initial command — load the
// workspace list as soon as the program starts. The refresh path is
// dual: when mgr is non-nil it uses mgr.List + mgr.Reconcile (project
// mode); when nil it falls back to state.BuildGlobalRows (global +
// popup-without-project mode). Either way the result lands in
// m.allRows; tab + search filtering happens on every render.
func (m *Model) Init() tea.Cmd {
	if m.memCache == nil {
		m.memCache = state.NewMemCache(state.DefaultMemCacheTTL)
	}
	// Auto-populate the Mem/CPU column on first render. Bubbletea's
	// async-Cmd model means the table still appears instantly with
	// Mem="—" placeholders; the rowsLoadedMsg arrives a moment later
	// (one ps -A per session, ~5-10ms on a typical workstation) and
	// triggers a re-render with the populated values. No explicit `r`
	// gesture required — the column "just works" the way every other
	// column does.
	cmds := []tea.Cmd{m.refresh()}
	// Async upgrade check refresh. Two gates: closure must be wired
	// (skipped for tests, popup mode, DEV builds), AND the caller
	// must have flagged the cache as stale/missing via
	// upgradeRefreshOnInit. Fresh-cache startup uses the cached value
	// and skips the network call to keep TUI launch quiet. The `r`
	// key fires the closure unconditionally (see actionRefresh) so
	// users can force a refresh even when the cache was fresh at
	// startup.
	if m.upgradeRefreshOnInit {
		if cmd := upgradeRefreshCmd(m.upgradeRefreshFn); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	// Kick off the agent-state badge poll loop. The first tick fires
	// after agentPollInterval; until then badges stay empty (matches
	// the v3 design's stale-then-fresh acceptance).
	if cmd := m.startAgentPolling(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// rowsLoadedMsg is the result of a refresh. Carries the new rows or an
// error; Update applies them to the Model.
type rowsLoadedMsg struct {
	rows []state.GlobalRow
	err  error
}

// remoteRowsLoadedMsg carries the result of a host.Refresher.Tick that
// fanned out `canopy ls --json` calls to each registered host. v0.17.0
// Phase 1b. Bubbletea routes this in parallel with the local
// rowsLoadedMsg; the Model merges both into the rendered listing.
type remoteRowsLoadedMsg struct {
	rows  []state.GlobalRow                    // already host-tagged + flattened
	snaps map[string]*state.RemoteHostSnapshot // for persistence to remotes-cache.json + Hosts tab
	hosts []host.Host                          // host registry snapshot, for the Hosts tab
	err   error
}

// refreshCmd reconciles state, then loads workspaces + the main row.
// Runs in a goroutine via tea.Cmd so the UI doesn't block on tmux/disk.
//
// Always uses state.BuildGlobalRows so the unified TUI's row data shape
// is uniform across project and global invocations. When mgr is non-nil,
// we run Reconcile first (which mutates state.json) so the freshly-built
// rows reflect the latest stopped/ready transitions.
func refreshCmd(mgr *workspace.Manager, tc *tmux.Client, store *state.Store) tea.Cmd {
	return refreshCmdWithMem(mgr, tc, store, nil)
}

// refresh is the model-bound refresh. Always populates the Mem+CPU
// column when a memCache is configured — auto-load is the right
// default since Bubbletea's async Cmd model keeps the first render
// instant and the populated values arrive on the next tick.
//
// v0.17.0 Phase 1b: also dispatches a remote-host refresh in parallel
// (when any hosts are registered). Local rows arrive via
// rowsLoadedMsg as before; remote rows arrive via remoteRowsLoadedMsg.
// The Update handler merges both into the rendered listing.
func (m *Model) refresh() tea.Cmd {
	cmds := []tea.Cmd{refreshCmdWithMem(m.mgr, m.tc, m.store, m.memCache)}
	if !m.remoteRefreshing {
		// refreshRemoteCmd is always non-nil — it returns an empty
		// remoteRowsLoadedMsg when no hosts are registered. We always
		// dispatch it so the refreshing-latch lifecycle is consistent.
		m.remoteRefreshing = true
		m.hostsSpinnerFrame = 0
		// Push the loading-hosts set into projectlist so the workspaces
		// tab can decorate each registered host's header with a spinner
		// while we wait for SSH to return. Mirrors the Hosts tab's
		// StatusLoading glyph so both surfaces signal "we're checking"
		// in lockstep. v0.22.
		m.pushLoadingHosts()
		cmds = append(cmds, refreshRemoteCmd())
		if !m.hostsSpinnerActive {
			m.hostsSpinnerActive = true
			cmds = append(cmds, hostsSpinnerTickCmd())
		}
	}
	return tea.Batch(cmds...)
}

// pushLoadingHosts recomputes which registered hosts are currently
// being refreshed and pushes the set into projectlist. While
// remoteRefreshing is true, every host in hostList is treated as
// loading (consistent with the Hosts tab which also lights up every
// host on refresh start). Otherwise the set is empty so headers
// render plain. v0.22.
func (m *Model) pushLoadingHosts() {
	if !m.remoteRefreshing || len(m.hostList) == 0 {
		m.list.SetLoadingHosts(nil)
		return
	}
	loading := make(map[string]bool, len(m.hostList))
	for _, h := range m.hostList {
		loading[h.Name] = true
	}
	m.list.SetLoadingHosts(loading)
}

// refreshRemoteCmd returns a tea.Cmd that fans out `canopy ls --json`
// to every registered remote host, merges results into []state.GlobalRow
// (each tagged with its Host name), and emits remoteRowsLoadedMsg.
//
// Per D8: each host gets its own 3s deadline so one slow/offline host
// can't block the others. Per the Phase 1b backend commit: results are
// also persisted to ~/.canopy/remotes-cache.json so the next session
// has last-known rows even if some hosts are offline at startup.
//
// CRITICAL: ALL I/O happens inside the returned closure. The outer
// function does no work — Bubbletea requires this so tea.Cmd dispatch
// stays off the UI thread. Loading hosts.json, flock-and-migration,
// SSH fan-out, and remotes-cache.json save all run in the goroutine
// Bubbletea spawns for the closure.
func refreshRemoteCmd() tea.Cmd {
	return func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return remoteRowsLoadedMsg{}
		}
		canopyHome := filepath.Join(home, ".canopy")
		reg, err := host.NewRegistry(canopyHome)
		if err != nil {
			log.Warn("ui.refresh.remote.registry-failed", "err", err)
			return remoteRowsLoadedMsg{err: err}
		}
		hosts, err := reg.List()
		if err != nil {
			log.Warn("ui.refresh.remote.list-failed", "err", err)
			return remoteRowsLoadedMsg{err: err}
		}
		if len(hosts) == 0 {
			// No registered hosts; emit an empty msg so the UI
			// clears remoteRefreshing and the next refresh tick is
			// free to run. Without this, m.remoteRefreshing latches
			// true forever and subsequent refreshes silently skip.
			return remoteRowsLoadedMsg{}
		}
		cache, _ := state.NewRemotesCache(canopyHome)
		refresher := &host.Refresher{} // default 3s timeout per host

		ctx := context.Background()
		results := refresher.Tick(ctx, hosts)

		// Convert to GlobalRow + build the persistence snapshot in one pass.
		rows := make([]state.GlobalRow, 0)
		snaps := make(map[string]*state.RemoteHostSnapshot, len(results))
		for _, r := range results {
			snap := &state.RemoteHostSnapshot{
				CanopyVersion:      r.CanopyVersion,
				ClipboardBridge:    r.ClipboardBridge,
				LastSeen:           r.LastSeen,
				LastRefreshAttempt: time.Now(),
			}
			if r.Err != nil {
				snap.LastError = r.Err.Error()
				snaps[r.HostName] = snap
				continue
			}
			snap.Workspaces = make([]state.RemoteWorkspaceRow, 0, len(r.Workspaces))
			for _, w := range r.Workspaces {
				snap.Workspaces = append(snap.Workspaces, state.RemoteWorkspaceRow{
					Name: w.Name, Project: w.Project, Branch: w.Branch,
					Status: w.Status, Port: w.Port,
					TmuxSession:   w.TmuxSession,
					Alive:         w.Alive,
					MemRSS:        w.MemRSS,
					CPU:           w.CPU,
					Hints:         w.Hints,
					LastErrorHint: w.LastErrorHint,
					AgentState:    w.AgentState,
					Attached:      w.Attached,
					Owner:         w.Owner,
					SourceKind:    w.SourceKind,
				})
				rows = append(rows, state.GlobalRow{
					Host:    r.HostName,
					Project: w.Project,
					// IsMain mirrors BuildGlobalRows's synthetic main
					// row convention. Without this, the local renderer
					// can't tell remote main rows apart from workspace
					// rows — displayStatus falls through to string(r.Status)
					// which renders the literal "main" instead of
					// "running"/"not started", and fillMainBranches
					// skips them entirely. v0.17 Phase 1k follow-up.
					IsMain:        w.Name == "(main)",
					Name:          w.Name,
					Branch:        w.Branch,
					Status:        state.Status(w.Status),
					Port:          w.Port,
					TmuxSession:   w.TmuxSession,
					Alive:         w.Alive,
					MemRSS:        w.MemRSS,
					CPU:           w.CPU,
					Hints:         w.Hints,
					LastErrorHint: w.LastErrorHint,
					AgentState:    w.AgentState,
					Attached:      w.Attached,
					Owner:         w.Owner,
					SourceKind:    w.SourceKind,
					// LastSeen carries the host's most-recent successful
					// refresh timestamp onto every remote row from that
					// host. The TUI renderer compares it against time.Now
					// to dim stale rows + show a stale banner on the host
					// section header. Zero for local rows (their Host is
					// empty, so the renderer's "is remote and stale?"
					// check short-circuits). v0.19.
					LastSeen: r.LastSeen,
				})
			}
			snaps[r.HostName] = snap
		}

		// Best-effort persist to disk. Failure doesn't affect the UI
		// — the snapshots will retry on the next tick.
		if cache != nil {
			if err := cache.WithLock(func(stored map[string]*state.RemoteHostSnapshot) error {
				for name, snap := range snaps {
					stored[name] = snap
				}
				return nil
			}); err != nil {
				log.Warn("ui.refresh.remote.cache-save-failed", "err", err)
			}
		}

		// Self-heal the "remote has the project but laptop never
		// registered it" trap (most commonly: the v0.20 add-project
		// flow's CANOPY_INIT_RESULT_FILE round-trip failed because
		// the remote canopy was pre-v0.20). autoRegisterRemoteOrphans
		// returns the possibly-updated hosts slice.
		hosts = autoRegisterRemoteOrphans(reg, hosts, results)

		return remoteRowsLoadedMsg{rows: rows, snaps: snaps, hosts: hosts}
	}
}

// autoRegisterRemoteOrphans walks the per-host refresh `results` for
// (host, project) pairs where the remote emitted a project_root field
// but the laptop's hosts.json snapshot doesn't have the registration
// yet. Validates each path, writes registered orphans into `reg`, and
// returns the possibly-updated host list (reloaded from disk only if
// at least one orphan registered cleanly — keeps the no-op case zero
// I/O). Idempotent: pre-v0.21.2 remotes leave ProjectRoot empty so
// nothing is touched.
//
// Bounded by design:
//   - skips projects the laptop already has — won't churn hosts.json
//     in steady state
//   - validates project_root against the same path-safety contract as
//     the v0.20 add-project result-file channel, so a compromised or
//     buggy remote can't poison the registry
//   - dedups across multiple workspaces of the same project (a project
//     with N workspaces shows up N times in r.Workspaces but only
//     needs one registration)
func autoRegisterRemoteOrphans(reg *host.Registry, hosts []host.Host, results []host.Result) []host.Host {
	registered := make(map[string]map[string]struct{}, len(hosts))
	for _, h := range hosts {
		projs := make(map[string]struct{}, len(h.Projects))
		for name := range h.Projects {
			projs[name] = struct{}{}
		}
		registered[h.Name] = projs
	}
	type orphanKey struct{ host, project string }
	orphans := make(map[orphanKey]string)
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		for _, w := range r.Workspaces {
			if w.ProjectRoot == "" {
				continue
			}
			projs, ok := registered[r.HostName]
			if !ok {
				continue
			}
			if _, already := projs[w.Project]; already {
				continue
			}
			key := orphanKey{host: r.HostName, project: w.Project}
			if _, queued := orphans[key]; queued {
				continue
			}
			orphans[key] = w.ProjectRoot
		}
	}
	registeredAny := false
	for key, path := range orphans {
		if err := validateRemoteResultPath(path); err != nil {
			log.Warn("ui.refresh.remote.orphan-path-invalid",
				"host", key.host, "project", key.project, "path", path, "err", err)
			continue
		}
		if err := reg.AddProject(key.host, key.project, path); err != nil {
			if errors.Is(err, host.ErrProjectExists) {
				continue
			}
			log.Warn("ui.refresh.remote.orphan-register-failed",
				"host", key.host, "project", key.project, "path", path, "err", err)
			continue
		}
		registeredAny = true
		log.Info("ui.refresh.remote.orphan-registered",
			"host", key.host, "project", key.project, "path", path)
	}
	if registeredAny {
		if updated, err := reg.List(); err == nil {
			return updated
		}
	}
	return hosts
}

// tmuxLoadAdapter wraps *tmux.Client to satisfy state.LoadProbe. The
// adapter exists because state can't import tmux (would create the
// usual layered-package cycle), so the load-shape struct lives in
// each package separately and we translate at the boundary. Cheap;
// just one struct copy per probe.
type tmuxLoadAdapter struct{ c *tmux.Client }

func (a tmuxLoadAdapter) SessionLoad(ctx context.Context, session string) (state.LoadValue, error) {
	if a.c == nil {
		return state.LoadValue{}, nil
	}
	got, err := a.c.SessionLoad(ctx, session)
	if err != nil {
		return state.LoadValue{}, err
	}
	return state.LoadValue{RSS: got.RSS, CPU: got.CPU}, nil
}

// refreshCmdWithMem is the cache-aware variant of refreshCmd. When
// memCache is non-nil, populates row.MemRSS+CPU via
// BuildGlobalRowsWithLoad so the Mem column has data on first render.
// Default refreshCmd keeps memCache nil for callers that don't want
// the column (cmd/canopy/ls).
func refreshCmdWithMem(mgr *workspace.Manager, tc *tmux.Client, store *state.Store, memCache *state.MemCache) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		// Project-context lazy reconcile so stale ready→stopped before
		// rows render. Errors are non-fatal; rows still build from the
		// latest state we can load.
		if mgr != nil {
			if _, err := mgr.Reconcile(ctx); err != nil {
				log.Warn("ui.refresh.reconcile-failed", "err", err)
			}
		}

		st, err := store.Load()
		if err != nil {
			return rowsLoadedMsg{err: err}
		}
		var rows []state.GlobalRow
		if memCache != nil && tc != nil {
			rows = st.BuildGlobalRowsWithLoad(ctx, tc, tmuxLoadAdapter{c: tc}, memCache)
		} else {
			rows = st.BuildGlobalRows(ctx, tc)
		}
		fillMainBranches(ctx, rows)
		return rowsLoadedMsg{rows: rows}
	}
}

// fillMainBranches replaces the "—" placeholder in main rows with the
// project's actual default branch (origin/main or origin/master). Done
// once per project — DetectDefaultBranch is one git rev-parse call,
// cheap but worth caching across multiple worktrees of the same repo.
//
// Failure is non-fatal: if the project has no remote or uses a
// non-conventional default, the row keeps "main" as a fallback so
// the column doesn't render bare. Either way the user reads
// "this is the main session, branched off X" at a glance.
func fillMainBranches(ctx context.Context, rows []state.GlobalRow) {
	defaults := map[string]string{}
	for i := range rows {
		if !rows[i].IsMain || rows[i].ProjectRoot == "" {
			continue
		}
		root := rows[i].ProjectRoot
		branch, ok := defaults[root]
		if !ok {
			b, err := git.DetectDefaultBranch(ctx, root)
			if err != nil || b == "" {
				b = "main" // fallback when origin/main|master both miss
			}
			branch = b
			defaults[root] = branch
		}
		rows[i].Branch = branch
	}
}

// rowHintsMsg carries a single workspace's lifecycle detector result.
// Update merges it into projectlist via UpdateRowHints (keyed by
// project + name so a concurrent reconcile that reordered rows doesn't
// strand the hint update).
type rowHintsMsg struct {
	project string
	name    string
	hints   []state.Hint
}

// loadRowHintsCmds returns a tea.Batch of per-row hint-loading cmds.
// Each runs lifecycle.RunFast for one workspace in its own goroutine
// and emits a rowHintsMsg when done. tea.Batch dispatches them
// concurrently, so cold-start gh latency parallelizes across rows.
//
// Skips main rows and rows with empty Path. Each row carries its own
// ProjectRoot so cross-project hint loading scopes correctly.
func loadRowHintsCmds(rows []state.GlobalRow) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(rows))
	for _, r := range rows {
		if r.IsMain || r.Path == "" {
			continue
		}
		row := r // capture by value
		cmds = append(cmds, func() tea.Msg {
			ws := state.Workspace{
				Name:        row.Name,
				Branch:      row.Branch,
				Path:        row.Path,
				ProjectRoot: row.ProjectRoot,
				Status:      row.Status,
			}
			return rowHintsMsg{
				project: row.Project,
				name:    row.Name,
				hints:   lifecycle.RunFast(context.Background(), ws),
			}
		})
	}
	return tea.Batch(cmds...)
}
