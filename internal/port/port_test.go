package port_test

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/avinashjoshi/canopy/internal/clog"
	"github.com/avinashjoshi/canopy/internal/port"
	"github.com/avinashjoshi/canopy/internal/state"
)

func TestMain(m *testing.M) {
	teardown, _ := clog.Init(false)
	defer teardown()
	m.Run()
}

// pickRange returns a range high enough that ad-hoc dev servers on the
// host machine are unlikely to conflict. 39000-39100 is well above the
// usual 3000s/4000s/5000s/8000s/9000s and below ephemeral.
const (
	rangeMin = 39000
	rangeMax = 39100
)

// TestAllocate_HappyPath: empty used list, first port in range is free,
// return it.
func TestAllocate_HappyPath(t *testing.T) {
	t.Parallel()
	got, err := port.Allocate(rangeMin, rangeMax, 1, nil)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got < rangeMin || got > rangeMax {
		t.Errorf("Allocate returned %d; want in [%d,%d]", got, rangeMin, rangeMax)
	}
}

// TestAllocate_SkipsUsed: ports listed in `used` must be skipped.
func TestAllocate_SkipsUsed(t *testing.T) {
	t.Parallel()
	used := []int{rangeMin, rangeMin + 1, rangeMin + 2}
	got, err := port.Allocate(rangeMin, rangeMax, 1, used)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got <= rangeMin+2 {
		t.Errorf("Allocate returned %d; should have skipped used [%d-%d]", got, rangeMin, rangeMin+2)
	}
}

// TestAllocate_SkipsExternallyHeld: bind a port externally with
// net.Listen, verify Allocate's probe catches it. This is the bug-class
// the eng review's failure-modes table flagged: state.json says "free"
// but Docker/puma/whatever holds the port.
func TestAllocate_SkipsExternallyHeld(t *testing.T) {
	t.Parallel()

	// Ask the kernel to pick a port for us by listening on :0, then
	// extract the port number. This avoids hardcoding a number that
	// might already be in use on the dev box running the tests.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	defer ln.Close()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	heldPort, _ := strconv.Atoi(p)

	got, err := port.Allocate(heldPort, heldPort+5, 1, nil)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got == heldPort {
		t.Errorf("Allocate returned %d, but that port is held by the test's own listener", heldPort)
	}
}

// TestAllocate_Exhaustion is CRITICAL test #2 from the eng review: when
// every port in the range is in `used`, Allocate must return
// ErrNoPortsAvailable rather than looping forever or panicking.
func TestAllocate_Exhaustion(t *testing.T) {
	t.Parallel()
	used := make([]int, 0, rangeMax-rangeMin+1)
	for p := rangeMin; p <= rangeMax; p++ {
		used = append(used, p)
	}
	_, err := port.Allocate(rangeMin, rangeMax, 1, used)
	if !errors.Is(err, port.ErrNoPortsAvailable) {
		t.Errorf("Allocate(all-used): got %v; want errors.Is(... ErrNoPortsAvailable)", err)
	}
}

// TestAllocate_Stride: stride > 1 visits only multiples of stride from min.
// With stride=10 starting at 39000, we'd see 39000, 39010, 39020, ...
// — used ports of 39000 and 39010 must push the allocator to 39020.
func TestAllocate_Stride(t *testing.T) {
	t.Parallel()
	const min, max = 39200, 39300
	used := []int{min, min + 10}
	got, err := port.Allocate(min, max, 10, used)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got != min+20 {
		t.Errorf("Allocate stride=10 used=[%d,%d] = %d; want %d", min, min+10, got, min+20)
	}
}

// TestAllocate_InvalidRange: min > max must error cleanly, not loop.
func TestAllocate_InvalidRange(t *testing.T) {
	t.Parallel()
	_, err := port.Allocate(rangeMax, rangeMin, 1, nil)
	if err == nil {
		t.Errorf("Allocate(min>max): want error; got nil")
	}
}

// TestAllocate_ConcurrentDistinctPorts is CRITICAL test #3 from the eng
// review. N goroutines simulate N parallel `canopy new` invocations.
// Each goroutine, inside state.WithLock:
//  1. Loads the current state.
//  2. Scans for in-use ports.
//  3. Calls port.Allocate to pick a fresh one.
//  4. Adds a workspace row pinning that port.
//
// After all N complete, every workspace must have a distinct port.
// Without state.WithLock, two goroutines would both load (port=39000
// available), both Allocate(39000), both append → port 39000 used twice.
// With WithLock, the second goroutine sees the first's write and picks
// 39001.
//
// This is the test that pins down the design doc's "atomic port
// allocation inside the lock" requirement.
func TestAllocate_ConcurrentDistinctPorts(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)

	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			err := store.WithLock(func(s *state.State) error {
				// Scan in-use ports from state.
				used := make([]int, 0, len(s.Workspaces))
				for _, w := range s.Workspaces {
					used = append(used, w.Port)
				}

				// Sleep amplifies any race window — without the lock,
				// concurrent goroutines would all see used==[] for ~1ms.
				time.Sleep(1 * time.Millisecond)

				p, err := port.Allocate(rangeMin, rangeMax, 1, used)
				if err != nil {
					return err
				}

				return s.Add(state.Workspace{
					ProjectRoot: "/tmp/concurrent-test",
					Name:        fmt.Sprintf("ws-%d", i),
					Port:        p,
					Status:      state.StatusReady,
				})
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("WithLock returned error: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Workspaces) != N {
		t.Fatalf("after %d parallel allocations, len = %d; want %d", N, len(loaded.Workspaces), N)
	}

	// Every workspace must have a distinct port.
	seen := map[int]string{}
	for _, w := range loaded.Workspaces {
		if other, dup := seen[w.Port]; dup {
			t.Errorf("port %d allocated to both %s and %s", w.Port, other, w.Name)
		}
		seen[w.Port] = w.Name
	}
}
