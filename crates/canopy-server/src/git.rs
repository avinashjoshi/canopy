//! Git operations the server performs by shelling out. Thin, explicit wrappers; every
//! error carries git's stderr.

use std::path::{Path, PathBuf};
use std::process::Command;

#[derive(Debug, thiserror::Error)]
pub enum GitError {
    #[error("git {args}: {stderr}")]
    Failed { args: String, stderr: String },
    #[error("not a git repository: {0}")]
    NotARepo(PathBuf),
    #[error("branch {0:?} is already checked out in another worktree")]
    BranchInUse(String),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
}

fn run(cwd: &Path, args: &[&str]) -> Result<String, GitError> {
    let out = Command::new("git").arg("-C").arg(cwd).args(args).output()?;
    if out.status.success() {
        Ok(String::from_utf8_lossy(&out.stdout).trim_end().to_string())
    } else {
        let stderr = String::from_utf8_lossy(&out.stderr).trim().to_string();
        if stderr.contains("is already checked out") || stderr.contains("already used by worktree") {
            let branch = args.iter().rev().find(|a| !a.starts_with('-')).unwrap_or(&"").to_string();
            return Err(GitError::BranchInUse(branch));
        }
        Err(GitError::Failed { args: args.join(" "), stderr })
    }
}

pub fn is_repo(path: &Path) -> bool {
    run(path, &["rev-parse", "--is-inside-work-tree"]).map(|s| s == "true").unwrap_or(false)
}

/// Canonical repo root: the *main* worktree's top level, even when `path` is inside a
/// linked worktree (`--git-common-dir` points at the shared `.git`).
pub fn root(path: &Path) -> Result<PathBuf, GitError> {
    if !is_repo(path) {
        return Err(GitError::NotARepo(path.to_path_buf()));
    }
    let common = run(path, &["rev-parse", "--git-common-dir"])?;
    let common = if Path::new(&common).is_absolute() { PathBuf::from(common) } else { path.join(common) };
    let common = common.canonicalize().unwrap_or(common);
    match common.file_name().map(|n| n == ".git") {
        Some(true) => Ok(common.parent().map(Path::to_path_buf).unwrap_or(common)),
        // Bare repo or unusual layout: fall back to this worktree's toplevel.
        _ => Ok(PathBuf::from(run(path, &["rev-parse", "--show-toplevel"])?)),
    }
}

/// `origin`'s HEAD branch (e.g. `main`), falling back to `main`/`master` detection.
pub fn default_branch(repo: &Path) -> String {
    if let Ok(s) = run(repo, &["symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"]) {
        if let Some(b) = s.strip_prefix("origin/") {
            return b.to_string();
        }
    }
    for b in ["main", "master"] {
        if run(repo, &["show-ref", "--verify", "--quiet", &format!("refs/remotes/origin/{b}")]).is_ok()
            || run(repo, &["show-ref", "--verify", "--quiet", &format!("refs/heads/{b}")]).is_ok()
        {
            return b.to_string();
        }
    }
    "main".to_string()
}

/// False for a freshly `git init`ed repository (no HEAD to branch from).
pub fn has_commits(repo: &Path) -> bool {
    run(repo, &["rev-parse", "--verify", "--quiet", "HEAD"]).is_ok()
}

pub fn has_remote(repo: &Path, name: &str) -> bool {
    run(repo, &["remote", "get-url", name]).is_ok()
}

/// Best-effort `git fetch origin`; errors are returned so callers can log, not fail.
pub fn fetch(repo: &Path) -> Result<(), GitError> {
    run(repo, &["fetch", "--quiet", "origin"]).map(|_| ())
}

pub fn branch_exists(repo: &Path, branch: &str) -> bool {
    run(repo, &["show-ref", "--verify", "--quiet", &format!("refs/heads/{branch}")]).is_ok()
}

pub fn remote_branch_exists(repo: &Path, branch: &str) -> bool {
    run(repo, &["show-ref", "--verify", "--quiet", &format!("refs/remotes/origin/{branch}")]).is_ok()
}

/// `git worktree add`. With `create_branch`, `-b <branch>` from `start_point`; otherwise
/// check out the existing branch (creating a local tracking branch from origin if needed).
pub fn worktree_add(repo: &Path, path: &Path, branch: &str, create_branch: bool, start_point: &str) -> Result<(), GitError> {
    let path_s = path.display().to_string();
    if create_branch {
        run(repo, &["worktree", "add", "-b", branch, &path_s, start_point]).map(|_| ())
    } else if branch_exists(repo, branch) {
        run(repo, &["worktree", "add", &path_s, branch]).map(|_| ())
    } else {
        // Not local yet: create a tracking branch from origin.
        run(repo, &["worktree", "add", "--track", "-b", branch, &path_s, &format!("origin/{branch}")]).map(|_| ())
    }
}

pub fn worktree_remove(repo: &Path, path: &Path, force: bool) -> Result<(), GitError> {
    let path_s = path.display().to_string();
    let mut args = vec!["worktree", "remove"];
    if force {
        args.push("--force");
    }
    args.push(&path_s);
    run(repo, &args).map(|_| ())?;
    let _ = run(repo, &["worktree", "prune"]);
    Ok(())
}

pub fn branch_delete(repo: &Path, branch: &str) -> Result<(), GitError> {
    run(repo, &["branch", "-D", branch]).map(|_| ())
}

/// Current branch of a worktree, or `None` on detached HEAD.
pub fn current_branch(worktree: &Path) -> Result<Option<String>, GitError> {
    let s = run(worktree, &["symbolic-ref", "--quiet", "--short", "HEAD"]);
    match s {
        Ok(b) => Ok(Some(b)),
        Err(GitError::Failed { .. }) => Ok(None),
        Err(e) => Err(e),
    }
}

