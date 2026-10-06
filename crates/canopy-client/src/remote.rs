//! Remote hosts, zero setup: `canopy --remote <ssh-target>`.
//!
//! The laptop never talks to a remote socket directly. Each API call runs
//! `ssh -T <target> canopy bridge` and the far side pipes stdio to its local server socket.
//! OpenSSH ControlMaster keeps one connection open so a call costs tens of milliseconds,
//! not a handshake. Attach is a real `ssh -t` (or mosh) into the remote tmux session.
//!
//! `CANOPY_SSH` overrides the ssh program (tests point it at a shell wrapper).

use anyhow::{bail, Context, Result};
use canopy_proto::{Method, Request, Response, ResultBody, PROTOCOL_VERSION};
use std::io::{BufRead, BufReader, Read, Write};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::time::Duration;

/// Shell snippet run on the far side. Non-interactive ssh does not source profiles, so
/// the usual install locations are added to PATH first.
pub const REMOTE_PATH_PREFIX: &str = r#"export PATH="$HOME/.local/bin:$HOME/.cargo/bin:/usr/local/bin:$PATH";"#;

pub fn remote_cmd(verb: &str) -> String {
    format!("sh -c '{REMOTE_PATH_PREFIX} exec canopy {verb}'")
}

fn control_dir() -> PathBuf {
    let home = std::env::var_os("HOME").map(PathBuf::from).unwrap_or_else(|| PathBuf::from("/tmp"));
    home.join(".canopy").join("ssh")
}

/// ssh invocation with multiplexing and sane keepalives. `--` guards against
/// option-shaped targets (v0 security fix).
pub fn ssh_command(target: &str, tty: bool) -> Command {
    let prog = std::env::var("CANOPY_SSH").unwrap_or_else(|_| "ssh".into());
    let mut c = Command::new(prog);
    let dir = control_dir();
    let _ = std::fs::create_dir_all(&dir);
    c.arg("-o").arg("ControlMaster=auto");
    c.arg("-o").arg(format!("ControlPath={}/%C", dir.display()));
    c.arg("-o").arg("ControlPersist=600");
    c.arg("-o").arg("ServerAliveInterval=15");
    c.arg("-o").arg("ServerAliveCountMax=4");
    c.arg("-o").arg("ConnectTimeout=8");
    c.arg(if tty { "-t" } else { "-T" });
    c.arg("--").arg(target);
    c
}

pub fn validate_target(target: &str) -> Result<()> {
    if target.is_empty() || target.starts_with('-') {
        bail!("invalid ssh target {target:?}");
    }
    Ok(())
}

/// One API call over ssh. `quiet` sends ssh's stderr to /dev/null (TUI mode).
pub fn call_raw(target: &str, method: Method, quiet: bool) -> Result<Response> {
    validate_target(target)?;
    let mut child = ssh_command(target, false)
        .arg(remote_cmd("bridge"))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(if quiet { Stdio::null() } else { Stdio::inherit() })
        .spawn()
        .context("spawn ssh")?;
    let req = Request { id: "1".into(), method };
    let mut line = serde_json::to_string(&req)?;
    line.push('\n');
    child.stdin.take().expect("piped").write_all(line.as_bytes())?;
    let mut out = String::new();
    BufReader::new(child.stdout.take().expect("piped")).read_line(&mut out)?;
    let status = child.wait()?;
    if out.trim().is_empty() {
        bail!("no response from canopy on {target} (ssh exit {status}); is canopy installed there? try: canopy --remote {target} --install");
    }
    serde_json::from_str(&out).with_context(|| format!("bad response from {target}: {}", out.trim()))
}

/// Long-lived `events.subscribe` over ssh. Returns the ssh child (kill to stop) and a reader.
pub fn subscribe(target: &str) -> Result<(Child, BufReader<std::process::ChildStdout>)> {
    validate_target(target)?;
    let mut child = ssh_command(target, false)
        .arg(remote_cmd("bridge"))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()?;
    let req = Request { id: "ev".into(), method: Method::EventsSubscribe { since: None } };
    let mut line = serde_json::to_string(&req)?;
    line.push('\n');
    child.stdin.take().expect("piped").write_all(line.as_bytes())?;
    // Keep stdin open (dropped would send EOF and end the bridge): leak it on purpose.
    let stdout = child.stdout.take().expect("piped");
    Ok((child, BufReader::new(stdout)))
}

