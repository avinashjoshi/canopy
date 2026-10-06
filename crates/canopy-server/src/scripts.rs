//! Lifecycle script runner (`scripts.setup` / `scripts.archive`).
//!
//! Scripts are executables relative to the project root, run with `cwd = workspace`, the
//! `CANOPY_*` env merged over the server's environment, in their own process group so a
//! timeout or cancel kills the whole tree (`bundle install` children included). Output is
//! streamed line by line to a per-workspace log file *while the script runs* (so a client
//! can tail it live), the last line is mirrored into `progress`, and the tail is kept for
//! diagnostics.

use std::collections::BTreeMap;
use std::fs::File;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// Last non-empty output line of a running script, shared with the server runtime so
/// list rows can show "what is it doing right now" without reading the log.
pub type Progress = Arc<Mutex<String>>;

pub fn set_progress(p: &Option<Progress>, text: &str) {
    if let Some(p) = p {
        if let Ok(mut g) = p.lock() {
            *g = text.to_string();
        }
    }
}

/// Append a canopy-authored marker line to a workspace log (phase changes, outcomes).
/// `══` opens a new run; `──` marks a step inside it. Clients key on these.
pub fn log_note(log: &Path, msg: &str) {
    if let Some(parent) = log.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(log) {
        let _ = writeln!(f, "── {} {msg}", chrono_like_now());
    }
}

