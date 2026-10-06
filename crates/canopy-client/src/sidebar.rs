//! `canopy sidebar`: the narrow pane that lives at the left of every session.
//!
//! A collapsible tree of projects and workspaces with badges, the current workspace
//! highlighted and its tmux windows listed as tabs. Enter switches session or tab via
//! `tmux switch-client` / `select-window` (through the server). `q` hides the sidebar
//! (the pane closes because the process exits); `prefix+b` brings it back.

use crate::newform::{self, Action, NewForm, Source};
use crate::tui::{agent_badge, live_glyph, source_tabs};
use crate::{call, subscribe};
use anyhow::Result;
use canopy_core::state::Status;
use canopy_proto::{AttachTarget, Method, ResultBody, Response, WindowRow, WorkspaceRef, WorkspaceRow};
use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use ratatui::layout::{Constraint, Layout, Rect};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span};
use ratatui::widgets::{Block, List, ListItem, ListState, Paragraph, Wrap};
use ratatui::Frame;
use std::collections::{BTreeSet, HashMap};
use std::io::BufRead;
use std::path::{Path, PathBuf};
use std::sync::mpsc;
use std::time::{Duration, Instant};

#[derive(Debug, Clone, PartialEq, Eq)]
#[allow(clippy::large_enum_variant)]
enum Node {
    Project { name: String, root: PathBuf, count: usize, working: usize, blocked: usize },
    /// The repo root itself: the project's main session.
    Main { root: PathBuf, name: String, branch: String, alive: bool, attached: bool, id: String },
    Workspace(WorkspaceRow),
    Tab { workspace_id: String, win: WindowRow },
    NewTab { workspace_id: String },
}

