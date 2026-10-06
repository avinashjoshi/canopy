//! `canopy upgrade`: replace this binary with the latest GitHub release and restart the
//! server. Uses the system `curl` and `tar` (present everywhere canopy runs) so the binary
//! carries no TLS stack of its own; the archive checksum is verified here before anything
//! is replaced.

use anyhow::{bail, Context, Result};
use sha2::{Digest, Sha256};
use std::cmp::Ordering;
use std::path::{Path, PathBuf};
use std::process::Command;

pub const REPO: &str = "avinashjoshi/canopy";
pub const CURRENT: &str = env!("CARGO_PKG_VERSION");

/// The release target for this machine, matching the names `release.yml` publishes.
pub fn target_triple() -> Option<&'static str> {
    target_for(std::env::consts::OS, std::env::consts::ARCH)
}

pub fn target_for(os: &str, arch: &str) -> Option<&'static str> {
    Some(match (os, arch) {
        ("linux", "x86_64") => "x86_64-unknown-linux-musl",
        ("linux", "aarch64") => "aarch64-unknown-linux-musl",
        ("macos", "x86_64") => "x86_64-apple-darwin",
        ("macos", "aarch64") => "aarch64-apple-darwin",
        _ => return None,
    })
}

pub fn asset_name(tag: &str, target: &str) -> String {
    format!("canopy-{tag}-{target}.tar.gz")
}

/// Tag of the latest release, read from the `/releases/latest` redirect (no API, no rate
/// limit, works for anonymous users).
pub fn latest_tag() -> Result<String> {
    let out = Command::new("curl")
        .args(["-fsS", "-o", "/dev/null", "-w", "%{redirect_url}", &format!("https://github.com/{REPO}/releases/latest")])
        .output()
        .context("run curl")?;
    if !out.status.success() {
        bail!("could not reach github.com: {}", String::from_utf8_lossy(&out.stderr).trim());
    }
    tag_from_redirect(String::from_utf8_lossy(&out.stdout).trim()).ok_or_else(|| anyhow::anyhow!("no releases published for {REPO} yet"))
}

pub fn tag_from_redirect(url: &str) -> Option<String> {
    let (_, tag) = url.rsplit_once("/tag/")?;
    let tag = tag.trim_end_matches('/');
    (!tag.is_empty()).then(|| tag.to_string())
}

/// Semver-ish ordering: numeric dotted components, then a pre-release suffix sorts below
/// the release (`1.0.0-beta.2 < 1.0.0`), pre-releases compared piecewise.
pub fn compare(a: &str, b: &str) -> Ordering {
    fn split(v: &str) -> (Vec<u64>, Option<Vec<String>>) {
        let v = v.trim().trim_start_matches('v');
        let (core, pre) = match v.split_once('-') {
            Some((c, p)) => (c, Some(p)),
            None => (v, None),
        };
        let nums = core.split('.').map(|p| p.parse::<u64>().unwrap_or(0)).collect();
        (nums, pre.map(|p| p.split('.').map(|s| s.to_string()).collect()))
    }
    let (an, ap) = split(a);
    let (bn, bp) = split(b);
    let len = an.len().max(bn.len());
    for i in 0..len {
        let (x, y) = (an.get(i).copied().unwrap_or(0), bn.get(i).copied().unwrap_or(0));
        match x.cmp(&y) {
            Ordering::Equal => {}
            o => return o,
        }
    }
    match (ap, bp) {
        (None, None) => Ordering::Equal,
        (None, Some(_)) => Ordering::Greater,
        (Some(_), None) => Ordering::Less,
        (Some(x), Some(y)) => {
            for (p, q) in x.iter().zip(y.iter()) {
                let o = match (p.parse::<u64>(), q.parse::<u64>()) {
                    (Ok(pn), Ok(qn)) => pn.cmp(&qn),
                    _ => p.cmp(q),
                };
                if o != Ordering::Equal {
                    return o;
                }
            }
            x.len().cmp(&y.len())
        }
    }
}

/// A binary under a cargo `target/` directory is a development build: never overwrite it.
pub fn is_dev_build(exe: &Path) -> bool {
    exe.components().any(|c| c.as_os_str() == "target") && (exe.to_string_lossy().contains("/release/") || exe.to_string_lossy().contains("/debug/"))
}

