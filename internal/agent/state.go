// State classification for the agent pane. The TUI polls each Ready
// workspace's agent pane on a tick, calls Detector.Observe with the
// captured content, and renders a badge from the returned State.
//
// The classification is heuristic: tmux capture-pane is the only signal
// available, and claude's TUI emits volatile chrome (spinners, elapsed
// timers, ANSI cursor escapes) that would flip a naive content hash on
// every poll. normalize() strips that chrome before hashing so "stable
// for N polls = idle" is reliable. Pattern matching for awaiting-input
// runs on the RAW content (codex review M2) so claude-only markers in
// the footer aren't normalized away before they can be matched.
//
// Two helpers (IsClaudeRendering, IsTrustDialog) feed the --prompt
// command's trust-dialog state machine; they live here because they
// share the pattern set with Observe.
package agent

import (
	"crypto/sha256"
	"regexp"
	"strings"
	"sync"
)

// State is what the agent pane is doing. Order matters for the badge
// renderer: higher numeric values get more visual prominence.
type State int

const (
	StateUnknown      State = iota // first observation, malformed role, or non-claude launcher at rest
	StateIdle                      // hash stable; either a claude idle marker matched or claude-launcher with no markers
	StateThinking                  // hash differs from prior observation
	StateAwaitingInput             // hash stable AND a claude awaiting-input pattern matched (y/N, tool permission, selector)
)

// String returns the lowercase tag used in logs + the badge renderer's
// switch. Mirrors state.Status's convention.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateThinking:
		return "thinking"
	case StateAwaitingInput:
		return "awaiting_input"
	default:
		return "unknown"
	}
}

// Detector classifies pane state across successive observations. One
// instance lives on the TUI Model; Observe is called per agent pane per
// poll tick. Per-pane history (last historyLen normalized-content
// hashes) lives in the struct.
//
// Concurrency: history is guarded by mu. Observe and Prune both lock.
// The activeIDs set passed to Prune must not be mutated by the caller
// during the call. tea.Tick fires single-threaded from Bubbletea today,
// so the mutex is precautionary — if polling ever moves to a worker
// pool the lock keeps Observe/Prune coherent.
type Detector struct {
	mu      sync.Mutex
	history map[string][]hashSnap
}

// historyLen is how many normalized-content hashes we keep per pane.
// At minimum we need 2 to detect motion (this tick vs last). 3 gives
// us "stable for two consecutive ticks" headroom for future tuning
// without growing the map.
const historyLen = 3

// hashSnap is a sha256 digest of the normalized pane content. Fixed
// size keeps the history slice cheap.
type hashSnap [32]byte

// NewDetector returns an empty detector ready for Observe calls.
func NewDetector() *Detector {
	return &Detector{history: make(map[string][]hashSnap)}
}

// Observe records the pane's current content and returns the classified
// state plus a 1-10 confidence score (debug only).
//
// launcher is the value from LauncherFromRole(roleTag) — derived once by
// the caller from the @canopy-role pane option, NOT re-parsed inside
// the detector (codex review H3 — pick one ownership site).
//
// Empty launcher means the role tag was malformed; classification stays
// StateUnknown. Empty current means we have no signal; same outcome.
func (d *Detector) Observe(paneID, launcher, current string) (State, int) {
	if current == "" {
		return StateUnknown, 10
	}

	normalized := normalize(current)
	h := sha256.Sum256([]byte(normalized))

	d.mu.Lock()
	snaps := append(d.history[paneID], h)
	if len(snaps) > historyLen {
		snaps = snaps[len(snaps)-historyLen:]
	}
	d.history[paneID] = snaps
	prev := hashSnap{}
	stable := false
	if len(snaps) >= 2 {
		prev = snaps[len(snaps)-2]
		stable = snaps[len(snaps)-1] == prev
	}
	d.mu.Unlock()

	// First observation: no motion signal yet.
	if len(snaps) < 2 {
		return StateUnknown, 3
	}

	// Activity: hash flipped this tick.
	if !stable {
		return StateThinking, 9
	}

	// Hash stable. Pattern matching uses RAW content (codex M2) so the
	// footer-living idle markers aren't normalized away.
	//
	// Per-launcher dispatch via Classifier (see classifier.go). The
	// behavioral contract: a launcher with REGISTERED markers (claude,
	// codex) gets full classification; a stub launcher with empty
	// marker slices (opencode, aider) AND any unrecognized launcher
	// fall through to StateUnknown — we have motion data but no UI
	// signal to interpret a stable pane.
	classifier := ClassifierFor(launcher)
	for _, p := range classifier.AwaitingMarkers() {
		if p.MatchString(current) {
			return StateAwaitingInput, 9
		}
	}
	for _, p := range classifier.IdleMarkers() {
		if p.MatchString(current) {
			return StateIdle, 8
		}
	}
	// Stable, no marker matched. If the launcher has REGISTERED markers
	// (real classifier, just none hit), call it Idle with low confidence
	// so dogfood logs surface it — same behavior as pre-refactor claude.
	// If markers are empty (stub or unknown), it's StateUnknown — we
	// don't fabricate an Idle signal for launchers we haven't dogfooded.
	if len(classifier.IdleMarkers()) > 0 || len(classifier.AwaitingMarkers()) > 0 {
		return StateIdle, 5
	}
	return StateUnknown, 4
}