impl Node {
    /// Stable identity so the cursor can follow an item across list rebuilds.
    fn key(&self) -> String {
        match self {
            Node::Project { root, .. } => format!("p:{}", root.display()),
            Node::Main { id, .. } => format!("m:{id}"),
            Node::Workspace(w) => format!("w:{}", w.id),
            Node::Tab { workspace_id, win } => format!("t:{workspace_id}:{}", win.index),
            Node::NewTab { workspace_id } => format!("n:{workspace_id}"),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum Mode {
    Tree,
    Help,
    ConfirmDelete,
    ConfirmKill,
    ConfirmForget(PathBuf, String),
    New(Box<NewForm>),
    AddProject(String),
    Busy(String),
    Error(String),
}

#[allow(clippy::large_enum_variant)]
enum Bg {
    Refresh,
    Picks(Source, Result<Vec<canopy_proto::PickItem>, String>),
    Done(Result<String, String>),
}

use crate::shared::{mtime, SharedUi};

struct Side {
    socket: PathBuf,
    current_id: Option<String>,
    rows: Vec<WorkspaceRow>,
    projects: Vec<canopy_proto::ProjectRow>,
    /// Tabs per expanded workspace / main id.
    windows_by: HashMap<String, Vec<WindowRow>>,
    /// Workspace / main ids whose tab list is open (shared; the current one is always open).
    expanded: BTreeSet<String>,
    collapsed: BTreeSet<PathBuf>,
    list: ListState,
    /// Identity of the selected item; `clamp` re-finds it after the list changes.
    sel_key: Option<String>,
    mode: Mode,
    status: String,
    status_at: Instant,
    last_refresh: Instant,
    last_windows: Instant,
    spinner: usize,
    collapsed_file: PathBuf,
    shared_mtime: Option<std::time::SystemTime>,
    /// Rendering as the thin strip (derived from the pane width each frame).
    strip: bool,
    /// What the pane *should* be: set by `q` / keys here, or by the server resizing us to
    /// exactly the strip or the full width. Transient widths never change it.
    intent_strip: bool,
    full_width: Option<u16>,
}

impl Side {
    fn nodes(&self) -> Vec<Node> {
        let mut by_project: HashMap<PathBuf, Vec<&WorkspaceRow>> = HashMap::new();
        for r in &self.rows {
            by_project.entry(r.project_root.clone()).or_default().push(r);
        }
        // Every registered project appears, even with no workspaces yet.
        let mut names: HashMap<PathBuf, String> = self.projects.iter().map(|p| (p.root.clone(), p.name.clone())).collect();
        for r in &self.rows {
            names.entry(r.project_root.clone()).or_insert_with(|| r.project.clone());
        }
        let mut roots: Vec<PathBuf> = names.keys().cloned().collect();
        roots.sort_by_key(|r| names[r].clone());
        let mut out = Vec::new();
        for root in roots {
            let mut wss: Vec<&WorkspaceRow> = by_project.get(&root).cloned().unwrap_or_default();
            wss.sort_by(|a, b| a.name.cmp(&b.name));
            let working = wss.iter().filter(|w| w.agent_state == canopy_proto::AgentState::Working).count();
            let blocked = wss.iter().filter(|w| w.agent_state == canopy_proto::AgentState::Blocked).count();
            out.push(Node::Project { name: names[&root].clone(), root: root.clone(), count: wss.len(), working, blocked });
            if self.collapsed.contains(&root) {
                continue;
            }
            if let Some(p) = self.projects.iter().find(|p| p.root == root) {
                let id = format!("main:{}", p.name);
                out.push(Node::Main { root: root.clone(), name: p.name.clone(), branch: p.main_branch.clone(), alive: p.main_alive, attached: p.main_attached, id: id.clone() });
                if self.expanded.contains(&id) && p.main_alive {
                    for win in self.windows_by.get(&id).into_iter().flatten() {
                        out.push(Node::Tab { workspace_id: id.clone(), win: win.clone() });
                    }
                    out.push(Node::NewTab { workspace_id: id.clone() });
                }
            }
            for w in wss {
                out.push(Node::Workspace(w.clone()));
                if self.expanded.contains(&w.id) && w.alive {
                    for win in self.windows_by.get(&w.id).into_iter().flatten() {
                        out.push(Node::Tab { workspace_id: w.id.clone(), win: win.clone() });
                    }
                    out.push(Node::NewTab { workspace_id: w.id.clone() });
                }
            }
        }
        out
    }

    fn selected(&self) -> Option<Node> {
        let n = self.nodes();
        self.list.selected().and_then(|i| n.get(i).cloned())
    }

    fn selected_workspace(&self) -> Option<WorkspaceRow> {
        match self.selected()? {
            Node::Workspace(w) => Some(w),
            Node::Tab { workspace_id, .. } | Node::NewTab { workspace_id } => self.rows.iter().find(|r| r.id == workspace_id).cloned(),
            Node::Project { .. } | Node::Main { .. } => None,
        }
    }

    /// Keep the cursor on the same item if it still exists; otherwise stay at the same index.
    fn clamp(&mut self) {
        let nodes = self.nodes();
        if nodes.is_empty() {
            self.list.select(None);
            return;
        }
        if let Some(key) = &self.sel_key {
            if let Some(i) = nodes.iter().position(|n| &n.key() == key) {
                self.list.select(Some(i));
                return;
            }
        }
        let i = self.list.selected().unwrap_or(0).min(nodes.len() - 1);
        self.list.select(Some(i));
        self.sel_key = Some(nodes[i].key());
    }

    fn mv(&mut self, d: i64) {
        let nodes = self.nodes();
        if nodes.is_empty() {
            return;
        }
        let cur = self.list.selected().unwrap_or(0) as i64;
        let i = (cur + d).clamp(0, nodes.len() as i64 - 1) as usize;
        self.list.select(Some(i));
        let key = Some(nodes[i].key());
        if key != self.sel_key {
            self.sel_key = key;
            self.save_shared();
        }
    }

    fn note(&mut self, s: impl Into<String>) {
        self.status = s.into();
        self.status_at = Instant::now();
    }

    fn refresh(&mut self) {
        if let Ok(ResultBody::WorkspaceList { workspaces }) = call(&self.socket, Method::WorkspaceList { project_root: None }) {
            self.rows = workspaces;
        }
        if let Ok(ResultBody::ProjectList { projects }) = call(&self.socket, Method::ProjectList) {
            self.projects = projects;
        }
        self.last_refresh = Instant::now();
        self.refresh_windows();
        self.clamp();
    }

    fn refresh_windows(&mut self) {
        let alive: std::collections::HashSet<String> = self
            .rows
            .iter()
            .filter(|r| r.alive)
            .map(|r| r.id.clone())
            .chain(self.projects.iter().filter(|p| p.main_alive).map(|p| format!("main:{}", p.name)))
            .collect();
        let ids: Vec<String> = self.expanded.iter().filter(|id| alive.contains(*id)).cloned().collect();
        for id in ids {
            if let Ok(ResultBody::WindowList { windows }) = call(&self.socket, Method::WorkspaceWindows { workspace: WorkspaceRef::Id { id: id.clone() } }) {
                self.windows_by.insert(id, windows);
            }
        }
        self.last_windows = Instant::now();
    }

    fn toggle_expanded(&mut self, id: &str) {
        if !self.expanded.remove(id) {
            self.expanded.insert(id.to_string());
            self.refresh_windows();
        }
        self.save_shared();
        self.clamp();
    }

    fn select_current(&mut self) {
        if let Some(id) = &self.current_id {
            let nodes = self.nodes();
            let idx = nodes.iter().position(|n| matches!(n, Node::Workspace(w) if &w.id == id) || matches!(n, Node::Main { id: mid, .. } if mid == id));
            if let Some(i) = idx {
                self.list.select(Some(i));
                self.sel_key = Some(nodes[i].key());
            }
        }
    }

    fn save_shared(&mut self) {
        let mut expanded = self.expanded.clone();
        if let Some(id) = &self.current_id {
            expanded.remove(id); // always open locally; don't force it on other sidebars
        }
        SharedUi { strip: self.intent_strip, collapsed: self.collapsed.clone(), expanded, cursor: self.sel_key.clone() }.save(&self.collapsed_file);
        self.shared_mtime = mtime(&self.collapsed_file);
    }

    /// Pick up changes other sidebars wrote. Returns true when the strip state changed.
    fn poll_shared(&mut self) -> bool {
        let m = mtime(&self.collapsed_file);
        if m == self.shared_mtime {
            return false;
        }
        self.shared_mtime = m;
        let shared = SharedUi::load(&self.collapsed_file);
        self.collapsed = shared.collapsed;
        self.expanded = shared.expanded;
        if let Some(id) = &self.current_id {
            self.expanded.insert(id.clone());
        }
        if shared.cursor.is_some() {
            self.sel_key = shared.cursor;
        }
        let changed = shared.strip != self.intent_strip;
        self.intent_strip = shared.strip;
        self.clamp();
        changed
    }
}

fn tmux(args: &[&str]) -> bool {
    std::process::Command::new("tmux").args(args).status().map(|s| s.success()).unwrap_or(false)
}

/// After changing window, stay in the sidebar: focus the sidebar pane of the session's
/// now-active window (marked, so the guard lets it through).
fn focus_sidebar_here() {
    let Ok(pane) = std::env::var("TMUX_PANE") else { return };
    let session = std::process::Command::new("tmux").args(["display-message", "-p", "-t", &pane, "#{session_id}"]).output().ok().map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string());
    let Some(session) = session.filter(|s| !s.is_empty()) else { return };
    let out = std::process::Command::new("tmux").args(["list-panes", "-t", &session, "-F", "#{pane_id}\t#{@canopy-role}"]).output().ok();
    if let Some(out) = out {
        let text = String::from_utf8_lossy(&out.stdout);
        if let Some(id) = text.lines().find(|l| l.ends_with("\tsidebar")).and_then(|l| l.split('\t').next()) {
            let _ = tmux(&["set-option", "-p", "-t", id, "@canopy-focus", "1"]);
            let _ = tmux(&["select-pane", "-t", id]);
        }
    }
}

/// Switch the client to `session` and land on *its* sidebar, so navigation continues there
/// (it expands via focus-follow). Falls back to a plain switch when it has no sidebar.
fn switch_to(session: &str) -> bool {
    if !tmux(&["switch-client", "-t", &format!("={session}")]) {
        return false;
    }
    let out = std::process::Command::new("tmux")
        .args(["list-panes", "-t", &format!("={session}"), "-F", "#{pane_id}\t#{@canopy-role}"])
        .output()
        .ok();
    if let Some(out) = out {
        let text = String::from_utf8_lossy(&out.stdout);
        if let Some(id) = text.lines().find(|l| l.ends_with("\tsidebar")).and_then(|l| l.split('\t').next()) {
            let _ = tmux(&["set-option", "-p", "-t", id, "@canopy-focus", "1"]);
            let _ = tmux(&["select-pane", "-t", id]);
        }
    }
    true
}

/// Width of the collapsed strip. "Collapsed" is derived from the pane's actual width, so
/// the server, the user (`prefix+b`) and this process all agree without messaging.
pub const COLLAPSED_WIDTH: u16 = 3;

fn is_collapsed_width(cols: u16) -> bool {
    cols <= COLLAPSED_WIDTH + 3
}

fn pane_width() -> Option<u16> {
    let pane = std::env::var("TMUX_PANE").ok()?;
    std::process::Command::new("tmux")
        .args(["display-message", "-p", "-t", &pane, "#{pane_width}"])
        .output()
        .ok()
        .and_then(|o| String::from_utf8_lossy(&o.stdout).trim().parse::<u16>().ok())
}

/// While focused, the sidebar is modal: `@canopy-lock=1` on its window makes the tmux
/// focus guard bounce plain pane navigation back here. Canopy's own moves out (q, prefix+b)
/// release it first.
/// Consume the one-shot "canopy focused me" marker. True when it was set.
fn take_focus_marker() -> bool {
    let Ok(pane) = std::env::var("TMUX_PANE") else { return true };
    let marked = std::process::Command::new("tmux")
        .args(["show-options", "-p", "-t", &pane, "-v", "@canopy-focus"])
        .output()
        .ok()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim() == "1")
        .unwrap_or(false);
    if marked {
        let _ = tmux(&["set-option", "-p", "-t", &pane, "-u", "@canopy-focus"]);
    }
    marked
}

fn lock_is_set() -> bool {
    let Ok(pane) = std::env::var("TMUX_PANE") else { return false };
    std::process::Command::new("tmux")
        .args(["show-options", "-w", "-t", &pane, "-v", "@canopy-lock"])
        .output()
        .ok()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim() == "1")
        .unwrap_or(false)
}

/// A sidebar alone in its window means the tab's real pane exited. Close the tab when the
/// session has other tabs; if this was the last one, keep the workspace session alive by
/// opening a fresh shell in the workspace directory instead.
fn ensure_not_alone() {
    let Ok(pane) = std::env::var("TMUX_PANE") else { return };
    let info = std::process::Command::new("tmux")
        .args(["display-message", "-p", "-t", &pane, "#{window_panes}\t#{session_windows}\t#{window_id}"])
        .output()
        .ok();
    let Some(info) = info else { return };
    let text = String::from_utf8_lossy(&info.stdout);
    let mut parts = text.trim().split('\t');
    let (Some(panes), Some(windows), Some(window)) = (parts.next(), parts.next(), parts.next()) else { return };
    if panes.trim() != "1" {
        return;
    }
    if windows.trim().parse::<u32>().unwrap_or(1) > 1 {
        let _ = tmux(&["kill-window", "-t", window]);
        return;
    }
    let cwd = std::env::var("CANOPY_WORKSPACE_PATH").ok().filter(|p| Path::new(p).is_dir()).or_else(|| std::env::var("CANOPY_PROJECT_ROOT").ok()).unwrap_or_else(|| "~".into());
    set_lock(false);
    let out = std::process::Command::new("tmux").args(["split-window", "-h", "-f", "-t", &pane, "-c", &cwd, "-P", "-F", "#{pane_id}"]).output().ok();
    if let Some(out) = out {
        let id = String::from_utf8_lossy(&out.stdout).trim().to_string();
        if id.starts_with('%') {
            let _ = tmux(&["set-option", "-p", "-t", &id, "@canopy-role", "terminal:shell"]);
        }
    }
}

