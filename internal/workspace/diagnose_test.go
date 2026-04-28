package workspace

import (
	"strings"
	"testing"
)

// TestDiagnose covers the curated registry one signature at a time.
// Table-driven so adding a new pattern means adding a row, not a new
// test function. The wantSubstr field lets assertions stay loose —
// we care that the right hint *category* came back, not the exact
// wording (which we expect to evolve).
func TestDiagnose(t *testing.T) {
	cases := []struct {
		name       string
		stderr     string
		wantSubstr string // substring expected in the hint; empty -> expect "" hint
	}{
		{
			name:       "rails master key missing",
			stderr:     "ActiveSupport::MessageEncryptor::InvalidMessage: missing encryption key\n",
			wantSubstr: "Rails master key",
		},
		{
			name:       "rails master.key file missing",
			stderr:     "Errno::ENOENT: No such file or directory @ rb_sysopen - config/master.key\n",
			wantSubstr: "Rails master key",
		},
		{
			name:       "rails RAILS_MASTER_KEY env",
			stderr:     "Set RAILS_MASTER_KEY in your environment\n",
			wantSubstr: "Rails master key",
		},
		{
			// Real failure from cravd 2026-04-28. Distinct from
			// master.key — Rails 7+ AR encryption requires its own
			// keys in config/credentials/<env>.yml.enc.
			name:       "rails AR encryption credential missing",
			stderr:     "ActiveRecord::Encryption::Errors::Configuration: Missing Active Record encryption credential: active_record_encryption.deterministic_key\n",
			wantSubstr: "Active Record encryption credentials missing",
		},
		{
			name:       "rails AR encryption primary_key key path",
			stderr:     "Missing key in active_record_encryption.primary_key\n",
			wantSubstr: "Active Record encryption credentials missing",
		},
		{
			name:       "database already exists postgres",
			stderr:     `database "cravd_canopy_dev" already exists` + "\n",
			wantSubstr: "Database already exists",
		},
		{
			name:       "bundle command not found",
			stderr:     "/usr/bin/env: 'bundle': No such file or directory\nbundle: command not found\n",
			wantSubstr: "bundle not in PATH",
		},
		{
			name:       "network timeout",
			stderr:     "fetch https://rubygems.org/specs.4.8.gz: dial tcp: i/o timeout\n",
			wantSubstr: "Network failure",
		},
		{
			name:       "dns resolve failure",
			stderr:     "curl: (6) Could not resolve host: rubygems.org\n",
			wantSubstr: "Network failure",
		},
		{
			name:       "permission denied",
			stderr:     "/bin/sh: ./bin/migrate: Permission denied\n",
			wantSubstr: "Permission denied",
		},
		{
			name:       "generic command not found falls through to catchall",
			stderr:     "yarn: command not found\n",
			wantSubstr: "not found in PATH",
		},
		{
			name:       "unknown stderr returns empty hint",
			stderr:     "Some completely novel failure mode that the registry doesn't know about\n",
			wantSubstr: "",
		},
		{
			name:       "empty stderr returns empty hint",
			stderr:     "",
			wantSubstr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Diagnose([]byte(tc.stderr))
			if tc.wantSubstr == "" {
				if got != "" {
					t.Errorf("Diagnose returned hint %q; wanted empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("Diagnose hint = %q; wanted substring %q", got, tc.wantSubstr)
			}
		})
	}
}

// TestDiagnose_OrderMatters guards the "specific before generic"
// invariant. The bundler pattern must match before the catchall
// "command not found" — otherwise users with broken Ruby envs get
// the generic hint instead of the bundler-specific one.
func TestDiagnose_OrderMatters(t *testing.T) {
	stderr := "bundle: command not found\n"
	got := Diagnose([]byte(stderr))
	if !strings.Contains(got, "bundle") {
		t.Errorf("expected bundler-specific hint, got %q", got)
	}
}

// TestDiagnose_NilSafe: the production call site can pass nil if no
// stderr was captured. Diagnose must not panic.
func TestDiagnose_NilSafe(t *testing.T) {
	if got := Diagnose(nil); got != "" {
		t.Errorf("Diagnose(nil) = %q; want empty", got)
	}
}