// ClassifyTwoShot returns a state from two captures of the same pane
// taken a short interval apart. Used by `canopy ls --json` on a remote
// host to detect motion (Thinking) without persistent Detector state:
// the CLI captures the agent pane twice (e.g., 100ms apart), passes
// both contents in here, and we hash-diff normalize(prev) vs
// normalize(cur) to decide Thinking. v0.18 remote-status-observability.
//
// Compared with ClassifyOneShot, this can return Thinking. Compared
// with Detector.Observe, it's stateless — no per-pane history map to
// maintain across calls, which fits the one-shot CLI invocation model
// of canopy ls --json.
//
// Decision tree (matches Detector.Observe's rules so local and remote
// produce the same badge for the same pane content):
//
//  1. prev=="" OR cur=="" OR launcher=="" → Unknown
//  2. launcher with NO registered Classifier markers (opencode/aider
//     stubs, unknown launchers) → Unknown. Codex + claude are real.
//  3. normalize(prev) != normalize(cur) → Thinking (motion detected)
//  4. stable + awaiting pattern match on RAW cur → AwaitingInput
//  5. stable + idle marker match on RAW cur → Idle
//  6. stable + no marker → Unknown (we have motion data but no idle
//     signal — same fallback as Detector.Observe's confidence-4 path
//     for stub launchers)
//
// Pattern matching uses RAW cur, not normalize(cur), so footer-living
// markers aren't stripped before they can be matched (codex review M2).
func ClassifyTwoShot(launcher, prev, cur string) State {
	if prev == "" || cur == "" || launcher == "" {
		return StateUnknown
	}
	classifier := ClassifierFor(launcher)
	// Stub-launcher early-out: empty markers ↔ we haven't dogfooded this
	// launcher's UI. Even if motion is present, we don't promise more
	// than Unknown — the remote badge column should stay honest.
	if len(classifier.IdleMarkers()) == 0 && len(classifier.AwaitingMarkers()) == 0 {
		return StateUnknown
	}
	if normalize(prev) != normalize(cur) {
		return StateThinking
	}
	for _, p := range classifier.AwaitingMarkers() {
		if p.MatchString(cur) {
			return StateAwaitingInput
		}
	}
	for _, p := range classifier.IdleMarkers() {
		if p.MatchString(cur) {
			return StateIdle
		}
	}
	return StateUnknown
}