/// Make sure a compatible canopy answers on the far side. Installs our own binary when
/// the remote has none and the architecture matches.
pub fn ensure_remote(target: &str, allow_install: bool) -> Result<()> {
    validate_target(target)?;
    let probe = ssh_command(target, false)
        .arg(format!("sh -c '{REMOTE_PATH_PREFIX} uname -sm; command -v canopy >/dev/null 2>&1 && canopy version | head -1 || echo NO-CANOPY'"))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .output()
        .context("ssh probe")?;
    if !probe.status.success() {
        bail!("cannot reach {target} over ssh (exit {})", probe.status);
    }
    let text = String::from_utf8_lossy(&probe.stdout);
    let mut lines = text.lines();
    let remote_arch = lines.next().unwrap_or("").trim().to_string();
    let version_line = lines.next().unwrap_or("").trim().to_string();
    if version_line == "NO-CANOPY" {
        if !allow_install {
            bail!("canopy is not installed on {target} ({remote_arch}). Re-run with --install to copy this binary there, or install it manually.");
        }
        let local_arch = local_uname_sm()?;
        if local_arch != remote_arch {
            bail!("canopy missing on {target} and architectures differ (local {local_arch}, remote {remote_arch}); install it there manually");
        }
        install_remote(target)?;
        return Ok(());
    }
    // "canopy 1.0.0-alpha.0 (protocol 1)"
    let proto: Option<u32> = version_line.rsplit("(protocol ").next().and_then(|s| s.trim_end_matches(')').parse().ok());
    match proto {
        Some(p) if p == PROTOCOL_VERSION => Ok(()),
        Some(p) => bail!("{target} runs canopy protocol {p}, this client speaks {PROTOCOL_VERSION} ({version_line}); update one side"),
        None => bail!("{target} has an incompatible canopy: {version_line} (v0?). Install v1 there, or re-run with --install"),
    }
}

fn local_uname_sm() -> Result<String> {
    let out = Command::new("uname").arg("-sm").output()?;
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

/// scp our own executable to `~/.local/bin/canopy` on the far side.
pub fn install_remote(target: &str) -> Result<()> {
    let exe = std::env::current_exe()?;
    eprintln!("installing canopy on {target} from {}…", exe.display());
    let mkdir = ssh_command(target, false).arg("mkdir -p ~/.local/bin").stdin(Stdio::null()).status()?;
    if !mkdir.success() {
        bail!("mkdir on {target} failed");
    }
    let prog = std::env::var("CANOPY_SCP").unwrap_or_else(|_| "scp".into());
    let dir = control_dir();
    let st = Command::new(prog)
        .arg("-q")
        .arg("-o").arg("ControlMaster=auto")
        .arg("-o").arg(format!("ControlPath={}/%C", dir.display()))
        .arg("-o").arg("ControlPersist=600")
        .arg("--")
        .arg(&exe)
        .arg(format!("{target}:.local/bin/canopy.new"))
        .status()?;
    if !st.success() {
        bail!("scp to {target} failed");
    }
    let mv = ssh_command(target, false).arg("chmod +x ~/.local/bin/canopy.new && mv -f ~/.local/bin/canopy.new ~/.local/bin/canopy").stdin(Stdio::null()).status()?;
    if !mv.success() {
        bail!("activating canopy on {target} failed");
    }
    eprintln!("installed canopy on {target}");
    Ok(())
}

/// Server side of the bridge: pipe stdin/stdout to the local server socket.
/// Starts the server if needed. Prints nothing itself.
pub fn run_bridge(paths: &canopy_core::paths::Paths) -> Result<()> {
    canopy_server::daemon::ensure_running(paths)?;
    let sock = std::os::unix::net::UnixStream::connect(paths.socket())?;
    sock.set_read_timeout(None)?;
    let mut sock_in = sock.try_clone()?;
    let mut sock_out = sock;
    // stdin -> socket
    let t_in = std::thread::spawn(move || {
        let mut stdin = std::io::stdin().lock();
        let mut buf = [0u8; 8192];
        loop {
            match stdin.read(&mut buf) {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    if sock_in.write_all(&buf[..n]).is_err() {
                        break;
                    }
                }
            }
        }
        let _ = sock_in.shutdown(std::net::Shutdown::Write);
    });
    // socket -> stdout
    let mut stdout = std::io::stdout().lock();
    let mut buf = [0u8; 8192];
    loop {
        match sock_out.read(&mut buf) {
            Ok(0) | Err(_) => break,
            Ok(n) => {
                if stdout.write_all(&buf[..n]).is_err() {
                    break;
                }
                let _ = stdout.flush();
            }
        }
    }
    let _ = t_in.join();
    let _ = Duration::from_millis(0);
    Ok(())
}

