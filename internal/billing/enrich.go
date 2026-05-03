package billing

import "context"

// StaticPrice represents a configured static per-tool price. Capture paths
// look one up by (server, tool) and, if matched and no cost was reported,
// use the configured CostUSD with PricingSource="static".
type StaticPrice struct {
	CostUSD  float64
	Provider string
	Model    string
}

// StaticPriceLookup is a function that returns the configured static price
// for a (server, tool) pair, if any. The boolean ok must be false when no
// rule matched.
type StaticPriceLookup func(serverName, toolName string) (StaticPrice, bool)

// Enrich is the canonical enrichment pipeline used by both the MCP proxy
// capture and the Claude Code hook capture. It:
//
//  1. Extracts billing fields from `responsePayload`.
//  2. If a reported cost is present, marks pricing_source=reported.
//  3. Otherwise tries to estimate cost from model+tokens via Estimate().
//  4. Otherwise consults a static per-tool price (if a lookup is supplied).
//
// Backwards-compatible shim that does not consult a live provider; use
// EnrichLive for the live-pricing-aware pipeline.
func Enrich(responsePayload []byte, serverName, toolName string, static StaticPriceLookup) Block {
	return EnrichLive(context.Background(), responsePayload, serverName, toolName, static, nil, "")
}

// EnrichLive is Enrich with an optional live-pricing lookup inserted between
// "reported" and "estimated". Priority chain:
//
//  1. Cost reported in the response body  → pricing_source=reported
//  2. Live provider hit (model + tokens)  → pricing_source=<liveSource>
//  3. Static built-in table (model+tokens) → pricing_source=estimated
//  4. Configured static per-tool price     → pricing_source=static
//
// A nil live lookup is equivalent to the legacy Enrich() pipeline. Live
// failures are silent — they fall through to the static estimate so capture
// is never blocked by network errors.
func EnrichLive(ctx context.Context, responsePayload []byte, serverName, toolName string, static StaticPriceLookup, live LiveLookup, liveSource string) Block {
	b := Extract(responsePayload)
	if b.CostUSD != nil {
		if b.PricingSource == "" {
			b.PricingSource = "reported"
		}
		b = fillProvider(b)
		return b
	}
	// Try estimation via tokens+model — live first, static fallback.
	if b.Model != "" && (b.TokensIn != nil || b.TokensOut != nil) {
		var ti, to int
		if b.TokensIn != nil {
			ti = *b.TokensIn
		}
		if b.TokensOut != nil {
			to = *b.TokensOut
		}
		if cost, source, ok := EstimateLive(ctx, live, liveSource, b.Model, ti, to); ok {
			c := cost
			b.CostUSD = &c
			b.PricingSource = source
			b = fillProvider(b)
			return b
		}
	}
	// Fall back to a configured static per-tool price.
	if static != nil {
		if sp, ok := static(serverName, toolName); ok {
			c := sp.CostUSD
			b.CostUSD = &c
			b.PricingSource = "static"
			if b.Provider == "" {
				b.Provider = sp.Provider
			}
			if b.Model == "" {
				b.Model = sp.Model
			}
		}
	}
	return b
}

// fillProvider sets b.Provider from the model id when not already set.
func fillProvider(b Block) Block {
	if b.Provider == "" && b.Model != "" {
		b.Provider = LookupProvider(b.Model)
	}
	return b
}
