//! `canopy.json`: the per-project file, committed to the repo.
//!
//! Compatible with v0 (`scripts.setup|run|archive|agent`, `agent.type|briefing|briefing_file`).
//! New in v1: named run scripts, script timeouts, an `agents` allowlist, a `layout` name and
//! `env_prefixes`. `canopy init --from <file>` adopts the `scripts` table of any JSON or TOML
//! file that has the same shape.

use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

pub const FILE_NAME: &str = "canopy.json";

#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    #[error("no {FILE_NAME} found in {0} or any parent directory")]
    NotFound(PathBuf),
    #[error("cannot read {path}: {source}")]
    Io { path: PathBuf, #[source] source: std::io::Error },
    #[error("invalid {path}: {source}")]
    Invalid { path: PathBuf, #[source] source: serde_json::Error },
    #[error("invalid {FILE_NAME}: {0}")]
    Semantic(String),
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default, deny_unknown_fields)]
pub struct ProjectConfig {
    pub scripts: Scripts,
    pub agent: AgentConfig,
    /// Agents allowed for `canopy new --agent` / `canopy ask`. Empty = any registered launcher.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub agents: Vec<String>,
    /// Named pane layout from `~/.canopy/config.toml` (`[layouts.<name>]`). Empty = default.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub layout: String,
    /// Extra prefixes under which the core variables (`WORKSPACE_PATH`, `ROOT_PATH`, `PORT`,
    /// `WORKSPACE_NAME`) are also exported, for scripts written against another tool's names.
    /// Merged with `[env] prefixes` from the user config.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub env_prefixes: Vec<String>,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default, deny_unknown_fields)]
pub struct Scripts {
    /// Runs once at workspace creation, after checkout. Non-zero exit => `broken`.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub setup: String,
    /// Long-running dev server(s). Launched on demand, never auto-started.
    #[serde(skip_serializing_if = "RunScripts::is_empty")]
    pub run: RunScripts,
    /// Runs at removal, before the worktree is deleted. Failure is logged, removal proceeds.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub archive: String,
    /// Power-user override: your own agent launcher. Receives `CANOPY_AGENT_BRIEFING`.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub agent: String,
    pub timeouts: Timeouts,
}

/// `scripts.run` is either one command string (v0 shape) or a table of named scripts.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(untagged)]
pub enum RunScripts {
    Single(String),
    Named(BTreeMap<String, RunScript>),
}

impl Default for RunScripts {
    fn default() -> Self {
        RunScripts::Single(String::new())
    }
}

impl RunScripts {
    pub fn is_empty(&self) -> bool {
        match self {
            RunScripts::Single(s) => s.is_empty(),
            RunScripts::Named(m) => m.is_empty(),
        }
    }

    /// Normalize to a named map. A single string becomes `{"default": ...}` marked default.
    pub fn named(&self) -> BTreeMap<String, RunScript> {
        match self {
            RunScripts::Single(s) if s.is_empty() => BTreeMap::new(),
            RunScripts::Single(s) => BTreeMap::from([(
                "default".to_string(),
                RunScript { command: s.clone(), default: true, ..RunScript::default() },
            )]),
            RunScripts::Named(m) => m.clone(),
        }
    }

    /// The script `canopy run` launches with no argument.
    pub fn default_script(&self) -> Option<(String, RunScript)> {
        let named = self.named();
        if named.len() == 1 {
            return named.into_iter().next();
        }
        named.into_iter().find(|(_, s)| s.default)
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default, deny_unknown_fields)]
pub struct RunScript {
    pub command: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,
    /// Working directory relative to the workspace.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub cwd: String,
    pub default: bool,
    pub hide: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default, deny_unknown_fields)]
pub struct Timeouts {
    /// Seconds before `scripts.setup` is killed (process group). 0 = no timeout.
    pub setup: u64,
    /// Seconds before `scripts.archive` is killed. 0 = no timeout.
    pub archive: u64,
}

impl Default for Timeouts {
    fn default() -> Self {
        Self { setup: 600, archive: 120 }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default, deny_unknown_fields)]
pub struct AgentConfig {
    /// Launcher name: `claude` (default), `codex`, `opencode`, `aider`, `gemini`, ...
    #[serde(rename = "type", skip_serializing_if = "String::is_empty")]
    pub kind: String,
    /// Project-specific briefing text appended to canopy's conventions.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub briefing: String,
    /// Path (relative to project root) whose contents replace `briefing` when set.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub briefing_file: String,
}

