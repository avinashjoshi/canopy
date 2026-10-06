//! tmux implementation of `SessionBackend`.
//!
//! Conventions carried over from canopy v0:
//! - one tmux session per workspace, named `<project>/<branch>`;
//! - panes are addressed by the `@canopy-role` user option, never by index;
//! - pane ids returned by `-P -F '#{pane_id}'` are validated against `%<digits>` so a user
//!   hook that prints to stdout fails at the boundary instead of poisoning state;
//! - long-lived pane commands are wrapped as `<cmd>; exec "$SHELL"` so a crashed agent or
//!   `:q` in the editor leaves a shell instead of closing the pane.

use super::{BackendError, PaneId, PaneInfo, PaneSpec, SessionBackend, SessionInfo, WindowInfo};
use std::process::Command;

#[derive(Debug, Clone, Default)]
pub struct Tmux {
    /// `-L <name>` socket, used by tests to stay off the user's tmux server.
    pub socket_name: Option<String>,
}

impl Tmux {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn with_socket(name: impl Into<String>) -> Self {
        Self { socket_name: Some(name.into()) }
    }

    fn cmd(&self) -> Command {
        let mut c = Command::new("tmux");
        if let Some(s) = &self.socket_name {
            c.arg("-L").arg(s);
        }
        c
    }

    fn run(&self, args: &[&str]) -> Result<String, BackendError> {
        let out = self.cmd().args(args).output()?;
        if out.status.success() {
            Ok(String::from_utf8_lossy(&out.stdout).into_owned())
        } else {
            Err(BackendError::Command(format!(
                "tmux {}: {}",
                args.join(" "),
                String::from_utf8_lossy(&out.stderr).trim()
            )))
        }
    }

    /// tmux accepts `=name` for some commands but not others (set-option, set-hook,
    /// split-window reject it on 3.7), so resolve the session to its `$id` once.
    fn session_target(&self, session: &str) -> Result<String, BackendError> {
        let out = self.run(&["list-sessions", "-F", "#{session_id}\t#{session_name}"])?;
        out.lines()
            .find_map(|l| l.split_once('\t').filter(|(_, n)| *n == session).map(|(id, _)| id.to_string()))
            .ok_or_else(|| BackendError::SessionNotFound(session.to_string()))
    }

    /// Window `@id` for a window index in a session.
    fn window_target(&self, session: &str, index: u32) -> Result<String, BackendError> {
        self.windows(session)?
            .into_iter()
            .find(|w| w.index == index)
            .map(|w| w.id)
            .ok_or_else(|| BackendError::Command(format!("no window {index} in {session}")))
    }

    pub fn installed() -> bool {
        Command::new("tmux").arg("-V").output().map(|o| o.status.success()).unwrap_or(false)
    }

    /// Validate a `#{pane_id}` capture: must be `%` followed by digits only.
    fn parse_pane_id(raw: &str) -> Result<PaneId, BackendError> {
        let s = raw.trim();
        let ok = s.len() >= 2 && s.starts_with('%') && s[1..].chars().all(|c| c.is_ascii_digit());
        if ok {
            Ok(PaneId(s.to_string()))
        } else {
            Err(BackendError::Command(format!("unexpected pane id from tmux: {raw:?}")))
        }
    }

    fn set_role(&self, pane: &PaneId, role: &str) -> Result<(), BackendError> {
        self.run(&["set-option", "-p", "-t", &pane.0, "@canopy-role", role])?;
        Ok(())
    }

    /// Key token of a `bind-key [-r] -T <table> <key> …` line from `list-keys`.
    fn bound_key(line: &str) -> Option<&str> {
        let mut it = line.split_whitespace();
        if it.next()? != "bind-key" {
            return None;
        }
        loop {
            let tok = it.next()?;
            if tok == "-T" {
                it.next()?; // table
                return it.next();
            }
            if !tok.starts_with('-') {
                return Some(tok);
            }
        }
    }

