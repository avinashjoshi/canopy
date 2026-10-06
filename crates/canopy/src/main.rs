//! The `canopy` binary: CLI, TUI client, server (`canopy server`) and, later, the remote
//! bridge. Routing only; logic lives in the library crates.

use anyhow::{bail, Context, Result};
use canopy_core::config::{ProjectConfig, FILE_NAME};
use canopy_core::paths::Paths;
use canopy_proto::{AttachTarget, Method, ResultBody, WorkspaceCreate, WorkspaceRef, WorkspaceRow};
use clap::{Args, Parser, Subcommand};
use std::io::IsTerminal;
use std::path::{Path, PathBuf};

mod doctor;
mod upgrade;

#[derive(Parser)]
#[command(name = "canopy", version, about = "Git worktree workspaces with paired terminal sessions for AI coding agents")]
struct Cli {
    /// Run against a remote host over ssh (any `ssh` target: `tower`, `user@host`). No setup
    /// needed; canopy must be installed there (or pass --install to copy this binary over).
    #[arg(long, global = true)]
    remote: Option<String>,
    /// With --remote: copy this binary to the host if canopy is missing there.
    #[arg(long, global = true, requires = "remote")]
    install: bool,
    /// With --remote: attach with plain ssh even when mosh is available.
    #[arg(long, global = true, requires = "remote")]
    ssh: bool,
    #[command(subcommand)]
    command: Option<Command>,
}

#[derive(Subcommand)]
enum Command {
    /// Create a workspace (worktree + port + setup script + session) and attach.
    New(NewArgs),
    /// List workspaces in the current project (or everywhere with --all).
    Ls {
        #[arg(long)]
        all: bool,
        #[arg(long)]
        json: bool,
    },
    /// Attach to a workspace (resurrecting it first if stopped).
    Switch {
        name: String,
        /// Don't detach other clients.
        #[arg(long)]
        share: bool,
    },
    /// Tear a workspace down: archive script, session, worktree, branch, state row.
    Rm {
        name: String,
        /// Skip the confirmation prompt.
        #[arg(short = 'y', long)]
        yes: bool,
        /// Remove even with uncommitted/unpushed work or an open PR.
        #[arg(long)]
        force: bool,
    },
    /// Re-run scripts.setup on a broken workspace, streaming its output.
    Retry {
        name: String,
        #[arg(long)]
        force: bool,
    },
    /// Show a workspace's setup log (default: the current workspace).
    Log {
        name: Option<String>,
        /// Keep printing while setup is running.
        #[arg(short, long)]
        follow: bool,
        /// How many trailing lines to start with.
        #[arg(short = 'n', long, default_value_t = 60)]
        lines: usize,
    },
    /// Sync the workspace label to its live branch; --pin freezes it.
    Rename {
        name: Option<String>,
        #[arg(long, conflicts_with = "unpin")]
        pin: bool,
        #[arg(long)]
        unpin: bool,
    },
    /// Kill the session; the worktree stays (status becomes stopped).
    Stop { name: String },
    /// Update statuses to match disk + session reality.
    Reconcile,
    /// Session anchored at the project root (port = project base).
    Main,
    /// Launch a run script (scripts.run) in the current workspace's session.
    Run { script: Option<String> },
    /// Onboard a project: a local path (default: cwd) or a git URL to clone. Writes
    /// canopy.json and registers the project.
    Init {
        /// Path of a repo, or a git URL (cloned into the source root).
        target: Option<String>,
        #[arg(long)]
        with_scripts: bool,
        /// Seed `scripts` from this JSON/TOML file (relative to the repo root). Without it,
        /// the `[init] adopt_from` candidates in ~/.canopy/config.toml are tried.
        #[arg(long)]
        from: Option<PathBuf>,
        /// Overwrite an existing canopy.json (local path only).
        #[arg(long)]
        force: bool,
    },
    /// Agent hook integrations (agent reports its own state to canopy).
    Integration {
        #[command(subcommand)]
        what: IntegrationCommand,
    },
    /// Pane-level helpers used by hooks.
    Pane {
        #[command(subcommand)]
        what: PaneCommand,
    },
    /// tmux status-right segment for a workspace (default: the one this runs in).
    Statusline {
        #[arg(long)]
        workspace: Option<String>,
        /// Accepted for compatibility with v0's `~/.tmux.conf` block; ignored.
        #[arg(long, hide = true)]
        format: Option<String>,
    },
    /// The sidebar pane (run inside a canopy session). `sidebar toggle` shows/hides it.
    Sidebar {
        /// Keep the pane at this width in columns.
        #[arg(long)]
        width: Option<u16>,
        #[command(subcommand)]
        what: Option<SidebarCommand>,
    },
    /// Server control.
    Server {
        #[command(subcommand)]
        what: Option<ServerCommand>,
    },
    /// Pipe stdio to the local server socket (run on the far side of `--remote`).
    #[command(hide = true)]
    Bridge,
    /// Full-screen UI pieces launched by the sidebar inside tmux popups.
    #[command(hide = true)]
    Ui {
        #[command(subcommand)]
        what: UiCommand,
    },
    /// Inspect or call the socket API.
    Api {
        #[command(subcommand)]
        what: ApiCommand,
    },
    /// Print the default ~/.canopy/config.toml.
    DefaultConfig,
    /// Check this machine: tmux, git, agents, server, hooks. Changes nothing.
    Doctor,
    /// Download the latest release, swap the binary in place and restart the server.
    Upgrade {
        /// Only report whether a newer release exists.
        #[arg(long)]
        check: bool,
    },
    /// Print version, protocol and paths.
    Version,
}

