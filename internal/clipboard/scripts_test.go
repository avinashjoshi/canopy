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
	// Catch regression where someone refactors out the flag handling
	// or the OSC 52 mechanism itself.
	pasteContent, _, _ := WrapperContent(WrapperWlPaste, "v0")
	for _, must := range []string{"--list-types", "--type", "--no-newline", "52;c;", "base64 -d"} {
		if !strings.Contains(pasteContent, must) {
			t.Errorf("wl-paste wrapper missing %q", must)
		}
	}
	copyContent, _, _ := WrapperContent(WrapperWlCopy, "v0")
	for _, must := range []string{"52;c;", "base64"} {
		if !strings.Contains(copyContent, must) {
			t.Errorf("wl-copy wrapper missing %q", must)
		}
	}
}

func TestWrapperContent_TmuxDCSPassthroughWrapping(t *testing.T) {
	// Both wrappers must wrap their OSC 52 sequence in tmux's DCS
	// passthrough envelope when $TMUX is set — otherwise the sequence
	// never reaches the outer terminal from inside a canopy workspace
	// (every canopy workspace IS a tmux session). Catch regression
	// where someone drops the ${TMUX:-} check.
	for _, w := range []WrapperScript{WrapperWlPaste, WrapperWlCopy} {
		content, _, _ := WrapperContent(w, "v0")
		if !strings.Contains(content, `TMUX:-`) {
			t.Errorf("%s wrapper missing the $TMUX check gating DCS passthrough wrapping", w)
		}
		if !strings.Contains(content, `Ptmux;`) {
			t.Errorf("%s wrapper missing the tmux DCS passthrough envelope (\\033Ptmux;...)", w)
		}
	}
}

func TestWrapperContent_ImagePasteExplicitlyUnsupported(t *testing.T) {
	// OSC 52 payloads are too size-constrained for images (terminals
	// cap them well below screenshot size — see the file header
	// comment for why herdr bridges images via a temp file instead).
	// wl-paste must fail clearly on --type image/png, not silently
	// return nothing or attempt a doomed OSC 52 round-trip.
	content, _, _ := WrapperContent(WrapperWlPaste, "v0")
	if !strings.Contains(content, "image/*") {
		t.Error("wl-paste wrapper missing the image/* case in its --type dispatch")
	}
	if !strings.Contains(content, "not supported over OSC 52") {
		t.Error("wl-paste wrapper's image rejection message missing — regression could silently return empty output instead of erroring")
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

// head returns the first n bytes of s; helper for fail-message readability.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// The socket-based --list-types liveness regression tests that used
// to live here (TestWrapperScripts_ListTypesOmitsTextPlainWhenSocketUnreachable
// / ...Reachable) were the regression coverage for the exact bug found
// in production (dogfood 2026-09-03, tower): `wl-paste --list-types`
// used to unconditionally print "text/plain;charset=utf-8" BEFORE
// attempting any connectivity check. That bug — and the SSH tunnel +
// socat mechanism it lived in — no longer exists: the wrapper now
// speaks OSC 52 directly to the local terminal (see wl-paste.sh's
// header comment), with the equivalent "claim nothing without a real
// round-trip" coverage in osc52_wrapper_test.go's
// TestOSC52Wrapper_PasteQueryReadDecode (list_types_responsive /
// list_types_unresponsive_claims_nothing cases), which exercises the
// real query/read/parse logic through a pty instead of a fake socket.

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