    fn sh_quote(s: &str) -> String {
        if !s.is_empty() && s.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '/')) {
            s.to_string()
        } else {
            format!("'{}'", s.replace('\'', "'\\''"))
        }
    }

    fn pane_command(spec: &PaneSpec<'_>) -> String {
        if spec.keep_alive { Self::keep_alive(spec.command) } else { spec.command.to_string() }
    }

    /// Shell command for a pane: keep a shell alive when the program exits.
    pub fn keep_alive(command: &str) -> String {
        if command.trim().is_empty() {
            return String::new();
        }
        format!("{command}; exec \"${{SHELL:-/bin/sh}}\"")
    }

    fn env_args(env: &std::collections::BTreeMap<String, String>) -> Vec<String> {
        let mut v = Vec::with_capacity(env.len() * 2);
        for (k, val) in env {
            v.push("-e".into());
            v.push(format!("{k}={val}"));
        }
        v
    }

    fn pane_by_role(&self, session: &str, role: &str) -> Result<Option<PaneId>, BackendError> {
        Ok(self.panes(session)?.into_iter().find(|p| p.role == role).map(|p| p.id))
    }
}

impl SessionBackend for Tmux {
    fn name(&self) -> &'static str {
        "tmux"
    }

    fn snapshot(&self) -> Result<Vec<SessionInfo>, BackendError> {
        let out = self.cmd()
            .args(["list-panes", "-a", "-F", "#{session_name}\t#{session_attached}\t#{pane_id}\t#{window_index}\t#{?#{&&:#{pane_active},#{window_active}},1,0}\t#{pane_width}\t#{@canopy-role}\t#{pane_pid}\t#{pane_current_command}"])
            .output()?;
        if !out.status.success() {
            // No server running: no sessions.
            return Ok(Vec::new());
        }
        let text = String::from_utf8_lossy(&out.stdout);
        let mut sessions: Vec<SessionInfo> = Vec::new();
        for line in text.lines() {
            let mut p = line.split('\t');
            let (Some(name), Some(attached), Some(id)) = (p.next(), p.next(), p.next()) else { continue };
            let Ok(id) = Self::parse_pane_id(id) else { continue };
            let window = p.next().and_then(|w| w.trim().parse().ok()).unwrap_or(0);
            let active = p.next().unwrap_or("0").trim() == "1";
            let width = p.next().and_then(|w| w.trim().parse().ok()).unwrap_or(0);
            let role = p.next().unwrap_or_default().to_string();
            let pid = p.next().and_then(|x| x.trim().parse().ok());
            let current_command = p.next().unwrap_or_default().to_string();
            let info = PaneInfo { id, window, active, width, role, pid, current_command };
            match sessions.iter_mut().find(|s| s.name == name) {
                Some(s) => s.panes.push(info),
                None => sessions.push(SessionInfo { name: name.to_string(), attached: attached.trim().parse::<u32>().unwrap_or(0) > 0, panes: vec![info] }),
            }
        }
        Ok(sessions)
    }

    fn session_exists(&self, session: &str) -> Result<bool, BackendError> {
        let out = self.cmd().args(["has-session", "-t", &format!("={session}")]).output()?;
        Ok(out.status.success())
    }

    fn create_session(&self, session: &str, window: &str, first: PaneSpec<'_>) -> Result<PaneId, BackendError> {
        if self.session_exists(session)? {
            return Err(BackendError::SessionExists(session.to_string()));
        }
        let cwd = first.cwd.display().to_string();
        let mut args: Vec<String> = vec!["new-session".into(), "-d".into(), "-s".into(), session.into(), "-n".into(), window.into(), "-c".into(), cwd];
        args.extend(Self::env_args(first.env));
        args.extend(["-P".into(), "-F".into(), "#{pane_id}".into()]);
        let cmd = Self::pane_command(&first);
        if !cmd.is_empty() {
            args.push(cmd);
        }
        let refs: Vec<&str> = args.iter().map(String::as_str).collect();
        let out = self.run(&refs)?;
        let id = Self::parse_pane_id(&out)?;
        // Keep the first tab's name stable instead of following the active command.
        if let Some(w) = self.windows(session).ok().and_then(|ws| ws.into_iter().next()) {
            let _ = self.run(&["set-option", "-w", "-t", &w.id, "automatic-rename", "off"]);
        }
        self.set_role(&id, first.role)?;
        Ok(id)
    }

    fn add_pane(&self, session: &str, spec: PaneSpec<'_>) -> Result<PaneId, BackendError> {
        let target = match (spec.split_of, spec.window) {
            (Some(role), _) => self
                .pane_by_role(session, role)?
                .ok_or_else(|| BackendError::PaneNotFound(role.to_string()))?
                .0,
            (None, Some(w)) => self.window_target(session, w)?,
            (None, None) => self.session_target(session)?,
        };
        let (flag, before) = match spec.split {
            Some("right") => ("-h", false),
            Some("left") => ("-h", true),
            Some("above") => ("-v", true),
            _ => ("-v", false),
        };
        let size = spec.size_cells.map(|c| c.to_string()).or_else(|| spec.size_percent.map(|p| format!("{p}%")));
        let cwd = spec.cwd.display().to_string();
        let mut args: Vec<String> = vec!["split-window".into(), "-d".into(), "-t".into(), target, flag.into(), "-c".into(), cwd];
        if before {
            args.push("-b".into());
        }
        if spec.full_span {
            args.push("-f".into());
        }
        if let Some(s) = &size {
            args.push("-l".into());
            args.push(s.clone());
        }
        // Env on split-window is per-pane (tmux >= 3.2); session-level env was set at creation.
        args.extend(Self::env_args(spec.env));
        args.extend(["-P".into(), "-F".into(), "#{pane_id}".into()]);
        let cmd = Self::pane_command(&spec);
        if !cmd.is_empty() {
            args.push(cmd);
        }
        let refs: Vec<&str> = args.iter().map(String::as_str).collect();
        let out = self.run(&refs)?;
        let id = Self::parse_pane_id(&out)?;
        self.set_role(&id, spec.role)?;
        Ok(id)
    }

    fn open_window(&self, session: &str, name: &str, spec: PaneSpec<'_>) -> Result<PaneId, BackendError> {
        let cwd = spec.cwd.display().to_string();
        let target = self.session_target(session)?;
        let mut args: Vec<String> = vec!["new-window".into(), "-d".into(), "-t".into(), target, "-n".into(), name.into(), "-c".into(), cwd];
        args.extend(Self::env_args(spec.env));
        args.extend(["-P".into(), "-F".into(), "#{pane_id}".into()]);
        let cmd = Self::pane_command(&spec);
        if !cmd.is_empty() {
            args.push(cmd);
        }
        let refs: Vec<&str> = args.iter().map(String::as_str).collect();
        let out = self.run(&refs)?;
        let id = Self::parse_pane_id(&out)?;
        self.set_role(&id, spec.role)?;
        Ok(id)
    }

    fn ensure_server_config(&self, bin: &str) -> Result<(), BackendError> {
        let popup = format!("CANOPY_IN_POPUP=1 {}", Self::sh_quote(bin));
        let toggle = format!("{} sidebar toggle --session '#{{session_name}}'", Self::sh_quote(bin));
        // (key table, key, tmux command argv). Bound only if the key is free or already ours.
        // The sidebar replaced the old no-prefix Ctrl+Alt+c switcher; an old binding of ours
        // on that key is removed so nothing global is left behind.
        let binds: [(&str, &str, Vec<&str>); 2] = [
            ("prefix", "b", vec!["run-shell", &toggle]),
            ("prefix", "g", vec!["display-popup", "-E", "-w", "80%", "-h", "80%", "-d", "#{pane_current_path}", &popup]),
        ];
        if let Ok(root_keys) = self.run(&["list-keys", "-T", "root"]) {
            if root_keys.lines().any(|l| Self::bound_key(l) == Some("C-M-c") && l.contains("canopy")) {
                let _ = self.run(&["unbind-key", "-T", "root", "C-M-c"]);
            }
        }
        for (table, key, cmd) in binds {
            // `list-keys -T <table> <key>` prints nothing on tmux 3.7; list the table and match.
            let listing = self.run(&["list-keys", "-T", table]).unwrap_or_default();
            let existing = listing.lines().find(|l| Self::bound_key(l) == Some(key)).unwrap_or("");
            if existing.trim().is_empty() || existing.contains("canopy") {
                let mut args = vec!["bind-key", "-T", table, key];
                args.extend(cmd);
                if let Err(e) = self.run(&args) {
                    tracing::debug!(key, error = %e, "bind failed");
                }
            }
        }
        // Copies made in tmux must land on this machine's clipboard (besides OSC 52) so a
        // remote client can mirror them. Three things make that true:
        //  1. the tmux server knows the Wayland display (it often does not: started by a
        //     terminal that predates the session, or over ssh), so `wl-copy` run from any
        //     binding can find it;
        //  2. `copy-command` points at the clipboard tool, used by copy-pipe without args;
        //  3. the default copy keys (which only fill tmux's buffer) become copy-pipe variants.
        // Each step is skipped when the user has customized it.
        if let Some(display) = crate::clipboard::wayland_display() {
            let have = self.run(&["show-environment", "-g", "WAYLAND_DISPLAY"]).unwrap_or_default();
            if !have.starts_with("WAYLAND_DISPLAY=") {
                let _ = self.run(&["set-environment", "-g", "WAYLAND_DISPLAY", &display]);
            }
        }
        // A persistent tmux server outlives whatever ssh login started it; the login's
        // SSH_CLIENT/SSH_CONNECTION/SSH_TTY describe nothing about the panes and make agents
        // believe they are remote (Claude Code then refuses to read the clipboard). Drop
        // them from the global environment; SSH_AUTH_SOCK stays.
        for var in ["SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY"] {
            let _ = self.run(&["set-environment", "-g", "-r", var]);
        }
        let current = self.run(&["show-options", "-sv", "copy-command"]).unwrap_or_default();
        if current.trim().is_empty() {
            if let Some(cmd) = crate::clipboard::tmux_copy_command() {
                let _ = self.run(&["set-option", "-s", "copy-command", &cmd]);
            }
        }
        for table in ["copy-mode", "copy-mode-vi"] {
            let listing = self.run(&["list-keys", "-T", table]).unwrap_or_default();
            for line in listing.lines() {
                let Some(key) = Self::bound_key(line) else { continue };
                // Rewrite the plain built-ins, and bindings that pipe straight to a clipboard
                // tool by bare name (older canopy wrote those; they break without the display in
                // the session environment). Anything else is the user's and stays.
                let target = if line.ends_with(" send-keys -X copy-selection-and-cancel") || line.ends_with(" send-keys -X copy-pipe-and-cancel wl-copy") || line.ends_with(" send-keys -X copy-pipe-and-cancel xclip") {
                    "copy-pipe-and-cancel"
                } else if line.ends_with(" send-keys -X copy-selection") || line.ends_with(" send-keys -X copy-pipe wl-copy") {
                    "copy-pipe"
                } else if line.ends_with(" send-keys -X copy-selection-no-clear") {
                    "copy-pipe-no-clear"
                } else {
                    continue;
                };
                let _ = self.run(&["bind-key", "-T", table, key, "send-keys", "-X", target]);
            }
        }
        // Tab styling: only when the user has not customized these.
        let defaults: [(&str, &str, &str); 4] = [
            ("window-status-format", "#I:#W#{?window_flags,#{window_flags}, }", " #I #W "),
            ("window-status-current-format", "#I:#W#{?window_flags,#{window_flags}, }", "#[bold,reverse] #I #W #[default]"),
            ("window-status-separator", " ", ""),
            ("allow-passthrough", "off", "on"),
        ];
        for (opt, default, ours) in defaults {
            let cur = self.run(&["show-options", "-gwv", opt]).unwrap_or_default();
            if cur.trim() == default || cur.trim().is_empty() {
                let _ = self.run(&["set-option", "-gw", opt, ours]);
            }
        }
        Ok(())
    }

    fn decorate_session(&self, session: &str, statusline_cmd: &str) -> Result<(), BackendError> {
        let t = self.session_target(session)?;
        for (opt, val) in [("status-position", "top"), ("status-justify", "left"), ("renumber-windows", "on"), ("set-titles", "on"), ("set-titles-string", "#S"), ("status-left-length", "40")] {
            let _ = self.run(&["set-option", "-t", &t, opt, val]);
        }
        // Sessions snapshot the global environment at creation: give existing ones the
        // Wayland display too, so clipboard tools run from their bindings can connect.
        if let Some(display) = crate::clipboard::wayland_display() {
            let _ = self.run(&["set-environment", "-t", &t, "WAYLAND_DISPLAY", &display]);
        }
        for var in ["SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY"] {
            // `-u` removes it from the session environment; `-r` only marks it for new sessions.
            let _ = self.run(&["set-environment", "-t", &t, "-u", var]);
        }
        // New windows (prefix+c included) get a sidebar: a session-scoped hook asks canopy.
        if let Some((bin, _)) = statusline_cmd.split_once(" statusline") {
            let hook = format!("run-shell -b \"{bin} sidebar ensure --session '#{{session_name}}'\"");
            let _ = self.run(&["set-hook", "-t", &t, "after-new-window", &hook]);
        }
        // Focus guard, inside tmux so nothing is drawn in between:
        //  - entering an unmarked sidebar pane bounces back (only canopy may focus it);
        //  - leaving a focused sidebar (window option @canopy-lock=1) to an unmarked pane bounces
        //    back (the sidebar is modal: leave with Enter, q or prefix+b).
        // The sidebar process clears its own @canopy-focus marker once it sees it was entered
        // legitimately (clearing here would hit the wrong pane for cross-session selects).
        let entering = "#{&&:#{==:#{@canopy-role},sidebar},#{!=:#{@canopy-focus},1}}";
        let leaving = "#{&&:#{!=:#{@canopy-role},sidebar},#{&&:#{==:#{@canopy-lock},1},#{!=:#{@canopy-focus},1}}}";
        let guard = format!("if -F \"#{{||:{entering},{leaving}}}\" \"select-pane -l\"");
        let _ = self.run(&["set-hook", "-t", &t, "after-select-pane", &guard]);
        let _ = self.run(&["set-hook", "-t", &t, "after-last-pane", &guard]);
        // Append our segment to the user's own status-right (global value), per session only.
        let global = self.run(&["show-options", "-gv", "status-right"]).unwrap_or_default();
        let base = global.trim().to_string();
        let segment = format!("#({statusline_cmd})");
        let right = if base.contains("canopy") { base } else if base.is_empty() { format!(" {segment} ") } else { format!("{base} {segment} ") };
        self.run(&["set-option", "-t", &t, "status-right", &right])?;
        Ok(())
    }

    fn windows(&self, session: &str) -> Result<Vec<WindowInfo>, BackendError> {
        if !self.session_exists(session)? {
            return Err(BackendError::SessionNotFound(session.to_string()));
        }
        let out = self.run(&["list-windows", "-t", &format!("={session}"), "-F", "#{window_id}\t#{window_index}\t#{window_name}\t#{window_active}\t#{window_panes}"])?;
        Ok(out
            .lines()
            .filter_map(|l| {
                let mut p = l.split('\t');
                Some(WindowInfo {
                    id: p.next()?.trim().to_string(),
                    index: p.next()?.trim().parse().ok()?,
                    name: p.next().unwrap_or_default().to_string(),
                    active: p.next().unwrap_or("0").trim() == "1",
                    panes: p.next().and_then(|x| x.trim().parse().ok()).unwrap_or(1),
                })
            })
            .collect())
    }

    fn select_window(&self, session: &str, index: u32) -> Result<(), BackendError> {
        let target = self.window_target(session, index)?;
        self.run(&["select-window", "-t", &target])?;
        Ok(())
    }

    fn kill_pane(&self, pane: &PaneId) -> Result<(), BackendError> {
        self.run(&["kill-pane", "-t", &pane.0])?;
        Ok(())
    }

    fn select_last_pane(&self, session: &str, window: u32) -> Result<(), BackendError> {
        let target = self.window_target(session, window)?;
        self.run(&["select-pane", "-l", "-t", &target])?;
        Ok(())
    }

    fn display_message(&self, session: &str, text: &str) -> Result<(), BackendError> {
        let target = self.session_target(session)?;
        // Escape `#` so tmux does not expand it as a format.
        let safe = text.replace('#', "##");
        self.run(&["display-message", "-d", "1500", "-t", &target, &safe])?;
        Ok(())
    }

    fn unlock_window(&self, session: &str, window: u32) -> Result<(), BackendError> {
        let target = self.window_target(session, window)?;
        let _ = self.run(&["set-option", "-w", "-t", &target, "-u", "@canopy-lock"]);
        Ok(())
    }

    fn mark_focus(&self, pane: &PaneId) -> Result<(), BackendError> {
        self.run(&["set-option", "-p", "-t", &pane.0, "@canopy-focus", "1"])?;
        Ok(())
    }

    fn resize_pane(&self, pane: &PaneId, width: u16) -> Result<(), BackendError> {
        self.run(&["resize-pane", "-t", &pane.0, "-x", &width.to_string()])?;
        Ok(())
    }

    fn select_pane(&self, _session: &str, pane: &PaneId) -> Result<(), BackendError> {
        self.run(&["select-pane", "-t", &pane.0])?;
        Ok(())
    }

    fn kill_session(&self, session: &str) -> Result<(), BackendError> {
        if !self.session_exists(session)? {
            return Err(BackendError::SessionNotFound(session.to_string()));
        }
        let target = self.session_target(session)?;
        self.run(&["kill-session", "-t", &target])?;
        Ok(())
    }

    fn rename_session(&self, from: &str, to: &str) -> Result<(), BackendError> {
        if self.session_exists(to)? {
            return Err(BackendError::SessionExists(to.to_string()));
        }
        let target = self.session_target(from)?;
        self.run(&["rename-session", "-t", &target, to])?;
        Ok(())
    }

    fn attached_clients(&self, session: &str) -> Result<usize, BackendError> {
        if !self.session_exists(session)? {
            return Ok(0);
        }
        let out = self.run(&["list-clients", "-t", &format!("={session}"), "-F", "#{client_tty}"])?;
        Ok(out.lines().filter(|l| !l.trim().is_empty()).count())
    }

    fn panes(&self, session: &str) -> Result<Vec<PaneInfo>, BackendError> {
        if !self.session_exists(session)? {
            return Err(BackendError::SessionNotFound(session.to_string()));
        }
        let out = self.run(&[
            "list-panes",
            "-s",
            "-t",
            &format!("={session}"),
            "-F",
            "#{pane_id}\t#{window_index}\t#{?#{&&:#{pane_active},#{window_active}},1,0}\t#{pane_width}\t#{@canopy-role}\t#{pane_pid}\t#{pane_current_command}",
        ])?;
        let mut v = Vec::new();
        for line in out.lines() {
            let mut parts = line.split('\t');
            let id = Self::parse_pane_id(parts.next().unwrap_or_default())?;
            let window = parts.next().and_then(|w| w.trim().parse().ok()).unwrap_or(0);
            let active = parts.next().unwrap_or("0").trim() == "1";
            let width = parts.next().and_then(|w| w.trim().parse().ok()).unwrap_or(0);
            let role = parts.next().unwrap_or_default().to_string();
            let pid = parts.next().and_then(|p| p.trim().parse().ok());
            let current_command = parts.next().unwrap_or_default().to_string();
            v.push(PaneInfo { id, window, active, width, role, pid, current_command });
        }
        Ok(v)
    }

    fn active_window(&self, session: &str) -> Result<u32, BackendError> {
        // `display-message -t <session>` needs a client; list-windows does not.
        let wins = self.windows(session)?;
        wins.iter().find(|w| w.active).or(wins.first()).map(|w| w.index).ok_or_else(|| BackendError::SessionNotFound(session.to_string()))
    }

    fn read_screen(&self, pane: &PaneId, lines: usize) -> Result<String, BackendError> {
        let start = format!("-{}", lines.max(1));
        self.run(&["capture-pane", "-p", "-t", &pane.0, "-S", &start])
    }

    fn send_text(&self, pane: &PaneId, text: &str, enter: bool) -> Result<(), BackendError> {
        if !text.is_empty() {
            if text.contains('\n') {
                // Multi-line: a named buffer + paste-buffer so a word like `Enter` is never a keypress
                // and concurrent senders don't clobber each other's buffer.
                let buf = format!("canopy-{}-{}", std::process::id(), pane.0.trim_start_matches('%'));
                let mut child = self
                    .cmd()
                    .args(["load-buffer", "-b", &buf, "-"])
                    .stdin(std::process::Stdio::piped())
                    .spawn()?;
                use std::io::Write;
                child.stdin.take().expect("piped").write_all(text.as_bytes())?;
                let st = child.wait()?;
                if !st.success() {
                    return Err(BackendError::Command("tmux load-buffer failed".into()));
                }
                self.run(&["paste-buffer", "-d", "-b", &buf, "-t", &pane.0])?;
            } else {
                self.run(&["send-keys", "-t", &pane.0, "-l", text])?;
            }
        }
        if enter {
            self.run(&["send-keys", "-t", &pane.0, "Enter"])?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    #[test]
    fn pane_id_validation() {
        assert_eq!(Tmux::parse_pane_id("%12\n").unwrap(), PaneId("%12".into()));
        assert!(Tmux::parse_pane_id("hook output\n%12").is_err());
        assert!(Tmux::parse_pane_id("%").is_err());
        assert!(Tmux::parse_pane_id("12").is_err());
    }

    #[test]
    fn bound_key_parsing() {
        assert_eq!(Tmux::bound_key("bind-key    -T prefix b       run-shell \"x\""), Some("b"));
        assert_eq!(Tmux::bound_key("bind-key -r -T prefix Up select-pane -U"), Some("Up"));
        assert_eq!(Tmux::bound_key("bind-key  -T root C-M-c display-popup"), Some("C-M-c"));
        assert_eq!(Tmux::bound_key("set -g x y"), None);
    }

    #[test]
    fn keep_alive_wraps() {
        assert_eq!(Tmux::keep_alive("nvim ."), "nvim .; exec \"${SHELL:-/bin/sh}\"");
        assert_eq!(Tmux::keep_alive(""), "");
    }

    /// Real tmux on a scoped socket. Skipped when tmux is missing.
    #[test]
    fn session_lifecycle_on_scoped_socket() {
        if !Tmux::installed() {
            eprintln!("tmux not installed; skipping");
            return;
        }
        let sock = format!("canopy-test-{}", std::process::id());
        let t = Tmux::with_socket(&sock);
        let dir = tempfile::tempdir().unwrap();
        let mut env = BTreeMap::new();
        env.insert("CANOPY_PORT".to_string(), "40010".to_string());
        let session = "proj/bold-falcon";
        let ide = t
            .create_session(
                session,
                "ws",
                PaneSpec { role: "ide", command: "", cwd: dir.path(), env: &env, split: None, split_of: None, size_percent: None, size_cells: None, keep_alive: true, full_span: false, window: None },
            )
            .unwrap();
        assert!(t.session_exists(session).unwrap());
        let shell = t
            .add_pane(
                session,
                PaneSpec { role: "terminal:shell", command: "", cwd: dir.path(), env: &env, split: Some("below"), split_of: Some("ide"), size_percent: Some(15), size_cells: None, keep_alive: true, full_span: false, window: None },
            )
            .unwrap();
        let agent = t
            .add_pane(
                session,
                PaneSpec { role: "agent:claude", command: "", cwd: dir.path(), env: &env, split: Some("right"), split_of: Some("ide"), size_percent: Some(30), size_cells: None, keep_alive: true, full_span: false, window: None },
            )
            .unwrap();
        let side = t
            .add_pane(
                session,
                PaneSpec { role: "sidebar", command: "sleep 30", cwd: dir.path(), env: &env, split: Some("left"), split_of: Some("ide"), size_percent: None, size_cells: Some(24), keep_alive: false, full_span: true, window: None },
            )
            .unwrap();
        let w = t.run(&["display", "-p", "-t", &side.0, "#{pane_width}"]).unwrap();
        assert_eq!(w.trim(), "24");
        let (h, wh) = (t.run(&["display", "-p", "-t", &side.0, "#{pane_height}"]).unwrap(), t.run(&["display", "-p", "-t", &side.0, "#{window_height}"]).unwrap());
        assert_eq!(h.trim(), wh.trim(), "sidebar spans the full window height");
        assert_eq!(t.windows(session).unwrap()[0].name, "ws");
        t.open_window(session, "run:web", PaneSpec { role: "run:web", command: "", cwd: dir.path(), env: &env, split: None, split_of: None, size_percent: None, size_cells: None, keep_alive: true, full_span: false, window: None }).unwrap();
        let wins = t.windows(session).unwrap();
        assert_eq!(wins.len(), 2);
        assert_eq!(wins[1].name, "run:web");
        t.select_window(session, wins[1].index).unwrap();
        assert!(t.windows(session).unwrap()[1].active);
        assert_eq!(t.active_window(session).unwrap(), wins[1].index);
        let in_w1 = t.add_pane(session, PaneSpec { role: "sidebar", command: "sleep 30", cwd: dir.path(), env: &env, split: Some("left"), split_of: None, size_percent: None, size_cells: Some(20), keep_alive: false, full_span: true, window: Some(wins[1].index) }).unwrap();
        assert!(t.panes(session).unwrap().iter().any(|p| p.id == in_w1 && p.window == wins[1].index));
        t.kill_pane(&in_w1).unwrap();
        t.select_window(session, wins[0].index).unwrap();
        t.resize_pane(&side, 3).unwrap();
        assert_eq!(t.panes(session).unwrap().iter().find(|p| p.id == side).unwrap().width, 3);
        t.kill_pane(&side).unwrap();
        assert!(t.panes(session).unwrap().iter().all(|p| p.role != "sidebar"));
        // Runtime config: binds land where free, session chrome is per session.
        t.run(&["bind-key", "-T", "prefix", "g", "display-message", "user-owned"]).unwrap();
        t.ensure_server_config("/opt/canopy").unwrap();
        let prefix_keys = t.run(&["list-keys", "-T", "prefix"]).unwrap();
        let line = |k: &str| prefix_keys.lines().find(|l| Tmux::bound_key(l) == Some(k)).unwrap_or("").to_string();
        assert!(line("b").contains("/opt/canopy sidebar toggle"), "{}", line("b"));
        assert!(line("g").contains("user-owned"), "user bind must survive: {}", line("g"));
        let root_keys = t.run(&["list-keys", "-T", "root"]).unwrap();
        assert!(!root_keys.lines().any(|l| Tmux::bound_key(l) == Some("C-M-c") && l.contains("canopy")), "no global switcher key anymore");
        t.ensure_server_config("/opt/canopy").unwrap(); // idempotent
        assert_eq!(t.run(&["show-options", "-gwv", "window-status-separator"]).unwrap().trim(), "");
        t.decorate_session(session, "/opt/canopy statusline --workspace w1").unwrap();
        let sid = t.session_target(session).unwrap();
        assert_eq!(t.run(&["show-options", "-t", &sid, "-v", "status-position"]).unwrap().trim(), "top");
        assert!(t.run(&["show-options", "-t", &sid, "-v", "status-right"]).unwrap().contains("statusline --workspace w1"));
        assert!(t.run(&["show-hooks", "-t", &sid]).unwrap().contains("sidebar ensure"));
        assert_ne!(ide, shell);
        assert_ne!(shell, agent);
        let panes = t.panes(session).unwrap();
        let roles: Vec<&str> = panes.iter().map(|p| p.role.as_str()).collect();
        assert!(roles.contains(&"ide") && roles.contains(&"terminal:shell") && roles.contains(&"agent:claude"), "{roles:?}");
        assert_eq!(t.attached_clients(session).unwrap(), 0);
        let snap = t.snapshot().unwrap();
        let me = snap.iter().find(|s| s.name == session).expect("session in snapshot");
        assert!(!me.attached);
        assert!(me.panes.iter().any(|p| p.role == "agent:claude"));
        t.send_text(&shell, "echo canopy-marker-$CANOPY_PORT", true).unwrap();
        std::thread::sleep(std::time::Duration::from_millis(300));
        let screen = t.read_screen(&shell, 20).unwrap();
        assert!(screen.contains("canopy-marker-40010"), "{screen}");
        t.rename_session(session, "proj/renamed").unwrap();
        assert!(t.session_exists("proj/renamed").unwrap());
        assert!(!t.session_exists(session).unwrap());
        t.kill_session("proj/renamed").unwrap();
        assert!(!t.session_exists("proj/renamed").unwrap());
        let _ = t.run(&["kill-server"]);
    }
}
