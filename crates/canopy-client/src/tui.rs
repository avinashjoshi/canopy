//! The canopy dashboard. Holds no state of its own: it renders what the server says and
//! refreshes on server events (plus a slow safety tick).
//!
//! Keys: ↑/k ↓/j move · enter attach · n new · d delete · K kill session · R retry ·
//! i inspect · / filter · tab switch Local/All · r refresh · ? help · q quit

use crate::livelog::{self, Creating, LiveLog, LogChunk, Poll};
use crate::newform::{self, Action, NewForm, Source};
use crate::Transport;
use anyhow::Result;
use canopy_proto::{AgentState, AttachTarget, Method, ProjectRow, ResultBody, Response, WorkspaceRef, WorkspaceRow};
use canopy_core::state::Status;
use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use ratatui::layout::{Constraint, Layout, Rect};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span};
use ratatui::widgets::{Block, BorderType, Clear, List, ListItem, ListState, Paragraph, Wrap};
use ratatui::Frame;
use std::io::BufRead;
use std::collections::BTreeSet;
use std::path::{Path, PathBuf};
use std::sync::mpsc;
use std::time::{Duration, Instant};

pub enum Outcome {
    Quit,
    Attach(AttachTarget),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Tab {
    Local,
    All,
}

#[derive(Debug, Clone)]
enum Mode {
    List,
    Help,
    Inspect,
    Filter,
    ConfirmDelete,
    ConfirmKill,
    ConfirmRetry,
    New(Box<NewForm>),
    AddProject(String),
    /// Live setup log of one workspace (`L`, enter on a setting-up row, or after `R`).
    Log(Box<LiveLog>),
    /// A create in flight: shows the server's progress as it happens.
    Creating(Box<Creating>),
    Busy(String),
    Error(String),
}

#[allow(clippy::large_enum_variant)]
enum Bg {
    Refresh,
    Picks(Source, Result<Vec<canopy_proto::PickItem>, String>),
    Created(Result<WorkspaceRow, String>),
    Done(Result<String, String>),
    AttachReady(Result<AttachTarget, String>),
    Log(Result<LogChunk, String>),
    Found(Result<Option<WorkspaceRow>, String>),
}

/// One line of the dashboard list.
#[derive(Debug, Clone, PartialEq)]
#[allow(clippy::large_enum_variant)]
enum DashNode {
    Project { name: String, root: PathBuf, port_base: u16, count: usize, working: usize, blocked: usize },
    Row(WorkspaceRow),
}

impl DashNode {
    fn key(&self) -> String {
        match self {
            DashNode::Project { root, .. } => format!("p:{}", root.display()),
            DashNode::Row(w) => format!("w:{}", w.id),
        }
    }
}

struct Ui {
    transport: Transport,
    local_root: Option<PathBuf>,
    tab: Tab,
    /// Real workspaces plus one synthetic `(main)` row per project.
    rows: Vec<WorkspaceRow>,
    workspaces: Vec<WorkspaceRow>,
    projects: Vec<ProjectRow>,
    list: ListState,
    /// Identity of the selected line; re-found after every list rebuild.
    sel_key: Option<String>,
    /// Folded projects, shared with the sidebar.
    collapsed: BTreeSet<PathBuf>,
    shared_path: PathBuf,
    mode: Mode,
    filter: String,
    status: String,
    status_at: Instant,
    popup: bool,
    last_refresh: Instant,
    spinner: usize,
}

impl Ui {
    /// Synthetic `main` row per project: the repo root session.
    fn main_rows(&self) -> Vec<WorkspaceRow> {
        self.projects
            .iter()
            .map(|p| WorkspaceRow {
                id: format!("main:{}", p.name),
                project: p.name.clone(),
                project_root: p.root.clone(),
                name: "main".into(),
                branch: p.main_branch.clone(),
                path: p.root.clone(),
                port: p.port_base,
                status: if p.main_alive { Status::Ready } else { Status::Stopped },
                session: p.main_session.clone(),
                alive: p.main_alive,
                attached: p.main_attached,
                agent: String::new(),
                agent_state: AgentState::Unknown,
                source_kind: canopy_core::state::SourceKind::Fresh,
                owner: String::new(),
                hints: Vec::new(),
                last_error_hint: String::new(),
                mem_rss_bytes: 0,
                cpu_percent: 0,
                host: self.transport.host_label(),
                pr_number: None,
                pr_state: String::new(),
                ci: String::new(),
                progress: String::new(),
            })
            .collect()
    }

    fn in_scope(&self, root: &Path) -> bool {
        match (self.tab, &self.local_root) {
            (Tab::Local, Some(r)) => root == r,
            _ => true,
        }
    }

    fn matches(&self, w: &WorkspaceRow) -> bool {
        self.filter.is_empty() || fuzzy(&self.filter, &format!("{} {} {}", w.name, w.branch, w.project))
    }

    /// Projects as headers, `main` first under each, then workspaces; folded projects show
    /// only their header. With a filter active, only matching rows (and their headers) show.
    fn nodes(&self) -> Vec<DashNode> {
        let mut projects: Vec<&ProjectRow> = self.projects.iter().filter(|p| self.in_scope(&p.root)).collect();
        projects.sort_by(|a, b| a.name.cmp(&b.name));
        let mut out = Vec::new();
        for p in projects {
            let mut rows: Vec<&WorkspaceRow> = self.rows.iter().filter(|r| r.project_root == p.root && self.matches(r)).collect();
            rows.sort_by(|a, b| b.id.starts_with("main:").cmp(&a.id.starts_with("main:")).then(a.name.cmp(&b.name)));
            if !self.filter.is_empty() && rows.iter().all(|r| r.id.starts_with("main:")) && !fuzzy(&self.filter, &p.name) {
                continue;
            }
            let real: Vec<&&WorkspaceRow> = rows.iter().filter(|r| !r.id.starts_with("main:")).collect();
            out.push(DashNode::Project {
                name: p.name.clone(),
                root: p.root.clone(),
                port_base: p.port_base,
                count: p.workspace_count,
                working: real.iter().filter(|r| r.agent_state == AgentState::Working).count(),
                blocked: real.iter().filter(|r| r.agent_state == AgentState::Blocked).count(),
            });
            if self.collapsed.contains(&p.root) && self.filter.is_empty() {
                continue;
            }
            for r in rows {
                out.push(DashNode::Row(r.clone()));
            }
        }
        out
    }

    fn selected_node(&self) -> Option<DashNode> {
        let n = self.nodes();
        self.list.selected().and_then(|i| n.get(i).cloned())
    }

    fn selected(&self) -> Option<WorkspaceRow> {
        match self.selected_node()? {
            DashNode::Row(w) => Some(w),
            DashNode::Project { .. } => None,
        }
    }

    fn selected_project(&self) -> Option<(PathBuf, String)> {
        match self.selected_node() {
            Some(DashNode::Project { root, name, .. }) => Some((root, name)),
            Some(DashNode::Row(w)) => Some((w.project_root.clone(), w.project.clone())),
            None => None,
        }
    }

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
        self.sel_key = Some(nodes[i].key());
    }

    fn toggle_fold(&mut self, root: &Path) {
        if !self.collapsed.remove(root) {
            self.collapsed.insert(root.to_path_buf());
        }
        let mut shared = crate::shared::SharedUi::load(&self.shared_path);
        shared.collapsed = self.collapsed.clone();
        shared.save(&self.shared_path);
        self.sel_key = Some(format!("p:{}", root.display()));
        self.clamp();
    }

    /// Keep the cursor on a real row while typing: the first match.
    fn after_filter_change(&mut self) {
        let nodes = self.nodes();
        if let Some(i) = nodes.iter().position(|n| matches!(n, DashNode::Row(_))) {
            self.list.select(Some(i));
            self.sel_key = Some(nodes[i].key());
        } else {
            self.clamp();
        }
    }

    fn note(&mut self, s: impl Into<String>) {
        self.status = s.into();
        self.status_at = Instant::now();
    }

