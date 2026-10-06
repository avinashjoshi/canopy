//! Newline-JSON API over a Unix socket.
//!
//! One request per connection; `events.subscribe` keeps the connection open and streams
//! `{"id":…,"result":{"type":"event","event":{…}}}` lines until the client disconnects.

use crate::app::{Shared, ShutdownReason};
use crate::manager;
use canopy_proto::{ApiError, ErrorCode, EventKind, Method, Request, Response, ResultBody, MAX_REQUEST_BYTES, PROTOCOL_VERSION};
use std::path::Path;
use std::time::Duration;
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{UnixListener, UnixStream};

/// Decode one request line. Malformed input becomes an `invalid_request` error response
/// addressed to id `"?"` when the id cannot be recovered.
#[allow(clippy::result_large_err)]
pub fn decode(line: &str) -> Result<Request, Response> {
    serde_json::from_str::<Request>(line).map_err(|e| {
        let id = serde_json::from_str::<serde_json::Value>(line)
            .ok()
            .and_then(|v| v.get("id").and_then(|i| i.as_str().map(str::to_owned)))
            .unwrap_or_else(|| "?".into());
        let code = if e.to_string().contains("unknown variant") { ErrorCode::UnknownMethod } else { ErrorCode::InvalidRequest };
        Response::Err { id, error: ApiError { code, message: e.to_string() } }
    })
}

pub fn capabilities() -> Vec<String> {
    vec!["events".into(), "tmux_backend".into(), "agent_reports".into()]
}

/// Bind the socket (0600, parent 0700), replacing a stale file.
pub fn bind(socket: &Path) -> std::io::Result<UnixListener> {
    if let Some(parent) = socket.parent() {
        std::fs::create_dir_all(parent)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o700));
        }
    }
    if socket.exists() {
        match std::os::unix::net::UnixStream::connect(socket) {
            Ok(_) => return Err(std::io::Error::new(std::io::ErrorKind::AddrInUse, "another canopy server owns this socket")),
            Err(_) => std::fs::remove_file(socket)?,
        }
    }
    let l = UnixListener::bind(socket)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(socket, std::fs::Permissions::from_mode(0o600))?;
    }
    Ok(l)
}

