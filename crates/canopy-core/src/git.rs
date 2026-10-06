//! Git plumbing that does not need a process: branch-name sanitization. Command wrappers
//! (worktree add/remove, fetch, default-branch detection) live in the server since they
//! are async process launches.

/// Turn an arbitrary string into a valid git ref component. Rules follow
/// `git check-ref-format --branch`: no spaces or control chars, none of `~ ^ : ? * [ \`,
/// no `..`, no leading `-` or `.`, no trailing `.` or `/`, no `.lock` suffix, no `@{`,
/// no consecutive slashes. Returns `None` if nothing valid remains.
pub fn sanitize_branch(input: &str) -> Option<String> {
    let mut out = String::with_capacity(input.len());
    let mut last_dash = false;
    for c in input.trim().chars() {
        let mapped = match c {
            c if c.is_whitespace() || c.is_control() => '-',
            '~' | '^' | ':' | '?' | '*' | '[' | '\\' => '-',
            c => c,
        };
        if mapped == '-' {
            if !last_dash && !out.is_empty() {
                out.push('-');
            }
            last_dash = true;
        } else {
            out.push(mapped);
            last_dash = false;
        }
    }
    // Collapse `..`, `@{`, `//`.
    while out.contains("..") { out = out.replace("..", "."); }
    out = out.replace("@{", "-");
    while out.contains("//") { out = out.replace("//", "/"); }
    // Each slash-separated component: no leading `.`, no `.lock` suffix.
    let comps: Vec<String> = out
        .split('/')
        .map(|c| {
            let c = c.trim_start_matches('.').trim_end_matches('.');
            let c = c.strip_suffix(".lock").unwrap_or(c);
            c.to_string()
        })
        .filter(|c| !c.is_empty())
        .collect();
    let mut joined = comps.join("/");
    joined = joined.trim_matches(|c| c == '-' || c == '.' || c == '/').to_string();
    if joined.is_empty() || joined == "@" { None } else { Some(joined) }
}

/// Session/path-safe form of a branch or project name: `/` and other separators become
/// `-`, so `feature/oauth` -> `feature-oauth`. `.` and `:` are rewritten too because tmux
/// silently turns them into `_` in session names, which would break exact lookups.
pub fn safe_name(input: &str) -> String {
    let s: String = input
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() || matches!(c, '-' | '_') { c } else { '-' })
        .collect();
    let mut collapsed = String::with_capacity(s.len());
    let mut last_dash = false;
    for c in s.chars() {
        if c == '-' {
            if !last_dash { collapsed.push('-'); }
            last_dash = true;
        } else {
            collapsed.push(c);
            last_dash = false;
        }
    }
    collapsed.trim_matches('-').to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sanitize_basic() {
        assert_eq!(sanitize_branch("fix timezone bug").as_deref(), Some("fix-timezone-bug"));
        assert_eq!(sanitize_branch("feature/oauth").as_deref(), Some("feature/oauth"));
        assert_eq!(sanitize_branch("  bold-falcon ").as_deref(), Some("bold-falcon"));
    }

    #[test]
    fn sanitize_git_invalid_sequences() {
        // The class of bug v0 left open: dot sequences and .lock.
        assert_eq!(sanitize_branch("a..b").as_deref(), Some("a.b"));
        assert_eq!(sanitize_branch("trailing.").as_deref(), Some("trailing"));
        assert_eq!(sanitize_branch("foo.lock").as_deref(), Some("foo"));
        assert_eq!(sanitize_branch(".hidden").as_deref(), Some("hidden"));
        assert_eq!(sanitize_branch("-lead").as_deref(), Some("lead"));
        assert_eq!(sanitize_branch("a//b").as_deref(), Some("a/b"));
        assert_eq!(sanitize_branch("x@{1}").as_deref(), Some("x-1}"));
        assert_eq!(sanitize_branch("w^:?*[\\"), None.or(Some("w".to_string())));
        assert_eq!(sanitize_branch("..."), None);
        assert_eq!(sanitize_branch(""), None);
    }

    #[test]
    fn safe_names() {
        assert_eq!(safe_name("feature/oauth"), "feature-oauth");
        assert_eq!(safe_name("canopy"), "canopy");
        assert_eq!(safe_name("a  b"), "a-b");
        assert_eq!(safe_name("tmp.4Dsl:x"), "tmp-4Dsl-x");
    }
}