    fn refresh(&mut self) {
        match self.transport.call(Method::WorkspaceList { project_root: None }) {
            Ok(ResultBody::WorkspaceList { mut workspaces }) => {
                let host = self.transport.host_label();
                for w in &mut workspaces {
                    w.host = host.clone();
                }
                self.workspaces = workspaces;
            }
            Ok(_) => {}
            Err(e) => self.note(format!("refresh failed: {e}")),
        }
        if let Ok(ResultBody::ProjectList { projects }) = self.transport.call(Method::ProjectList) {
            self.projects = projects;
        }
        if !self.transport.is_remote() {
            self.collapsed = crate::shared::SharedUi::load(&self.shared_path).collapsed;
        }
        let mut rows = self.main_rows();
        rows.extend(self.workspaces.iter().cloned());
        self.rows = rows;
        self.last_refresh = Instant::now();
        self.clamp();
    }
}

fn fuzzy(needle: &str, hay: &str) -> bool {
    let hay = hay.to_lowercase();
    let mut it = hay.chars();
    needle.to_lowercase().chars().all(|c| it.any(|h| h == c))
}

pub fn run(transport: Transport, local_root: Option<PathBuf>) -> Result<Outcome> {
    let (tx, rx) = mpsc::channel::<Bg>();
    // Event subscription thread: any server event triggers a refresh.
    {
        let t = transport.clone();
        let tx = tx.clone();
        std::thread::spawn(move || loop {
            if let Ok((reader, guard)) = t.subscribe() {
                for line in reader.lines() {
                    let Ok(line) = line else { break };
                    if let Ok(Response::Ok { result: ResultBody::Event { .. }, .. }) = serde_json::from_str::<Response>(&line) {
                        if tx.send(Bg::Refresh).is_err() {
                            if let Some(mut c) = guard { let _ = c.kill(); }
                            return;
                        }
                    }
                }
                if let Some(mut c) = guard { let _ = c.kill(); }
            }
            std::thread::sleep(Duration::from_secs(1));
        });
    }

    let mut ui = Ui {
        transport,
        local_root: local_root.clone(),
        tab: if local_root.is_some() { Tab::Local } else { Tab::All },
        rows: vec![],
        workspaces: vec![],
        projects: vec![],
        list: ListState::default(),
        sel_key: None,
        collapsed: BTreeSet::new(),
        shared_path: crate::shared::SharedUi::path(&canopy_core::paths::Paths::from_env().home),
        mode: Mode::List,
        filter: String::new(),
        status: String::new(),
        status_at: Instant::now(),
        popup: std::env::var_os("CANOPY_IN_POPUP").is_some(),
        last_refresh: Instant::now(),
        spinner: 0,
    };
    ui.refresh();
    if ui.list.selected().is_none() {
        ui.clamp();
    }

    let mut terminal = ratatui::init();
    let result = event_loop(&mut terminal, &mut ui, &tx, &rx);
    ratatui::restore();
    result
}

fn event_loop(terminal: &mut ratatui::DefaultTerminal, ui: &mut Ui, tx: &mpsc::Sender<Bg>, rx: &mpsc::Receiver<Bg>) -> Result<Outcome> {
    loop {
        terminal.draw(|f| draw(f, ui))?;
        // Background results.
        while let Ok(msg) = rx.try_recv() {
            match msg {
                Bg::Refresh => ui.refresh(),
                Bg::Picks(source, items) => {
                    if let Mode::New(form) = &mut ui.mode {
                        form.set_items(source, items);
                    }
                }
                Bg::Created(Ok(w)) => {
                    ui.mode = Mode::List;
                    ui.note(format!("created {} ({})", w.name, w.status.as_str()));
                    ui.sel_key = Some(DashNode::Row(w).key());
                    ui.refresh();
                }
                Bg::Created(Err(e)) => {
                    // If the log view is already up, keep it: the FAILED marker and the
                    // script's own output say more than the error text alone.
                    match std::mem::replace(&mut ui.mode, Mode::List) {
                        Mode::Creating(c) if c.log.is_some() => {
                            ui.mode = Mode::Log(Box::new(c.log.clone().unwrap_or_else(|| unreachable!())));
                            ui.note(first_line(&e));
                        }
                        _ => ui.mode = Mode::Error(e),
                    }
                    ui.refresh();
                }
                Bg::Done(Ok(s)) => {
                    // A retry watched in the log view stays there: the title flips to ✓.
                    if !matches!(ui.mode, Mode::Log(_)) {
                        ui.mode = Mode::List;
                    }
                    ui.note(s);
                    ui.refresh();
                }
                Bg::Log(res) => match &mut ui.mode {
                    Mode::Log(l) => l.apply(res),
                    Mode::Creating(c) => c.on_log(res),
                    _ => {}
                },
                Bg::Found(res) => {
                    if let Mode::Creating(c) = &mut ui.mode {
                        c.on_found(res);
                    }
                }
                Bg::Done(Err(e)) => {
                    if matches!(ui.mode, Mode::Log(_)) {
                        ui.note(first_line(&e));
                        ui.refresh();
                    } else {
                        ui.mode = Mode::Error(e);
                    }
                }
                Bg::AttachReady(Ok(target)) => {
                    if ui.popup {
                        if let AttachTarget::Tmux { session, .. } = &target {
                            let _ = std::process::Command::new("tmux").args(["switch-client", "-t", &format!("={session}")]).status();
                            return Ok(Outcome::Quit);
                        }
                    }
                    return Ok(Outcome::Attach(target));
                }
                Bg::AttachReady(Err(e)) => ui.mode = Mode::Error(e),
            }
        }
        if ui.last_refresh.elapsed() > Duration::from_secs(5) {
            ui.refresh();
        }
        tick_live(ui, tx);
        if event::poll(Duration::from_millis(120))? {
            if let Event::Key(key) = event::read()? {
                if key.kind != KeyEventKind::Press {
                    continue;
                }
                if let Some(out) = handle_key(ui, key, tx) {
                    return Ok(out);
                }
            }
        }
        ui.spinner = ui.spinner.wrapping_add(1);
    }
}

/// Drive the live log / create views: one request in flight at a time, rate limited.
fn tick_live(ui: &mut Ui, tx: &mpsc::Sender<Bg>) {
    let poll = match &mut ui.mode {
        Mode::Log(l) => l.take_due().map(|(target, offset)| Poll::Fetch { target, offset }),
        Mode::Creating(c) => Some(c.next_poll()),
        _ => None,
    };
    match poll {
        Some(Poll::Fetch { target, offset }) => spawn_bg(ui, tx, move |t| Bg::Log(livelog::fetch(t, &target, offset, Some(200)))),
        Some(Poll::Find { root, known, name }) => spawn_bg(ui, tx, move |t| Bg::Found(livelog::find_new(t, &root, &known, name.as_deref()))),
        _ => {}
    }
}

fn spawn_bg(ui: &Ui, tx: &mpsc::Sender<Bg>, f: impl FnOnce(&Transport) -> Bg + Send + 'static) {
    let t = ui.transport.clone();
    let tx = tx.clone();
    std::thread::spawn(move || {
        let _ = tx.send(f(&t));
    });
}

/// Log view for a row: while setting up, just this run; otherwise the whole recent tail.
fn log_view_for(w: &WorkspaceRow) -> LiveLog {
    let l = LiveLog::new(WorkspaceRef::Id { id: w.id.clone() }, w.name.clone());
    if w.status == Status::SettingUp {
        l
    } else {
        l.whole_tail()
    }
}

fn log_scroll_key(l: &mut LiveLog, code: KeyCode) {
    match code {
        KeyCode::Up | KeyCode::Char('k') => l.scroll_by(-1, 20),
        KeyCode::Down | KeyCode::Char('j') => l.scroll_by(1, 20),
        KeyCode::PageUp => l.scroll_by(-20, 20),
        KeyCode::PageDown => l.scroll_by(20, 20),
        KeyCode::Char('g') | KeyCode::Home => l.scroll_home(),
        KeyCode::Char('G') | KeyCode::End => l.scroll_end(),
        _ => {}
    }
}

