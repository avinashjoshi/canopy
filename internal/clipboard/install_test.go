package clipboard

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSystemctl records every invocation and optionally returns an
// error. Test pattern: stub before Install/EnableSystemdUnit, then
// assert against .calls after.
type fakeSystemctl struct {
	calls [][]string
	err   error
}

func (f *fakeSystemctl) run(args ...string) error {
	f.calls = append(f.calls, append([]string(nil), args...))
	return f.err
}

func newTestInstaller(t *testing.T) (*LocalInstaller, *fakeSystemctl) {
	t.Helper()
	home := t.TempDir()
	sc := &fakeSystemctl{}
	return &LocalInstaller{HomeDir: home, SystemdRun: sc.run}, sc
}

func TestEnsureSystemdUnit_WritesFile(t *testing.T) {
	inst, _ := newTestInstaller(t)
	var out bytes.Buffer
	if err := inst.EnsureSystemdUnit(&out); err != nil {
		t.Fatalf("EnsureSystemdUnit: %v", err)
	}
	path := filepath.Join(inst.HomeDir, ".config", "systemd", "user", SystemdUnitName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("unit file not created: %v", err)
	}
	if string(data) != systemdUnitBody {
		t.Errorf("unit body mismatch:\ngot:\n%s\nwant:\n%s", data, systemdUnitBody)
	}
	if !strings.Contains(out.String(), "wrote") {
		t.Errorf("progress output missing 'wrote': %q", out.String())
	}
}