fn set_lock(on: bool) {
    let Ok(pane) = std::env::var("TMUX_PANE") else { return };
    if on {
        let _ = tmux(&["set-option", "-w", "-t", &pane, "@canopy-lock", "1"]);
    } else {
        let _ = tmux(&["set-option", "-w", "-t", &pane, "-u", "@canopy-lock"]);
    }
}

/// Is this pane the active pane of its window? (tmux only; `None` outside tmux.)
fn pane_active() -> Option<bool> {
    let pane = std::env::var("TMUX_PANE").ok()?;
    std::process::Command::new("tmux")
        .args(["display-message", "-p", "-t", &pane, "#{pane_active}"])
        .output()
        .ok()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim() == "1")
}

/// Is this pane still the full-height left column of its window? `swap-pane` (`prefix+{`)
/// and friends can move it anywhere; `None` outside tmux.
fn anchored() -> Option<bool> {
    let pane = std::env::var("TMUX_PANE").ok()?;
    std::process::Command::new("tmux")
        .args(["display-message", "-p", "-t", &pane, "#{pane_at_left}#{pane_at_top}#{pane_at_bottom}"])
        .output()
        .ok()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim() == "111")
}

/// Put this pane back as the full-height left column, keeping whoever had focus focused.
///
/// Usually we were swapped (`prefix+{`): some other pane now occupies the left column, so
/// swapping with it undoes the move exactly and restores that pane's size too. If no pane
/// holds the left column (layout was rearranged some other way), go out through a scratch
/// window and join back in; tmux refuses to join a pane into its own window directly.
fn reanchor(width: u16) {
    let Ok(pane) = std::env::var("TMUX_PANE") else { return };
    let info = std::process::Command::new("tmux").args(["display-message", "-p", "-t", &pane, "#{window_id}\t#{session_id}"]).output().ok();
    let Some(info) = info else { return };
    let text = String::from_utf8_lossy(&info.stdout);
    let mut parts = text.trim().split('\t');
    let (Some(window), Some(session)) = (parts.next(), parts.next()) else { return };
    let listing = std::process::Command::new("tmux")
        .args(["list-panes", "-t", window, "-F", "#{pane_id}\t#{pane_active}\t#{pane_at_left}#{pane_at_top}#{pane_at_bottom}"])
        .output()
        .ok()
        .map(|o| String::from_utf8_lossy(&o.stdout).into_owned())
        .unwrap_or_default();
    let focused = listing.lines().find(|l| l.split('\t').nth(1) == Some("1")).map(|l| l.split('\t').next().unwrap_or("").to_string()).filter(|id| id != &pane);
    let left_column = listing.lines().find(|l| l.split('\t').nth(2) == Some("111") && !l.starts_with(&pane)).map(|l| l.split('\t').next().unwrap_or("").to_string());
    match left_column {
        Some(other) => {
            let _ = tmux(&["swap-pane", "-d", "-s", &pane, "-t", &other]);
            // The swap carried the user's focus into the sidebar; give it back to the pane
            // they were actually moving so the sidebar stays a strip.
            let _ = tmux(&["select-pane", "-t", focused.as_deref().unwrap_or(&other)]);
        }
        None => {
            if !tmux(&["break-pane", "-d", "-s", &pane, "-t", session]) {
                return;
            }
            let _ = tmux(&["join-pane", "-h", "-b", "-f", "-l", &width.to_string(), "-s", &pane, "-t", window]);
            if let Some(f) = focused {
                let _ = tmux(&["select-pane", "-t", &f]);
            }
        }
    }
}

fn resize_self(width: u16) {
    if let Ok(pane) = std::env::var("TMUX_PANE") {
        let _ = tmux(&["resize-pane", "-t", &pane, "-x", &width.to_string()]);
    }
}

/// Snap the pane back to the intended width (full or strip). tmux rescales panes when the
/// window changes size and hands growth to the leftmost pane, so the observed width says
/// nothing about intent; only exact matches with one of the two legal widths do.
fn enforce_width(width: u16, strip: bool) {
    let Some(cur) = pane_width() else { return };
    let target = if strip { COLLAPSED_WIDTH } else { width };
    if cur != target {
        resize_self(target);
    }
}

pub fn run(socket: &Path, home: &Path, width: Option<u16>) -> Result<()> {
    let current_id = std::env::var("CANOPY_WORKSPACE_ID").ok();
    let collapsed_file = home.join("sidebar.json");
    let shared = SharedUi::load(&collapsed_file);
    let mut initially_expanded: BTreeSet<String> = shared.expanded.clone();
    initially_expanded.extend(current_id.iter().cloned());
    let start_strip = pane_width().is_some_and(is_collapsed_width);
    // Pane size follows the shared state, not the other way round.
    if let Some(w) = width {
        enforce_width(w, shared.strip);
    }
    let collapsed_file = home.join("sidebar.json");
    let mut side = Side {
        socket: socket.to_path_buf(),
        current_id,
        rows: vec![],
        projects: vec![],
        windows_by: HashMap::new(),
        expanded: initially_expanded,
        collapsed: shared.collapsed.clone(),
        list: ListState::default(),
        sel_key: None,
        mode: Mode::Tree,
        status: String::new(),
        status_at: Instant::now(),
        last_refresh: Instant::now(),
        last_windows: Instant::now(),
        spinner: 0,
        shared_mtime: mtime(&collapsed_file),
        collapsed_file,
        strip: start_strip,
        intent_strip: shared.strip,
        full_width: width,
    };
    side.refresh();
    if shared.cursor.is_some() {
        side.sel_key = shared.cursor.clone();
        side.clamp();
    } else {
        side.select_current();
    }
    if side.list.selected().is_none() {
        side.clamp();
    }

    let (tx, rx) = mpsc::channel::<Bg>();
    {
        let sock = socket.to_path_buf();
        let tx = tx.clone();
        std::thread::spawn(move || loop {
            if let Ok(reader) = subscribe(&sock, None) {
                for line in reader.lines() {
                    let Ok(line) = line else { break };
                    if let Ok(Response::Ok { result: ResultBody::Event { .. }, .. }) = serde_json::from_str::<Response>(&line) {
                        if tx.send(Bg::Refresh).is_err() {
                            return;
                        }
                    }
                }
            }
            std::thread::sleep(Duration::from_secs(1));
        });
    }

    let mut terminal = ratatui::init();
    let res = event_loop(&mut terminal, &mut side, &tx, &rx, width);
    ratatui::restore();
    res
}