#[derive(Args)]
struct NewArgs {
    /// Explicit workspace name (default: random adjective-noun).
    #[arg(long)]
    name: Option<String>,
    /// Check out an existing branch instead of creating one.
    #[arg(long, conflicts_with_all = ["pr", "issue"])]
    branch: Option<String>,
    /// Check out a GitHub PR's branch; briefing seeded from the PR body.
    #[arg(long, conflicts_with = "issue")]
    pr: Option<u64>,
    /// Fresh branch; briefing seeded from the issue body.
    #[arg(long)]
    issue: Option<u64>,
    /// Opening message typed into the agent.
    #[arg(long, conflicts_with = "prompt_file")]
    prompt: Option<String>,
    /// Opening message from a file (max 32 KB).
    #[arg(long)]
    prompt_file: Option<PathBuf>,
    /// Agent launcher (claude, codex, opencode, gemini, aider).
    #[arg(long)]
    agent: Option<String>,
    /// Project (name or root path) when not running inside one; required with --remote
    /// unless the host has exactly one project.
    #[arg(long)]
    project: Option<String>,
    /// Create but don't attach.
    #[arg(long)]
    no_attach: bool,
}

#[derive(Subcommand)]
enum IntegrationCommand {
    Install { agent: String },
    Uninstall { agent: String },
    List,
}

#[derive(Subcommand)]
enum PaneCommand {
    /// Report agent state for a workspace (called by hook scripts).
    ReportAgent {
        #[arg(long)]
        workspace_id: String,
        #[arg(long)]
        state: String,
        #[arg(long, default_value = "canopy:hook")]
        source: String,
        #[arg(long)]
        seq: u64,
        #[arg(long)]
        session_id: Option<String>,
        #[arg(long, default_value = "")]
        agent: String,
    },
}

#[derive(Subcommand)]
enum UiCommand {
    /// New-workspace form for a project; switches to the new session on success.
    New {
        #[arg(long)]
        project_root: PathBuf,
    },
    /// Add-project prompt.
    AddProject,
}

#[derive(Subcommand)]
enum SidebarCommand {
    /// Show / focus / hide the sidebar in the session's active window.
    Toggle {
        #[arg(long)]
        session: Option<String>,
    },
    /// Add a sidebar to the session's active window if missing (used by the tmux hook).
    Ensure {
        #[arg(long)]
        session: Option<String>,
    },
}

#[derive(Subcommand)]
enum ServerCommand {
    Stop,
    Status,
    ReloadConfig,
}

#[derive(Subcommand)]
enum ApiCommand {
    /// Print the JSON schema of requests, responses and events.
    Schema,
    /// Ping the running server (starting one if needed).
    Ping,
    /// Call any method: `canopy api call workspace.list '{"project_root":"/x"}'`.
    Call { method: String, params: Option<String> },
}

fn main() {
    if let Err(e) = real_main() {
        eprintln!("canopy: {e:#}");
        std::process::exit(1);
    }
}

