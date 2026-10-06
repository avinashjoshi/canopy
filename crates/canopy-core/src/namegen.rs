//! Random `adjective-noun` workspace names ("bold-falcon").

use rand::seq::IndexedRandom;

const ADJECTIVES: &[&str] = &[
    "bold", "brave", "bright", "calm", "clever", "cool", "crisp", "eager", "fair", "fast",
    "fond", "glad", "grand", "happy", "humble", "jolly", "keen", "kind", "lively", "lucky",
    "merry", "mild", "neat", "nimble", "noble", "plucky", "proud", "quick", "quiet", "rapid",
    "ready", "sharp", "shiny", "sleek", "smart", "snug", "steady", "sunny", "swift", "tidy",
    "vivid", "warm", "wild", "wise", "witty", "zesty",
];

const NOUNS: &[&str] = &[
    "falcon", "otter", "heron", "badger", "beacon", "birch", "canyon", "cedar", "comet", "coral",
    "crane", "delta", "dune", "ember", "fjord", "forest", "glacier", "harbor", "island", "jaguar",
    "kestrel", "lagoon", "lantern", "lynx", "maple", "meadow", "meteor", "nebula", "oak", "orca",
    "osprey", "pebble", "pine", "prairie", "quartz", "raven", "reef", "river", "sparrow", "summit",
    "tundra", "valley", "walrus", "willow", "yarrow", "zephyr",
];

/// Generate one random name.
pub fn generate() -> String {
    let mut rng = rand::rng();
    let adj = ADJECTIVES.choose(&mut rng).expect("non-empty");
    let noun = NOUNS.choose(&mut rng).expect("non-empty");
    format!("{adj}-{noun}")
}

/// Generate a name not in `taken`. Falls back to a numeric suffix after many collisions.
pub fn unique(taken: impl Fn(&str) -> bool) -> String {
    for _ in 0..64 {
        let n = generate();
        if !taken(&n) {
            return n;
        }
    }
    let base = generate();
    for i in 2..10_000u32 {
        let n = format!("{base}-{i}");
        if !taken(&n) {
            return n;
        }
    }
    // Pathological: the caller claims everything is taken. Hand back a random suffix.
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    format!("{base}-{nanos:x}")
}

/// True when `name` looks like one of ours (`adjective-noun`), which drives the
/// "rename me first" briefing nudge.
pub fn is_generated(name: &str) -> bool {
    let Some((a, n)) = name.split_once('-') else { return false };
    ADJECTIVES.contains(&a) && NOUNS.contains(&n)
}

/// Valid workspace name: lowercase letters, digits, `-`, `_`, `.`; starts with alnum;
/// 1..=64 chars. Branch sanitization is separate (see `git::sanitize_branch`).
pub fn is_valid(name: &str) -> bool {
    let ok_len = !name.is_empty() && name.len() <= 64;
    let first_ok = name.chars().next().is_some_and(|c| c.is_ascii_alphanumeric());
    ok_len
        && first_ok
        && name.chars().all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, '-' | '_' | '.'))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generated_names_are_valid_and_recognized() {
        for _ in 0..50 {
            let n = generate();
            assert!(is_valid(&n), "{n}");
            assert!(is_generated(&n), "{n}");
        }
    }

    #[test]
    fn user_names_not_flagged_as_generated() {
        assert!(!is_generated("fix-timezone"));
        assert!(!is_generated("bold"));
    }

    #[test]
    fn unique_avoids_taken() {
        let taken: std::collections::HashSet<String> = (0..20).map(|_| generate()).collect();
        let n = unique(|c| taken.contains(c));
        assert!(!taken.contains(&n));
        assert!(is_valid(&n));
    }

    #[test]
    fn unique_terminates_when_everything_is_taken() {
        let n = unique(|c| !c.contains('-') || c.matches('-').count() < 2 || c.len() < 12);
        assert!(is_valid(&n), "{n}");
    }

    #[test]
    fn validity() {
        assert!(is_valid("fix-bug"));
        assert!(is_valid("v1.2"));
        assert!(!is_valid(""));
        assert!(!is_valid("-lead"));
        assert!(!is_valid("Has Space"));
        assert!(!is_valid("Upper"));
    }
}