// ---------------------------------------------------------------------------------------
// Attach with a resident clipboard mirror
// ---------------------------------------------------------------------------------------

/// Program and argv for attaching to a remote tmux session. mosh takes exactly one `--`
/// (before the target) and the remote command as separate words after it; a second `--`
/// would reach `mosh-server` as the command itself.
pub fn attach_argv(target: &str, session: &str, mosh: bool, detach_others: bool) -> (String, Vec<String>) {
    let mut tmux: Vec<String> = vec!["tmux".into(), "attach-session".into()];
    if detach_others {
        tmux.push("-d".into());
    }
    tmux.push("-t".into());
    tmux.push(format!("={session}"));
    if mosh {
        let mut args = vec!["--".to_string(), target.to_string()];
        args.extend(tmux);
        ("mosh".into(), args)
    } else {
        let cmd = ssh_command(target, true);
        let mut args: Vec<String> = cmd.get_args().map(|a| a.to_string_lossy().into_owned()).collect();
        args.push(tmux.join(" "));
        (cmd.get_program().to_string_lossy().into_owned(), args)
    }
}

/// Attach (mosh or ssh) as a child process while this process stays resident and mirrors
/// the local clipboard to the remote machine: text goes straight to its clipboard, images
/// are uploaded and placed there too, so `Ctrl+V` in a remote agent pastes a local
/// screenshot. Returns when the attach ends.
pub fn attach_supervised(target: &str, session: &str, mosh: bool, detach_others: bool) -> Result<()> {
    validate_target(target)?;
    let (prog, args) = attach_argv(target, session, mosh, detach_others);
    let mut child = Command::new(&prog).args(&args).spawn().with_context(|| format!("spawn {prog}"))?;
    let stop = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let watcher = {
        let stop = stop.clone();
        let target = target.to_string();
        let session = session.to_string();
        std::thread::spawn(move || clipboard_mirror(&target, &session, stop))
    };
    let status = child.wait()?;
    stop.store(true, std::sync::atomic::Ordering::Relaxed);
    let _ = watcher.join();
    if !status.success() {
        if mosh {
            eprintln!("canopy: mosh attach ended with {status}; if mosh is the problem, use --ssh");
        } else {
            eprintln!("canopy: ssh attach ended with {status}");
        }
    }
    Ok(())
}

/// Local clipboard snapshot: what is on it and a cheap fingerprint.
enum Clip {
    Text(String),
    Png(Vec<u8>),
    Empty,
}

fn fingerprint(c: &Clip) -> u64 {
    use std::hash::{Hash, Hasher};
    let mut h = std::collections::hash_map::DefaultHasher::new();
    match c {
        Clip::Text(t) => {
            0u8.hash(&mut h);
            t.hash(&mut h);
        }
        Clip::Png(b) => {
            1u8.hash(&mut h);
            b.len().hash(&mut h);
            b.iter().step_by((b.len() / 4096).max(1)).for_each(|x| x.hash(&mut h));
        }
        Clip::Empty => 2u8.hash(&mut h),
    }
    h.finish()
}

fn sh(cmd: &str) -> Option<Vec<u8>> {
    let out = Command::new("sh").arg("-c").arg(cmd).stdin(Stdio::null()).stderr(Stdio::null()).output().ok()?;
    out.status.success().then_some(out.stdout)
}

/// macOS: NSPasteboard change counter, so we only read the clipboard when it changed.
fn mac_change_count() -> Option<u64> {
    let out = sh("osascript -l JavaScript -e 'ObjC.import(\"AppKit\"); $.NSPasteboard.generalPasteboard.changeCount' 2>/dev/null")?;
    String::from_utf8_lossy(&out).trim().parse().ok()
}

