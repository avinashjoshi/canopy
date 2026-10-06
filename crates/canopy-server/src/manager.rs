//! Workspace lifecycle: the orchestration layer every API method calls into.
//!
//! Create is three phases so parallel creates only serialize on the millisecond-long
//! registration windows, never on `bundle install`:
//!
//! ```text
//! Phase 1 (lock): resolve project, name, branch, port; insert row `setting_up`; persist.
//! Phase 2 (no lock): mkdir, fetch, worktree add, scripts.setup, build session.
//! Phase 3 (lock): status ready (or broken with last_error); persist.
//! ```

use crate::app::{App, Shared};
use crate::backend::{BackendError, PaneSpec};
use crate::launcher::{self, BriefingMode, Launcher};
use crate::{git, scripts};
use canopy_core::config::{ProjectConfig, FILE_NAME};
use canopy_core::env::WorkspaceEnv;
use canopy_core::ports::{tcp_probe, PortPlan};
use canopy_core::settings::Layout;
use canopy_core::state::{Project, SourceKind, Status, Workspace};
use canopy_core::{git as coregit, namegen};
use canopy_proto::{ApiError, AttachTarget, ErrorCode, EventKind, WorkspaceCreate, WorkspaceRef, WorkspaceRow};
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::time::Duration;

#[derive(Debug, thiserror::Error)]
pub enum ManagerError {
    #[error("{0}")]
    InvalidParams(String),
    #[error("project not found: {0}")]
    ProjectNotFound(PathBuf),
    #[error("project {0} still has {1} workspace(s); remove them first")]
    ProjectHasWorkspaces(PathBuf, usize),
    #[error("workspace not found: {0}")]
    WorkspaceNotFound(String),
    #[error("workspace {0:?} already exists")]
    WorkspaceExists(String),
    #[error("workspace {0:?} is busy ({1})")]
    Busy(String, &'static str),
    #[error("removal blocked: {0}. Pass force to override.")]
    RemovalBlocked(String),
    #[error("{0}\n\nThe workspace is kept as `broken`: fix the script or config, then press R (retry) on it.")]
    Setup(String),
    #[error(transparent)]
    Backend(#[from] BackendError),
    #[error(transparent)]
    Git(#[from] git::GitError),
    #[error(transparent)]
    Launcher(#[from] launcher::LauncherError),
    #[error(transparent)]
    Ports(#[from] canopy_core::ports::PortError),
    #[error(transparent)]
    State(#[from] canopy_core::state::StateError),
    #[error(transparent)]
    Config(#[from] canopy_core::config::ConfigError),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    #[error("{0}")]
    Other(String),
}

impl From<ManagerError> for ApiError {
    fn from(e: ManagerError) -> Self {
        let code = match &e {
            ManagerError::InvalidParams(_) => ErrorCode::InvalidParams,
            ManagerError::ProjectNotFound(_) => ErrorCode::ProjectNotFound,
            ManagerError::ProjectHasWorkspaces(..) => ErrorCode::ProjectHasWorkspaces,
            ManagerError::WorkspaceNotFound(_) => ErrorCode::WorkspaceNotFound,
            ManagerError::WorkspaceExists(_) => ErrorCode::WorkspaceExists,
            ManagerError::Busy(..) => ErrorCode::WorkspaceBusy,
            ManagerError::RemovalBlocked(_) => ErrorCode::RemovalBlocked,
            ManagerError::Setup(_) => ErrorCode::SetupFailed,
            ManagerError::Backend(_) => ErrorCode::BackendError,
            ManagerError::Git(_) => ErrorCode::GitError,
            ManagerError::Launcher(launcher::LauncherError::NotAllowed(_)) => ErrorCode::AgentNotAllowed,
            ManagerError::Launcher(_) => ErrorCode::AgentNotFound,
            ManagerError::Ports(_) => ErrorCode::NoPortsAvailable,
            ManagerError::Config(_) => ErrorCode::InvalidParams,
            ManagerError::State(canopy_core::state::StateError::WorkspaceExists(..)) => ErrorCode::WorkspaceExists,
            ManagerError::State(canopy_core::state::StateError::WorkspaceNotFound(_)) => ErrorCode::WorkspaceNotFound,
            _ => ErrorCode::InternalError,
        };
        ApiError { code, message: e.to_string() }
    }
}

type R<T> = Result<T, ManagerError>;

/// Extra env prefixes for a project: user config `[env] prefixes` plus the project's own.
fn env_prefixes(app: &App, cfg: &ProjectConfig) -> Vec<String> {
    let mut v = app.settings.env.prefixes.clone();
    for p in &cfg.env_prefixes {
        if !v.contains(p) {
            v.push(p.clone());
        }
    }
    v
}

/// Environment for a workspace's scripts and panes. Also carries the display variables so
/// clipboard tools run by agents (`wl-paste` for an image paste) work even when the server
/// or tmux was started without a graphical session in its environment (ssh, boot).
fn workspace_env(app: &App, ws: &Workspace, prefixes: &[String], with_socket: bool) -> BTreeMap<String, String> {
    let project_name = app.project_name(&ws.project_root);
    let socket = app.paths.socket();
    let mut env = WorkspaceEnv {
        workspace_path: &ws.path,
        root_path: &ws.project_root,
        port: ws.port,
        workspace_name: &ws.name,
        branch: &ws.branch,
        project: &project_name,
        workspace_id: &ws.id,
        socket_path: with_socket.then_some(socket.as_path()),
        extra_prefixes: prefixes,
    }
    .vars();
    if let Some(d) = crate::clipboard::wayland_display() {
        env.entry("WAYLAND_DISPLAY".into()).or_insert(d);
    }
    if let Ok(d) = std::env::var("DISPLAY") {
        env.entry("DISPLAY".into()).or_insert(d);
    }
    env
}

/// Setting-up rows older than this are considered broken by reconcile.
const SETTING_UP_STALE: Duration = Duration::from_secs(5 * 60);

// ---------------------------------------------------------------------------------------
// Lookup helpers
// ---------------------------------------------------------------------------------------

pub fn canonical_root(path: &Path) -> R<PathBuf> {
    let root = git::root(path)?;
    Ok(root.canonicalize().unwrap_or(root))
}

pub fn resolve_ref(app: &App, r: &WorkspaceRef) -> R<Workspace> {
    match r {
        WorkspaceRef::Id { id } if id.starts_with("main:") => {
            let name = &id["main:".len()..];
            app.state
                .projects
                .values()
                .find(|p| p.name == name)
                .and_then(|p| app.main_pseudo(&p.root))
                .ok_or_else(|| ManagerError::WorkspaceNotFound(id.clone()))
        }
        WorkspaceRef::Id { id } => app.state.by_id(id).cloned().ok_or_else(|| ManagerError::WorkspaceNotFound(id.clone())),
        WorkspaceRef::Named { project_root, name } => {
            let root = canonical_root(project_root).unwrap_or_else(|_| project_root.canonicalize().unwrap_or_else(|_| project_root.clone()));
            app.state
                .find(&root, name)
                .cloned()
                .or_else(|| app.state.workspaces.iter().find(|w| w.name == *name && w.project_root.ends_with(project_root)).cloned())
                .ok_or_else(|| ManagerError::WorkspaceNotFound(name.clone()))
        }
    }
}

/// Register a project (idempotent) and return it.
pub fn ensure_project(app: &mut App, root: &Path) -> R<Project> {
    let root = canonical_root(root)?;
    if let Some(p) = app.state.project(&root) {
        return Ok(p.clone());
    }
    if !root.join(FILE_NAME).is_file() {
        return Err(ManagerError::ProjectNotFound(root));
    }
    let plan: PortPlan = app.settings.ports;
    let base = plan.next_project_base(&app.state.used_project_bases())?;
    let name = root.file_name().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default();
    let p = Project { root: root.clone(), name, port_base: base };
    app.state.projects.insert(root, p.clone());
    app.persist()?;
    Ok(p)
}

fn project_config(root: &Path) -> R<ProjectConfig> {
    Ok(ProjectConfig::load(&root.join(FILE_NAME))?)
}

fn now_rfc3339() -> String {
    scripts::chrono_like_now()
}

fn created_age(ws: &Workspace) -> Option<Duration> {
    // created_at is `YYYY-MM-DDTHH:MM:SSZ`; parse back to seconds via a tiny inverse.
    let s = ws.created_at.as_bytes();
    if s.len() != 20 {
        return None;
    }
    let num = |a: usize, b: usize| std::str::from_utf8(&s[a..b]).ok()?.parse::<i64>().ok();
    let (y, m, d, hh, mm, ss) = (num(0, 4)?, num(5, 7)?, num(8, 10)?, num(11, 13)?, num(14, 16)?, num(17, 19)?);
    // days from civil
    let (y, m) = if m <= 2 { (y - 1, m + 9) } else { (y, m - 3) };
    let era = y.div_euclid(400);
    let yoe = y.rem_euclid(400);
    let doy = (153 * m + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146_097 + doe - 719_468;
    let secs = days * 86400 + hh * 3600 + mm * 60 + ss;
    let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).ok()?.as_secs() as i64;
    Some(Duration::from_secs((now - secs).max(0) as u64))
}

// ---------------------------------------------------------------------------------------
// Session building (shared by create, resurrect, main)
// ---------------------------------------------------------------------------------------

struct SessionPlan {
    session: String,
    cwd: PathBuf,
    env: BTreeMap<String, String>,
    layout: Layout,
    editor_cmd: String,
    agent_cmd: String,
    agent_role: String,
    sidebar_width: u16,
    /// Name of the first window (tab).
    window_name: String,
    workspace_id: String,
}

fn briefing_path(app: &App, id: &str) -> PathBuf {
    app.paths.tmp_dir().join(format!("briefing-{id}.md"))
}

/// Compute the session plan for a workspace (or the project's main session when `ws.name == "main"` and `is_main`).
fn plan_session(app: &App, ws: &Workspace, cfg: &ProjectConfig, resume: bool, is_main: bool) -> R<SessionPlan> {
    let project_name = app.project_name(&ws.project_root);
    let session = if is_main { app.main_session_name(&ws.project_root) } else { app.session_name(ws) };
    let env = workspace_env(app, ws, &env_prefixes(app, cfg), true);

    let launcher: &Launcher = launcher::resolve(&ws.agent, &cfg.agent.kind, &app.settings.agent.kind, &cfg.agents)?;
    let hints = app.runtime_of(&ws.id).hints;
    let project_briefing = if !cfg.agent.briefing_file.is_empty() {
        std::fs::read_to_string(ws.project_root.join(&cfg.agent.briefing_file)).unwrap_or_else(|_| cfg.agent.briefing.clone())
    } else {
        cfg.agent.briefing.clone()
    };
    let briefing = if is_main {
        String::new()
    } else {
        launcher::render(&launcher::BriefingInput { ws, project_name: &project_name, session: &session, hints: &hints, project_briefing: &project_briefing, env: &env })
    };
    let bpath = briefing_path(app, &ws.id);
    if !briefing.is_empty() {
        std::fs::create_dir_all(app.paths.tmp_dir())?;
        std::fs::write(&bpath, &briefing)?;
    }

    let agent_cmd = if !cfg.scripts.agent.is_empty() {
        let mut e = env.clone();
        e.insert("CANOPY_AGENT_BRIEFING".into(), briefing.clone());
        format!("CANOPY_AGENT_BRIEFING={} {}", launcher::sh_quote(&briefing), launcher::sh_quote(&ws.project_root.join(&cfg.scripts.agent).display().to_string()))
    } else if launcher::installed(launcher) {
        let resume_id = if resume { app.agents.session_id(&ws.id).map(str::to_owned).or_else(|| (!ws.agent_session_id.is_empty()).then(|| ws.agent_session_id.clone())) } else { None };
        let text = if matches!(launcher.briefing, BriefingMode::None) { "" } else { briefing.as_str() };
        launcher::command_line(launcher, resume_id.as_deref(), resume, text, &bpath.display().to_string())
    } else {
        format!("echo 'canopy: agent {} is not installed (looked for `{}` on PATH)'", launcher.name, launcher.cmd)
    };

    Ok(SessionPlan {
        session,
        cwd: ws.path.clone(),
        env,
        layout: app.settings.layout(&cfg.layout),
        editor_cmd: app.settings.editor.command.clone(),
        agent_cmd,
        agent_role: format!("agent:{}", launcher.name),
        sidebar_width: if app.settings.sidebar.enabled { app.settings.sidebar.width } else { 0 },
        window_name: if is_main { "main".into() } else { "work".into() },
        workspace_id: ws.id.clone(),
    })
}

fn build_session(app: &App, plan: &SessionPlan) -> R<()> {
    let backend = app.backend.clone();
    let mut first = true;
    let mut agent_pane = None;
    for lp in &plan.layout.panes {
        let (role, command): (String, String) = match lp.role.as_str() {
            "ide" => ("ide".into(), plan.editor_cmd.clone()),
            "agent" => (plan.agent_role.clone(), plan.agent_cmd.clone()),
            "shell" => ("terminal:shell".into(), String::new()),
            other if other.starts_with("run:") => (other.to_string(), String::new()),
            other => (format!("terminal:{other}"), String::new()),
        };
        if role == "ide" && command.trim().is_empty() {
            continue;
        }
        let of_role = lp.of.as_deref().map(|r| match r {
            "ide" => "ide".to_string(),
            "agent" => plan.agent_role.clone(),
            "shell" => "terminal:shell".to_string(),
            o => o.to_string(),
        });
        let spec = PaneSpec {
            role: &role,
            command: &command,
            cwd: &plan.cwd,
            env: &plan.env,
            split: lp.split.as_deref(),
            split_of: of_role.as_deref(),
            size_percent: lp.size,
            size_cells: None,
            keep_alive: true,
            full_span: false,
            window: None,
        };
        let id = if first {
            first = false;
            backend.create_session(&plan.session, &plan.window_name, spec)?
        } else {
            backend.add_pane(&plan.session, spec)?
        };
        if role.starts_with("agent:") {
            agent_pane = Some(id);
        }
    }
    if plan.sidebar_width > 0 {
        if let Err(e) = add_sidebar(app, &plan.session, &plan.cwd, &plan.env, plan.sidebar_width, None) {
            tracing::warn!(error = %e, "sidebar pane not created");
        }
    }
    let statusline = format!("{} statusline --workspace {}", launcher::sh_quote(&canopy_bin()), plan.workspace_id);
    if let Err(e) = backend.decorate_session(&plan.session, &statusline) {
        tracing::warn!(error = %e, "session chrome not applied");
    }
    if let Err(e) = backend.ensure_server_config(&canopy_bin()) {
        tracing::warn!(error = %e, "tmux keybinds not applied");
    }
    if let Some(p) = agent_pane {
        let _ = backend.select_pane(&plan.session, &p);
    }
    Ok(())
}

/// Path of the `canopy` binary for pane commands: PATH first (so `canopy use`-style swaps
/// take effect), else the running server's executable.
fn canopy_bin() -> String {
    if let Some(p) = std::env::var_os("PATH") {
        if let Some(found) = std::env::split_paths(&p).map(|d| d.join("canopy")).find(|c| c.is_file()) {
            return found.display().to_string();
        }
    }
    std::env::current_exe().map(|p| p.display().to_string()).unwrap_or_else(|_| "canopy".into())
}

pub const SIDEBAR_ROLE: &str = "sidebar";
/// Width of the collapsed sidebar strip. The sidebar derives "collapsed" from its own
/// width, so the server only ever resizes; it never messages the sidebar process.
pub const SIDEBAR_COLLAPSED_WIDTH: u16 = 3;
fn is_collapsed(width: u16) -> bool {
    width <= SIDEBAR_COLLAPSED_WIDTH + 3
}

/// Re-apply per-session chrome (status bar, hooks, focus guard) to every live session, so
/// sessions created by an older build pick up new behavior after a server restart.
pub fn redecorate_sessions(app: &App) -> usize {
    let bin = launcher::sh_quote(&canopy_bin());
    let mut n = 0;
    let targets: Vec<(String, String)> = app
        .state
        .workspaces
        .iter()
        .map(|w| (app.session_name(w), w.id.clone()))
        .chain(app.state.projects.values().map(|p| (app.main_session_name(&p.root), format!("main:{}", p.name))))
        .collect();
    for (session, id) in targets {
        if !app.backend.session_exists(&session).unwrap_or(false) {
            continue;
        }
        let statusline = format!("{bin} statusline --workspace {id}");
        match app.backend.decorate_session(&session, &statusline) {
            Ok(()) => n += 1,
            Err(e) => tracing::debug!(error = %e, session = %session, "redecorate"),
        }
    }
    n
}

/// Respawn every sidebar pane so they run the current binary (called on server start).
/// Keeps each pane's window and collapsed state.
pub fn restart_sidebars(app: &App) -> usize {
    let mut restarted = 0;
    let sessions: Vec<(String, PathBuf, BTreeMap<String, String>)> = app
        .state
        .workspaces
        .iter()
        .map(|w| (app.session_name(w), w.path.clone(), {
            let cfg = project_config(&w.project_root).unwrap_or_default();
            workspace_env(app, w, &env_prefixes(app, &cfg), true)
        }))
        .chain(app.state.projects.values().map(|p| {
            let (cwd, env) = sidebar_context(app, &app.main_session_name(&p.root));
            (app.main_session_name(&p.root), cwd, env)
        }))
        .collect();
    for (session, cwd, env) in sessions {
        if !app.backend.session_exists(&session).unwrap_or(false) {
            continue;
        }
        let Ok(panes) = app.backend.panes(&session) else { continue };
        for p in panes.iter().filter(|p| p.role == SIDEBAR_ROLE) {
            let width = if is_collapsed(p.width) { SIDEBAR_COLLAPSED_WIDTH } else { app.settings.sidebar.width };
            let focused = panes.iter().find(|q| q.window == p.window && q.active && q.id != p.id).map(|q| q.id.clone());
            if app.backend.kill_pane(&p.id).is_err() {
                continue;
            }
            match add_sidebar(app, &session, &cwd, &env, width, Some(p.window)) {
                Ok(_) => restarted += 1,
                Err(e) => tracing::warn!(error = %e, session = %session, "sidebar respawn"),
            }
            if let Some(f) = focused {
                let _ = app.backend.select_pane(&session, &f);
            }
        }
    }
    restarted
}

/// Apply canopy's runtime tmux configuration (keybinds, tab styling). Safe to call often.
pub fn ensure_backend_config(app: &App) {
    if let Err(e) = app.backend.ensure_server_config(&canopy_bin()) {
        tracing::debug!(error = %e, "backend config");
    }
}

/// Add a full-height sidebar pane at the left of `window` (default: the active window).
/// `pane_width` is the initial size (the strip width when created collapsed); the process is
/// always told the configured full width so expanding later lands on the right size.
fn add_sidebar(app: &App, session: &str, cwd: &Path, env: &BTreeMap<String, String>, pane_width: u16, window: Option<u32>) -> R<crate::backend::PaneId> {
    let full = app.settings.sidebar.width.max(SIDEBAR_COLLAPSED_WIDTH + 4);
    let cmd = format!("{} sidebar --width {full}", launcher::sh_quote(&canopy_bin()));
    Ok(app.backend.add_pane(
        session,
        PaneSpec {
            role: SIDEBAR_ROLE,
            command: &cmd,
            cwd,
            env,
            split: Some("left"),
            split_of: None,
            size_percent: None,
            size_cells: Some(pane_width),
            keep_alive: false,
            full_span: true,
            window,
        },
    )?)
}

/// cwd + env for sidebar panes of a session (workspace, or a project's main session).
fn sidebar_context(app: &App, session: &str) -> (PathBuf, BTreeMap<String, String>) {
    let ws = app.state.workspaces.iter().find(|w| app.session_name(w) == session).cloned();
    match ws {
        Some(w) => {
            let cfg = project_config(&w.project_root).unwrap_or_default();
            let env = workspace_env(app, &w, &env_prefixes(app, &cfg), true);
            (w.path.clone(), env)
        }
        None => {
            // A project's main session: identify as `main:<project>` so the sidebar can
            // highlight itself and list its tabs.
            match app.state.projects.values().find(|p| app.main_session_name(&p.root) == session).and_then(|p| app.main_pseudo(&p.root)) {
                Some(pseudo) => {
                    let cfg = project_config(&pseudo.project_root).unwrap_or_default();
                    let env = workspace_env(app, &pseudo, &env_prefixes(app, &cfg), true);
                    (pseudo.path.clone(), env)
                }
                None => {
                    let mut env = BTreeMap::new();
                    env.insert("CANOPY_SOCKET_PATH".to_string(), app.paths.socket().display().to_string());
                    (PathBuf::from("/"), env)
                }
            }
        }
    }
}

/// Make sure the active window of `session` has a sidebar (used by the new-window hook).
pub async fn sidebar_ensure(shared: Shared, session: &str) -> R<()> {
    let app = shared.lock().await;
    if !app.settings.sidebar.enabled || !app.backend.session_exists(session)? {
        return Ok(());
    }
    let win = app.backend.active_window(session)?;
    let panes = app.backend.panes(session)?;
    if panes.iter().any(|p| p.window == win && p.role == SIDEBAR_ROLE) {
        return Ok(());
    }
    // New tabs inherit the collapsed state of the session's other sidebars.
    let collapsed = panes.iter().filter(|p| p.role == SIDEBAR_ROLE).all(|p| is_collapsed(p.width)) && panes.iter().any(|p| p.role == SIDEBAR_ROLE);
    let width = if collapsed { SIDEBAR_COLLAPSED_WIDTH } else { app.settings.sidebar.width };
    let (cwd, env) = sidebar_context(&app, session);
    // The hook fires with the new window's pane focused; put focus back on it afterwards.
    let focused = panes.iter().find(|p| p.window == win && p.active).map(|p| p.id.clone());
    add_sidebar(&app, session, &cwd, &env, width, Some(win))?;
    if let Some(p) = focused {
        let _ = app.backend.select_pane(session, &p);
    }
    Ok(())
}

/// `prefix+b` in the active window: missing -> add and focus; collapsed -> expand and
/// focus; expanded but unfocused -> focus; focused -> collapse to a thin strip.
/// Returns true when the sidebar is expanded afterwards.
pub async fn sidebar_toggle(shared: Shared, session: &str) -> R<bool> {
    let app = shared.lock().await;
    if !app.backend.session_exists(session)? {
        return Err(ManagerError::InvalidParams(format!("no session {session:?}")));
    }
    let win = app.backend.active_window(session)?;
    let panes = app.backend.panes(session)?;
    if let Some(p) = panes.iter().find(|p| p.window == win && p.role == SIDEBAR_ROLE) {
        if is_collapsed(p.width) {
            app.backend.resize_pane(&p.id, app.settings.sidebar.width)?;
            app.backend.mark_focus(&p.id)?;
            app.backend.select_pane(session, &p.id)?;
            return Ok(true);
        }
        if p.active {
            app.backend.resize_pane(&p.id, SIDEBAR_COLLAPSED_WIDTH)?;
            // Hand focus back to the pane the user came from (release the modal lock first).
            let _ = app.backend.unlock_window(session, win);
            if app.backend.select_last_pane(session, win).is_err() {
                if let Some(main) = panes.iter().find(|q| q.window == win && q.role != SIDEBAR_ROLE) {
                    let _ = app.backend.select_pane(session, &main.id);
                }
            }
            return Ok(false);
        }
        app.backend.mark_focus(&p.id)?;
        app.backend.select_pane(session, &p.id)?;
        return Ok(true);
    }
    let (cwd, env) = sidebar_context(&app, session);
    let id = add_sidebar(&app, session, &cwd, &env, app.settings.sidebar.width, Some(win))?;
    app.backend.mark_focus(&id)?;
    app.backend.select_pane(session, &id)?;
    Ok(true)
}

pub async fn windows(shared: Shared, r: WorkspaceRef) -> R<Vec<canopy_proto::WindowRow>> {
    let app = shared.lock().await;
    let ws = resolve_ref(&app, &r)?;
    let session = app.session_name(&ws);
    if !app.backend.session_exists(&session)? {
        return Ok(Vec::new());
    }
    Ok(app.backend.windows(&session)?.into_iter().map(|w| canopy_proto::WindowRow { index: w.index, name: w.name, active: w.active, panes: w.panes }).collect())
}

pub async fn select_window(shared: Shared, r: WorkspaceRef, index: u32) -> R<()> {
    let app = shared.lock().await;
    let ws = resolve_ref(&app, &r)?;
    let session = app.session_name(&ws);
    app.backend.select_window(&session, index)?;
    Ok(())
}

pub async fn new_window(shared: Shared, r: WorkspaceRef, name: Option<String>) -> R<()> {
    let app = shared.lock().await;
    let ws = resolve_ref(&app, &r)?;
    let session = app.session_name(&ws);
    if !app.backend.session_exists(&session)? {
        return Err(ManagerError::InvalidParams("session is not running; attach first".into()));
    }
    let cfg = project_config(&ws.project_root).unwrap_or_default();
    let env = workspace_env(&app, &ws, &env_prefixes(&app, &cfg), true);
    let name = name.unwrap_or_else(|| "shell".into());
    let role = format!("terminal:{name}");
    app.backend.open_window(&session, &name, PaneSpec { role: &role, command: "", cwd: &ws.path, env: &env, split: None, split_of: None, size_percent: None, size_cells: None, keep_alive: true, full_span: false, window: None })?;
    sidebar_for_last_window(&app, &session, &ws.path, &env);
    let wins = app.backend.windows(&session)?;
    if let Some(w) = wins.last() {
        let _ = app.backend.select_window(&session, w.index);
    }
    Ok(())
}

/// Give the most recently created window a sidebar, keeping focus on its main pane.
fn sidebar_for_last_window(app: &App, session: &str, cwd: &Path, env: &BTreeMap<String, String>) {
    if !app.settings.sidebar.enabled {
        return;
    }
    let Ok(wins) = app.backend.windows(session) else { return };
    let Some(last) = wins.last() else { return };
    let main_pane = app.backend.panes(session).ok().and_then(|ps| ps.into_iter().find(|p| p.window == last.index && p.role != SIDEBAR_ROLE).map(|p| p.id));
    if let Err(e) = add_sidebar(app, session, cwd, env, app.settings.sidebar.width, Some(last.index)) {
        tracing::debug!(error = %e, "sidebar for new window");
    }
    if let Some(p) = main_pane {
        let _ = app.backend.select_pane(session, &p);
    }
}

// ---------------------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------------------

struct GhRef {
    branch: String,
    body: String,
    author: String,
}

fn gh_json(cwd: &Path, args: &[&str]) -> R<serde_json::Value> {
    let out = std::process::Command::new("gh").args(args).current_dir(cwd).output()?;
    if !out.status.success() {
        return Err(ManagerError::Other(format!("gh {}: {}", args.join(" "), String::from_utf8_lossy(&out.stderr).trim())));
    }
    serde_json::from_slice(&out.stdout).map_err(|e| ManagerError::Other(format!("gh output: {e}")))
}

fn gh_pr(root: &Path, n: u64) -> R<GhRef> {
    let v = gh_json(root, &["pr", "view", &n.to_string(), "--json", "headRefName,body,author"])?;
    Ok(GhRef {
        branch: v["headRefName"].as_str().unwrap_or_default().to_string(),
        body: v["body"].as_str().unwrap_or_default().to_string(),
        author: v["author"]["login"].as_str().unwrap_or_default().to_string(),
    })
}

fn gh_issue(root: &Path, n: u64) -> R<GhRef> {
    let v = gh_json(root, &["issue", "view", &n.to_string(), "--json", "title,body"])?;
    let title = v["title"].as_str().unwrap_or_default();
    let body = format!("# {title}\n\n{}", v["body"].as_str().unwrap_or_default());
    Ok(GhRef { branch: String::new(), body, author: String::new() })
}

pub async fn create(shared: Shared, req: WorkspaceCreate) -> R<WorkspaceRow> {
    // ---- Phase 1: register under lock ------------------------------------------------
    let (ws, cfg, project, start_session, prompt, progress) = {
        let mut app = shared.lock().await;
        let project = ensure_project(&mut app, &req.project_root)?;
        let cfg = project_config(&project.root)?;
        let sources = [req.pr.is_some(), req.issue.is_some(), req.branch.is_some()].iter().filter(|b| **b).count();
        if sources > 1 {
            return Err(ManagerError::InvalidParams("pr, issue and branch are mutually exclusive".into()));
        }
        if let Some(a) = &req.agent {
            launcher::resolve(a, &cfg.agent.kind, &app.settings.agent.kind, &cfg.agents)?;
        }

        // Source-specific info (gh calls are quick; done here so the name can follow the PR branch).
        let mut source_kind = SourceKind::Fresh;
        let mut source_context = String::new();
        let mut owner = String::new();
        let mut branch_from_source: Option<String> = None;
        let mut create_branch = true;
        if let Some(n) = req.pr {
            let pr = gh_pr(&project.root, n)?;
            source_kind = SourceKind::Pr;
            source_context = pr.body;
            owner = pr.author;
            branch_from_source = Some(pr.branch);
            create_branch = false;
        } else if let Some(n) = req.issue {
            let issue = gh_issue(&project.root, n)?;
            source_kind = SourceKind::Issue;
            source_context = issue.body;
        } else if let Some(b) = &req.branch {
            source_kind = SourceKind::Branch;
            branch_from_source = Some(b.clone());
            create_branch = false;
        }

        let auto = req.name.is_none();
        let name = match &req.name {
            Some(n) => {
                if !namegen::is_valid(n) {
                    return Err(ManagerError::InvalidParams(format!("invalid workspace name {n:?}: lowercase letters, digits, - _ . only")));
                }
                n.clone()
            }
            None => match &branch_from_source {
                Some(b) => coregit::safe_name(b),
                None => namegen::unique(|c| app.state.find(&project.root, c).is_some()),
            },
        };
        if app.state.find(&project.root, &name).is_some() {
            return Err(ManagerError::WorkspaceExists(name));
        }
        let branch = match branch_from_source {
            Some(b) => b,
            None => coregit::sanitize_branch(&name).ok_or_else(|| ManagerError::InvalidParams(format!("cannot derive a branch from {name:?}")))?,
        };
        if create_branch && git::branch_exists(&project.root, &branch) {
            return Err(ManagerError::InvalidParams(format!("branch {branch:?} already exists; use branch = \"{branch}\" to check it out")));
        }

        let plan = app.settings.ports;
        let port = plan.allocate_workspace(project.port_base, &app.state.used_ports(), tcp_probe)?;
        let id = app.state.new_id();
        let ws = Workspace {
            id: id.clone(),
            project_root: project.root.clone(),
            name: name.clone(),
            branch,
            path: app.paths.workspace_dir(&project.name, &name),
            port,
            status: Status::SettingUp,
            created_at: now_rfc3339(),
            agent: req.agent.clone().unwrap_or_default(),
            source_kind,
            name_auto_generated: auto && source_kind == SourceKind::Fresh,
            source_context,
            source_number: req.pr.or(req.issue),
            owner,
            ..Workspace::default()
        };
        app.state.add(ws.clone())?;
        let progress = {
            let rt = app.runtime_mut(&id);
            rt.busy = true;
            rt.progress.clone()
        };
        scripts::set_progress(&Some(progress.clone()), "preparing worktree…");
        app.persist()?;
        app.events.publish(EventKind::WorkspaceCreated { workspace_id: id });
        (ws, cfg, project, req.start_session.unwrap_or(true), req.prompt.clone(), progress)
    };
    let (ws, cfg, project, start_session, prompt, progress) = (ws, cfg, project, start_session, prompt, progress);
    let prefixes = env_prefixes(&*shared.lock().await, &cfg);

    // ---- Phase 2: slow work, no lock -------------------------------------------------
    let ws2 = ws.clone();
    let cfg2 = cfg.clone();
    let create_branch = ws.source_kind == SourceKind::Fresh || ws.source_kind == SourceKind::Issue;
    let project_root = project.root.clone();
    let log_file = shared.lock().await.paths.log_dir().join(format!("{}-{}.log", project.name, ws.name));
    let progress2 = Some(progress.clone());
    let started = std::time::Instant::now();
    let log2 = log_file.clone();
    let phase2: R<()> = tokio::task::spawn_blocking(move || -> R<()> {
        scripts::log_run_header(&log2, &format!("creating {} on branch {} (port {}) at {}", ws2.name, ws2.branch, ws2.port, ws2.path.display()));
        if !git::has_commits(&project_root) {
            return Err(ManagerError::InvalidParams(format!("{} has no commits yet; make an initial commit before creating workspaces", project_root.display())));
        }
        std::fs::create_dir_all(ws2.path.parent().unwrap_or(&ws2.path))?;
        let default = git::default_branch(&project_root);
        let has_origin = git::has_remote(&project_root, "origin");
        if has_origin {
            scripts::set_progress(&progress2, "fetching origin…");
            scripts::log_note(&log2, "git fetch origin");
            if let Err(e) = git::fetch(&project_root) {
                tracing::warn!(error = %e, "git fetch failed; continuing with local refs");
                scripts::log_note(&log2, &format!("fetch failed, using local refs: {e}"));
            }
        }
        let start_point = if has_origin && git::remote_branch_exists(&project_root, &default) { format!("origin/{default}") } else { default.clone() };
        scripts::set_progress(&progress2, &format!("adding worktree from {start_point}…"));
        scripts::log_note(&log2, &format!("git worktree add from {start_point}"));
        git::worktree_add(&project_root, &ws2.path, &ws2.branch, create_branch, &start_point)?;
        if cfg2.scripts.setup.is_empty() {
            scripts::log_note(&log2, "no scripts.setup configured; skipping setup");
        } else {
            scripts::set_progress(&progress2, &format!("running {}…", cfg2.scripts.setup));
            scripts::log_note(&log2, &format!("running {} (timeout {}s)", cfg2.scripts.setup, cfg2.scripts.timeouts.setup));
            let env = WorkspaceEnv {
                workspace_path: &ws2.path,
                root_path: &project_root,
                port: ws2.port,
                workspace_name: &ws2.name,
                branch: &ws2.branch,
                project: project_root.file_name().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default().as_str(),
                workspace_id: &ws2.id,
                socket_path: None,
                extra_prefixes: &prefixes,
            }
            .vars();
            scripts::run(scripts::ScriptRun {
                project_root: &project_root,
                script: &cfg2.scripts.setup,
                cwd: &ws2.path,
                env: &env,
                timeout_secs: cfg2.scripts.timeouts.setup,
                log_file: Some(&log2),
                progress: progress2.clone(),
            })
            .map_err(|e| ManagerError::Setup(e.to_string()))?;
        }
        scripts::log_note(&log2, &format!("setup finished in {}s", started.elapsed().as_secs()));
        Ok(())
    })
    .await
    .map_err(|e| ManagerError::Other(format!("join: {e}")))?;

    if let Err(e) = phase2 {
        scripts::log_note(&log_file, &format!("FAILED after {}s: {}", started.elapsed().as_secs(), e.to_string().lines().next().unwrap_or_default()));
        scripts::set_progress(&Some(progress.clone()), "");
        mark_broken(&shared, &ws.id, &e.to_string()).await;
        return Err(e);
    }

    let session_result: R<()> = if start_session {
        scripts::set_progress(&Some(progress.clone()), "starting session…");
        scripts::log_note(&log_file, "starting session");
        let app = shared.lock().await;
        let plan = plan_session(&app, &ws, &cfg, false, false);
        drop(app);
        match plan {
            Ok(p) => {
                let app = shared.lock().await;
                let r = build_session(&app, &p);
                drop(app);
                if r.is_ok() {
                    if let Some(prompt) = prompt {
                        deliver_prompt(&shared, &ws.id, &p.session, &prompt).await;
                    }
                }
                r
            }
            Err(e) => Err(e),
        }
    } else {
        Ok(())
    };

    // ---- Phase 3: finalize under lock ----------------------------------------------
    let mut app = shared.lock().await;
    let status = match &session_result {
        Ok(()) if start_session => Status::Ready,
        Ok(()) => Status::Stopped,
        Err(_) => Status::Broken,
    };
    let err_text = session_result.as_ref().err().map(|e| e.to_string());
    match &err_text {
        Some(e) => scripts::log_note(&log_file, &format!("session FAILED: {}", e.lines().next().unwrap_or_default())),
        None => scripts::log_note(&log_file, &format!("{} after {}s", if start_session { "ready" } else { "created (no session)" }, started.elapsed().as_secs())),
    }
    if let Some(w) = app.state.find_mut(&ws.project_root, &ws.name) {
        w.status = status;
        w.last_error = err_text.clone().unwrap_or_default();
        if start_session && status == Status::Ready {
            w.agent_launch_count += 1;
        }
    }
    let id = ws.id.clone();
    {
        let rt = app.runtime_mut(&id);
        rt.busy = false;
        rt.alive = status == Status::Ready;
        scripts::set_progress(&Some(rt.progress.clone()), "");
    }
    app.persist()?;
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: id.clone() });
    let w = app.state.by_id(&id).cloned().ok_or_else(|| ManagerError::WorkspaceNotFound(id.clone()))?;
    let row = app.row(&w);
    drop(app);
    session_result?;
    Ok(row)
}

/// A slice of the workspace log for clients that tail it (see `Method::WorkspaceLog`).
/// Returns `(path, text, offset_after, running, status)`.
pub async fn log(shared: Shared, r: WorkspaceRef, offset: Option<u64>, lines: Option<usize>) -> R<(PathBuf, String, u64, bool, Status)> {
    let (path, running, status) = {
        let app = shared.lock().await;
        let ws = resolve_ref(&app, &r)?;
        let path = app.paths.log_dir().join(format!("{}-{}.log", app.project_name(&ws.project_root), ws.name));
        (path, ws.status == Status::SettingUp || app.is_busy(&ws.id), ws.status)
    };
    let (text, end) = read_log_slice(&path, offset, lines.unwrap_or(200));
    Ok((path, text, end, running, status))
}

/// `offset = None`: the last `lines` lines. `offset = Some(n)`: bytes `n..` (clamped).
/// Either way the returned offset is the file length, i.e. where the next read starts.
pub fn read_log_slice(path: &Path, offset: Option<u64>, lines: usize) -> (String, u64) {
    let data = std::fs::read(path).unwrap_or_default();
    let len = data.len() as u64;
    match offset {
        Some(o) => {
            let o = o.min(len) as usize;
            (String::from_utf8_lossy(&data[o..]).into_owned(), len)
        }
        None => {
            let s = String::from_utf8_lossy(&data);
            let v: Vec<&str> = s.lines().collect();
            let start = v.len().saturating_sub(lines);
            let mut text = v[start..].join("\n");
            if !text.is_empty() && s.ends_with('\n') {
                text.push('\n');
            }
            (text, len)
        }
    }
}

async fn mark_broken(shared: &Shared, id: &str, error: &str) {
    let mut app = shared.lock().await;
    let hint = scripts::diagnose(error);
    if let Some(w) = app.state.workspaces.iter_mut().find(|w| w.id == id) {
        w.status = Status::Broken;
        w.last_error = error.to_string();
        w.last_error_hint = hint;
    }
    app.runtime_mut(id).busy = false;
    if let Err(e) = app.persist() {
        tracing::error!(error = %e, "persist after failure");
    }
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: id.to_string() });
}

/// Type the opening prompt into the agent pane once the agent is rendering. Failure is
/// logged, never fatal: the workspace is fine, only the prompt was not delivered.
async fn deliver_prompt(shared: &Shared, id: &str, session: &str, prompt: &str) {
    let (backend, launcher_name) = {
        let app = shared.lock().await;
        let ws = app.state.by_id(id).cloned();
        (app.backend.clone(), ws.map(|w| app.launcher_name_for(&w)).unwrap_or_default())
    };
    let deadline = std::time::Instant::now() + Duration::from_secs(20);
    let pane = loop {
        let panes = backend.panes(session).unwrap_or_default();
        let agent = panes.into_iter().find(|p| p.role.starts_with("agent:"));
        if let Some(p) = agent {
            let screen = backend.read_screen(&p.id, 40).unwrap_or_default();
            if crate::agent::is_agent_rendering(&launcher_name, &screen) {
                // Dismiss claude's trust-folder dialog if it is up.
                if screen.contains("Yes, I trust this folder") || screen.contains("Enter to confirm") {
                    let _ = backend.send_text(&p.id, "", true);
                    tokio::time::sleep(Duration::from_millis(1500)).await;
                    continue;
                }
                if launcher_name != "claude" || screen.contains('❯') {
                    break Some(p.id);
                }
            }
        }
        if std::time::Instant::now() >= deadline {
            break None;
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    };
    match pane {
        Some(p) => {
            if let Err(e) = backend.send_text(&p, prompt, false) {
                tracing::warn!(error = %e, "prompt delivery failed");
                return;
            }
            tokio::time::sleep(Duration::from_millis(300)).await;
            let _ = backend.send_text(&p, "", true);
        }
        None => tracing::warn!(workspace = id, "agent never became ready; prompt not delivered"),
    }
}

// ---------------------------------------------------------------------------------------
// Remove / stop / retry / resurrect
// ---------------------------------------------------------------------------------------

pub async fn remove(shared: Shared, r: WorkspaceRef, force: bool) -> R<()> {
    let (ws, cfg, session, backend, log_file, bpath, prefixes) = {
        let mut app = shared.lock().await;
        let ws = resolve_ref(&app, &r)?;
        if app.is_busy(&ws.id) {
            return Err(ManagerError::Busy(ws.name, "operation in flight"));
        }
        if !force && ws.path.is_dir() {
            let blockers = removal_blockers(&app, &ws);
            if !blockers.is_empty() {
                return Err(ManagerError::RemovalBlocked(blockers.join("; ")));
            }
        }
        let cfg = project_config(&ws.project_root).unwrap_or_default();
        let prefixes = env_prefixes(&app, &cfg);
        let session = app.session_name(&ws);
        // Drop the row first so a double-press cannot double-run the teardown.
        app.state.remove(&ws.project_root, &ws.name)?;
        app.runtime.remove(&ws.id);
        app.agents.forget(&ws.id);
        app.persist()?;
        app.events.publish(EventKind::WorkspaceRemoved { workspace_id: ws.id.clone() });
        let log_file = app.paths.log_dir().join(format!("{}-{}.log", app.project_name(&ws.project_root), ws.name));
        (ws.clone(), cfg, session, app.backend.clone(), log_file, briefing_path(&app, &ws.id), prefixes)
    };
    let (ws, cfg, session, backend, log_file, bpath, prefixes) = (ws, cfg, session, backend, log_file, bpath, prefixes);
    tokio::task::spawn_blocking(move || {
        if !cfg.scripts.archive.is_empty() && ws.path.is_dir() {
            let env = WorkspaceEnv {
                workspace_path: &ws.path,
                root_path: &ws.project_root,
                port: ws.port,
                workspace_name: &ws.name,
                branch: &ws.branch,
                project: "",
                workspace_id: &ws.id,
                socket_path: None,
                extra_prefixes: &prefixes,
            }
            .vars();
            if let Err(e) = scripts::run(scripts::ScriptRun {
                project_root: &ws.project_root,
                script: &cfg.scripts.archive,
                cwd: &ws.path,
                env: &env,
                timeout_secs: cfg.scripts.timeouts.archive,
                log_file: Some(&log_file),
                progress: None,
            }) {
                tracing::warn!(error = %e, "archive script failed; continuing removal");
            }
        }
        if backend.session_exists(&session).unwrap_or(false) {
            if let Err(e) = backend.kill_session(&session) {
                tracing::warn!(error = %e, "kill session");
            }
        }
        if ws.path.exists() {
            if let Err(e) = git::worktree_remove(&ws.project_root, &ws.path, true) {
                tracing::warn!(error = %e, "worktree remove failed; deleting directory");
                let _ = std::fs::remove_dir_all(&ws.path);
                let _ = std::process::Command::new("git").arg("-C").arg(&ws.project_root).args(["worktree", "prune"]).output();
            }
        } else {
            let _ = std::process::Command::new("git").arg("-C").arg(&ws.project_root).args(["worktree", "prune"]).output();
        }
        if git::branch_exists(&ws.project_root, &ws.branch) {
            if let Err(e) = git::branch_delete(&ws.project_root, &ws.branch) {
                tracing::warn!(error = %e, "branch delete");
            }
        }
        let _ = std::fs::remove_file(&bpath);
    })
    .await
    .map_err(|e| ManagerError::Other(format!("join: {e}")))?;
    Ok(())
}

/// Why removal might lose work. Each check is independent and best-effort.
fn removal_blockers(app: &App, ws: &Workspace) -> Vec<String> {
    let default = git::default_branch(&ws.project_root);
    let st = git::stats(&ws.path, &default);
    let mut v = Vec::new();
    let merged = app.runtime_of(&ws.id).pr.as_ref().is_some_and(|p| p.state == "MERGED") || git::upstream_exists(&ws.path) == Some(false);
    if merged {
        return v;
    }
    if st.dirty_tracked > 0 {
        v.push(format!("{} uncommitted change(s)", st.dirty_tracked));
    }
    match st.unpushed {
        Some(n) if n > 0 => v.push(format!("{n} unpushed commit(s)")),
        None if st.ahead > 0 => v.push(format!("{} commit(s) not on origin/{default} and no upstream", st.ahead)),
        _ => {}
    }
    if let Some(pr) = &app.runtime_of(&ws.id).pr {
        if pr.state == "OPEN" {
            v.push(format!("open PR #{}", pr.number));
        }
    }
    v
}

pub async fn stop(shared: Shared, r: WorkspaceRef) -> R<()> {
    let mut app = shared.lock().await;
    let ws = resolve_ref(&app, &r)?;
    let session = app.session_name(&ws);
    if app.backend.session_exists(&session)? {
        app.backend.kill_session(&session)?;
    }
    if let Some(w) = app.state.find_mut(&ws.project_root, &ws.name) {
        if w.status == Status::Ready {
            w.status = Status::Stopped;
        }
    }
    app.runtime_mut(&ws.id).alive = false;
    app.persist()?;
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: ws.id });
    Ok(())
}

pub async fn retry(shared: Shared, r: WorkspaceRef, force: bool) -> R<WorkspaceRow> {
    let (ws, cfg, log_file, prefixes, progress) = {
        let mut app = shared.lock().await;
        let ws = resolve_ref(&app, &r)?;
        match ws.status {
            Status::Broken => {}
            Status::SettingUp => return Err(ManagerError::Busy(ws.name, "still setting up")),
            Status::Orphaned => return Err(ManagerError::InvalidParams("workspace directory is gone; run rm".into())),
            _ if !force => return Err(ManagerError::InvalidParams(format!("workspace is {}; pass force to re-run setup anyway", ws.status.as_str()))),
            _ => {}
        }
        if app.is_busy(&ws.id) {
            return Err(ManagerError::Busy(ws.name, "operation in flight"));
        }
        let cfg = project_config(&ws.project_root)?;
        let progress = {
            let rt = app.runtime_mut(&ws.id);
            rt.busy = true;
            rt.progress.clone()
        };
        scripts::set_progress(&Some(progress.clone()), "re-running setup…");
        if let Some(w) = app.state.find_mut(&ws.project_root, &ws.name) {
            w.status = Status::SettingUp;
            w.created_at = now_rfc3339();
        }
        app.persist()?;
        app.events.publish(EventKind::WorkspaceUpdated { workspace_id: ws.id.clone() });
        let log_file = app.paths.log_dir().join(format!("{}-{}.log", app.project_name(&ws.project_root), ws.name));
        let prefixes = env_prefixes(&app, &cfg);
        (ws, cfg, log_file, prefixes, progress)
    };
    let (ws, cfg, log_file, prefixes, progress) = (ws, cfg, log_file, prefixes, progress);
    let ws2 = ws.clone();
    let cfg2 = cfg.clone();
    let log2 = log_file.clone();
    let progress2 = Some(progress.clone());
    let started = std::time::Instant::now();
    let res: R<()> = tokio::task::spawn_blocking(move || -> R<()> {
        scripts::log_run_header(&log2, &format!("retrying setup for {} (port {})", ws2.name, ws2.port));
        if !ws2.path.is_dir() {
            // Worktree missing but branch may exist: recreate the checkout.
            scripts::set_progress(&progress2, "recreating worktree…");
            scripts::log_note(&log2, "worktree missing; recreating it");
            let create = !git::branch_exists(&ws2.project_root, &ws2.branch);
            let default = git::default_branch(&ws2.project_root);
            git::worktree_add(&ws2.project_root, &ws2.path, &ws2.branch, create, &default)?;
        }
        if cfg2.scripts.setup.is_empty() {
            scripts::log_note(&log2, "no scripts.setup configured; nothing to re-run");
        } else {
            scripts::set_progress(&progress2, &format!("running {}…", cfg2.scripts.setup));
            scripts::log_note(&log2, &format!("running {} (timeout {}s)", cfg2.scripts.setup, cfg2.scripts.timeouts.setup));
            let env = WorkspaceEnv {
                workspace_path: &ws2.path,
                root_path: &ws2.project_root,
                port: ws2.port,
                workspace_name: &ws2.name,
                branch: &ws2.branch,
                project: "",
                workspace_id: &ws2.id,
                socket_path: None,
                extra_prefixes: &prefixes,
            }
            .vars();
            scripts::run(scripts::ScriptRun {
                project_root: &ws2.project_root,
                script: &cfg2.scripts.setup,
                cwd: &ws2.path,
                env: &env,
                timeout_secs: cfg2.scripts.timeouts.setup,
                log_file: Some(&log2),
                progress: progress2.clone(),
            })
            .map_err(|e| ManagerError::Setup(e.to_string()))?;
        }
        scripts::log_note(&log2, &format!("setup finished in {}s", started.elapsed().as_secs()));
        Ok(())
    })
    .await
    .map_err(|e| ManagerError::Other(format!("join: {e}")))?;
    scripts::set_progress(&Some(progress.clone()), "");
    if let Err(e) = res {
        scripts::log_note(&log_file, &format!("FAILED after {}s: {}", started.elapsed().as_secs(), e.to_string().lines().next().unwrap_or_default()));
        mark_broken(&shared, &ws.id, &e.to_string()).await;
        return Err(e);
    }
    let mut app = shared.lock().await;
    let session = app.session_name(&ws);
    let alive = app.backend.session_exists(&session).unwrap_or(false);
    if let Some(w) = app.state.find_mut(&ws.project_root, &ws.name) {
        w.status = if alive { Status::Ready } else { Status::Stopped };
        w.last_error.clear();
        w.last_error_hint.clear();
    }
    {
        let rt = app.runtime_mut(&ws.id);
        rt.busy = false;
        rt.alive = alive;
    }
    app.persist()?;
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: ws.id.clone() });
    let w = app.state.by_id(&ws.id).cloned().ok_or_else(|| ManagerError::WorkspaceNotFound(ws.id.clone()))?;
    Ok(app.row(&w))
}