pub fn run(check_only: bool) -> Result<()> {
    let tag = latest_tag()?;
    let latest = tag.trim_start_matches('v');
    match compare(latest, CURRENT) {
        Ordering::Greater => {}
        _ => {
            println!("canopy {CURRENT} is up to date (latest release: {tag})");
            return Ok(());
        }
    }
    if check_only {
        println!("canopy {latest} is available (you have {CURRENT}). Run: canopy upgrade");
        return Ok(());
    }

    let exe = std::env::current_exe().context("locate this binary")?;
    let exe = exe.canonicalize().unwrap_or(exe);
    if is_dev_build(&exe) {
        bail!("{} is a development build (under target/). Upgrade it with git pull && cargo build --release, or install a release with install.sh.", exe.display());
    }
    let Some(target) = target_triple() else {
        bail!("no prebuilt binary for {}/{}; build from source: cargo install --git https://github.com/{REPO} canopy", std::env::consts::OS, std::env::consts::ARCH)
    };
    let dir = exe.parent().map(Path::to_path_buf).unwrap_or_else(|| PathBuf::from("."));
    let asset = asset_name(&tag, target);
    let base = format!("https://github.com/{REPO}/releases/download/{tag}");

    let tmp = std::env::temp_dir().join(format!("canopy-upgrade-{}", std::process::id()));
    std::fs::create_dir_all(&tmp)?;
    let cleanup = Cleanup(tmp.clone());

    eprintln!("downloading canopy {tag} ({target})…");
    let archive = tmp.join(&asset);
    curl(&format!("{base}/{asset}"), &archive)?;
    let sums = tmp.join(format!("{asset}.sha256"));
    curl(&format!("{base}/{asset}.sha256"), &sums)?;
    let expected = std::fs::read_to_string(&sums)?.split_whitespace().next().unwrap_or_default().to_ascii_lowercase();
    let actual = sha256_hex(&std::fs::read(&archive)?);
    if expected.is_empty() || expected != actual {
        bail!("checksum mismatch for {asset}: expected {expected}, got {actual}");
    }

    let st = Command::new("tar").args(["-xzf"]).arg(&archive).arg("-C").arg(&tmp).status().context("run tar")?;
    if !st.success() {
        bail!("tar failed to extract {asset}");
    }
    let fresh = tmp.join("canopy");
    if !fresh.is_file() {
        bail!("archive did not contain a canopy binary");
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&fresh, std::fs::Permissions::from_mode(0o755))?;
    }

    // Copy next to the old binary, then rename over it: atomic on the same filesystem, and
    // processes still running the old binary keep their inode until they exit.
    let staged = dir.join("canopy.new");
    std::fs::copy(&fresh, &staged).with_context(|| format!("write {} (is {} writable?)", staged.display(), dir.display()))?;
    std::fs::rename(&staged, &exe).with_context(|| format!("replace {}", exe.display()))?;
    drop(cleanup);

    // The old server keeps running old code until stopped; the next command starts the new
    // one, and it respawns the sidebars itself.
    let _ = Command::new(&exe).args(["server", "stop"]).stdout(std::process::Stdio::null()).stderr(std::process::Stdio::null()).status();
    let _ = Command::new(&exe).args(["api", "ping"]).stdout(std::process::Stdio::null()).stderr(std::process::Stdio::null()).status();
    println!("upgraded canopy {CURRENT} → {latest} at {}; server restarted, sessions untouched", exe.display());
    Ok(())
}

fn curl(url: &str, to: &Path) -> Result<()> {
    let out = Command::new("curl").args(["-fsSL", "--retry", "3", "-o"]).arg(to).arg(url).output().context("run curl")?;
    if !out.status.success() {
        bail!("download failed: {url}: {}", String::from_utf8_lossy(&out.stderr).trim());
    }
    Ok(())
}

pub fn sha256_hex(data: &[u8]) -> String {
    let mut h = Sha256::new();
    h.update(data);
    h.finalize().iter().map(|b| format!("{b:02x}")).collect()
}

struct Cleanup(PathBuf);
impl Drop for Cleanup {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn version_ordering() {
        assert_eq!(compare("1.0.0", "1.0.0"), Ordering::Equal);
        assert_eq!(compare("v1.0.1", "1.0.0"), Ordering::Greater);
        assert_eq!(compare("1.0.0", "1.0.0-alpha.0"), Ordering::Greater, "release beats pre-release");
        assert_eq!(compare("1.0.0-beta.1", "1.0.0-alpha.0"), Ordering::Greater);
        assert_eq!(compare("1.0.0-beta.2", "1.0.0-beta.10"), Ordering::Less, "numeric pre-release parts");
        assert_eq!(compare("1.2", "1.2.0"), Ordering::Equal);
        assert_eq!(compare("0.9.9", "1.0.0-alpha.0"), Ordering::Less);
    }

    #[test]
    fn redirect_tag_and_asset_names() {
        assert_eq!(tag_from_redirect("https://github.com/o/r/releases/tag/v1.2.3").as_deref(), Some("v1.2.3"));
        assert_eq!(tag_from_redirect("https://github.com/o/r/releases/tag/v1.2.3/").as_deref(), Some("v1.2.3"));
        assert_eq!(tag_from_redirect("https://github.com/o/r/releases"), None);
        assert_eq!(tag_from_redirect(""), None);
        assert_eq!(asset_name("v1.2.3", "x86_64-unknown-linux-musl"), "canopy-v1.2.3-x86_64-unknown-linux-musl.tar.gz");
    }

    #[test]
    fn targets_match_the_release_matrix() {
        assert_eq!(target_for("linux", "x86_64"), Some("x86_64-unknown-linux-musl"));
        assert_eq!(target_for("linux", "aarch64"), Some("aarch64-unknown-linux-musl"));
        assert_eq!(target_for("macos", "aarch64"), Some("aarch64-apple-darwin"));
        assert_eq!(target_for("macos", "x86_64"), Some("x86_64-apple-darwin"));
        assert_eq!(target_for("windows", "x86_64"), None);
    }

    #[test]
    fn dev_builds_are_recognised() {
        assert!(is_dev_build(Path::new("/home/u/Work/canopy/target/release/canopy")));
        assert!(is_dev_build(Path::new("/home/u/Work/canopy/target/debug/canopy")));
        assert!(!is_dev_build(Path::new("/home/u/.local/bin/canopy")));
        assert!(!is_dev_build(Path::new("/home/u/target/canopy")));
    }

    #[test]
    fn sha256_matches_known_vector() {
        assert_eq!(sha256_hex(b""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
        assert_eq!(sha256_hex(b"abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    }
}