fn real_main() -> Result<()> {
    let cli = Cli::parse();
    let paths = Paths::from_env();
    let remote = cli.remote.clone();
    let use_mosh = !cli.ssh && which("mosh");
    if let Some(target) = &remote {
        canopy_client::remote::ensure_remote(target, cli.install)?;
        return remote_main(target, use_mosh, cli.command);
    }
    match cli.command {
        None => tui(&paths),
        Some(Command::Bridge) => canopy_client::remote::run_bridge(&paths),
        Some(Command::Ui { what }) => {
            let sock = canopy_client::connect(&paths)?;
            let t = canopy_client::Transport::Local(sock);
            match what {
                UiCommand::New { project_root } => {
                    let name = project_root.file_name().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default();
                    if let Some(ws) = canopy_client::tui::run_new_popup(t, project_root, name)? {
                        let _ = std::process::Command::new("tmux").args(["switch-client", "-t", &format!("={}", ws.session)]).status();
                        if let Ok(out) = std::process::Command::new("tmux").args(["list-panes", "-t", &format!("={}", ws.session), "-F", "#{pane_id}\t#{@canopy-role}"]).output() {
                            let text = String::from_utf8_lossy(&out.stdout);
                            if let Some(id) = text.lines().find(|l| l.ends_with("\tsidebar")).and_then(|l| l.split('\t').next()) {
                                let _ = std::process::Command::new("tmux").args(["set-option", "-p", "-t", id, "@canopy-focus", "1"]).status();
                                let _ = std::process::Command::new("tmux").args(["select-pane", "-t", id]).status();
                            }
                        }
                    }
                    Ok(())
                }
                UiCommand::AddProject => {
                    canopy_client::tui::run_add_project_popup(t)?;
                    Ok(())
                }
            }
        }
        Some(Command::New(a)) => cmd_new(&paths, a),
        Some(Command::Ls { all, json }) => cmd_ls(&paths, all, json),
        Some(Command::Switch { name, share }) => {
            if share {
                std::env::set_var("CANOPY_NO_DETACH", "1");
            }
            let sock = canopy_client::connect(&paths)?;
            let target = attach_target(&sock, &ws_ref(&paths, &name)?)?;
            canopy_client::exec_attach(&target)
        }
        Some(Command::Rm { name, yes, force }) => cmd_rm(&paths, &name, yes, force),
        Some(Command::Retry { name, force }) => {
            let sock = canopy_client::connect(&paths)?;
            let t = canopy_client::Transport::Local(sock);
            eprintln!("re-running setup for {name}…");
            let workspace = canopy_client::livelog::retry_streaming(&t, ws_ref(&paths, &name)?, force, &mut std::io::stderr())?;
            println!("{} is {}", workspace.name, status_word(&workspace));
            Ok(())
        }
        Some(Command::Log { name, follow, lines }) => {
            let sock = canopy_client::connect(&paths)?;
            let r = match name {
                Some(n) => ws_ref(&paths, &n)?,
                None => current_ws_ref(&paths)?,
            };
            let t = canopy_client::Transport::Local(sock);
            canopy_client::livelog::print_log(&t, r, follow, lines, &mut std::io::stdout())
        }
        Some(Command::Rename { name, pin, unpin }) => {
            let sock = canopy_client::connect(&paths)?;
            let r = match name {
                Some(n) => ws_ref(&paths, &n)?,
                None => current_ws_ref(&paths)?,
            };
            let pin = if pin { Some(true) } else if unpin { Some(false) } else { None };
            let res = canopy_client::call(&sock, Method::WorkspaceRename { workspace: r, pin })?;
            if let ResultBody::Workspace { workspace } = res {
                println!("{} -> branch {}{}", workspace.name, workspace.branch, if pin == Some(true) { " (pinned)" } else { "" });
            }
            Ok(())
        }
        Some(Command::Stop { name }) => {
            let sock = canopy_client::connect(&paths)?;
            canopy_client::call(&sock, Method::WorkspaceStop { workspace: ws_ref(&paths, &name)? })?;
            println!("stopped {name}");
            Ok(())
        }
        Some(Command::Reconcile) => {
            let sock = canopy_client::connect(&paths)?;
            let root = project_root_opt(&paths);
            let r = canopy_client::call(&sock, Method::WorkspaceReconcile { project_root: root })?;
            if let ResultBody::WorkspaceList { workspaces } = r {
                print_table(&workspaces, true);
            }
            Ok(())
        }
        Some(Command::Main) => {
            let sock = canopy_client::connect(&paths)?;
            let root = project_root(&paths)?;
            let r = canopy_client::call(&sock, Method::ProjectMain { root })?;
            if let ResultBody::AttachTarget { target } = r {
                canopy_client::exec_attach(&target)?;
            }
            Ok(())
        }
        Some(Command::Run { script }) => {
            let sock = canopy_client::connect(&paths)?;
            canopy_client::call(&sock, Method::WorkspaceRun { workspace: current_ws_ref(&paths)?, script })?;
            Ok(())
        }
        Some(Command::Init { target, with_scripts, from, force }) => {
            let is_url = target.as_deref().is_some_and(|t| t.contains("://") || (t.contains('@') && t.contains(':')));
            if is_url {
                let sock = canopy_client::connect(&paths)?;
                let r = canopy_client::call(&sock, Method::ProjectInit { path: None, url: target, with_scripts, adopt_from: from })?;
                if let ResultBody::ProjectList { projects } = r {
                    for p in projects {
                        println!("added project {} at {}", p.name, p.root.display());
                    }
                }
                Ok(())
            } else {
                let dir = match &target {
                    Some(t) => PathBuf::from(t),
                    None => std::env::current_dir()?,
                };
                let settings = canopy_core::settings::Settings::load(&paths.settings_file()).unwrap_or_default();
                cmd_init(Some(dir.clone()), with_scripts, force, from, &settings.init.adopt_from)?;
                // Register with the server right away so it shows up in the UI.
                if let Ok(sock) = canopy_client::connect(&paths) {
                    let root = target_root(&dir)?;
                    let _ = canopy_client::call(&sock, Method::ProjectAdd { root });
                }
                Ok(())
            }
        }
        Some(Command::Integration { what }) => cmd_integration(&paths, what),
        Some(Command::Pane { what: PaneCommand::ReportAgent { workspace_id, state, source, seq, session_id, agent } }) => {
            let state: canopy_proto::AgentState = serde_json::from_value(serde_json::Value::String(state)).context("state must be idle|working|blocked|done|unknown")?;
            // Hooks must never start a server; if none is running there is nothing to report to.
            let sock = paths.socket();
            if std::os::unix::net::UnixStream::connect(&sock).is_err() {
                return Ok(());
            }
            canopy_client::call(&sock, Method::PaneReportAgent(canopy_proto::AgentReport { workspace_id, source, seq, agent, state, session_id }))?;
            Ok(())
        }
        Some(Command::Sidebar { what: None, width }) => {
            if !std::io::stdout().is_terminal() {
                bail!("canopy sidebar needs a terminal (it runs as a pane inside a canopy session)");
            }
            let sock = canopy_client::connect(&paths)?;
            canopy_client::sidebar::run(&sock, &paths.home, width)
        }
        Some(Command::Sidebar { what: Some(cmd), .. }) => {
            let (session, ensure) = match cmd {
                SidebarCommand::Toggle { session } => (session, false),
                SidebarCommand::Ensure { session } => (session, true),
            };
            let session = match session {
                Some(s) => s,
                None => {
                    let out = std::process::Command::new("tmux").args(["display-message", "-p", "#S"]).output().context("tmux display-message")?;
                    String::from_utf8_lossy(&out.stdout).trim().to_string()
                }
            };
            if session.is_empty() {
                bail!("no tmux session (run inside tmux or pass --session)");
            }
            // Hooks must never start a server.
            let sock = paths.socket();
            if ensure && std::os::unix::net::UnixStream::connect(&sock).is_err() {
                return Ok(());
            }
            let sock = canopy_client::connect(&paths)?;
            let method = if ensure { Method::SessionSidebarEnsure { session } } else { Method::SessionSidebarToggle { session } };
            canopy_client::call(&sock, method)?;
            Ok(())
        }
        Some(Command::Statusline { workspace, .. }) => {
            // Iron rules: never print errors to stdout; escape `#` for tmux.
            print!("{}", statusline(&paths, workspace).unwrap_or_default());
            Ok(())
        }
        Some(Command::Server { what }) => match what {
            None => {
                canopy_server::daemon::init_logging(&paths, "server")?;
                canopy_server::daemon::run(paths)
            }
            Some(ServerCommand::Stop) => {
                let sock = paths.socket();
                if std::os::unix::net::UnixStream::connect(&sock).is_err() {
                    println!("no server running");
                    return Ok(());
                }
                canopy_client::call(&sock, Method::ServerStop)?;
                println!("server stopping");
                Ok(())
            }
            Some(ServerCommand::Status) => {
                let sock = paths.socket();
                if std::os::unix::net::UnixStream::connect(&sock).is_err() {
                    println!("server: not running ({})", sock.display());
                    return Ok(());
                }
                let r = canopy_client::call(&sock, Method::ServerStatus)?;
                println!("{}", serde_json::to_string_pretty(&r)?);
                Ok(())
            }
            Some(ServerCommand::ReloadConfig) => {
                let sock = canopy_client::connect(&paths)?;
                canopy_client::call(&sock, Method::ServerReloadConfig)?;
                println!("config reloaded");
                Ok(())
            }
        },
        Some(Command::Api { what }) => match what {
            ApiCommand::Schema => {
                println!("{}", serde_json::to_string_pretty(&canopy_proto::schema())?);
                Ok(())
            }
            ApiCommand::Ping => {
                let sock = canopy_client::connect(&paths)?;
                let r = canopy_client::call(&sock, Method::Ping)?;
                println!("{}", serde_json::to_string(&r)?);
                Ok(())
            }
            ApiCommand::Call { method, params } => {
                let sock = canopy_client::connect(&paths)?;
                // Unit methods (ping, agent.list, …) take no `params` key at all.
                let envelope = match params {
                    Some(p) => {
                        let v: serde_json::Value = serde_json::from_str(&p).context("params must be JSON")?;
                        serde_json::json!({"method": method, "params": v})
                    }
                    None => serde_json::json!({"method": method}),
                };
                let m: Method = serde_json::from_value(envelope.clone())
                    .or_else(|_| serde_json::from_value(serde_json::json!({"method": method, "params": {}})))
                    .context("unknown method or bad params")?;
                match canopy_client::call_raw(&sock, m, None)? {
                    canopy_proto::Response::Ok { result, .. } => println!("{}", serde_json::to_string_pretty(&result)?),
                    canopy_proto::Response::Err { error, .. } => {
                        eprintln!("{}", serde_json::to_string(&error)?);
                        std::process::exit(1);
                    }
                }
                Ok(())
            }
        },
        Some(Command::DefaultConfig) => {
            print!("{}", canopy_core::settings::Settings::default_toml());
            Ok(())
        }
        Some(Command::Doctor) => {
            let checks = doctor::run(&paths)?;
            if doctor::print(&checks) {
                Ok(())
            } else {
                std::process::exit(1)
            }
        }
        Some(Command::Upgrade { check }) => upgrade::run(check),
        Some(Command::Version) => {
            println!("canopy {} (protocol {})", env!("CARGO_PKG_VERSION"), canopy_proto::PROTOCOL_VERSION);
            println!("home:   {}", paths.home.display());
            println!("socket: {}", paths.socket().display());
            Ok(())
        }
    }
}