/// Rebuild the session for a `stopped` workspace. Does not re-run setup; the agent resumes.
pub async fn resurrect(shared: Shared, r: WorkspaceRef) -> R<WorkspaceRow> {
    let (ws, plan) = {
        let mut app = shared.lock().await;
        let ws = resolve_ref(&app, &r)?;
        if !ws.path.is_dir() {
            if let Some(w) = app.state.find_mut(&ws.project_root, &ws.name) {
                w.status = Status::Orphaned;
            }
            app.persist()?;
            return Err(ManagerError::InvalidParams(format!("workspace directory {} is gone; run rm", ws.path.display())));
        }
        if ws.status == Status::Broken {
            return Err(ManagerError::InvalidParams("workspace is broken; run retry first".into()));
        }
        if app.is_busy(&ws.id) {
            return Err(ManagerError::Busy(ws.name, "operation in flight"));
        }
        let session = app.session_name(&ws);
        if app.backend.session_exists(&session)? {
            // Already alive: just make sure state agrees.
            if let Some(w) = app.state.find_mut(&ws.project_root, &ws.name) {
                w.status = Status::Ready;
            }
            app.runtime_mut(&ws.id).alive = true;
            app.persist()?;
            let w = app.state.by_id(&ws.id).cloned().unwrap();
            return Ok(app.row(&w));
        }
        let cfg = project_config(&ws.project_root).unwrap_or_default();
        let plan = plan_session(&app, &ws, &cfg, true, false)?;
        app.runtime_mut(&ws.id).busy = true;
        (ws, plan)
    };
    let result = {
        let app = shared.lock().await;
        build_session(&app, &plan)
    };
    let mut app = shared.lock().await;
    {
        let rt = app.runtime_mut(&ws.id);
        rt.busy = false;
        rt.alive = result.is_ok();
    }
    if let Some(w) = app.state.find_mut(&ws.project_root, &ws.name) {
        if result.is_ok() {
            w.status = Status::Ready;
            w.agent_launch_count += 1;
            w.last_error.clear();
        } else {
            w.last_error = result.as_ref().err().map(|e| e.to_string()).unwrap_or_default();
        }
    }
    app.persist()?;
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: ws.id.clone() });
    result?;
    let w = app.state.by_id(&ws.id).cloned().ok_or_else(|| ManagerError::WorkspaceNotFound(ws.id.clone()))?;
    Ok(app.row(&w))
}