func TestEnsureSystemdUnit_IdempotentWhenContentMatches(t *testing.T) {
	inst, _ := newTestInstaller(t)
	// Pre-seed with the exact body that EnsureSystemdUnit would write.
	unitDir := filepath.Join(inst.HomeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(unitDir, SystemdUnitName)
	if err := os.WriteFile(path, []byte(systemdUnitBody), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	if err := inst.EnsureSystemdUnit(&out); err != nil {
		t.Fatalf("EnsureSystemdUnit: %v", err)
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Errorf("expected idempotent no-op message, got %q", out.String())
	}
}

func TestEnsureSystemdUnit_OverwritesMismatchedContent(t *testing.T) {
	inst, _ := newTestInstaller(t)
	unitDir := filepath.Join(inst.HomeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(unitDir, SystemdUnitName)
	if err := os.WriteFile(path, []byte("# old unit body from a prior version\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	if err := inst.EnsureSystemdUnit(&out); err != nil {
		t.Fatalf("EnsureSystemdUnit: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != systemdUnitBody {
		t.Errorf("expected overwrite to current body, got:\n%s", data)
	}
	if !strings.Contains(out.String(), "wrote") {
		t.Errorf("expected 'wrote' message on overwrite, got %q", out.String())
	}
}

func TestEnsureSystemdUnit_ExecStartUsesSymlinkPath(t *testing.T) {
	// D3 in /plan-eng-review: ExecStart must point at ~/.local/bin/canopy
	// (the symlink) so `canopy use` swaps survive without reinstall.
	if !strings.Contains(systemdUnitBody, "ExecStart=%h/.local/bin/canopy clipboard-server") {
		t.Errorf("systemdUnitBody must use %%h/.local/bin/canopy (symlink path), got body:\n%s", systemdUnitBody)
	}
}

func TestEnsureCanopySSHDir_CreatesMode0700(t *testing.T) {
	inst, _ := newTestInstaller(t)
	var out bytes.Buffer
	if err := inst.EnsureCanopySSHDir(&out); err != nil {
		t.Fatalf("EnsureCanopySSHDir: %v", err)
	}
	dir := filepath.Join(inst.HomeDir, ".ssh", "config.d", "canopy")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700 (sshd refuses Include into world-readable dirs)", info.Mode().Perm())
	}
}

func TestEnsureSSHInclude_CreatesConfigWhenMissing(t *testing.T) {
	inst, _ := newTestInstaller(t)
	var out bytes.Buffer
	if err := inst.EnsureSSHInclude(&out); err != nil {
		t.Fatalf("EnsureSSHInclude: %v", err)
	}
	configPath := filepath.Join(inst.HomeDir, ".ssh", "config")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config not created: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, SSHIncludeMarkerStart) {
		t.Errorf("created config missing start marker: %q", body)
	}
	if !strings.Contains(body, SSHIncludeMarkerEnd) {
		t.Errorf("created config missing end marker: %q", body)
	}
	if !strings.Contains(body, "Include ~/.ssh/config.d/canopy/*.conf") {
		t.Errorf("created config missing Include directive: %q", body)
	}
}

func TestEnsureSSHInclude_AppendsWhenConfigHasOtherContent(t *testing.T) {
	inst, _ := newTestInstaller(t)
	sshDir := filepath.Join(inst.HomeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configPath := filepath.Join(sshDir, "config")
	pre := "Host tower\n  HostName tower.lan\n  User avi\n"
	if err := os.WriteFile(configPath, []byte(pre), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	if err := inst.EnsureSSHInclude(&out); err != nil {
		t.Fatalf("EnsureSSHInclude: %v", err)
	}
	data, _ := os.ReadFile(configPath)
	body := string(data)
	if !strings.HasPrefix(body, pre) {
		t.Errorf("user content was clobbered:\n%s", body)
	}
	if !strings.Contains(body, SSHIncludeMarkerStart) {
		t.Errorf("marker block not appended:\n%s", body)
	}
}

func TestEnsureSSHInclude_AppendsWhenConfigMissingTrailingNewline(t *testing.T) {
	inst, _ := newTestInstaller(t)
	sshDir := filepath.Join(inst.HomeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configPath := filepath.Join(sshDir, "config")
	// No trailing newline — checks the "insert one before block" path.
	pre := "Host tower\n  HostName tower.lan"
	if err := os.WriteFile(configPath, []byte(pre), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	if err := inst.EnsureSSHInclude(&out); err != nil {
		t.Fatalf("EnsureSSHInclude: %v", err)
	}
	data, _ := os.ReadFile(configPath)
	body := string(data)
	// The user's original line must still terminate cleanly before the
	// canopy marker — no `tower.lan# canopy:start...` mashup.
	if strings.Contains(body, "tower.lan"+SSHIncludeMarkerStart) {
		t.Errorf("marker block fused into user line without newline:\n%s", body)
	}
	if !strings.Contains(body, SSHIncludeMarkerStart) {
		t.Errorf("marker block missing:\n%s", body)
	}
}

func TestEnsureSSHInclude_IdempotentWhenMarkerPresent(t *testing.T) {
	inst, _ := newTestInstaller(t)
	sshDir := filepath.Join(inst.HomeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configPath := filepath.Join(sshDir, "config")
	pre := "Host foo\n  User bar\n\n" + SSHIncludeMarkerStart + "\nInclude ~/.ssh/config.d/canopy/*.conf\n" + SSHIncludeMarkerEnd + "\n"
	if err := os.WriteFile(configPath, []byte(pre), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	if err := inst.EnsureSSHInclude(&out); err != nil {
		t.Fatalf("EnsureSSHInclude: %v", err)
	}
	data, _ := os.ReadFile(configPath)
	if string(data) != pre {
		t.Errorf("idempotent run changed file content:\ngot:\n%s\nwant:\n%s", data, pre)
	}
	if !strings.Contains(out.String(), "already has canopy Include block") {
		t.Errorf("expected no-op message, got %q", out.String())
	}
}

func TestEnableSystemdUnit_CallsDaemonReloadAndEnableNow(t *testing.T) {
	inst, sc := newTestInstaller(t)
	var out bytes.Buffer
	if err := inst.EnableSystemdUnit(&out); err != nil {
		t.Fatalf("EnableSystemdUnit: %v", err)
	}
	if len(sc.calls) != 2 {
		t.Fatalf("expected 2 systemctl calls, got %d: %v", len(sc.calls), sc.calls)
	}
	want0 := []string{"--user", "daemon-reload"}
	want1 := []string{"--user", "enable", "--now", SystemdUnitName}
	if !stringSliceEq(sc.calls[0], want0) {
		t.Errorf("call[0] = %v, want %v", sc.calls[0], want0)
	}
	if !stringSliceEq(sc.calls[1], want1) {
		t.Errorf("call[1] = %v, want %v", sc.calls[1], want1)
	}
}

func TestEnableSystemdUnit_PropagatesError(t *testing.T) {
	inst, sc := newTestInstaller(t)
	sc.err = errors.New("DBUS_SESSION_BUS_ADDRESS not set")
	var out bytes.Buffer
	err := inst.EnableSystemdUnit(&out)
	if err == nil {
		t.Fatal("expected error to propagate from systemctl")
	}
	if !strings.Contains(err.Error(), "EnableSystemdUnit") {
		t.Errorf("error not wrapped with EnableSystemdUnit prefix: %v", err)
	}
}

func TestInstall_RunsAllStepsInOrder(t *testing.T) {
	inst, sc := newTestInstaller(t)
	var out bytes.Buffer
	if err := inst.Install(&out); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// All four artifacts produced:
	if _, err := os.Stat(filepath.Join(inst.HomeDir, ".config", "systemd", "user", SystemdUnitName)); err != nil {
		t.Errorf("systemd unit not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inst.HomeDir, ".ssh", "config.d", "canopy")); err != nil {
		t.Errorf("canopy ssh dir not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inst.HomeDir, ".ssh", "config")); err != nil {
		t.Errorf("ssh config not created: %v", err)
	}
	if len(sc.calls) != 2 {
		t.Errorf("systemctl not invoked twice: %v", sc.calls)
	}
}

func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
