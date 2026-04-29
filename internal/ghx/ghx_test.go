package ghx

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// TestAvailable_RespectsLookPath: Available() boils down to
// exec.LookPath("gh"). On a CI box with gh installed it returns
// true; on a bare image it returns false. We don't try to be
// stricter than that — gh's auth state is checked at the
// individual command level via runGH.
func TestAvailable_RespectsLookPath(t *testing.T) {
	_, err := exec.LookPath("gh")
	gotInstalled := Available()
	wantInstalled := err == nil
	if gotInstalled != wantInstalled {
		t.Errorf("Available() = %v; LookPath says installed=%v", gotInstalled, wantInstalled)
	}
}

// TestFetchPR_GHMissing_ReturnsErrUnavailable: when gh is missing,
// FetchPR returns ErrUnavailable directly so callers can branch on
// errors.Is and surface the install hint.
//
// We can't trivially uninstall gh in the test, so we instead PATH-
// override by setting PATH to a temp dir that contains nothing.
func TestFetchPR_GHMissing_ReturnsErrUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty PATH → gh not findable
	_, err := FetchPR(context.Background(), "/tmp", 1)
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("expected ErrUnavailable; got %v", err)
	}
}

// TestFetchIssue_GHMissing_ReturnsErrUnavailable: same shape as
// the PR test — silent skip is the contract.
func TestFetchIssue_GHMissing_ReturnsErrUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := FetchIssue(context.Background(), "/tmp", 1)
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("expected ErrUnavailable; got %v", err)
	}
}

// TestFetchPR_NotFound_ReturnsErrNotFound: gh exits non-zero when a
// PR doesn't exist. We can't reliably hit a "not found" path in unit
// tests without a real repo + gh auth, so this is a smoke test that
// asserts the error wrapping shape against a guaranteed-bad input
// (negative PR number) when gh IS available.
func TestFetchPR_NotFound_ReturnsErrNotFound(t *testing.T) {
	if !Available() {
		t.Skip("gh not on PATH; skipping live FetchPR test")
	}
	// /tmp is not a git repo; gh will exit with an error. We just
	// want to verify the error path returns ErrNotFound, not panics.
	_, err := FetchPR(context.Background(), "/tmp", 999999999)
	if err == nil {
		t.Errorf("expected error against /tmp; got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}