// ---------------------------------------------------------------------------------------
// Rename (branch follows), reconcile
// ---------------------------------------------------------------------------------------

/// Make the stored branch and the session name follow the live git branch. Returns true
/// when something changed. Session rename happens first (tmux is the liveness oracle);
/// a name collision leaves the stale label in place rather than half-applying.
pub fn sync_branch(app: &mut App, id: &str) -> R<bool> {
    let Some(ws) = app.state.by_id(id).cloned() else { return Ok(false) };
    if ws.pinned || !ws.path.is_dir() {
        return Ok(false);
    }
    let Some(live) = git::current_branch(&ws.path)? else { return Ok(false) };
    if live == ws.branch {
        return Ok(false);
    }
    let old_session = app.session_name(&ws);
    let mut next = ws.clone();
    next.branch = live.clone();
    let new_session = app.session_name(&next);
    if app.backend.session_exists(&old_session)? {
        if app.backend.session_exists(&new_session)? {
            tracing::warn!(old = %old_session, new = %new_session, "session name in use; keeping stale label");
            return Ok(false);
        }
        app.backend.rename_session(&old_session, &new_session)?;
    }
    if let Some(w) = app.state.workspaces.iter_mut().find(|w| w.id == id) {
        w.branch = live;
    }
    app.persist()?;
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: id.to_string() });
    Ok(true)
}