fn handle_key(ui: &mut Ui, key: KeyEvent, tx: &mpsc::Sender<Bg>) -> Option<Outcome> {
    let ctrl_c = key.code == KeyCode::Char('c') && key.modifiers.contains(KeyModifiers::CONTROL);
    match ui.mode.clone() {
        Mode::Busy(_) => {
            if ctrl_c {
                return Some(Outcome::Quit);
            }
        }
        Mode::Error(_) | Mode::Help | Mode::Inspect => {
            if ctrl_c {
                return Some(Outcome::Quit);
            }
            ui.mode = Mode::List;
        }
        Mode::Log(mut l) => match key.code {
            _ if ctrl_c => return Some(Outcome::Quit),
            KeyCode::Esc | KeyCode::Char('q') | KeyCode::Char('L') => {
                ui.mode = Mode::List;
                ui.refresh();
            }
            KeyCode::Enter if l.finished_ok() => {
                let r = l.target.clone();
                ui.mode = Mode::Busy(format!("opening {}…", l.name));
                spawn_bg(ui, tx, move |s| match s.call(Method::WorkspaceAttachTarget { workspace: r }) {
                    Ok(ResultBody::AttachTarget { target }) => Bg::AttachReady(Ok(target)),
                    Ok(_) => Bg::AttachReady(Err("unexpected response".into())),
                    Err(e) => Bg::AttachReady(Err(e.to_string())),
                });
            }
            _ => {
                log_scroll_key(&mut l, key.code);
                ui.mode = Mode::Log(l);
            }
        },
        Mode::Creating(mut c) => match key.code {
            _ if ctrl_c => return Some(Outcome::Quit),
            KeyCode::Esc | KeyCode::Char('q') => {
                ui.mode = Mode::List;
                ui.note("creation continues in the background; press L on the row to follow it");
                ui.refresh();
            }
            _ => {
                if let Some(l) = c.log.as_mut() {
                    log_scroll_key(l, key.code);
                }
                ui.mode = Mode::Creating(c);
            }
        },
        Mode::Filter => match key.code {
            KeyCode::Esc => {
                ui.filter.clear();
                ui.mode = Mode::List;
                ui.clamp();
            }
            _ if ctrl_c => return Some(Outcome::Quit),
            KeyCode::Down => ui.mv(1),
            KeyCode::Up => ui.mv(-1),
            KeyCode::Enter => {
                // Attach to the highlighted match straight from the filter.
                ui.mode = Mode::List;
                if ui.selected().is_none() {
                    ui.mv(1);
                }
                return handle_key(ui, KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), tx);
            }
            KeyCode::Backspace => {
                ui.filter.pop();
                ui.after_filter_change();
            }
            KeyCode::Char(c) => {
                ui.filter.push(c);
                ui.after_filter_change();
            }
            _ => {}
        },
        Mode::ConfirmDelete | Mode::ConfirmKill | Mode::ConfirmRetry => {
            let yes = matches!(key.code, KeyCode::Char('y') | KeyCode::Char('Y'));
            let mode = ui.mode.clone();
            ui.mode = Mode::List;
            if !yes {
                return None;
            }
            let w = ui.selected()?;
            let r = WorkspaceRef::Id { id: w.id.clone() };
            let name = w.name.clone();
            match mode {
                Mode::ConfirmDelete => {
                    ui.mode = Mode::Busy(format!("removing {name}…"));
                    spawn_bg(ui, tx, move |s| Bg::Done(s.call(Method::WorkspaceRemove { workspace: r, force: true }).map(|_| format!("removed {name}")).map_err(|e| e.to_string())));
                }
                Mode::ConfirmKill if w.id.starts_with("main:") => {
                    let root = w.project_root.clone();
                    ui.mode = Mode::Busy(format!("stopping {} main…", w.project));
                    spawn_bg(ui, tx, move |s| Bg::Done(s.call(Method::ProjectStopMain { root }).map(|_| format!("stopped {name}")).map_err(|e| e.to_string())));
                }
                Mode::ConfirmKill => {
                    ui.mode = Mode::Busy(format!("stopping {name}…"));
                    spawn_bg(ui, tx, move |s| Bg::Done(s.call(Method::WorkspaceStop { workspace: r }).map(|_| format!("stopped {name}")).map_err(|e| e.to_string())));
                }
                Mode::ConfirmRetry => {
                    // Watch it happen: the log view polls while the retry runs.
                    ui.mode = Mode::Log(Box::new(LiveLog::new(r.clone(), name.clone())));
                    spawn_bg(ui, tx, move |s| Bg::Done(s.call(Method::WorkspaceRetry { workspace: r, force: true }).map(|_| format!("setup finished for {name}")).map_err(|e| e.to_string())));
                }
                _ => {}
            }
        }
        Mode::New(mut form) => match form.handle_key(key) {
            Action::Cancel => ui.mode = Mode::List,
            Action::None => ui.mode = Mode::New(form),
            Action::Load(source) => {
                let root = form.project_root.clone();
                ui.mode = Mode::New(form);
                spawn_bg(ui, tx, move |t| Bg::Picks(source, newform::load(t, &root, source)));
            }
            Action::Submit(req) => {
                let known: BTreeSet<String> = ui.workspaces.iter().map(|w| w.id.clone()).collect();
                ui.mode = Mode::Creating(Box::new(Creating::new(req.project_root.clone(), req.name.clone(), known)));
                spawn_bg(ui, tx, move |s| match s.call(Method::WorkspaceCreate(req)) {
                    Ok(ResultBody::Workspace { workspace }) => Bg::Created(Ok(workspace)),
                    Ok(_) => Bg::Created(Err("unexpected response".into())),
                    Err(e) => Bg::Created(Err(e.to_string())),
                });
            }
        },
        Mode::AddProject(mut text) => match key.code {
            KeyCode::Esc => ui.mode = Mode::List,
            _ if ctrl_c => return Some(Outcome::Quit),
            KeyCode::Backspace => {
                text.pop();
                ui.mode = Mode::AddProject(text);
            }
            KeyCode::Enter => {
                let v = text.trim().to_string();
                if v.is_empty() {
                    return None;
                }
                let is_url = v.contains("://") || (v.contains('@') && v.contains(':'));
                let (path, url) = if is_url { (None, Some(v.clone())) } else { (Some(PathBuf::from(shellexpand_home(&v))), None) };
                ui.mode = Mode::Busy(if is_url { format!("cloning {v}…") } else { format!("adding {v}…") });
                spawn_bg(ui, tx, move |t| Bg::Done(match t.call(Method::ProjectInit { path, url, with_scripts: true, adopt_from: None }) {
                    Ok(ResultBody::ProjectList { projects }) => Ok(format!("added project {}", projects.first().map(|p| p.name.clone()).unwrap_or_default())),
                    Ok(_) => Err("unexpected response".into()),
                    Err(e) => Err(e.to_string()),
                }));
            }
            KeyCode::Char(c) => {
                text.push(c);
                ui.mode = Mode::AddProject(text);
            }
            _ => {}
        },
        Mode::List => match key.code {
            KeyCode::Char('q') | KeyCode::Esc => return Some(Outcome::Quit),
            _ if ctrl_c => return Some(Outcome::Quit),
            KeyCode::Enter if matches!(ui.selected_node(), Some(DashNode::Project { .. })) => {
                if let Some(DashNode::Project { root, .. }) = ui.selected_node() {
                    ui.toggle_fold(&root);
                }
            }
            KeyCode::Char(' ') | KeyCode::Left | KeyCode::Right if matches!(ui.selected_node(), Some(DashNode::Project { .. })) => {
                if let Some(DashNode::Project { root, .. }) = ui.selected_node() {
                    ui.toggle_fold(&root);
                }
            }
            KeyCode::Char('d') if matches!(ui.selected_node(), Some(DashNode::Project { count: 0, .. })) => {
                if let Some(DashNode::Project { root, name, .. }) = ui.selected_node() {
                    ui.mode = Mode::Busy(format!("forgetting {name}…"));
                    spawn_bg(ui, tx, move |t| Bg::Done(t.call(Method::ProjectRemove { root }).map(|_| format!("forgot {name}")).map_err(|e| e.to_string())));
                }
            }
            KeyCode::Char('d') if matches!(ui.selected_node(), Some(DashNode::Project { .. })) => {
                ui.note("remove its workspaces first");
            }
            KeyCode::Char('j') | KeyCode::Down => ui.mv(1),
            KeyCode::Char('k') | KeyCode::Up => ui.mv(-1),
            KeyCode::Char('g') | KeyCode::Home => ui.mv(-1_000_000),
            KeyCode::Char('G') | KeyCode::End => ui.mv(1_000_000),
            KeyCode::Tab | KeyCode::Left | KeyCode::Right | KeyCode::Char('h') | KeyCode::Char('l') => {
                if ui.local_root.is_some() {
                    ui.tab = if ui.tab == Tab::Local { Tab::All } else { Tab::Local };
                    ui.clamp();
                }
            }
            KeyCode::Char('r') => {
                ui.refresh();
                ui.note("refreshed");
            }
            KeyCode::Char('?') => ui.mode = Mode::Help,
            KeyCode::Char('/') => ui.mode = Mode::Filter,
            KeyCode::Char('L') => {
                if let Some(w) = ui.selected() {
                    if w.id.starts_with("main:") {
                        ui.note("(main) is the repo root; it has no setup log");
                    } else {
                        ui.mode = Mode::Log(Box::new(log_view_for(&w)));
                    }
                }
            }
            KeyCode::Enter if ui.selected().is_some_and(|w| w.status == Status::SettingUp) => {
                if let Some(w) = ui.selected() {
                    ui.mode = Mode::Log(Box::new(log_view_for(&w)));
                }
            }
            KeyCode::Char('i') => {
                if ui.selected().is_some() {
                    ui.mode = Mode::Inspect;
                }
            }
            KeyCode::Char('n') => {
                let ctx = ui.selected_project()
                    .or_else(|| ui.local_root.clone().map(|r| (r.clone(), r.file_name().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default())))
                    .or_else(|| ui.projects.first().map(|p| (p.root.clone(), p.name.clone())));
                match ctx {
                    Some((root, name)) => ui.mode = Mode::New(Box::new(NewForm::new(root, name))),
                    None => ui.mode = Mode::AddProject(String::new()),
                }
            }
            KeyCode::Char('a') => ui.mode = Mode::AddProject(String::new()),
            KeyCode::Char('d') | KeyCode::Char('R') if ui.selected().is_some_and(|w| w.id.starts_with("main:")) => {
                ui.note("(main) is the repo root; it has no setup to retry and nothing to delete");
            }
            KeyCode::Char('d') => {
                if ui.selected().is_some() {
                    ui.mode = Mode::ConfirmDelete;
                }
            }
            KeyCode::Char('K') => {
                if ui.selected().is_some_and(|w| w.alive) {
                    ui.mode = Mode::ConfirmKill;
                }
            }
            KeyCode::Char('R') => {
                if ui.selected().is_some() {
                    ui.mode = Mode::ConfirmRetry;
                }
            }
            KeyCode::Enter if ui.selected().is_some_and(|w| w.id.starts_with("main:")) => {
                if let Some(w) = ui.selected() {
                    let root = w.project_root.clone();
                    ui.mode = Mode::Busy(format!("opening {} main…", w.project));
                    spawn_bg(ui, tx, move |s| match s.call(Method::ProjectMain { root }) {
                        Ok(ResultBody::AttachTarget { target }) => Bg::AttachReady(Ok(target)),
                        Ok(_) => Bg::AttachReady(Err("unexpected response".into())),
                        Err(e) => Bg::AttachReady(Err(e.to_string())),
                    });
                }
            }
            KeyCode::Enter => {
                if let Some(w) = ui.selected() {
                    let r = WorkspaceRef::Id { id: w.id.clone() };
                    ui.mode = Mode::Busy(if w.alive { format!("attaching {}…", w.name) } else { format!("resurrecting {}…", w.name) });
                    spawn_bg(ui, tx, move |s| match s.call(Method::WorkspaceAttachTarget { workspace: r }) {
                        Ok(ResultBody::AttachTarget { target }) => Bg::AttachReady(Ok(target)),
                        Ok(_) => Bg::AttachReady(Err("unexpected response".into())),
                        Err(e) => Bg::AttachReady(Err(e.to_string())),
                    });
                }
            }
            _ => {}
        },
    }
    None
}

