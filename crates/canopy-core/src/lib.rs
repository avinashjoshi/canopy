//! canopy domain model. No sockets, no TUI, no tmux: everything here is pure data,
//! filesystem persistence, and process-free algorithms that both the server and the
//! client can share.
//!
//! Dependency direction across the workspace is strictly leaf-up:
//! `canopy-core` <- `canopy-proto` <- `canopy-server` / `canopy-client` <- `canopy` (bin).

pub mod config;
pub mod env;
pub mod git;
pub mod namegen;
pub mod paths;
pub mod ports;
pub mod settings;
pub mod state;

pub use config::ProjectConfig;
pub use ports::PortPlan;
pub use state::{Project, State, Status, Workspace};
