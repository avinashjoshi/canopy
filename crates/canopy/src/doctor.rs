//! `canopy doctor`: is this machine ready, and is the install healthy? Pure reporting; it
//! changes nothing. Exit status 1 when something required is missing.

use anyhow::Result;
use canopy_core::paths::Paths;
use canopy_proto::{Method, ResultBody, Response};
use std::path::Path;
use std::process::Command;
use std::time::Duration;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Level {
    Ok,
    Warn,
    Fail,
}

pub struct Check {
    pub level: Level,
    pub what: &'static str,
    pub detail: String,
}

fn check(level: Level, what: &'static str, detail: impl Into<String>) -> Check {
    Check { level, what, detail: detail.into() }
}

fn version_of(bin: &str) -> Option<String> {
    let out = Command::new(bin).arg(if bin == "tmux" { "-V" } else { "--version" }).output().ok()?;
    if !out.status.success() {
        return None;
    }
    let text = String::from_utf8_lossy(&out.stdout);
    Some(text.lines().next().unwrap_or_default().trim().to_string())
}

/// `tmux 3.5a` → (3, 5).
pub fn tmux_major_minor(v: &str) -> Option<(u32, u32)> {
    let num = v.split_whitespace().last()?;
    let digits: String = num.chars().take_while(|c| c.is_ascii_digit() || *c == '.').collect();
    let mut it = digits.split('.');
    Some((it.next()?.parse().ok()?, it.next().unwrap_or("0").parse().unwrap_or(0)))
}

pub fn run(paths: &Paths) -> Result<Vec<Check>> {
    let mut out = Vec::new();

    // ---- this binary -----------------------------------------------------------------
    let exe = std::env::current_exe().ok().map(|p| p.canonicalize().unwrap_or(p));
    match &exe {
        Some(p) if crate::upgrade::is_dev_build(p) => out.push(check(Level::Ok, "canopy", format!("{} (development build at {})", crate::upgrade::CURRENT, p.display()))),
        Some(p) => out.push(check(Level::Ok, "canopy", format!("{} at {}", crate::upgrade::CURRENT, p.display()))),
        None => out.push(check(Level::Warn, "canopy", format!("{} (could not locate the binary)", crate::upgrade::CURRENT))),
    }
    if let Some(exe) = &exe {
        // Some `canopy` on PATH must resolve to this very file (symlinks included).
        let on_path = std::env::var_os("PATH").is_some_and(|p| std::env::split_paths(&p).any(|d| d.join("canopy").canonicalize().ok().as_deref() == Some(exe.as_path())));
        if !on_path {
            let dir = exe.parent().map(|d| d.display().to_string()).unwrap_or_default();
            out.push(check(Level::Warn, "PATH", format!("no `canopy` on PATH resolves to this binary; add {dir} to PATH (agent hooks and scripts call `canopy` by name)")));
        }
    }

    // ---- required tools --------------------------------------------------------------
    match version_of("tmux") {
        Some(v) => match tmux_major_minor(&v) {
            Some((maj, min)) if (maj, min) >= (3, 2) => out.push(check(Level::Ok, "tmux", v)),
            Some(_) => out.push(check(Level::Warn, "tmux", format!("{v}; 3.2 or newer recommended (popups, pane hooks)"))),
            None => out.push(check(Level::Ok, "tmux", v)),
        },
        None => out.push(check(Level::Fail, "tmux", "not found; install tmux (pacman -S tmux / apt install tmux / brew install tmux)")),
    }
    match version_of("git") {
        Some(v) => out.push(check(Level::Ok, "git", v)),
        None => out.push(check(Level::Fail, "git", "not found; canopy workspaces are git worktrees")),
    }

    // ---- optional tools --------------------------------------------------------------
    match version_of("gh") {
        Some(v) => out.push(check(Level::Ok, "gh", v)),
        None => out.push(check(Level::Warn, "gh", "not found; `canopy new --pr/--issue` and the PR badges need the GitHub CLI")),
    }
    let agents: Vec<&str> = ["claude", "codex", "opencode", "gemini", "aider"].into_iter().filter(|a| which(a)).collect();
    if agents.is_empty() {
        out.push(check(Level::Warn, "agents", "no agent CLI found (claude, codex, opencode, gemini, aider); workspaces will open with a shell in the agent pane"));
    } else {
        out.push(check(Level::Ok, "agents", agents.join(", ")));
    }
    let clip = if which("wl-copy") && which("wl-paste") {
        Some("wl-clipboard")
    } else if which("xclip") {
        Some("xclip")
    } else if which("pbcopy") {
        Some("pbcopy")
    } else {
        None
    };
    match clip {
        Some(c) => out.push(check(Level::Ok, "clipboard", c)),
        None => out.push(check(Level::Warn, "clipboard", "no wl-clipboard/xclip/pbcopy; copy from tmux and the remote clipboard bridge need one")),
    }
    if which("mosh") {
        out.push(check(Level::Ok, "mosh", "available for --remote attach"));
    }

    // ---- home + state ----------------------------------------------------------------
    match std::fs::create_dir_all(&paths.home) {
        Ok(()) => {
            let state = paths.state_file();
            let detail = match std::fs::read_to_string(&state).ok().and_then(|t| serde_json::from_str::<serde_json::Value>(&t).ok()) {
                Some(v) => {
                    let ws = v.get("workspaces").and_then(|w| w.as_array()).map(|a| a.len()).unwrap_or(0);
                    let pr = v.get("projects").and_then(|p| p.as_array().map(|a| a.len()).or_else(|| p.as_object().map(|o| o.len()))).unwrap_or(0);
                    format!("{} · {pr} project{} · {ws} workspace{}", paths.home.display(), if pr == 1 { "" } else { "s" }, if ws == 1 { "" } else { "s" })
                }
                None => format!("{} (no state yet; created on first use)", paths.home.display()),
            };
            out.push(check(Level::Ok, "home", detail));
        }
        Err(e) => out.push(check(Level::Fail, "home", format!("{} is not writable: {e}", paths.home.display()))),
    }
    let settings = paths.settings_file();
    if settings.is_file() {
        match canopy_core::settings::Settings::load(&settings) {
            Ok(_) => out.push(check(Level::Ok, "config", settings.display().to_string())),
            Err(e) => out.push(check(Level::Fail, "config", format!("{} does not parse: {e}", settings.display()))),
        }
    } else {
        out.push(check(Level::Ok, "config", format!("defaults ({} not present; `canopy default-config` prints a template)", settings.display())));
    }

    // ---- server ----------------------------------------------------------------------
    let socket = paths.socket();
    match canopy_client::call_raw(&socket, Method::Ping, Some(Duration::from_secs(3))) {
        Ok(Response::Ok { result: ResultBody::Pong { version, .. }, .. }) if version == crate::upgrade::CURRENT => out.push(check(Level::Ok, "server", format!("running {version} on {}", socket.display()))),
        Ok(Response::Ok { result: ResultBody::Pong { version, .. }, .. }) => out.push(check(Level::Warn, "server", format!("running {version} but this binary is {}; it restarts on the next command", crate::upgrade::CURRENT))),
        Ok(_) => out.push(check(Level::Warn, "server", "answered unexpectedly; run `canopy server stop`")),
        Err(_) => out.push(check(Level::Ok, "server", format!("not running (starts on the first command; socket {})", socket.display()))),
    }

    // ---- agent hook integration ------------------------------------------------------
    for integ in canopy_server::integration::INTEGRATIONS {
        if !which(integ.agent) {
            continue;
        }
        match (canopy_server::integration::status(integ.agent), canopy_server::integration::healthy(integ.agent)) {
            (Ok(true), Ok(true)) => out.push(check(Level::Ok, "hooks", format!("{} reports agent state to canopy", integ.agent))),
            (Ok(true), _) => out.push(check(Level::Warn, "hooks", format!("{} hooks point at a missing script; run `canopy integration install {}`", integ.agent, integ.agent))),
            (Ok(false), _) => out.push(check(Level::Warn, "hooks", format!("{} hooks not installed; the server installs them on start, or run `canopy integration install {}`", integ.agent, integ.agent))),
            (Err(e), _) => out.push(check(Level::Warn, "hooks", format!("{}: {e}", integ.agent))),
        }
    }

    Ok(out)
}

