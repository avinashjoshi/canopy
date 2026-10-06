//! Agent hook integrations: make the agent itself tell canopy what it is doing.
//!
//! `canopy integration install claude` writes a managed hook script and registers it in
//! `~/.claude/settings.json`. The script posts `pane.report_agent` through the `canopy`
//! binary (so there is no JSON-over-socket code in shell), only when run inside a canopy
//! session (`CANOPY_WORKSPACE_ID` + `CANOPY_SOCKET_PATH` set); elsewhere it is a no-op.

use canopy_core::paths::Paths;
use std::path::{Path, PathBuf};

pub const SCRIPT_NAME: &str = "canopy-agent-state.sh";
pub const MARKER: &str = "canopy-agent-state";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Integration {
    pub agent: &'static str,
    pub settings: &'static str,
    /// (hook event, canopy state)
    pub events: &'static [(&'static str, &'static str)],
}

pub const INTEGRATIONS: &[Integration] = &[Integration {
    agent: "claude",
    settings: ".claude/settings.json",
    events: &[("SessionStart", "idle"), ("UserPromptSubmit", "working"), ("PreToolUse", "working"), ("Stop", "idle"), ("Notification", "blocked"), ("SessionEnd", "done")],
}];

pub fn find(agent: &str) -> Option<&'static Integration> {
    INTEGRATIONS.iter().find(|i| i.agent == agent)
}

pub fn script_path(paths: &Paths) -> PathBuf {
    paths.home.join("hooks").join(SCRIPT_NAME)
}

pub fn script_body(canopy_bin: &Path) -> String {
    format!(
        r#"#!/bin/sh
# Managed by canopy ({MARKER}). Reports agent state to the canopy server.
# Usage: {SCRIPT_NAME} <state>   (hook JSON on stdin)
state="$1"
[ -n "$CANOPY_WORKSPACE_ID" ] || exit 0
[ -n "$CANOPY_SOCKET_PATH" ] || exit 0
input="$(cat 2>/dev/null)"
session_id="$(printf '%s' "$input" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
seq="$(date +%s%N 2>/dev/null || date +%s)"
bin="$(command -v canopy 2>/dev/null || printf '%s' '{bin}')"
if [ -n "$session_id" ]; then
  "$bin" pane report-agent --workspace-id "$CANOPY_WORKSPACE_ID" --state "$state" --seq "$seq" --session-id "$session_id" >/dev/null 2>&1 &
else
  "$bin" pane report-agent --workspace-id "$CANOPY_WORKSPACE_ID" --state "$state" --seq "$seq" >/dev/null 2>&1 &
fi
exit 0
"#,
        bin = canopy_bin.display()
    )
}

fn hook_entry(script: &Path, state: &str) -> serde_json::Value {
    serde_json::json!({
        "hooks": [{ "type": "command", "command": format!("sh {} {}", script.display(), state), "timeout": 5 }]
    })
}

fn is_ours(entry: &serde_json::Value) -> bool {
    entry.get("hooks").and_then(|h| h.as_array()).is_some_and(|arr| arr.iter().any(|h| h.get("command").and_then(|c| c.as_str()).is_some_and(|c| c.contains(MARKER))))
}

/// Add our hooks to an agent settings document (idempotent). Returns the new document.
pub fn install_into(mut settings: serde_json::Value, integ: &Integration, script: &Path) -> serde_json::Value {
    if !settings.is_object() {
        settings = serde_json::json!({});
    }
    let hooks = settings.as_object_mut().unwrap().entry("hooks").or_insert_with(|| serde_json::json!({}));
    if !hooks.is_object() {
        *hooks = serde_json::json!({});
    }
    for (event, state) in integ.events {
        let list = hooks.as_object_mut().unwrap().entry(*event).or_insert_with(|| serde_json::json!([]));
        if !list.is_array() {
            *list = serde_json::json!([]);
        }
        let arr = list.as_array_mut().unwrap();
        arr.retain(|e| !is_ours(e));
        arr.push(hook_entry(script, state));
    }
    settings
}

pub fn uninstall_from(mut settings: serde_json::Value) -> serde_json::Value {
    if let Some(hooks) = settings.get_mut("hooks").and_then(|h| h.as_object_mut()) {
        for (_, list) in hooks.iter_mut() {
            if let Some(arr) = list.as_array_mut() {
                arr.retain(|e| !is_ours(e));
            }
        }
        hooks.retain(|_, v| v.as_array().is_some_and(|a| !a.is_empty()));
    }
    settings
}

pub fn is_installed(settings: &serde_json::Value) -> bool {
    settings.get("hooks").and_then(|h| h.as_object()).is_some_and(|m| m.values().any(|l| l.as_array().is_some_and(|a| a.iter().any(is_ours))))
}