pub async fn serve(shared: Shared, listener: UnixListener) {
    loop {
        let shutdown = shared.lock().await.shutdown.is_some();
        if shutdown {
            break;
        }
        let accept = tokio::time::timeout(Duration::from_millis(500), listener.accept()).await;
        match accept {
            Ok(Ok((stream, _))) => {
                let shared = shared.clone();
                tokio::spawn(async move {
                    if let Err(e) = handle(shared, stream).await {
                        tracing::debug!(error = %e, "connection ended");
                    }
                });
            }
            Ok(Err(e)) => {
                tracing::warn!(error = %e, "accept failed");
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
            Err(_) => {}
        }
    }
}

async fn write_line<W: AsyncWriteExt + Unpin>(w: &mut W, v: &impl serde::Serialize) -> std::io::Result<()> {
    let mut s = serde_json::to_string(v)?;
    s.push('\n');
    w.write_all(s.as_bytes()).await?;
    w.flush().await
}

async fn handle(shared: Shared, stream: UnixStream) -> anyhow::Result<()> {
    let (r, mut w) = stream.into_split();
    let mut reader = BufReader::new(r).take(MAX_REQUEST_BYTES as u64 + 1);
    let mut line = String::new();
    let n = tokio::time::timeout(Duration::from_secs(5), reader.read_line(&mut line)).await??;
    if n == 0 {
        return Ok(());
    }
    if line.len() > MAX_REQUEST_BYTES {
        let resp = Response::Err { id: "?".into(), error: ApiError { code: ErrorCode::InvalidRequest, message: "request too large".into() } };
        write_line(&mut w, &resp).await?;
        return Ok(());
    }
    let req = match decode(&line) {
        Ok(r) => r,
        Err(resp) => {
            write_line(&mut w, &resp).await?;
            return Ok(());
        }
    };
    let id = req.id.clone();
    if let Method::EventsSubscribe { since } = req.method {
        return subscribe(shared, &mut w, id, since).await;
    }
    let resp = match dispatch(shared, req.method).await {
        Ok(result) => Response::Ok { id, result },
        Err(error) => Response::Err { id, error },
    };
    write_line(&mut w, &resp).await?;
    Ok(())
}

async fn subscribe<W: AsyncWriteExt + Unpin>(shared: Shared, w: &mut W, id: String, since: Option<u64>) -> anyhow::Result<()> {
    let (events, mut rx) = {
        let app = shared.lock().await;
        (app.events.clone(), app.events.subscribe())
    };
    if let Some(s) = since {
        match events.replay(s) {
            Ok(list) => {
                for ev in list {
                    write_line(w, &Response::Ok { id: id.clone(), result: ResultBody::Event { event: ev } }).await?;
                }
            }
            Err(_) => {
                write_line(w, &Response::Err { id, error: ApiError { code: ErrorCode::EventsLost, message: "cursor too old; resync with a full list".into() } }).await?;
                return Ok(());
            }
        }
    }
    loop {
        match rx.recv().await {
            Ok(ev) => {
                let stop = matches!(ev.kind, EventKind::ServerShutdown);
                write_line(w, &Response::Ok { id: id.clone(), result: ResultBody::Event { event: ev } }).await?;
                if stop {
                    break;
                }
            }
            Err(tokio::sync::broadcast::error::RecvError::Lagged(_)) => {
                write_line(w, &Response::Err { id: id.clone(), error: ApiError { code: ErrorCode::EventsLost, message: "subscriber lagged".into() } }).await?;
                break;
            }
            Err(_) => break,
        }
    }
    Ok(())
}

pub async fn dispatch(shared: Shared, method: Method) -> Result<ResultBody, ApiError> {
    use ResultBody as B;
    Ok(match method {
        Method::Ping => B::Pong { version: crate::VERSION.into(), protocol: PROTOCOL_VERSION, capabilities: capabilities() },
        Method::ServerStatus => {
            let app = shared.lock().await;
            B::ServerStatus {
                pid: std::process::id(),
                version: crate::VERSION.into(),
                uptime_secs: app.started.elapsed().as_secs(),
                backend: app.backend.name().into(),
                socket: app.paths.socket(),
            }
        }
        Method::ServerStop => {
            let mut app = shared.lock().await;
            app.shutdown = Some(ShutdownReason::ApiStop);
            B::Ack
        }
        Method::ServerReloadConfig => {
            let mut app = shared.lock().await;
            app.reload_settings().map_err(internal)?;
            B::Ack
        }
        Method::ProjectList => {
            let app = shared.lock().await;
            B::ProjectList { projects: manager::project_rows(&app) }
        }
        Method::ProjectAdd { root } => {
            let mut app = shared.lock().await;
            let p = manager::ensure_project(&mut app, &root)?;
            B::ProjectList { projects: manager::project_rows(&app).into_iter().filter(|r| r.root == p.root).collect() }
        }
        Method::ProjectMain { root } => B::AttachTarget { target: manager::main_target(shared, &root).await? },
        Method::ProjectInit { path, url, with_scripts, adopt_from } => {
            let p = manager::project_init(shared.clone(), path, url, with_scripts, adopt_from).await?;
            let app = shared.lock().await;
            B::ProjectList { projects: manager::project_rows(&app).into_iter().filter(|r| r.root == p.root).collect() }
        }
        Method::ProjectStopMain { root } => {
            manager::stop_main(shared, &root).await?;
            B::Ack
        }
        Method::ProjectRemove { root } => {
            manager::project_remove(shared, &root).await?;
            B::Ack
        }
        Method::ProjectPullRequests { root } => B::PickList { items: manager::pick_pull_requests(shared, &root).await? },
        Method::ProjectIssues { root } => B::PickList { items: manager::pick_issues(shared, &root).await? },
        Method::ProjectBranches { root } => B::PickList { items: manager::pick_branches(shared, &root).await? },
        Method::WorkspaceList { project_root } => {
            let app = shared.lock().await;
            B::WorkspaceList { workspaces: app.rows(project_root.as_deref()) }
        }
        Method::WorkspaceGet { workspace } => {
            let app = shared.lock().await;
            let ws = manager::resolve_ref(&app, &workspace)?;
            B::Workspace { workspace: app.row(&ws) }
        }
        Method::WorkspaceCreate(req) => B::Workspace { workspace: manager::create(shared, req).await? },
        Method::WorkspaceRemove { workspace, force } => {
            manager::remove(shared, workspace, force).await?;
            B::Ack
        }
        Method::WorkspaceStop { workspace } => {
            manager::stop(shared, workspace).await?;
            B::Ack
        }
        Method::WorkspaceRetry { workspace, force } => B::Workspace { workspace: manager::retry(shared, workspace, force).await? },
        Method::WorkspaceLog { workspace, offset, lines } => {
            let (path, text, offset, running, status) = manager::log(shared, workspace, offset, lines).await?;
            B::Log { path, text, offset, running, status }
        }
        Method::WorkspaceRename { workspace, pin } => B::Workspace { workspace: manager::rename(shared, workspace, pin).await? },
        Method::WorkspaceSetOwner { workspace, owner } => B::Workspace { workspace: manager::set_owner(shared, workspace, owner).await? },
        Method::WorkspaceReconcile { project_root } => {
            let mut app = shared.lock().await;
            B::WorkspaceList { workspaces: manager::reconcile(&mut app, project_root.as_deref())? }
        }
        Method::WorkspaceResurrect { workspace } => B::Workspace { workspace: manager::resurrect(shared, workspace).await? },
        Method::WorkspaceAttachTarget { workspace } => B::AttachTarget { target: manager::attach_target(shared, workspace).await? },
        Method::WorkspaceRun { workspace, script } => {
            manager::run_script(shared, workspace, script).await?;
            B::Ack
        }
        Method::WorkspaceWindows { workspace } => B::WindowList { windows: manager::windows(shared, workspace).await? },
        Method::WorkspaceSelectWindow { workspace, index } => {
            manager::select_window(shared, workspace, index).await?;
            B::Ack
        }
        Method::WorkspaceNewWindow { workspace, name } => {
            manager::new_window(shared, workspace, name).await?;
            B::Ack
        }
        Method::SessionSidebarToggle { session } => {
            manager::sidebar_toggle(shared, &session).await?;
            B::Ack
        }
        Method::SessionNotify { session, text } => {
            let app = shared.lock().await;
            app.backend.display_message(&session, &text).map_err(|e| ApiError { code: ErrorCode::BackendError, message: e.to_string() })?;
            B::Ack
        }
        Method::SessionSidebarEnsure { session } => {
            manager::sidebar_ensure(shared, &session).await?;
            B::Ack
        }
        Method::PaneReportAgent(rep) => {
            let mut app = shared.lock().await;
            let known = app.state.by_id(&rep.workspace_id).is_some();
            if !known {
                return Err(ApiError { code: ErrorCode::WorkspaceNotFound, message: rep.workspace_id });
            }
            if let Some(sid) = &rep.session_id {
                if let Some(w) = app.state.workspaces.iter_mut().find(|w| w.id == rep.workspace_id) {
                    if w.agent_session_id != *sid {
                        w.agent_session_id = sid.clone();
                        let _ = app.persist();
                    }
                }
            }
            if let Some(state) = app.agents.report(&rep.workspace_id, &rep.source, rep.seq, rep.state, rep.session_id.clone()) {
                app.events.publish(EventKind::AgentStatusChanged { workspace_id: rep.workspace_id.clone(), state });
            }
            B::Ack
        }
        Method::ClipboardSetText { text } => {
            tokio::task::spawn_blocking(move || crate::clipboard::set_text(&text)).await.map_err(internal)?.map_err(internal)?;
            B::Ack
        }
        Method::ClipboardGet => {
            let text = tokio::task::spawn_blocking(crate::clipboard::get_text).await.map_err(internal)?.map_err(internal)?;
            B::Clipboard { text }
        }
        Method::ClipboardSetFile { path, mime } => {
            tokio::task::spawn_blocking(move || crate::clipboard::set_file(&path, &mime)).await.map_err(internal)?.map_err(internal)?;
            B::Ack
        }
        Method::AgentList => {
            let app = shared.lock().await;
            let agents = app
                .state
                .workspaces
                .iter()
                .filter(|w| app.runtime_of(&w.id).alive)
                .map(|w| canopy_proto::AgentRow { workspace_id: w.id.clone(), agent: app.launcher_name_for(w), state: app.agents.state(&w.id), since_secs: app.agents.since(&w.id).as_secs() })
                .collect();
            B::AgentList { agents }
        }
        Method::EventsSubscribe { .. } => unreachable!("handled before dispatch"),
    })
}

fn internal(e: impl std::fmt::Display) -> ApiError {
    ApiError { code: ErrorCode::InternalError, message: e.to_string() }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decode_ok() {
        let r = decode(r#"{"id":"1","method":"ping"}"#).unwrap();
        assert_eq!(r.method, Method::Ping);
    }

    #[test]
    fn decode_unknown_method_keeps_id() {
        let Response::Err { id, error } = decode(r#"{"id":"9","method":"nope.x"}"#).unwrap_err() else { panic!() };
        assert_eq!(id, "9");
        assert_eq!(error.code, ErrorCode::UnknownMethod);
    }

    #[test]
    fn decode_garbage() {
        let Response::Err { id, error } = decode("not json").unwrap_err() else { panic!() };
        assert_eq!(id, "?");
        assert_eq!(error.code, ErrorCode::InvalidRequest);
    }
}
