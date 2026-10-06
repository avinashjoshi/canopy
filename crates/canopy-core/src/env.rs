//! Environment canopy passes to scripts, sessions and agent hooks.
//!
//! `CANOPY_*` is canonical. The core variables are also exported under the built-in
//! compatibility prefixes (`COMPAT_ENV_PREFIXES`, so scripts written for the tool canopy
//! replaced keep working with no per-project setup) and under any `extra_prefixes` from
//! the user config `[env] prefixes` or a project's `env_prefixes`.

use std::collections::BTreeMap;
use std::path::Path;

/// Prefixes always mirrored, for backward compatibility with scripts written for the
/// workspace tool canopy replaced. Built in on purpose: projects need no configuration.
pub const COMPAT_ENV_PREFIXES: &[&str] = &["CONDUCTOR"];

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WorkspaceEnv<'a> {
    pub workspace_path: &'a Path,
    pub root_path: &'a Path,
    pub port: u16,
    pub workspace_name: &'a str,
    pub branch: &'a str,
    pub project: &'a str,
    /// Stable public id (`w7K`), used by hooks to report agent state.
    pub workspace_id: &'a str,
    pub socket_path: Option<&'a Path>,
    /// Additional prefixes to mirror the core variables under (upper-cased, `_` appended).
    pub extra_prefixes: &'a [String],
}

impl WorkspaceEnv<'_> {
    pub fn vars(&self) -> BTreeMap<String, String> {
        let mut m = BTreeMap::new();
        let ws = self.workspace_path.display().to_string();
        let root = self.root_path.display().to_string();
        let port = self.port.to_string();
        for (k, v) in [
            ("CANOPY_WORKSPACE_PATH", ws.clone()),
            ("CANOPY_ROOT_PATH", root.clone()),
            ("CANOPY_PORT", port.clone()),
            ("CANOPY_PORT_END", (u32::from(self.port) + 9).min(u32::from(u16::MAX)).to_string()),
            ("CANOPY_WORKSPACE_NAME", self.workspace_name.to_string()),
            ("CANOPY_BRANCH", self.branch.to_string()),
            ("CANOPY_PROJECT", self.project.to_string()),
            ("CANOPY_WORKSPACE_ID", self.workspace_id.to_string()),
        ] {
            m.insert(k.to_string(), v);
        }
        for prefix in COMPAT_ENV_PREFIXES.iter().map(|p| p.to_string()).chain(self.extra_prefixes.iter().cloned()) {
            let prefix = prefix.trim().trim_end_matches('_').to_ascii_uppercase();
            if prefix.is_empty() || prefix == "CANOPY" {
                continue;
            }
            m.insert(format!("{prefix}_WORKSPACE_PATH"), ws.clone());
            m.insert(format!("{prefix}_ROOT_PATH"), root.clone());
            m.insert(format!("{prefix}_PORT"), port.clone());
            m.insert(format!("{prefix}_WORKSPACE_NAME"), self.workspace_name.to_string());
        }
        if let Some(sock) = self.socket_path {
            m.insert("CANOPY_SOCKET_PATH".into(), sock.display().to_string());
        }
        m
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn aliases_match() {
        let e = WorkspaceEnv {
            workspace_path: Path::new("/w"),
            root_path: Path::new("/r"),
            port: 40010,
            workspace_name: "bold-falcon",
            branch: "bold-falcon",
            project: "canopy",
            workspace_id: "w7K",
            socket_path: Some(Path::new("/run/canopy.sock")),
            extra_prefixes: &["other_".to_string(), "canopy".to_string(), "".to_string()],
        };
        let v = e.vars();
        assert_eq!(v["CANOPY_PORT"], "40010");
        assert_eq!(v["CANOPY_PORT_END"], "40019");
        assert_eq!(v["OTHER_PORT"], "40010");
        assert_eq!(v["OTHER_WORKSPACE_PATH"], "/w");
        for prefix in COMPAT_ENV_PREFIXES {
            assert_eq!(v[&format!("{prefix}_WORKSPACE_PATH")], "/w", "built-in compatibility prefix");
        }
        assert_eq!(v["CANOPY_SOCKET_PATH"], "/run/canopy.sock");
        assert!(!v.keys().any(|k| k.starts_with("_")));
    }
}