pub async fn rename(shared: Shared, r: WorkspaceRef, pin: Option<bool>) -> R<WorkspaceRow> {
    let mut app = shared.lock().await;
    let ws = resolve_ref(&app, &r)?;
    if let Some(p) = pin {
        if let Some(w) = app.state.find_mut(&ws.project_root, &ws.name) {
            w.pinned = p;
        }
        app.persist()?;
    }
    sync_branch(&mut app, &ws.id)?;
    let w = app.state.by_id(&ws.id).cloned().ok_or_else(|| ManagerError::WorkspaceNotFound(ws.id.clone()))?;
    Ok(app.row(&w))
}

pub async fn set_owner(shared: Shared, r: WorkspaceRef, owner: String) -> R<WorkspaceRow> {
    let mut app = shared.lock().await;
    let ws = resolve_ref(&app, &r)?;
    if let Some(w) = app.state.find_mut(&ws.project_root, &ws.name) {
        w.owner = owner;
    }
    app.persist()?;
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: ws.id.clone() });
    let w = app.state.by_id(&ws.id).cloned().unwrap();
    Ok(app.row(&w))
}

/// Observe disk + backend truth for every row and update statuses. Never deletes rows.
pub fn reconcile(app: &mut App, project_root: Option<&Path>) -> R<Vec<WorkspaceRow>> {
    let ids: Vec<String> = app
        .state
        .workspaces
        .iter()
        .filter(|w| project_root.is_none_or(|r| w.project_root == r))
        .map(|w| w.id.clone())
        .collect();
    let mut changed = false;
    for id in &ids {
        if app.is_busy(id) {
            continue;
        }
        if let Err(e) = sync_branch(app, id) {
            tracing::debug!(error = %e, workspace = %id, "sync branch");
        }
        let Some(ws) = app.state.by_id(id).cloned() else { continue };
        let session = app.session_name(&ws);
        let alive = app.backend.session_exists(&session).unwrap_or(false);
        let attached = if alive { app.backend.attached_clients(&session).unwrap_or(0) > 0 } else { false };
        let next = match ws.status {
            Status::SettingUp => {
                if created_age(&ws).is_some_and(|a| a > SETTING_UP_STALE) { Status::Broken } else { Status::SettingUp }
            }
            Status::Broken => Status::Broken,
            _ if !ws.path.is_dir() => Status::Orphaned,
            _ if alive => Status::Ready,
            _ => Status::Stopped,
        };
        {
            let rt = app.runtime_mut(id);
            rt.alive = alive;
            rt.attached = attached;
        }
        if next != ws.status {
            if let Some(w) = app.state.workspaces.iter_mut().find(|w| w.id == *id) {
                w.status = next;
                if next == Status::Broken && w.last_error.is_empty() {
                    w.last_error = "setup never finished (stale for over 5 minutes)".into();
                }
            }
            changed = true;
            app.events.publish(EventKind::WorkspaceUpdated { workspace_id: id.clone() });
        }
    }
    if changed {
        app.persist()?;
    }
    Ok(app.rows(project_root))
}