fn event_loop(terminal: &mut ratatui::DefaultTerminal, side: &mut Side, tx: &mpsc::Sender<Bg>, rx: &mpsc::Receiver<Bg>, width: Option<u16>) -> Result<()> {
    let mut last_enforce = Instant::now();
    // Focus following: collapse when focus leaves, expand when it arrives.
    let auto_collapse = canopy_core::settings::Settings::load(&canopy_core::paths::Paths::from_env().settings_file()).map(|s| s.sidebar.auto_collapse).unwrap_or(true);
    let mut last_focus_poll = Instant::now();
    let mut last_layout_poll = Instant::now();
    // Start as if focused so a sidebar born unfocused (fresh session, respawn) collapses on
    // its first poll instead of sitting expanded next to the pane you are actually in.
    let mut was_active: Option<bool> = if auto_collapse { Some(true) } else { None };
    // Resizes ripple into every other pane (editors redraw, some crash on rapid churn), so
    // snap only after the pane has sat at a wrong width for two consecutive polls.
    let mut off_size_polls: u8 = 0;
    loop {
        terminal.draw(|f| draw(f, side))?;
        while let Ok(msg) = rx.try_recv() {
            match msg {
                Bg::Refresh => side.refresh(),
                Bg::Picks(source, items) => {
                    if let Mode::New(form) = &mut side.mode {
                        form.set_items(source, items);
                    }
                }
                Bg::Done(Ok(s)) => {
                    side.mode = Mode::Tree;
                    side.note(s);
                    side.refresh();
                }
                Bg::Done(Err(e)) => side.mode = Mode::Error(e),
            }
        }
        if side.last_refresh.elapsed() > Duration::from_secs(2) {
            side.refresh();
        } else if side.last_windows.elapsed() > Duration::from_millis(1500) {
            side.refresh_windows();
            side.clamp();
        }
        // Shared state from other sidebars (open/closed, folds).
        if side.poll_shared() {
            if let Some(w) = width {
                resize_self(if side.intent_strip { COLLAPSED_WIDTH } else { w });
            }
        }
        // Layout guard on its own timer (the focus poll below resets its timer every tick).
        if last_layout_poll.elapsed() > Duration::from_millis(300) {
            last_layout_poll = Instant::now();
            ensure_not_alone();
            // Displaced by swap-pane or similar? Move back to the left edge first.
            if anchored() == Some(false) {
                let target = if side.intent_strip { COLLAPSED_WIDTH } else { width.unwrap_or(30) };
                reanchor(target);
                // Focus just bounced through this pane; don't read that as "user entered".
                was_active = Some(false);
                last_focus_poll = Instant::now();
            }
        }
        if last_focus_poll.elapsed() > Duration::from_millis(120) && !auto_collapse {
            last_focus_poll = Instant::now();
            let now_active = pane_active();
            if now_active.is_some() && now_active != was_active {
                if now_active == Some(true) {
                    // Entered. Legitimate entries carry the marker (or the lock is already
                    // ours); anything else slipped past the tmux hook (mouse, odd bindings):
                    // send it back.
                    if take_focus_marker() || lock_is_set() {
                        set_lock(true);
                        was_active = Some(true);
                    } else {
                        let _ = tmux(&["select-pane", "-l"]);
                        was_active = Some(false);
                    }
                } else {
                    set_lock(false);
                    was_active = Some(false);
                }
            }
        }
        if auto_collapse && last_focus_poll.elapsed() > Duration::from_millis(300) {
            last_focus_poll = Instant::now();
            let now_active = pane_active();
            if let (Some(prev), Some(cur), Some(w)) = (was_active, now_active, width) {
                if cur && !prev && side.intent_strip {
                    side.intent_strip = false;
                    resize_self(w);
                } else if !cur && prev && !side.intent_strip && matches!(side.mode, Mode::Tree) {
                    side.intent_strip = true;
                    resize_self(COLLAPSED_WIDTH);
                }
            }
            if now_active.is_some() {
                was_active = now_active;
            }
        }
        // Width guard: poll the terminal size rather than rely on SIGWINCH delivery, which
        // is not guaranteed for a pane in a detached window.
        if let Some(w) = width {
            if last_enforce.elapsed() > Duration::from_millis(500) {
                last_enforce = Instant::now();
                if let Ok((cols, _)) = crossterm::terminal::size() {
                    // Exact legal widths are deliberate (server toggle, our own resize).
                    if cols == COLLAPSED_WIDTH {
                        if !side.intent_strip {
                            side.intent_strip = true;
                            side.save_shared();
                        }
                        off_size_polls = 0;
                    } else if cols == w {
                        if side.intent_strip {
                            side.intent_strip = false;
                            side.save_shared();
                        }
                        off_size_polls = 0;
                    } else {
                        off_size_polls = off_size_polls.saturating_add(1);
                        if off_size_polls >= 2 {
                            enforce_width(w, side.intent_strip);
                            off_size_polls = 0;
                        }
                    }
                }
            }
        }
        if event::poll(Duration::from_millis(120))? {
            match event::read()? {
                Event::Key(key) => {
                    if key.kind == KeyEventKind::Press && handle_key(side, key, tx) {
                        return Ok(());
                    }
                }
                Event::Resize(..) => {
                    // Let the poll above handle it after the size has settled.
                    last_enforce = Instant::now();
                }
                _ => {}
            }
        }
        side.spinner = side.spinner.wrapping_add(1);
    }
}

/// Run a canopy UI subcommand in a tmux popup over the whole client, then refresh.
/// `display-popup` blocks until the popup closes, so it runs on a thread.
fn popup(side: &Side, tx: &mpsc::Sender<Bg>, args: Vec<String>, cwd: &Path) {
    let bin = std::env::current_exe().map(|p| p.display().to_string()).unwrap_or_else(|_| "canopy".into());
    let socket = side.socket.display().to_string();
    let cmd = format!("CANOPY_SOCKET_PATH={} {} {}", shq(&socket), shq(&bin), args.iter().map(|a| shq(a)).collect::<Vec<_>>().join(" "));
    let cwd = cwd.display().to_string();
    let tx = tx.clone();
    std::thread::spawn(move || {
        // Borderless: the dialog draws its own frame, so no double box.
        let (w, h) = if args.first().map(String::as_str) == Some("ui") && args.get(1).map(String::as_str) == Some("add-project") { ("76", "9") } else { ("92", "22") };
        let _ = std::process::Command::new("tmux").args(["display-popup", "-B", "-E", "-w", w, "-h", h, "-d", &cwd, &cmd]).status();
        let _ = tx.send(Bg::Refresh);
    });
}

fn shq(s: &str) -> String {
    if !s.is_empty() && s.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '/' | ':' | '=')) {
        s.to_string()
    } else {
        format!("'{}'", s.replace('\'', "'\\''"))
    }
}

fn spawn_bg(side: &Side, tx: &mpsc::Sender<Bg>, f: impl FnOnce(&Path) -> Result<String, String> + Send + 'static) {
    let sock = side.socket.clone();
    let tx = tx.clone();
    std::thread::spawn(move || {
        let _ = tx.send(Bg::Done(f(&sock)));
    });
}

