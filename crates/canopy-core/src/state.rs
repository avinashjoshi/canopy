//! `~/.canopy/state.json`: the registry of projects and workspaces.
//!
//! Only canonical state is stored. Anything derivable (hints, agent state, PR status,
//! session name) is recomputed, because persisted derived data goes stale after manual
//! git operations. Schema v3; v0's v2 files are imported one-way on first load.

use crate::paths::Paths;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

pub const SCHEMA_VERSION: u32 = 3;

#[derive(Debug, thiserror::Error)]
pub enum StateError {
    #[error("cannot read {0}: {1}")]
    Io(PathBuf, #[source] std::io::Error),
    #[error("invalid {0}: {1}")]
    Parse(PathBuf, #[source] serde_json::Error),
    #[error("unsupported state schema_version {0} (this canopy knows {SCHEMA_VERSION})")]
    UnsupportedSchema(u32),
    #[error("workspace {0:?} already exists in project {1}")]
    WorkspaceExists(String, String),
    #[error("workspace {0:?} not found")]
    WorkspaceNotFound(String),
    #[error("project {0} not found")]
    ProjectNotFound(PathBuf),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum Status {
    /// Registered; worktree/scripts/session being built.
    SettingUp,
    /// Session alive.
    Ready,
    /// Worktree present, session gone. `switch` resurrects.
    Stopped,
    /// Setup failed. `retry` re-runs setup in place.
    Broken,
    /// Worktree directory gone. Only `rm` applies.
    Orphaned,
}

impl Status {
    pub fn as_str(self) -> &'static str {
        match self {
            Status::SettingUp => "setting_up",
            Status::Ready => "ready",
            Status::Stopped => "stopped",
            Status::Broken => "broken",
            Status::Orphaned => "orphaned",
        }
    }
}

/// Where a workspace's branch came from. Drives the briefing variant and the
/// "rename me" nudge (only `Fresh` + auto-generated names get it).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum SourceKind {
    #[default]
    Fresh,
    Pr,
    Issue,
    Branch,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct Project {
    /// Canonical absolute path (symlinks resolved). The project key.
    pub root: PathBuf,
    /// Display name, defaults to the root's basename.
    pub name: String,
    pub port_base: u16,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct Workspace {
    /// Stable public id (`w7K`). Survives renames; what API clients and hooks use.
    pub id: String,
    pub project_root: PathBuf,
    pub name: String,
    pub branch: String,
    pub path: PathBuf,
    pub port: u16,
    pub status: Status,
    /// RFC 3339.
    pub created_at: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub last_error: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub last_error_hint: String,
    /// Incremented on every create + resurrect. 0 => full briefing, >0 => delta briefing.
    pub agent_launch_count: u32,
    /// Launcher name (`claude`, `codex`, ...). Empty => project/user default.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub agent: String,
    /// Agent session id reported through hooks, for real `--resume <id>` on resurrect.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub agent_session_id: String,
    pub source_kind: SourceKind,
    pub name_auto_generated: bool,
    /// PR / issue body captured once at creation (wrapped as data, never re-fetched).
    #[serde(skip_serializing_if = "String::is_empty")]
    pub source_context: String,
    /// `--pr N` / `--issue N` number, if any.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub source_number: Option<u64>,
    /// Freeze the display name instead of following the live branch.
    pub pinned: bool,
    /// `""` => derive from source_kind; `"(me)"` => explicitly mine; else a login.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub owner: String,
}

impl Default for Workspace {
    fn default() -> Self {
        Self {
            id: String::new(),
            project_root: PathBuf::new(),
            name: String::new(),
            branch: String::new(),
            path: PathBuf::new(),
            port: 0,
            status: Status::SettingUp,
            created_at: String::new(),
            last_error: String::new(),
            last_error_hint: String::new(),
            agent_launch_count: 0,
            agent: String::new(),
            agent_session_id: String::new(),
            source_kind: SourceKind::Fresh,
            name_auto_generated: false,
            source_context: String::new(),
            source_number: None,
            pinned: false,
            owner: String::new(),
        }
    }
}

impl Workspace {
    /// Session name: `<project>/<branch>` through `safe_name`, or `<project>/<name>` when pinned.
    pub fn session_name(&self, project_name: &str) -> String {
        let suffix = if self.pinned || self.branch.is_empty() { &self.name } else { &self.branch };
        format!("{}/{}", crate::git::safe_name(project_name), crate::git::safe_name(suffix))
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct State {
    pub schema_version: u32,
    /// Keyed by canonical root path.
    pub projects: BTreeMap<PathBuf, Project>,
    pub workspaces: Vec<Workspace>,
}

impl Default for State {
    fn default() -> Self {
        Self { schema_version: SCHEMA_VERSION, projects: BTreeMap::new(), workspaces: Vec::new() }
    }
}

impl State {
    pub fn project(&self, root: &Path) -> Option<&Project> {
        self.projects.get(root)
    }

    pub fn find(&self, project_root: &Path, name: &str) -> Option<&Workspace> {
        self.workspaces.iter().find(|w| w.project_root == project_root && w.name == name)
    }

    pub fn find_mut(&mut self, project_root: &Path, name: &str) -> Option<&mut Workspace> {
        self.workspaces.iter_mut().find(|w| w.project_root == project_root && w.name == name)
    }

    pub fn by_id(&self, id: &str) -> Option<&Workspace> {
        self.workspaces.iter().find(|w| w.id == id)
    }

    pub fn used_ports(&self) -> std::collections::BTreeSet<u16> {
        self.workspaces.iter().map(|w| w.port).chain(self.projects.values().map(|p| p.port_base)).collect()
    }

    pub fn used_project_bases(&self) -> std::collections::BTreeSet<u16> {
        self.projects.values().map(|p| p.port_base).collect()
    }

    pub fn add(&mut self, ws: Workspace) -> Result<(), StateError> {
        if self.find(&ws.project_root, &ws.name).is_some() {
            let proj = ws.project_root.display().to_string();
            return Err(StateError::WorkspaceExists(ws.name, proj));
        }
        self.workspaces.push(ws);
        Ok(())
    }

    pub fn remove(&mut self, project_root: &Path, name: &str) -> Result<Workspace, StateError> {
        let idx = self
            .workspaces
            .iter()
            .position(|w| w.project_root == project_root && w.name == name)
            .ok_or_else(|| StateError::WorkspaceNotFound(name.to_string()))?;
        Ok(self.workspaces.remove(idx))
    }

    /// Fresh public id not used by any workspace: `w` + 3 chars from a confusable-free alphabet.
    pub fn new_id(&self) -> String {
        const ALPHABET: &[u8] = b"123456789ABCDEFGHJKMNPQRSTVWXYZ";
        use rand::Rng;
        let mut rng = rand::rng();
        loop {
            let s: String = (0..3).map(|_| ALPHABET[rng.random_range(0..ALPHABET.len())] as char).collect();
            let id = format!("w{s}");
            if self.by_id(&id).is_none() {
                return id;
            }
        }
    }

    /// Parse any supported schema version, upgrading older ones in memory.
    pub fn from_json(text: &str, path: &Path) -> Result<Self, StateError> {
        #[derive(Deserialize)]
        struct Probe { #[serde(default)] schema_version: u32 }
        let probe: Probe = serde_json::from_str(text).map_err(|e| StateError::Parse(path.to_path_buf(), e))?;
        match probe.schema_version {
            SCHEMA_VERSION => serde_json::from_str(text).map_err(|e| StateError::Parse(path.to_path_buf(), e)),
            2 => Ok(v2::import(text, path)?),
            v => Err(StateError::UnsupportedSchema(v)),
        }
    }
}

/// One-way import of canopy v0 `state.json` (schema_version 2).
mod v2 {
    use super::*;

    #[derive(Deserialize)]
    struct StateV2 {
        #[serde(default)]
        projects: BTreeMap<String, ProjectV2>,
        #[serde(default)]
        workspaces: Vec<WorkspaceV2>,
    }
    #[derive(Deserialize)]
    struct ProjectV2 { root: PathBuf, port_base: u16 }
    #[derive(Deserialize)]
    struct WorkspaceV2 {
        project_root: PathBuf,
        name: String,
        #[serde(default)] branch: String,
        path: PathBuf,
        port: u16,
        status: String,
        #[serde(default)] created_at: String,
        #[serde(default)] last_error: String,
        #[serde(default)] last_error_hint: String,
        #[serde(default)] agent_launch_count: u32,
        #[serde(default)] source_kind: String,
        #[serde(default)] name_auto_generated: bool,
        #[serde(default)] source_context: String,
        #[serde(default)] pin_display_name: bool,
        #[serde(default)] owner: String,
    }

    pub fn import(text: &str, path: &Path) -> Result<State, StateError> {
        let old: StateV2 = serde_json::from_str(text).map_err(|e| StateError::Parse(path.to_path_buf(), e))?;
        let mut st = State::default();
        for (_, p) in old.projects {
            let name = p.root.file_name().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default();
            st.projects.insert(p.root.clone(), Project { root: p.root, name, port_base: p.port_base });
        }
        for w in old.workspaces {
            let status = match w.status.as_str() {
                "ready" => Status::Ready,
                "stopped" => Status::Stopped,
                "broken" => Status::Broken,
                "orphaned" => Status::Orphaned,
                _ => Status::SettingUp,
            };
            let source_kind = match w.source_kind.as_str() {
                "pr" => SourceKind::Pr,
                "issue" => SourceKind::Issue,
                "branch" => SourceKind::Branch,
                _ => SourceKind::Fresh,
            };
            let id = st.new_id();
            st.workspaces.push(Workspace {
                id,
                project_root: w.project_root,
                name: w.name,
                branch: w.branch,
                path: w.path,
                port: w.port,
                status,
                created_at: w.created_at,
                last_error: w.last_error,
                last_error_hint: w.last_error_hint,
                agent_launch_count: w.agent_launch_count,
                source_kind,
                name_auto_generated: w.name_auto_generated,
                source_context: w.source_context,
                pinned: w.pin_display_name,
                owner: w.owner,
                ..Workspace::default()
            });
        }
        Ok(st)
    }
}

/// File-backed store with an advisory `flock` for read-modify-write and atomic
/// tmp+rename saves. The server is the only writer in v1, but the lock keeps a
/// concurrently running v0 binary (or a second server by mistake) from corrupting state.
pub struct Store {
    path: PathBuf,
    lock_path: PathBuf,
}

impl Store {
    pub fn new(paths: &Paths) -> Self {
        Self { path: paths.state_file(), lock_path: paths.state_lock() }
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    pub fn load(&self) -> Result<State, StateError> {
        match fs::read_to_string(&self.path) {
            Ok(text) => State::from_json(&text, &self.path),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(State::default()),
            Err(e) => Err(StateError::Io(self.path.clone(), e)),
        }
    }

    pub fn save(&self, state: &State) -> Result<(), StateError> {
        let parent = self.path.parent().unwrap_or(Path::new("."));
        fs::create_dir_all(parent).map_err(|e| StateError::Io(parent.to_path_buf(), e))?;
        let tmp = self.path.with_extension("json.tmp");
        {
            let mut f = File::create(&tmp).map_err(|e| StateError::Io(tmp.clone(), e))?;
            let text = serde_json::to_string_pretty(state).expect("state serializes");
            f.write_all(text.as_bytes()).map_err(|e| StateError::Io(tmp.clone(), e))?;
            f.write_all(b"\n").map_err(|e| StateError::Io(tmp.clone(), e))?;
            f.sync_all().map_err(|e| StateError::Io(tmp.clone(), e))?;
        }
        fs::rename(&tmp, &self.path).map_err(|e| StateError::Io(self.path.clone(), e))
    }

    /// Run `f` with the lock held: load, mutate, save. Returns `f`'s value.
    pub fn with_lock<T>(&self, f: impl FnOnce(&mut State) -> Result<T, StateError>) -> Result<T, StateError> {
        let parent = self.lock_path.parent().unwrap_or(Path::new("."));
        fs::create_dir_all(parent).map_err(|e| StateError::Io(parent.to_path_buf(), e))?;
        let lock = OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .open(&self.lock_path)
            .map_err(|e| StateError::Io(self.lock_path.clone(), e))?;
        nix::fcntl::Flock::lock(lock, nix::fcntl::FlockArg::LockExclusive)
            .map_err(|(_, errno)| StateError::Io(self.lock_path.clone(), std::io::Error::from(errno)))?;
        // Lock released when `_guard` drops at the end of this scope.
        let mut state = self.load()?;
        let out = f(&mut state)?;
        self.save(&state)?;
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ws(name: &str, port: u16) -> Workspace {
        Workspace {
            id: format!("w{name}"),
            project_root: PathBuf::from("/p"),
            name: name.into(),
            branch: name.into(),
            path: PathBuf::from(format!("/w/{name}")),
            port,
            status: Status::Ready,
            ..Workspace::default()
        }
    }

    #[test]
    fn add_find_remove() {
        let mut st = State::default();
        st.add(ws("a", 40010)).unwrap();
        assert!(matches!(st.add(ws("a", 40020)), Err(StateError::WorkspaceExists(..))));
        assert_eq!(st.find(Path::new("/p"), "a").unwrap().port, 40010);
        st.remove(Path::new("/p"), "a").unwrap();
        assert!(matches!(st.remove(Path::new("/p"), "a"), Err(StateError::WorkspaceNotFound(_))));
    }

    #[test]
    fn session_name_follows_branch_unless_pinned() {
        let mut w = ws("bold-falcon", 1);
        w.branch = "feature/oauth".into();
        assert_eq!(w.session_name("canopy"), "canopy/feature-oauth");
        w.pinned = true;
        assert_eq!(w.session_name("canopy"), "canopy/bold-falcon");
    }

    #[test]
    fn roundtrip_and_lock() {
        let dir = tempfile::tempdir().unwrap();
        let paths = Paths::with_home(dir.path());
        let store = Store::new(&paths);
        assert_eq!(store.load().unwrap(), State::default());
        store
            .with_lock(|st| {
                st.projects.insert(PathBuf::from("/p"), Project { root: "/p".into(), name: "p".into(), port_base: 40000 });
                st.add(ws("a", 40010))
            })
            .unwrap();
        let st = store.load().unwrap();
        assert_eq!(st.schema_version, SCHEMA_VERSION);
        assert_eq!(st.workspaces.len(), 1);
        assert_eq!(st.used_ports(), [40000, 40010].into_iter().collect());
        assert!(!dir.path().join("state.json.tmp").exists());
    }

    #[test]
    fn imports_v2() {
        let text = r#"{
          "schema_version": 2,
          "projects": {"/home/x/canopy": {"root": "/home/x/canopy", "port_base": 40000}},
          "workspaces": [{
            "project_root": "/home/x/canopy", "name": "bold-falcon", "branch": "fix-tz",
            "path": "/home/x/.canopy/workspaces/canopy/bold-falcon", "port": 40010,
            "status": "stopped", "created_at": "2026-05-01T00:00:00Z", "agent_launch_count": 3,
            "source_kind": "pr", "name_auto_generated": true, "pin_display_name": true, "owner": "octocat"
          }]
        }"#;
        let st = State::from_json(text, Path::new("state.json")).unwrap();
        assert_eq!(st.schema_version, SCHEMA_VERSION);
        let p = st.project(Path::new("/home/x/canopy")).unwrap();
        assert_eq!(p.name, "canopy");
        let w = &st.workspaces[0];
        assert_eq!(w.status, Status::Stopped);
        assert_eq!(w.source_kind, SourceKind::Pr);
        assert!(w.pinned);
        assert_eq!(w.owner, "octocat");
        assert!(w.id.starts_with('w') && w.id.len() == 4);
    }

    #[test]
    fn unsupported_schema() {
        assert!(matches!(
            State::from_json(r#"{"schema_version": 99}"#, Path::new("s")),
            Err(StateError::UnsupportedSchema(99))
        ));
    }
}