/// A foreign config (JSON or TOML): a `scripts` table whose run scripts may carry keys
/// canopy does not model. Parsed leniently, then narrowed to `ProjectConfig`.
#[derive(Deserialize, Default)]
struct ForeignFile {
    #[serde(default)]
    scripts: ForeignScripts,
}

#[derive(Deserialize, Default)]
struct ForeignScripts {
    #[serde(default)]
    setup: String,
    #[serde(default)]
    run: Option<ForeignRun>,
    #[serde(default)]
    archive: String,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum ForeignRun {
    Single(String),
    Named(BTreeMap<String, ForeignRunScript>),
}

#[derive(Deserialize)]
struct ForeignRunScript {
    command: String,
    #[serde(default)]
    args: Vec<String>,
    #[serde(default)]
    options: ForeignRunOptions,
    #[serde(default)]
    default: bool,
    #[serde(default)]
    hide: bool,
}

#[derive(Deserialize, Default)]
struct ForeignRunOptions {
    #[serde(default)]
    cwd: String,
}

impl From<ForeignFile> for ProjectConfig {
    fn from(c: ForeignFile) -> Self {
        let run = match c.scripts.run {
            None => RunScripts::default(),
            Some(ForeignRun::Single(s)) => RunScripts::Single(s),
            Some(ForeignRun::Named(m)) => RunScripts::Named(
                m.into_iter()
                    .map(|(k, v)| {
                        (k, RunScript { command: v.command, args: v.args, cwd: v.options.cwd, default: v.default, hide: v.hide })
                    })
                    .collect(),
            ),
        };
        Self {
            scripts: Scripts { setup: c.scripts.setup, run, archive: c.scripts.archive, ..Scripts::default() },
            ..Self::default()
        }
    }
}

impl ProjectConfig {
    /// Walk up from `start` looking for `canopy.json`. Returns (project root, config).
    pub fn discover(start: &Path) -> Result<(PathBuf, Self), ConfigError> {
        let mut dir = Some(start);
        while let Some(d) = dir {
            let candidate = d.join(FILE_NAME);
            if candidate.is_file() {
                return Ok((d.to_path_buf(), Self::load(&candidate)?));
            }
            dir = d.parent();
        }
        Err(ConfigError::NotFound(start.to_path_buf()))
    }

    pub fn load(path: &Path) -> Result<Self, ConfigError> {
        let text = std::fs::read_to_string(path)
            .map_err(|source| ConfigError::Io { path: path.to_path_buf(), source })?;
        let cfg: Self = serde_json::from_str(&text)
            .map_err(|source| ConfigError::Invalid { path: path.to_path_buf(), source })?;
        cfg.validate()?;
        Ok(cfg)
    }

    /// Semantic checks. Existence/executability of scripts is deliberately *not* checked
    /// here: the runner's error is more precise, and `canopy init` may write the config
    /// before the scripts exist.
    pub fn validate(&self) -> Result<(), ConfigError> {
        if let RunScripts::Named(m) = &self.scripts.run {
            let defaults = m.values().filter(|s| s.default).count();
            if defaults > 1 {
                return Err(ConfigError::Semantic("more than one run script marked default".into()));
            }
            if let Some((name, _)) = m.iter().find(|(_, s)| s.command.is_empty()) {
                return Err(ConfigError::Semantic(format!("run script {name:?} has an empty command")));
            }
        }
        Ok(())
    }

    /// Build a config from any JSON text with a compatible `scripts` table.
    pub fn from_scripts_json(text: &str) -> Result<Self, serde_json::Error> {
        let c: ForeignFile = serde_json::from_str(text)?;
        Ok(c.into())
    }

    /// Build a config from any TOML text with a compatible `scripts` table. Keys we do not
    /// model (`run_mode`, `icon`, `available_in`, …) are dropped.
    pub fn from_scripts_toml(text: &str) -> Result<Self, toml::de::Error> {
        let c: ForeignFile = toml::from_str(text)?;
        Ok(c.into())
    }

