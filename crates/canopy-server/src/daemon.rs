//! Server process lifecycle: logging, socket, pollers, signals, shutdown.

use crate::app::{App, Shared, ShutdownReason};
use crate::{api, manager};
use canopy_core::paths::Paths;
use canopy_proto::EventKind;
use std::sync::Arc;
use std::time::{Duration, Instant};

pub fn init_logging(paths: &Paths, name: &str) -> anyhow::Result<()> {
    std::fs::create_dir_all(paths.log_dir())?;
    let file = std::fs::OpenOptions::new().create(true).append(true).open(paths.log_dir().join(format!("{name}.log")))?;
    let filter = tracing_subscriber::EnvFilter::try_from_env("CANOPY_LOG").unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(filter).with_writer(file).with_ansi(false).with_target(false).json().init();
    Ok(())
}

/// Tools the server shells out to (`git`, `gh`, `tmux`, agents) often live in per-user
/// locations that a non-interactive ssh session does not put on PATH. Prepend the usual
/// suspects so a server spawned over ssh behaves like one spawned from a terminal.
fn widen_path() {
    let Some(home) = std::env::var_os("HOME") else { return };
    let home = std::path::PathBuf::from(home);
    let extra = [
        home.join(".local/bin"),
        home.join(".cargo/bin"),
        home.join(".local/share/mise/shims"),
        home.join(".asdf/shims"),
        std::path::PathBuf::from("/usr/local/bin"),
        std::path::PathBuf::from("/opt/homebrew/bin"),
    ];
    let current = std::env::var_os("PATH").unwrap_or_default();
    let mut parts: Vec<std::path::PathBuf> = extra.into_iter().filter(|p| p.is_dir()).collect();
    parts.extend(std::env::split_paths(&current));
    let mut seen = std::collections::HashSet::new();
    parts.retain(|p| seen.insert(p.clone()));
    if let Ok(joined) = std::env::join_paths(parts) {
        std::env::set_var("PATH", joined);
    }
}

/// Run the server until `server.stop` or a signal. Blocks the current thread.
pub fn run(paths: Paths) -> anyhow::Result<()> {
    widen_path();
    // Same reasoning as the tmux environment: the server is a daemon, not an ssh login.
    for var in ["SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY"] {
        std::env::remove_var(var);
    }
    let rt = tokio::runtime::Builder::new_multi_thread().enable_all().build()?;
    let result = rt.block_on(async_main(paths));
    // Dropping the runtime would wait for every `spawn_blocking` task (a slow `gh` call, a
    // tmux probe) to finish, and a stop that never completes leaves a ghost server holding
    // an unlinked socket. Give them a moment, then leave regardless.
    rt.shutdown_timeout(Duration::from_secs(2));
    result?;
    std::process::exit(0)
}

