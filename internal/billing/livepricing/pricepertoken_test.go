package livepricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPricePerTokenLookupFetchesAndCachesModelPricing(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Method != "tools/call" || req.Params.Name != "get_model" {
			t.Fatalf("unexpected JSON-RPC request: %#v", req)
		}
		if req.Params.Arguments["author"] != "anthropic" || req.Params.Arguments["model"] != "claude-opus-4-7" {
			t.Fatalf("unexpected args: %#v", req.Params.Arguments)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"structuredContent": map[string]any{
					"result": `{"author":"anthropic","model":"claude-opus-4.7","pricing":{"input_per_1m":5,"output_per_1m":25,"cache_read_per_1m":0.5,"cache_write_per_1m":6.25}}`,
				},
			},
		})
	}))
	defer srv.Close()

	p := NewPricePerToken(PricePerTokenOptions{URL: srv.URL, CacheTTL: time.Hour})
	got, ok := p.Lookup(context.Background(), "anthropic", "claude-opus-4-7")
	if !ok {
		t.Fatalf("Lookup ok=false")
	}
	if got.InputPerM != 5 || got.OutputPerM != 25 || got.CacheReadPerM != 0.5 || got.CacheWritePerM != 6.25 {
		t.Fatalf("pricing = %#v", got)
	}
	if got.Provider != "anthropic" {
		t.Fatalf("provider = %q", got.Provider)
	}

	got2, ok := p.Lookup(context.Background(), "anthropic", "claude-opus-4-7")
	if !ok || got2 != got {
		t.Fatalf("cached lookup = %#v, %v; want %#v, true", got2, ok, got)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls = %d, want 1 cache hit", calls)
	}
}

func TestPricePerTokenLookupPersistsCacheAcrossProviderInstances(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"structuredContent": map[string]any{
					"result": `{"author":"anthropic","model":"claude-sonnet-4.6","pricing":{"input_per_1m":3,"output_per_1m":15,"cache_read_per_1m":0.3,"cache_write_per_1m":3.75}}`,
				},
			},
		})
	}))
	defer srv.Close()

	cachePath := t.TempDir() + "/pricepertoken-cache.json"
	p1 := NewPricePerToken(PricePerTokenOptions{URL: srv.URL, CacheTTL: time.Hour, CachePath: cachePath})
	got, ok := p1.Lookup(context.Background(), "anthropic", "claude-sonnet-4-6")
	if !ok {
		t.Fatalf("initial Lookup ok=false")
	}

	p2 := NewPricePerToken(PricePerTokenOptions{URL: "http://127.0.0.1:1/unreachable", Timeout: 10 * time.Millisecond, CacheTTL: time.Hour, CachePath: cachePath})
	got2, ok := p2.Lookup(context.Background(), "anthropic", "claude-sonnet-4-6")
	if !ok || got2 != got {
		t.Fatalf("disk cached lookup = %#v, %v; want %#v, true", got2, ok, got)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls = %d, want exactly one upstream call", calls)
	}
}

func TestPricePerTokenLookupCachesNegativeResults(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"structuredContent": map[string]any{"result": `{"error":"not found"}`},
			},
		})
	}))
	defer srv.Close()

	p := NewPricePerToken(PricePerTokenOptions{URL: srv.URL, CacheTTL: time.Hour})
	if _, ok := p.Lookup(context.Background(), "anthropic", "unknown-model"); ok {
		t.Fatalf("Lookup ok=true for unknown model")
	}
	if _, ok := p.Lookup(context.Background(), "anthropic", "unknown-model"); ok {
		t.Fatalf("cached Lookup ok=true for unknown model")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls = %d, want 1 negative cache hit", calls)
	}
}

func TestPricePerTokenLookupReturnsFalseOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := NewPricePerToken(PricePerTokenOptions{URL: srv.URL, CacheTTL: time.Hour})
	if _, ok := p.Lookup(context.Background(), "anthropic", "claude-opus-4-7"); ok {
		t.Fatalf("Lookup ok=true on HTTP error")
	}
}
