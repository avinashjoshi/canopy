//! Live view of a workspace's setup log, shared by the dashboard, the create popup,
//! `canopy new` / `canopy retry` and `canopy log -f`.
//!
//! The server owns the file and streams script output into it as it happens; clients
//! tail it over the API (`workspace.log` with a byte offset) so the same view works for a
//! remote host. Nothing here touches the filesystem.

use crate::tui::{frame, hints, PANEL_BG};
use crate::Transport;
use anyhow::{bail, Result};
use canopy_core::state::Status;
use canopy_proto::{Method, ResultBody, WorkspaceCreate, WorkspaceRef, WorkspaceRow};
use ratatui::layout::Rect;
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span};
use ratatui::widgets::Paragraph;
use ratatui::Frame;
use std::collections::BTreeSet;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::mpsc;
use std::time::{Duration, Instant};

pub const SPIN: &[&str] = &["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
const POLL_EVERY: Duration = Duration::from_millis(400);
const MAX_LINES: usize = 4000;

/// One `workspace.log` answer.
#[derive(Debug, Clone)]
pub struct LogChunk {
    pub text: String,
    pub offset: u64,
    pub running: bool,
    pub status: Status,
    pub path: PathBuf,
}

pub fn fetch(t: &Transport, r: &WorkspaceRef, offset: Option<u64>, lines: Option<usize>) -> Result<LogChunk, String> {
    match t.call(Method::WorkspaceLog { workspace: r.clone(), offset, lines }) {
        Ok(ResultBody::Log { path, text, offset, running, status }) => Ok(LogChunk { text, offset, running, status, path }),
        Ok(_) => Err("unexpected response".into()),
        Err(e) => Err(e.to_string()),
    }
}

/// The workspace a just-submitted create produced: a `setting-up` row in `root` that is
/// either named `name` or was not in `known` before the request went out.
pub fn find_new(t: &Transport, root: &Path, known: &BTreeSet<String>, name: Option<&str>) -> Result<Option<WorkspaceRow>, String> {
    match t.call(Method::WorkspaceList { project_root: Some(root.to_path_buf()) }) {
        Ok(ResultBody::WorkspaceList { workspaces }) => Ok(workspaces.into_iter().find(|w| match name {
            Some(n) => w.name == n,
            None => w.status == Status::SettingUp && !known.contains(&w.id),
        })),
        Ok(_) => Err("unexpected response".into()),
        Err(e) => Err(e.to_string()),
    }
}

/// Tail state for one workspace.
#[derive(Debug, Clone)]
pub struct LiveLog {
    pub target: WorkspaceRef,
    pub name: String,
    pub lines: Vec<String>,
    partial: String,
    pub offset: Option<u64>,
    pub running: bool,
    pub status: Status,
    pub path: Option<PathBuf>,
    pub started: Instant,
    last_poll: Instant,
    pub pending: bool,
    /// `None` = follow the end; `Some(n)` = pinned to line `n` at the top.
    pub scroll: Option<usize>,
    pub error: Option<String>,
    /// Only show this run: drop everything before the last `══` header on first fill.
    trim_to_run: bool,
    /// How many polls have been applied (the first one is the big backfill).
    polls: usize,
}

impl LiveLog {
    pub fn new(target: WorkspaceRef, name: impl Into<String>) -> Self {
        Self {
            target,
            name: name.into(),
            lines: Vec::new(),
            partial: String::new(),
            offset: None,
            running: true,
            status: Status::SettingUp,
            path: None,
            started: Instant::now(),
            last_poll: Instant::now() - POLL_EVERY,
            pending: false,
            scroll: None,
            error: None,
            trim_to_run: true,
            polls: 0,
        }
    }

    /// Show the whole tail instead of just the latest run (for `L` on an old workspace).
    pub fn whole_tail(mut self) -> Self {
        self.trim_to_run = false;
        self
    }

    /// True when the next `fetch` should go out; marks it in flight.
    pub fn take_due(&mut self) -> Option<(WorkspaceRef, Option<u64>)> {
        if self.pending || self.last_poll.elapsed() < POLL_EVERY {
            return None;
        }
        // Keep polling a little after `running` flips so the final marker lines land.
        if !self.running && self.polls > 0 && self.last_poll.elapsed() < Duration::from_secs(2) {
            return None;
        }
        self.pending = true;
        Some((self.target.clone(), self.offset))
    }

    pub fn apply(&mut self, res: Result<LogChunk, String>) {
        self.pending = false;
        self.last_poll = Instant::now();
        match res {
            Ok(c) => {
                self.error = None;
                self.push_text(&c.text);
                if self.polls == 0 && self.trim_to_run {
                    if let Some(i) = self.lines.iter().rposition(|l| l.starts_with("══")) {
                        self.lines.drain(..i);
                    }
                }
                self.offset = Some(c.offset);
                self.running = c.running;
                self.status = c.status;
                self.path = Some(c.path);
                self.polls += 1;
            }
            Err(e) => self.error = Some(e),
        }
    }

    fn push_text(&mut self, text: &str) {
        self.partial.push_str(text);
        while let Some(i) = self.partial.find('\n') {
            let line = self.partial[..i].trim_end_matches('\r').to_string();
            self.partial = self.partial[i + 1..].to_string();
            self.lines.push(line);
        }
        if self.lines.len() > MAX_LINES {
            let drop = self.lines.len() - MAX_LINES;
            self.lines.drain(..drop);
            if let Some(s) = self.scroll.as_mut() {
                *s = s.saturating_sub(drop);
            }
        }
    }

    /// The last line worth showing as a one-line summary.
    pub fn last_line(&self) -> Option<&str> {
        if !self.partial.trim().is_empty() {
            return Some(self.partial.trim());
        }
        self.lines.iter().rev().map(|l| l.trim()).find(|l| !l.is_empty())
    }

    pub fn elapsed_label(&self) -> String {
        let s = self.started.elapsed().as_secs();
        if s >= 60 {
            format!("{}m{:02}s", s / 60, s % 60)
        } else {
            format!("{s}s")
        }
    }

    pub fn finished_ok(&self) -> bool {
        !self.running && matches!(self.status, Status::Ready | Status::Stopped)
    }

    pub fn finished_broken(&self) -> bool {
        !self.running && matches!(self.status, Status::Broken | Status::Orphaned)
    }

    pub fn scroll_by(&mut self, delta: i32, view_height: usize) {
        let max_top = self.lines.len().saturating_sub(view_height);
        let cur = self.scroll.unwrap_or(max_top);
        let next = if delta < 0 { cur.saturating_sub(delta.unsigned_abs() as usize) } else { (cur + delta as usize).min(max_top) };
        self.scroll = if next >= max_top { None } else { Some(next) };
    }

    pub fn scroll_home(&mut self) {
        self.scroll = Some(0);
    }

    pub fn scroll_end(&mut self) {
        self.scroll = None;
    }

    /// Title for the panel: state glyph, name, elapsed time.
    pub fn title(&self, spinner: usize) -> String {
        if self.running {
            format!("{} {} · setting up · {}", SPIN[spinner % SPIN.len()], self.name, self.elapsed_label())
        } else if self.finished_broken() {
            format!("✗ {} · setup failed", self.name)
        } else if self.finished_ok() {
            format!("✓ {} · {}", self.name, self.status.as_str())
        } else {
            format!("{} · log", self.name)
        }
    }

    /// Draw the log inside `rect` with the standard panel frame.
    pub fn render(&self, f: &mut Frame, rect: Rect, spinner: usize, extra_hints: &[(&str, &str)]) {
        let inner = frame(f, rect, &self.title(spinner));
        if inner.height < 3 {
            return;
        }
        let body_h = inner.height.saturating_sub(2) as usize;
        let width = inner.width as usize;
        let total = self.lines.len() + usize::from(!self.partial.is_empty());
        let max_top = total.saturating_sub(body_h);
        let top = self.scroll.map(|s| s.min(max_top)).unwrap_or(max_top);

        let dim = Style::new().fg(Color::Rgb(150, 158, 170));
        let marker = Style::new().fg(crate::brand::ACCENT);
        let fail = Style::new().fg(Color::Red).add_modifier(Modifier::BOLD);
        let mut lines: Vec<Line<'static>> = Vec::with_capacity(body_h + 2);
        let all: Vec<&str> = self.lines.iter().map(|s| s.as_str()).chain((!self.partial.is_empty()).then_some(self.partial.as_str())).collect();
        for (i, l) in all.iter().enumerate().skip(top).take(body_h) {
            let last = i + 1 == all.len();
            let style = if l.starts_with("══") || l.starts_with("── ") {
                if l.contains("FAILED") {
                    fail
                } else {
                    marker
                }
            } else if last {
                Style::new().fg(Color::White)
            } else {
                dim
            };
            lines.push(Line::from(Span::styled(fit(&display_line(l), width), style)));
        }
        if all.is_empty() {
            let msg = if self.running { "waiting for output…" } else { "no log for this workspace yet" };
            lines.push(Line::from(Span::styled(msg, dim)));
        }
        while lines.len() < body_h {
            lines.push(Line::from(""));
        }
        if let Some(e) = &self.error {
            lines.push(Line::from(Span::styled(fit(&format!("log unavailable: {e}"), width), Style::new().fg(Color::Yellow))));
        } else if let Some(p) = &self.path {
            let at = if self.scroll.is_some() { "  (scrolled; G to follow)" } else { "" };
            lines.push(Line::from(Span::styled(fit(&format!("{}{at}", p.display()), width), Style::new().fg(Color::Rgb(92, 99, 112)))));
        } else {
            lines.push(Line::from(""));
        }
        let mut pairs: Vec<(&str, &str)> = Vec::new();
        if self.finished_ok() {
            pairs.push(("⏎", "open"));
        }
        pairs.extend_from_slice(extra_hints);
        pairs.extend_from_slice(&[("↑↓", "scroll"), ("esc", "back")]);
        lines.push(hints(&pairs));
        f.render_widget(Paragraph::new(lines).style(Style::new().bg(PANEL_BG)), inner);
    }
}

/// Marker lines carry a timestamp for the file; on screen the words are enough.
fn display_line(l: &str) -> String {
    for prefix in ["══ ", "── ", "=== "] {
        if let Some(rest) = l.strip_prefix(prefix) {
            if let Some((ts, msg)) = rest.split_once(' ') {
                if ts.len() == 20 && ts.ends_with('Z') && ts.as_bytes()[4] == b'-' {
                    return format!("{prefix}{}", msg.trim_end_matches(" ==="));
                }
            }
        }
    }
    l.to_string()
}

fn fit(s: &str, n: usize) -> String {
    if n == 0 {
        return String::new();
    }
    let mut out = String::new();
    let mut w = 0usize;
    for ch in s.chars() {
        let cw = unicode_width(ch);
        if w + cw > n {
            if n >= 1 {
                out.pop();
                out.push('…');
            }
            return out;
        }
        out.push(ch);
        w += cw;
    }
    out
}

fn unicode_width(ch: char) -> usize {
    // Enough for log text: wide CJK/emoji as 2, everything else as 1.
    let c = ch as u32;
    if (0x1100..=0x115F).contains(&c) || (0x2E80..=0xA4CF).contains(&c) || (0xAC00..=0xD7A3).contains(&c) || (0xF900..=0xFAFF).contains(&c) || (0xFE30..=0xFE4F).contains(&c) || (0xFF00..=0xFF60).contains(&c) || (0xFFE0..=0xFFE6).contains(&c) || (0x1F300..=0x1FAFF).contains(&c) {
        2
    } else {
        1
    }
}

/// A create in flight: until the server has registered the row we only know the project;
/// once found, it becomes a normal `LiveLog`.
#[derive(Debug, Clone)]
pub struct Creating {
    pub root: PathBuf,
    pub name: Option<String>,
    pub known: BTreeSet<String>,
    pub log: Option<LiveLog>,
    pub started: Instant,
    last_find: Instant,
    pub find_pending: bool,
}

/// What the owner should ask the server next.
pub enum Poll {
    Nothing,
    Find { root: PathBuf, known: BTreeSet<String>, name: Option<String> },
    Fetch { target: WorkspaceRef, offset: Option<u64> },
}

impl Creating {
    pub fn new(root: PathBuf, name: Option<String>, known: BTreeSet<String>) -> Self {
        Self { root, name, known, log: None, started: Instant::now(), last_find: Instant::now() - POLL_EVERY, find_pending: false }
    }

    pub fn next_poll(&mut self) -> Poll {
        match self.log.as_mut() {
            Some(log) => match log.take_due() {
                Some((target, offset)) => Poll::Fetch { target, offset },
                None => Poll::Nothing,
            },
            None => {
                if self.find_pending || self.last_find.elapsed() < Duration::from_millis(250) {
                    return Poll::Nothing;
                }
                self.find_pending = true;
                Poll::Find { root: self.root.clone(), known: self.known.clone(), name: self.name.clone() }
            }
        }
    }

    pub fn on_found(&mut self, res: Result<Option<WorkspaceRow>, String>) {
        self.find_pending = false;
        self.last_find = Instant::now();
        if let Ok(Some(w)) = res {
            let mut log = LiveLog::new(WorkspaceRef::Id { id: w.id }, w.name);
            log.started = self.started;
            self.log = Some(log);
        }
    }

    pub fn on_log(&mut self, res: Result<LogChunk, String>) {
        if let Some(log) = self.log.as_mut() {
            log.apply(res);
        }
    }

    pub fn render(&self, f: &mut Frame, rect: Rect, spinner: usize, extra_hints: &[(&str, &str)]) {
        match &self.log {
            Some(log) => log.render(f, rect, spinner, extra_hints),
            None => {
                let inner = frame(f, rect, &format!("{} creating workspace…", SPIN[spinner % SPIN.len()]));
                let dim = Style::new().fg(Color::Rgb(150, 158, 170));
                let _ = extra_hints;
                let lines = vec![Line::from(""), Line::from(Span::styled("registering the workspace and allocating a port…", dim)), Line::from(""), hints(&[("esc", "hide; creation continues")])];
                f.render_widget(Paragraph::new(lines).style(Style::new().bg(PANEL_BG)), inner);
            }
        }
    }
}

// ---------------------------------------------------------------------------------------
// Plain-text streaming for the CLI
// ---------------------------------------------------------------------------------------

/// Run a long server call while printing the workspace log to `out` as it grows.
/// `target` is the row to follow: known up front (retry) or discovered (create).
pub fn run_streaming<F>(t: &Transport, mut creating: Creating, op: F, out: &mut dyn Write) -> Result<ResultBody>
where
    F: FnOnce(&Transport) -> Result<ResultBody> + Send + 'static,
{
    let (tx, rx) = mpsc::channel::<Result<ResultBody>>();
    let t2 = t.clone();
    std::thread::spawn(move || {
        let _ = tx.send(op(&t2));
    });
    let mut printed = 0usize;
    let mut result: Option<Result<ResultBody>> = None;
    let mut final_polls = 0u8;
    loop {
        if result.is_none() {
            if let Ok(r) = rx.try_recv() {
                result = Some(r);
            }
        }
        match creating.next_poll() {
            Poll::Find { root, known, name } => creating.on_found(find_new(t, &root, &known, name.as_deref())),
            Poll::Fetch { target, offset } => creating.on_log(fetch(t, &target, offset, Some(200))),
            Poll::Nothing => {}
        }
        if let Some(log) = &creating.log {
            for l in &log.lines[printed..] {
                let _ = writeln!(out, "  {l}");
            }
            printed = log.lines.len();
        }
        if result.is_some() {
            // One or two more reads so the closing marker lines are not cut off.
            final_polls += 1;
            if final_polls > 3 || creating.log.is_none() {
                break;
            }
            if let Some(log) = creating.log.as_mut() {
                log.pending = false;
                log.running = true; // force take_due through its cool-down
                log.scroll = None;
                if let Some((target, offset)) = log.take_due() {
                    log.apply(fetch(t, &target, offset, Some(0)));
                }
            }
            std::thread::sleep(Duration::from_millis(150));
            continue;
        }
        std::thread::sleep(Duration::from_millis(150));
    }
    if let Some(log) = &creating.log {
        for l in &log.lines[printed..] {
            let _ = writeln!(out, "  {l}");
        }
    }
    let _ = out.flush();
    result.unwrap_or_else(|| Err(anyhow::anyhow!("operation ended without a result")))
}

/// `canopy new` with live output.
pub fn create_streaming(t: &Transport, req: WorkspaceCreate, out: &mut dyn Write) -> Result<WorkspaceRow> {
    let known: BTreeSet<String> = match t.call(Method::WorkspaceList { project_root: Some(req.project_root.clone()) }) {
        Ok(ResultBody::WorkspaceList { workspaces }) => workspaces.into_iter().map(|w| w.id).collect(),
        _ => BTreeSet::new(),
    };
    let creating = Creating::new(req.project_root.clone(), req.name.clone(), known);
    match run_streaming(t, creating, move |t| t.call(Method::WorkspaceCreate(req)), out)? {
        ResultBody::Workspace { workspace } => Ok(workspace),
        other => bail!("unexpected response {other:?}"),
    }
}

/// `canopy retry` with live output.
pub fn retry_streaming(t: &Transport, r: WorkspaceRef, force: bool, out: &mut dyn Write) -> Result<WorkspaceRow> {
    // Resolve the row first so the follower has a stable id.
    let row = match t.call(Method::WorkspaceGet { workspace: r.clone() }) {
        Ok(ResultBody::Workspace { workspace }) => workspace,
        Ok(other) => bail!("unexpected response {other:?}"),
        Err(e) => return Err(e),
    };
    let mut creating = Creating::new(row.project_root.clone(), Some(row.name.clone()), BTreeSet::new());
    creating.log = Some(LiveLog::new(WorkspaceRef::Id { id: row.id.clone() }, row.name.clone()));
    // Start from the end of the file: only this run's output is interesting.
    if let Some(log) = creating.log.as_mut() {
        if let Ok(c) = fetch(t, &log.target, None, Some(0)) {
            log.offset = Some(c.offset);
        }
    }
    match run_streaming(t, creating, move |t| t.call(Method::WorkspaceRetry { workspace: r, force }), out)? {
        ResultBody::Workspace { workspace } => Ok(workspace),
        other => bail!("unexpected response {other:?}"),
    }
}

/// `canopy log [-f]`: print the tail, then keep printing while setup runs.
pub fn print_log(t: &Transport, r: WorkspaceRef, follow: bool, lines: usize, out: &mut dyn Write) -> Result<()> {
    let first = fetch(t, &r, None, Some(lines)).map_err(|e| anyhow::anyhow!(e))?;
    write!(out, "{}", first.text)?;
    if !first.text.is_empty() && !first.text.ends_with('\n') {
        writeln!(out)?;
    }
    if !follow {
        if first.text.is_empty() {
            writeln!(out, "(no log yet: {})", first.path.display())?;
        }
        return Ok(());
    }
    let mut offset = first.offset;
    let mut quiet_after_stop = 0u8;
    loop {
        std::thread::sleep(Duration::from_millis(400));
        let c = fetch(t, &r, Some(offset), None).map_err(|e| anyhow::anyhow!(e))?;
        write!(out, "{}", c.text)?;
        out.flush()?;
        offset = c.offset;
        if !c.running {
            quiet_after_stop += 1;
            if quiet_after_stop > 3 {
                break;
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn chunk(text: &str, offset: u64, running: bool, status: Status) -> Result<LogChunk, String> {
        Ok(LogChunk { text: text.into(), offset, running, status, path: PathBuf::from("/l") })
    }

    #[test]
    fn first_fill_trims_to_latest_run_and_tails_after() {
        let mut l = LiveLog::new(WorkspaceRef::Id { id: "wAAA".into() }, "x");
        l.apply(chunk("══ t old run\nold output\n══ t new run\n── step\n", 40, true, Status::SettingUp));
        assert_eq!(l.lines, vec!["══ t new run", "── step"]);
        assert_eq!(l.offset, Some(40));
        l.apply(chunk("partial", 47, true, Status::SettingUp));
        assert_eq!(l.lines.len(), 2);
        assert_eq!(l.last_line(), Some("partial"));
        l.apply(chunk(" line\n", 53, false, Status::Stopped));
        assert_eq!(l.lines[2], "partial line");
        assert!(l.finished_ok());
        assert!(!l.finished_broken());
    }

    #[test]
    fn whole_tail_keeps_older_runs() {
        let mut l = LiveLog::new(WorkspaceRef::Id { id: "wAAA".into() }, "x").whole_tail();
        l.apply(chunk("══ a\n1\n══ b\n2\n", 10, false, Status::Broken));
        assert_eq!(l.lines.len(), 4);
        assert!(l.finished_broken());
    }

    #[test]
    fn polls_are_rate_limited_and_single_flight() {
        let mut l = LiveLog::new(WorkspaceRef::Id { id: "w".into() }, "x");
        assert!(l.take_due().is_some(), "first poll is immediate");
        assert!(l.take_due().is_none(), "no second poll while one is pending");
        l.apply(chunk("", 0, true, Status::SettingUp));
        assert!(l.take_due().is_none(), "too soon after the last answer");
    }

    #[test]
    fn scrolling_pins_and_releases() {
        let mut l = LiveLog::new(WorkspaceRef::Id { id: "w".into() }, "x").whole_tail();
        let text: String = (0..50).map(|i| format!("{i}\n")).collect();
        l.apply(chunk(&text, 100, false, Status::Stopped));
        assert_eq!(l.scroll, None);
        l.scroll_by(-5, 10);
        assert_eq!(l.scroll, Some(35));
        l.scroll_by(100, 10);
        assert_eq!(l.scroll, None, "scrolling past the end follows again");
        l.scroll_home();
        assert_eq!(l.scroll, Some(0));
    }

    #[test]
    fn creating_finds_then_fetches() {
        let mut c = Creating::new(PathBuf::from("/p"), None, BTreeSet::from(["wOLD".to_string()]));
        assert!(matches!(c.next_poll(), Poll::Find { .. }));
        assert!(matches!(c.next_poll(), Poll::Nothing), "find is single-flight");
        let ws = canopy_core::state::Workspace { id: "wNEW".into(), name: "fresh".into(), status: Status::SettingUp, ..Default::default() };
        let row = WorkspaceRow::from_workspace(&ws, "p");
        c.on_found(Ok(Some(row)));
        assert!(matches!(c.next_poll(), Poll::Fetch { target: WorkspaceRef::Id { ref id }, offset: None } if id == "wNEW"));
    }

    #[test]
    fn display_strips_marker_timestamps_only() {
        assert_eq!(display_line("── 2026-10-06T00:35:08Z running bin/setup"), "── running bin/setup");
        assert_eq!(display_line("══ 2026-10-06T00:35:08Z retrying setup"), "══ retrying setup");
        assert_eq!(display_line("=== 2026-10-06T00:35:08Z bin/setup ==="), "=== bin/setup");
        assert_eq!(display_line("── no timestamp here"), "── no timestamp here");
        assert_eq!(display_line("plain output 2026-10-06T00:35:08Z"), "plain output 2026-10-06T00:35:08Z");
    }

    #[test]
    fn fit_truncates_with_ellipsis() {
        assert_eq!(fit("hello", 10), "hello");
        assert_eq!(fit("hello world", 5), "hell…");
        assert_eq!(fit("", 3), "");
    }
}