async fn async_main(paths: Paths) -> anyhow::Result<()> {
    let socket = paths.socket();
    let listener = api::bind(&socket)?;
    let own_socket = socket_identity(&socket);
    let app = App::load(paths)?;
    let shared: Shared = Arc::new(tokio::sync::Mutex::new(app));
    tracing::info!(socket = %socket.display(), version = crate::VERSION, "canopy server started");

    // Reconcile once on startup so stale statuses from a previous life are corrected, apply
    // tmux config, and make sure installed agents report their state to us.
    {
        let mut app = shared.lock().await;
        if let Err(e) = manager::reconcile(&mut app, None) {
            tracing::warn!(error = %e, "startup reconcile");
        }
        // Always write once on startup so an imported older schema lands on disk as v3.
        if let Err(e) = app.persist() {
            tracing::warn!(error = %e, "startup persist");
        }
        manager::ensure_backend_config(&app);
        manager::redecorate_sessions(&app);
        let shims = crate::clipboard::ensure_shims();
        if !shims.is_empty() {
            tracing::info!(count = shims.len(), "wrote wayland clipboard shims");
        }
        let n = manager::restart_sidebars(&app);
        if n > 0 {
            tracing::info!(count = n, "respawned sidebar panes on the current build");
        }
        if app.settings.integrations.auto_install {
            auto_install_integrations(&app.paths);
        }
    }

    // Test hook: a blocking task that never finishes, standing in for a hung `gh` or tmux
    // call. `server stop` must still end the process (see tests/server_stop.rs).
    if std::env::var_os("CANOPY_TEST_STUCK_BLOCKING").is_some() {
        tokio::task::spawn_blocking(|| loop {
            std::thread::sleep(Duration::from_secs(3600));
        });
    }

    let serve = tokio::spawn(api::serve(shared.clone(), listener));
    let poll = tokio::spawn(crate::pollers::run(shared.clone()));
    let sig = tokio::spawn(wait_for_signal(shared.clone()));

    // Wait for shutdown.
    loop {
        if shared.lock().await.shutdown.is_some() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    {
        let app = shared.lock().await;
        app.events.publish(EventKind::ServerShutdown);
        if let Err(e) = app.persist() {
            tracing::error!(error = %e, "final persist");
        }
        tracing::info!(reason = ?app.shutdown, "canopy server stopping");
    }
    tokio::time::sleep(Duration::from_millis(200)).await;
    serve.abort();
    poll.abort();
    sig.abort();
    // Only unlink the socket file if it is still *ours*: a newer server may have replaced it
    // (an upgrade restarts us), and deleting its file would strand it on an unlinked inode.
    if own_socket.is_some() && socket_identity(&socket) == own_socket {
        let _ = std::fs::remove_file(&socket);
    }
    Ok(())
}

/// `(device, inode)` of the socket file, to tell our socket from a successor's at the same path.
fn socket_identity(path: &std::path::Path) -> Option<(u64, u64)> {
    use std::os::unix::fs::MetadataExt;
    std::fs::metadata(path).ok().map(|m| (m.dev(), m.ino()))
}

/// Install hook integrations for agents that are on PATH and not yet wired up. Backs up
/// the agent's settings file before editing. Failures are logged, never fatal.
fn auto_install_integrations(paths: &Paths) {
    let bin = std::env::current_exe().unwrap_or_else(|_| "canopy".into());
    for integ in crate::integration::INTEGRATIONS {
        let on_path = std::env::var_os("PATH").map(|p| std::env::split_paths(&p).any(|d| d.join(integ.agent).is_file())).unwrap_or(false);
        if !on_path {
            continue;
        }
        match crate::integration::healthy(integ.agent) {
            Ok(true) => {}
            Ok(false) => match crate::integration::install(paths, integ.agent, &bin) {
                Ok(file) => tracing::info!(agent = integ.agent, file = %file.display(), "installed agent hooks"),
                Err(e) => tracing::warn!(agent = integ.agent, error = %e, "could not install agent hooks"),
            },
            Err(e) => tracing::debug!(agent = integ.agent, error = %e, "integration status"),
        }
    }
}

async fn wait_for_signal(shared: Shared) {
    use tokio::signal::unix::{signal, SignalKind};
    let mut term = signal(SignalKind::terminate()).expect("SIGTERM handler");
    let mut int = signal(SignalKind::interrupt()).expect("SIGINT handler");
    let mut hup = signal(SignalKind::hangup()).expect("SIGHUP handler");
    loop {
        tokio::select! {
            _ = term.recv() => break,
            _ = int.recv() => break,
            _ = hup.recv() => {
                let mut app = shared.lock().await;
                if let Err(e) = app.reload_settings() { tracing::warn!(error = %e, "reload settings on SIGHUP"); }
            }
        }
    }
    shared.lock().await.shutdown = Some(ShutdownReason::Signal);
}

/// Spawn `current_exe server` detached (own session, stdio to /dev/null).
pub fn spawn_detached() -> anyhow::Result<()> {
    use std::process::{Command, Stdio};
    let exe = std::env::current_exe()?;
    let mut cmd = Command::new(exe);
    cmd.arg("server").stdin(Stdio::null()).stdout(Stdio::null()).stderr(Stdio::null());
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        // SAFETY: setsid is async-signal-safe and we touch nothing else before exec.
        unsafe {
            cmd.pre_exec(|| {
                nix::unistd::setsid().map_err(|e| std::io::Error::other(e.to_string()))?;
                Ok(())
            });
        }
    }
    cmd.spawn()?;
    Ok(())
}

/// Make sure a server is answering on `socket`, spawning one if needed.
pub fn ensure_running(paths: &Paths) -> anyhow::Result<()> {
    let socket = paths.socket();
    if std::os::unix::net::UnixStream::connect(&socket).is_ok() {
        return Ok(());
    }
    spawn_detached()?;
    let deadline = Instant::now() + Duration::from_secs(15);
    while Instant::now() < deadline {
        if std::os::unix::net::UnixStream::connect(&socket).is_ok() {
            return Ok(());
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    anyhow::bail!("canopy server did not start within 15s (see {})", paths.log_dir().join("server.log").display())
}

#[cfg(test)]
mod tests {
    use super::socket_identity;

    #[test]
    fn socket_identity_changes_when_the_file_is_replaced() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("s");
        assert_eq!(socket_identity(&p), None);
        std::fs::write(&p, b"a").unwrap();
        let first = socket_identity(&p);
        assert!(first.is_some());
        assert_eq!(socket_identity(&p), first, "same file, same identity");
        std::fs::remove_file(&p).unwrap();
        std::fs::write(&p, b"b").unwrap();
        assert_ne!(socket_identity(&p), first, "a replacement at the same path is a different file");
    }
}
