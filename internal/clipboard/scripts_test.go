package clipboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrapperContent_StampsVersionInHeader(t *testing.T) {
	for _, w := range []WrapperScript{WrapperWlPaste, WrapperWlCopy} {
		t.Run(string(w), func(t *testing.T) {
			content, hash, err := WrapperContent(w, "v0.18.0+test")
			if err != nil {
				t.Fatalf("WrapperContent: %v", err)
			}
			if !strings.Contains(content, "v0.18.0+test") {
				t.Errorf("wrapper missing version stamp; content head:\n%s", head(content, 200))
			}
			if len(hash) != 12 {
				t.Errorf("hash = %q, want 12 chars", hash)
			}
		})
	}
}

func TestWrapperContent_HashChangesWithVersion(t *testing.T) {
	_, h1, _ := WrapperContent(WrapperWlPaste, "v0.18.0+a")
	_, h2, _ := WrapperContent(WrapperWlPaste, "v0.18.0+b")
	if h1 == h2 {
		t.Errorf("hash must differ across versions (so reinstall detects drift); both = %q", h1)
	}
}

func TestWrapperContent_HashStableForSameInput(t *testing.T) {
	// Reinstall fast-skip relies on deterministic hashing. Re-rendering
	// with the same inputs must produce the same hash bit-for-bit.
	_, h1, _ := WrapperContent(WrapperWlPaste, "v0.18.0+stable")
	_, h2, _ := WrapperContent(WrapperWlPaste, "v0.18.0+stable")
	if h1 != h2 {
		t.Errorf("hash drifted across identical renders: %q vs %q", h1, h2)
	}
}

func TestWrapperContent_BodyMentionsCanonicalFlags(t *testing.T) {
	// Catch regression where someone refactors out the flag handling.
	pasteContent, _, _ := WrapperContent(WrapperWlPaste, "v0")
	for _, must := range []string{"--list-types", "--type", "--no-newline", "clip-text.sock", "clip-image.sock"} {
		if !strings.Contains(pasteContent, must) {
			t.Errorf("wl-paste wrapper missing %q", must)
		}
	}
	copyContent, _, _ := WrapperContent(WrapperWlCopy, "v0")
	if !strings.Contains(copyContent, "clip-copy.sock") {
		t.Error("wl-copy wrapper missing clip-copy.sock reference")
	}
}

func TestWrapperContent_TimeoutGuardOnSocat(t *testing.T) {
	// Locked decision at /plan-eng-review: wrappers wrap socat with
	// `timeout 2` so a daemon-down state fails fast.
	for _, w := range []WrapperScript{WrapperWlPaste, WrapperWlCopy} {
		content, _, _ := WrapperContent(w, "v0")
		if !strings.Contains(content, "timeout 2 socat") {
			t.Errorf("%s wrapper must wrap socat with `timeout 2`; not found in:\n%s", w, head(content, 400))
		}
	}
}

func TestWrapperContent_PNGSignatureProbe(t *testing.T) {
	// wl-paste --list-types path inspects the first 8 PNG header bytes
	// to decide whether to report image/png. Catch regression where
	// someone changes the magic-number check.
	content, _, _ := WrapperContent(WrapperWlPaste, "v0")
	if !strings.Contains(content, "89504e470d0a1a0a") {
		t.Error("wl-paste --list-types path missing PNG signature probe (89504e470d0a1a0a)")
	}
}

func TestWrapperContent_UnknownScriptErrors(t *testing.T) {
	_, _, err := WrapperContent(WrapperScript("nonexistent"), "v0")
	if err == nil {
		t.Fatal("expected error for unknown wrapper script")
	}
}

func TestWrapperRemoteName_StripsShExtension(t *testing.T) {
	// PATH precedence: Claude Code's exec.LookPath("wl-paste") must
	// find our wrapper, not the real wl-clipboard binary in /usr/bin.
	// Therefore the wrapper must be deployed as `wl-paste`, not
	// `wl-paste.sh`.
	if got, want := WrapperWlPaste.RemoteName(), "wl-paste"; got != want {
		t.Errorf("RemoteName = %q, want %q", got, want)
	}
	if got, want := WrapperWlCopy.RemoteName(), "wl-copy"; got != want {
		t.Errorf("RemoteName = %q, want %q", got, want)
	}
}