/// Git state a workspace can be stuck in.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StuckState {
    Rebasing,
    Merging,
    CherryPicking,
    Detached,
}

pub fn stuck_state(worktree: &Path) -> Option<StuckState> {
    let gitdir = run(worktree, &["rev-parse", "--git-dir"]).ok()?;
    let g = if Path::new(&gitdir).is_absolute() { PathBuf::from(gitdir) } else { worktree.join(gitdir) };
    if g.join("rebase-merge").exists() || g.join("rebase-apply").exists() {
        return Some(StuckState::Rebasing);
    }
    if g.join("MERGE_HEAD").exists() {
        return Some(StuckState::Merging);
    }
    if g.join("CHERRY_PICK_HEAD").exists() {
        return Some(StuckState::CherryPicking);
    }
    if current_branch(worktree).ok().flatten().is_none() {
        return Some(StuckState::Detached);
    }
    None
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Stats {
    /// Commits ahead of / behind `origin/<default>`.
    pub ahead: u32,
    pub behind: u32,
    /// Tracked files with modifications (untracked excluded on purpose: build noise).
    pub dirty_tracked: u32,
    pub untracked: u32,
    /// Commits not on the branch's upstream (`None` when no upstream).
    pub unpushed: Option<u32>,
    pub upstream_diverged: bool,
    pub conflicted: u32,
}

pub fn stats(worktree: &Path, default_branch: &str) -> Stats {
    let mut s = Stats::default();
    if let Ok(out) = run(worktree, &["rev-list", "--left-right", "--count", &format!("HEAD...origin/{default_branch}")]) {
        let mut it = out.split_whitespace();
        s.ahead = it.next().and_then(|x| x.parse().ok()).unwrap_or(0);
        s.behind = it.next().and_then(|x| x.parse().ok()).unwrap_or(0);
    }
    if let Ok(out) = run(worktree, &["status", "--porcelain=v1", "--untracked-files=normal"]) {
        for line in out.lines() {
            let code = line.get(..2).unwrap_or("");
            if code == "??" {
                s.untracked += 1;
            } else if code.contains('U') || code == "AA" || code == "DD" {
                s.conflicted += 1;
                s.dirty_tracked += 1;
            } else if !code.trim().is_empty() {
                s.dirty_tracked += 1;
            }
        }
    }
    if let Ok(out) = run(worktree, &["rev-list", "--left-right", "--count", "HEAD...@{upstream}"]) {
        let mut it = out.split_whitespace();
        let ahead: u32 = it.next().and_then(|x| x.parse().ok()).unwrap_or(0);
        let behind: u32 = it.next().and_then(|x| x.parse().ok()).unwrap_or(0);
        s.unpushed = Some(ahead);
        s.upstream_diverged = behind > 0;
    }
    s
}

/// Upstream branch of the worktree's HEAD, if configured and still present on the remote.
pub fn upstream_exists(worktree: &Path) -> Option<bool> {
    let name = run(worktree, &["rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"]).ok()?;
    Some(run(worktree, &["show-ref", "--verify", "--quiet", &format!("refs/remotes/{name}")]).is_ok())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn init_repo() -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path();
        run(p, &["init", "-q", "-b", "main"]).unwrap();
        run(p, &["config", "user.email", "t@t"]).unwrap();
        run(p, &["config", "user.name", "t"]).unwrap();
        std::fs::write(p.join("a.txt"), "a\n").unwrap();
        run(p, &["add", "."]).unwrap();
        run(p, &["commit", "-q", "-m", "init"]).unwrap();
        dir
    }

    #[test]
    fn worktree_roundtrip() {
        let repo = init_repo();
        let wt = tempfile::tempdir().unwrap();
        let path = wt.path().join("ws");
        worktree_add(repo.path(), &path, "fix-tz", true, "main").unwrap();
        assert_eq!(current_branch(&path).unwrap().as_deref(), Some("fix-tz"));
        assert_eq!(root(&path).unwrap().canonicalize().unwrap(), repo.path().canonicalize().unwrap());
        // Same branch in a second worktree is refused with a typed error.
        let path2 = wt.path().join("ws2");
        assert!(matches!(worktree_add(repo.path(), &path2, "fix-tz", false, "main"), Err(GitError::BranchInUse(_))));
        std::fs::write(path.join("a.txt"), "b\n").unwrap();
        std::fs::write(path.join("junk"), "x\n").unwrap();
        let st = stats(&path, "main");
        assert_eq!(st.dirty_tracked, 1);
        assert_eq!(st.untracked, 1);
        assert_eq!(st.unpushed, None);
        assert_eq!(stuck_state(&path), None);
        worktree_remove(repo.path(), &path, true).unwrap();
        assert!(!path.exists());
        branch_delete(repo.path(), "fix-tz").unwrap();
        assert!(!branch_exists(repo.path(), "fix-tz"));
    }

    #[test]
    fn has_commits_detects_empty_repo() {
        let empty = tempfile::tempdir().unwrap();
        run(empty.path(), &["init", "-q", "-b", "main"]).unwrap();
        assert!(!has_commits(empty.path()));
        let repo = init_repo();
        assert!(has_commits(repo.path()));
    }

    #[test]
    fn default_branch_without_origin_falls_back() {
        let repo = init_repo();
        assert_eq!(default_branch(repo.path()), "main");
        assert!(!has_remote(repo.path(), "origin"));
        assert!(!is_repo(Path::new("/")));
    }
}