pub fn log_run_header(log: &Path, msg: &str) {
    if let Some(parent) = log.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(log) {
        let _ = writeln!(f, "══ {} {msg}", chrono_like_now());
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ScriptError {
    #[error("script not found or not executable: {0}")]
    NotFound(PathBuf),
    #[error("script {script} exited with {code}; last output:\n{tail}")]
    Failed { script: String, code: i32, tail: String },
    #[error("script {script} killed by signal; last output:\n{tail}")]
    Signaled { script: String, tail: String },
    #[error("script {script} timed out after {secs}s; last output:\n{tail}")]
    TimedOut { script: String, secs: u64, tail: String },
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
}

#[derive(Debug, Clone)]
pub struct ScriptRun<'a> {
    pub project_root: &'a Path,
    pub script: &'a str,
    pub cwd: &'a Path,
    pub env: &'a BTreeMap<String, String>,
    /// 0 = no timeout.
    pub timeout_secs: u64,
    /// Append stdout+stderr here (created if missing), live as the script runs.
    pub log_file: Option<&'a Path>,
    /// Mirror the latest output line here while running.
    pub progress: Option<Progress>,
}

/// Human hint for recognizable failures (v0's `Diagnose`).
pub fn diagnose(tail: &str) -> String {
    let t = tail.to_ascii_lowercase();
    let hints: &[(&str, &str)] = &[
        ("master.key", "Rails master key missing: symlink config/master.key from $CANOPY_ROOT_PATH in scripts.setup"),
        ("database", "database step failed: prefer db:prepare over db:create so retry is idempotent"),
        ("could not resolve host", "network error while fetching dependencies; retry when online"),
        ("bundle: command not found", "bundler not on PATH for non-interactive shells; check mise/asdf activation"),
        ("permission denied", "permission denied: check the script's executable bit and file ownership"),
        ("eaddrinuse", "port already in use: something outside canopy holds $CANOPY_PORT"),
        ("address already in use", "port already in use: something outside canopy holds $CANOPY_PORT"),
    ];
    for (needle, hint) in hints {
        if t.contains(needle) {
            return (*hint).to_string();
        }
    }
    String::new()
}

fn tail_of(buf: &[u8], lines: usize) -> String {
    let s = String::from_utf8_lossy(buf);
    let v: Vec<&str> = s.lines().collect();
    let start = v.len().saturating_sub(lines);
    v[start..].join("\n")
}

/// Run a script to completion. Blocking; call from `spawn_blocking`.
pub fn run(r: ScriptRun<'_>) -> Result<String, ScriptError> {
    let path = if Path::new(r.script).is_absolute() { PathBuf::from(r.script) } else { r.project_root.join(r.script) };
    if !path.is_file() {
        return Err(ScriptError::NotFound(path));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if std::fs::metadata(&path)?.permissions().mode() & 0o111 == 0 {
            return Err(ScriptError::NotFound(path));
        }
    }

    let mut cmd = Command::new(&path);
    cmd.current_dir(r.cwd).envs(r.env).stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::piped());
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        cmd.process_group(0);
    }
    // ETXTBSY: a just-written script may still be held open by a concurrently forked child
    // (the fd closes on exec). Retry briefly instead of failing a fresh workspace.
    let mut child = {
        let mut attempt = 0;
        loop {
            match cmd.spawn() {
                Ok(c) => break c,
                Err(e) if e.raw_os_error() == Some(26) && attempt < 20 => {
                    attempt += 1;
                    std::thread::sleep(Duration::from_millis(25));
                }
                Err(e) => return Err(e.into()),
            }
        }
    };
    let pid = child.id();
    let stdout = child.stdout.take().expect("piped");
    let stderr = child.stderr.take().expect("piped");

    // The log is open for the whole run so every chunk lands as it is produced.
    let log: Arc<Mutex<Option<File>>> = Arc::new(Mutex::new(r.log_file.and_then(|log| {
        if let Some(parent) = log.parent() {
            let _ = std::fs::create_dir_all(parent);
        }
        let mut f = std::fs::OpenOptions::new().create(true).append(true).open(log).ok()?;
        let _ = writeln!(f, "=== {} {} ===", chrono_like_now(), r.script);
        Some(f)
    })));
    let progress = r.progress.clone();

    // Drain both pipes on helper threads so a chatty script never blocks on a full pipe;
    // each chunk is appended to the log and the last complete line becomes `progress`.
    let drain = |mut pipe: Box<dyn Read + Send>, log: Arc<Mutex<Option<File>>>, progress: Option<Progress>| {
        std::thread::spawn(move || {
            let mut all = Vec::new();
            let mut buf = [0u8; 4096];
            let mut partial = String::new();
            loop {
                let n = match pipe.read(&mut buf) {
                    Ok(0) | Err(_) => break,
                    Ok(n) => n,
                };
                let chunk = &buf[..n];
                all.extend_from_slice(chunk);
                if let Ok(mut g) = log.lock() {
                    if let Some(f) = g.as_mut() {
                        let _ = f.write_all(chunk);
                    }
                }
                partial.push_str(&String::from_utf8_lossy(chunk));
                let mut last_line: Option<String> = None;
                while let Some(i) = partial.find(['\n', '\r']) {
                    let line = partial[..i].trim().to_string();
                    partial = partial[i + 1..].to_string();
                    if !line.is_empty() {
                        last_line = Some(line);
                    }
                }
                if let Some(l) = last_line {
                    set_progress(&progress, &l);
                }
            }
            all
        })
    };
    let out_t = drain(Box::new(stdout), log.clone(), progress.clone());
    let err_t = drain(Box::new(stderr), log.clone(), progress.clone());

    let deadline = (r.timeout_secs > 0).then(|| Instant::now() + Duration::from_secs(r.timeout_secs));
    let status = loop {
        if let Some(st) = child.try_wait()? {
            break Some(st);
        }
        if deadline.is_some_and(|d| Instant::now() >= d) {
            kill_group(pid);
            let _ = child.wait();
            break None;
        }
        std::thread::sleep(Duration::from_millis(50));
    };
    let mut combined = out_t.join().unwrap_or_default();
    combined.extend_from_slice(&err_t.join().unwrap_or_default());
    if let Ok(mut g) = log.lock() {
        if let Some(f) = g.as_mut() {
            let _ = writeln!(f);
        }
    }
    let tail = tail_of(&combined, 30);
    match status {
        None => Err(ScriptError::TimedOut { script: r.script.to_string(), secs: r.timeout_secs, tail }),
        Some(st) if st.success() => Ok(tail),
        Some(st) => match st.code() {
            Some(code) => Err(ScriptError::Failed { script: r.script.to_string(), code, tail }),
            None => Err(ScriptError::Signaled { script: r.script.to_string(), tail }),
        },
    }
}

fn kill_group(pid: u32) {
    #[cfg(unix)]
    {
        use nix::sys::signal::{killpg, Signal};
        use nix::unistd::Pid;
        let pg = Pid::from_raw(pid as i32);
        let _ = killpg(pg, Signal::SIGTERM);
        std::thread::sleep(Duration::from_millis(500));
        let _ = killpg(pg, Signal::SIGKILL);
    }
}

