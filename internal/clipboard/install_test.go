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
	return &LocalInstaller{
		HomeDir:    home,
		SystemdRun: sc.run,
		// Default to a passing verifier so existing tests focus on what
		// they're actually testing (unit body, SSH config, etc.). Tests
		// that need to assert the guard explicitly override this field.
		VerifyBinary: func(_ string) error { return nil },
	}, sc
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

func TestEnsureSystemdUnit_OrdersAfterGraphicalSession(t *testing.T) {
	// Wayland gotcha: systemd user services that need WAYLAND_DISPLAY
	// must start AFTER the compositor activates graphical-session.target
	// (so the env vars have been imported into the user manager). Without
	// this ordering, the daemon's first start happens before WAYLAND_DISPLAY
	// is in the env and Detect() bails with ErrNoProvider.
	for _, must := range []string{
		"After=graphical-session.target",
		"PartOf=graphical-session.target",
		"WantedBy=graphical-session.target",
	} {
		if !strings.Contains(systemdUnitBody, must) {
			t.Errorf("systemdUnitBody missing %q\nbody:\n%s", must, systemdUnitBody)
		}
	}
}

func TestEnsureSystemdUnit_BoundsRestartAttempts(t *testing.T) {
	// Headless / broken-compositor case: daemon should give up after a
	// reasonable burst instead of pinning CPU forever.
	for _, must := range []string{"StartLimitBurst=", "StartLimitIntervalSec="} {
		if !strings.Contains(systemdUnitBody, must) {
			t.Errorf("systemdUnitBody missing %q so a broken setup would restart-loop forever", must)
		}
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

func TestEnableSystemdUnit_CallsDaemonReloadEnableNowAndRestart(t *testing.T) {
	inst, sc := newTestInstaller(t)
	var out bytes.Buffer
	if err := inst.EnableSystemdUnit(&out); err != nil {
		t.Fatalf("EnableSystemdUnit: %v", err)
	}
	if len(sc.calls) != 3 {
		t.Fatalf("expected 3 systemctl calls (daemon-reload, enable --now, restart), got %d: %v", len(sc.calls), sc.calls)
	}
	want := [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", SystemdUnitName},
		{"--user", "restart", SystemdUnitName},
	}
	for i, w := range want {
		if !stringSliceEq(sc.calls[i], w) {
			t.Errorf("call[%d] = %v, want %v", i, sc.calls[i], w)
		}
	}
}

func TestImportSessionEnv_RunsImportWhenWaylandSet(t *testing.T) {
	inst, sc := newTestInstaller(t)
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	var out bytes.Buffer
	if err := inst.ImportSessionEnv(&out); err != nil {
		t.Fatalf("ImportSessionEnv: %v", err)
	}
	if len(sc.calls) != 1 {
		t.Fatalf("expected one systemctl call, got %d: %v", len(sc.calls), sc.calls)
	}
	args := sc.calls[0]
	if len(args) < 2 || args[0] != "--user" || args[1] != "import-environment" {
		t.Fatalf("expected `systemctl --user import-environment ...`, got %v", args)
	}
	// Verify WAYLAND_DISPLAY is in the imported vars (the load-bearing one).
	found := false
	for _, v := range args[2:] {
		if v == "WAYLAND_DISPLAY" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("import-environment did not include WAYLAND_DISPLAY; args=%v", args)
	}
}

func TestVerifyDaemonBinary_PassesWhenSubcommandSupported(t *testing.T) {
	inst, _ := newTestInstaller(t)
	// Default verifier in newTestInstaller returns nil → success path.
	var out bytes.Buffer
	if err := inst.VerifyDaemonBinary(&out); err != nil {
		t.Fatalf("VerifyDaemonBinary should pass when verifier returns nil, got: %v", err)
	}
	if !strings.Contains(out.String(), "verified") || !strings.Contains(out.String(), "clipboard-server") {
		t.Errorf("expected success message mentioning verified + clipboard-server, got %q", out.String())
	}
}

func TestVerifyDaemonBinary_RefusesAndHintsWhenSubcommandMissing(t *testing.T) {
	inst, _ := newTestInstaller(t)
	inst.VerifyBinary = func(_ string) error {
		return errors.New("unknown command \"clipboard-server\" for \"canopy\"")
	}
	var out bytes.Buffer
	err := inst.VerifyDaemonBinary(&out)
	if err == nil {
		t.Fatal("VerifyDaemonBinary should refuse when verifier errors")
	}
	body := out.String()
	for _, must := range []string{"Refusing to install", "make dev", "canopy use"} {
		if !strings.Contains(body, must) {
			t.Errorf("error output missing %q\nfull output:\n%s", must, body)
		}
	}
}

func TestInstall_RefusesWhenBinaryGuardFails(t *testing.T) {
	// The whole point of the guard: NO filesystem state is written when
	// the binary doesn't support clipboard-server. Otherwise the user
	// hits the start-limit-hit dance.
	inst, sc := newTestInstaller(t)
	inst.VerifyBinary = func(_ string) error {
		return errors.New("unknown command")
	}
	var out bytes.Buffer
	if err := inst.Install(&out); err == nil {
		t.Fatal("Install should refuse when guard errors")
	}
	// No artifacts produced:
	for _, path := range []string{
		filepath.Join(inst.HomeDir, ".config", "systemd", "user", SystemdUnitName),
		filepath.Join(inst.HomeDir, ".ssh", "config.d", "canopy"),
		filepath.Join(inst.HomeDir, ".ssh", "config"),
	} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("guard failed BUT %s was written; install should be atomic", path)
		}
	}
	if len(sc.calls) != 0 {
		t.Errorf("guard failed BUT systemctl was invoked %d times: %v", len(sc.calls), sc.calls)
	}
}

