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