pub fn print(checks: &[Check]) -> bool {
    let mut ok = true;
    for c in checks {
        let (glyph, color) = match c.level {
            Level::Ok => ("✓", "\x1b[32m"),
            Level::Warn => ("!", "\x1b[33m"),
            Level::Fail => ("✗", "\x1b[31m"),
        };
        if c.level == Level::Fail {
            ok = false;
        }
        let reset = "\x1b[0m";
        let tty = std::io::IsTerminal::is_terminal(&std::io::stdout());
        if tty {
            println!("{color}{glyph}{reset} {:<10} {}", c.what, c.detail);
        } else {
            println!("{glyph} {:<10} {}", c.what, c.detail);
        }
    }
    let fails = checks.iter().filter(|c| c.level == Level::Fail).count();
    let warns = checks.iter().filter(|c| c.level == Level::Warn).count();
    println!();
    if fails > 0 {
        println!("{fails} problem{} to fix before canopy can run.", if fails == 1 { "" } else { "s" });
    } else if warns > 0 {
        println!("ready, with {warns} note{}.", if warns == 1 { "" } else { "s" });
    } else {
        println!("all good.");
    }
    ok
}

fn which(bin: &str) -> bool {
    std::env::var_os("PATH").is_some_and(|p| std::env::split_paths(&p).any(|d| is_exec(&d.join(bin))))
}

fn is_exec(p: &Path) -> bool {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::metadata(p).map(|m| m.is_file() && m.permissions().mode() & 0o111 != 0).unwrap_or(false)
    }
    #[cfg(not(unix))]
    {
        p.is_file()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tmux_versions_parse() {
        assert_eq!(tmux_major_minor("tmux 3.5a"), Some((3, 5)));
        assert_eq!(tmux_major_minor("tmux 3.7"), Some((3, 7)));
        assert_eq!(tmux_major_minor("tmux next-3.6"), None);
        assert_eq!(tmux_major_minor("tmux 2.9"), Some((2, 9)));
    }
}