    /// Adopt the `scripts` table from `file` (TOML by `.toml` extension, else JSON). Scripts
    /// written for another tool read that tool's variable names, so the tool's name (taken
    /// from the config file's own name) is added to `env_prefixes`.
    pub fn adopt_from(file: &Path) -> Result<Self, ConfigError> {
        let text = std::fs::read_to_string(file).map_err(|source| ConfigError::Io { path: file.to_path_buf(), source })?;
        let mut cfg = if file.extension().is_some_and(|e| e == "toml") {
            Self::from_scripts_toml(&text).map_err(|e| ConfigError::Semantic(format!("{}: {e}", file.display())))?
        } else {
            Self::from_scripts_json(&text).map_err(|source| ConfigError::Invalid { path: file.to_path_buf(), source })?
        };
        if let Some(prefix) = Self::tool_prefix(file) {
            if !cfg.env_prefixes.contains(&prefix) {
                cfg.env_prefixes.push(prefix);
            }
        }
        Ok(cfg)
    }

    /// `.oldtool/settings.toml` -> `OLDTOOL`, `othertool.json` -> `OTHERTOOL`: the first
    /// path component that is not a generic word, upper-cased. `None` for `canopy.json`.
    pub fn tool_prefix(file: &Path) -> Option<String> {
        const GENERIC: &[&str] = &["settings", "config", "configuration", "workspace", "workspaces"];
        let stem = file.file_stem().and_then(|s| s.to_str()).map(|s| s.trim_start_matches('.').to_ascii_lowercase()).unwrap_or_default();
        if stem == "canopy" {
            return None;
        }
        let name = if GENERIC.contains(&stem.as_str()) || stem.is_empty() {
            // `.oldtool/settings.toml`: the directory names the tool.
            let parent = file.parent().and_then(|p| p.file_name()).and_then(|s| s.to_str()).map(|s| s.trim_start_matches('.').to_ascii_lowercase()).unwrap_or_default();
            if parent.is_empty() || parent == "canopy" || GENERIC.contains(&parent.as_str()) {
                return None;
            }
            parent
        } else {
            stem
        };
        Some(name.chars().map(|ch| if ch.is_ascii_alphanumeric() { ch.to_ascii_uppercase() } else { '_' }).collect())
    }

    /// First existing candidate (relative to `root`) from a user-configured list.
    pub fn find_adoptable(root: &Path, candidates: &[String]) -> Option<PathBuf> {
        candidates.iter().map(|c| root.join(c)).find(|p| p.is_file())
    }

    pub fn to_json_pretty(&self) -> String {
        serde_json::to_string_pretty(self).expect("config serializes")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_object_is_valid() {
        let cfg: ProjectConfig = serde_json::from_str("{}").unwrap();
        assert_eq!(cfg, ProjectConfig::default());
        assert!(cfg.scripts.run.default_script().is_none());
    }

    #[test]
    fn v0_shape_parses() {
        let cfg: ProjectConfig = serde_json::from_str(r#"{
            "scripts": {"setup": "bin/canopy-setup", "run": "bin/dev", "archive": "bin/canopy-archive"},
            "agent": {"type": "codex", "briefing": "hi"}
        }"#).unwrap();
        assert_eq!(cfg.scripts.setup, "bin/canopy-setup");
        assert_eq!(cfg.agent.kind, "codex");
        let (name, s) = cfg.scripts.run.default_script().unwrap();
        assert_eq!(name, "default");
        assert_eq!(s.command, "bin/dev");
        assert_eq!(cfg.scripts.timeouts, Timeouts::default());
    }

    #[test]
    fn named_run_scripts() {
        let cfg: ProjectConfig = serde_json::from_str(r#"{"scripts": {"run": {
            "web": {"command": "bin/dev", "default": true},
            "jobs": {"command": "bin/jobs", "args": ["--once"], "cwd": "svc"}
        }}}"#).unwrap();
        cfg.validate().unwrap();
        let (name, s) = cfg.scripts.run.default_script().unwrap();
        assert_eq!(name, "web");
        assert_eq!(s.command, "bin/dev");
    }

