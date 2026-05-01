package jsonrpc

import (
	"encoding/json"
	"sync"
	"time"
)

// PendingRequest is the data we retain about an outbound request while
// waiting for its response.
type PendingRequest struct {
	Method    string
	Params    json.RawMessage
	StartedAt time.Time
	SpanID    string
}

// MatchedPair is what Correlator.MatchResponse returns when it pairs a
// response with a previously-seen request.
type MatchedPair struct {
	ID        string
	Method    string
	Params    json.RawMessage
	StartedAt time.Time
	EndedAt   time.Time
	SpanID    string
	Result    json.RawMessage
	Error     json.RawMessage
}

// Correlator pairs JSON-RPC responses with their preceding requests.
//
// Goroutine-safe. Caller is responsible for invoking SweepOrphans on a
// timer; Correlator does not start its own goroutine.
type Correlator struct {
	mu       sync.Mutex
	inflight map[string]PendingRequest
	maxAge   time.Duration
	now      func() time.Time

	missesTotal  uint64
	orphansTotal uint64
}

// NewCorrelator returns a Correlator that GCs pending requests older than
// maxAge. If maxAge is zero, the default of 5 minutes is used.
func NewCorrelator(maxAge time.Duration) *Correlator {
	if maxAge == 0 {
		maxAge = 5 * time.Minute
	}
	return &Correlator{
		inflight: make(map[string]PendingRequest),
		maxAge:   maxAge,
		now:      time.Now,
	}
}

// SetClock replaces the time source. Test-only.
func (c *Correlator) SetClock(fn func() time.Time) { c.now = fn }

// AddRequest records an outbound request. Returns the canonical id under
// which it was stored so callers can use it as a span key.
func (c *Correlator) AddRequest(m *Message, spanID string) string {
	if !m.IsRequest() {
		return ""
	}
	id := CanonicalID(m.ID)
	c.mu.Lock()
	c.inflight[id] = PendingRequest{
		Method:    m.Method,
		Params:    append([]byte(nil), m.Params...),
		StartedAt: c.now(),
		SpanID:    spanID,
	}
	c.mu.Unlock()
	return id
}

// MatchResponse looks up the pending request for a response. Returns
// (pair, true) on success. On miss, increments missesTotal and returns
// (zero, false).
func (c *Correlator) MatchResponse(m *Message) (MatchedPair, bool) {
	if !m.IsResponse() {
		return MatchedPair{}, false
	}
	id := CanonicalID(m.ID)
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.inflight[id]
	if !ok {
		c.missesTotal++
		return MatchedPair{}, false
	}
	delete(c.inflight, id)
	return MatchedPair{
		ID:        id,
		Method:    p.Method,
		Params:    p.Params,
		StartedAt: p.StartedAt,
		EndedAt:   c.now(),
		SpanID:    p.SpanID,
		Result:    append([]byte(nil), m.Result...),
		Error:     append([]byte(nil), m.Error...),
	}, true
}

// SweepOrphans evicts pending requests older than maxAge and returns them.
// Callers can emit them as `partial` events.
func (c *Correlator) SweepOrphans() []MatchedPair {
	cutoff := c.now().Add(-c.maxAge)
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []MatchedPair
	for id, p := range c.inflight {
		if p.StartedAt.Before(cutoff) {
			out = append(out, MatchedPair{
				ID:        id,
				Method:    p.Method,
				Params:    p.Params,
				StartedAt: p.StartedAt,
				EndedAt:   c.now(),
				SpanID:    p.SpanID,
			})
			delete(c.inflight, id)
			c.orphansTotal++
		}
	}
	return out
}

// Counters returns a snapshot of internal counters.
func (c *Correlator) Counters() (misses, orphans uint64, inflight int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.missesTotal, c.orphansTotal, len(c.inflight)
}