/// Returns true to exit the process (only Ctrl-C does; `q` collapses to the thin strip).
fn handle_key(side: &mut Side, key: KeyEvent, tx: &mpsc::Sender<Bg>) -> bool {
    let ctrl_c = key.code == KeyCode::Char('c') && key.modifiers.contains(KeyModifiers::CONTROL);
    if side.strip {
        // Any key on the strip expands it; focus stays here so you can navigate at once.
        if ctrl_c {
            return true;
        }
        if let Some(w) = side.full_width {
            side.intent_strip = false;
            resize_self(w);
            side.strip = false;
            side.save_shared();
        }
        return false;
    }
    match side.mode.clone() {
        Mode::Busy(_) => return ctrl_c,
        Mode::Help | Mode::Error(_) => {
            side.mode = Mode::Tree;
        }
        Mode::ConfirmDelete | Mode::ConfirmKill => {
            let yes = matches!(key.code, KeyCode::Char('y') | KeyCode::Char('Y'));
            let mode = side.mode.clone();
            side.mode = Mode::Tree;
            if !yes {
                return false;
            }
            let Some(w) = side.selected_workspace() else { return false };
            let r = WorkspaceRef::Id { id: w.id.clone() };
            let name = w.name.clone();
            if mode == Mode::ConfirmDelete {
                side.mode = Mode::Busy(format!("removing {name}"));
                spawn_bg(side, tx, move |s| call(s, Method::WorkspaceRemove { workspace: r, force: true }).map(|_| format!("removed {name}")).map_err(|e| e.to_string()));
            } else {
                side.mode = Mode::Busy(format!("stopping {name}"));
                spawn_bg(side, tx, move |s| call(s, Method::WorkspaceStop { workspace: r }).map(|_| format!("stopped {name}")).map_err(|e| e.to_string()));
            }
        }
        Mode::New(mut form) => match form.handle_key(key) {
            Action::Cancel => side.mode = Mode::Tree,
            Action::None => side.mode = Mode::New(form),
            Action::Load(source) => {
                let root = form.project_root.clone();
                side.mode = Mode::New(form);
                let sock = side.socket.clone();
                let tx = tx.clone();
                std::thread::spawn(move || {
                    let t = crate::Transport::Local(sock);
                    let _ = tx.send(Bg::Picks(source, newform::load(&t, &root, source)));
                });
            }
            Action::Submit(req) => {
                side.mode = Mode::Busy("creating".into());
                spawn_bg(side, tx, move |s| match call(s, Method::WorkspaceCreate(req)) {
                    Ok(ResultBody::Workspace { workspace }) => {
                        let _ = switch_to(&workspace.session);
                        Ok(format!("created {}", workspace.name))
                    }
                    Ok(_) => Err("unexpected response".into()),
                    Err(e) => Err(e.to_string()),
                });
            }
        },
        Mode::AddProject(mut text) => match key.code {
            KeyCode::Esc => side.mode = Mode::Tree,
            _ if ctrl_c => return true,
            KeyCode::Backspace => {
                text.pop();
                side.mode = Mode::AddProject(text);
            }
            KeyCode::Enter => {
                let v = text.trim().to_string();
                if v.is_empty() {
                    return false;
                }
                let is_url = v.contains("://") || (v.contains('@') && v.contains(':'));
                let home = std::env::var("HOME").unwrap_or_default();
                let expanded = v.strip_prefix("~/").map(|r| format!("{home}/{r}")).unwrap_or(v.clone());
                let (path, url) = if is_url { (None, Some(v.clone())) } else { (Some(PathBuf::from(expanded)), None) };
                side.mode = Mode::Busy(if is_url { "cloning".into() } else { "adding".into() });
                spawn_bg(side, tx, move |s| match call(s, Method::ProjectInit { path, url, with_scripts: true, adopt_from: None }) {
                    Ok(ResultBody::ProjectList { projects }) => Ok(format!("added {}", projects.first().map(|p| p.name.clone()).unwrap_or_default())),
                    Ok(_) => Err("unexpected response".into()),
                    Err(e) => Err(e.to_string()),
                });
            }
            KeyCode::Char(c) => {
                text.push(c);
                side.mode = Mode::AddProject(text);
            }
            _ => {}
        },
        Mode::ConfirmForget(root, name) => {
            side.mode = Mode::Tree;
            if matches!(key.code, KeyCode::Char('y') | KeyCode::Char('Y')) {
                side.mode = Mode::Busy(format!("forgetting {name}"));
                spawn_bg(side, tx, move |s| call(s, Method::ProjectRemove { root }).map(|_| format!("forgot {name}")).map_err(|e| e.to_string()));
            }
        }
        Mode::Tree => match key.code {
            KeyCode::Char('q') | KeyCode::Esc => {
                // Collapse to the strip and hand focus to the main pane.
                side.intent_strip = true;
                resize_self(COLLAPSED_WIDTH);
                side.strip = true;
                side.save_shared();
                set_lock(false);
                let _ = tmux(&["select-pane", "-l"]);
            }
            _ if ctrl_c => return true,
            KeyCode::Char('j') | KeyCode::Down => side.mv(1),
            KeyCode::Char('k') | KeyCode::Up => side.mv(-1),
            KeyCode::Char('g') | KeyCode::Home => side.mv(-1_000_000),
            KeyCode::Char('G') | KeyCode::End => side.mv(1_000_000),
            KeyCode::Char('?') => side.mode = Mode::Help,
            KeyCode::Char('r') => {
                side.refresh();
                side.note("refreshed");
            }
            KeyCode::Char(' ') | KeyCode::Right | KeyCode::Char('l') | KeyCode::Tab
                if matches!(side.selected(), Some(Node::Workspace(_)) | Some(Node::Main { .. })) =>
            {
                let id = match side.selected() {
                    Some(Node::Workspace(w)) => w.id,
                    Some(Node::Main { id, .. }) => id,
                    _ => return false,
                };
                side.toggle_expanded(&id);
            }
            KeyCode::Char(' ') | KeyCode::Left | KeyCode::Right | KeyCode::Char('h') | KeyCode::Char('l') | KeyCode::Tab => {
                let root = match side.selected() {
                    Some(Node::Project { root, .. }) => Some(root),
                    Some(Node::Workspace(w)) if matches!(key.code, KeyCode::Left | KeyCode::Char('h')) => Some(w.project_root),
                    Some(Node::Main { root, .. }) if matches!(key.code, KeyCode::Left | KeyCode::Char('h')) => Some(root),
                    _ => None,
                };
                if let Some(root) = root {
                    if !side.collapsed.remove(&root) {
                        side.collapsed.insert(root.clone());
                        // Keep the cursor on the project header.
                        side.sel_key = Some(format!("p:{}", root.display()));
                    }
                    side.save_shared();
                    side.clamp();
                }
            }
            KeyCode::Char('n') => {
                let ctx = match side.selected() {
                    Some(Node::Project { root, name, .. }) | Some(Node::Main { root, name, .. }) => Some((root, name)),
                    _ => side.selected_workspace().map(|w| (w.project_root.clone(), w.project.clone())).or_else(|| side.projects.first().map(|p| (p.root.clone(), p.name.clone()))),
                };
                match ctx {
                    Some((root, name)) => {
                        if std::env::var_os("TMUX").is_some() {
                            // Full-screen popup; the sidebar is too narrow for a form.
                            popup(side, tx, vec!["ui".into(), "new".into(), "--project-root".into(), root.display().to_string()], &root);
                        } else {
                            side.mode = Mode::New(Box::new(NewForm::new(root, name)));
                        }
                    }
                    None => {
                        if std::env::var_os("TMUX").is_some() {
                            popup(side, tx, vec!["ui".into(), "add-project".into()], Path::new("/"));
                        } else {
                            side.mode = Mode::AddProject(String::new());
                        }
                    }
                }
            }
            KeyCode::Char('a') => {
                if std::env::var_os("TMUX").is_some() {
                    popup(side, tx, vec!["ui".into(), "add-project".into()], Path::new("/"));
                } else {
                    side.mode = Mode::AddProject(String::new());
                }
            }
            KeyCode::Char('t') => {
                let target = match side.selected() {
                    Some(Node::Main { id, alive: true, .. }) => Some(id),
                    _ => side.selected_workspace().map(|w| w.id),
                };
                if let Some(id) = target {
                    let same_session = side.current_id.as_deref() == Some(&id);
                    let r = WorkspaceRef::Id { id };
                    if let Err(e) = call(&side.socket, Method::WorkspaceNewWindow { workspace: r, name: None }) {
                        side.mode = Mode::Error(e.to_string());
                    } else {
                        if same_session {
                            focus_sidebar_here();
                        }
                        side.refresh_windows();
                    }
                }
            }
            KeyCode::Char('d') if matches!(side.selected(), Some(Node::Project { .. })) => {
                if let Some(Node::Project { root, name, count, .. }) = side.selected() {
                    if count > 0 {
                        side.note("remove its workspaces first");
                    } else {
                        side.mode = Mode::ConfirmForget(root, name);
                    }
                }
            }
            KeyCode::Char('K') if matches!(side.selected(), Some(Node::Main { .. })) => {
                if let Some(Node::Main { root, name, alive, id, .. }) = side.selected() {
                    if side.current_id.as_deref() == Some(&id) {
                        side.note("that's this session; switch away first");
                    } else if alive {
                        side.mode = Mode::Busy(format!("stopping {name} main"));
                        spawn_bg(side, tx, move |s| call(s, Method::ProjectStopMain { root }).map(|_| format!("stopped {name} main")).map_err(|e| e.to_string()));
                    }
                }
            }
            KeyCode::Char('d') | KeyCode::Char('K') => {
                let Some(w) = side.selected_workspace() else { return false };
                if side.current_id.as_deref() == Some(&w.id) {
                    side.note("that's this workspace; switch away first");
                } else if key.code == KeyCode::Char('d') {
                    side.mode = Mode::ConfirmDelete;
                } else if w.alive {
                    side.mode = Mode::ConfirmKill;
                }
            }
            KeyCode::Enter => match side.selected() {
                Some(Node::Main { root, name, id, .. }) => {
                    if side.current_id.as_deref() == Some(&id) {
                        return false;
                    }
                    side.mode = Mode::Busy(format!("opening {name} main"));
                    spawn_bg(side, tx, move |s| match call(s, Method::ProjectMain { root }) {
                        Ok(ResultBody::AttachTarget { target: AttachTarget::Tmux { session, .. } }) => {
                            if switch_to(&session) { Ok(format!("→ {session}")) } else { Err(format!("tmux switch-client to {session} failed")) }
                        }
                        Ok(_) => Err("unexpected attach target".into()),
                        Err(e) => Err(e.to_string()),
                    });
                }
                Some(Node::Project { root, .. }) => {
                    if !side.collapsed.remove(&root) {
                        side.collapsed.insert(root);
                    }
                    side.save_shared();
                    side.clamp();
                }
                Some(Node::Tab { workspace_id, win }) => {
                    let same_session = side.current_id.as_deref() == Some(&workspace_id);
                    let _ = call(&side.socket, Method::WorkspaceSelectWindow { workspace: WorkspaceRef::Id { id: workspace_id.clone() }, index: win.index });
                    if same_session {
                        focus_sidebar_here();
                    } else if let Some(w) = side.rows.iter().find(|r| r.id == workspace_id).map(|r| r.session.clone()).or_else(|| side.projects.iter().find(|p| format!("main:{}", p.name) == workspace_id).map(|p| p.main_session.clone())) {
                        let _ = switch_to(&w);
                    }
                    side.refresh_windows();
                }
                Some(Node::NewTab { workspace_id }) => {
                    let same_session = side.current_id.as_deref() == Some(&workspace_id);
                    if let Err(e) = call(&side.socket, Method::WorkspaceNewWindow { workspace: WorkspaceRef::Id { id: workspace_id }, name: None }) {
                        side.mode = Mode::Error(e.to_string());
                    } else if same_session {
                        focus_sidebar_here();
                    }
                    side.refresh_windows();
                }
                Some(Node::Workspace(w)) => {
                    if side.current_id.as_deref() == Some(&w.id) {
                        return false;
                    }
                    let r = WorkspaceRef::Id { id: w.id.clone() };
                    side.mode = Mode::Busy(if w.alive { format!("switching to {}", w.name) } else { format!("resurrecting {}", w.name) });
                    spawn_bg(side, tx, move |s| match call(s, Method::WorkspaceAttachTarget { workspace: r }) {
                        Ok(ResultBody::AttachTarget { target: AttachTarget::Tmux { session, .. } }) => {
                            if switch_to(&session) {
                                Ok(format!("→ {session}"))
                            } else {
                                Err(format!("tmux switch-client to {session} failed"))
                            }
                        }
                        Ok(_) => Err("unexpected attach target".into()),
                        Err(e) => Err(e.to_string()),
                    });
                }
                None => {}
            },
            _ => {}
        },
    }
    false
}

