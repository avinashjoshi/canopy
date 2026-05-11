package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPrompt_BothEmpty_ReturnsEmpty(t *testing.T) {
	got, err := loadPrompt("", "")
	if err != nil {
		t.Errorf("loadPrompt(\"\", \"\") err = %v, want nil", err)
	}
	if got != "" {
		t.Errorf("loadPrompt(\"\", \"\") = %q, want empty", got)
	}
}

func TestLoadPrompt_PromptOnly(t *testing.T) {
	got, err := loadPrompt("hello", "")
	if err != nil {
		t.Errorf("loadPrompt err = %v, want nil", err)
	}
	if got != "hello" {
		t.Errorf("loadPrompt = %q, want %q", got, "hello")
	}
}

func TestLoadPrompt_FileOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	const content = "this is a multi-line\nprompt from a file\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := loadPrompt("", path)
	if err != nil {
		t.Errorf("loadPrompt err = %v, want nil", err)
	}
	if got != content {
		t.Errorf("loadPrompt = %q, want %q", got, content)
	}
}

func TestLoadPrompt_BothSet_ReturnsError(t *testing.T) {
	_, err := loadPrompt("hello", "/some/path")
	if err == nil {
		t.Fatal("loadPrompt with both flags = nil err, want mutually-exclusive error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %q, want contains 'mutually exclusive'", err.Error())
	}
}

func TestLoadPrompt_FileNotFound_ReturnsError(t *testing.T) {
	_, err := loadPrompt("", "/this/path/definitely/does/not/exist.txt")
	if err == nil {
		t.Fatal("loadPrompt with missing file = nil err, want filesystem error")
	}
	if !strings.Contains(err.Error(), "--prompt-file") {
		t.Errorf("err = %q, want prefix '--prompt-file'", err.Error())
	}
}

func TestLoadPrompt_FileTooLarge_RejectedNotTruncated(t *testing.T) {
	// Per the v3 design's failure-modes table: REJECT, do not truncate.
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.txt")
	huge := strings.Repeat("x", promptMaxBytes+1)
	if err := os.WriteFile(path, []byte(huge), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := loadPrompt("", path)
	if err == nil {
		t.Fatal("loadPrompt with oversize file = nil err, want size-limit error")
	}
	if got != "" {
		t.Errorf("loadPrompt with oversize file = %q (truncated content?), want empty", got[:50])
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %q, want contains 'too large'", err.Error())
	}
	if !strings.Contains(err.Error(), "Split into multiple workspaces") {
		t.Errorf("err = %q, want suggestion to split", err.Error())
	}
}

func TestLoadPrompt_ExactlyAtLimit_Accepted(t *testing.T) {
	// Boundary: exactly promptMaxBytes (32KB) is OK; one more byte is not.
	dir := t.TempDir()
	path := filepath.Join(dir, "limit.txt")
	atLimit := strings.Repeat("x", promptMaxBytes)
	if err := os.WriteFile(path, []byte(atLimit), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := loadPrompt("", path)
	if err != nil {
		t.Errorf("loadPrompt at exactly the limit err = %v, want nil", err)
	}
	if len(got) != promptMaxBytes {
		t.Errorf("loadPrompt at limit returned %d bytes, want %d", len(got), promptMaxBytes)
	}
}

func TestErrPromptFailed_ErrorsAs(t *testing.T) {
	// Verifies the sentinel works with errors.As — main.go's exit-code-2
	// branch depends on this.
	var inner error = &errPromptFailed{Reason: "test reason"}
	wrapped := errors.New("not a prompt failure")

	var pf *errPromptFailed
	if !errors.As(inner, &pf) {
		t.Error("errors.As on direct *errPromptFailed = false, want true")
	}
	if pf.Reason != "test reason" {
		t.Errorf("Reason after As = %q, want %q", pf.Reason, "test reason")
	}

	// Negative: a different error should NOT match.
	pf = nil
	if errors.As(wrapped, &pf) {
		t.Error("errors.As on plain error = true, want false")
	}
}

func TestErrPromptFailed_ErrorMessageFormat(t *testing.T) {
	e := &errPromptFailed{Reason: "test reason here"}
	want := "prompt not sent: test reason here"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
