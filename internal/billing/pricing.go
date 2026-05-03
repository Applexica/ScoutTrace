package billing

import (
	"context"
	"strings"
)

// modelPrice is per-million-token pricing in USD for a model family.
type modelPrice struct {
	// MatchSubstr is matched case-insensitively against the model id.
	MatchSubstr string
	InputPerM   float64
	OutputPerM  float64
	Provider    string
}

// pricingTable is a small, deliberately conservative built-in catalogue of
// commonly-seen Anthropic / OpenAI model id substrings. Order matters:
// more specific substrings come first so e.g. "claude-haiku-4-5" matches
// before a generic "claude" entry. Numbers reflect publicly-listed list
// prices at the time of writing and are intentionally rounded; they exist
// to give "reasonable" estimates when no cost is reported, not to match a
// vendor invoice exactly.
var pricingTable = []modelPrice{
	// Anthropic — most-specific first.
	{MatchSubstr: "claude-opus-4-7", InputPerM: 15.0, OutputPerM: 75.0, Provider: "anthropic"},
	{MatchSubstr: "claude-opus-4", InputPerM: 15.0, OutputPerM: 75.0, Provider: "anthropic"},
	{MatchSubstr: "claude-opus", InputPerM: 15.0, OutputPerM: 75.0, Provider: "anthropic"},
	{MatchSubstr: "claude-sonnet-4-6", InputPerM: 3.0, OutputPerM: 15.0, Provider: "anthropic"},
	{MatchSubstr: "claude-sonnet-4", InputPerM: 3.0, OutputPerM: 15.0, Provider: "anthropic"},
	{MatchSubstr: "claude-sonnet", InputPerM: 3.0, OutputPerM: 15.0, Provider: "anthropic"},
	{MatchSubstr: "claude-haiku-4-5", InputPerM: 1.0, OutputPerM: 5.0, Provider: "anthropic"},
	{MatchSubstr: "claude-haiku-4", InputPerM: 1.0, OutputPerM: 5.0, Provider: "anthropic"},
	{MatchSubstr: "claude-haiku", InputPerM: 0.8, OutputPerM: 4.0, Provider: "anthropic"},
	{MatchSubstr: "sonnet", InputPerM: 3.0, OutputPerM: 15.0, Provider: "anthropic"},
	{MatchSubstr: "haiku", InputPerM: 1.0, OutputPerM: 5.0, Provider: "anthropic"},
	{MatchSubstr: "opus", InputPerM: 15.0, OutputPerM: 75.0, Provider: "anthropic"},
	// OpenAI.
	{MatchSubstr: "gpt-4o-mini", InputPerM: 0.15, OutputPerM: 0.60, Provider: "openai"},
	{MatchSubstr: "gpt-4o", InputPerM: 2.5, OutputPerM: 10.0, Provider: "openai"},
	{MatchSubstr: "gpt-4-turbo", InputPerM: 10.0, OutputPerM: 30.0, Provider: "openai"},
	{MatchSubstr: "gpt-4", InputPerM: 30.0, OutputPerM: 60.0, Provider: "openai"},
	{MatchSubstr: "gpt-3.5", InputPerM: 0.5, OutputPerM: 1.5, Provider: "openai"},
	{MatchSubstr: "o1-mini", InputPerM: 3.0, OutputPerM: 12.0, Provider: "openai"},
	{MatchSubstr: "o1", InputPerM: 15.0, OutputPerM: 60.0, Provider: "openai"},
}

// LivePricing is per-million-token pricing returned from a live provider. It
// is intentionally a value type (no methods, no pointers) so it round-trips
// safely through cache layers and is cheap to copy.
type LivePricing struct {
	InputPerM      float64
	OutputPerM     float64
	CacheReadPerM  float64
	CacheWritePerM float64
	Provider       string
}

// LiveLookup resolves a (provider, model) tuple to a LivePricing entry. ok
// must be false on any failure (network, parse, unknown model, context
// cancelled). Implementations live under internal/billing/livepricing.
type LiveLookup func(ctx context.Context, provider, model string) (LivePricing, bool)

