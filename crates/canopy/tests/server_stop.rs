//! `canopy server stop` must end the server *process*, not just its socket. (A runtime
//! drop that waits on blocking tasks once left ghost servers behind after every upgrade.)

use std::os::unix::net::UnixStream;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

fn bin() -> Command {
    Command::new(env!("CARGO_BIN_EXE_canopy"))
}

#[test]
fn server_stop_exits_the_process() {
    let dir = std::env::temp_dir().join(format!("canopy-stop-test-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let socket = dir.join("canopy.sock");
    let env = [
        ("CANOPY_HOME", dir.to_string_lossy().into_owned()),
        ("CANOPY_SOCKET_PATH", socket.to_string_lossy().into_owned()),
        ("CANOPY_TMUX_SOCKET", format!("canopy-stop-test-{}", std::process::id())),
        ("CANOPY_NO_VERSION_CHECK", "1".into()),
        // A blocking task that never returns, like a hung `gh` call mid-shutdown.
        ("CANOPY_TEST_STUCK_BLOCKING", "1".into()),
    ];

    let mut server = bin().args(["server"]).envs(env.iter().cloned()).stdout(Stdio::null()).stderr(Stdio::null()).spawn().expect("spawn server");
    let deadline = Instant::now() + Duration::from_secs(15);
    while UnixStream::connect(&socket).is_err() {
        assert!(Instant::now() < deadline, "server never opened its socket");
        std::thread::sleep(Duration::from_millis(50));
    }

    let st = bin().args(["server", "stop"]).envs(env.iter().cloned()).stdout(Stdio::null()).stderr(Stdio::null()).status().expect("run stop");
    assert!(st.success(), "server stop failed");

    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        match server.try_wait().expect("try_wait") {
            Some(status) => {
                assert!(status.success(), "server exited with {status}");
                break;
            }
            None if Instant::now() >= deadline => {
                let _ = server.kill();
                panic!("server process still alive 10s after `server stop`");
            }
            None => std::thread::sleep(Duration::from_millis(100)),
        }
    }
    assert!(UnixStream::connect(&socket).is_err(), "socket still accepting after stop");

    let _ = Command::new("tmux").args(["-L", &env[2].1, "kill-server"]).status();
    let _ = std::fs::remove_dir_all(&dir);
}

/// An old server stopping after a newer one took over the socket path (what an upgrade does)
/// must not delete the newer server's socket file.
#[test]
fn stopping_an_old_server_leaves_a_successors_socket_alone() {
    let dir = std::env::temp_dir().join(format!("canopy-succ-test-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let socket = dir.join("canopy.sock");
    let tmux = format!("canopy-succ-test-{}", std::process::id());
    let env = [
        ("CANOPY_HOME", dir.to_string_lossy().into_owned()),
        ("CANOPY_SOCKET_PATH", socket.to_string_lossy().into_owned()),
        ("CANOPY_TMUX_SOCKET", tmux.clone()),
        ("CANOPY_NO_VERSION_CHECK", "1".into()),
    ];
    let wait_socket = |socket: &std::path::Path| {
        let deadline = Instant::now() + Duration::from_secs(15);
        while UnixStream::connect(socket).is_err() {
            assert!(Instant::now() < deadline, "server never opened its socket");
            std::thread::sleep(Duration::from_millis(50));
        }
    };

    let mut old = bin().args(["server"]).envs(env.iter().cloned()).stdout(Stdio::null()).stderr(Stdio::null()).spawn().expect("spawn old");
    wait_socket(&socket);
    // A successor appears at the same path (bind unlinks the stale file and rebinds).
    std::fs::remove_file(&socket).unwrap();
    let mut new = bin().args(["server"]).envs(env.iter().cloned()).stdout(Stdio::null()).stderr(Stdio::null()).spawn().expect("spawn new");
    wait_socket(&socket);

    // Stop the OLD one via a signal (its socket file is gone, so `server stop` would reach the new one).
    unsafe {
        libc_kill(old.id() as i32);
    }
    let deadline = Instant::now() + Duration::from_secs(10);
    while old.try_wait().expect("try_wait").is_none() {
        assert!(Instant::now() < deadline, "old server did not exit");
        std::thread::sleep(Duration::from_millis(100));
    }

    assert!(UnixStream::connect(&socket).is_ok(), "the successor's socket file was deleted by the old server's shutdown");

    let _ = bin().args(["server", "stop"]).envs(env.iter().cloned()).stdout(Stdio::null()).stderr(Stdio::null()).status();
    let _ = new.wait();
    let _ = Command::new("tmux").args(["-L", &tmux, "kill-server"]).status();
    let _ = std::fs::remove_dir_all(&dir);
}

unsafe fn libc_kill(pid: i32) {
    extern "C" {
        fn kill(pid: i32, sig: i32) -> i32;
    }
    kill(pid, 15); // SIGTERM
}
