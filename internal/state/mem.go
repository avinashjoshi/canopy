// Per-workspace RSS cache for the TUI's Mem column. Without caching,
// every refresh tick would spawn a `ps -A` per workspace row — single-
// digit-ms each, but at 10+ workspaces it adds noticeable overhead
// for a column that doesn't need to be that fresh.
//
// TTL is the load-bearing knob. 5s is the default: fresh enough that
// the column reflects recent kills/launches, slow enough that scrolling
// through a long list doesn't burn CPU. The cache is invalidated
// explicitly on K (kill) so the just-killed row's mem column flips to
// "—" immediately rather than waiting for TTL.

package state

import (
	"context"
	"sync"
	"time"
)

// loadCache stores per-session LoadValue (RSS + CPU) with a TTL.
// Safe for concurrent use. The TUI creates one cache for the program's
// lifetime; tests create their own.
type loadCache struct {
	mu     sync.Mutex
	ttl    time.Duration
	values map[string]loadCacheEntry
}

type loadCacheEntry struct {
	load LoadValue
	ts   time.Time
}

// DefaultMemCacheTTL is the default cache TTL — 5s. Changeable via
// NewMemCache when tests need a tighter or looser window. Name kept
// for backwards compat though the cache now holds RSS+CPU.
const DefaultMemCacheTTL = 5 * time.Second

// NewMemCache is the back-compat constructor name; returns a LoadCache
// (RSS + CPU). Tests still call NewMemCache so we keep this around.
func NewMemCache(ttl time.Duration) *MemCache {
	return NewLoadCache(ttl)
}

// NewLoadCache returns an empty cache with the given TTL.
func NewLoadCache(ttl time.Duration) *MemCache {
	if ttl <= 0 {
		ttl = DefaultMemCacheTTL
	}
	return &MemCache{c: &loadCache{ttl: ttl, values: make(map[string]loadCacheEntry)}}
}

// MemCache is the public handle (name retained for back-compat — it
// now holds full LoadValue, not just RSS). Pointer semantics so cache
// state survives across Build calls.
type MemCache struct{ c *loadCache }

// Get returns the cached RSS for session if fresh; otherwise calls
// the legacy MemProbe.SessionRSS and caches RSS-only. Use GetLoad for
// the modern probe that returns CPU too.
func (mc *MemCache) Get(ctx context.Context, probe MemProbe, session string) (int64, error) {
	if mc == nil || probe == nil || session == "" {
		return 0, nil
	}
	mc.c.mu.Lock()
	if e, ok := mc.c.values[session]; ok && time.Since(e.ts) < mc.c.ttl {
		mc.c.mu.Unlock()
		return e.load.RSS, nil
	}
	mc.c.mu.Unlock()

	rss, err := probe.SessionRSS(ctx, session)
	if err != nil {
		return 0, err
	}
	mc.c.mu.Lock()
	mc.c.values[session] = loadCacheEntry{load: LoadValue{RSS: rss}, ts: time.Now()}
	mc.c.mu.Unlock()
	return rss, nil
}

// GetLoad returns the cached {RSS, CPU} for session if fresh;
// otherwise probes and caches both. Errors from probe are returned
// untouched and the cache is NOT updated (so a transient probe
// failure doesn't poison the cache for the TTL window).
func (mc *MemCache) GetLoad(ctx context.Context, probe LoadProbe, session string) (LoadValue, error) {
	if mc == nil || probe == nil || session == "" {
		return LoadValue{}, nil
	}
	mc.c.mu.Lock()
	if e, ok := mc.c.values[session]; ok && time.Since(e.ts) < mc.c.ttl {
		mc.c.mu.Unlock()
		return e.load, nil
	}
	mc.c.mu.Unlock()

	load, err := probe.SessionLoad(ctx, session)
	if err != nil {
		return LoadValue{}, err
	}
	mc.c.mu.Lock()
	mc.c.values[session] = loadCacheEntry{load: load, ts: time.Now()}
	mc.c.mu.Unlock()
	return load, nil
}

// Invalidate drops the cached value for one session. Used on K
// (kill) so the row's Mem column doesn't lag the actual state by up
// to TTL seconds.
func (mc *MemCache) Invalidate(session string) {
	if mc == nil || session == "" {
		return
	}
	mc.c.mu.Lock()
	delete(mc.c.values, session)
	mc.c.mu.Unlock()
}

// InvalidateAll drops every cached value. Used on `r` explicit refresh
// — the user asked for fresh data; honor that on every column.
func (mc *MemCache) InvalidateAll() {
	if mc == nil {
		return
	}
	mc.c.mu.Lock()
	mc.c.values = make(map[string]loadCacheEntry)
	mc.c.mu.Unlock()
}

// BuildGlobalRowsWithMem is the legacy single-probe form. Populates
// MemRSS only; CPU stays at zero. Kept so existing callers and tests
// that pass a MemProbe-only fake keep working. New callers should
// use BuildGlobalRowsWithLoad to get CPU populated too.
func (s *State) BuildGlobalRowsWithMem(
	ctx context.Context,
	live LivenessProbe,
	mem MemProbe,
	cache *MemCache,
) []GlobalRow {
	rows := s.BuildGlobalRows(ctx, live)
	if mem == nil {
		return rows
	}
	for i := range rows {
		if !rows[i].Alive || rows[i].TmuxSession == "" {
			continue
		}
		rss, err := cache.Get(ctx, mem, rows[i].TmuxSession)
		if err != nil {
			continue
		}
		rows[i].MemRSS = rss
	}
	return rows
}

// BuildGlobalRowsWithLoad is the same as BuildGlobalRows but
// additionally populates MemRSS+CPU for live workspace rows via the
// LoadProbe (one probe call returns both). Main rows and dead
// workspace rows skip the probe (probe-on-dead-session would just
// round-trip as 0+error).
func (s *State) BuildGlobalRowsWithLoad(
	ctx context.Context,
	live LivenessProbe,
	load LoadProbe,
	cache *MemCache,
) []GlobalRow {
	rows := s.BuildGlobalRows(ctx, live)
	if load == nil {
		return rows
	}
	for i := range rows {
		if !rows[i].Alive || rows[i].TmuxSession == "" {
			continue
		}
		l, err := cache.GetLoad(ctx, load, rows[i].TmuxSession)
		if err != nil {
			continue
		}
		rows[i].MemRSS = l.RSS
		rows[i].CPU = l.CPU
	}
	return rows
}
