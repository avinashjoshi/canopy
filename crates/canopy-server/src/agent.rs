//! Agent state: what the agent pane is doing right now.
//!
//! Two sources, arbitrated per workspace:
//! 1. **Reports** from hooks (`pane.report_agent`), authoritative while fresh. Each source
//!    has a monotonic `seq`; stale reports are dropped.
//! 2. **Screen rules** over the bottom lines of the agent pane, the fallback when no hook
//!    has reported recently. Ported from v0's claude heuristics: a changing screen means
//!    working; a stable screen is pattern-matched for blockers and idle markers; the
//!    input line is stripped before hashing so typing never reads as "working".

use canopy_proto::AgentState;
use regex::Regex;
use std::collections::HashMap;
use std::sync::LazyLock;
use std::time::{Duration, Instant};

/// How long a hook report is trusted before screen rules take over again.
pub const REPORT_TTL: Duration = Duration::from_secs(90);

#[derive(Debug, Clone)]
struct Report {
    source: String,
    seq: u64,
    state: AgentState,
    at: Instant,
    session_id: Option<String>,
}

#[derive(Debug, Default, Clone)]
struct ScreenMemory {
    hashes: Vec<u64>,
}

#[derive(Debug, Default)]
pub struct Tracker {
    reports: HashMap<String, Report>,
    screens: HashMap<String, ScreenMemory>,
    current: HashMap<String, (AgentState, Instant)>,
}

impl Tracker {
    pub fn new() -> Self {
        Self::default()
    }

    /// Apply a hook report. Returns the new state if it changed.
    pub fn report(&mut self, workspace_id: &str, source: &str, seq: u64, state: AgentState, session_id: Option<String>) -> Option<AgentState> {
        if let Some(prev) = self.reports.get(workspace_id) {
            if prev.source == source && seq <= prev.seq {
                return None;
            }
        }
        self.reports.insert(
            workspace_id.to_string(),
            Report { source: source.to_string(), seq, state, at: Instant::now(), session_id },
        );
        self.set(workspace_id, state)
    }

    /// Session id the agent last reported (for `--resume <id>`).
    pub fn session_id(&self, workspace_id: &str) -> Option<&str> {
        self.reports.get(workspace_id).and_then(|r| r.session_id.as_deref())
    }

    /// Feed a screen capture; ignored while a fresh hook report exists.
    pub fn observe_screen(&mut self, workspace_id: &str, launcher: &str, screen: &str) -> Option<AgentState> {
        if let Some(r) = self.reports.get(workspace_id) {
            if r.at.elapsed() < REPORT_TTL && r.state != AgentState::Unknown {
                return None;
            }
        }
        let mem = self.screens.entry(workspace_id.to_string()).or_default();
        let h = hash(&normalize(screen));
        let changed = mem.hashes.last().is_some_and(|last| *last != h);
        mem.hashes.push(h);
        if mem.hashes.len() > 3 {
            mem.hashes.remove(0);
        }
        let state = if changed {
            AgentState::Working
        } else {
            classify_stable(launcher, screen)
        };
        self.set(workspace_id, state)
    }

    pub fn state(&self, workspace_id: &str) -> AgentState {
        self.current.get(workspace_id).map(|(s, _)| *s).unwrap_or(AgentState::Unknown)
    }

    pub fn since(&self, workspace_id: &str) -> Duration {
        self.current.get(workspace_id).map(|(_, t)| t.elapsed()).unwrap_or_default()
    }

    pub fn forget(&mut self, workspace_id: &str) {
        self.reports.remove(workspace_id);
        self.screens.remove(workspace_id);
        self.current.remove(workspace_id);
    }

    /// Drop memory for workspaces no longer present.
    pub fn retain(&mut self, alive: &dyn Fn(&str) -> bool) {
        self.reports.retain(|k, _| alive(k));
        self.screens.retain(|k, _| alive(k));
        self.current.retain(|k, _| alive(k));
    }

    fn set(&mut self, id: &str, state: AgentState) -> Option<AgentState> {
        match self.current.get(id) {
            Some((s, _)) if *s == state => None,
            _ => {
                self.current.insert(id.to_string(), (state, Instant::now()));
                Some(state)
            }
        }
    }
}