// ---------------------------------------------------------------------------------------
// Attach / run / main
// ---------------------------------------------------------------------------------------

pub async fn attach_target(shared: Shared, r: WorkspaceRef) -> R<AttachTarget> {
    let ws = {
        let mut app = shared.lock().await;
        let ws = resolve_ref(&app, &r)?;
        reconcile(&mut app, Some(&ws.project_root.clone()))?;
        app.state.by_id(&ws.id).cloned().ok_or(ManagerError::WorkspaceNotFound(ws.id))?
    };
    match ws.status {
        Status::Ready => {}
        Status::Stopped => {
            resurrect(shared.clone(), WorkspaceRef::Id { id: ws.id.clone() }).await?;
        }
        Status::SettingUp => return Err(ManagerError::Busy(ws.name, "still setting up")),
        Status::Broken => return Err(ManagerError::InvalidParams(format!("workspace is broken: {}. Run retry or rm.", ws.last_error))),
        Status::Orphaned => return Err(ManagerError::InvalidParams("workspace directory is gone; run rm".into())),
    }
    let app = shared.lock().await;
    let ws = app.state.by_id(&ws.id).cloned().ok_or(ManagerError::WorkspaceNotFound(ws.id))?;
    Ok(AttachTarget::Tmux { session: app.session_name(&ws), detach_others: true })
}