// ---------------------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------------------

const SPIN: &[&str] = &["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];

pub(crate) fn agent_badge(w: &WorkspaceRow) -> Span<'static> {
    match w.agent_state {
        AgentState::Working => Span::styled("⚡", Style::new().fg(Color::Cyan)),
        AgentState::Idle => Span::styled("💤", Style::new().fg(Color::DarkGray)),
        AgentState::Blocked => Span::styled("✋", Style::new().fg(Color::Yellow).add_modifier(Modifier::BOLD)),
        AgentState::Done => Span::styled("✓", Style::new().fg(Color::Green)),
        AgentState::Unknown => Span::styled(if w.alive { "·" } else { " " }, Style::new().fg(Color::DarkGray)),
    }
}

pub(crate) fn live_glyph(w: &WorkspaceRow) -> Span<'static> {
    match (w.alive, w.attached) {
        (true, true) => Span::styled("⊙", Style::new().fg(Color::Green)),
        (true, false) => Span::styled("●", Style::new().fg(Color::Green)),
        _ => Span::styled("○", Style::new().fg(Color::DarkGray)),
    }
}


fn hints_line(w: &WorkspaceRow) -> Line<'static> {
    let mut spans = Vec::new();
    for h in &w.hints {
        let style = match h.kind {
            canopy_proto::HintKind::Conflict | canopy_proto::HintKind::Rebasing | canopy_proto::HintKind::Merging | canopy_proto::HintKind::CherryPicking | canopy_proto::HintKind::Detached => Style::new().fg(Color::Red),
            canopy_proto::HintKind::Shipped => Style::new().fg(Color::Green),
            canopy_proto::HintKind::RenameSuggested => Style::new().fg(Color::Magenta),
            canopy_proto::HintKind::PrStatus => Style::new().fg(Color::Blue),
            _ => Style::new().fg(Color::Yellow),
        };
        spans.push(Span::styled(h.message.clone(), style));
        spans.push(Span::raw("  "));
    }
    if !w.last_error_hint.is_empty() {
        spans.push(Span::styled(w.last_error_hint.clone(), Style::new().fg(Color::Red)));
    }
    Line::from(spans)
}