// ClassifyOneShot returns a state from a single pane capture using
// static pattern matching only — no motion / history needed. Used by
// `canopy ls --json` to stamp each workspace's agent_state per
// invocation; the laptop Refresher reads it into the remote row's
// AgentState. v0.17 Phase 1d.2.
//
// Tradeoff vs Detector.Observe: a single-shot can't see motion, so
// "Thinking" is never returned — agents that ARE working but match
// no idle/awaiting marker fall through to Unknown. That's acceptable
// for the remote-row badge: the load-bearing signal is "this is
// blocked on me" (AwaitingInput), which pattern matching handles
// well. Local rows keep the diff-based Observe path for the full
// Idle/Thinking/AwaitingInput trio.
//
// Superseded by ClassifyTwoShot on remote hosts as of v0.19 — kept
// as a fallback when the second capture fails (network blip, pane
// disappeared between captures).
//
// launcher mirrors LauncherFromRole(roleTag); empty means malformed
// role → Unknown. Empty content also → Unknown.
func ClassifyOneShot(launcher, content string) State {
	if content == "" || launcher == "" {
		return StateUnknown
	}
	classifier := ClassifierFor(launcher)
	// Stub-launcher early-out: empty markers ↔ undogfooded launcher.
	// Stays Unknown so the remote-row badge column doesn't lie.
	if len(classifier.IdleMarkers()) == 0 && len(classifier.AwaitingMarkers()) == 0 {
		return StateUnknown
	}
	for _, p := range classifier.AwaitingMarkers() {
		if p.MatchString(content) {
			return StateAwaitingInput
		}
	}
	for _, p := range classifier.IdleMarkers() {
		if p.MatchString(content) {
			return StateIdle
		}
	}
	return StateUnknown
}

// Prune drops history entries whose paneID is not in activeIDs. Called
// every TUI poll tick to bound memory as panes are killed/recreated;
// without it the map grows unboundedly across a long TUI session.
func (d *Detector) Prune(activeIDs map[string]struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for paneID := range d.history {
		if _, alive := activeIDs[paneID]; !alive {
			delete(d.history, paneID)
		}
	}
}

// HistoryLen reports how many panes the detector currently tracks. Used
// by tests to verify Prune.
func (d *Detector) HistoryLen() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.history)
}