pub async fn run_script(shared: Shared, r: WorkspaceRef, script: Option<String>) -> R<String> {
    let app = shared.lock().await;
    let ws = resolve_ref(&app, &r)?;
    let cfg = project_config(&ws.project_root)?;
    let named = cfg.scripts.run.named();
    let (name, rs) = match script {
        Some(n) => named.get(&n).cloned().map(|s| (n.clone(), s)).ok_or_else(|| ManagerError::InvalidParams(format!("no run script {n:?}; available: {}", named.keys().cloned().collect::<Vec<_>>().join(", "))))?,
        None => cfg.scripts.run.default_script().ok_or_else(|| ManagerError::InvalidParams("no run script configured (scripts.run in canopy.json)".into()))?,
    };
    let session = app.session_name(&ws);
    if !app.backend.session_exists(&session)? {
        return Err(ManagerError::InvalidParams("session is not running; attach first".into()));
    }
    let env = workspace_env(&app, &ws, &env_prefixes(&app, &cfg), true);
    let cwd = if rs.cwd.is_empty() { ws.path.clone() } else { ws.path.join(&rs.cwd) };
    // `command` is a shell string (pipes, `&&`, env assignments are fine); `args` are quoted.
    let mut cmd = rs.command.clone();
    for a in &rs.args {
        cmd.push(' ');
        cmd.push_str(&launcher::sh_quote(a));
    }
    let role = format!("run:{name}");
    // Reuse an existing run pane for this script if present.
    if let Some(p) = app.backend.panes(&session)?.into_iter().find(|p| p.role == role) {
        app.backend.send_text(&p.id, &cmd, true)?;
    } else {
        app.backend.open_window(&session, &role, PaneSpec { role: &role, command: &cmd, cwd: &cwd, env: &env, split: None, split_of: None, size_percent: None, size_cells: None, keep_alive: true, full_span: false, window: None })?;
        sidebar_for_last_window(&app, &session, &ws.path, &env);
    }
    app.events.publish(EventKind::RunStarted { workspace_id: ws.id.clone(), script: name.clone() });
    Ok(name)
}