// ---------------------------------------------------------------------------------------

fn tui(paths: &Paths) -> Result<()> {
    if !std::io::stdout().is_terminal() {
        bail!("no terminal; try `canopy ls` or `canopy --help`");
    }
    let sock = canopy_client::connect(paths)?;
    let root = project_root_opt(paths);
    let outcome = canopy_client::tui::run(canopy_client::Transport::Local(sock), root)?;
    match outcome {
        canopy_client::tui::Outcome::Quit => Ok(()),
        canopy_client::tui::Outcome::Attach(target) => canopy_client::exec_attach(&target),
    }
}

fn which(prog: &str) -> bool {
    std::env::var_os("PATH").map(|p| std::env::split_paths(&p).any(|d| d.join(prog).is_file())).unwrap_or(false)
}

/// Attach target as seen from this machine for a remote host.
fn remotify(target: AttachTarget, host: &str, mosh: bool) -> AttachTarget {
    match target {
        AttachTarget::Tmux { session, .. } => AttachTarget::RemoteTmux { ssh_target: host.to_string(), session, mosh },
        other => other,
    }
}

/// `canopy --remote <host> [verb]`: the dashboard or a verb, against the remote server.
fn remote_main(host: &str, mosh: bool, command: Option<Command>) -> Result<()> {
    use canopy_client::Transport;
    let t = Transport::Ssh { target: host.to_string() };
    let project_of = |t: &Transport, wanted: Option<&str>| -> Result<PathBuf> {
        let ResultBody::ProjectList { projects } = t.call(Method::ProjectList)? else { bail!("unexpected response") };
        match wanted {
            Some(w) => projects
                .iter()
                .find(|p| p.name == w || p.root.to_string_lossy() == w)
                .map(|p| p.root.clone())
                .ok_or_else(|| anyhow::anyhow!("no project {w:?} on {host}; known: {}", projects.iter().map(|p| p.name.as_str()).collect::<Vec<_>>().join(", "))),
            None if projects.len() == 1 => Ok(projects[0].root.clone()),
            None => bail!("pass --project <name>; projects on {host}: {}", projects.iter().map(|p| p.name.as_str()).collect::<Vec<_>>().join(", ")),
        }
    };
    let named = |t: &Transport, name: &str| -> Result<WorkspaceRef> {
        let ResultBody::WorkspaceList { workspaces } = t.call(Method::WorkspaceList { project_root: None })? else { bail!("unexpected response") };
        let hits: Vec<&WorkspaceRow> = workspaces.iter().filter(|w| w.name == name || w.id == name).collect();
        match hits.len() {
            1 => Ok(WorkspaceRef::Id { id: hits[0].id.clone() }),
            0 => bail!("no workspace {name:?} on {host}"),
            _ => bail!("{name:?} exists in several projects on {host}; use its id: {}", hits.iter().map(|w| format!("{} ({})", w.id, w.project)).collect::<Vec<_>>().join(", ")),
        }
    };
    match command {
        None => {
            if !std::io::stdout().is_terminal() {
                bail!("no terminal; try `canopy --remote {host} ls`");
            }
            match canopy_client::tui::run(t.clone(), None)? {
                canopy_client::tui::Outcome::Quit => Ok(()),
                canopy_client::tui::Outcome::Attach(target) => canopy_client::exec_attach(&remotify(target, host, mosh)),
            }
        }
        Some(Command::Ls { json, .. }) => {
            let ResultBody::WorkspaceList { mut workspaces } = t.call(Method::WorkspaceList { project_root: None })? else { bail!("unexpected response") };
            for w in &mut workspaces {
                w.host = host.to_string();
            }
            if json {
                println!("{}", serde_json::to_string_pretty(&serde_json::json!({"schema_version": 7, "host": host, "workspaces": workspaces}))?);
            } else {
                print_table(&workspaces, true);
            }
            Ok(())
        }
        Some(Command::New(a)) => {
            let root = project_of(&t, a.project.as_deref())?;
            let prompt = match (&a.prompt, &a.prompt_file) {
                (Some(p), _) => Some(p.clone()),
                (None, Some(f)) => Some(std::fs::read_to_string(f)?),
                _ => None,
            };
            eprintln!("creating workspace on {host}…");
            let req = WorkspaceCreate { project_root: root, name: a.name, branch: a.branch, pr: a.pr, issue: a.issue, prompt, agent: a.agent, start_session: Some(true) };
            let ws = canopy_client::livelog::create_streaming(&t, req, &mut std::io::stderr())?;
            eprintln!("{} on branch {} (port {}) @{host}", ws.name, ws.branch, ws.port);
            if a.no_attach {
                println!("{}", ws.name);
                return Ok(());
            }
            let ResultBody::AttachTarget { target } = t.call(Method::WorkspaceAttachTarget { workspace: WorkspaceRef::Id { id: ws.id } })? else { bail!("unexpected response") };
            canopy_client::exec_attach(&remotify(target, host, mosh))
        }
        Some(Command::Switch { name, share }) => {
            if share {
                std::env::set_var("CANOPY_NO_DETACH", "1");
            }
            let r = named(&t, &name)?;
            let ResultBody::AttachTarget { target } = t.call(Method::WorkspaceAttachTarget { workspace: r })? else { bail!("unexpected response") };
            canopy_client::exec_attach(&remotify(target, host, mosh))
        }
        Some(Command::Rm { name, yes, force }) => {
            let r = named(&t, &name)?;
            if !yes {
                eprint!("remove workspace {name} on {host}? [y/N] ");
                if !confirm()? {
                    println!("aborted");
                    return Ok(());
                }
            }
            match t.call_raw(Method::WorkspaceRemove { workspace: r.clone(), force }, None)? {
                canopy_proto::Response::Ok { .. } => println!("removed {name} on {host}"),
                canopy_proto::Response::Err { error, .. } if error.code == canopy_proto::ErrorCode::RemovalBlocked => {
                    eprintln!("{}", error.message);
                    eprint!("remove anyway? [y/N] ");
                    if !confirm()? {
                        println!("aborted");
                        return Ok(());
                    }
                    t.call(Method::WorkspaceRemove { workspace: r, force: true })?;
                    println!("removed {name} on {host}");
                }
                canopy_proto::Response::Err { error, .. } => return Err(canopy_client::ApiFailure(error).into()),
            }
            Ok(())
        }
        Some(Command::Stop { name }) => {
            t.call(Method::WorkspaceStop { workspace: named(&t, &name)? })?;
            println!("stopped {name} on {host}");
            Ok(())
        }
        Some(Command::Retry { name, force }) => {
            eprintln!("re-running setup for {name} on {host}…");
            let ws = canopy_client::livelog::retry_streaming(&t, named(&t, &name)?, force, &mut std::io::stderr())?;
            println!("{} is {} on {host}", ws.name, status_word(&ws));
            Ok(())
        }
        Some(Command::Log { name, follow, lines }) => {
            let Some(name) = name else { bail!("--remote needs a workspace name: canopy --remote {host} log <name>") };
            canopy_client::livelog::print_log(&t, named(&t, &name)?, follow, lines, &mut std::io::stdout())
        }
        Some(Command::Reconcile) => {
            let ResultBody::WorkspaceList { workspaces } = t.call(Method::WorkspaceReconcile { project_root: None })? else { bail!("unexpected response") };
            print_table(&workspaces, true);
            Ok(())
        }
        Some(Command::Init { target, with_scripts, from, .. }) => {
            let Some(target) = target else { bail!("`canopy --remote {host} init <path-or-url>` needs a path on {host} or a git URL") };
            let is_url = target.contains("://") || (target.contains('@') && target.contains(':'));
            let (path, url) = if is_url { (None, Some(target)) } else { (Some(PathBuf::from(target)), None) };
            let ResultBody::ProjectList { projects } = t.call(Method::ProjectInit { path, url, with_scripts, adopt_from: from })? else { bail!("unexpected response") };
            for p in projects {
                println!("added project {} at {} on {host}", p.name, p.root.display());
            }
            Ok(())
        }
        Some(Command::Main) => bail!("`canopy --remote {host} main` needs a project: use `canopy --remote {host} new --project <name>` or attach from the dashboard"),
        Some(Command::Upgrade { .. }) => bail!("to upgrade canopy on {host}, copy this machine's binary over: canopy --remote {host} --install  (after upgrading locally)"),
        Some(Command::Doctor) => {
            let ResultBody::Pong { version, .. } = t.call(Method::Ping)? else { bail!("unexpected response") };
            println!("canopy {version} is answering on {host}; run `canopy doctor` there for the full check");
            Ok(())
        }
        Some(Command::Api { what: ApiCommand::Ping }) => {
            println!("{}", serde_json::to_string(&t.call(Method::Ping)?)?);
            Ok(())
        }
        Some(Command::Api { what: ApiCommand::Call { method, params } }) => {
            let envelope = match params {
                Some(p) => serde_json::json!({"method": method, "params": serde_json::from_str::<serde_json::Value>(&p).context("params must be JSON")?}),
                None => serde_json::json!({"method": method}),
            };
            let m: Method = serde_json::from_value(envelope).context("unknown method or bad params")?;
            println!("{}", serde_json::to_string_pretty(&t.call_raw(m, None)?)?);
            Ok(())
        }
        Some(Command::Server { what: Some(ServerCommand::Status) }) => {
            println!("{}", serde_json::to_string_pretty(&t.call(Method::ServerStatus)?)?);
            Ok(())
        }
        Some(_) => bail!("that command is not available with --remote yet"),
    }
}