func TestImportSessionEnv_SkipsWhenWaylandUnset(t *testing.T) {
	inst, sc := newTestInstaller(t)
	t.Setenv("WAYLAND_DISPLAY", "")
	var out bytes.Buffer
	if err := inst.ImportSessionEnv(&out); err != nil {
		t.Fatalf("ImportSessionEnv should not error when WAYLAND_DISPLAY unset, got: %v", err)
	}
	if len(sc.calls) != 0 {
		t.Errorf("expected no systemctl call when WAYLAND_DISPLAY unset, got %v", sc.calls)
	}
	if !strings.Contains(out.String(), "skipping systemctl import-environment") {
		t.Errorf("expected skip message in output, got %q", out.String())
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

func TestInstall_RunsAllStepsInOrder_NoWayland(t *testing.T) {
	// Path: WAYLAND_DISPLAY unset → import-environment is skipped.
	// Expected systemctl calls: daemon-reload, enable --now, restart.
	t.Setenv("WAYLAND_DISPLAY", "")
	inst, sc := newTestInstaller(t)
	var out bytes.Buffer
	if err := inst.Install(&out); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inst.HomeDir, ".config", "systemd", "user", SystemdUnitName)); err != nil {
		t.Errorf("systemd unit not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inst.HomeDir, ".ssh", "config.d", "canopy")); err != nil {
		t.Errorf("canopy ssh dir not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inst.HomeDir, ".ssh", "config")); err != nil {
		t.Errorf("ssh config not created: %v", err)
	}
	if len(sc.calls) != 3 {
		t.Errorf("expected 3 systemctl calls (daemon-reload + enable --now + restart), got %d: %v", len(sc.calls), sc.calls)
	}
}

func TestInstall_RunsAllStepsInOrder_WithWayland(t *testing.T) {
	// Path: WAYLAND_DISPLAY set → import-environment runs once before
	// the systemctl daemon-reload / enable / restart trio.
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	inst, sc := newTestInstaller(t)
	var out bytes.Buffer
	if err := inst.Install(&out); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(sc.calls) != 4 {
		t.Errorf("expected 4 systemctl calls (import-env + daemon-reload + enable --now + restart), got %d: %v", len(sc.calls), sc.calls)
	}
	if len(sc.calls) > 0 && (len(sc.calls[0]) < 2 || sc.calls[0][1] != "import-environment") {
		t.Errorf("expected first call to be import-environment, got %v", sc.calls[0])
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