fn draw(f: &mut Frame, ui: &mut Ui) {
    let area = f.area();
    let [header, filter_a, body, footer] = Layout::vertical([Constraint::Length(2), Constraint::Length(1), Constraint::Min(1), Constraint::Length(2)]).areas(area);
    let width = area.width as usize;

    // ---- title bar ----
    let bar = Style::new().bg(Color::Rgb(28, 32, 40));
    let mut left = vec![
        Span::styled(" ", bar),
        Span::styled(format!(" {} ", crate::brand::glyph()), Style::new().fg(Color::Black).bg(crate::brand::ACCENT).add_modifier(Modifier::BOLD)),
        Span::styled(" canopy", bar.fg(Color::White).add_modifier(Modifier::BOLD)),
    ];
    if ui.transport.is_remote() {
        left.push(Span::styled(format!("  @{}", ui.transport.host_label()), bar.fg(Color::Yellow).add_modifier(Modifier::BOLD)));
    }
    if ui.local_root.is_some() {
        let local = ui.local_root.as_ref().and_then(|r| r.file_name()).map(|s| s.to_string_lossy().into_owned()).unwrap_or_default();
        for (t, label) in [(Tab::Local, local), (Tab::All, "all".to_string())] {
            let st = if ui.tab == t { bar.fg(crate::brand::ACCENT).add_modifier(Modifier::BOLD | Modifier::UNDERLINED) } else { bar.fg(Color::Rgb(120, 128, 140)) };
            left.push(Span::styled(format!("   {label}"), st));
        }
    }
    let working = ui.workspaces.iter().filter(|r| r.agent_state == AgentState::Working).count();
    let blocked = ui.workspaces.iter().filter(|r| r.agent_state == AgentState::Blocked).count();
    let mut right: Vec<Span> = Vec::new();
    if blocked > 0 {
        right.push(Span::styled(format!("✋{blocked}  "), bar.fg(Color::Yellow).add_modifier(Modifier::BOLD)));
    }
    if working > 0 {
        right.push(Span::styled(format!("⚡{working}  "), bar.fg(Color::Cyan)));
    }
    right.push(Span::styled(
        format!("{} project{} · {} workspace{} · v{} ", ui.projects.len(), if ui.projects.len() == 1 { "" } else { "s" }, ui.workspaces.len(), if ui.workspaces.len() == 1 { "" } else { "s" }, env!("CARGO_PKG_VERSION")),
        bar.fg(Color::Rgb(120, 128, 140)),
    ));
    let used: usize = left.iter().chain(right.iter()).map(|s| s.width()).sum();
    let mut title = left;
    title.push(Span::styled(" ".repeat(width.saturating_sub(used)), bar));
    title.extend(right);
    f.render_widget(Paragraph::new(vec![Line::from(title), rule(area.width)]), header);

    // ---- filter field (always visible) ----
    let active = matches!(ui.mode, Mode::Filter);
    let n_rows = ui.nodes().iter().filter(|n| matches!(n, DashNode::Row(_))).count();
    let mut fl = vec![Span::styled("  / ", Style::new().fg(if active { crate::brand::ACCENT } else { Color::Rgb(120, 128, 140) }).add_modifier(Modifier::BOLD))];
    if ui.filter.is_empty() && !active {
        fl.push(Span::styled("filter workspaces by name, branch or project", Style::new().fg(Color::Rgb(92, 99, 112)).add_modifier(Modifier::ITALIC)));
    } else {
        fl.push(Span::styled(ui.filter.clone(), Style::new().fg(Color::White)));
        if active {
            fl.push(Span::styled("▏", Style::new().fg(crate::brand::ACCENT)));
        }
    }
    if !ui.filter.is_empty() {
        let tail = format!("{n_rows} match{}  esc clears ", if n_rows == 1 { "" } else { "es" });
        let used: usize = fl.iter().map(|s| s.width()).sum();
        fl.push(Span::raw(" ".repeat(width.saturating_sub(used + tail.len()))));
        fl.push(Span::styled(tail, Style::new().fg(Color::Rgb(120, 128, 140))));
    }
    f.render_widget(Paragraph::new(Line::from(fl)), filter_a);

    // ---- body ----
    if ui.projects.is_empty() && ui.rows.is_empty() {
        draw_welcome(f, body, ui);
    } else {
        let nodes = ui.nodes();
        let cols = Columns::for_width(width);
        let items: Vec<ListItem> = nodes.iter().map(|n| ListItem::new(render_node(n, &cols, ui))).collect();
        if items.is_empty() {
            f.render_widget(Paragraph::new("  nothing matches the filter").style(Style::new().fg(Color::DarkGray)), body);
        } else {
            let list = List::new(items).highlight_style(Style::new().bg(ROW_HL));
            f.render_stateful_widget(list, body, &mut ui.list);
        }
    }

    // ---- footer ----
    let foot = if ui.status_at.elapsed() < Duration::from_secs(4) && !ui.status.is_empty() {
        Line::from(Span::styled(format!(" {}", ui.status), Style::new().fg(Color::Yellow)))
    } else {
        let mut h = if area.width >= 150 {
            hints(&[("⏎", "attach"), ("n", "new"), ("a", "add project"), ("d", "delete"), ("K", "kill"), ("R", "retry"), ("L", "log"), ("i", "inspect"), ("/", "filter"), ("?", "help"), ("q", "quit")])
        } else {
            hints(&[("⏎", "attach"), ("n", "new"), ("a", "add"), ("d", "delete"), ("/", "filter"), ("?", "help"), ("q", "quit")])
        };
        h.spans.insert(0, Span::raw(" "));
        h
    };
    f.render_widget(Paragraph::new(vec![rule(area.width), foot]), footer);

    // ---- dialogs ----
    draw_dialogs(f, area, ui);
}

struct Columns {
    name: usize,
    branch: usize,
    status: usize,
    port: usize,
}

impl Columns {
    fn for_width(w: usize) -> Self {
        if w >= 120 {
            Self { name: 24, branch: 32, status: 13, port: 7 }
        } else if w >= 100 {
            Self { name: 20, branch: 26, status: 13, port: 7 }
        } else {
            Self { name: 18, branch: 18, status: 10, port: 7 }
        }
    }
}

fn pad(s: &str, n: usize) -> String {
    let t = fit(s, n);
    let len = t.chars().count();
    format!("{t}{}", " ".repeat(n.saturating_sub(len)))
}

fn render_node(n: &DashNode, c: &Columns, ui: &Ui) -> Line<'static> {
    match n {
        DashNode::Project { name, root, port_base, count, working, blocked } => {
            let folded = ui.collapsed.contains(root);
            let arrow = if folded { "▶" } else { "▼" };
            let home = std::env::var("HOME").unwrap_or_default();
            let path = root.display().to_string();
            let path = path.strip_prefix(&home).map(|r| format!("~{r}")).unwrap_or(path);
            let mut spans = vec![
                Span::styled(format!(" {arrow} "), Style::new().fg(crate::brand::ACCENT).add_modifier(Modifier::BOLD)),
                Span::styled(name.clone(), Style::new().fg(Color::White).add_modifier(Modifier::BOLD)),
                Span::styled(format!("   {path} · :{port_base}"), Style::new().fg(Color::Rgb(120, 128, 140))),
            ];
            if folded || *count > 0 {
                spans.push(Span::styled(format!("   {count} workspace{}", if *count == 1 { "" } else { "s" }), Style::new().fg(Color::Rgb(120, 128, 140))));
            }
            if *blocked > 0 {
                spans.push(Span::styled(format!("  ✋{blocked}"), Style::new().fg(Color::Yellow)));
            }
            if *working > 0 {
                spans.push(Span::styled(format!("  ⚡{working}"), Style::new().fg(Color::Cyan)));
            }
            Line::from(spans)
        }
        DashNode::Row(w) => {
            let is_main = w.id.starts_with("main:");
            let name = if is_main { "main".to_string() } else if w.owner.is_empty() { w.name.clone() } else { format!("{} @{}", w.name, w.owner) };
            let name_style = if is_main { Style::new().fg(Color::Gray).add_modifier(Modifier::ITALIC) } else if w.status == Status::Broken || w.status == Status::Orphaned { Style::new().fg(Color::Red).add_modifier(Modifier::BOLD) } else if w.alive { Style::new().fg(Color::White).add_modifier(Modifier::BOLD) } else { Style::new().fg(Color::Gray) };
            let branch_style = if w.branch == w.name || is_main { Style::new().fg(Color::Rgb(120, 128, 140)) } else { Style::new().fg(Color::Gray) };
            let mut spans = vec![
                Span::raw("   "),
                live_glyph(w),
                Span::raw(" "),
                agent_badge(w),
                Span::raw(" "),
                Span::styled(pad(&name, c.name), name_style),
                Span::styled(pad(&w.branch, c.branch), branch_style),
                Span::styled(pad(&status_text(w), c.status), status_style(w)),
                Span::styled(pad(&format!(":{}", w.port), c.port), Style::new().fg(Color::Rgb(120, 128, 140))),
            ];
            spans.extend(hints_line(w).spans);
            if w.status == Status::SettingUp && !w.progress.is_empty() {
                spans.push(Span::styled(format!("  {}", fit(&w.progress, 70)), Style::new().fg(Color::Yellow)));
            }
            Line::from(spans)
        }
    }
}

fn status_text(w: &WorkspaceRow) -> String {
    if w.id.starts_with("main:") {
        return if w.alive { "main".into() } else { "main · off".into() };
    }
    match w.status {
        Status::Ready => "ready".into(),
        Status::Stopped => "⏸ stopped".into(),
        Status::SettingUp => "… setting up".into(),
        Status::Broken => "✗ broken".into(),
        Status::Orphaned => "! orphaned".into(),
    }
}

fn status_style(w: &WorkspaceRow) -> Style {
    if w.id.starts_with("main:") {
        return if w.alive { Style::new().fg(Color::Green).add_modifier(Modifier::ITALIC) } else { Style::new().fg(Color::DarkGray).add_modifier(Modifier::ITALIC) };
    }
    match w.status {
        Status::Ready => Style::new().fg(Color::Green),
        Status::Stopped => Style::new().fg(Color::DarkGray),
        Status::SettingUp => Style::new().fg(Color::Yellow),
        Status::Broken | Status::Orphaned => Style::new().fg(Color::Red),
    }
}

