//! Workspace health hints, recomputed every refresh and never persisted.
//!
//! Stuck-state hints (rebasing, merging, conflict, detached) preempt the ahead/behind
//! numbers because git's counts are transient mid-operation.

use crate::git::{self, StuckState};
use canopy_core::state::{SourceKind, Workspace};
use canopy_proto::{Hint, HintKind};
use std::path::Path;
use std::process::Command;

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct PrStatus {
    pub number: u64,
    /// `OPEN` | `MERGED` | `CLOSED`
    pub state: String,
    /// `APPROVED` | `CHANGES_REQUESTED` | `REVIEW_REQUIRED` | ``
    pub review: String,
    /// `SUCCESS` | `FAILURE` | `PENDING` | `` (rollup of status checks)
    pub checks: String,
    pub url: String,
}

/// Query `gh` for the PR of the branch checked out in `worktree`. `None` when gh is
/// missing, unauthenticated, or there is no PR.
pub fn pr_status(worktree: &Path) -> Option<PrStatus> {
    let out = Command::new("gh")
        .arg("pr")
        .arg("view")
        .arg("--json")
        .arg("number,state,reviewDecision,url,statusCheckRollup")
        .current_dir(worktree)
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    let v: serde_json::Value = serde_json::from_slice(&out.stdout).ok()?;
    let checks = v.get("statusCheckRollup").and_then(|r| r.as_array()).map(|arr| {
        let mut failed = false;
        let mut pending = false;
        for c in arr {
            let concl = c.get("conclusion").and_then(|x| x.as_str()).unwrap_or("");
            let status = c.get("status").and_then(|x| x.as_str()).unwrap_or("");
            let state = c.get("state").and_then(|x| x.as_str()).unwrap_or("");
            if matches!(concl, "FAILURE" | "TIMED_OUT" | "CANCELLED") || state == "FAILURE" || state == "ERROR" {
                failed = true;
            } else if (!status.is_empty() && status != "COMPLETED") || state == "PENDING" {
                pending = true;
            }
        }
        if arr.is_empty() { "" } else if failed { "FAILURE" } else if pending { "PENDING" } else { "SUCCESS" }.to_string()
    });
    Some(PrStatus {
        number: v.get("number").and_then(|n| n.as_u64()).unwrap_or(0),
        state: v.get("state").and_then(|s| s.as_str()).unwrap_or("").to_string(),
        review: v.get("reviewDecision").and_then(|s| s.as_str()).unwrap_or("").to_string(),
        checks: checks.unwrap_or_default(),
        url: v.get("url").and_then(|s| s.as_str()).unwrap_or("").to_string(),
    })
}

/// Git-derived hints for one workspace. Blocking; call from `spawn_blocking`.
pub fn git_hints(ws: &Workspace, default_branch: &str) -> Vec<Hint> {
    let mut hints = Vec::new();
    if !ws.path.is_dir() {
        return hints;
    }
    if let Some(stuck) = git::stuck_state(&ws.path) {
        let (kind, msg) = match stuck {
            StuckState::Rebasing => (HintKind::Rebasing, "rebase in progress"),
            StuckState::Merging => (HintKind::Merging, "merge in progress"),
            StuckState::CherryPicking => (HintKind::CherryPicking, "cherry-pick in progress"),
            StuckState::Detached => (HintKind::Detached, "detached HEAD"),
        };
        hints.push(Hint { kind, message: msg.into(), action: "finish or abort the git operation".into() });
    }
    let st = git::stats(&ws.path, default_branch);
    if st.conflicted > 0 {
        hints.push(Hint { kind: HintKind::Conflict, message: format!("{} conflicted file(s)", st.conflicted), action: "resolve conflicts".into() });
    }
    if hints.is_empty() && (st.ahead > 0 || st.behind > 0 || st.dirty_tracked > 0) {
        let mut parts = Vec::new();
        if st.ahead > 0 { parts.push(format!("↑{}", st.ahead)); }
        if st.behind > 0 { parts.push(format!("↓{}", st.behind)); }
        if st.dirty_tracked > 0 { parts.push(format!("*{}", st.dirty_tracked)); }
        hints.push(Hint { kind: HintKind::AheadBehind, message: parts.join(" "), action: String::new() });
    }
    match (st.unpushed, st.upstream_diverged) {
        (_, true) => hints.push(Hint { kind: HintKind::Diverged, message: "⇅ diverged from upstream".into(), action: "pull --rebase or force-push".into() }),
        (Some(n), false) if n > 0 => hints.push(Hint { kind: HintKind::Unpushed, message: format!("⇡{n} unpushed"), action: "git push".into() }),
        _ => {}
    }
    // Rename suggested: still on a generated name but there is real progress (commits past
    // origin/<default> or tracked-file edits; untracked noise excluded on purpose).
    if ws.source_kind == SourceKind::Fresh && canopy_core::namegen::is_generated(&ws.branch) && (st.ahead > 0 || st.dirty_tracked > 0) {
        hints.push(Hint { kind: HintKind::RenameSuggested, message: "↗ rename-suggested".into(), action: "git branch -m <name>".into() });
    }
    hints
}

pub fn pr_hint(pr: &PrStatus) -> Hint {
    let mut label = match pr.state.as_str() {
        "MERGED" => "✓ merged".to_string(),
        "CLOSED" => "✗ closed".to_string(),
        _ => match pr.review.as_str() {
            "APPROVED" => "PR approved".into(),
            "CHANGES_REQUESTED" => "PR changes requested".into(),
            _ => "PR open".into(),
        },
    };
    match pr.checks.as_str() {
        "FAILURE" => label.push_str(" · CI ✗"),
        "PENDING" => label.push_str(" · CI …"),
        "SUCCESS" => label.push_str(" · CI ✓"),
        _ => {}
    }
    let action = if pr.state == "MERGED" { "canopy rm".to_string() } else { pr.url.clone() };
    Hint { kind: if pr.state == "MERGED" { HintKind::Shipped } else { HintKind::PrStatus }, message: format!("{label} #{}", pr.number), action }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pr_hint_labels() {
        let h = pr_hint(&PrStatus { number: 7, state: "MERGED".into(), ..Default::default() });
        assert_eq!(h.message, "✓ merged #7");
        assert_eq!(h.kind, HintKind::Shipped);
        let h = pr_hint(&PrStatus { number: 8, state: "OPEN".into(), review: "APPROVED".into(), checks: "FAILURE".into(), url: "u".into() });
        assert_eq!(h.message, "PR approved · CI ✗ #8");
        assert_eq!(h.action, "u");
    }

    #[test]
    fn missing_dir_yields_nothing() {
        let ws = Workspace { path: "/nonexistent/x".into(), ..Workspace::default() };
        assert!(git_hints(&ws, "main").is_empty());
    }
}
