package livepricing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultPricePerTokenURL is the public no-auth MCP-over-HTTP endpoint exposed
// by https://www.pricepertoken.com. The path is fixed and does not currently
// require any kind of credential.
const DefaultPricePerTokenURL = "https://api.pricepertoken.com/mcp/mcp"

// PricePerToken is a Provider backed by the PricePerToken JSON-RPC endpoint.
// All requests are HTTP POST with a JSON-RPC 2.0 body; the only tool we use is
// `get_model`. Failures (network, non-2xx, parse, missing fields) are returned
// as ok=false so callers fall back to the static table.
type PricePerToken struct {
	URL       string
	Timeout   time.Duration
	Client    *http.Client
	CachePath string
	cache     *memoryCache
}

// PricePerTokenOptions configures a PricePerToken provider.
type PricePerTokenOptions struct {
	URL       string        // overrides DefaultPricePerTokenURL when non-empty
	Timeout   time.Duration // per-request HTTP timeout; 0 → 1500ms
	CacheTTL  time.Duration // in-process cache TTL; 0 disables caching
	CachePath string        // optional persistent JSON cache path shared across processes
	Client    *http.Client  // overrides the default client when non-nil
}

// NewPricePerToken builds a Provider with conservative defaults. Callers are
// expected to share one instance across the whole capture pipeline so the
// in-memory cache is effective.
func NewPricePerToken(opts PricePerTokenOptions) *PricePerToken {
	url := opts.URL
	if strings.TrimSpace(url) == "" {
		url = DefaultPricePerTokenURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &PricePerToken{
		URL:       url,
		Timeout:   timeout,
		Client:    client,
		CachePath: opts.CachePath,
		cache:     newMemoryCache(opts.CacheTTL),
	}
}

// Name implements Provider.
func (p *PricePerToken) Name() string { return "pricepertoken" }

// Lookup implements Provider. The cache returns immediately on a hit (positive
// or negative). On a miss we issue a JSON-RPC tools/call get_model request and
// cache whatever we got back.
func (p *PricePerToken) Lookup(ctx context.Context, provider, model string) (Pricing, bool) {
	if p == nil {
		return Pricing{}, false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if model == "" {
		return Pricing{}, false
	}

	key := cacheKey(provider, model)
	if e, ok := p.cache.get(key); ok {
		return e.pricing, e.ok
	}
	if e, ok := p.diskGet(key); ok {
		p.cache.setEntry(key, e)
		return e.pricing, e.ok
	}

	pricing, ok := p.fetch(ctx, provider, model)
	e := p.cache.set(key, cacheEntry{pricing: pricing, ok: ok})
	p.diskSet(key, e)
	return pricing, ok
}

type diskCacheFile struct {
	Version int                       `json:"version"`
	Entries map[string]diskCacheEntry `json:"entries"`
}

type diskCacheEntry struct {
	Pricing Pricing   `json:"pricing"`
	OK      bool      `json:"ok"`
	Expires time.Time `json:"expires"`
}

func (p *PricePerToken) diskGet(key string) (cacheEntry, bool) {
	if p == nil || strings.TrimSpace(p.CachePath) == "" {
		return cacheEntry{}, false
	}
	f, ok := p.readDiskCache()
	if !ok || f.Entries == nil {
		return cacheEntry{}, false
	}
	de, ok := f.Entries[key]
	if !ok || time.Now().After(de.Expires) {
		return cacheEntry{}, false
	}
	return cacheEntry{pricing: de.Pricing, ok: de.OK, expires: de.Expires}, true
}

func (p *PricePerToken) diskSet(key string, e cacheEntry) {
	if p == nil || strings.TrimSpace(p.CachePath) == "" || e.expires.IsZero() {
		return
	}
	f, _ := p.readDiskCache()
	if f.Entries == nil {
		f.Entries = map[string]diskCacheEntry{}
	}
	f.Version = 1
	now := time.Now()
	for k, v := range f.Entries {
		if now.After(v.Expires) {
			delete(f.Entries, k)
		}
	}
	f.Entries[key] = diskCacheEntry{Pricing: e.pricing, OK: e.ok, Expires: e.expires}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(p.CachePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(p.CachePath)+"-tmp-")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(b)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		return
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, p.CachePath); err != nil {
		_ = os.Remove(tmpName)
	}
}

func (p *PricePerToken) readDiskCache() (diskCacheFile, bool) {
	if p == nil || strings.TrimSpace(p.CachePath) == "" {
		return diskCacheFile{}, false
	}
	b, err := os.ReadFile(p.CachePath)
	if err != nil {
		return diskCacheFile{}, false
	}
	var f diskCacheFile
	if err := json.Unmarshal(b, &f); err != nil {
		return diskCacheFile{}, false
	}
	return f, true
}

// fetch issues a single JSON-RPC request against the configured endpoint.
func (p *PricePerToken) fetch(ctx context.Context, provider, model string) (Pricing, bool) {
	args := map[string]any{"model": model}
	if provider != "" {
		args["author"] = provider
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_model",
			"arguments": args,
		},
	})
	if err != nil {
		return Pricing{}, false
	}
	reqCtx := ctx
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return Pricing{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "ScoutTrace live-pricing")
	resp, err := p.Client.Do(req)
	if err != nil {
		return Pricing{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Pricing{}, false
	}

	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return Pricing{}, false
	}
	if len(rpc.Error) > 0 && string(rpc.Error) != "null" {
		return Pricing{}, false
	}
	return parsePricePerTokenResult(rpc.Result, provider)
}

