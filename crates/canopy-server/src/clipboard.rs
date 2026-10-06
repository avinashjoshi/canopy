//! This machine's clipboard, written to by remote clients that mirror their own.
//!
//! Wayland first (`wl-copy`, with `WAYLAND_DISPLAY` discovered from the runtime dir when the
//! server was started over ssh), then X11 (`xclip`), then macOS (`pbcopy`, text only).
//! System binaries are preferred over anything in `~/.local/bin`, where wrapper scripts
//! from older tooling may shadow them.

use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

#[derive(Debug, thiserror::Error)]
pub enum ClipboardError {
    #[error("no clipboard tool available on this machine (install wl-clipboard or xclip)")]
    NoTool,
    #[error("{0}")]
    Failed(String),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
}

fn system_bin(name: &str) -> Option<PathBuf> {
    ["/usr/bin", "/usr/local/bin", "/opt/homebrew/bin", "/bin"].iter().map(|d| Path::new(d).join(name)).find(|p| p.is_file())
}

/// `WAYLAND_DISPLAY`, or the first `wayland-N` socket in the runtime dir.
pub fn wayland_display() -> Option<String> {
    if let Ok(d) = std::env::var("WAYLAND_DISPLAY") {
        if !d.is_empty() {
            return Some(d);
        }
    }
    let run = std::env::var("XDG_RUNTIME_DIR").map(PathBuf::from).unwrap_or_else(|_| PathBuf::from(format!("/run/user/{}", unsafe { getuid() })));
    let mut names: Vec<String> = std::fs::read_dir(&run)
        .ok()?
        .flatten()
        .filter_map(|e| e.file_name().into_string().ok())
        .filter(|n| n.starts_with("wayland-") && !n.ends_with(".lock"))
        .collect();
    names.sort();
    names.into_iter().next()
}

extern "C" {
    fn getuid() -> u32;
}

/// `wl-copy` forks a background process that keeps serving the selection, and that child
/// inherits our pipes. Never wait for pipe EOF here: feed stdin, close it, wait for the
/// parent only, and leave stderr unconnected.
fn run_with_stdin(mut cmd: Command, data: &[u8]) -> Result<(), ClipboardError> {
    use std::io::Write;
    let mut child = cmd.stdin(Stdio::piped()).stdout(Stdio::null()).stderr(Stdio::null()).spawn()?;
    {
        let mut stdin = child.stdin.take().expect("piped");
        stdin.write_all(data)?;
    }
    let status = child.wait()?;
    if status.success() {
        Ok(())
    } else {
        Err(ClipboardError::Failed(format!("clipboard tool exited with {status}")))
    }
}

pub fn set(data: &[u8], mime: &str) -> Result<(), ClipboardError> {
    if let (Some(wl), Some(display)) = (system_bin("wl-copy"), wayland_display()) {
        let mut cmd = Command::new(wl);
        cmd.env("WAYLAND_DISPLAY", display).arg("--type").arg(mime);
        return run_with_stdin(cmd, data);
    }
    if let Some(xclip) = system_bin("xclip") {
        let mut cmd = Command::new(xclip);
        cmd.args(["-selection", "clipboard", "-t", mime]);
        return run_with_stdin(cmd, data);
    }
    if mime.starts_with("text/") {
        if let Some(pb) = system_bin("pbcopy") {
            return run_with_stdin(Command::new(pb), data);
        }
    }
    Err(ClipboardError::NoTool)
}

/// Text currently on this machine's clipboard (`None` when empty or not text).
pub fn get_text() -> Result<Option<String>, ClipboardError> {
    let out = if let (Some(wl), Some(display)) = (system_bin("wl-paste"), wayland_display()) {
        Command::new(wl).env("WAYLAND_DISPLAY", display).args(["-n", "-t", "text"]).stdin(Stdio::null()).stderr(Stdio::null()).output()?
    } else if let Some(xclip) = system_bin("xclip") {
        Command::new(xclip).args(["-selection", "clipboard", "-o"]).stdin(Stdio::null()).stderr(Stdio::null()).output()?
    } else if let Some(pb) = system_bin("pbpaste") {
        Command::new(pb).stdin(Stdio::null()).stderr(Stdio::null()).output()?
    } else {
        return Err(ClipboardError::NoTool);
    };
    if !out.status.success() || out.stdout.is_empty() {
        return Ok(None);
    }
    Ok(Some(String::from_utf8_lossy(&out.stdout).into_owned()))
}

/// Shell command tmux should pipe copies into so they land on this machine's clipboard
/// as well as going out over OSC 52 (`copy-command`). `None` when no tool is available.
pub fn tmux_copy_command() -> Option<String> {
    if let (Some(wl), Some(display)) = (system_bin("wl-copy"), wayland_display()) {
        return Some(format!("WAYLAND_DISPLAY={display} {}", wl.display()));
    }
    if let Some(xclip) = system_bin("xclip") {
        return Some(format!("{} -selection clipboard", xclip.display()));
    }
    system_bin("pbcopy").map(|p| p.display().to_string())
}

pub fn set_text(text: &str) -> Result<(), ClipboardError> {
    set(text.as_bytes(), "text/plain;charset=utf-8")
}

pub fn set_file(path: &Path, mime: &str) -> Result<(), ClipboardError> {
    let data = std::fs::read(path)?;
    let r = set(&data, mime);
    let _ = std::fs::remove_file(path);
    r
}

/// Agents call `wl-paste`/`wl-copy` by name from processes that may have no
/// `WAYLAND_DISPLAY` (sessions created over ssh, long-running panes). Tiny shims in
/// `~/.local/bin` fill the variable in from the runtime dir and exec the real tool, which
/// fixes already-running agents too since the lookup happens at call time. Written only
/// when the name is free there and the real tool exists; marked so we never clobber
/// anything else.
pub const SHIM_MARKER: &str = "canopy wayland shim";

pub fn ensure_shims() -> Vec<PathBuf> {
    let Some(home) = std::env::var_os("HOME") else { return Vec::new() };
    let bin = Path::new(&home).join(".local/bin");
    let mut written = Vec::new();
    for tool in ["wl-paste", "wl-copy"] {
        let Some(real) = system_bin(tool) else { continue };
        let shim = bin.join(tool);
        if shim.exists() {
            let ours = std::fs::read_to_string(&shim).map(|t| t.contains(SHIM_MARKER)).unwrap_or(false);
            if !ours {
                continue;
            }
        }
        let body = format!(
            concat!(
                "#!/bin/sh\n",
                "# {marker}: supplies XDG_RUNTIME_DIR / WAYLAND_DISPLAY when the caller has none, then runs the real tool.\n",
                "[ -n \"$XDG_RUNTIME_DIR\" ] || export XDG_RUNTIME_DIR=\"/run/user/$(id -u)\"\n",
                "if [ -z \"$WAYLAND_DISPLAY\" ]; then\n",
                "  d=$(ls \"$XDG_RUNTIME_DIR\" 2>/dev/null | grep -m1 '^wayland-[0-9]*$')\n",
                "  [ -n \"$d\" ] && export WAYLAND_DISPLAY=\"$d\"\n",
                "fi\n",
                "exec {real} \"$@\"\n"
            ),
            marker = SHIM_MARKER,
            real = real.display()
        );
        if std::fs::read_to_string(&shim).ok().as_deref() == Some(body.as_str()) {
            continue;
        }
        if std::fs::create_dir_all(&bin).is_err() || std::fs::write(&shim, &body).is_err() {
            continue;
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = std::fs::set_permissions(&shim, std::fs::Permissions::from_mode(0o755));
        }
        written.push(shim);
    }
    written
}