/// RFC 3339-ish UTC timestamp without pulling in chrono.
pub fn chrono_like_now() -> String {
    let secs = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
    // Civil-from-days (Howard Hinnant's algorithm).
    let days = (secs / 86400) as i64;
    let rem = secs % 86400;
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    format!("{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z", rem / 3600, (rem % 3600) / 60, rem % 60)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write_script(dir: &Path, name: &str, body: &str) -> String {
        let p = dir.join(name);
        std::fs::write(&p, format!("#!/bin/sh\n{body}\n")).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        name.to_string()
    }

    #[test]
    fn output_is_logged_live_and_progress_tracks_last_line() {
        let dir = tempfile::tempdir().unwrap();
        let s = write_script(dir.path(), "slow.sh", "echo first\nsleep 1\necho second");
        let log = dir.path().join("log/live.log");
        let progress: Progress = Arc::new(Mutex::new(String::new()));
        let log2 = log.clone();
        let progress2 = progress.clone();
        let probe = std::thread::spawn(move || {
            // Sample while the script is still sleeping: the first line must already be there.
            std::thread::sleep(Duration::from_millis(500));
            (std::fs::read_to_string(&log2).unwrap_or_default(), progress2.lock().unwrap().clone())
        });
        run(ScriptRun { project_root: dir.path(), script: &s, cwd: dir.path(), env: &BTreeMap::new(), timeout_secs: 10, log_file: Some(&log), progress: Some(progress.clone()) }).unwrap();
        let (mid_log, mid_progress) = probe.join().unwrap();
        assert!(mid_log.contains("first"), "log mid-run: {mid_log:?}");
        assert!(!mid_log.contains("second"), "log mid-run: {mid_log:?}");
        assert_eq!(mid_progress, "first");
        assert_eq!(progress.lock().unwrap().as_str(), "second");
        let full = std::fs::read_to_string(&log).unwrap();
        assert!(full.contains("first\nsecond"), "{full:?}");
    }

    #[test]
    fn log_notes_mark_runs_and_steps() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("log/n.log");
        log_run_header(&log, "creating x");
        log_note(&log, "worktree ready");
        let text = std::fs::read_to_string(&log).unwrap();
        let lines: Vec<&str> = text.lines().collect();
        assert_eq!(lines.len(), 2);
        assert!(lines[0].starts_with("══ ") && lines[0].ends_with("creating x"));
        assert!(lines[1].starts_with("── ") && lines[1].ends_with("worktree ready"));
    }

    #[test]
    fn success_streams_to_log() {
        let dir = tempfile::tempdir().unwrap();
        let s = write_script(dir.path(), "ok.sh", "echo hello $CANOPY_PORT; echo err >&2");
        let mut env = BTreeMap::new();
        env.insert("CANOPY_PORT".into(), "40010".into());
        let log = dir.path().join("log/setup.log");
        let tail = run(ScriptRun { project_root: dir.path(), script: &s, cwd: dir.path(), env: &env, timeout_secs: 10, log_file: Some(&log), progress: None }).unwrap();
        assert!(tail.contains("hello 40010"));
        assert!(tail.contains("err"));
        let logged = std::fs::read_to_string(log).unwrap();
        assert!(logged.contains("ok.sh") && logged.contains("hello 40010"));
    }

    #[test]
    fn failure_has_code_and_tail() {
        let dir = tempfile::tempdir().unwrap();
        let s = write_script(dir.path(), "bad.sh", "echo 'rails aborted! config/master.key missing'; exit 3");
        let err = run(ScriptRun { project_root: dir.path(), script: &s, cwd: dir.path(), env: &BTreeMap::new(), timeout_secs: 0, log_file: None, progress: None }).unwrap_err();
        match err {
            ScriptError::Failed { code, tail, .. } => {
                assert_eq!(code, 3);
                assert!(diagnose(&tail).contains("master key"));
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn timeout_kills_process_group() {
        let dir = tempfile::tempdir().unwrap();
        let s = write_script(dir.path(), "slow.sh", "sleep 30 & wait");
        let start = Instant::now();
        let err = run(ScriptRun { project_root: dir.path(), script: &s, cwd: dir.path(), env: &BTreeMap::new(), timeout_secs: 1, log_file: None, progress: None }).unwrap_err();
        assert!(matches!(err, ScriptError::TimedOut { secs: 1, .. }), "{err:?}");
        assert!(start.elapsed() < Duration::from_secs(5));
    }

    #[test]
    fn missing_or_non_executable() {
        let dir = tempfile::tempdir().unwrap();
        assert!(matches!(
            run(ScriptRun { project_root: dir.path(), script: "nope.sh", cwd: dir.path(), env: &BTreeMap::new(), timeout_secs: 0, log_file: None, progress: None }),
            Err(ScriptError::NotFound(_))
        ));
        std::fs::write(dir.path().join("noexec.sh"), "#!/bin/sh\n").unwrap();
        assert!(matches!(
            run(ScriptRun { project_root: dir.path(), script: "noexec.sh", cwd: dir.path(), env: &BTreeMap::new(), timeout_secs: 0, log_file: None, progress: None }),
            Err(ScriptError::NotFound(_))
        ));
    }

    #[test]
    fn timestamp_shape() {
        let t = chrono_like_now();
        assert_eq!(t.len(), 20, "{t}");
        assert!(t.ends_with('Z'));
    }
}