/// Installed *and* every referenced script file still exists (a sandbox install whose
/// `CANOPY_HOME` was deleted would otherwise leave dangling hooks behind).
pub fn is_healthy(settings: &serde_json::Value) -> bool {
    let Some(hooks) = settings.get("hooks").and_then(|h| h.as_object()) else { return false };
    let mut seen = false;
    for list in hooks.values() {
        for entry in list.as_array().into_iter().flatten().filter(|e| is_ours(e)) {
            seen = true;
            for h in entry.get("hooks").and_then(|h| h.as_array()).into_iter().flatten() {
                let cmd = h.get("command").and_then(|c| c.as_str()).unwrap_or("");
                // "sh <path> <state>"
                if let Some(path) = cmd.split_whitespace().nth(1) {
                    if !Path::new(path).is_file() {
                        return false;
                    }
                }
            }
        }
    }
    seen
}

fn settings_file(integ: &Integration) -> anyhow::Result<PathBuf> {
    let home = std::env::var_os("HOME").ok_or_else(|| anyhow::anyhow!("HOME not set"))?;
    Ok(Path::new(&home).join(integ.settings))
}

fn read_settings(path: &Path) -> anyhow::Result<serde_json::Value> {
    match std::fs::read_to_string(path) {
        Ok(t) if t.trim().is_empty() => Ok(serde_json::json!({})),
        Ok(t) => Ok(serde_json::from_str(&t)?),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(serde_json::json!({})),
        Err(e) => Err(e.into()),
    }
}

fn write_settings(path: &Path, v: &serde_json::Value) -> anyhow::Result<()> {
    if let Some(p) = path.parent() {
        std::fs::create_dir_all(p)?;
    }
    if path.exists() {
        let backup = path.with_extension(format!("json.bak-{}", crate::scripts::chrono_like_now().replace(':', "")));
        std::fs::copy(path, backup)?;
    }
    let tmp = path.with_extension("json.tmp");
    std::fs::write(&tmp, serde_json::to_string_pretty(v)? + "\n")?;
    std::fs::rename(tmp, path)?;
    Ok(())
}

pub fn install(paths: &Paths, agent: &str, canopy_bin: &Path) -> anyhow::Result<PathBuf> {
    let integ = find(agent).ok_or_else(|| anyhow::anyhow!("no hook integration for {agent:?}; available: {}", INTEGRATIONS.iter().map(|i| i.agent).collect::<Vec<_>>().join(", ")))?;
    let script = script_path(paths);
    std::fs::create_dir_all(script.parent().unwrap())?;
    std::fs::write(&script, script_body(canopy_bin))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755))?;
    }
    let file = settings_file(integ)?;
    let settings = read_settings(&file)?;
    write_settings(&file, &install_into(settings, integ, &script))?;
    Ok(file)
}

pub fn uninstall(paths: &Paths, agent: &str) -> anyhow::Result<PathBuf> {
    let integ = find(agent).ok_or_else(|| anyhow::anyhow!("no hook integration for {agent:?}"))?;
    let file = settings_file(integ)?;
    let settings = read_settings(&file)?;
    write_settings(&file, &uninstall_from(settings))?;
    let _ = std::fs::remove_file(script_path(paths));
    Ok(file)
}

pub fn status(agent: &str) -> anyhow::Result<bool> {
    let integ = find(agent).ok_or_else(|| anyhow::anyhow!("no hook integration for {agent:?}"))?;
    let file = settings_file(integ)?;
    Ok(is_installed(&read_settings(&file)?))
}

/// Installed and the hook script is present on disk.
pub fn healthy(agent: &str) -> anyhow::Result<bool> {
    let integ = find(agent).ok_or_else(|| anyhow::anyhow!("no hook integration for {agent:?}"))?;
    let file = settings_file(integ)?;
    Ok(is_healthy(&read_settings(&file)?))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn install_is_idempotent_and_preserves_others() {
        let integ = find("claude").unwrap();
        let script = Path::new("/home/x/.canopy/hooks/canopy-agent-state.sh");
        let existing = serde_json::json!({
            "model": "opus",
            "hooks": { "Stop": [ { "hooks": [ { "type": "command", "command": "notify-send done" } ] } ] }
        });
        let once = install_into(existing, integ, script);
        let twice = install_into(once.clone(), integ, script);
        assert_eq!(once, twice);
        assert!(is_installed(&twice));
        assert_eq!(twice["model"], "opus");
        let stop = twice["hooks"]["Stop"].as_array().unwrap();
        assert_eq!(stop.len(), 2, "user hook kept + ours added");
        assert!(stop[0]["hooks"][0]["command"].as_str().unwrap().contains("notify-send"));
        assert!(!is_healthy(&twice), "script path does not exist in this test");
        let removed = uninstall_from(twice);
        assert!(!is_installed(&removed));
        assert_eq!(removed["hooks"]["Stop"].as_array().unwrap().len(), 1);
        assert!(removed["hooks"].get("UserPromptSubmit").is_none());
    }

    #[test]
    fn script_mentions_marker_and_states() {
        let body = script_body(Path::new("/usr/local/bin/canopy"));
        assert!(body.contains(MARKER));
        assert!(body.contains("pane report-agent"));
        assert!(body.contains("/usr/local/bin/canopy"));
    }
}
