//! UI state shared by every canopy surface through `~/.canopy/sidebar.json`: the
//! sidebar's open/closed state, folded projects, expanded tab lists and the cursor. The
//! dashboard reads and writes the folds too, so both views agree.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};

#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(default)]
pub struct SharedUi {
    pub strip: bool,
    pub collapsed: BTreeSet<PathBuf>,
    pub expanded: BTreeSet<String>,
    pub cursor: Option<String>,
}

impl SharedUi {
    pub fn path(home: &Path) -> PathBuf {
        home.join("sidebar.json")
    }
    pub fn load(path: &Path) -> Self {
        std::fs::read_to_string(path).ok().and_then(|t| serde_json::from_str(&t).ok()).unwrap_or_default()
    }
    pub fn save(&self, path: &Path) {
        if let Some(parent) = path.parent() {
            let _ = std::fs::create_dir_all(parent);
        }
        let tmp = path.with_extension("json.tmp");
        if std::fs::write(&tmp, serde_json::to_string(self).unwrap_or_default()).is_ok() {
            let _ = std::fs::rename(tmp, path);
        }
    }
}

pub fn mtime(path: &Path) -> Option<std::time::SystemTime> {
    std::fs::metadata(path).and_then(|m| m.modified()).ok()
}
