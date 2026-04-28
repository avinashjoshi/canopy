// Package port allocates TCP ports for canopy workspaces.
//
// Allocate is a pure function: given a port range and a list of
// already-claimed ports, it returns the first available port. The
// "available" check has two layers:
//
//  1. Not in the caller-supplied `used` list (workspaces canopy already
//     knows about, fed in from state.json).
//  2. Not bound by some other process on the box. We probe via
//     net.Listen("tcp", ":N"); if it succeeds, the port is genuinely
//     free and we close immediately. This catches Docker, leftover
//     puma, or anything else outside canopy's awareness.
//
// Concurrent uniqueness (two `canopy new` invocations don't pick the same
// port) is the *caller's* responsibility — wrap Allocate inside a
// state.WithLock so the read-scan-pick-write sequence is atomic. See
// TestAllocate_ConcurrentDistinctPorts in port_test.go for the canonical
// integration pattern.
package port

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// ErrNoPortsAvailable is returned when every port in [min, max] is either
// in `used` or held by another process. The workspace lifecycle surfaces
// this as a clean error to the user; reconciliation can suggest cleaning
// up old workspaces or expanding the range.
var ErrNoPortsAvailable = errors.New("port: no ports available in range")

// Allocate returns the first port in [min, max] that is neither in `used`
// nor bound by an external process. stride controls the increment between
// candidates: stride=1 visits every port, stride=10 visits min, min+10,
// min+20, ... — useful when the port plan reserves blocks of N adjacent
// ports per workspace (Rails on 3000 + Sidekiq on 3001 + Redis on 3002,
// next workspace at 3010).
//
// Probing via net.Listen briefly opens and immediately closes a listener.
// On Linux the kernel marks the freed port TIME_WAIT for ~60s, but TCP's
// SO_REUSEADDR-by-default behavior on the listener side means a follow-up
// dev server should bind without trouble. In practice the probe-then-bind
// gap is microseconds.
func Allocate(min, max, stride int, used []int) (int, error) {
	if min > max {
		return 0, fmt.Errorf("port.Allocate: invalid range [%d,%d]", min, max)
	}
	if stride <= 0 {
		stride = 1
	}

	usedSet := make(map[int]struct{}, len(used))
	for _, p := range used {
		usedSet[p] = struct{}{}
	}

	for p := min; p <= max; p += stride {
		if _, claimed := usedSet[p]; claimed {
			continue
		}
		if !isFree(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("port.Allocate(range %d-%d stride %d, %d used): %w",
		min, max, stride, len(used), ErrNoPortsAvailable)
}

// isFree returns true if the port can be bound by net.Listen on localhost.
// We do not bind to 0.0.0.0 because we don't want to (a) require root for
// privileged ports or (b) fail on machines with restrictive firewalls that
// block external binds. localhost-only is the correct match for "is some
// dev server using this?".
func isFree(p int) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