/// Project root for the cwd: the main repo root (even from inside a worktree).
fn project_root(paths: &Paths) -> Result<PathBuf> {
    project_root_opt(paths).ok_or_else(|| anyhow::anyhow!("not inside a canopy project (no {FILE_NAME} found up from here). Run `canopy init`."))
}

/// Walk up for `canopy.json`; failing that, use the main repo root (a worktree whose
/// `canopy.json` is not committed yet still belongs to the project that has one).
fn project_root_opt(_paths: &Paths) -> Option<PathBuf> {
    let cwd = std::env::current_dir().ok()?;
    if let Ok((root, _)) = ProjectConfig::discover(&cwd) {
        return Some(canopy_server::manager::canonical_root(&root).unwrap_or(root));
    }
    let root = canopy_server::manager::canonical_root(&cwd).ok()?;
    root.join(FILE_NAME).is_file().then_some(root)
}

fn ws_ref(paths: &Paths, name: &str) -> Result<WorkspaceRef> {
    if name.starts_with('w') && name.len() == 4 && name[1..].chars().all(|c| c.is_ascii_alphanumeric()) && !name.contains('-') {
        return Ok(WorkspaceRef::Id { id: name.to_string() });
    }
    Ok(WorkspaceRef::Named { project_root: project_root(paths)?, name: name.to_string() })
}

