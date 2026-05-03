// Package livepricing fetches per-million-token model pricing from external
// providers (currently PricePerToken) at runtime. It exists so ScoutTrace can
// charge tool/LLM events at the rates models actually charge today rather than
// the static estimate baked into internal/billing's table — without ever
// blocking event capture if the live source is unreachable.
//
// Implementations cache lookups in-process for a configured TTL and can also
// persist the same entries to disk when configured. They must honor the
// caller's context for timeout/cancellation. A failed lookup
// (network error, parse error, missing model) returns ok=false; callers fall
// back to the static estimate. Negative results are also cached so we do not
// hammer the upstream API for a model it does not know.
package livepricing

import (
	"context"
	"sync"
	"time"
)

// Pricing is per-million-token list pricing for a single model. Cache rates
// are optional: a zero CacheReadPerM / CacheWritePerM means "the live source
// did not expose a cache rate" — callers that bill cache tokens should fall
// back to InputPerM in that case (Anthropic-style cache hits would otherwise
// be charged at $0/M, which is wrong).
type Pricing struct {
	InputPerM      float64
	OutputPerM     float64
	CacheReadPerM  float64
	CacheWritePerM float64
	Provider       string
}

// Provider is the live-pricing interface. Lookup must return ok=false on any
// failure (network, parse, unknown model, context cancelled) so the caller can
// gracefully fall through to the static estimate. Implementations MUST NOT
// panic on bad input.
type Provider interface {
	// Lookup returns pricing for (provider, model). It must respect ctx for
	// timeouts and cancellation.
	Lookup(ctx context.Context, provider, model string) (Pricing, bool)
	// Name returns a stable identifier (e.g. "pricepertoken") used for the
	// PricingSource field on captured events.
	Name() string
}

// cacheEntry holds a single lookup result and the time at which it must be
// refreshed. Both successful and failed lookups are cached so a model that
// PricePerToken does not know about does not trigger a network round trip on
// every event.
type cacheEntry struct {
	pricing Pricing
	ok      bool
	expires time.Time
}

// memoryCache is an unbounded in-memory cache keyed by (provider, model).
// "Unbounded" is fine for ScoutTrace because the per-process model set is
// small (typically <10 distinct models per session). When a disk cache is
// configured, this remains the fast first-level cache for the current process.
type memoryCache struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]cacheEntry
}

func newMemoryCache(ttl time.Duration) *memoryCache {
	return &memoryCache{ttl: ttl, now: time.Now, m: map[string]cacheEntry{}}
}

func (c *memoryCache) get(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return cacheEntry{}, false
	}
	if c.now().After(e.expires) {
		delete(c.m, key)
		return cacheEntry{}, false
	}
	return e, true
}

func (c *memoryCache) set(key string, e cacheEntry) cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl <= 0 {
		return cacheEntry{}
	}
	e.expires = c.now().Add(c.ttl)
	c.m[key] = e
	return e
}

func (c *memoryCache) setEntry(key string, e cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl <= 0 || e.expires.IsZero() {
		return
	}
	c.m[key] = e
}

func cacheKey(provider, model string) string { return provider + "|" + model }
