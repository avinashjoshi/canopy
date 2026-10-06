//! canopy thin client: a blocking API client for the CLI plus the ratatui TUI.

pub mod brand;
pub mod livelog;
pub mod newform;
pub mod remote;
pub mod shared;
pub mod sidebar;
pub mod transport;
pub mod tui;

pub use transport::Transport;

use anyhow::{Context, Result};
use canopy_core::paths::Paths;
use canopy_proto::{ApiError, Method, Request, Response, ResultBody, MAX_REQUEST_BYTES};
use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::time::Duration;

/// One request, one connection, one response. `timeout` bounds the read; slow verbs
/// (create, remove) pass `None` and wait as long as the server takes.
pub fn call_raw(socket: &Path, method: Method, timeout: Option<Duration>) -> Result<Response> {
    let mut stream = UnixStream::connect(socket).with_context(|| format!("connect to canopy server at {}", socket.display()))?;
    stream.set_read_timeout(timeout)?;
    stream.set_write_timeout(Some(Duration::from_secs(5)))?;
    let req = Request { id: "1".into(), method };
    let mut line = serde_json::to_string(&req)?;
    anyhow::ensure!(line.len() <= MAX_REQUEST_BYTES, "request exceeds {MAX_REQUEST_BYTES} bytes");
    line.push('\n');
    stream.write_all(line.as_bytes())?;
    let mut reader = BufReader::new(stream);
    let mut buf = String::new();
    reader.read_line(&mut buf).context("read response")?;
    anyhow::ensure!(!buf.is_empty(), "server closed the connection without a response");
    serde_json::from_str(&buf).context("decode response")
}

/// Call and unwrap into the result body, turning API errors into `anyhow` errors.
pub fn call(socket: &Path, method: Method) -> Result<ResultBody> {
    let slow = matches!(method, Method::WorkspaceCreate(_) | Method::WorkspaceRemove { .. } | Method::WorkspaceRetry { .. } | Method::WorkspaceResurrect { .. } | Method::WorkspaceAttachTarget { .. } | Method::ProjectMain { .. });
    let timeout = if slow { None } else { Some(Duration::from_secs(10)) };
    match call_raw(socket, method, timeout)? {
        Response::Ok { result, .. } => Ok(result),
        Response::Err { error, .. } => Err(ApiFailure(error).into()),
    }
}

#[derive(Debug, thiserror::Error)]
#[error("{}: {}", serde_json::to_value(.0.code).map(|v| v.as_str().unwrap_or("error").to_string()).unwrap_or_default(), .0.message)]
pub struct ApiFailure(pub ApiError);

/// Connect to the server, starting one if none is running. A server built from another
/// version of this binary (left over from an upgrade) is restarted first so client and
/// server always agree; tmux sessions are untouched and the sidebars respawn.
pub fn connect(paths: &Paths) -> Result<std::path::PathBuf> {
    canopy_server::daemon::ensure_running(paths)?;
    let socket = paths.socket();
    if std::env::var_os("CANOPY_NO_VERSION_CHECK").is_none() {
        if let Ok(Response::Ok { result: ResultBody::Pong { version, .. }, .. }) = call_raw(&socket, Method::Ping, Some(Duration::from_secs(3))) {
            if version != canopy_server::VERSION {
                eprintln!("canopy: server is {version}, this binary is {}; restarting the server", canopy_server::VERSION);
                let _ = call_raw(&socket, Method::ServerStop, Some(Duration::from_secs(5)));
                let deadline = std::time::Instant::now() + Duration::from_secs(5);
                while std::time::Instant::now() < deadline && UnixStream::connect(&socket).is_ok() {
                    std::thread::sleep(Duration::from_millis(50));
                }
                canopy_server::daemon::ensure_running(paths)?;
            }
        }
    }
    Ok(socket)
}

/// Open an `events.subscribe` stream. Returns the reader; each line is a `Response`.
pub fn subscribe(socket: &Path, since: Option<u64>) -> Result<BufReader<UnixStream>> {
    let mut stream = UnixStream::connect(socket)?;
    let req = Request { id: "ev".into(), method: Method::EventsSubscribe { since } };
    let mut line = serde_json::to_string(&req)?;
    line.push('\n');
    stream.write_all(line.as_bytes())?;
    Ok(BufReader::new(stream))
}

/// Replace this process with the attach command for a target.
pub fn exec_attach(target: &canopy_proto::AttachTarget) -> Result<()> {
    use canopy_proto::AttachTarget as T;
    use std::os::unix::process::CommandExt;
    let err = match target {
        T::Tmux { session, detach_others } => {
            let inside = std::env::var_os("TMUX").is_some();
            let mut cmd = std::process::Command::new("tmux");
            if inside {
                cmd.args(["switch-client", "-t", &format!("={session}")]);
                let st = cmd.status()?;
                if st.success() {
                    return Ok(());
                }
                anyhow::bail!("tmux switch-client failed");
            }
            cmd.arg("attach-session");
            if *detach_others && std::env::var_os("CANOPY_NO_DETACH").is_none() {
                cmd.arg("-d");
            }
            cmd.args(["-t", &format!("={session}")]);
            cmd.exec()
        }
        T::RemoteTmux { ssh_target, session, mosh } => {
            // Stay resident: the attach runs as a child while we mirror the local clipboard.
            let detach = std::env::var_os("CANOPY_NO_DETACH").is_none();
            remote::attach_supervised(ssh_target, session, *mosh, detach)?;
            std::process::exit(0);
        }
        T::Native { .. } => anyhow::bail!("native attach is not implemented yet"),
    };
    Err(anyhow::anyhow!("exec failed: {err}"))
}