/// Dim second line under a workspace: port, PR + CI, ahead/behind.
fn detail_line(w: &WorkspaceRow, width: usize) -> Line<'static> {
    let dim = Style::new().fg(Color::Rgb(92, 99, 112));
    if w.status == Status::SettingUp {
        // What setup is doing right now, straight from the server's live progress.
        let ms = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_millis()).unwrap_or(0);
        let spin = crate::livelog::SPIN[(ms / 120) as usize % crate::livelog::SPIN.len()];
        let text = if w.progress.is_empty() { "setting up…" } else { w.progress.as_str() };
        return Line::from(vec![Span::styled("       ", dim), Span::styled(format!("{spin} {}", trunc(text, width.saturating_sub(9))), Style::new().fg(Color::Yellow))]);
    }
    let mut spans = vec![Span::styled(format!("       :{}", w.port), dim)];
    if let Some(n) = w.pr_number {
        let (pr_style, tag) = match w.pr_state.as_str() {
            "MERGED" => (Style::new().fg(Color::Green), " merged"),
            "CLOSED" => (Style::new().fg(Color::Red), " closed"),
            _ => (Style::new().fg(Color::Blue), ""),
        };
        spans.push(Span::styled(" · ", dim));
        spans.push(Span::styled(format!("#{n}{tag}"), pr_style));
        match w.ci.as_str() {
            "SUCCESS" => spans.push(Span::styled(" ✓", Style::new().fg(Color::Green))),
            "FAILURE" => spans.push(Span::styled(" ✗", Style::new().fg(Color::Red))),
            "PENDING" => spans.push(Span::styled(" …", Style::new().fg(Color::Yellow))),
            _ => {}
        }
    }
    if let Some(h) = w.hints.iter().find(|h| matches!(h.kind, canopy_proto::HintKind::AheadBehind | canopy_proto::HintKind::Unpushed | canopy_proto::HintKind::Diverged)) {
        spans.push(Span::styled(format!(" · {}", h.message), dim));
    }
    let total: usize = spans.iter().map(|s| s.width()).sum();
    if total > width {
        // Drop the trailing git counts first; the port and PR matter more.
        spans.truncate(spans.len().saturating_sub(1).max(1));
    }
    Line::from(spans)
}

/// Caret for a row that can show its tabs: dim when there are none to show.
fn caret_span(expanded: bool, alive: bool) -> Span<'static> {
    let glyph = if expanded { "▾" } else { "▸" };
    let style = if alive { Style::new().fg(Color::Gray) } else { Style::new().fg(Color::DarkGray) };
    Span::styled(glyph, style)
}

fn trunc(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        s.to_string()
    } else {
        let mut t: String = s.chars().take(n.saturating_sub(1)).collect();
        t.push('…');
        t
    }
}

