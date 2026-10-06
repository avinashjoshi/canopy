//! Background observation loops. They derive runtime facts and publish events; they never
//! own state the API cannot also compute on demand.
//!
//! - every 500 ms: one backend snapshot -> session liveness, attached clients, agent screens
//! - every 15 s: branch follow + git hints
//! - every 60 s: PR status via `gh` (when installed)

use crate::app::Shared;
use crate::{hints, manager};
use canopy_core::state::Status;
use canopy_proto::EventKind;
use std::time::{Duration, Instant};

pub async fn run(shared: Shared) {
    let mut last_git = Instant::now() - Duration::from_secs(60);
    let mut last_pr = Instant::now() - Duration::from_secs(120);
    let mut last_cfg = Instant::now() - Duration::from_secs(60);
    let gh_installed = std::process::Command::new("gh").arg("--version").output().map(|o| o.status.success()).unwrap_or(false);
    loop {
        tokio::time::sleep(Duration::from_millis(500)).await;
        if shared.lock().await.shutdown.is_some() {
            return;
        }
        fast_tick(&shared).await;
        if last_cfg.elapsed() >= Duration::from_secs(10) {
            last_cfg = Instant::now();
            // The user's tmux server may have restarted; keybinds live only in its memory.
            let app = shared.lock().await;
            manager::ensure_backend_config(&app);
        }
        if last_git.elapsed() >= Duration::from_secs(15) {
            last_git = Instant::now();
            git_tick(&shared).await;
        }
        if gh_installed && last_pr.elapsed() >= Duration::from_secs(60) {
            last_pr = Instant::now();
            pr_tick(&shared).await;
        }
    }
}

async fn fast_tick(shared: &Shared) {
    let (backend, targets) = {
        let app = shared.lock().await;
        let targets: Vec<(String, String, String, Status)> = app
            .state
            .workspaces
            .iter()
            .filter(|w| !app.is_busy(&w.id))
            .map(|w| (w.id.clone(), app.session_name(w), app.launcher_name_for(w), w.status))
            .collect();
        (app.backend.clone(), targets)
    };
    // One snapshot for everything, then one screen read per live agent pane; off the lock.
    let (observed, mains_seen) = tokio::task::spawn_blocking(move || {
        let snap = backend.snapshot().unwrap_or_default();
        let mut out = Vec::with_capacity(targets.len());
        for (id, session, launcher, status) in targets {
            let info = snap.iter().find(|s| s.name == session);
            let alive = info.is_some();
            let attached = info.is_some_and(|s| s.attached);
            let screen = info
                .and_then(|s| s.panes.iter().find(|p| p.role.starts_with("agent:")))
                .and_then(|p| backend.read_screen(&p.id, 40).ok());
            out.push((id, launcher, status, alive, attached, screen));
        }
        let mains: Vec<(String, bool, bool)> = snap.iter().map(|s| (s.name.clone(), true, s.attached)).collect();
        (out, mains)
    })
    .await
    .unwrap_or_default();

    let mut app = shared.lock().await;
    let mut changed_status = false;
    let mut events = Vec::new();
    for (id, launcher, status, alive, attached, screen) in observed {
        {
            let rt = app.runtime_mut(&id);
            if rt.alive != alive || rt.attached != attached {
                events.push(EventKind::WorkspaceUpdated { workspace_id: id.clone() });
            }
            rt.alive = alive;
            rt.attached = attached;
        }
        if status == Status::Ready && !alive {
            if let Some(w) = app.state.workspaces.iter_mut().find(|w| w.id == id) {
                w.status = Status::Stopped;
                changed_status = true;
                events.push(EventKind::WorkspaceUpdated { workspace_id: id.clone() });
            }
        } else if status == Status::Stopped && alive {
            if let Some(w) = app.state.workspaces.iter_mut().find(|w| w.id == id) {
                w.status = Status::Ready;
                changed_status = true;
                events.push(EventKind::WorkspaceUpdated { workspace_id: id.clone() });
            }
        }
        if let Some(s) = screen {
            if let Some(state) = app.agents.observe_screen(&id, &launcher, &s) {
                events.push(EventKind::AgentStatusChanged { workspace_id: id.clone(), state });
            }
        }
    }
    // Main sessions: emit an update when their liveness/attachment flips too.
    let mains: Vec<(String, String)> = app.state.projects.values().map(|p| (format!("main:{}", p.name), app.main_session_name(&p.root))).collect();
    for (id, session) in mains {
        let found = mains_seen.iter().find(|(name, _, _)| *name == session);
        let alive = found.is_some();
        let attached = found.is_some_and(|(_, _, a)| *a);
        let rt = app.runtime_mut(&id);
        if rt.alive != alive || rt.attached != attached {
            rt.alive = alive;
            rt.attached = attached;
            events.push(EventKind::WorkspaceUpdated { workspace_id: id });
        }
    }
    let ids: std::collections::HashSet<String> = app.state.workspaces.iter().map(|w| w.id.clone()).collect();
    app.agents.retain(&|id| ids.contains(id));
    if changed_status {
        if let Err(e) = app.persist() {
            tracing::warn!(error = %e, "persist in poller");
        }
    }
    for ev in events {
        app.events.publish(ev);
    }
}

async fn git_tick(shared: &Shared) {
    let workspaces = {
        let mut app = shared.lock().await;
        let ids: Vec<String> = app.state.workspaces.iter().map(|w| w.id.clone()).collect();
        for id in ids {
            if let Err(e) = manager::sync_branch(&mut app, &id) {
                tracing::debug!(error = %e, "sync branch");
            }
        }
        app.state.workspaces.clone()
    };
    let computed = tokio::task::spawn_blocking(move || {
        workspaces
            .into_iter()
            .filter(|w| w.path.is_dir())
            .map(|w| {
                let default = crate::git::default_branch(&w.project_root);
                let h = hints::git_hints(&w, &default);
                (w.id, h)
            })
            .collect::<Vec<_>>()
    })
    .await
    .unwrap_or_default();
    let mut app = shared.lock().await;
    for (id, h) in computed {
        let rt = app.runtime_mut(&id);
        let changed = rt.hints != h;
        rt.hints = h;
        rt.hints_checked = Some(Instant::now());
        if changed {
            app.events.publish(EventKind::WorkspaceUpdated { workspace_id: id });
        }
    }
}

async fn pr_tick(shared: &Shared) {
    let paths: Vec<(String, std::path::PathBuf)> = {
        let app = shared.lock().await;
        app.state.workspaces.iter().filter(|w| w.path.is_dir()).map(|w| (w.id.clone(), w.path.clone())).collect()
    };
    let results = tokio::task::spawn_blocking(move || paths.into_iter().map(|(id, p)| (id, hints::pr_status(&p))).collect::<Vec<_>>())
        .await
        .unwrap_or_default();
    let mut app = shared.lock().await;
    for (id, pr) in results {
        let rt = app.runtime_mut(&id);
        let changed = rt.pr != pr;
        rt.pr = pr;
        rt.pr_checked = Some(Instant::now());
        if changed {
            app.events.publish(EventKind::WorkspaceUpdated { workspace_id: id });
        }
    }
}
