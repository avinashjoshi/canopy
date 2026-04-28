package workspace

import "regexp"

// diagnosis pairs a regex against a one-line user-facing hint.
// Order matters: Diagnose returns the FIRST match, so the most
// specific patterns belong at the top.
type diagnosis struct {
	pattern *regexp.Regexp
	hint    string
}

// diagnoses is the small, hand-curated registry of known
// scripts.setup failure signatures. Keep tight — a wrong hint is
// worse than no hint. New entries should come from real failures
// observed in dogfood, not speculative patterns.
//
// Compile-once at package init via MustCompile; the regex cost is
// paid once on program startup, not per Diagnose call.
var diagnoses = []diagnosis{
	// Rails / Active Record encryption — the cravd dogfood failure
	// that motivated this whole verb. master.key missing means the
	// user copied the repo but not the credential.
	{
		pattern: regexp.MustCompile(`(?i)(missing\s+(active\s+record\s+)?encryption\s+key|RAILS_MASTER_KEY|master\.key)`),
		hint:    "Missing Rails master key — symlink config/master.key from the source repo or set RAILS_MASTER_KEY in scripts.setup",
	},

	// Database already exists. Common after a partial scripts.setup
	// crashed past db:create — the retry then trips the same step.
	// Hint nudges toward an idempotent setup.
	{
		pattern: regexp.MustCompile(`(?i)(database\s+["'].*["']\s+already\s+exists|already\s+exists\s+\(PG::DuplicateDatabase\))`),
		hint:    "Database already exists from a previous setup attempt — switch scripts.setup to use db:prepare instead of db:create, or guard with `[ -f ... ] ||`",
	},

	// Bundler / gem install missing.
	{
		pattern: regexp.MustCompile(`(?i)(bundle:\s+command\s+not\s+found|bundler:\s+command\s+not\s+found)`),
		hint:    "bundle not in PATH — `gem install bundler` or check that scripts.setup loads your Ruby env (rbenv/asdf/mise shim)",
	},

	// Network / DNS errors. Setup hooks frequently call out to
	// network resources (gem servers, npm registry, package.json
	// repos) and most failures here are transient.
	{
		pattern: regexp.MustCompile(`(?i)(dial\s+tcp.*i/o\s+timeout|could\s+not\s+resolve\s+host|network\s+is\s+unreachable|temporary\s+failure\s+in\s+name\s+resolution|dial\s+tcp.*connection\s+refused)`),
		hint:    "Network failure during setup — check connectivity, then retry (R in TUI, `canopy retry` in CLI)",
	},

	// Permission denied — most often the script itself isn't +x.
	{
		pattern: regexp.MustCompile(`(?i)(permission\s+denied)`),
		hint:    "Permission denied — chmod +x scripts.setup (and any sub-scripts it sources or execs)",
	},

	// Generic "command not found" catch-all. Stays last so the more
	// specific bundler / network patterns above hit first.
	{
		pattern: regexp.MustCompile(`([\w.-]+):\s+command\s+not\s+found`),
		hint:    "A command in scripts.setup was not found in PATH — check that your shell env (PATH, asdf/mise/rbenv) is what scripts.setup expects when canopy spawns it",
	},
}

// Diagnose scans captured setup stderr for a known failure signature
// and returns a one-line hint. Returns "" when nothing matches —
// callers should fall back to displaying the raw error chain
// untouched. Safe to call with nil or empty input.
func Diagnose(stderr []byte) string {
	if len(stderr) == 0 {
		return ""
	}
	for _, d := range diagnoses {
		if d.pattern.Match(stderr) {
			return d.hint
		}
	}
	return ""
}
