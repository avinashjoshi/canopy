package host

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRefreshOneHost_DeadlineExceededReturnsTimeoutError pins the v0.21.12+
// timeout-classification contract: when the per-host context deadline
// fires before cmd.Run() completes, refreshOneHost must return an error
// whose message contains the literal substring "timeout" (so the Hosts
// tab classifier in BuildRows maps it to StatusOffline, not the
// StatusBroken catchall).
//
// Mechanism: exec.Cmd.Start checks the context's Done channel BEFORE it
// resolves the binary or spawns a process. Passing an already-expired
// context means cmd.Run returns context.DeadlineExceeded immediately
// regardless of whether ssh is even on PATH — so this test exercises
// the timeout branch without any SSH I/O.
func TestRefreshOneHost_DeadlineExceededReturnsTimeoutError(t *testing.T) {
	// Deadline already in the past — cmd.Start sees ctx.Done() before
	// spawning anything and bails with context.DeadlineExceeded.
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer cancel()

	h := Host{Name: "tower", Type: "ssh", SSHTarget: "avi@tower.invalid"}
	res := refreshOneHost(parent, h, 100*time.Millisecond)

	if res.Err == nil {
		t.Fatalf("refreshOneHost on expired ctx: got nil Err, want timeout error")
	}
	if !strings.Contains(res.Err.Error(), "timeout") {
		t.Errorf("refreshOneHost on expired ctx: err = %q, want substring %q so hosts.BuildRows classifies as Offline (not Broken)", res.Err.Error(), "timeout")
	}
}
