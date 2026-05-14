package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runConfigCmd executes a `canopy config <args...>` invocation in
// isolation: fresh HOME under t.TempDir() so config.json doesn't
// stomp the user's real ~/.canopy. Returns the captured stdout/stderr
// and the error from RunE so tests can assert on both surfaces.
func runConfigCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// Ensure no stale env override surprises us — tests that exercise
	// the env branch set it explicitly.
	t.Setenv("CANOPY_SOURCE_ROOT", "")
	os.Unsetenv("CANOPY_SOURCE_ROOT")

	cmd := configCmd()
	cmd.SetArgs(args)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

// TestConfigSet_Get_RoundTrip is the bread-and-butter case:
// set a key, get it back, see the source annotation.
func TestConfigSet_Get_RoundTrip(t *testing.T) {
	// Not Parallel: each subtest sets HOME via t.Setenv.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	os.Unsetenv("CANOPY_SOURCE_ROOT")

	// Set
	cmd := configCmd()
	cmd.SetArgs([]string{"set", "source-root", "/home/avi/Work"})
	var setOut bytes.Buffer
	cmd.SetOut(&setOut)
	cmd.SetErr(&setOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set: %v\n%s", err, setOut.String())
	}
	if !strings.Contains(setOut.String(), "Set source-root = /home/avi/Work") {
		t.Errorf("set output = %q; want confirmation line", setOut.String())
	}

	// Get
	cmd = configCmd()
	cmd.SetArgs([]string{"get", "source-root"})
	var getOut bytes.Buffer
	cmd.SetOut(&getOut)
	cmd.SetErr(&getOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("get: %v\n%s", err, getOut.String())
	}
	want := "/home/avi/Work  (config)\n"
	if getOut.String() != want {
		t.Errorf("get = %q; want %q", getOut.String(), want)
	}

	// Verify the on-disk file shape so future schema edits don't silently
	// break compatibility with hand-edited files.
	confPath := filepath.Join(fakeHome, ".canopy", "config.json")
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, `"source-root": "/home/avi/Work"`) {
		t.Errorf("config.json missing source-root field; got:\n%s", got)
	}
	if !strings.Contains(got, `"version": 1`) {
		t.Errorf("config.json missing version field; got:\n%s", got)
	}
}

// TestConfigGet_Default returns the built-in default with `(default)`
// annotation when no config and no env var are set.
func TestConfigGet_Default(t *testing.T) {
	stdout, _, err := runConfigCmd(t, "get", "source-root")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Default is <fakeHome>/.canopy/sources — we can't know the exact
	// tempdir path, but we can assert on the (default) suffix.
	if !strings.HasSuffix(strings.TrimRight(stdout, "\n"), "(default)") {
		t.Errorf("get with no config = %q; want suffix '(default)'", stdout)
	}
	if !strings.Contains(stdout, ".canopy/sources") {
		t.Errorf("default value should end in .canopy/sources; got %q", stdout)
	}
}

