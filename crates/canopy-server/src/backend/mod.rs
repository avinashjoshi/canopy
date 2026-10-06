//! The seam between canopy's workspace model and whatever hosts the terminals.
//!
//! Two implementations are planned: `tmux` (Phase 1) and `native` (Phase 3, server-owned
//! PTYs + terminal emulation). Everything above this trait is backend-agnostic.

use std::collections::BTreeMap;
use std::path::Path;

#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct PaneId(pub String);

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PaneSpec<'a> {
    /// `ide`, `agent:<kind>`, `terminal:shell`, `run:<name>`.
    pub role: &'a str,
    pub command: &'a str,
    pub cwd: &'a Path,
    pub env: &'a BTreeMap<String, String>,
    /// `right` | `below` | `left` | `above`; `None` for the first pane.
    pub split: Option<&'a str>,
    /// Role of the pane being split. `None` = the session's first pane.
    pub split_of: Option<&'a str>,
    pub size_percent: Option<u8>,
    /// Absolute size in columns/rows; wins over `size_percent`.
    pub size_cells: Option<u16>,
    /// Wrap the command so the pane drops to a shell when it exits (default for user panes).
    pub keep_alive: bool,
    /// Split across the whole window (full height for left/right) instead of just the target pane.
    pub full_span: bool,
    /// Window to split in when `split_of` is `None`; `None` = the session's active window.
    pub window: Option<u32>,
}

/// One session as seen in a whole-server snapshot.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct SessionInfo {
    pub name: String,
    pub attached: bool,
    pub panes: Vec<PaneInfo>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WindowInfo {
    /// Backend-native stable id (`@3` in tmux).
    pub id: String,
    pub index: u32,
    pub name: String,
    pub active: bool,
    pub panes: u32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PaneInfo {
    pub id: PaneId,
    pub window: u32,
    pub active: bool,
    pub width: u16,
    pub role: String,
    pub pid: Option<u32>,
    pub current_command: String,
}

#[derive(Debug, thiserror::Error)]
pub enum BackendError {
    #[error("session {0:?} already exists")]
    SessionExists(String),
    #[error("session {0:?} not found")]
    SessionNotFound(String),
    #[error("pane {0:?} not found")]
    PaneNotFound(String),
    #[error("backend command failed: {0}")]
    Command(String),
    #[error(transparent)]
    Io(#[from] std::io::Error),
}

/// Hosts terminal sessions for workspaces. Implementations must be `Send + Sync`;
/// the server calls them from its async loop via `spawn_blocking` where needed.
pub trait SessionBackend: Send + Sync {
    fn name(&self) -> &'static str;

    fn session_exists(&self, session: &str) -> Result<bool, BackendError>;
    /// Every session with its panes and attachment, in one backend call (the poller's view).
    fn snapshot(&self) -> Result<Vec<SessionInfo>, BackendError>;
    /// Create the session with its first window named `window` holding `first`.
    fn create_session(&self, session: &str, window: &str, first: PaneSpec<'_>) -> Result<PaneId, BackendError>;
    fn add_pane(&self, session: &str, spec: PaneSpec<'_>) -> Result<PaneId, BackendError>;
    fn select_pane(&self, session: &str, pane: &PaneId) -> Result<(), BackendError>;
    /// A new window/tab in the session holding one pane (used for `canopy run` and new tabs).
    fn open_window(&self, session: &str, name: &str, spec: PaneSpec<'_>) -> Result<PaneId, BackendError>;
    fn windows(&self, session: &str) -> Result<Vec<WindowInfo>, BackendError>;
    /// Idempotent, runtime-only server configuration: canopy keybinds (only where nothing
    /// else is bound) and tab styling (only where the user kept tmux defaults). Never edits
    /// files. `bin` is the canopy executable for key commands.
    fn ensure_server_config(&self, bin: &str) -> Result<(), BackendError>;
    /// Per-session chrome: status bar at the top as tabs, canopy statusline segment.
    fn decorate_session(&self, session: &str, statusline_cmd: &str) -> Result<(), BackendError>;
    fn select_window(&self, session: &str, index: u32) -> Result<(), BackendError>;
    fn active_window(&self, session: &str) -> Result<u32, BackendError>;
    fn kill_pane(&self, pane: &PaneId) -> Result<(), BackendError>;
    /// Mark the next focus of `pane` as intentional (the sidebar bounces unmarked focus).
    fn mark_focus(&self, pane: &PaneId) -> Result<(), BackendError>;
    /// Focus the previously active pane of `window` (what the user was in before the sidebar).
    fn select_last_pane(&self, session: &str, window: u32) -> Result<(), BackendError>;
    /// Release the sidebar's modal focus lock on `window` before moving focus out of it.
    fn unlock_window(&self, session: &str, window: u32) -> Result<(), BackendError>;
    /// Flash `text` in the session's status line for a moment.
    fn display_message(&self, session: &str, text: &str) -> Result<(), BackendError>;
    fn resize_pane(&self, pane: &PaneId, width: u16) -> Result<(), BackendError>;
    fn kill_session(&self, session: &str) -> Result<(), BackendError>;
    fn rename_session(&self, from: &str, to: &str) -> Result<(), BackendError>;
    fn attached_clients(&self, session: &str) -> Result<usize, BackendError>;
    fn panes(&self, session: &str) -> Result<Vec<PaneInfo>, BackendError>;
    /// Bottom `lines` of the pane's screen as plain text (for screen-based agent detection).
    fn read_screen(&self, pane: &PaneId, lines: usize) -> Result<String, BackendError>;
    /// Type `text` literally; `enter` appends a newline keypress.
    fn send_text(&self, pane: &PaneId, text: &str, enter: bool) -> Result<(), BackendError>;
}

pub mod tmux;