static ANSI: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\x1b\[[0-9;?]*[A-Za-z]").unwrap());
static SPINNER: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)(churned|baked|cooking|simmering|brewing|thinking|musing|pondering|working|crafting|forging|percolating)[^\n]*\d+s").unwrap()
});
static BLOCKED: LazyLock<Vec<Regex>> = LazyLock::new(|| {
    [
        r"\(y/N\)|\[y/N\]|\[Y/n\]",
        r"Approve this command\?|Allow tool use|Do you want to proceed",
        r"Enter to confirm.*Esc to cancel",
        r"(?m)^\s*❯\s+\d+\.",
        r"Yes, I trust this folder",
    ]
    .iter()
    .map(|p| Regex::new(p).unwrap())
    .collect()
});
static CLAUDE_IDLE: LazyLock<Vec<Regex>> = LazyLock::new(|| {
    [r#"❯ Try ""#, r"⏵⏵ auto mode on", r"shift\+tab to cycle", r"Tips for getting started", r"Welcome back", r"Claude Code v\d", r"\? for shortcuts"]
        .iter()
        .map(|p| Regex::new(p).unwrap())
        .collect()
});

/// Strip volatile chrome so a stable agent screen hashes the same every tick.
pub fn normalize(screen: &str) -> String {
    let no_ansi = ANSI.replace_all(screen, "");
    let no_spin = SPINNER.replace_all(&no_ansi, "");
    let mut lines: Vec<&str> = no_spin
        .lines()
        .filter(|l| !l.trim_start().starts_with('❯'))
        .filter(|l| !l.contains("auto mode on") && !l.contains("/effort"))
        .map(|l| l.trim_end())
        .collect();
    while lines.last().is_some_and(|l| l.is_empty()) {
        lines.pop();
    }
    lines.join("\n")
}

fn hash(s: &str) -> u64 {
    use std::hash::{Hash, Hasher};
    let mut h = std::collections::hash_map::DefaultHasher::new();
    s.hash(&mut h);
    h.finish()
}

/// Bottom-N lines of a screen (the live region; scrollback banners must not count).
fn bottom(screen: &str, n: usize) -> String {
    let lines: Vec<&str> = screen.lines().collect();
    let start = lines.len().saturating_sub(n);
    lines[start..].join("\n")
}

/// Classify a screen that has not changed since the last tick.
pub fn classify_stable(launcher: &str, screen: &str) -> AgentState {
    let live = bottom(&ANSI.replace_all(screen, ""), 12);
    if BLOCKED.iter().any(|r| r.is_match(&live)) {
        return AgentState::Blocked;
    }
    if launcher == "claude" {
        if CLAUDE_IDLE.iter().any(|r| r.is_match(&live)) {
            return AgentState::Idle;
        }
        // Claude is rendering (has an input line) but shows none of the idle markers: the
        // bare prompt alone is deliberately not enough (shell prompts look the same).
        return AgentState::Unknown;
    }
    AgentState::Unknown
}

/// Is the pane showing the agent at all (vs. the keep-alive shell after a crash)?
/// Checked on the bottom lines only, so a stale banner in scrollback cannot fool it.
pub fn is_agent_rendering(launcher: &str, screen: &str) -> bool {
    let live = bottom(&ANSI.replace_all(screen, ""), 12);
    match launcher {
        "claude" => CLAUDE_IDLE.iter().any(|r| r.is_match(&live)) || BLOCKED.iter().any(|r| r.is_match(&live)) || live.contains('❯'),
        _ => !live.trim().is_empty(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const IDLE: &str = "╭──╮\n│ Claude Code v2.1 │\n╰──╯\n\n❯ Try \"fix the bug\"\n\n  ⏵⏵ auto mode on · shift+tab to cycle\n";

    #[test]
    fn typing_does_not_flip_to_working() {
        let mut t = Tracker::new();
        assert_eq!(t.observe_screen("w1", "claude", IDLE), Some(AgentState::Idle));
        let typed = IDLE.replace("❯ Try \"fix the bug\"", "❯ please refactor auth");
        assert_eq!(t.observe_screen("w1", "claude", &typed), None);
        assert_eq!(t.state("w1"), AgentState::Idle);
    }

    #[test]
    fn changing_screen_is_working_then_blocked() {
        let mut t = Tracker::new();
        t.observe_screen("w1", "claude", "line a\n");
        assert_eq!(t.observe_screen("w1", "claude", "line a\nline b\n"), Some(AgentState::Working));
        let prompt = "Bash(rm -rf build)\nDo you want to proceed? (y/N)\n";
        t.observe_screen("w1", "claude", prompt);
        assert_eq!(t.observe_screen("w1", "claude", prompt), Some(AgentState::Blocked));
    }

    #[test]
    fn spinner_elapsed_is_ignored() {
        let a = normalize("✻ Thinking for 3s\nfoo");
        let b = normalize("✻ Thinking for 4s\nfoo");
        assert_eq!(a, b);
    }

    #[test]
    fn hook_reports_win_and_stale_seq_dropped() {
        let mut t = Tracker::new();
        assert_eq!(t.report("w1", "canopy:claude", 5, AgentState::Working, Some("sess-1".into())), Some(AgentState::Working));
        assert_eq!(t.report("w1", "canopy:claude", 4, AgentState::Idle, None), None);
        assert_eq!(t.observe_screen("w1", "claude", IDLE), None);
        assert_eq!(t.state("w1"), AgentState::Working);
        assert_eq!(t.session_id("w1"), Some("sess-1"));
        assert_eq!(t.report("w1", "canopy:claude", 6, AgentState::Idle, None), Some(AgentState::Idle));
    }

    #[test]
    fn crashed_agent_banner_in_scrollback_not_rendering() {
        let mut s = String::from("Claude Code v2.1\nWelcome back\n");
        for i in 0..20 {
            s.push_str(&format!("$ echo {i}\n{i}\n"));
        }
        s.push_str("$ ");
        assert!(!is_agent_rendering("claude", &s));
        assert!(is_agent_rendering("claude", IDLE));
    }

    #[test]
    fn non_claude_stable_is_unknown_unless_blocked() {
        assert_eq!(classify_stable("codex", "some output\n> "), AgentState::Unknown);
        assert_eq!(classify_stable("codex", "Allow tool use? [y/N]"), AgentState::Blocked);
    }
}