/// The workspace we are "in": `CANOPY_WORKSPACE_ID` (set in every canopy session), else
/// the registered workspace whose path contains the cwd.
fn current_ws_ref(paths: &Paths) -> Result<WorkspaceRef> {
    if let Ok(id) = std::env::var("CANOPY_WORKSPACE_ID") {
        return Ok(WorkspaceRef::Id { id });
    }
    let cwd = std::env::current_dir()?.canonicalize()?;
    let sock = canopy_client::connect(paths)?;
    if let ResultBody::WorkspaceList { workspaces } = canopy_client::call(&sock, Method::WorkspaceList { project_root: None })? {
        if let Some(w) = workspaces.iter().find(|w| cwd.starts_with(&w.path)) {
            return Ok(WorkspaceRef::Id { id: w.id.clone() });
        }
    }
    bail!("not inside a workspace (run from a workspace directory or a canopy session)")
}

fn attach_target(sock: &Path, r: &WorkspaceRef) -> Result<AttachTarget> {
    match canopy_client::call(sock, Method::WorkspaceAttachTarget { workspace: r.clone() })? {
        ResultBody::AttachTarget { target } => Ok(target),
        other => bail!("unexpected response {other:?}"),
    }
}

fn cmd_new(paths: &Paths, a: NewArgs) -> Result<()> {
    let root = project_root(paths)?;
    let prompt = match (&a.prompt, &a.prompt_file) {
        (Some(p), _) => Some(p.clone()),
        (None, Some(f)) => {
            let text = std::fs::read_to_string(f).with_context(|| format!("read {}", f.display()))?;
            if text.len() > 32 * 1024 {
                bail!("prompt file exceeds 32 KB; refusing to truncate silently");
            }
            Some(text)
        }
        _ => None,
    };
    let sock = canopy_client::connect(paths)?;
    eprintln!("creating workspace…");
    let req = WorkspaceCreate { project_root: root, name: a.name, branch: a.branch, pr: a.pr, issue: a.issue, prompt, agent: a.agent, start_session: Some(true) };
    let t = canopy_client::Transport::Local(sock.clone());
    let ws = canopy_client::livelog::create_streaming(&t, req, &mut std::io::stderr())?;
    eprintln!("{} on branch {} at {} (port {})", ws.name, ws.branch, ws.path.display(), ws.port);
    if a.no_attach {
        println!("{}", ws.name);
        return Ok(());
    }
    let target = attach_target(&sock, &WorkspaceRef::Id { id: ws.id.clone() })?;
    canopy_client::exec_attach(&target)
}