    #[test]
    fn two_defaults_rejected() {
        let cfg: ProjectConfig = serde_json::from_str(r#"{"scripts": {"run": {
            "a": {"command": "x", "default": true}, "b": {"command": "y", "default": true}}}}"#).unwrap();
        assert!(matches!(cfg.validate(), Err(ConfigError::Semantic(_))));
    }

    #[test]
    fn unknown_fields_rejected() {
        assert!(serde_json::from_str::<ProjectConfig>(r#"{"scirpts": {}}"#).is_err());
    }

    #[test]
    fn foreign_json_adoption() {
        let cfg = ProjectConfig::from_scripts_json(r#"{"scripts": {"setup": "s", "run": "r", "archive": "a"}}"#).unwrap();
        assert_eq!(cfg.scripts.setup, "s");
        assert_eq!(cfg.scripts.archive, "a");
    }

    #[test]
    fn foreign_toml_adoption() {
        let cfg = ProjectConfig::from_scripts_toml(r#"
[scripts]
setup = "bin/setup"
archive = "bin/archive"
run_mode = "concurrent"

[scripts.run.web]
command = "bin/dev"
default = true
icon = "server"
available_in = ["local"]

[scripts.run.jobs]
command = "bin/jobs"
args = ["--once"]
options = { cwd = "svc" }
"#).unwrap();
        cfg.validate().unwrap();
        assert_eq!(cfg.scripts.setup, "bin/setup");
        let (name, web) = cfg.scripts.run.default_script().unwrap();
        assert_eq!(name, "web");
        assert_eq!(web.command, "bin/dev");
        let named = cfg.scripts.run.named();
        assert_eq!(named["jobs"].cwd, "svc");
        assert_eq!(named["jobs"].args, vec!["--once"]);
    }

    #[test]
    fn adopt_from_file_and_candidates() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join(".other")).unwrap();
        std::fs::write(dir.path().join(".other/settings.toml"), "[scripts]\nsetup = \"t\"\n").unwrap();
        std::fs::write(dir.path().join("other.json"), r#"{"scripts": {"setup": "j"}}"#).unwrap();
        let candidates = vec![".other/settings.toml".to_string(), "other.json".to_string()];
        let found = ProjectConfig::find_adoptable(dir.path(), &candidates).unwrap();
        assert!(found.ends_with(".other/settings.toml"));
        assert_eq!(ProjectConfig::adopt_from(&found).unwrap().scripts.setup, "t");
        assert_eq!(ProjectConfig::adopt_from(&dir.path().join("other.json")).unwrap().scripts.setup, "j");
        assert!(ProjectConfig::find_adoptable(dir.path(), &["nope.toml".to_string()]).is_none());
        assert!(ProjectConfig::adopt_from(Path::new("/nonexistent.json")).is_err());
    }

    #[test]
    fn adoption_derives_env_prefix_from_tool_name() {
        assert_eq!(ProjectConfig::tool_prefix(Path::new("/r/.oldtool/settings.toml")).as_deref(), Some("OLDTOOL"));
        assert_eq!(ProjectConfig::tool_prefix(Path::new("/r/oldtool.json")).as_deref(), Some("OLDTOOL"));
        assert_eq!(ProjectConfig::tool_prefix(Path::new("/r/.othertool/config.json")).as_deref(), Some("OTHERTOOL"));
        assert_eq!(ProjectConfig::tool_prefix(Path::new("/r/canopy.json")), None);
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join(".oldtool")).unwrap();
        std::fs::write(dir.path().join(".oldtool/settings.toml"), "[scripts]\nsetup = \"bin/s\"\n").unwrap();
        let cfg = ProjectConfig::adopt_from(&dir.path().join(".oldtool/settings.toml")).unwrap();
        assert_eq!(cfg.env_prefixes, vec!["OLDTOOL"]);
    }

    #[test]
    fn env_prefixes_roundtrip() {
        let cfg: ProjectConfig = serde_json::from_str(r#"{"env_prefixes": ["LEGACY"]}"#).unwrap();
        assert_eq!(cfg.env_prefixes, vec!["LEGACY"]);
        assert!(cfg.to_json_pretty().contains("env_prefixes"));
        assert!(!ProjectConfig::default().to_json_pretty().contains("env_prefixes"));
    }

    #[test]
    fn discover_walks_up() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join(FILE_NAME), "{}").unwrap();
        let nested = dir.path().join("a/b/c");
        std::fs::create_dir_all(&nested).unwrap();
        let (root, _) = ProjectConfig::discover(&nested).unwrap();
        assert_eq!(root, dir.path());
        assert!(matches!(ProjectConfig::discover(Path::new("/")), Err(ConfigError::NotFound(_))));
    }
}