/// First run: no projects yet.
fn draw_welcome(f: &mut Frame, body: Rect, _ui: &Ui) {
    let w = 64.min(body.width.saturating_sub(2));
    let h = 12.min(body.height.saturating_sub(1));
    let rect = Rect { x: body.x + (body.width.saturating_sub(w)) / 2, y: body.y + (body.height.saturating_sub(h)) / 2, width: w, height: h };
    let inner = frame(f, rect, "welcome");
    let lines = vec![
        Line::from(""),
        Line::from(Span::styled("Git worktree workspaces with paired terminal sessions,", Style::new().fg(Color::Gray))),
        Line::from(Span::styled("ports and agents, one per branch.", Style::new().fg(Color::Gray))),
        Line::from(""),
        hints(&[("a", "add a project (path or git URL)")]),
        Line::from(""),
        Line::from(vec![Span::styled("or run ", Style::new().fg(Color::Rgb(120, 128, 140))), Span::styled("canopy init", Style::new().fg(Color::White)), Span::styled(" inside a repository, then ", Style::new().fg(Color::Rgb(120, 128, 140))), Span::styled("canopy new", Style::new().fg(Color::White)), Span::styled(".", Style::new().fg(Color::Rgb(120, 128, 140)))]),
        Line::from(""),
        hints(&[("?", "keys"), ("q", "quit")]),
    ];
    f.render_widget(Paragraph::new(lines).wrap(Wrap { trim: false }).style(Style::new().bg(PANEL_BG)), inner);
}

fn draw_dialogs(f: &mut Frame, area: Rect, ui: &Ui) {
    match &ui.mode {
        Mode::Help => modal(f, area, " keys ", 60, 16, help_text()),
        Mode::Inspect => {
            if let Some(w) = ui.selected() {
                modal(f, area, &format!(" {} ", w.name), 80, 18, inspect_text(&w));
            }
        }
        Mode::ConfirmDelete => {
            if let Some(w) = ui.selected() {
                modal(f, area, " delete ", 70, 7, vec![
                    Line::from(format!("Remove {} ({})?", w.name, w.branch)),
                    Line::from("This runs scripts.archive, kills the session, deletes the worktree and the branch."),
                    Line::from(""),
                    hints(&[("y", "yes, remove"), ("n", "keep it")]),
                ]);
            }
        }
        Mode::ConfirmKill => {
            if let Some(w) = ui.selected() {
                modal(f, area, " kill session ", 60, 6, vec![
                    Line::from(format!("Kill the session for {}? The worktree stays; enter resurrects it.", w.name)),
                    Line::from(""),
                    hints(&[("y", "yes, kill"), ("n", "keep it")]),
                ]);
            }
        }
        Mode::ConfirmRetry => {
            if let Some(w) = ui.selected() {
                modal(f, area, " retry setup ", 60, 6, vec![
                    Line::from(format!("Re-run scripts.setup for {} ({})?", w.name, w.status.as_str())),
                    Line::from(""),
                    hints(&[("y", "yes, re-run setup"), ("n", "cancel")]),
                ]);
            }
        }
        Mode::New(form) => render_new_form(f, area, form, false),
        Mode::AddProject(text) => modal(f, area, " add project ", 70, 8, vec![
            Line::from("Path of a git repo on this machine, or a git URL to clone."),
            Line::from(""),
            Line::from(vec![Span::styled("▸ ", Style::new().fg(crate::brand::ACCENT)), Span::styled(text.clone(), Style::new().fg(Color::White)), Span::styled("▏", Style::new().fg(crate::brand::ACCENT))]),
            Line::from(""),
            hints(&[("⏎", "add"), ("esc", "cancel")]),
        ]),
        Mode::Log(l) => l.render(f, log_rect(area), ui.spinner, &[]),
        Mode::Creating(c) => c.render(f, log_rect(area), ui.spinner, &[]),
        Mode::Busy(msg) => {
            let spin = SPIN[ui.spinner % SPIN.len()];
            modal(f, area, " working ", 50, 5, vec![Line::from(format!("{spin} {msg}"))]);
        }
        Mode::Error(e) => {
            let mut lines = error_lines(e);
            lines.push(Line::from(""));
            lines.push(hints(&[("any key", "back")]));
            modal(f, area, " error ", 80, (lines.len() as u16 + 3).min(20), lines);
        }
        Mode::List | Mode::Filter => {}
    }
}

/// Error text as separate lines (a `Line` with embedded newlines renders as one run-on).
pub(crate) fn error_lines(e: &str) -> Vec<Line<'static>> {
    let red = Style::new().fg(Color::Red);
    let dim = Style::new().fg(Color::Rgb(150, 158, 170));
    let mut out = Vec::new();
    for (i, l) in e.lines().enumerate() {
        out.push(Line::from(Span::styled(l.to_string(), if i == 0 { red } else { dim })));
    }
    if out.is_empty() {
        out.push(Line::from(Span::styled("unknown error", red)));
    }
    out
}

fn first_line(e: &str) -> String {
    e.lines().next().unwrap_or("error").to_string()
}

/// Large centered panel for the log views.
fn log_rect(area: Rect) -> Rect {
    let w = area.width.saturating_sub(4).clamp(20, 120);
    let h = area.height.saturating_sub(2).clamp(8, 40);
    Rect { x: (area.width.saturating_sub(w)) / 2, y: (area.height.saturating_sub(h)) / 2, width: w, height: h }
}

/// Truncate to `n` columns with an ellipsis.
fn fit(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        s.to_string()
    } else {
        let mut t: String = s.chars().take(n.saturating_sub(1)).collect();
        t.push('…');
        t
    }
}

fn shellexpand_home(p: &str) -> String {
    if let Some(rest) = p.strip_prefix("~/") {
        if let Some(h) = std::env::var_os("HOME") {
            return format!("{}/{rest}", h.to_string_lossy());
        }
    }
    p.to_string()
}

pub(crate) fn source_tabs(form: &NewForm, compact: bool) -> Line<'static> {
    let mut spans = Vec::new();
    for s in Source::ALL {
        let st = if s == form.source { Style::new().fg(Color::Black).bg(Color::Cyan).add_modifier(Modifier::BOLD) } else { Style::new().fg(Color::DarkGray) };
        spans.push(Span::styled(if compact { s.label().to_string() } else { format!(" {} ", s.label()) }, st));
        spans.push(Span::raw(" "));
    }
    Line::from(spans)
}

