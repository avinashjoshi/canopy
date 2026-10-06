//! `~/.canopy/config.toml`: per-user, per-machine settings (never committed).

use crate::ports::PortPlan;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::path::Path;

#[derive(Debug, thiserror::Error)]
pub enum SettingsError {
    #[error("cannot read {0}: {1}")]
    Io(std::path::PathBuf, #[source] std::io::Error),
    #[error("invalid {0}: {1}")]
    Parse(std::path::PathBuf, #[source] toml::de::Error),
    #[error("invalid settings: {0}")]
    Semantic(String),
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct Settings {
    pub ports: PortPlan,
    /// Where `canopy init <git-url>` clones. Env override: `CANOPY_SOURCE_ROOT`.
    pub source_root: Option<String>,
    pub editor: Editor,
    pub agent: AgentDefaults,
    pub backend: Backend,
    pub sidebar: Sidebar,
    pub integrations: Integrations,
    pub env: EnvSettings,
    pub init: InitSettings,
    pub ui: UiSettings,
    pub layouts: BTreeMap<String, Layout>,
}

/// Look of the TUI surfaces.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct UiSettings {
    /// Single-character mark used in one-line spots (dashboard pill, narrow headers).
    pub glyph: String,
}
impl Default for UiSettings {
    fn default() -> Self { Self { glyph: "ᛉ".into() } }
}

/// Environment handed to scripts and sessions.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct EnvSettings {
    /// Prefixes under which the core variables are mirrored for every project (a project's
    /// `env_prefixes` adds to this). Example: `["LEGACY"]` also exports `LEGACY_PORT`.
    pub prefixes: Vec<String>,
}

/// `canopy init` behavior.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct InitSettings {
    /// Files (relative to the repo root) to adopt a `scripts` table from when `canopy.json`
    /// is missing, tried in order. `canopy init --from <file>` overrides.
    pub adopt_from: Vec<String>,
}

/// Agent hook integrations (the agent reports its own state to canopy).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct Integrations {
    /// On server start, install hooks for agents found on PATH (backs up their settings).
    pub auto_install: bool,
}
impl Default for Integrations {
    fn default() -> Self { Self { auto_install: true } }
}

/// The `canopy sidebar` pane added to the left of every session.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct Sidebar {
    pub enabled: bool,
    /// Width in columns.
    pub width: u16,
    /// Follow focus: collapse to the strip when focus leaves the sidebar pane, expand when it
    /// enters. Off by default: the sidebar keeps one shared open/closed state across all
    /// sessions until you change it.
    pub auto_collapse: bool,
}
impl Default for Sidebar {
    fn default() -> Self { Self { enabled: true, width: 30, auto_collapse: false } }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct Editor {
    /// Command for the `ide` pane. Empty disables the pane.
    pub command: String,
}
impl Default for Editor {
    fn default() -> Self { Self { command: "nvim .".into() } }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct AgentDefaults {
    /// Launcher used when a project's `canopy.json` does not name one.
    pub kind: String,
}
impl Default for AgentDefaults {
    fn default() -> Self { Self { kind: "claude".into() } }
}

/// Which session backend the server uses on this machine.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum Backend {
    #[default]
    Tmux,
    Native,
}

/// A pane layout: list of panes with roles and relative sizes. The default is v0's
/// three-pane layout (editor top-left, agent top-right 30%, shell bottom 15%).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct Layout {
    pub panes: Vec<LayoutPane>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
pub struct LayoutPane {
    /// `ide`, `agent`, `shell`, or `run:<name>`.
    pub role: String,
    /// `right` | `below` relative to the pane named by `of`; first pane fills the window.
    #[serde(default)]
    pub split: Option<String>,
    /// Role of the pane to split. Defaults to the first pane.
    #[serde(default)]
    pub of: Option<String>,
    /// Percent of the split dimension given to this pane.
    #[serde(default)]
    pub size: Option<u8>,
}

impl Layout {
    pub fn default_three_pane() -> Self {
        Self {
            panes: vec![
                LayoutPane { role: "ide".into(), split: None, of: None, size: None },
                LayoutPane { role: "shell".into(), split: Some("below".into()), of: Some("ide".into()), size: Some(15) },
                LayoutPane { role: "agent".into(), split: Some("right".into()), of: Some("ide".into()), size: Some(30) },
            ],
        }
    }
}

impl Settings {
    pub fn load(path: &Path) -> Result<Self, SettingsError> {
        if !path.exists() {
            return Ok(Self::default());
        }
        let text = std::fs::read_to_string(path).map_err(|e| SettingsError::Io(path.to_path_buf(), e))?;
        let s: Self = toml::from_str(&text).map_err(|e| SettingsError::Parse(path.to_path_buf(), e))?;
        s.ports.validate().map_err(|e| SettingsError::Semantic(e.to_string()))?;
        Ok(s)
    }

    pub fn source_root(&self) -> Option<String> {
        std::env::var("CANOPY_SOURCE_ROOT").ok().or_else(|| self.source_root.clone())
    }

    pub fn layout(&self, name: &str) -> Layout {
        if !name.is_empty() {
            if let Some(l) = self.layouts.get(name) {
                return l.clone();
            }
        }
        self.layouts.get("default").cloned().unwrap_or_else(Layout::default_three_pane)
    }

    pub fn default_toml() -> String {
        toml::to_string_pretty(&Self::default()).expect("settings serialize")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_file_is_default() {
        let s = Settings::load(Path::new("/nonexistent/config.toml")).unwrap();
        assert_eq!(s, Settings::default());
    }

    #[test]
    fn partial_override() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("config.toml");
        std::fs::write(&p, "[ports]\nbase = 50000\n\n[editor]\ncommand = \"hx .\"\n").unwrap();
        let s = Settings::load(&p).unwrap();
        assert_eq!(s.ports.base, 50000);
        assert_eq!(s.ports.project_stride, 1000);
        assert_eq!(s.editor.command, "hx .");
        assert_eq!(s.backend, Backend::Tmux);
    }

    #[test]
    fn invalid_ports_rejected() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("config.toml");
        std::fs::write(&p, "[ports]\nworkspace_stride = 5000\n").unwrap();
        assert!(matches!(Settings::load(&p), Err(SettingsError::Semantic(_))));
    }

    #[test]
    fn default_toml_roundtrips() {
        let s: Settings = toml::from_str(&Settings::default_toml()).unwrap();
        assert_eq!(s, Settings::default());
    }

    #[test]
    fn layout_fallback() {
        let s = Settings::default();
        assert_eq!(s.layout("nope"), Layout::default_three_pane());
    }
}
