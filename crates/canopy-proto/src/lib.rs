//! canopy API protocol.
//!
//! Transport: newline-delimited JSON over the server's Unix socket. One request per
//! connection, except `events.subscribe`, which streams `Event`s until the client hangs up.
//!
//! ```text
//! -> {"id":"1","method":"workspace.list","params":{}}
//! <- {"id":"1","result":{"type":"workspace_list","workspaces":[...]}}
//! <- {"id":"1","error":{"code":"workspace_not_found","message":"..."}}
//! ```
//!
//! `PROTOCOL_VERSION` bumps on any incompatible change. Fields are only ever added.

use canopy_core::state::{SourceKind, Status, Workspace};
use serde::{Deserialize, Serialize};
use std::path::PathBuf;

pub const PROTOCOL_VERSION: u32 = 1;
pub const MAX_REQUEST_BYTES: usize = 1024 * 1024;

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct Request {
    pub id: String,
    #[serde(flatten)]
    pub method: Method,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(tag = "method", content = "params", rename_all = "snake_case")]
#[allow(clippy::large_enum_variant)]
pub enum Method {
    Ping,
    #[serde(rename = "server.stop")]
    ServerStop,
    #[serde(rename = "server.status")]
    ServerStatus,
    #[serde(rename = "server.reload_config")]
    ServerReloadConfig,
    #[serde(rename = "project.list")]
    ProjectList,
    #[serde(rename = "project.add")]
    ProjectAdd { root: PathBuf },
    /// Ensure the project's main session (anchored at the repo root) exists; return how to attach.
    #[serde(rename = "project.main")]
    ProjectMain { root: PathBuf },
    /// Kill the project's main session (the repo root checkout is untouched).
    #[serde(rename = "project.stop_main")]
    ProjectStopMain { root: PathBuf },
    /// Onboard a project from a local path or a git URL (cloned into the source root).
    /// Writes `canopy.json` if missing and registers it. `adopt_from` names a file (relative
    /// to the repo root) whose `scripts` table seeds the config; otherwise the user config's
    /// `[init] adopt_from` candidates are tried.
    #[serde(rename = "project.init")]
    ProjectInit {
        #[serde(default)]
        path: Option<PathBuf>,
        #[serde(default)]
        url: Option<String>,
        #[serde(default)]
        with_scripts: bool,
        #[serde(default)]
        adopt_from: Option<PathBuf>,
    },
    /// Forget a project (refused while it has workspaces). Files are untouched.
    #[serde(rename = "project.remove")]
    ProjectRemove { root: PathBuf },
    /// Open pull requests of the project (via `gh`).
    #[serde(rename = "project.pull_requests")]
    ProjectPullRequests { root: PathBuf },
    /// Open issues of the project (via `gh`).
    #[serde(rename = "project.issues")]
    ProjectIssues { root: PathBuf },
    /// Remote branches of the project (`origin/*`), newest first.
    #[serde(rename = "project.branches")]
    ProjectBranches { root: PathBuf },
    #[serde(rename = "workspace.list")]
    WorkspaceList {
        #[serde(default, skip_serializing_if = "Option::is_none")]
        project_root: Option<PathBuf>,
    },
    #[serde(rename = "workspace.get")]
    WorkspaceGet { workspace: WorkspaceRef },
    #[serde(rename = "workspace.create")]
    WorkspaceCreate(WorkspaceCreate),
    #[serde(rename = "workspace.remove")]
    WorkspaceRemove { workspace: WorkspaceRef, #[serde(default)] force: bool },
    #[serde(rename = "workspace.retry")]
    WorkspaceRetry { workspace: WorkspaceRef, #[serde(default)] force: bool },
    /// Setup/archive log of a workspace. Without `offset`: the last `lines` lines (default
    /// 200). With `offset`: everything written since that byte offset, so clients tail.
    #[serde(rename = "workspace.log")]
    WorkspaceLog {
        workspace: WorkspaceRef,
        #[serde(default)]
        offset: Option<u64>,
        #[serde(default)]
        lines: Option<usize>,
    },
    #[serde(rename = "workspace.rename")]
    WorkspaceRename { workspace: WorkspaceRef, #[serde(default)] pin: Option<bool> },
    #[serde(rename = "workspace.reconcile")]
    WorkspaceReconcile {
        #[serde(default, skip_serializing_if = "Option::is_none")]
        project_root: Option<PathBuf>,
    },
    #[serde(rename = "workspace.resurrect")]
    WorkspaceResurrect { workspace: WorkspaceRef },
    /// How to attach to the workspace's session from the calling machine.
    #[serde(rename = "workspace.attach_target")]
    WorkspaceAttachTarget { workspace: WorkspaceRef },
    #[serde(rename = "workspace.run")]
    WorkspaceRun { workspace: WorkspaceRef, #[serde(default)] script: Option<String> },
    /// Kill the session; the worktree and state row survive (`stopped`).
    #[serde(rename = "workspace.stop")]
    WorkspaceStop { workspace: WorkspaceRef },
    #[serde(rename = "workspace.set_owner")]
    WorkspaceSetOwner { workspace: WorkspaceRef, owner: String },
    /// Windows (tabs) of the workspace's session.
    #[serde(rename = "workspace.windows")]
    WorkspaceWindows { workspace: WorkspaceRef },
    #[serde(rename = "workspace.select_window")]
    WorkspaceSelectWindow { workspace: WorkspaceRef, index: u32 },
    /// Open a new shell window (tab) in the workspace's session.
    #[serde(rename = "workspace.new_window")]
    WorkspaceNewWindow { workspace: WorkspaceRef, #[serde(default)] name: Option<String> },
    /// Show or hide the sidebar pane of a session (by workspace, or by raw session name).
    #[serde(rename = "session.sidebar_toggle")]
    SessionSidebarToggle { session: String },
    /// Flash a short message in the session's status line (e.g. clipboard confirmations).
    #[serde(rename = "session.notify")]
    SessionNotify { session: String, text: String },
    /// Add a sidebar to the session's active window if it has none (new-window hook).
    #[serde(rename = "session.sidebar_ensure")]
    SessionSidebarEnsure { session: String },
    #[serde(rename = "pane.report_agent")]
    PaneReportAgent(AgentReport),
    /// Put text on this machine's clipboard (mirrored from a remote client's clipboard).
    #[serde(rename = "clipboard.set_text")]
    ClipboardSetText { text: String },
    /// Put a file's bytes on this machine's clipboard with the given MIME type (e.g. a PNG
    /// screenshot uploaded by a remote client). The file is removed after use.
    #[serde(rename = "clipboard.set_file")]
    ClipboardSetFile { path: PathBuf, mime: String },
    /// Read this machine's clipboard as text (remote clients mirror it to their own).
    #[serde(rename = "clipboard.get")]
    ClipboardGet,
    #[serde(rename = "agent.list")]
    AgentList,
    #[serde(rename = "events.subscribe")]
    EventsSubscribe {
        #[serde(default)]
        since: Option<u64>,
    },
}

/// Address a workspace by stable id or by `(project_root, name)`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(untagged)]
pub enum WorkspaceRef {
    Id { id: String },
    Named { project_root: PathBuf, name: String },
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct WorkspaceCreate {
    pub project_root: PathBuf,
    pub name: Option<String>,
    pub branch: Option<String>,
    pub pr: Option<u64>,
    pub issue: Option<u64>,
    pub prompt: Option<String>,
    pub agent: Option<String>,
    /// Build the session now (default) or leave it `stopped` until first attach.
    pub start_session: Option<bool>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct AgentReport {
    pub workspace_id: String,
    /// Reporter identity, e.g. `canopy:claude`. Monotonic `seq` per source.
    pub source: String,
    pub seq: u64,
    pub agent: String,
    pub state: AgentState,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub session_id: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum AgentState {
    Idle,
    Working,
    Blocked,
    Done,
    Unknown,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(untagged)]
#[allow(clippy::large_enum_variant)]
pub enum Response {
    Ok { id: String, result: ResultBody },
    Err { id: String, error: ApiError },
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(tag = "type", rename_all = "snake_case")]
#[allow(clippy::large_enum_variant)]
pub enum ResultBody {
    Pong { version: String, protocol: u32, capabilities: Vec<String> },
    Ack,
    ServerStatus { pid: u32, version: String, uptime_secs: u64, backend: String, socket: PathBuf },
    ProjectList { projects: Vec<ProjectRow> },
    WorkspaceList { workspaces: Vec<WorkspaceRow> },
    Workspace { workspace: WorkspaceRow },
    AttachTarget { target: AttachTarget },
    WindowList { windows: Vec<WindowRow> },
    /// A slice of a workspace log. `offset` is the byte length after this read: pass it
    /// back to get only what was appended since. `running` while setup is in flight.
    Log { path: PathBuf, text: String, offset: u64, running: bool, status: Status },
    Clipboard {
        #[serde(default, skip_serializing_if = "Option::is_none")]
        text: Option<String>,
    },
    /// Pickable items (PRs, issues, branches).
    PickList { items: Vec<PickItem> },
    AgentList { agents: Vec<AgentRow> },
    Event { event: Event },
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct ApiError {
    pub code: ErrorCode,
    pub message: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum ErrorCode {
    UnknownMethod,
    InvalidRequest,
    InvalidParams,
    InternalError,
    ServerUnavailable,
    Timeout,
    EventsLost,
    ProjectNotFound,
    ProjectHasWorkspaces,
    WorkspaceNotFound,
    WorkspaceExists,
    WorkspaceBusy,
    /// Removal refused: uncommitted changes, unpushed commits or an open PR. Pass `force`.
    RemovalBlocked,
    SetupFailed,
    BackendError,
    GitError,
    NoPortsAvailable,
    AgentNotFound,
    AgentNotAllowed,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct ProjectRow {
    pub root: PathBuf,
    pub name: String,
    pub port_base: u16,
    pub workspace_count: usize,
    /// The main session (editor + agent + shell at the repo root, port = `port_base`).
    pub main_alive: bool,
    #[serde(default)]
    pub main_attached: bool,
    /// Branch currently checked out at the repo root.
    #[serde(default)]
    pub main_branch: String,
    #[serde(default)]
    pub main_session: String,
}

/// A workspace plus everything the server derives about it. The row the TUI renders.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct WorkspaceRow {
    pub id: String,
    pub project: String,
    pub project_root: PathBuf,
    pub name: String,
    pub branch: String,
    pub path: PathBuf,
    pub port: u16,
    pub status: Status,
    pub session: String,
    pub alive: bool,
    pub attached: bool,
    pub agent: String,
    pub agent_state: AgentState,
    pub source_kind: SourceKind,
    pub owner: String,
    pub hints: Vec<Hint>,
    pub last_error_hint: String,
    /// While setting up: the latest line of setup output (or the current phase).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub progress: String,
    pub mem_rss_bytes: u64,
    pub cpu_percent: u16,
    /// Host this row came from; empty for local.
    pub host: String,
    /// Pull request on this branch, when `gh` knows one.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pr_number: Option<u64>,
    /// `OPEN` | `MERGED` | `CLOSED` | `` .
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub pr_state: String,
    /// `SUCCESS` | `FAILURE` | `PENDING` | `` (CI rollup).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub ci: String,
}

impl WorkspaceRow {
    pub fn from_workspace(w: &Workspace, project_name: &str) -> Self {
        Self {
            id: w.id.clone(),
            project: project_name.to_string(),
            project_root: w.project_root.clone(),
            name: w.name.clone(),
            branch: w.branch.clone(),
            path: w.path.clone(),
            port: w.port,
            status: w.status,
            session: w.session_name(project_name),
            alive: false,
            attached: false,
            agent: w.agent.clone(),
            agent_state: AgentState::Unknown,
            source_kind: w.source_kind,
            owner: w.owner.clone(),
            hints: Vec::new(),
            last_error_hint: w.last_error_hint.clone(),
            mem_rss_bytes: 0,
            cpu_percent: 0,
            host: String::new(),
            pr_number: None,
            pr_state: String::new(),
            ci: String::new(),
            progress: String::new(),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct Hint {
    pub kind: HintKind,
    pub message: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub action: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum HintKind {
    RenameSuggested,
    Conflict,
    Rebasing,
    Merging,
    CherryPicking,
    Detached,
    AheadBehind,
    Unpushed,
    Diverged,
    PrStatus,
    Shipped,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum AttachTarget {
    /// Exec `tmux attach -t <session>` (local) — tmux backend.
    Tmux { session: String, detach_others: bool },
    /// Exec `ssh -t <target> tmux attach …` or mosh — tmux backend on a remote host.
    RemoteTmux { ssh_target: String, session: String, mosh: bool },
    /// Attach through the server's pane-surface stream — native backend.
    Native { workspace_id: String },
}

/// One row of a picker: `key` is what `workspace.create` takes (PR/issue number or branch name).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct PickItem {
    pub key: String,
    pub label: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub detail: String,
    /// Already checked out in a workspace (branch/PR in use).
    #[serde(default)]
    pub in_use: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct WindowRow {
    pub index: u32,
    pub name: String,
    pub active: bool,
    pub panes: u32,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct AgentRow {
    pub workspace_id: String,
    pub agent: String,
    pub state: AgentState,
    pub since_secs: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct Event {
    pub seq: u64,
    #[serde(flatten)]
    pub kind: EventKind,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum EventKind {
    WorkspaceCreated { workspace_id: String },
    WorkspaceUpdated { workspace_id: String },
    WorkspaceRemoved { workspace_id: String },
    AgentStatusChanged { workspace_id: String, state: AgentState },
    RunStarted { workspace_id: String, script: String },
    RunExited { workspace_id: String, script: String, code: Option<i32> },
    HostOnline { host: String },
    HostOffline { host: String, error: String },
    ServerShutdown,
}

/// JSON schema for the whole protocol (`canopy api schema`).
pub fn schema() -> serde_json::Value {
    #[derive(schemars::JsonSchema)]
    #[allow(dead_code)]
    struct Protocol { request: Request, response: Response, event: Event }
    serde_json::to_value(schemars::schema_for!(Protocol)).expect("schema serializes")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_wire_shape() {
        let r = Request { id: "1".into(), method: Method::WorkspaceList { project_root: None } };
        let json = serde_json::to_string(&r).unwrap();
        assert_eq!(json, r#"{"id":"1","method":"workspace.list","params":{}}"#);
        let back: Request = serde_json::from_str(&json).unwrap();
        assert_eq!(back, r);
    }

    #[test]
    fn ping_has_no_params_key_required() {
        let r: Request = serde_json::from_str(r#"{"id":"x","method":"ping"}"#).unwrap();
        assert_eq!(r.method, Method::Ping);
    }

    #[test]
    fn response_shapes() {
        let ok = Response::Ok { id: "1".into(), result: ResultBody::Ack };
        assert_eq!(serde_json::to_string(&ok).unwrap(), r#"{"id":"1","result":{"type":"ack"}}"#);
        let err = Response::Err { id: "1".into(), error: ApiError { code: ErrorCode::WorkspaceNotFound, message: "nope".into() } };
        assert_eq!(
            serde_json::to_string(&err).unwrap(),
            r#"{"id":"1","error":{"code":"workspace_not_found","message":"nope"}}"#
        );
    }

    #[test]
    fn workspace_ref_variants() {
        let by_id: WorkspaceRef = serde_json::from_str(r#"{"id":"w7K"}"#).unwrap();
        assert_eq!(by_id, WorkspaceRef::Id { id: "w7K".into() });
        let named: WorkspaceRef = serde_json::from_str(r#"{"project_root":"/p","name":"a"}"#).unwrap();
        assert!(matches!(named, WorkspaceRef::Named { .. }));
    }

    #[test]
    fn schema_builds() {
        let s = schema();
        assert!(s.get("$schema").is_some() || s.get("title").is_some());
    }
}