/// Project rows with main-session facts, sorted by name.
pub fn project_rows(app: &App) -> Vec<canopy_proto::ProjectRow> {
    let mut rows: Vec<canopy_proto::ProjectRow> = app
        .state
        .projects
        .values()
        .map(|p| {
            let session = app.main_session_name(&p.root);
            let alive = app.backend.session_exists(&session).unwrap_or(false);
            canopy_proto::ProjectRow {
                root: p.root.clone(),
                name: p.name.clone(),
                port_base: p.port_base,
                workspace_count: app.state.workspaces.iter().filter(|w| w.project_root == p.root).count(),
                main_alive: alive,
                main_attached: alive && app.backend.attached_clients(&session).unwrap_or(0) > 0,
                main_branch: git::current_branch(&p.root).ok().flatten().unwrap_or_default(),
                main_session: session,
            }
        })
        .collect();
    rows.sort_by(|a, b| a.name.cmp(&b.name));
    rows
}

pub async fn stop_main(shared: Shared, root: &Path) -> R<()> {
    let app = shared.lock().await;
    let root = canonical_root(root).unwrap_or_else(|_| root.to_path_buf());
    let session = app.main_session_name(&root);
    if app.backend.session_exists(&session)? {
        app.backend.kill_session(&session)?;
    }
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: format!("main:{}", app.project_name(&root)) });
    Ok(())
}

/// Ensure `<project>/main` exists (editor + agent + shell at the repo root, port = project base).
pub async fn main_target(shared: Shared, root: &Path) -> R<AttachTarget> {
    let mut app = shared.lock().await;
    let project = ensure_project(&mut app, root)?;
    let session = app.main_session_name(&project.root);
    if !app.backend.session_exists(&session)? {
        let cfg = project_config(&project.root).unwrap_or_default();
        let mut pseudo = app.main_pseudo(&project.root).ok_or_else(|| ManagerError::ProjectNotFound(project.root.clone()))?;
        pseudo.branch = git::current_branch(&project.root)?.unwrap_or_else(|| "main".into());
        let plan = plan_session(&app, &pseudo, &cfg, true, true)?;
        build_session(&app, &plan)?;
        app.events.publish(EventKind::WorkspaceUpdated { workspace_id: pseudo.id.clone() });
    }
    Ok(AttachTarget::Tmux { session, detach_others: true })
}

// ---------------------------------------------------------------------------------------
// Projects: init (path or URL), remove, pickers
// ---------------------------------------------------------------------------------------

fn repo_name_from_url(url: &str) -> String {
    let trimmed = url.trim_end_matches('/').trim_end_matches(".git");
    trimmed.rsplit(['/', ':']).next().unwrap_or("repo").to_string()
}