/// `fill`: use the whole area (inside a borderless tmux popup). Otherwise a fixed-size
/// centered dialog; the size never depends on the selected source.
fn render_new_form(f: &mut Frame, area: Rect, form: &NewForm, fill: bool) {
    let rect = if fill {
        area
    } else {
        let w = 92.min(area.width.saturating_sub(2));
        let h = (area.height.saturating_sub(4)).clamp(16, 24).min(area.height.saturating_sub(2));
        Rect { x: (area.width.saturating_sub(w)) / 2, y: (area.height.saturating_sub(h)) / 2, width: w, height: h }
    };
    let inner = frame(f, rect, &format!("new workspace · {}", form.project_name));
    let [tabs_a, rule_a, body_a, rule_b, foot_a] = Layout::vertical([Constraint::Length(2), Constraint::Length(1), Constraint::Min(1), Constraint::Length(1), Constraint::Length(1)]).areas(inner);
    f.render_widget(Paragraph::new(vec![Line::from(""), source_tabs(form, false)]), tabs_a);
    f.render_widget(Paragraph::new(rule(inner.width)), rule_a);
    f.render_widget(Paragraph::new(rule(inner.width)), rule_b);

    let label_style = Style::new().fg(Color::Rgb(120, 128, 140));
    let active_label = Style::new().fg(crate::brand::ACCENT).add_modifier(Modifier::BOLD);
    match form.source {
        Source::Fresh => {
            let field = |i: usize, label: &str, val: String, hint: &str| {
                let active = form.field == i;
                let shown = if val.is_empty() && !active { Span::styled(hint.to_string(), Style::new().fg(Color::DarkGray).add_modifier(Modifier::ITALIC)) } else { Span::styled(val, Style::new().fg(Color::White)) };
                Line::from(vec![
                    Span::styled(if active { "▸ " } else { "  " }, active_label),
                    Span::styled(format!("{label:<8}"), if active { active_label } else { label_style }),
                    shown,
                    Span::styled(if active { "▏" } else { "" }, Style::new().fg(crate::brand::ACCENT)),
                ])
            };
            let lines = vec![
                Line::from(""),
                field(0, "name", form.name.clone(), "random adjective-noun"),
                Line::from(""),
                field(1, "prompt", form.prompt.clone(), "opening message for the agent (optional)"),
                Line::from(""),
                field(2, "agent", form.agent.clone(), "project default"),
            ];
            f.render_widget(Paragraph::new(lines), body_a);
            f.render_widget(Paragraph::new(hints(&[("⏎", "create"), ("↑↓", "field"), ("tab", "source"), ("esc", "cancel")])), foot_a);
        }
        _ => {
            let mut lines = vec![Line::from(vec![
                Span::styled("  / ", Style::new().fg(crate::brand::ACCENT)),
                Span::styled(form.filter.clone(), Style::new().fg(Color::White)),
                Span::styled("▏", Style::new().fg(crate::brand::ACCENT)),
                Span::styled(if form.filter.is_empty() { " type to filter" } else { "" }, Style::new().fg(Color::DarkGray).add_modifier(Modifier::ITALIC)),
            ]), Line::from("")];
            if form.loading {
                lines.push(Line::from(Span::styled("  loading…", Style::new().fg(Color::DarkGray))));
            } else if !form.error.is_empty() {
                lines.push(Line::from(Span::styled(format!("  {}", form.error), Style::new().fg(Color::Red))));
            } else {
                let items = form.visible();
                if items.is_empty() {
                    lines.push(Line::from(Span::styled("  nothing here", Style::new().fg(Color::DarkGray))));
                }
                let max_rows = body_a.height.saturating_sub(2).max(3) as usize;
                let start = form.sel.saturating_sub(max_rows.saturating_sub(1));
                let inner_w = body_a.width as usize;
                for (i, it) in items.iter().enumerate().skip(start).take(max_rows) {
                    let selected = i == form.sel;
                    let row_bg = if selected { Style::new().bg(ROW_HL) } else { Style::new() };
                    let tag = if it.in_use { "  ● in use " } else { " " };
                    let room = inner_w.saturating_sub(2 + tag.chars().count());
                    let label = fit(&it.label, room.min(60));
                    let used = label.chars().count();
                    let detail = if it.detail.is_empty() || room <= used + 3 { String::new() } else { format!("  {}", fit(&it.detail, room - used - 2)) };
                    let pad = inner_w.saturating_sub(2 + used + detail.chars().count() + tag.chars().count());
                    let mut spans = vec![
                        Span::styled(if selected { "▶ " } else { "  " }, row_bg.fg(crate::brand::ACCENT)),
                        Span::styled(label, row_bg.fg(Color::White).add_modifier(if selected { Modifier::BOLD } else { Modifier::empty() })),
                        Span::styled(detail, row_bg.fg(Color::Rgb(120, 128, 140))),
                        Span::styled(" ".repeat(pad), row_bg),
                    ];
                    spans.push(Span::styled(tag, row_bg.fg(Color::Yellow)));
                    lines.push(Line::from(spans));
                }
            }
            f.render_widget(Paragraph::new(lines), body_a);
            f.render_widget(Paragraph::new(hints(&[("⏎", "create"), ("↑↓", "pick"), ("tab", "source"), ("esc", "cancel")])), foot_a);
        }
    }
}

pub(crate) const PANEL_BG: Color = Color::Rgb(20, 23, 30);
pub(crate) const PANEL_RULE: Color = Color::Rgb(52, 58, 68);
pub(crate) const ROW_HL: Color = Color::Rgb(40, 44, 52);

/// Dialog frame: rounded accent border on a dark panel, the mark in the title. Returns the
/// inner area (with one column of horizontal padding) to draw into.
pub(crate) fn frame(f: &mut Frame, rect: Rect, title: &str) -> Rect {
    f.render_widget(Clear, rect);
    let title_line = Line::from(vec![
        Span::styled(format!(" {} ", crate::brand::glyph()), Style::new().fg(Color::Black).bg(crate::brand::ACCENT).add_modifier(Modifier::BOLD)),
        Span::styled(format!(" {} ", title.trim()), Style::new().fg(Color::White).add_modifier(Modifier::BOLD)),
    ]);
    let block = Block::bordered()
        .border_type(BorderType::Rounded)
        .border_style(Style::new().fg(crate::brand::ACCENT))
        .style(Style::new().bg(PANEL_BG))
        .title(title_line);
    let inner = block.inner(rect);
    f.render_widget(block, rect);
    inner.inner(ratatui::layout::Margin { horizontal: 1, vertical: 0 })
}

/// A row of `key  action` hints in the dialog footer.
pub(crate) fn hints(pairs: &[(&str, &str)]) -> Line<'static> {
    let mut spans = Vec::new();
    for (i, (k, v)) in pairs.iter().enumerate() {
        if i > 0 {
            spans.push(Span::raw("   "));
        }
        spans.push(Span::styled(format!(" {k} "), Style::new().fg(Color::Black).bg(Color::Rgb(90, 98, 112)).add_modifier(Modifier::BOLD)));
        spans.push(Span::styled(format!(" {v}"), Style::new().fg(Color::Gray)));
    }
    Line::from(spans)
}

pub(crate) fn rule(width: u16) -> Line<'static> {
    Line::from(Span::styled("─".repeat(width as usize), Style::new().fg(PANEL_RULE)))
}

fn modal(f: &mut Frame, area: Rect, title: &str, w: u16, h: u16, lines: Vec<Line<'static>>) {
    let w = w.min(area.width.saturating_sub(2));
    let h = (h + 1).min(area.height.saturating_sub(2));
    let x = (area.width.saturating_sub(w)) / 2;
    let y = (area.height.saturating_sub(h)) / 2;
    let inner = frame(f, Rect { x, y, width: w, height: h }, title);
    let mut body = vec![Line::from("")];
    body.extend(lines);
    f.render_widget(Paragraph::new(body).wrap(Wrap { trim: false }).style(Style::new().bg(PANEL_BG)), inner);
}

fn help_text() -> Vec<Line<'static>> {
    [
        ("↑/k ↓/j", "move"),
        ("enter", "attach (resurrect if stopped)"),
        ("n", "new workspace: fresh / PR / issue / branch"),
        ("a", "add project (path or git URL)"),
        ("d", "delete workspace (archive script, session, worktree, branch)"),
        ("K", "kill session only"),
        ("R", "re-run scripts.setup (watch it live)"),
        ("L", "setup log; live while setting up"),
        ("i", "inspect"),
        ("/", "filter by name, branch, project"),
        ("tab", "switch local / all"),
        ("r", "refresh"),
        ("q", "quit"),
        ("", ""),
        ("⚡ 💤 ✋", "agent working / idle / blocked"),
        ("● ○ ⊙", "session alive / dead / attached"),
    ]
    .into_iter()
    .map(|(k, v)| Line::from(vec![Span::styled(format!("{k:<9}"), Style::new().fg(Color::Cyan)), Span::raw(v)]))
    .collect()
}

fn inspect_text(w: &WorkspaceRow) -> Vec<Line<'static>> {
    let kv = |k: &str, v: String| Line::from(vec![Span::styled(format!("{k:<10}"), Style::new().fg(Color::DarkGray)), Span::raw(v)]);
    let mut v = vec![
        kv("id", w.id.clone()),
        kv("project", format!("{} ({})", w.project, w.project_root.display())),
        kv("branch", w.branch.clone()),
        kv("path", w.path.display().to_string()),
        kv("port", format!("{} .. {}", w.port, w.port + 9)),
        kv("session", w.session.clone()),
        kv("status", format!("{}{}", w.status.as_str(), if w.alive { " · alive" } else { "" })),
        kv("agent", format!("{} ({:?})", w.agent, w.agent_state).to_lowercase()),
        kv("source", format!("{:?}", w.source_kind).to_lowercase()),
    ];
    if !w.owner.is_empty() {
        v.push(kv("owner", w.owner.clone()));
    }
    if !w.hints.is_empty() {
        v.push(Line::from(""));
        for h in &w.hints {
            v.push(Line::from(vec![Span::styled("  • ", Style::new().fg(Color::Yellow)), Span::raw(h.message.clone()), Span::styled(if h.action.is_empty() { String::new() } else { format!("  → {}", h.action) }, Style::new().fg(Color::DarkGray))]));
        }
    }
    if !w.last_error_hint.is_empty() {
        v.push(Line::from(""));
        v.push(Line::from(Span::styled(w.last_error_hint.clone(), Style::new().fg(Color::Red))));
    }
    v
}


