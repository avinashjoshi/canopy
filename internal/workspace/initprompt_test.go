package workspace

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrPromptFailed_ErrorsAs(t *testing.T) {
	// Verifies the sentinel works with errors.As — cmd/canopy/main.go's
	// exit-code-2 branch and TUI dispatch both depend on this.
	var inner error = &ErrPromptFailed{Reason: "test reason"}
	wrapped := errors.New("not a prompt failure")

	var pf *ErrPromptFailed
	if !errors.As(inner, &pf) {
		t.Error("errors.As on direct *ErrPromptFailed = false, want true")
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
	e := &ErrPromptFailed{Reason: "test reason here"}
	want := "prompt not sent: test reason here"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestIsPromptFailed_DirectAndWrapped(t *testing.T) {
	// Direct match.
	direct := &ErrPromptFailed{Reason: "direct"}
	pf, ok := IsPromptFailed(direct)
	if !ok {
		t.Error("IsPromptFailed(direct) = false, want true")
	}
	if pf == nil || pf.Reason != "direct" {
		t.Errorf("IsPromptFailed(direct) returned %+v, want Reason=direct", pf)
	}

	// Wrapped via fmt.Errorf %w — errors.As traverses the chain.
	wrapped := fmt.Errorf("outer: %w", &ErrPromptFailed{Reason: "inner"})
	pf, ok = IsPromptFailed(wrapped)
	if !ok {
		t.Error("IsPromptFailed(wrapped) = false, want true (errors.As should traverse %w)")
	}
	if pf == nil || pf.Reason != "inner" {
		t.Errorf("IsPromptFailed(wrapped) returned %+v, want Reason=inner", pf)
	}

	// Negative: a plain error is NOT a prompt failure.
	plain := errors.New("nope")
	if pf, ok := IsPromptFailed(plain); ok {
		t.Errorf("IsPromptFailed(plain) = true (%+v), want false", pf)
	}

	// nil: defensively, IsPromptFailed(nil) should be (nil, false).
	if pf, ok := IsPromptFailed(nil); ok || pf != nil {
		t.Errorf("IsPromptFailed(nil) = (%+v, %v), want (nil, false)", pf, ok)
	}
}