fn cmd_ls(paths: &Paths, all: bool, json: bool) -> Result<()> {
    let sock = canopy_client::connect(paths)?;
    let root = if all { None } else { project_root_opt(paths) };
    if !all && root.is_none() {
        bail!("not inside a canopy project; use `canopy ls --all`");
    }
    let rows = match canopy_client::call(&sock, Method::WorkspaceList { project_root: root })? {
        ResultBody::WorkspaceList { workspaces } => workspaces,
        other => bail!("unexpected response {other:?}"),
    };
    if json {
        let out = serde_json::json!({
            "schema_version": 7,
            "canopy_version": env!("CARGO_PKG_VERSION"),
            "hostname": hostname(),
            "generated_at": canopy_server::scripts::chrono_like_now(),
            "workspaces": rows,
        });
        println!("{}", serde_json::to_string_pretty(&out)?);
    } else {
        print_table(&rows, all);
    }
    Ok(())
}

fn hostname() -> String {
    std::fs::read_to_string("/etc/hostname").map(|s| s.trim().to_string()).unwrap_or_default()
}

fn status_word(w: &WorkspaceRow) -> &'static str {
    w.status.as_str()
}

fn print_table(rows: &[WorkspaceRow], show_project: bool) {
    if rows.is_empty() {
        println!("(no workspaces)");
        return;
    }
    let agent = |w: &WorkspaceRow| match w.agent_state {
        canopy_proto::AgentState::Working => "⚡",
        canopy_proto::AgentState::Idle => "💤",
        canopy_proto::AgentState::Blocked => "✋",
        canopy_proto::AgentState::Done => "✓",
        canopy_proto::AgentState::Unknown => if w.alive { "·" } else { " " },
    };
    let live = |w: &WorkspaceRow| if w.attached { "⊙" } else if w.alive { "●" } else { "○" };
    for w in rows {
        let hints: Vec<&str> = w.hints.iter().map(|h| h.message.as_str()).collect();
        let proj = if show_project { format!("{:<14} ", w.project) } else { String::new() };
        println!(
            "{} {} {proj}{:<22} {:<28} {:<10} :{:<5} {}",
            live(w),
            agent(w),
            w.name,
            w.branch,
            w.status.as_str(),
            w.port,
            hints.join("  ")
        );
    }
}