// normalize strips volatile claude-TUI artifacts before hashing so the
// activity-stable check ("hash unchanged since last tick") doesn't flip
// every second on cosmetic-only changes.
//
// What gets stripped:
//   - ANSI CSI sequences (color codes, cursor moves)
//   - The spinner/elapsed-timer line (✻ Baked for Ns / Cooking for Ns)
//   - The mode-toggle footer (⏵⏵ auto mode on / · /effort)
//   - Lines starting with `❯` — claude's input-prompt line. Without
//     stripping it, every character the user types into the prompt
//     flips the hash and the Detector flags "thinking" when claude is
//     actually idle waiting on the user. The line is non-load-bearing
//     for the thinking/idle distinction since claude doesn't react
//     until Enter is pressed.
//   - Trailing whitespace per line + trailing blank lines
//
// Pattern matching MUST use raw content, not normalized — the footer
// strip + input-line strip remove markers that pattern matching needs
// to see (codex review M2).
func normalize(content string) string {
	s := ansiCSI.ReplaceAllString(content, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, line := range strings.Split(s, "\n") {
		if spinnerLine.MatchString(line) || footerLine.MatchString(line) || inputLine.MatchString(line) {
			continue
		}
		b.WriteString(strings.TrimRight(line, " \t"))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// ANSI CSI escape sequence (color, cursor moves, etc.). Covers the
// common shape `\x1b[<digits;digits>X` where X is a final byte in
// 0x40–0x7E. Sufficient for claude's TUI; full ANSI parsing would be
// overkill.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// spinnerLine matches claude's elapsed-timer line. The verb rotates
// (Baked, Cooking, Simmering, Brewing, Churned) — match all known
// shapes. The number changes every second so this line WILL flip the
// hash if not stripped.
//
// Also matches codex's spinner: `• Working (Ns • esc to interrupt)`.
// Codex's spinner is a single fixed verb ("Working") wrapped in a
// parenthesized timer, structurally different from claude's "Baked
// for Ns", so it gets its own alternation arm rather than trying to
// jam it into the (verb) for Ns grammar. Last verified for codex:
// 2026-06-17 against codex-cli 0.140.0.
var spinnerLine = regexp.MustCompile(
	`(?i)(churned|baked|cooking|simmering|brewing|thinking|musing|pondering) for \d+s` +
		`|` +
		`• Working \(\d+s • esc to interrupt\)`,
)

// footerLine matches claude's mode-toggle footer that toggles when the
// user hits shift+tab. We strip it from normalized content (avoids
// activity flips when toggling) but the raw content keeps it for
// pattern matching against claudeIdleMarkers.
var footerLine = regexp.MustCompile(`⏵⏵ auto mode on|· /effort`)

// inputLine matches the input-prompt line of any agent TUI we
// classify. Anything after the chevron is what the user is typing —
// character-by-character changes would otherwise flip the hash and
// mis-classify "user typing" as "agent thinking." Stripping handles
// the load-bearing UX bug where the badge said ⚡ Thinking while you
// were actively composing.
//
// The character class covers both chevrons we currently see:
//
//   - ❯ (U+276F HEAVY RIGHT-POINTING ANGLE QUOTATION MARK ORNAMENT) — claude
//   - › (U+203A SINGLE RIGHT-POINTING ANGLE QUOTATION MARK)        — codex
//
// 2026-06-26 (ultrareview bug_006): the original regex was hardcoded
// to claude's ❯, so typing into idle codex flipped the normalized
// hash on every keystroke and Detector.Observe returned StateThinking
// at confidence 9 from the motion check, before any marker matching.
// Adding codex's › fixes the badge regression.
var inputLine = regexp.MustCompile(`(?m)^\s*[❯›]`)

// claudeAwaitingPatterns are claude-TUI markers that mean a user
// action is required RIGHT NOW. Empty-input cursor is intentionally
// NOT here — that's idle, not awaiting (codex review H4 / state taxonomy).
var claudeAwaitingPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\(y/N\)|\[y/N\]`),               // tool permission y/N
	regexp.MustCompile(`Approve this command\?|Allow tool use`),
	regexp.MustCompile(`Enter to confirm.*Esc to cancel`), // selector dialogs (incl. trust dialog)
	regexp.MustCompile(`(?m)^\s*❯\s+\d+\.`),               // numbered-option selector
}

// claudeIdleMarkers are claude-only UI elements that prove the pane is
// rendering claude (not the keepAlive shell). At least one must match
// for IsClaudeRendering to return true — used by --prompt's Phase 3
// to abort send-keys if claude crashed and the pane fell back to shell.
//
// Bare `❯` is INTENTIONALLY excluded (codex review v3-B1): starship,
// oh-my-posh and other shell prompts render exactly that, so a shell
// could mimic it and pass the check. Every marker here is
// claude-specific UI chrome.
var claudeIdleMarkers = []*regexp.Regexp{
	regexp.MustCompile(`❯ Try "`),                 // rotating placeholder hint
	regexp.MustCompile(`⏵⏵ auto mode on`),          // mode-toggle footer (claude-only glyph)
	regexp.MustCompile(`shift\+tab to cycle`),      // mode-toggle hint
	regexp.MustCompile(`Tips for getting started`), // welcome banner
	regexp.MustCompile(`Welcome back`),             // welcome banner (returning user)
	regexp.MustCompile(`Claude Code v\d`),          // version line in welcome chrome
}

// IsClaudeRendering reports whether the captured pane content shows at
// least one claude-only marker. Used by --prompt's Phase 3 to verify
// the pane is actually running claude before send-keys fires (defends
// against keepAlive-shell command injection — codex review B4).
//
// Looks at the BOTTOM 12 lines of the capture (where claude's live
// input box + footer live). Matching anywhere on screen is too loose:
// a crashed claude can leave its welcome banner in scrollback while
// the actual cursor is at a shell prompt below. Bottom-only matching
// guarantees the live UI is claude, not stale chrome.
func IsClaudeRendering(content string) bool {
	tail := bottomLines(content, 12)
	for _, p := range claudeIdleMarkers {
		if p.MatchString(tail) {
			return true
		}
	}
	return false
}

// bottomLines returns the last n lines of content (or all of it if
// fewer than n). Used to scope claude-marker checks to the live pane
// area instead of the whole scrollback.
func bottomLines(content string, n int) string {
	lines := strings.Split(content, "\n")
	if len(lines) <= n {
		return content
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// trustDialogPattern matches claude's first-launch trust-folder
// confirmation. --prompt's Phase 1 polls capture-pane until this
// pattern (or a claude idle marker) appears, then sends Enter to
// dismiss it before sending the user's prompt.
var trustDialogPattern = regexp.MustCompile(`Yes, I trust this folder|Enter to confirm.*Esc to cancel`)

// IsTrustDialog reports whether the captured pane content shows
// claude's trust-folder confirmation dialog.
func IsTrustDialog(content string) bool {
	return trustDialogPattern.MatchString(content)
}
