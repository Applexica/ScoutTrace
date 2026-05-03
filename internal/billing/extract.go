// Package billing extracts cost/token/model accounting metadata from
// arbitrary tool-call response payloads. It is intentionally permissive:
// callers feed in opaque JSON (MCP tool result bodies, Claude Code hook
// payloads, transcript lines) and Extract returns whatever it can recognise
// without mutating or capturing prompt text.
package billing

import (
	"encoding/json"
	"strings"
)

// Block is the package-internal mirror of event.BillingBlock. It is kept
// separate so the billing package does not depend on the event package
// (which imports envelope/redaction logic that does not concern this layer).
type Block struct {
	CostUSD       *float64
	TokensIn      *int
	TokensOut     *int
	Model         string
	Provider      string
	PricingSource string
}

// Empty reports whether b carries no metadata.
func (b Block) Empty() bool {
	return b.CostUSD == nil && b.TokensIn == nil && b.TokensOut == nil &&
		b.Model == "" && b.Provider == "" && b.PricingSource == ""
}

// Merge returns dst with any unset fields filled from src. Existing fields
// in dst are preserved — callers should think of dst as the authoritative
// source and src as the fallback.
func Merge(dst, src Block) Block {
	out := dst
	if out.CostUSD == nil && src.CostUSD != nil {
		v := *src.CostUSD
		out.CostUSD = &v
	}
	if out.TokensIn == nil && src.TokensIn != nil {
		v := *src.TokensIn
		out.TokensIn = &v
	}
	if out.TokensOut == nil && src.TokensOut != nil {
		v := *src.TokensOut
		out.TokensOut = &v
	}
	if out.Model == "" {
		out.Model = src.Model
	}
	if out.Provider == "" {
		out.Provider = src.Provider
	}
	if out.PricingSource == "" {
		out.PricingSource = src.PricingSource
	}
	return out
}

// Extract walks raw and pulls billing fields it can recognise. It accepts a
// JSON object, a JSON array of objects, or an MCP-shaped {content:[{text:"<json>"}]}
// envelope where the inner text itself parses as JSON.
//
// Extract never panics on bad input and never returns prompt text — only the
// recognised numeric/string fields are kept.
func Extract(raw json.RawMessage) Block {
	if len(raw) == 0 || string(raw) == "null" {
		return Block{}
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return Block{}
	}
	return extractFromNode(node)
}

func extractFromNode(node any) Block {
	switch v := node.(type) {
	case map[string]any:
		return extractFromObject(v)
	case []any:
		// Try to merge from each element. Useful for {content:[...]}.
		var acc Block
		for _, el := range v {
			acc = Merge(acc, extractFromNode(el))
		}
		return acc
	default:
		return Block{}
	}
}

// recognised aliases for cost in USD.
var costKeys = []string{"cost_usd", "costUsd", "costUSD", "cost", "total_cost_usd", "totalCostUsd"}

// recognised aliases for input/prompt token counts.
var tokensInKeys = []string{"tokens_in", "tokensIn", "input_tokens", "inputTokens", "prompt_tokens", "promptTokens"}

// recognised aliases for output/completion token counts.
var tokensOutKeys = []string{"tokens_out", "tokensOut", "output_tokens", "outputTokens", "completion_tokens", "completionTokens"}

var modelKeys = []string{"model", "modelName", "model_name", "model_id", "modelId"}

var providerKeys = []string{"provider", "providerName", "provider_name"}

func extractFromObject(o map[string]any) Block {
	var b Block

	if c, ok := pickNumber(o, costKeys); ok {
		v := c
		b.CostUSD = &v
	}
	if n, ok := pickInt(o, tokensInKeys); ok {
		v := n
		b.TokensIn = &v
	}
	// Anthropic-style usage splits prompt tokens across input_tokens,
	// cache_creation_input_tokens, and cache_read_input_tokens. Sum those
	// cache fields into TokensIn so the displayed/billed input total reflects
	// all prompt tokens — otherwise tool-loop turns appear with input_tokens=1
	// while the bulk of context shows up only under cache fields.
	for _, key := range []string{"cache_creation_input_tokens", "cacheCreationInputTokens", "cache_read_input_tokens", "cacheReadInputTokens"} {
		if v, ok := o[key]; ok {
			n, ok := toInt(v)
			if !ok {
				continue
			}
			if b.TokensIn == nil {
				vv := n
				b.TokensIn = &vv
			} else {
				vv := *b.TokensIn + n
				b.TokensIn = &vv
			}
		}
	}
	if n, ok := pickInt(o, tokensOutKeys); ok {
		v := n
		b.TokensOut = &v
	}
	if s, ok := pickString(o, modelKeys); ok {
		b.Model = s
	}
	if s, ok := pickString(o, providerKeys); ok {
		b.Provider = s
	}

	// Look inside a nested usage block (Anthropic and OpenAI both nest
	// token counts under "usage"; some providers nest model id alongside).
	if usage, ok := o["usage"].(map[string]any); ok {
		nested := extractFromObject(usage)
		b = Merge(b, nested)
	}

	// Many MCP responses are {content:[{type:"text", text:"<embedded>"}]}.
	// Try to pull billing out of any embedded text body that itself parses
	// as JSON.
	if content, ok := o["content"].([]any); ok {
		for _, el := range content {
			if m, ok := el.(map[string]any); ok {
				if txt, ok := m["text"].(string); ok && strings.TrimSpace(txt) != "" {
					var inner any
					if err := json.Unmarshal([]byte(txt), &inner); err == nil {
						b = Merge(b, extractFromNode(inner))
					}
				}
			}
		}
	}

	// Some hosts attach metadata under a sibling key.
	for _, key := range []string{"metadata", "meta", "_meta"} {
		if sub, ok := o[key].(map[string]any); ok {
			b = Merge(b, extractFromObject(sub))
		}
	}

	return b
}

func pickNumber(o map[string]any, keys []string) (float64, bool) {
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
			f, err := x.Float64()
			if err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func pickInt(o map[string]any, keys []string) (int, bool) {
	for _, k := range keys {
		v, ok := o[k]
		if !ok {
			continue
		}
		if n, ok := toInt(v); ok {
			return n, true
		}
	}
	return 0, false
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case json.Number:
		n, err := x.Int64()
		if err == nil {
			return int(n), true
		}
	}
	return 0, false
}

func pickString(o map[string]any, keys []string) (string, bool) {
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