fn read_local_clipboard(os: &str) -> Clip {
    match os {
        "Darwin" => {
            let info = sh("osascript -e 'clipboard info' 2>/dev/null").map(|b| String::from_utf8_lossy(&b).into_owned()).unwrap_or_default();
            if info.contains("PNGf") || info.contains("TIFF") {
                let tmp = std::env::temp_dir().join(format!("canopy-clip-{}.png", std::process::id()));
                let script = format!(
                    "osascript -e 'set f to open for access POSIX file \"{}\" with write permission' -e 'set eof f to 0' -e 'write (the clipboard as «class PNGf») to f' -e 'close access f' 2>/dev/null",
                    tmp.display()
                );
                if sh(&script).is_some() {
                    if let Ok(bytes) = std::fs::read(&tmp) {
                        let _ = std::fs::remove_file(&tmp);
                        if !bytes.is_empty() {
                            return Clip::Png(bytes);
                        }
                    }
                }
            }
            match sh("pbpaste") {
                Some(b) if !b.is_empty() => Clip::Text(String::from_utf8_lossy(&b).into_owned()),
                _ => Clip::Empty,
            }
        }
        _ => {
            // Linux: Wayland first, then X11.
            let (list, get_png, get_text) = if sh("command -v wl-paste").is_some() {
                ("wl-paste --list-types 2>/dev/null", "wl-paste -t image/png 2>/dev/null", "wl-paste -n -t text 2>/dev/null")
            } else if sh("command -v xclip").is_some() {
                ("xclip -selection clipboard -t TARGETS -o 2>/dev/null", "xclip -selection clipboard -t image/png -o 2>/dev/null", "xclip -selection clipboard -o 2>/dev/null")
            } else {
                return Clip::Empty;
            };
            let types = sh(list).map(|b| String::from_utf8_lossy(&b).into_owned()).unwrap_or_default();
            if types.contains("image/png") {
                if let Some(b) = sh(get_png) {
                    if !b.is_empty() {
                        return Clip::Png(b);
                    }
                }
            }
            match sh(get_text) {
                Some(b) if !b.is_empty() => Clip::Text(String::from_utf8_lossy(&b).into_owned()),
                _ => Clip::Empty,
            }
        }
    }
}

fn local_host() -> String {
    sh("hostname -s 2>/dev/null || hostname").map(|b| String::from_utf8_lossy(&b).trim().to_string()).filter(|s| !s.is_empty()).unwrap_or_else(|| "local".into())
}

fn short_host(target: &str) -> String {
    let host = target.rsplit('@').next().unwrap_or(target);
    host.split('.').next().unwrap_or(host).to_string()
}

/// Put text on the local clipboard.
fn write_local_text(os: &str, text: &str) -> Result<()> {
    let cmd = match os {
        "Darwin" => "pbcopy",
        _ if sh("command -v wl-copy").is_some() => "wl-copy",
        _ if sh("command -v xclip").is_some() => "xclip -selection clipboard",
        _ => bail!("no local clipboard tool"),
    };
    use std::io::Write;
    let mut child = Command::new("sh").arg("-c").arg(cmd).stdin(Stdio::piped()).stdout(Stdio::null()).stderr(Stdio::null()).spawn()?;
    child.stdin.take().expect("piped").write_all(text.as_bytes())?;
    let _ = child.wait();
    Ok(())
}

/// Mirror clipboards both ways while attached: local changes (text, PNG) go to the remote
/// machine; text copied on the remote (tmux `copy-command`, agents) comes back here.
fn notify(target: &str, session: &str, text: &str) {
    let _ = call_raw(target, Method::SessionNotify { session: session.to_string(), text: text.to_string() }, true);
}