// parsePricePerTokenResult walks an MCP tools/call result and returns the
// embedded Pricing. PricePerToken canonically returns prices under
// result.structuredContent.result, but content[0].text occasionally carries
// the same JSON, so we look at both. Numeric fields use snake_case_per_1m.
func parsePricePerTokenResult(raw json.RawMessage, provider string) (Pricing, bool) {
	if len(raw) == 0 {
		return Pricing{}, false
	}
	var top struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return Pricing{}, false
	}
	if top.IsError {
		return Pricing{}, false
	}

	candidates := []json.RawMessage{}
	if len(top.StructuredContent) > 0 {
		candidates = append(candidates, top.StructuredContent)
	}
	for _, c := range top.Content {
		if c.Type != "" && c.Type != "text" {
			continue
		}
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		candidates = append(candidates, json.RawMessage(c.Text))
	}

	for _, cand := range candidates {
		if pr, ok := pickPricingFromJSON(cand, provider); ok {
			return pr, true
		}
	}
	return Pricing{}, false
}

// pickPricingFromJSON walks an opaque JSON value and returns the first object
// that carries an input-side per-million price. It tolerates wrapping under
// `result`, `model`, etc., so a small future-proofing change to the upstream
// shape does not break us.
func pickPricingFromJSON(raw json.RawMessage, provider string) (Pricing, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return Pricing{}, false
	}
	return walkForPricing(v, provider)
}

func walkForPricing(node any, provider string) (Pricing, bool) {
	switch n := node.(type) {
	case map[string]any:
		if pr, ok := pricingFromObject(n, provider); ok {
			return pr, true
		}
		for _, key := range []string{"result", "model", "data", "pricing", "prices"} {
			if child, ok := n[key]; ok {
				if pr, ok := walkForPricing(child, provider); ok {
					return pr, true
				}
			}
		}
	case []any:
		for _, el := range n {
			if pr, ok := walkForPricing(el, provider); ok {
				return pr, true
			}
		}
	case string:
		trimmed := strings.TrimSpace(n)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var inner any
			if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
				return walkForPricing(inner, provider)
			}
		}
	}
	return Pricing{}, false
}

// pricingFromObject maps the canonical PricePerToken keys onto Pricing. We
// require at least input_per_1m; everything else is optional. CamelCase
// aliases are accepted for symmetry with other JSON conventions in the repo.
func pricingFromObject(o map[string]any, provider string) (Pricing, bool) {
	in, hasIn := pickFloat(o, "input_per_1m", "inputPer1m", "input_per_million", "input_cost_per_1m")
	if !hasIn {
		return Pricing{}, false
	}
	out, _ := pickFloat(o, "output_per_1m", "outputPer1m", "output_per_million", "output_cost_per_1m")
	cr, _ := pickFloat(o, "cache_read_per_1m", "cacheReadPer1m", "cache_read_per_million")
	cw, _ := pickFloat(o, "cache_write_per_1m", "cacheWritePer1m", "cache_creation_per_1m", "cache_write_per_million")

	prov := provider
	if s, ok := pickString(o, "author", "provider", "providerName", "provider_name"); ok && s != "" {
		prov = strings.ToLower(s)
	}
	return Pricing{
		InputPerM:      in,
		OutputPerM:     out,
		CacheReadPerM:  cr,
		CacheWritePerM: cw,
		Provider:       prov,
	}, true
}

func pickFloat(o map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		v, ok := o[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case float64:
			return x, true
		case int:
			return float64(x), true
		case json.Number:
			if f, err := x.Float64(); err == nil {
				return f, true
			}
		case string:
			var f float64
			if _, err := fmt.Sscanf(x, "%f", &f); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func pickString(o map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		v, ok := o[k]
		if !ok {
			continue
		}
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}
