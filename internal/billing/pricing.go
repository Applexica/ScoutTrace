package billing

import "strings"

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
