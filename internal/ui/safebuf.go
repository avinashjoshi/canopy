package ui

import (
	"strings"
	"sync"
)

// safeBuffer is a tiny mutex-protected string accumulator used by the
// new-workspace busyMode to stream scripts.setup output from a
// background goroutine into the foreground View. The producer
// (workspace.Manager.Create's stdout/stderr writer) calls Write; the
// consumer (the Update tick handler) calls Drain at ~150ms intervals
// to fetch any new content.
//
// Why not a chan: scripts produce output in arbitrary chunks (could
// be byte-per-byte, could be 4KB at once). A chan-of-strings would
// either need a chunker on the producer side or aggressive batching
// on the consumer. A buffer is simpler — the tick batches naturally,
// and partial-line reads are normal (the View just appends).
//
// Why not just bytes.Buffer with a mutex inline: keeping the type
// small and named lets the test harness mock it cleanly and makes
// the streaming intent visible at the call site.
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

// Write satisfies io.Writer so the buffer can be passed directly to
// workspace.Manager.Create as the stdout/stderr sink. Always returns
// (len(p), nil) — we don't propagate Builder errors because they
// don't happen in practice (Builder only errors on after-Reset use,
// which we don't do).
func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// Drain returns the accumulated content and resets the buffer. The
// reset is what makes the polling pattern work: every tick fetches
// only NEW content, not the whole history. The View accumulates the
// drained chunks into m.busyOutput so the visible state is the full
// stream, while the buffer itself stays small.
func (s *safeBuffer) Drain() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.buf.String()
	s.buf.Reset()
	return out
}