// Usage breaks an LLM turn's input side into base + cache components so
// EstimateUsage can apply per-component live prices when available. Output
// tokens are billed at OutputPerM unconditionally.
type Usage struct {
	Input         int
	CacheCreation int
	CacheRead     int
	Output        int
}

// LookupProvider returns the provider associated with a model id, or "" if
// the model is not in the built-in table.
func LookupProvider(model string) string {
	if p := lookup(model); p != nil {
		return p.Provider
	}
	return ""
}

// Estimate returns a cost-in-USD estimate for the given model and token
// counts using the built-in pricing table. It returns ok=false when the
// model is unknown or both token counts are zero. The returned source is
// always "estimated" when ok=true.
func Estimate(model string, tokensIn, tokensOut int) (cost float64, source string, ok bool) {
	if tokensIn <= 0 && tokensOut <= 0 {
		return 0, "", false
	}
	p := lookup(model)
	if p == nil {
		return 0, "", false
	}
	cost = float64(tokensIn)*p.InputPerM/1_000_000 + float64(tokensOut)*p.OutputPerM/1_000_000
	return cost, "estimated", true
}

// EstimateLive returns a cost-in-USD estimate trying the live provider first
// and falling back to the built-in static table on any failure. The returned
// source is the live provider's source identifier (liveSource) on a hit, or
// "estimated" on fallback. ok=false means neither path produced a price.
//
// Cache breakdown is not visible at this layer; the full input total is
// charged at InputPerM. Callers that have access to per-component token
// deltas (for example the Claude transcript scanner) should use EstimateUsage
// instead so cache rates are applied correctly.
func EstimateLive(ctx context.Context, live LiveLookup, liveSource, model string, tokensIn, tokensOut int) (cost float64, source string, ok bool) {
	if tokensIn <= 0 && tokensOut <= 0 {
		return 0, "", false
	}
	if live != nil {
		provider := LookupProvider(model)
		if pr, hit := live(ctx, provider, model); hit {
			cost = float64(tokensIn)*pr.InputPerM/1_000_000 + float64(tokensOut)*pr.OutputPerM/1_000_000
			src := liveSource
			if src == "" {
				src = "live"
			}
			return cost, src, true
		}
	}
	return Estimate(model, tokensIn, tokensOut)
}

// EstimateUsage returns a cost-in-USD estimate using a per-component Usage
// breakdown. The live provider is tried first; on success, cache_creation
// tokens are billed at CacheWritePerM and cache_read tokens at CacheReadPerM
// (when the live source exposed those rates — otherwise they fall back to
// InputPerM, which matches Anthropic's "no discount" semantics for unknown
// caches better than charging $0/M would).
//
// On live failure or any time the live source returns ok=false, falls back to
// the static pricing table charging the full input total at InputPerM. The
// returned source is liveSource on a live hit or "estimated" on fallback.
func EstimateUsage(ctx context.Context, live LiveLookup, liveSource, model string, u Usage) (cost float64, source string, ok bool) {
	if u.Input <= 0 && u.CacheCreation <= 0 && u.CacheRead <= 0 && u.Output <= 0 {
		return 0, "", false
	}
	if live != nil {
		provider := LookupProvider(model)
		if pr, hit := live(ctx, provider, model); hit {
			cacheWrite := pr.CacheWritePerM
			if cacheWrite <= 0 {
				cacheWrite = pr.InputPerM
			}
			cacheRead := pr.CacheReadPerM
			if cacheRead <= 0 {
				cacheRead = pr.InputPerM
			}
			cost = float64(u.Input)*pr.InputPerM/1_000_000 +
				float64(u.CacheCreation)*cacheWrite/1_000_000 +
				float64(u.CacheRead)*cacheRead/1_000_000 +
				float64(u.Output)*pr.OutputPerM/1_000_000
			src := liveSource
			if src == "" {
				src = "live"
			}
			return cost, src, true
		}
	}
	tokensIn := u.Input + u.CacheCreation + u.CacheRead
	return Estimate(model, tokensIn, u.Output)
}

func lookup(model string) *modelPrice {
	if model == "" {
		return nil
	}
	lower := strings.ToLower(model)
	for i := range pricingTable {
		if strings.Contains(lower, strings.ToLower(pricingTable[i].MatchSubstr)) {
			return &pricingTable[i]
		}
	}
	return nil
}
