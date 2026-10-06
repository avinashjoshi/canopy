//! `App`: everything the server knows. `state` is the persisted truth; `runtime` is what
//! the pollers derive (liveness, attachment, hints, agent state) and is never written to disk.

use crate::agent;
use crate::backend::SessionBackend;
use crate::events::EventHub;
use crate::hints::PrStatus;
use canopy_core::paths::Paths;
use canopy_core::settings::{Backend, Settings};
use canopy_core::state::{State, StateError, Store, Workspace};
use canopy_proto::{AgentState, Hint, WorkspaceRow};
use std::collections::HashMap;
use std::path::Path;
use std::sync::Arc;
use std::time::Instant;

#[derive(Debug, Clone, Default)]
pub struct RuntimeInfo {
    pub alive: bool,
    pub attached: bool,
    pub hints: Vec<Hint>,
    pub pr: Option<PrStatus>,
    pub pr_checked: Option<Instant>,
    pub hints_checked: Option<Instant>,
    /// A create/remove/retry is in flight; other mutations must wait.
    pub busy: bool,
    /// What the in-flight operation is doing right now (last setup output line or phase).
    pub progress: crate::scripts::Progress,
    pub mem_rss_bytes: u64,
    pub cpu_percent: u16,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ShutdownReason {
    Signal,
    ApiStop,
}

pub struct App {
    pub paths: Paths,
    pub settings: Settings,
    pub store: Store,
    pub state: State,
    pub backend: Arc<dyn SessionBackend>,
    pub events: Arc<EventHub>,
    pub agents: agent::Tracker,
    pub runtime: HashMap<String, RuntimeInfo>,
    pub started: Instant,
    pub shutdown: Option<ShutdownReason>,
}

pub type Shared = Arc<tokio::sync::Mutex<App>>;

impl App {
    pub fn load(paths: Paths) -> anyhow::Result<Self> {
        std::fs::create_dir_all(&paths.home)?;
        let settings = Settings::load(&paths.settings_file())?;
        let store = Store::new(&paths);
        let state = store.load()?;
        let backend: Arc<dyn SessionBackend> = match settings.backend {
            // `CANOPY_TMUX_SOCKET` scopes the server to a named tmux socket (tests, dogfooding).
            Backend::Tmux => match std::env::var("CANOPY_TMUX_SOCKET") {
                Ok(name) if !name.is_empty() => Arc::new(crate::backend::tmux::Tmux::with_socket(name)),
                _ => Arc::new(crate::backend::tmux::Tmux::new()),
            },
            Backend::Native => anyhow::bail!("native backend is not implemented yet; set backend = \"tmux\" in {}", paths.settings_file().display()),
        };
        Ok(Self {
            paths,
            settings,
            store,
            state,
            backend,
            events: Arc::new(EventHub::new()),
            agents: agent::Tracker::new(),
            runtime: HashMap::new(),
            started: Instant::now(),
            shutdown: None,
        })
    }

    /// Test constructor with an explicit backend and home.
    pub fn with_backend(paths: Paths, settings: Settings, backend: Arc<dyn SessionBackend>) -> anyhow::Result<Self> {
        std::fs::create_dir_all(&paths.home)?;
        let store = Store::new(&paths);
        let state = store.load()?;
        Ok(Self {
            paths,
            settings,
            store,
            state,
            backend,
            events: Arc::new(EventHub::new()),
            agents: agent::Tracker::new(),
            runtime: HashMap::new(),
            started: Instant::now(),
            shutdown: None,
        })
    }

    /// Write in-memory state to disk under the advisory lock.
    pub fn persist(&self) -> Result<(), StateError> {
        let snapshot = self.state.clone();
        self.store.with_lock(move |disk| {
            *disk = snapshot;
            Ok(())
        })
    }

    pub fn reload_settings(&mut self) -> anyhow::Result<()> {
        self.settings = Settings::load(&self.paths.settings_file())?;
        Ok(())
    }

    pub fn project_name(&self, root: &Path) -> String {
        self.state
            .project(root)
            .map(|p| p.name.clone())
            .unwrap_or_else(|| root.file_name().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default())
    }

    pub fn runtime_mut(&mut self, id: &str) -> &mut RuntimeInfo {
        self.runtime.entry(id.to_string()).or_default()
    }

    pub fn runtime_of(&self, id: &str) -> RuntimeInfo {
        self.runtime.get(id).cloned().unwrap_or_default()
    }

    pub fn is_busy(&self, id: &str) -> bool {
        self.runtime.get(id).is_some_and(|r| r.busy)
    }

    pub fn row(&self, ws: &Workspace) -> WorkspaceRow {
        let project = self.project_name(&ws.project_root);
        let rt = self.runtime_of(&ws.id);
        let mut row = WorkspaceRow::from_workspace(ws, &project);
        row.alive = rt.alive;
        row.attached = rt.attached;
        row.hints = rt.hints.clone();
        if let Some(pr) = &rt.pr {
            row.hints.push(crate::hints::pr_hint(pr));
            row.pr_number = Some(pr.number);
            row.pr_state = pr.state.clone();
            row.ci = pr.checks.clone();
        }
        row.agent_state = if rt.alive { self.agents.state(&ws.id) } else { AgentState::Unknown };
        row.agent = self.launcher_name_for(ws);
        row.mem_rss_bytes = rt.mem_rss_bytes;
        row.cpu_percent = rt.cpu_percent;
        if ws.status == canopy_core::state::Status::SettingUp || rt.busy {
            row.progress = rt.progress.lock().map(|p| p.clone()).unwrap_or_default();
        }
        row
    }

    pub fn rows(&self, project_root: Option<&Path>) -> Vec<WorkspaceRow> {
        self.state
            .workspaces
            .iter()
            .filter(|w| project_root.is_none_or(|r| w.project_root == r))
            .map(|w| self.row(w))
            .collect()
    }

    /// Effective launcher name for a workspace (explicit, else project, else user default).
    pub fn launcher_name_for(&self, ws: &Workspace) -> String {
        if !ws.agent.is_empty() {
            return ws.agent.clone();
        }
        let project_default = canopy_core::config::ProjectConfig::load(&ws.project_root.join(canopy_core::config::FILE_NAME))
            .map(|c| c.agent.kind)
            .unwrap_or_default();
        if !project_default.is_empty() {
            return project_default;
        }
        self.settings.agent.kind.clone()
    }

    pub fn session_name(&self, ws: &Workspace) -> String {
        if ws.id.starts_with("main:") {
            return self.main_session_name(&ws.project_root);
        }
        ws.session_name(&self.project_name(&ws.project_root))
    }

    /// Pseudo-workspace for a project's main session (`main:<project>`), so window/tab APIs
    /// work on it like on any workspace.
    pub fn main_pseudo(&self, root: &Path) -> Option<Workspace> {
        let p = self.state.project(root)?;
        Some(Workspace {
            id: format!("main:{}", p.name),
            project_root: p.root.clone(),
            name: "main".into(),
            branch: String::new(),
            path: p.root.clone(),
            port: p.port_base,
            status: canopy_core::state::Status::Ready,
            agent_launch_count: 1,
            ..Workspace::default()
        })
    }

    pub fn main_session_name(&self, root: &Path) -> String {
        format!("{}/main", canopy_core::git::safe_name(&self.project_name(root)))
    }
}