fn cmd_rm(paths: &Paths, name: &str, yes: bool, force: bool) -> Result<()> {
    let sock = canopy_client::connect(paths)?;
    let r = ws_ref(paths, name)?;
    if !yes {
        eprint!("remove workspace {name}? This deletes the worktree and branch. [y/N] ");
        if !confirm()? {
            println!("aborted");
            return Ok(());
        }
    }
    match canopy_client::call_raw(&sock, Method::WorkspaceRemove { workspace: r.clone(), force }, None)? {
        canopy_proto::Response::Ok { .. } => {
            println!("removed {name}");
            Ok(())
        }
        canopy_proto::Response::Err { error, .. } if error.code == canopy_proto::ErrorCode::RemovalBlocked => {
            eprintln!("{}", error.message);
            if yes && !std::io::stdin().is_terminal() {
                bail!("refusing without --force");
            }
            eprint!("remove anyway? [y/N] ");
            if !confirm()? {
                println!("aborted");
                return Ok(());
            }
            canopy_client::call(&sock, Method::WorkspaceRemove { workspace: r, force: true })?;
            println!("removed {name}");
            Ok(())
        }
        canopy_proto::Response::Err { error, .. } => Err(canopy_client::ApiFailure(error).into()),
    }
}

fn confirm() -> Result<bool> {
    let mut s = String::new();
    std::io::stdin().read_line(&mut s)?;
    Ok(matches!(s.trim(), "y" | "Y" | "yes"))
}

fn target_root(dir: &Path) -> Result<PathBuf> {
    Ok(canopy_server::git::root(dir)?)
}

fn cmd_init(path: Option<PathBuf>, with_scripts: bool, force: bool, from: Option<PathBuf>, candidates: &[String]) -> Result<()> {
    let dir = match path {
        Some(p) => p,
        None => std::env::current_dir()?,
    };
    let dir = dir.canonicalize().with_context(|| format!("{} does not exist", dir.display()))?;
    if !canopy_server::git::is_repo(&dir) {
        bail!("{} is not a git repository", dir.display());
    }
    let root = canopy_server::git::root(&dir)?;
    let file = root.join(FILE_NAME);
    if file.exists() && !force {
        bail!("{} already exists (use --force to overwrite)", file.display());
    }
    let source = from.map(|p| if p.is_absolute() { p } else { root.join(p) }).or_else(|| ProjectConfig::find_adoptable(&root, candidates));
    let mut cfg = match source {
        Some(f) => {
            let c = ProjectConfig::adopt_from(&f)?;
            eprintln!("adopted scripts from {}", f.display());
            c
        }
        None => ProjectConfig::default(),
    };
    if with_scripts {
        canopy_server::manager::write_stub_scripts(&root, &mut cfg)?;
    }
    std::fs::write(&file, cfg.to_json_pretty() + "\n")?;
    println!("wrote {}", file.display());
    println!("next: `canopy new` from inside the repo");
    Ok(())
}

fn cmd_integration(paths: &Paths, what: IntegrationCommand) -> Result<()> {
    use canopy_server::integration as integ;
    match what {
        IntegrationCommand::Install { agent } => {
            let bin = std::env::current_exe()?;
            let file = integ::install(paths, &agent, &bin)?;
            println!("installed {agent} hooks in {} (script: {})", file.display(), integ::script_path(paths).display());
            Ok(())
        }
        IntegrationCommand::Uninstall { agent } => {
            let file = integ::uninstall(paths, &agent)?;
            println!("removed {agent} hooks from {}", file.display());
            Ok(())
        }
        IntegrationCommand::List => {
            for i in integ::INTEGRATIONS {
                let on = integ::status(i.agent).unwrap_or(false);
                println!("{:<10} {}", i.agent, if on { "installed" } else { "not installed" });
            }
            Ok(())
        }
    }
}

/// `<project> / <branch> ● :<port>` with `#` escaped. Errors become empty output.
fn statusline(paths: &Paths, workspace: Option<String>) -> Option<String> {
    let id = workspace.or_else(|| std::env::var("CANOPY_WORKSPACE_ID").ok())?;
    let sock = paths.socket();
    std::os::unix::net::UnixStream::connect(&sock).ok()?;
    let row = match canopy_client::call_raw(&sock, Method::WorkspaceGet { workspace: WorkspaceRef::Id { id } }, Some(std::time::Duration::from_millis(800))).ok()? {
        canopy_proto::Response::Ok { result: ResultBody::Workspace { workspace }, .. } => workspace,
        _ => return None,
    };
    let esc = |s: &str| s.replace('#', "##");
    let mut out = format!("{} / {}", esc(&row.project), esc(&row.branch));
    if row.name != row.branch {
        out = format!("{} / {}", esc(&row.name), esc(&row.branch));
    }
    out.push_str(&format!(" ● :{}", row.port));
    if let Ok(host) = std::env::var("CANOPY_REMOTE_HOST") {
        out = format!("@{} {out}", esc(&host));
    }
    Some(out)
}