// ---------------------------------------------------------------------------------------
// Popup forms (launched by the sidebar via `tmux display-popup`)
// ---------------------------------------------------------------------------------------

/// Full-screen new-workspace form. Returns the created workspace, or `None` on cancel.
pub fn run_new_popup(transport: Transport, root: PathBuf, project: String) -> Result<Option<WorkspaceRow>> {
    let mut terminal = ratatui::init();
    let res = new_popup_loop(&mut terminal, &transport, root, project);
    ratatui::restore();
    res
}

fn new_popup_loop(terminal: &mut ratatui::DefaultTerminal, transport: &Transport, root: PathBuf, project: String) -> Result<Option<WorkspaceRow>> {
    let mut form = NewForm::new(root, project);
    let (tx, rx) = mpsc::channel::<Bg>();
    let mut creating: Option<Creating> = None;
    let mut error: Option<String> = None;
    let mut spinner = 0usize;
    loop {
        terminal.draw(|f| {
            let area = f.area();
            f.render_widget(Clear, area);
            match &creating {
                Some(c) => c.render(f, area, spinner, &[]),
                None => render_new_form(f, area, &form, true),
            }
            if let Some(e) = &error {
                let mut lines = error_lines(e);
                lines.push(Line::from(""));
                lines.push(hints(&[("any key", "back to the form")]));
                modal(f, area, " error ", 80, (lines.len() as u16 + 3).min(20), lines);
            }
        })?;
        while let Ok(msg) = rx.try_recv() {
            match msg {
                Bg::Picks(source, items) => form.set_items(source, items),
                Bg::Created(Ok(w)) => return Ok(Some(w)),
                Bg::Created(Err(e)) => {
                    creating = None;
                    error = Some(e);
                }
                Bg::Log(res) => {
                    if let Some(c) = creating.as_mut() {
                        c.on_log(res);
                    }
                }
                Bg::Found(res) => {
                    if let Some(c) = creating.as_mut() {
                        c.on_found(res);
                    }
                }
                _ => {}
            }
        }
        if let Some(c) = creating.as_mut() {
            let t = transport.clone();
            let tx = tx.clone();
            match c.next_poll() {
                Poll::Fetch { target, offset } => {
                    std::thread::spawn(move || {
                        let _ = tx.send(Bg::Log(livelog::fetch(&t, &target, offset, Some(200))));
                    });
                }
                Poll::Find { root, known, name } => {
                    std::thread::spawn(move || {
                        let _ = tx.send(Bg::Found(livelog::find_new(&t, &root, &known, name.as_deref())));
                    });
                }
                Poll::Nothing => {}
            }
        }
        if event::poll(Duration::from_millis(120))? {
            if let Event::Key(key) = event::read()? {
                if key.kind != KeyEventKind::Press {
                    continue;
                }
                let ctrl_c = key.code == KeyCode::Char('c') && key.modifiers.contains(KeyModifiers::CONTROL);
                if ctrl_c {
                    return Ok(None);
                }
                if error.is_some() {
                    error = None;
                    continue;
                }
                if let Some(c) = creating.as_mut() {
                    // Esc hides the popup; the server keeps creating (L on the row follows it).
                    if key.code == KeyCode::Esc || key.code == KeyCode::Char('q') {
                        return Ok(None);
                    }
                    if let Some(l) = c.log.as_mut() {
                        log_scroll_key(l, key.code);
                    }
                    continue;
                }
                match form.handle_key(key) {
                    Action::Cancel => return Ok(None),
                    Action::None => {}
                    Action::Load(source) => {
                        let t = transport.clone();
                        let r = form.project_root.clone();
                        let tx = tx.clone();
                        std::thread::spawn(move || {
                            let _ = tx.send(Bg::Picks(source, newform::load(&t, &r, source)));
                        });
                    }
                    Action::Submit(req) => {
                        let known: BTreeSet<String> = match transport.call(Method::WorkspaceList { project_root: Some(req.project_root.clone()) }) {
                            Ok(ResultBody::WorkspaceList { workspaces }) => workspaces.into_iter().map(|w| w.id).collect(),
                            _ => BTreeSet::new(),
                        };
                        creating = Some(Creating::new(req.project_root.clone(), req.name.clone(), known));
                        let t = transport.clone();
                        let tx = tx.clone();
                        std::thread::spawn(move || {
                            let _ = tx.send(match t.call(Method::WorkspaceCreate(req)) {
                                Ok(ResultBody::Workspace { workspace }) => Bg::Created(Ok(workspace)),
                                Ok(_) => Bg::Created(Err("unexpected response".into())),
                                Err(e) => Bg::Created(Err(e.to_string())),
                            });
                        });
                    }
                }
            }
        }
        spinner = spinner.wrapping_add(1);
    }
}

/// Full-screen add-project prompt. Returns the added project's name, or `None` on cancel.
pub fn run_add_project_popup(transport: Transport) -> Result<Option<String>> {
    let mut terminal = ratatui::init();
    let res = (|| -> Result<Option<String>> {
        let mut text = String::new();
        let mut busy = false;
        let mut error: Option<String> = None;
        let (tx, rx) = mpsc::channel::<Result<String, String>>();
        let mut spinner = 0usize;
        loop {
            terminal.draw(|f| {
                let area = f.area();
                f.render_widget(Clear, area);
                let mut lines = vec![
                    Line::from("Path of a git repo on this machine, or a git URL to clone."),
                    Line::from(""),
                    Line::from(vec![Span::styled("▸ ", Style::new().fg(crate::brand::ACCENT)), Span::styled(text.clone(), Style::new().fg(Color::White)), Span::styled("▏", Style::new().fg(crate::brand::ACCENT))]),
                    Line::from(""),
                    hints(&[("⏎", "add"), ("esc", "cancel")]),
                ];
                if busy {
                    lines.push(Line::from(""));
                    lines.push(Line::from(format!("{} working…", SPIN[spinner % SPIN.len()])));
                }
                if let Some(e) = &error {
                    lines.push(Line::from(""));
                    lines.push(Line::from(Span::styled(e.clone(), Style::new().fg(Color::Red))));
                }
                let inner = frame(f, area, "add project");
                let mut body = vec![Line::from("")];
                body.extend(lines);
                f.render_widget(Paragraph::new(body).wrap(Wrap { trim: false }).style(Style::new().bg(PANEL_BG)), inner);
            })?;
            while let Ok(r) = rx.try_recv() {
                match r {
                    Ok(name) => return Ok(Some(name)),
                    Err(e) => {
                        busy = false;
                        error = Some(e);
                    }
                }
            }
            if event::poll(Duration::from_millis(120))? {
                if let Event::Key(key) = event::read()? {
                    if key.kind != KeyEventKind::Press || busy {
                        continue;
                    }
                    let ctrl_c = key.code == KeyCode::Char('c') && key.modifiers.contains(KeyModifiers::CONTROL);
                    match key.code {
                        KeyCode::Esc => return Ok(None),
                        _ if ctrl_c => return Ok(None),
                        KeyCode::Backspace => {
                            text.pop();
                        }
                        KeyCode::Enter => {
                            let v = text.trim().to_string();
                            if v.is_empty() {
                                continue;
                            }
                            error = None;
                            busy = true;
                            let is_url = v.contains("://") || (v.contains('@') && v.contains(':'));
                            let (path, url) = if is_url { (None, Some(v.clone())) } else { (Some(PathBuf::from(shellexpand_home(&v))), None) };
                            let t = transport.clone();
                            let tx = tx.clone();
                            std::thread::spawn(move || {
                                let _ = tx.send(match t.call(Method::ProjectInit { path, url, with_scripts: true, adopt_from: None }) {
                                    Ok(ResultBody::ProjectList { projects }) => Ok(projects.first().map(|p| p.name.clone()).unwrap_or_default()),
                                    Ok(_) => Err("unexpected response".into()),
                                    Err(e) => Err(e.to_string()),
                                });
                            });
                        }
                        KeyCode::Char(c) => text.push(c),
                        _ => {}
                    }
                }
            }
            spinner = spinner.wrapping_add(1);
        }
    })();
    ratatui::restore();
    res
}