func TestSnippetContent_RendersWithUIDsAndHostName(t *testing.T) {
	content, err := SnippetContent(SnippetData{
		HostName:  "tower",
		Version:   "v0.18.0+test",
		LocalUID:  1000,
		RemoteUID: 1001,
	})
	if err != nil {
		t.Fatalf("SnippetContent: %v", err)
	}
	for _, must := range []string{
		"Host tower",
		"/run/user/1000/canopy/clip-text.sock",
		"/run/user/1001/canopy/clip-text.sock",
		"/run/user/1000/canopy/clip-image.sock",
		"/run/user/1000/canopy/clip-copy.sock",
		"StreamLocalBindUnlink yes",
		"v0.18.0+test",
	} {
		if !strings.Contains(content, must) {
			t.Errorf("snippet missing %q\nbody:\n%s", must, content)
		}
	}
}

func TestSnippetContent_RemoteUIDFirstOnEachLine(t *testing.T) {
	// Per the snippet template comment: RemoteForward syntax is
	// `RemoteForward <remote-path> <local-path>`. Mixing these up
	// would silently make the bridge fail (sockets created on the
	// wrong side). Test catches the swap.
	content, _ := SnippetContent(SnippetData{
		HostName:  "tower",
		Version:   "v0",
		LocalUID:  1000,
		RemoteUID: 1001,
	})
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "RemoteForward ") {
			continue
		}
		// Remote (1001) must come BEFORE local (1000).
		remote := strings.Index(line, "/run/user/1001/")
		local := strings.Index(line, "/run/user/1000/")
		if remote == -1 || local == -1 {
			t.Errorf("RemoteForward line missing one of the UIDs: %q", line)
			continue
		}
		if remote > local {
			t.Errorf("RemoteForward arg order swapped (remote must come first): %q", line)
		}
	}
}

func TestSnippetContent_RefusesEmptyHostName(t *testing.T) {
	// Empty HostName would render as `Host ` (whitespace + nothing),
	// which SSH treats as the wildcard `Host *` — broadcasting every
	// canopy bridge to every SSH connection. Catastrophic; refuse.
	_, err := SnippetContent(SnippetData{HostName: "", Version: "v0", LocalUID: 1000, RemoteUID: 1001})
	if err == nil {
		t.Fatal("SnippetContent must refuse empty HostName (would broadcast to all hosts)")
	}
}

func TestSnippetContent_RefusesNonPositiveUID(t *testing.T) {
	cases := []SnippetData{
		{HostName: "tower", LocalUID: 0, RemoteUID: 1001},
		{HostName: "tower", LocalUID: 1000, RemoteUID: 0},
		{HostName: "tower", LocalUID: -1, RemoteUID: 1001},
		{HostName: "tower", LocalUID: 1000, RemoteUID: -1},
	}
	for _, c := range cases {
		_, err := SnippetContent(c)
		if err == nil {
			t.Errorf("SnippetContent should refuse non-positive UID: %+v", c)
		}
	}
}

// head returns the first n bytes of s; helper for fail-message readability.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func TestWrapperScripts_PassBashSyntaxCheck(t *testing.T) {
	// Render each wrapper and run `bash -n` against it. Catches the
	// nightmare class of bug where the embedded script has a syntax
	// error that only surfaces the first time it's invoked on a
	// remote host (after install ran clean). Skipped when bash isn't
	// on the test machine's PATH (effectively never on a dev box,
	// but matters in some restricted CI images).
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH; skipping syntax check")
	}
	dir := t.TempDir()
	for _, w := range []WrapperScript{WrapperWlPaste, WrapperWlCopy} {
		t.Run(string(w), func(t *testing.T) {
			content, _, err := WrapperContent(w, "v0.18.0+test")
			if err != nil {
				t.Fatalf("WrapperContent: %v", err)
			}
			path := filepath.Join(dir, w.RemoteName()+".sh")
			if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
				t.Fatalf("write: %v", err)
			}
			cmd := exec.Command("bash", "-n", path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("bash -n failed for %s: %v\noutput: %s", w, err, out)
			}
		})
	}
}