// TestConfigGet_EnvOverride: $CANOPY_SOURCE_ROOT overrides both config
// and default, annotated with (env) so users can debug "why isn't my
// config value taking effect."
func TestConfigGet_EnvOverride(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("CANOPY_SOURCE_ROOT", "/tmp/from-env")

	// Set a config value too, so we can prove env wins over config.
	cmd := configCmd()
	cmd.SetArgs([]string{"set", "source-root", "/from-config"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set: %v", err)
	}

	cmd = configCmd()
	cmd.SetArgs([]string{"get", "source-root"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("get: %v", err)
	}
	want := "/tmp/from-env  (env)\n"
	if out.String() != want {
		t.Errorf("get with env = %q; want %q", out.String(), want)
	}
}

// TestConfigUnset clears a previously-set value and falls back to default.
func TestConfigUnset(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	os.Unsetenv("CANOPY_SOURCE_ROOT")

	// Set
	cmd := configCmd()
	cmd.SetArgs([]string{"set", "source-root", "/home/avi/Work"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Unset
	cmd = configCmd()
	cmd.SetArgs([]string{"unset", "source-root"})
	var unsetOut bytes.Buffer
	cmd.SetOut(&unsetOut)
	cmd.SetErr(&unsetOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unset: %v\n%s", err, unsetOut.String())
	}
	if !strings.Contains(unsetOut.String(), "Unset source-root") {
		t.Errorf("unset output = %q; want confirmation line", unsetOut.String())
	}

	// Get post-unset → default
	cmd = configCmd()
	cmd.SetArgs([]string{"get", "source-root"})
	var getOut bytes.Buffer
	cmd.SetOut(&getOut)
	cmd.SetErr(&getOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.HasSuffix(strings.TrimRight(getOut.String(), "\n"), "(default)") {
		t.Errorf("get after unset = %q; want (default) suffix", getOut.String())
	}
}

// TestConfigUnset_NeverSet succeeds even when the key was never set —
// unset describes intent ("I want this cleared"), not state.
func TestConfigUnset_NeverSet(t *testing.T) {
	_, _, err := runConfigCmd(t, "unset", "source-root")
	if err != nil {
		t.Fatalf("unset never-set: %v", err)
	}
}

// TestConfigSet_EmptyValue rejects the empty string and points at unset.
// Empty would silently turn into a fallback-to-default on next get, which
// is confusing — force explicit unset for "go back to default."
func TestConfigSet_EmptyValue(t *testing.T) {
	_, _, err := runConfigCmd(t, "set", "source-root", "")
	if err == nil {
		t.Fatal("set empty value: nil error; want refused")
	}
	if !strings.Contains(err.Error(), "empty value") {
		t.Errorf("err = %v; want mention of empty value", err)
	}
}

// TestConfigSet_UnknownKey rejects with a helpful error listing
// known keys.
func TestConfigSet_UnknownKey(t *testing.T) {
	_, _, err := runConfigCmd(t, "set", "made-up-key", "value")
	if err == nil {
		t.Fatal("set unknown key: nil error; want refused")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("err = %v; want 'unknown key' message", err)
	}
	if !strings.Contains(err.Error(), "source-root") {
		t.Errorf("err = %v; want known keys listed (e.g. source-root)", err)
	}
}

// TestConfigGet_UnknownKey rejects with the same shape.
func TestConfigGet_UnknownKey(t *testing.T) {
	_, _, err := runConfigCmd(t, "get", "made-up-key")
	if err == nil {
		t.Fatal("get unknown key: nil error; want refused")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("err = %v; want 'unknown key' message", err)
	}
}

// TestConfigList prints every known key with VALUE and SOURCE columns.
func TestConfigList(t *testing.T) {
	stdout, _, err := runConfigCmd(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "KEY") {
		t.Errorf("list output missing header; got %q", stdout)
	}
	if !strings.Contains(stdout, "source-root") {
		t.Errorf("list output missing source-root row; got %q", stdout)
	}
	if !strings.Contains(stdout, "default") {
		t.Errorf("list output missing default source annotation; got %q", stdout)
	}
}

// TestConfigLs_Alias: `canopy config ls` works as an alias for `list`,
// matching canopy's convention (canopy ls / host ls / project ls).
func TestConfigLs_Alias(t *testing.T) {
	stdout, _, err := runConfigCmd(t, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(stdout, "source-root") {
		t.Errorf("ls (alias) output missing source-root row; got %q", stdout)
	}
}

// TestConfigSet_OverwritesExisting: a second set replaces the first
// without prompting. Same semantics as `git config`.
func TestConfigSet_OverwritesExisting(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	os.Unsetenv("CANOPY_SOURCE_ROOT")

	for _, val := range []string{"/first", "/second"} {
		cmd := configCmd()
		cmd.SetArgs([]string{"set", "source-root", val})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("set %q: %v", val, err)
		}
	}
	cmd := configCmd()
	cmd.SetArgs([]string{"get", "source-root"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(out.String(), "/second  (config)") {
		t.Errorf("get after double-set = %q; want /second (config)", out.String())
	}
}
