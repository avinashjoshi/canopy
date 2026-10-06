//! Strided port plan: one base per project, one block per workspace.
//!
//! ```text
//! project_base(Nth project) = base + N * project_stride      (first-come-first-served, persisted)
//! project_base + 0                 reserved for `canopy main`
//! project_base + M * workspace_stride   = Mth workspace        (smallest free slot)
//! ```
//!
//! Every workspace owns the block `port .. port + workspace_stride - 1` (default ten ports,
//! exposed as `CANOPY_PORT` .. `CANOPY_PORT+9`).

use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, schemars::JsonSchema)]
#[serde(default)]
pub struct PortPlan {
    /// First project's base port. Default 40000: clear of webapp defaults (3000-9000),
    /// k8s NodePort (30000-32767) and the IANA ephemeral range (49152+).
    pub base: u16,
    /// Distance between consecutive project bases.
    pub project_stride: u16,
    /// Distance between workspaces within a project.
    pub workspace_stride: u16,
}

impl Default for PortPlan {
    fn default() -> Self {
        Self { base: 40000, project_stride: 1000, workspace_stride: 10 }
    }
}

#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum PortError {
    #[error("no free port in {min}..={max} (stride {stride})")]
    Exhausted { min: u16, max: u16, stride: u16 },
    #[error("invalid port plan: {0}")]
    InvalidPlan(&'static str),
    #[error("too many projects for the port plan (max {0})")]
    TooManyProjects(usize),
}

impl PortPlan {
    pub const MAX_PROJECTS: usize = 100;

    pub fn validate(&self) -> Result<(), PortError> {
        if self.base < 1024 {
            return Err(PortError::InvalidPlan("base must be >= 1024"));
        }
        if self.project_stride == 0 {
            return Err(PortError::InvalidPlan("project_stride must be > 0"));
        }
        if self.workspace_stride == 0 {
            return Err(PortError::InvalidPlan("workspace_stride must be > 0"));
        }
        if self.workspace_stride > self.project_stride {
            return Err(PortError::InvalidPlan("workspace_stride must be <= project_stride"));
        }
        Ok(())
    }

    /// Base port for the next project, given the bases already handed out.
    pub fn next_project_base(&self, used_bases: &BTreeSet<u16>) -> Result<u16, PortError> {
        for n in 0..Self::MAX_PROJECTS {
            let candidate = u32::from(self.base) + (n as u32) * u32::from(self.project_stride);
            if candidate > u32::from(u16::MAX) {
                break;
            }
            let candidate = candidate as u16;
            if !used_bases.contains(&candidate) {
                return Ok(candidate);
            }
        }
        Err(PortError::TooManyProjects(Self::MAX_PROJECTS))
    }

    /// Range of workspace ports for a project with the given base: first slot is one
    /// stride above the base (base itself is `canopy main`), last slot is just below
    /// the next project's base.
    pub fn workspace_range(&self, project_base: u16) -> (u16, u16) {
        let min = project_base.saturating_add(self.workspace_stride);
        let max = u32::from(project_base) + u32::from(self.project_stride) - 1;
        (min, max.min(u32::from(u16::MAX)) as u16)
    }

    /// Smallest free strided slot in `[min, max]` that is not in `used` and passes `probe`
    /// (callers inject a `127.0.0.1` bind probe; tests inject a closure).
    pub fn allocate(
        min: u16,
        max: u16,
        stride: u16,
        used: &BTreeSet<u16>,
        mut probe: impl FnMut(u16) -> bool,
    ) -> Result<u16, PortError> {
        if stride == 0 {
            return Err(PortError::InvalidPlan("stride must be > 0"));
        }
        let mut p = u32::from(min);
        while p <= u32::from(max) {
            let port = p as u16;
            if !used.contains(&port) && probe(port) {
                return Ok(port);
            }
            p += u32::from(stride);
        }
        Err(PortError::Exhausted { min, max, stride })
    }

    /// Allocate a workspace port inside a project's range.
    pub fn allocate_workspace(
        &self,
        project_base: u16,
        used: &BTreeSet<u16>,
        probe: impl FnMut(u16) -> bool,
    ) -> Result<u16, PortError> {
        let (min, max) = self.workspace_range(project_base);
        Self::allocate(min, max, self.workspace_stride, used, probe)
    }
}

/// Probe whether `127.0.0.1:port` can be bound right now. Catches ports held by Docker,
/// stray dev servers, or anything canopy does not know about.
pub fn tcp_probe(port: u16) -> bool {
    std::net::TcpListener::bind(("127.0.0.1", port)).is_ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn always(_: u16) -> bool {
        true
    }

    #[test]
    fn defaults_validate() {
        PortPlan::default().validate().unwrap();
    }

    #[test]
    fn project_bases_step_by_stride() {
        let plan = PortPlan::default();
        let mut used = BTreeSet::new();
        assert_eq!(plan.next_project_base(&used).unwrap(), 40000);
        used.insert(40000);
        assert_eq!(plan.next_project_base(&used).unwrap(), 41000);
        used.insert(41000);
        used.insert(42000);
        assert_eq!(plan.next_project_base(&used).unwrap(), 43000);
    }

    #[test]
    fn workspace_range_skips_main_slot() {
        let plan = PortPlan::default();
        assert_eq!(plan.workspace_range(40000), (40010, 40999));
    }

    #[test]
    fn allocate_picks_smallest_free_slot() {
        let plan = PortPlan::default();
        let used: BTreeSet<u16> = [40010, 40020].into_iter().collect();
        assert_eq!(plan.allocate_workspace(40000, &used, always).unwrap(), 40030);
    }

    #[test]
    fn allocate_respects_probe() {
        let plan = PortPlan::default();
        let used = BTreeSet::new();
        let port = plan.allocate_workspace(40000, &used, |p| p != 40010).unwrap();
        assert_eq!(port, 40020);
    }

    #[test]
    fn allocate_exhausts() {
        let err = PortPlan::allocate(40010, 40029, 10, &BTreeSet::new(), |_| false).unwrap_err();
        assert_eq!(err, PortError::Exhausted { min: 40010, max: 40029, stride: 10 });
    }

    #[test]
    fn invalid_plans_rejected() {
        let bad = PortPlan { workspace_stride: 2000, ..PortPlan::default() };
        assert!(bad.validate().is_err());
        let bad = PortPlan { base: 80, ..PortPlan::default() };
        assert!(bad.validate().is_err());
    }
}