fn clipboard_mirror(target: &str, session: &str, stop: std::sync::Arc<std::sync::atomic::AtomicBool>) {
    let os = sh("uname -s").map(|b| String::from_utf8_lossy(&b).trim().to_string()).unwrap_or_default();
    let mut last_count = if os == "Darwin" { mac_change_count() } else { None };
    // Whatever is on either clipboard when we start is not "new": don't push it.
    let mut last_fp = fingerprint(&read_local_clipboard(&os));
    let mut last_remote: Option<String> = match call_raw(target, Method::ClipboardGet, true) {
        Ok(Response::Ok { result: ResultBody::Clipboard { text }, .. }) => text,
        _ => None,
    };
    let mut tick: u32 = 0;
    while !stop.load(std::sync::atomic::Ordering::Relaxed) {
        std::thread::sleep(Duration::from_millis(700));
        tick = tick.wrapping_add(1);

        // ---- local -> remote ----
        let changed = if os == "Darwin" {
            let c = mac_change_count();
            let ch = c != last_count;
            last_count = c;
            ch
        } else {
            true
        };
        if changed {
            let clip = read_local_clipboard(&os);
            let fp = fingerprint(&clip);
            if fp != last_fp {
                last_fp = fp;
                let result = match &clip {
                    Clip::Text(t) if t.len() <= 512 * 1024 => {
                        last_remote = Some(t.clone());
                        call_raw(target, Method::ClipboardSetText { text: t.clone() }, true).map(|_| ())
                    }
                    Clip::Png(bytes) => {
                        let r = push_image(target, bytes);
                        if r.is_ok() {
                            notify(target, session, &format!("⟶ screenshot on {}  ·  Ctrl+V to paste", short_host(target)));
                        }
                        r
                    }
                    _ => Ok(()),
                };
                if let Err(e) = result {
                    eprintln!("\r\ncanopy: clipboard mirror: {e}\r");
                }
                continue;
            }
        }

        // ---- remote -> local (text), every other tick ----
        if tick % 2 == 0 {
            if let Ok(Response::Ok { result: ResultBody::Clipboard { text: Some(t) }, .. }) = call_raw(target, Method::ClipboardGet, true) {
                if Some(&t) != last_remote.as_ref() {
                    last_remote = Some(t.clone());
                    if write_local_text(&os, &t).is_ok() {
                        // Our own write: remember it so it is not pushed back.
                        let n = t.chars().count();
                        last_fp = fingerprint(&Clip::Text(t));
                        if os == "Darwin" {
                            last_count = mac_change_count();
                        }
                        notify(target, session, &format!("⟵ copied to {} clipboard ({n} chars)", local_host()));
                    }
                }
            }
        }
    }
}

/// Upload a PNG to the remote temp dir and place it on the remote clipboard.
fn push_image(target: &str, bytes: &[u8]) -> Result<()> {
    let name = format!("clip-{}-{}.png", std::process::id(), std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_millis()).unwrap_or(0));
    let local = std::env::temp_dir().join(&name);
    std::fs::write(&local, bytes)?;
    let remote_rel = format!(".canopy/tmp/{name}");
    let mkdir = ssh_command(target, false).arg("mkdir -p ~/.canopy/tmp").stdin(Stdio::null()).stdout(Stdio::null()).stderr(Stdio::null()).status()?;
    if !mkdir.success() {
        bail!("mkdir on {target} failed");
    }
    let prog = std::env::var("CANOPY_SCP").unwrap_or_else(|_| "scp".into());
    let dir = control_dir();
    let st = Command::new(prog)
        .arg("-q")
        .arg("-o").arg("ControlMaster=auto")
        .arg("-o").arg(format!("ControlPath={}/%C", dir.display()))
        .arg("-o").arg("ControlPersist=600")
        .arg("--")
        .arg(&local)
        .arg(format!("{target}:{remote_rel}"))
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()?;
    let _ = std::fs::remove_file(&local);
    if !st.success() {
        bail!("scp to {target} failed");
    }
    // The server resolves `~` itself: pass the path relative to the remote home.
    let home = ssh_command(target, false).arg("printf %s \"$HOME\"").stdin(Stdio::null()).stderr(Stdio::null()).output()?;
    let home = String::from_utf8_lossy(&home.stdout).trim().to_string();
    let remote_abs = PathBuf::from(format!("{home}/{remote_rel}"));
    call_raw(target, Method::ClipboardSetFile { path: remote_abs, mime: "image/png".into() }, true)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mosh_has_single_dash_dash_and_plain_words() {
        let (prog, args) = attach_argv("tower", "cravd/x", true, true);
        assert_eq!(prog, "mosh");
        assert_eq!(args, vec!["--", "tower", "tmux", "attach-session", "-d", "-t", "=cravd/x"]);
        assert_eq!(args.iter().filter(|a| *a == "--").count(), 1);
    }

    #[test]
    fn ssh_attach_uses_tty_and_joined_command() {
        let (prog, args) = attach_argv("tower", "cravd/x", false, false);
        assert!(prog.ends_with("ssh"));
        assert!(args.contains(&"-t".to_string()));
        assert_eq!(args.last().unwrap(), "tmux attach-session -t =cravd/x");
    }
}