fn draw(f: &mut Frame, side: &mut Side) {
    let area = f.area();
    side.strip = is_collapsed_width(area.width);
    if side.strip {
        draw_collapsed(f, side, area);
        return;
    }
    let [header, body, footer] = Layout::vertical([Constraint::Length(3), Constraint::Min(1), Constraint::Length(1)]).areas(area);
    let width = area.width as usize;

    let working = side.rows.iter().filter(|r| r.agent_state == canopy_proto::AgentState::Working).count();
    let blocked = side.rows.iter().filter(|r| r.agent_state == canopy_proto::AgentState::Blocked).count();
    // Title bar: shaded full width, mark in its pill, name, attention counts right-aligned.
    let bar = Style::new().bg(Color::Rgb(28, 32, 40));
    let pill = format!(" {} ", crate::brand::glyph());
    let left: Vec<Span> = vec![
        Span::styled(" ", bar),
        Span::styled(pill.clone(), Style::new().fg(Color::Black).bg(crate::brand::ACCENT).add_modifier(Modifier::BOLD)),
        Span::styled(" canopy", bar.fg(Color::White).add_modifier(Modifier::BOLD)),
    ];
    let mut right: Vec<Span> = Vec::new();
    if blocked > 0 {
        right.push(Span::styled(format!("✋{blocked} "), bar.fg(Color::Yellow).add_modifier(Modifier::BOLD)));
    }
    if working > 0 {
        right.push(Span::styled(format!("⚡{working} "), bar.fg(Color::Cyan)));
    }
    let used: usize = left.iter().chain(right.iter()).map(|s| s.width()).sum();
    let mut title = left;
    title.push(Span::styled(" ".repeat(width.saturating_sub(used)), bar));
    title.extend(right);
    let rule = Line::from(Span::styled("─".repeat(width), Style::new().fg(Color::Rgb(50, 56, 66))));
    f.render_widget(Paragraph::new(vec![Line::from(""), Line::from(title), rule]), header);

    let nodes = side.nodes();
    let items: Vec<ListItem> = nodes
        .iter()
        .map(|n| match n {
            Node::Project { name, root, count, working, blocked } => {
                let arrow = if side.collapsed.contains(root) { "▶" } else { "▼" };
                let mut spans = vec![Span::styled(format!("{arrow} "), Style::new().fg(crate::brand::ACCENT).add_modifier(Modifier::BOLD)), Span::styled(trunc(name, width.saturating_sub(8)), Style::new().fg(Color::White).add_modifier(Modifier::BOLD))];
                if side.collapsed.contains(root) {
                    spans.push(Span::styled(format!(" {count}"), Style::new().fg(Color::DarkGray)));
                    if *blocked > 0 {
                        spans.push(Span::styled(" ✋", Style::new().fg(Color::Yellow)));
                    } else if *working > 0 {
                        spans.push(Span::styled(" ⚡", Style::new().fg(Color::Cyan)));
                    }
                }
                ListItem::new(Line::from(spans))
            }
            Node::Main { branch, alive, attached, id, root, .. } => {
                let current = side.current_id.as_deref() == Some(id);
                let glyph = match (alive, attached) { (true, true) => Span::styled("⊙", Style::new().fg(Color::Green)), (true, false) => Span::styled("●", Style::new().fg(Color::Green)), _ => Span::styled("○", Style::new().fg(Color::DarkGray)) };
                let st = if current { Style::new().fg(Color::Cyan).add_modifier(Modifier::BOLD) } else if *alive { Style::new() } else { Style::new().fg(Color::DarkGray) };
                let caret = caret_span(side.expanded.contains(id), *alive);
                let mut spans = vec![Span::raw(" "), caret, Span::raw(" "), glyph, Span::raw(" "), Span::styled("main", st.add_modifier(Modifier::ITALIC))];
                if !branch.is_empty() {
                    spans.push(Span::styled(format!(" {}", trunc(branch, width.saturating_sub(12))), Style::new().fg(Color::DarkGray)));
                }
                let port = side.projects.iter().find(|p| &p.root == root).map(|p| p.port_base).unwrap_or(0);
                ListItem::new(vec![Line::from(spans), Line::from(Span::styled(format!("       :{port}"), Style::new().fg(Color::Rgb(92, 99, 112))))])
            }
            Node::Workspace(w) => {
                let current = side.current_id.as_deref() == Some(&w.id);
                let name_style = if current { Style::new().fg(Color::Cyan).add_modifier(Modifier::BOLD) } else if w.status == Status::Broken || w.status == Status::Orphaned { Style::new().fg(Color::Red) } else if !w.alive { Style::new().fg(Color::DarkGray) } else { Style::new() };
                let label = if w.branch.is_empty() || w.branch == w.name { w.name.clone() } else { w.branch.clone() };
                let mut spans = vec![Span::raw(" "), caret_span(side.expanded.contains(&w.id), w.alive), Span::raw(" "), live_glyph(w), Span::raw(""), agent_badge(w), Span::raw(" "), Span::styled(trunc(&label, width.saturating_sub(11)), name_style)];
                if let Some(h) = w.hints.iter().find(|h| matches!(h.kind, canopy_proto::HintKind::Conflict | canopy_proto::HintKind::Rebasing | canopy_proto::HintKind::Merging | canopy_proto::HintKind::Shipped)) {
                    let (glyph, color) = match h.kind {
                        canopy_proto::HintKind::Shipped => ("✓", Color::Green),
                        _ => ("⚠", Color::Red),
                    };
                    spans.push(Span::styled(format!(" {glyph}"), Style::new().fg(color)));
                }
                ListItem::new(vec![Line::from(spans), detail_line(w, width)])
            }
            Node::Tab { win, .. } => {
                let st = if win.active { Style::new().fg(Color::White).add_modifier(Modifier::BOLD) } else { Style::new().fg(Color::DarkGray) };
                let mark = if win.active { "●" } else { "○" };
                ListItem::new(Line::from(vec![Span::raw("      "), Span::styled(format!("{mark} "), st), Span::styled(format!("{} {}", win.index, trunc(&win.name, width.saturating_sub(12))), st)]))
            }
            Node::NewTab { .. } => ListItem::new(Line::from(vec![Span::raw("      "), Span::styled("+ tab", Style::new().fg(Color::DarkGray))])),
        })
        .collect();
    let list = List::new(items).highlight_style(Style::new().bg(Color::Rgb(40, 44, 52))).block(Block::default());
    if nodes.is_empty() {
        f.render_widget(Paragraph::new("no workspaces\n\nn  new").style(Style::new().fg(Color::DarkGray)).wrap(Wrap { trim: true }), body.inner(ratatui::layout::Margin { horizontal: 1, vertical: 1 }));
    } else {
        f.render_stateful_widget(list, body, &mut side.list);
    }

    let foot = if side.status_at.elapsed() < Duration::from_secs(4) && !side.status.is_empty() {
        Line::from(Span::styled(format!(" {}", trunc(&side.status, width.saturating_sub(2))), Style::new().fg(Color::Yellow)))
    } else {
        Line::from(vec![Span::styled(" ⏎", Style::new().fg(Color::Cyan)), Span::raw(" go "), Span::styled("n", Style::new().fg(Color::Cyan)), Span::raw(" new "), Span::styled("t", Style::new().fg(Color::Cyan)), Span::raw(" tab "), Span::styled("q", Style::new().fg(Color::Cyan)), Span::raw(" hide "), Span::styled("?", Style::new().fg(Color::Cyan))])
    };
    f.render_widget(Paragraph::new(foot).style(Style::new().fg(Color::Gray)), footer);

    match &side.mode {
        Mode::Help => modal(f, area, " keys ", vec![
            kv("⏎", "select workspace / tab (stay here)"),
            kv("space", "fold project / workspace tabs"),
            kv("n", "new: fresh / PR / issue / branch"),
            kv("a", "add project (path or URL)"),
            kv("t", "new tab"),
            kv("d", "delete workspace"),
            kv("K", "kill session"),
            kv("r", "refresh"),
            kv("q", "leave: collapse to strip"),
            Line::from(""),
            kv("prefix+b", "enter / leave the sidebar"),
            kv("prefix+g", "full dashboard"),
        ]),
        Mode::ConfirmDelete => {
            let name = side.selected_workspace().map(|w| w.name).unwrap_or_default();
            modal(f, area, " delete ", vec![Line::from(format!("remove {name}?")), Line::from("worktree + branch are deleted"), Line::from(""), Line::from(vec![Span::styled("y", Style::new().fg(Color::Red).add_modifier(Modifier::BOLD)), Span::raw(" yes  "), Span::styled("n", Style::new().fg(Color::Cyan)), Span::raw(" no")])]);
        }
        Mode::ConfirmKill => {
            let name = side.selected_workspace().map(|w| w.name).unwrap_or_default();
            modal(f, area, " kill ", vec![Line::from(format!("kill session {name}?")), Line::from(""), Line::from(vec![Span::styled("y", Style::new().fg(Color::Yellow).add_modifier(Modifier::BOLD)), Span::raw(" yes  "), Span::styled("n", Style::new().fg(Color::Cyan)), Span::raw(" no")])]);
        }
        Mode::New(form) => {
            let mut lines = vec![source_tabs(form, true), Line::from("")];
            match form.source {
                Source::Fresh => {
                    let cur = |i: usize| if form.field == i { "▏" } else { "" };
                    lines.push(Line::from(vec![Span::styled("name   ", Style::new().fg(Color::DarkGray)), Span::raw(if form.name.is_empty() && form.field != 0 { "(random)".into() } else { form.name.clone() }), Span::styled(cur(0), Style::new().fg(Color::Cyan))]));
                    lines.push(Line::from(vec![Span::styled("prompt ", Style::new().fg(Color::DarkGray)), Span::raw(trunc(&form.prompt, width.saturating_sub(10))), Span::styled(cur(1), Style::new().fg(Color::Cyan))]));
                    lines.push(Line::from(vec![Span::styled("agent  ", Style::new().fg(Color::DarkGray)), Span::raw(form.agent.clone()), Span::styled(cur(2), Style::new().fg(Color::Cyan))]));
                }
                _ => {
                    lines.push(Line::from(vec![Span::styled("/", Style::new().fg(Color::DarkGray)), Span::raw(form.filter.clone()), Span::styled("▏", Style::new().fg(Color::Cyan))]));
                    if form.loading {
                        lines.push(Line::from(Span::styled("loading…", Style::new().fg(Color::DarkGray))));
                    } else if !form.error.is_empty() {
                        lines.push(Line::from(Span::styled(trunc(&form.error, width.saturating_sub(4)), Style::new().fg(Color::Red))));
                    } else {
                        let items = form.visible();
                        let max_rows = area.height.saturating_sub(10).max(3) as usize;
                        let start = form.sel.saturating_sub(max_rows.saturating_sub(1));
                        for (i, it) in items.iter().enumerate().skip(start).take(max_rows) {
                            let st = if i == form.sel { Style::new().add_modifier(Modifier::BOLD) } else if it.in_use { Style::new().fg(Color::Yellow) } else { Style::new() };
                            lines.push(Line::from(vec![Span::styled(if i == form.sel { "▶" } else { " " }, Style::new().fg(Color::Cyan)), Span::styled(trunc(&it.label, width.saturating_sub(5)), st)]));
                        }
                        if items.is_empty() {
                            lines.push(Line::from(Span::styled("nothing here", Style::new().fg(Color::DarkGray))));
                        }
                    }
                }
            }
            lines.push(Line::from(""));
            lines.push(Line::from(vec![Span::styled("⏎", Style::new().fg(Color::Cyan)), Span::raw(" create "), Span::styled("esc", Style::new().fg(Color::Cyan)), Span::raw(" cancel")]));
            modal(f, area, &format!(" new in {} ", trunc(&form.project_name, 14)), lines);
        }
        Mode::AddProject(text) => modal(f, area, " add project ", vec![Line::from("path or git URL"), Line::from(vec![Span::styled("▸ ", Style::new().fg(Color::Cyan)), Span::raw(trunc(text, width.saturating_sub(6))), Span::styled("▏", Style::new().fg(Color::Cyan))]), Line::from(""), Line::from(vec![Span::styled("⏎", Style::new().fg(Color::Cyan)), Span::raw(" add  "), Span::styled("esc", Style::new().fg(Color::Cyan)), Span::raw(" cancel")])]),
        Mode::ConfirmForget(_, name) => modal(f, area, " forget project ", vec![Line::from(format!("forget {name}? files stay")), Line::from(""), Line::from(vec![Span::styled("y", Style::new().fg(Color::Yellow).add_modifier(Modifier::BOLD)), Span::raw(" yes  "), Span::styled("n", Style::new().fg(Color::Cyan)), Span::raw(" no")])]),
        Mode::Busy(msg) => {
            const SPIN: &[&str] = &["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
            modal(f, area, "", vec![Line::from(format!("{} {msg}", SPIN[side.spinner % SPIN.len()]))]);
        }
        Mode::Error(e) => modal(f, area, " error ", vec![Line::from(Span::styled(e.clone(), Style::new().fg(Color::Red))), Line::from(""), Line::from("any key")]),
        Mode::Tree => {}
    }
}

/// The thin strip: a shaded column with the mark in its pill, attention badges, the name
/// running down the side, and an expand hint at the bottom.
fn draw_collapsed(f: &mut Frame, side: &Side, area: Rect) {
    let bg = Color::Rgb(36, 40, 48);
    let base = Style::new().bg(bg);
    let dim = base.fg(Color::Rgb(92, 99, 112));
    let working = side.rows.iter().filter(|r| r.agent_state == canopy_proto::AgentState::Working).count();
    let blocked = side.rows.iter().filter(|r| r.agent_state == canopy_proto::AgentState::Blocked).count();
    let blank = Line::from(Span::styled("   ", base));
    let mut lines: Vec<Line> = vec![
        Line::from(Span::styled(format!(" {} ", crate::brand::glyph()), Style::new().fg(Color::Black).bg(crate::brand::ACCENT).add_modifier(Modifier::BOLD))),
        blank.clone(),
    ];
    if blocked > 0 {
        lines.push(Line::from(Span::styled(format!("✋{}", blocked.min(9)), base.fg(Color::Yellow).add_modifier(Modifier::BOLD))));
    }
    if working > 0 {
        lines.push(Line::from(Span::styled(format!("⚡{}", working.min(9)), base.fg(Color::Cyan))));
    }
    if blocked > 0 || working > 0 {
        lines.push(blank.clone());
    }
    // Vertical name when there is room for it.
    if area.height as usize >= lines.len() + 6 + 3 {
        for c in "canopy".chars() {
            lines.push(Line::from(Span::styled(format!(" {c} "), dim)));
        }
    }
    while (lines.len() as u16) < area.height.saturating_sub(1) {
        lines.push(blank.clone());
    }
    lines.push(Line::from(Span::styled(" » ", base.fg(crate::brand::ACCENT).add_modifier(Modifier::BOLD))));
    f.render_widget(Paragraph::new(lines).style(base), area);
}

fn kv(k: &str, v: &str) -> Line<'static> {
    Line::from(vec![Span::styled(format!("{k:<9}"), Style::new().fg(Color::Cyan)), Span::raw(v.to_string())])
}

fn modal(f: &mut Frame, area: Rect, title: &str, lines: Vec<Line<'static>>) {
    let h = (lines.len() as u16 + 2).min(area.height.saturating_sub(2));
    let w = area.width;
    let rect = Rect { x: 0, y: (area.height.saturating_sub(h)) / 2, width: w, height: h };
    let inner = crate::tui::frame(f, rect, title);
    f.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }).style(Style::new().bg(crate::tui::PANEL_BG)), inner);
}
