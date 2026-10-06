//! canopy server. See docs/design/v1-architecture.md.

pub mod agent;
pub mod api;
pub mod app;
pub mod backend;
pub mod clipboard;
pub mod daemon;
pub mod events;
pub mod git;
pub mod hints;
pub mod integration;
pub mod launcher;
pub mod manager;
pub mod pollers;
pub mod scripts;

pub const VERSION: &str = env!("CARGO_PKG_VERSION");