/// Onboard a project. With `url`, clone into `<source_root>/<repo>` first. Writes
/// `canopy.json` when missing (adopting a legacy workspace config) and registers the project.
pub async fn project_init(shared: Shared, path: Option<PathBuf>, url: Option<String>, with_scripts: bool, adopt_from: Option<PathBuf>) -> R<Project> {
    let (dest, source_root, candidates) = {
        let app = shared.lock().await;
        let source_root = app.settings.source_root().map(PathBuf::from).unwrap_or_else(|| app.paths.home.join("sources"));
        let candidates = app.settings.init.adopt_from.clone();
        let dest = match (&path, &url) {
            (Some(p), _) => p.clone(),
            (None, Some(u)) => source_root.join(repo_name_from_url(u)),
            (None, None) => return Err(ManagerError::InvalidParams("pass a path or a git url".into())),
        };
        (dest, source_root, candidates)
    };
    if let Some(u) = &url {
        if u.starts_with('-') {
            return Err(ManagerError::InvalidParams("invalid url".into()));
        }
        if !dest.exists() {
            let u = u.clone();
            let dest2 = dest.clone();
            tokio::task::spawn_blocking(move || -> R<()> {
                std::fs::create_dir_all(&source_root)?;
                let out = std::process::Command::new("git").args(["clone", "--", &u]).arg(&dest2).output()?;
                if !out.status.success() {
                    return Err(ManagerError::Other(format!("git clone failed: {}", String::from_utf8_lossy(&out.stderr).trim())));
                }
                Ok(())
            })
            .await
            .map_err(|e| ManagerError::Other(format!("join: {e}")))??;
        }
    }
    let dest = dest.canonicalize().map_err(|_| ManagerError::InvalidParams(format!("{} does not exist", dest.display())))?;
    if !git::is_repo(&dest) {
        return Err(ManagerError::InvalidParams(format!("{} is not a git repository", dest.display())));
    }
    let root = git::root(&dest)?;
    let file = root.join(FILE_NAME);
    if !file.exists() {
        let source = adopt_from.map(|p| if p.is_absolute() { p } else { root.join(p) }).or_else(|| ProjectConfig::find_adoptable(&root, &candidates));
        let mut cfg = match source {
            Some(f) => ProjectConfig::adopt_from(&f)?,
            None => ProjectConfig::default(),
        };
        if with_scripts {
            write_stub_scripts(&root, &mut cfg)?;
        }
        std::fs::write(&file, cfg.to_json_pretty() + "\n")?;
    }
    let mut app = shared.lock().await;
    let p = ensure_project(&mut app, &root)?;
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: format!("project:{}", p.name) });
    Ok(p)
}

/// Stub lifecycle scripts under `bin/` (only files that do not exist yet).
pub fn write_stub_scripts(root: &Path, cfg: &mut ProjectConfig) -> R<()> {
    let bin = root.join("bin");
    std::fs::create_dir_all(&bin)?;
    for (name, body) in [
        ("canopy-setup", "#!/usr/bin/env bash\n# Runs once at workspace creation (cwd = workspace). Keep it idempotent: `canopy retry` re-runs it.\nset -euo pipefail\necho \"setup: $CANOPY_WORKSPACE_NAME on port $CANOPY_PORT\"\n"),
        ("canopy-run", "#!/usr/bin/env bash\n# Long-running dev server; launched on demand by `canopy run`. Bind to $CANOPY_PORT.\nset -euo pipefail\necho \"run: would start the dev server on $CANOPY_PORT\"\nexec sleep infinity\n"),
        ("canopy-archive", "#!/usr/bin/env bash\n# Runs at `canopy rm` before the worktree is deleted (drop databases, etc).\nset -euo pipefail\necho \"archive: $CANOPY_WORKSPACE_NAME\"\n"),
    ] {
        let p = bin.join(name);
        if !p.exists() {
            std::fs::write(&p, body)?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755))?;
            }
        }
    }
    if cfg.scripts.setup.is_empty() {
        cfg.scripts.setup = "bin/canopy-setup".into();
    }
    if cfg.scripts.run.is_empty() {
        cfg.scripts.run = canopy_core::config::RunScripts::Single("bin/canopy-run".into());
    }
    if cfg.scripts.archive.is_empty() {
        cfg.scripts.archive = "bin/canopy-archive".into();
    }
    Ok(())
}

pub async fn project_remove(shared: Shared, root: &Path) -> R<()> {
    let mut app = shared.lock().await;
    let root = root.canonicalize().unwrap_or_else(|_| root.to_path_buf());
    if app.state.project(&root).is_none() {
        return Err(ManagerError::ProjectNotFound(root));
    }
    let n = app.state.workspaces.iter().filter(|w| w.project_root == root).count();
    if n > 0 {
        return Err(ManagerError::ProjectHasWorkspaces(root, n));
    }
    let session = app.main_session_name(&root);
    if app.backend.session_exists(&session).unwrap_or(false) {
        let _ = app.backend.kill_session(&session);
    }
    app.state.projects.remove(&root);
    app.persist()?;
    app.events.publish(EventKind::WorkspaceUpdated { workspace_id: "project:removed".into() });
    Ok(())
}

fn in_use_branches(app: &App, root: &Path) -> std::collections::HashSet<String> {
    app.state.workspaces.iter().filter(|w| w.project_root == root).map(|w| w.branch.clone()).collect()
}

pub async fn pick_pull_requests(shared: Shared, root: &Path) -> R<Vec<canopy_proto::PickItem>> {
    let (root, used) = {
        let app = shared.lock().await;
        let root = canonical_root(root)?;
        (root.clone(), in_use_branches(&app, &root))
    };
    let v = tokio::task::spawn_blocking(move || gh_json(&root, &["pr", "list", "--state", "open", "--limit", "60", "--json", "number,title,headRefName,author,isDraft,updatedAt"]))
        .await
        .map_err(|e| ManagerError::Other(format!("join: {e}")))??;
    Ok(v.as_array()
        .into_iter()
        .flatten()
        .map(|pr| {
            let n = pr["number"].as_u64().unwrap_or(0);
            let branch = pr["headRefName"].as_str().unwrap_or("").to_string();
            let author = pr["author"]["login"].as_str().unwrap_or("");
            let draft = if pr["isDraft"].as_bool().unwrap_or(false) { " (draft)" } else { "" };
            canopy_proto::PickItem {
                key: n.to_string(),
                label: format!("#{n} {}{draft}", pr["title"].as_str().unwrap_or("")),
                detail: format!("@{author} · {branch}"),
                in_use: used.contains(&branch),
            }
        })
        .collect())
}

pub async fn pick_issues(shared: Shared, root: &Path) -> R<Vec<canopy_proto::PickItem>> {
    let root = canonical_root(root)?;
    let _ = shared;
    let v = tokio::task::spawn_blocking(move || gh_json(&root, &["issue", "list", "--state", "open", "--limit", "60", "--json", "number,title,author,updatedAt"]))
        .await
        .map_err(|e| ManagerError::Other(format!("join: {e}")))??;
    Ok(v.as_array()
        .into_iter()
        .flatten()
        .map(|is| {
            let n = is["number"].as_u64().unwrap_or(0);
            canopy_proto::PickItem {
                key: n.to_string(),
                label: format!("#{n} {}", is["title"].as_str().unwrap_or("")),
                detail: format!("@{}", is["author"]["login"].as_str().unwrap_or("")),
                in_use: false,
            }
        })
        .collect())
}

pub async fn pick_branches(shared: Shared, root: &Path) -> R<Vec<canopy_proto::PickItem>> {
    let (root, used) = {
        let app = shared.lock().await;
        let root = canonical_root(root)?;
        (root.clone(), in_use_branches(&app, &root))
    };
    let out = tokio::task::spawn_blocking(move || {
        let _ = git::fetch(&root);
        std::process::Command::new("git")
            .arg("-C")
            .arg(&root)
            .args(["for-each-ref", "--sort=-committerdate", "--count=200", "--format=%(refname:short)%09%(committerdate:relative)%09%(subject)", "refs/remotes/origin", "refs/heads"])
            .output()
    })
    .await
    .map_err(|e| ManagerError::Other(format!("join: {e}")))??;
    let text = String::from_utf8_lossy(&out.stdout);
    let mut seen = std::collections::HashSet::new();
    Ok(text
        .lines()
        .filter_map(|l| {
            let mut p = l.split('\t');
            let full = p.next()?.trim();
            // `origin/HEAD` shortens to plain `origin`; neither is a branch.
            if full.ends_with("/HEAD") || full == "origin" {
                return None;
            }
            let name = full.strip_prefix("origin/").unwrap_or(full).to_string();
            if !seen.insert(name.clone()) {
                return None;
            }
            let when = p.next().unwrap_or("").to_string();
            let subject = p.next().unwrap_or("").to_string();
            Some(canopy_proto::PickItem { key: name.clone(), label: name.clone(), detail: format!("{when} · {subject}"), in_use: used.contains(&name) })
        })
        .collect())
}

#[cfg(test)]
mod log_slice_tests {
    use super::read_log_slice;

    #[test]
    fn tail_then_offset_reads_only_new_bytes() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("w.log");
        std::fs::write(&p, "a\nb\nc\n").unwrap();
        let (tail, off) = read_log_slice(&p, None, 2);
        assert_eq!(tail, "b\nc\n");
        assert_eq!(off, 6);
        let (same, off2) = read_log_slice(&p, Some(off), 200);
        assert_eq!(same, "");
        assert_eq!(off2, 6);
        std::fs::write(&p, "a\nb\nc\nd\n").unwrap();
        let (new, off3) = read_log_slice(&p, Some(off), 200);
        assert_eq!(new, "d\n");
        assert_eq!(off3, 8);
    }

    #[test]
    fn missing_file_and_past_offset_are_empty() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("none.log");
        assert_eq!(read_log_slice(&p, None, 10), (String::new(), 0));
        std::fs::write(&p, "x\n").unwrap();
        assert_eq!(read_log_slice(&p, Some(99), 10), (String::new(), 2));
    }
}
