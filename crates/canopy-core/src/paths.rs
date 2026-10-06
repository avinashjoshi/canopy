//! Where canopy keeps things on disk.
//!
//! Everything lives under `~/.canopy` (override: `CANOPY_HOME`) so that v0 workspaces at
//! `~/.canopy/workspaces/<project>/<name>` are adopted unchanged. The API socket prefers
//! `$XDG_RUNTIME_DIR/canopy/` because sockets belong on a tmpfs the login session owns.

use std::path::{Path, PathBuf};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Paths {
    pub home: PathBuf,
}

impl Paths {
    /// Resolve from the environment: `CANOPY_HOME`, else `$HOME/.canopy`.
    pub fn from_env() -> Self {
        let home = std::env::var_os("CANOPY_HOME")
            .map(PathBuf::from)
            .or_else(|| std::env::var_os("HOME").map(|h| Path::new(&h).join(".canopy")))
            .unwrap_or_else(|| PathBuf::from(".canopy"));
        Self { home }
    }

    pub fn with_home(home: impl Into<PathBuf>) -> Self {
        Self { home: home.into() }
    }

    pub fn state_file(&self) -> PathBuf {
        self.home.join("state.json")
    }
    pub fn state_lock(&self) -> PathBuf {
        self.home.join("state.json.lock")
    }
    pub fn snapshots_dir(&self) -> PathBuf {
        self.home.join("snapshots")
    }
    pub fn settings_file(&self) -> PathBuf {
        self.home.join("config.toml")
    }
    pub fn hosts_file(&self) -> PathBuf {
        self.home.join("hosts.json")
    }
    pub fn workspaces_dir(&self) -> PathBuf {
        self.home.join("workspaces")
    }
    pub fn log_dir(&self) -> PathBuf {
        self.home.join("log")
    }
    pub fn tmp_dir(&self) -> PathBuf {
        self.home.join("tmp")
    }

    /// Directory of a workspace: `<home>/workspaces/<project>/<name>`.
    pub fn workspace_dir(&self, project: &str, name: &str) -> PathBuf {
        self.workspaces_dir().join(project).join(name)
    }

    /// API socket. `CANOPY_SOCKET_PATH` wins, then the user's runtime dir
    /// (`$XDG_RUNTIME_DIR`, or `/run/user/<uid>` when that exists even though the variable is
    /// unset, as under a non-interactive ssh session), then `<home>/canopy.sock`.
    /// Deterministic on purpose: an ssh-spawned server and a terminal-spawned one must agree.
    pub fn socket(&self) -> PathBuf {
        if let Some(p) = std::env::var_os("CANOPY_SOCKET_PATH") {
            return PathBuf::from(p);
        }
        if let Some(rt) = std::env::var_os("XDG_RUNTIME_DIR") {
            return Path::new(&rt).join("canopy").join("canopy.sock");
        }
        #[cfg(unix)]
        {
            let uid = unsafe { libc_getuid() };
            let run = PathBuf::from(format!("/run/user/{uid}"));
            if run.is_dir() {
                return run.join("canopy").join("canopy.sock");
            }
        }
        self.home.join("canopy.sock")
    }
}

#[cfg(unix)]
unsafe fn libc_getuid() -> u32 {
    extern "C" {
        fn getuid() -> u32;
    }
    getuid()
}
